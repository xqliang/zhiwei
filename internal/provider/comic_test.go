package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 桩 Seedream 返回 b64_json，验 SeedreamComic 解析 + 请求体正确。
func TestSeedreamComicGenerate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if gotBody["response_format"] != "b64_json" {
			t.Errorf("response_format 应为 b64_json, got %v", gotBody["response_format"])
		}
		w.Write([]byte(`{"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()

	c := NewSeedreamComic(srv.URL, "k", "doubao-seedream-4-0-250828")
	b64, err := c.Generate(context.Background(), "一个漫画 prompt")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if b64 != "aGVsbG8=" {
		t.Errorf("b64=%q", b64)
	}
}

func TestSeedreamComicError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer srv.Close()
	c := NewSeedreamComic(srv.URL, "k", "m")
	if _, err := c.Generate(context.Background(), "x"); err == nil {
		t.Error("应返回 error")
	}
}
