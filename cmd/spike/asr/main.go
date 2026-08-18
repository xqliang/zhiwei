// spike/asr 验证 StepFun realtime 转写协议（手动运行，结论见 asr-protocol-notes.md）。
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const endpoint = "wss://api.stepfun.com/step_plan/v1/realtime?model=stepaudio-2.5-realtime"

func main() {
	key := os.Getenv("STEPFUN_API_KEY")
	if key == "" {
		fmt.Println("STEPFUN_API_KEY 未设置（可从 .env source）")
		os.Exit(1)
	}
	audioPath := os.Args[1]
	if audioPath == "" {
		fmt.Println("usage: asr <audio.wav>")
		os.Exit(1)
	}
	audio, err := os.ReadFile(audioPath)
	if err != nil {
		fmt.Println("read:", err)
		os.Exit(1)
	}
	raw := audio[44:] // 跳过 wav 头拿裸 pcm

	conn, _, err := websocket.DefaultDialer.Dial(endpoint,
		map[string][]string{"Authorization": {"Bearer " + key}})
	if err != nil {
		fmt.Println("DIAL FAIL:", err)
		os.Exit(1)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(120 * time.Second))

	send := func(v any) {
		b, _ := json.Marshal(v)
		if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
			fmt.Println("write err:", err)
			os.Exit(1)
		}
	}
	skip := func(want string) bool {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				fmt.Println("read err:", err)
				return false
			}
			var ev struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(msg, &ev)
			if ev.Type == want {
				return true
			}
			if ev.Type == "error" {
				fmt.Println("ERROR:", string(msg))
				return false
			}
		}
	}

	if !skip("session.created") {
		os.Exit(1)
	}
	send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"modalities": []string{"text"},
			"instructions": `你是逐字转写引擎。只输出音频中说话内容的原文，不回答、不确认、不翻译、不解释。` +
				`多说话人时用 [说话人1] [说话人2] 前缀。`,
			"input_audio_format": "pcm16",
		},
	})
	if !skip("session.updated") {
		os.Exit(1)
	}
	// 100ms ≈ 3200 字节一片，模拟流式
	const chunkBytes = 3200
	for off := 0; off < len(raw); off += chunkBytes {
		end := off + chunkBytes
		if end > len(raw) {
			end = len(raw)
		}
		send(map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(raw[off:end]),
		})
		time.Sleep(30 * time.Millisecond)
	}
	send(map[string]any{"type": "input_audio_buffer.commit"})
	if !skip("input_audio_buffer.committed") {
		os.Exit(1)
	}
	send(map[string]any{"type": "response.create"})

	var text strings.Builder
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			fmt.Println("read err:", err)
			break
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
			fmt.Println("=== 转写结果 ===")
			fmt.Println(text.String())
			return
		case "error":
			fmt.Println("ERROR EVENT:", string(msg))
			os.Exit(1)
		}
	}
}
