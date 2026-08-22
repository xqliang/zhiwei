// spike: 验证用 stepaudio-2.5-realtime（Step Plan WSS）+ diarization prompt
// 让模型按 [spkN][开始-结束秒]内容 模板输出（秒·2 位小数 + spk0/spk1）。
//
// 用法: set -a; source .env; set +a; go run ./cmd/spike/asr-realtime testdata/speech.wav
//
// 目的：文件 ASR(/file/submit)配额超限后，看 realtime 端点（可能不同配额）能否用
// prompt 直接产出"说话人+时间戳+内容"。跑通后把 prompt + 解析固化进 provider。
package main

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

// asrInstructions 用户提供的 diarization prompt（任务式表述，非角色设定——避免模型复读指令）。
const asrInstructions = `我有个录音，你帮我整理一下。主要就是把不同人的发言时间都给我找出来，告诉我谁在什么时候说了话。
你需要按着说话人、时间戳的格式来排列结果。其中时间戳包括开始时间和结束时间，要注意开始时间和结束时间的单位为秒，可以精确到小数点后两位，说话人可以按着出场顺序标记成 spk0、spk1、spk2 等等来代替。
可以参考下面的模板：
[说话人][开始时间-结束时间]说话内容[说话人][开始时间-结束时间]说话内容...
注意你只能按着模板输出结果，请勿输出其它无关的信息和内容。`

// timedSegPattern 匹配 [spk0]0.00-4.15 或 [spk0][0.00-4.15]（时间段方括号可选，模型常省略）。
var timedSegPattern = regexp.MustCompile(`\[spk(\d+)\]\[?(\d+(?:\.\d+)?)-(\d+(?:\.\d+)?)\]?`)

type piece struct {
	Speaker string
	Text    string
	StartMS int64
	EndMS   int64
}

// parseTimedTranscript 把模型输出按 [spkN][start-end]模板切成片段。
func parseTimedTranscript(text string) []piece {
	text = strings.TrimSpace(text)
	var out []piece
	idx := 0
	for idx < len(text) {
		loc := timedSegPattern.FindStringIndex(text[idx:])
		if loc == nil {
			break
		}
		m := timedSegPattern.FindStringSubmatch(text[idx:])
		// m[1]=spk序号 m[2]=开始秒 m[3]=结束秒
		startMS := secToMS(m[2])
		endMS := secToMS(m[3])
		segStart := idx + loc[1] // 模板结束位置之后是说话内容
		// 下一个 [spk 的位置（或末尾）
		next := idx + loc[1]
		nextLoc := timedSegPattern.FindStringIndex(text[segStart:])
		if nextLoc != nil {
			next = segStart + nextLoc[0]
		} else {
			next = len(text)
		}
		content := strings.TrimSpace(text[segStart:next])
		if content != "" {
			out = append(out, piece{Speaker: m[1], Text: content, StartMS: startMS, EndMS: endMS})
		}
		idx = next
	}
	return out
}

func secToMS(s string) int64 {
	var sec float64
	fmt.Sscanf(s, "%f", &sec)
	return int64(sec * 1000)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: source .env; go run ./cmd/spike/asr-realtime <wav>")
		os.Exit(1)
	}
	apiKey := os.Getenv("STEPFUN_API_KEY")
	if apiKey == "" {
		fmt.Println("STEPFUN_API_KEY 未设置"); os.Exit(1)
	}
	audio, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	model := "stepaudio-2.5-asr"
	if len(os.Args) > 2 {
		model = os.Args[2]
	}
	if len(audio) <= 44 {
		panic("音频过短")
	}
	raw := audio[44:] // 跳过 44 字节 wav 头拿裸 pcm

	endpoint := "wss://api.stepfun.com/step_plan/v1/realtime?model=" + model
	fmt.Println("model:", model)
	conn, resp, err := websocket.DefaultDialer.DialContext(context.Background(), endpoint,
		map[string][]string{"Authorization": {"Bearer " + apiKey}})
	if err != nil {
		if resp != nil {
			fmt.Printf("连接失败 http %d: %v\n", resp.StatusCode, err)
		} else {
			fmt.Printf("连接失败: %v\n", err)
		}
		os.Exit(1)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(180 * time.Second))

	send := func(v any) error {
		b, _ := json.Marshal(v)
		return conn.WriteMessage(websocket.TextMessage, b)
	}
	waitFor := func(want string) error {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return fmt.Errorf("读事件(等 %s): %w", want, err)
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
				return fmt.Errorf("服务端错误: %s", string(msg))
			}
		}
	}

	if err := waitFor("session.created"); err != nil {
		panic(err)
	}
	if err := send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"modalities":         []string{"text"},
			"instructions":       asrInstructions,
			"input_audio_format": "pcm16",
		},
	}); err != nil {
		panic(err)
	}
	if err := waitFor("session.updated"); err != nil {
		panic(err)
	}

	const chunkBytes = 3200 // 100ms ≈ 3200B
	for off := 0; off < len(raw); off += chunkBytes {
		end := off + chunkBytes
		if end > len(raw) {
			end = len(raw)
		}
		if err := send(map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(raw[off:end]),
		}); err != nil {
			panic(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err := send(map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		panic(err)
	}
	if err := waitFor("input_audio_buffer.committed"); err != nil {
		panic(err)
	}
	if err := send(map[string]any{"type": "response.create"}); err != nil {
		panic(err)
	}

	var text strings.Builder
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			panic(fmt.Errorf("读转写: %w", err))
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
			fmt.Println("===== 模型原始输出 =====")
			fmt.Println(text.String())
			fmt.Println("===== 解析片段 =====")
			pieces := parseTimedTranscript(text.String())
			if len(pieces) == 0 {
				fmt.Println("(未解析到 [spkN][start-end] 片段——模型未按模板输出)")
			}
			for i, p := range pieces {
				fmt.Printf("[%d] spk%s %d-%dms %s\n", i, p.Speaker, p.StartMS, p.EndMS, p.Text)
			}
			return
		case "error":
			panic(fmt.Errorf("服务端错误: %s", string(msg)))
		}
	}
}
