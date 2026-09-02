package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArkLLMChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing bearer auth")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "doubao-seed-1.6-flash" {
			t.Errorf("model = %v", body["model"])
		}
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"content": "{\"ok\":true}"}}],
			"usage": {"total_tokens": 42}
		}`))
	}))
	defer srv.Close()

	p := NewArkLLM(srv.URL, "test-key")
	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:  "doubao-seed-1.6-flash",
		System: "你是助手",
		User:   "你好",
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != `{"ok":true}` {
		t.Fatalf("content = %s", resp.Content)
	}
	if resp.TotalTokens != 42 {
		t.Fatalf("tokens = %d", resp.TotalTokens)
	}
}

// TestArkLLMChatThinkingPayload 守护 NoThinking 的 wire 契约：默认不传 thinking
// （用服务端默认），NoThinking=true 时传 {"type":"disabled"}——doubao-seed 系默认
// 开思考，结构化小输出的调用靠此参数提速（2026-09-02 实测 ~10 倍，见 ChatRequest 注释）。
func TestArkLLMChatThinkingPayload(t *testing.T) {
	var gotThinking map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotThinking, _ = body["thinking"].(map[string]any)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}],"usage":{}}`))
	}))
	defer srv.Close()

	p := NewArkLLM(srv.URL, "test-key")
	// 默认（零值）：不传 thinking 字段
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", User: "hi"}); err != nil {
		t.Fatal(err)
	}
	if gotThinking != nil {
		t.Fatalf("默认请求不应带 thinking 字段，实际 %v", gotThinking)
	}
	// NoThinking：显式 disabled
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", User: "hi", NoThinking: true}); err != nil {
		t.Fatal(err)
	}
	if gotThinking == nil || gotThinking["type"] != "disabled" {
		t.Fatalf("NoThinking 请求应带 thinking.type=disabled，实际 %v", gotThinking)
	}
}
