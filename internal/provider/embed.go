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

// EmbeddingProvider 是向量化接口。
type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ArkEmbed 走 Ark OpenAI 兼容 /embeddings 接口。
type ArkEmbed struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func NewArkEmbed(baseURL, apiKey, model string) *ArkEmbed {
	return &ArkEmbed{baseURL: baseURL, apiKey: apiKey, model: model, client: &http.Client{Timeout: 30 * time.Second}}
}

type embedResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *ArkEmbed) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	// Ark 单次请求有输入条数上限，按 16 条分批
	var all [][]float32
	for i := 0; i < len(texts); i += 16 {
		end := i + 16
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := p.embedBatch(ctx, texts[i:end])
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}
	return all, nil
}

func (p *ArkEmbed) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": p.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/embeddings", bytes.NewReader(body))
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

	var er embedResp
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("ark embed 响应解析失败 (http %d): %s", resp.StatusCode, truncate(raw))
	}
	if er.Error != nil {
		return nil, fmt.Errorf("ark embed 错误: %s", er.Error.Message)
	}
	out := make([][]float32, len(er.Data))
	for i, d := range er.Data {
		out[i] = d.Embedding
	}
	return out, nil
}
