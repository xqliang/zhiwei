package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// TranscriptPiece 是 ASR 输出的最小转写单元：一段话 + 说话人标签。
type TranscriptPiece struct {
	SpeakerLabel string // ASR 标签（"1"/"2"/...），空表示未知/单人
	Text         string
	StartMS      int64 // 段起始毫秒。文件 ASR(stepaudio-2.5-asr)返回真实时间戳；旧 realtime 方案恒 0
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

// asrInstructions 转写指令。任务式表述（非"你是XX引擎"角色设定——实测角色设定会让模型复读指令）。
// 让模型按 [spkN][开始秒-结束秒]说话内容 模板输出：说话人按出场顺序标 spk0/spk1…，
// 并强约束「同人同 spk 编号、按音色判同、拿不准归并而非新增」——缓解 realtime prompt 式
// diarization 把一人拆成多个 spk 的问题（speaker stage 仍会做声纹聚类兜底，见 stage_speaker.go）。
// 时间戳为秒（2 位小数），便于切片与声纹解析。配 ParseTimedSpeakerTranscript 解析。
// 注：模型常省略时间段的第二层方括号（输出 [spk0]0.00-4.15内容），解析正则已兼容。
const asrInstructions = `我有个录音，你帮我整理一下。主要就是把不同人的发言时间都给我找出来，告诉我谁在什么时候说了话。
你需要按着说话人、时间戳的格式来排列结果。其中时间戳包括开始时间和结束时间，要注意开始时间和结束时间的单位为秒，可以精确到小数点后两位，说话人可以按着出场顺序标记成 spk0、spk1、spk2 等等来代替。
重要：同一个人必须始终使用同一个 spk 编号，不要把同一个人的发言拆成多个不同的 spk 编号。判断是否同一个人主要看音色（嗓音特征）：音色相同或相近的，即使中间被别人插话打断、或内容不连续，也是同一个人，必须复用已有的 spk 编号，不能新建。只有音色明显不同的才是另一个人、才用新的 spk 编号。拿不准两个人是不是同一人时，宁可归并为已有编号，也不要轻易新增 spk 编号。整个录音里 spk 编号的数量应尽量少，等于实际不同的人数。
可以参考下面的模板：
[说话人][开始时间-结束时间]说话内容[说话人][开始时间-结束时间]说话内容...
注意你只能按着模板输出结果，请勿输出其它无关的信息和内容。`

// Transcribe 实现 ASRProvider：建会话 → 配置 → 分片喂数学频 → 收转写文本 → 解析说话人。
func (p *StepFunASR) Transcribe(ctx context.Context, audioPath string) ([]TranscriptPiece, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("asr: STEPFUN_API_KEY 未设置（检查 .env 是否已 source）")
	}
	audio, err := os.ReadFile(audioPath)
	if err != nil {
		return nil, fmt.Errorf("读取音频: %w", err)
	}
	if len(audio) <= 44 {
		return nil, fmt.Errorf("音频文件过短: %s", audioPath)
	}
	raw := audio[44:] // 跳过 44 字节 wav 头，拿裸 pcm

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, p.Endpoint,
		map[string][]string{"Authorization": {"Bearer " + p.APIKey}})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("asr 连接 (http %d): %w", resp.StatusCode, err)
		}
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
			return ParseTimedSpeakerTranscript(text.String()), nil
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

// TOSUploader 抽象 TOS 上传/删除，便于 ASR provider 解耦存储 + 测试注入。
// 生产实现见 internal/storage/tos.go（后续任务）。
type TOSUploader interface {
	UploadWAV(ctx context.Context, localPath, key string) (presignedURL string, err error)
	Delete(ctx context.Context, key string) error
}

// StepFunFileASR 用 StepFun 异步文件 ASR（POST /v1/audio/asr/file/submit + /file/query）做转写。
// 原生返回每句 ms 级 start/end_time + speaker.id(spk_N)，见 docs/superpowers/specs/asr-protocol-notes.md。
// 音频需公网可访问：由 TOSUploader 上传后返回 presigned GET URL 喂给 StepFun。
type StepFunFileASR struct {
	BaseURL      string              // https://api.c.ibasemind.com/v1（默认）/ https://api.stepfun.com/v1（生产）
	APIKey       string              // File ASR 专用 Key（STEPFUN_ASR_FILE_API_KEY）
	Model        string              // stepaudio-2.5-asr
	TOS          TOSUploader         // 音频上传/删除的存储抽象
	KeyPrefix    string              // TOS 对象 key 前缀，如 zhiwei/
	sleep        func(time.Duration) // 可注入跳过真实 sleep（测试用）
	pollInterval time.Duration       // 轮询 query 的间隔
}

// NewStepFunFileASR 构造。sleep 为 nil 时用 time.Sleep；测试可注入假 sleep。
func NewStepFunFileASR(baseURL, apiKey, model string, tos TOSUploader, sleep func(time.Duration)) *StepFunFileASR {
	if sleep == nil {
		sleep = time.Sleep
	}
	return &StepFunFileASR{
		BaseURL: baseURL, APIKey: apiKey, Model: model, TOS: tos,
		KeyPrefix: "zhiwei/", sleep: sleep, pollInterval: 2 * time.Second,
	}
}

// Transcribe 实现 ASRProvider：上传 TOS 拿 presigned URL → submit → 轮询 query → 解析。
// defer 删 TOS 对象（best-effort，删失败不影响转写结果）。
func (p *StepFunFileASR) Transcribe(ctx context.Context, audioPath string) ([]TranscriptPiece, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("asr: STEPFUN_ASR_FILE_API_KEY 未设置")
	}
	// key 用纳秒时间戳保证同一前缀下不重名
	key := p.KeyPrefix + fmt.Sprintf("%d.wav", time.Now().UnixNano())
	url, err := p.TOS.UploadWAV(ctx, audioPath, key)
	if err != nil {
		return nil, fmt.Errorf("tos 上传: %w", err)
	}
	defer func() { _ = p.TOS.Delete(ctx, key) }()

	taskID, err := p.submit(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("asr submit: %w", err)
	}
	raw, err := p.poll(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("asr query: %w", err)
	}
	return ParseFileASRResult(raw), nil
}

// submit 提交转写任务，返回 task_id。音频参数固定 16k mono wav（调用方保证格式）。
func (p *StepFunFileASR) submit(ctx context.Context, audioURL string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"audio":   map[string]any{"format": "wav", "channel": 1, "rate": 16000, "url": audioURL},
		"request": map[string]any{"model_name": p.Model, "show_utterances": true, "enable_speaker_info": true},
	})
	resp, err := p.do(ctx, "POST", p.BaseURL+"/audio/asr/file/submit", body)
	if err != nil {
		return "", err
	}
	var r struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(resp, &r); err != nil || r.TaskID == "" {
		return "", fmt.Errorf("submit 响应非法: %s", resp)
	}
	return r.TaskID, nil
}

// poll 轮询 query 直到任务结束（SUCCEEDED/FAILED）或超时。
// 返回成功时的原始响应，交给 ParseFileASRResult 解析。
func (p *StepFunFileASR) poll(ctx context.Context, taskID string) ([]byte, error) {
	maxAttempts := 150 // 2s × 150 ≈ 5min
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			p.sleep(p.pollInterval)
		}
		body, _ := json.Marshal(map[string]string{"task_id": taskID})
		raw, err := p.do(ctx, "POST", p.BaseURL+"/audio/asr/file/query", body)
		if err != nil {
			continue // 单次网络抖动不致命，继续轮询
		}
		var r fileASRQueryResponse
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		if r.Status == "FAILED" {
			return nil, fmt.Errorf("asr failed: %s", raw)
		}
		if r.Status != "PENDING" && r.Status != "RUNNING" {
			return raw, nil // SUCCEEDED 等终态
		}
	}
	return nil, fmt.Errorf("asr 超时（task=%s）", taskID)
}

// do 发一个带 Bearer 鉴权的 JSON 请求，返回响应体原始字节。
func (p *StepFunFileASR) do(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// timedSegPattern 匹配模型按 prompt 输出的说话人时间段标记。
// 兼容两种写法：[spk0]0.00-4.15（模型常省略时间段方括号）与 [spk0][0.00-4.15]。
// 组：g1=spk 序号，g2=开始秒，g3=结束秒。
var timedSegPattern = regexp.MustCompile(`\[spk(\d+)\]\[?(\d+(?:\.\d+)?)-(\d+(?:\.\d+)?)\]?`)

// ParseTimedSpeakerTranscript 把 realtime 模型按 prompt 输出的
// [spkN][开始秒-结束秒]说话内容 文本解析成片段（纯函数，可单测）。
// spk0 → SpeakerLabel "0"；秒（2 位小数）→ StartMS/EndMS（毫秒）；
// 两个标记之间的文本即该段说话内容；无标记前缀的文本被忽略（模型应全程按模板输出）。
func ParseTimedSpeakerTranscript(text string) []TranscriptPiece {
	text = strings.TrimSpace(text)
	matches := timedSegPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []TranscriptPiece
	for i, m := range matches {
		// m: [全匹配起止, g1起止, g2起止, g3起止]
		spk := text[m[2]:m[3]]
		startMS := secStrToMS(text[m[4]:m[5]])
		endMS := secStrToMS(text[m[6]:m[7]])
		contentStart := m[1] // 当前标记结束位置
		var contentEnd int
		if i+1 < len(matches) {
			contentEnd = matches[i+1][0] // 下一个标记起点
		} else {
			contentEnd = len(text)
		}
		content := strings.TrimSpace(text[contentStart:contentEnd])
		if content != "" {
			out = append(out, TranscriptPiece{SpeakerLabel: spk, Text: content, StartMS: startMS, EndMS: endMS})
		}
	}
	return out
}

// secStrToMS 把秒字符串（如 "4.15"）转毫秒（int64）。解析失败返回 0。
func secStrToMS(s string) int64 {
	var sec float64
	if _, err := fmt.Sscanf(s, "%f", &sec); err != nil {
		return 0
	}
	return int64(sec * 1000)
}

// fileASRQueryResponse 对应 StepFun 异步文件 ASR POST /v1/audio/asr/file/query 的响应。
// 字段见 docs/superpowers/specs/asr-protocol-notes.md（2026-08-22 更新）与设计 §2.1：
// result[].utterances[] 每句含 start_time/end_time（毫秒）+ speaker.id（spk_N，任务内稳定）。
type fileASRQueryResponse struct {
	Status string `json:"status"` // SUCCEEDED | PENDING | RUNNING | FAILED
	Error  *struct {
		Stage   string `json:"stage"`
		Message string `json:"message"`
	} `json:"error"`
	Duration float64 `json:"duration"`
	Result   []struct {
		Text       string `json:"text"`
		Utterances []struct {
			Text      string `json:"text"`
			StartTime int    `json:"start_time"` // 毫秒
			EndTime   int    `json:"end_time"`   // 毫秒
			Speaker   *struct {
				ID string `json:"id"` // spk_1、spk_2 …（同一任务内稳定，跨任务不保证）
			} `json:"speaker"`
		} `json:"utterances"`
	} `json:"result"`
}

// ParseFileASRResult 把 StepFun 异步文件 ASR 的 /file/query 响应解析成转写片段（纯函数，可单测）。
// speaker.id 形如 "spk_1" → 去前缀得 "1"；speaker 字段缺失（未开 enable_speaker_info）→ 空 label；
// start_time/end_time（ms）→ StartMS/EndMS。非法 JSON 或空结果返回 nil，不 panic。
func ParseFileASRResult(raw []byte) []TranscriptPiece {
	var resp fileASRQueryResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil
	}
	var out []TranscriptPiece
	for _, r := range resp.Result {
		for _, u := range r.Utterances {
			p := TranscriptPiece{
				Text: u.Text, StartMS: int64(u.StartTime), EndMS: int64(u.EndTime),
			}
			if u.Speaker != nil {
				p.SpeakerLabel = strings.TrimPrefix(u.Speaker.ID, "spk_")
			}
			if p.Text != "" { // 过滤空文本
				out = append(out, p)
			}
		}
	}
	return out
}
