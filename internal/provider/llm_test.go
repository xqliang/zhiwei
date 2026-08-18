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
