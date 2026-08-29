package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ComicProvider 文生图接口：一个 prompt → 一张图（多格漫画整图）的 base64。
type ComicProvider interface {
	Generate(ctx context.Context, prompt string) (imageB64 string, err error)
}

// SeedreamComic 调火山方舟 Seedream 文生图（1 次调用出整张多格漫画）。
type SeedreamComic struct {
	baseURL, apiKey, model string
	client                 *http.Client
}

func NewSeedreamComic(baseURL, apiKey, model string) *SeedreamComic {
	return &SeedreamComic{baseURL: baseURL, apiKey: apiKey, model: model, client: &http.Client{Timeout: 180 * time.Second}}
}

type comicReq struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	Watermark      bool   `json:"watermark"`
}

type comicResp struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
		URL     string `json:"url"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate 1 次调用出整张多格漫画（b64_json）。prompt 含多格描述 + 统一风格指令。
func (p *SeedreamComic) Generate(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(comicReq{
		Model: p.model, Prompt: prompt, Size: "1792x1024",
		ResponseFormat: "b64_json", Watermark: false,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/images/generations", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var cr comicResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("响应解析(http %d): %s", resp.StatusCode, truncate(raw))
	}
	if cr.Error != nil {
		return "", fmt.Errorf("seedream 错误: %s", cr.Error.Message)
	}
	if len(cr.Data) == 0 || cr.Data[0].B64JSON == "" {
		return "", fmt.Errorf("空响应(http %d): %s", resp.StatusCode, truncate(raw))
	}
	return cr.Data[0].B64JSON, nil
}
