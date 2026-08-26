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

// ArkVisionEmbed 走 Ark 多模态向量端点 /embeddings/multimodal（doubao-embedding-vision）。
// 与文本版 ArkEmbed 的关键差异（实测 2026-08-26）：input 是类型化对象数组、响应 data 是
// 单个对象（一次一个向量、无服务端批量），故 Embed 对 texts 逐条单调用（有界并发）。
type ArkVisionEmbed struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func NewArkVisionEmbed(baseURL, apiKey, model string) *ArkVisionEmbed {
	return &ArkVisionEmbed{baseURL: baseURL, apiKey: apiKey, model: model,
		client: &http.Client{Timeout: 30 * time.Second}}
}

type visionEmbedResp struct {
	Data struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed 逐条向量化（multimodal 端点一次一个向量）。有界并发 4，保序返回。
// 任一条失败即整体失败（调用方 backfill 会跳过该批、下轮重试）。
func (p *ArkVisionEmbed) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	sem := make(chan struct{}, 4)
	errCh := make(chan error, len(texts))
	done := make(chan struct{}, len(texts))
	for i, t := range texts {
		sem <- struct{}{}
		go func(i int, t string) {
			defer func() { <-sem; done <- struct{}{} }()
			v, err := p.embedOne(ctx, t)
			if err != nil {
				errCh <- err
				return
			}
			out[i] = v
		}(i, t)
	}
	for range texts {
		<-done
	}
	select {
	case err := <-errCh:
		return nil, err
	default:
		return out, nil
	}
}

func (p *ArkVisionEmbed) embedOne(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{
		"model": p.model,
		"input": []map[string]any{{"type": "text", "text": text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/embeddings/multimodal", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var er visionEmbedResp
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("ark vision embed 响应解析失败 (http %d): %s", resp.StatusCode, truncate(raw))
	}
	if er.Error != nil {
		return nil, fmt.Errorf("ark vision embed 错误: %s", er.Error.Message)
	}
	if len(er.Data.Embedding) == 0 {
		return nil, fmt.Errorf("ark vision embed 空向量 (http %d)", resp.StatusCode)
	}
	return er.Data.Embedding, nil
}
