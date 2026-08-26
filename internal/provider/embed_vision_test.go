package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArkVisionEmbedShape(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		var req struct {
			Model string `json:"model"`
			Input []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) != 1 || req.Input[0].Type != "text" {
			t.Errorf("请求体 input 形状错: %+v", req.Input)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   map[string]any{"embedding": []float32{float32(len(req.Input[0].Text)), 1, 2}},
		})
	}))
	defer srv.Close()

	p := NewArkVisionEmbed(srv.URL, "k", "doubao-embedding-vision")
	out, err := p.Embed(context.Background(), []string{"aa", "bbbb"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 2 || len(out[0]) != 3 {
		t.Fatalf("应回 2 个向量、每个 3 维: %+v", out)
	}
	if out[0][0] != 2 || out[1][0] != 4 { // len("aa")=2, len("bbbb")=4 → 逐条单调用
		t.Errorf("应逐条调用 multimodal 端点: %+v", out)
	}
	for _, p := range gotPaths {
		if p != "/embeddings/multimodal" {
			t.Errorf("端点应为 /embeddings/multimodal, got %q", p)
		}
	}
}

func TestArkVisionEmbedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
	}))
	defer srv.Close()
	p := NewArkVisionEmbed(srv.URL, "k", "m")
	if _, err := p.Embed(context.Background(), []string{"x"}); err == nil {
		t.Error("error 响应应返回 err")
	}
}
