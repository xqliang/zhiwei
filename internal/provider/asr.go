package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// TranscriptPiece 是 ASR 输出的最小转写单元：一段话 + 说话人标签。
type TranscriptPiece struct {
	SpeakerLabel string  // ASR 标签（"1"/"2"/...），空表示未知/单人
	Text         string
	StartMS      int64 // 当前 StepFun 方案无时间戳，恒为 0
	EndMS        int64
	Confidence   float64
}

// ASRProvider 是语音转写接口。
type ASRProvider interface {
	// Transcribe 输入音频文件路径（调用方保证已转成 wav 16k mono s16le），
	// 返回转写片段（按说话人切分）。
	Transcribe(ctx context.Context, audioPath string) ([]TranscriptPiece, error)
}

// StepFunASR 用 stepaudio-2.5-realtime（OpenAI Realtime 协议）做转写。
// 协议结论见 docs/superpowers/specs/asr-protocol-notes.md。
type StepFunASR struct {
	Endpoint string // wss://api.stepfun.com/step_plan/v1/realtime?model=stepaudio-2.5-realtime
	APIKey   string
}

func NewStepFunASR(endpoint, apiKey string) *StepFunASR {
	return &StepFunASR{Endpoint: endpoint, APIKey: apiKey}
}

const asrInstructions = `你是逐字转写引擎。只输出音频中说话内容的原文，不回答、不确认、不翻译、不解释。` +
	`多说话人时用 [说话人1] [说话人2] 前缀。`

// Transcribe 实现 ASRProvider：建会话 → 配置 → 分片喂数学频 → 收转写文本 → 解析说话人。
func (p *StepFunASR) Transcribe(ctx context.Context, audioPath string) ([]TranscriptPiece, error) {
	audio, err := os.ReadFile(audioPath)
	if err != nil {
		return nil, fmt.Errorf("读取音频: %w", err)
	}
	if len(audio) <= 44 {
		return nil, fmt.Errorf("音频文件过短: %s", audioPath)
	}
	raw := audio[44:] // 跳过 44 字节 wav 头，拿裸 pcm

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, p.Endpoint,
		map[string][]string{"Authorization": {"Bearer " + p.APIKey}})
	if err != nil {
		return nil, fmt.Errorf("asr 连接: %w", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(180 * time.Second))

	send := func(v any) error {
		b, _ := json.Marshal(v)
		return conn.WriteMessage(websocket.TextMessage, b)
	}
	// 等待指定事件（跳过无关事件；error 事件返回错误）
	waitFor := func(want string) error {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return fmt.Errorf("asr 读事件（等 %s）: %w", want, err)
			}
			var ev struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(msg, &ev)
			if ev.Type == want {
				return nil
			}
			if ev.Type == "error" {
				return fmt.Errorf("asr 服务端错误: %s", string(msg))
			}
		}
	}

	if err := waitFor("session.created"); err != nil {
		return nil, err
	}
	if err := send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"modalities":         []string{"text"},
			"instructions":       asrInstructions,
			"input_audio_format": "pcm16",
		},
	}); err != nil {
		return nil, err
	}
	if err := waitFor("session.updated"); err != nil {
		return nil, err
	}

	// 100ms ≈ 3200 字节一片，模拟流式送入
	const chunkBytes = 3200
	for off := 0; off < len(raw); off += chunkBytes {
		end := off + chunkBytes
		if end > len(raw) {
			end = len(raw)
		}
		if err := send(map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(raw[off:end]),
		}); err != nil {
			return nil, err
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err := send(map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		return nil, err
	}
	if err := waitFor("input_audio_buffer.committed"); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"type": "response.create"}); err != nil {
		return nil, err
	}

	// 收增量文本直到 response.done
	var text strings.Builder
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("asr 读转写: %w", err)
		}
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(msg, &ev)
		switch ev.Type {
		case "response.audio_transcript.delta":
			text.WriteString(ev.Delta)
		case "response.done":
			return ParseSpeakerTranscript(text.String()), nil
		case "error":
			return nil, fmt.Errorf("asr 服务端错误: %s", string(msg))
		}
	}
}

// speakerPrefix 匹配行首的 [说话人1] / [说话人 2] 等前缀。
var speakerPrefix = regexp.MustCompile(`^\s*\[说话人\s*(\d+)\s*\]\s*`)

// timePrefix 匹配模型偶尔输出的 [00:12] 时间码前缀，清洗掉。
var timePrefix = regexp.MustCompile(`^\s*\[\d{1,2}:\d{2}(?::\d{2})?\]\s*`)

// ParseSpeakerTranscript 把转写文本按 [说话人N] 前缀解析成片段（纯函数，可单测）。
// 无前缀的文本归为一段、标签为空。
func ParseSpeakerTranscript(text string) []TranscriptPiece {
	var pieces []TranscriptPiece
	var cur *TranscriptPiece
	for _, line := range strings.Split(text, "\n") {
		line = timePrefix.ReplaceAllString(strings.TrimSpace(line), "")
		if line == "" {
			continue
		}
		if m := speakerPrefix.FindStringSubmatch(line); m != nil {
			pieces = append(pieces, TranscriptPiece{
				SpeakerLabel: m[1],
				Text:         strings.TrimSpace(speakerPrefix.ReplaceAllString(line, "")),
			})
			cur = &pieces[len(pieces)-1]
		} else if cur != nil {
			// 无前缀的续行并入上一段
			cur.Text += " " + line
		} else {
			pieces = append(pieces, TranscriptPiece{Text: line})
			cur = &pieces[len(pieces)-1]
		}
	}
	// 过滤空文本
	out := pieces[:0]
	for _, p := range pieces {
		if p.Text != "" {
			out = append(out, p)
		}
	}
	return out
}
