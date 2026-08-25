// Package provider 抽象全部 AI 能力。业务代码只依赖接口，
// 具体实现（火山 Ark）可整体替换。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLMProvider 是大模型调用接口。
type LLMProvider interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

type ChatRequest struct {
	Model       string  // 模型名或 endpoint id（ep-xxx）
	System      string  // system prompt
	User        string  // 用户输入
	Temperature float64 // 0 表示用服务端默认
}

type ChatResponse struct {
	Content     string
	TotalTokens int
}

// ArkLLM 走 Ark 的 OpenAI 兼容 chat/completions 接口。
type ArkLLM struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewArkLLM(baseURL, apiKey string) *ArkLLM {
	return NewArkLLMWithTimeout(baseURL, apiKey, 60*time.Second)
}

// NewArkLLMWithTimeout 同 NewArkLLM 但可指定 HTTP 超时。抽取/分类等短调用用默认 60s；
// 报告这类「大 prompt + 结构化长输出」的生成（doubao thinking 模型 + 全天/全周数据）
// 常超 60s（实测 context deadline exceeded while awaiting headers），需更长超时。
func NewArkLLMWithTimeout(baseURL, apiKey string, timeout time.Duration) *ArkLLM {
	return &ArkLLM{baseURL: baseURL, apiKey: apiKey, client: &http.Client{Timeout: timeout}}
}

// NewArkLLMForReports 为报告引擎构造更长超时（180s）的客户端。
func NewArkLLMForReports(baseURL, apiKey string) *ArkLLM {
	return NewArkLLMWithTimeout(baseURL, apiKey, 180*time.Second)
}

type chatPayload struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *ArkLLM) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	msgs := []chatMessage{}
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: req.User})

	pl := chatPayload{Model: req.Model, Messages: msgs}
	if req.Temperature > 0 {
		pl.Temperature = &req.Temperature
	}
	body, _ := json.Marshal(pl)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var cr chatResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return ChatResponse{}, fmt.Errorf("ark llm 响应解析失败 (http %d): %s", resp.StatusCode, truncate(raw))
	}
	if cr.Error != nil {
		return ChatResponse{}, fmt.Errorf("ark llm 错误: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("ark llm 空响应 (http %d): %s", resp.StatusCode, truncate(raw))
	}
	return ChatResponse{Content: strings.TrimSpace(cr.Choices[0].Message.Content), TotalTokens: cr.Usage.TotalTokens}, nil
}

func truncate(b []byte) string {
	if len(b) > 500 {
		return string(b[:500]) + "..."
	}
	return string(b)
}
