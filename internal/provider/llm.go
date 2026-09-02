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
	// NoThinking 显式关闭端侧思考（doubao-seed 系混合推理模型默认开启）。结构化小输出
	// 的调用（实体纠错/名字推断）不需要隐性推理：2026-09-02 实测 doubao-seed-1-6-flash，
	// 默认思考单次 9~26s、completion ~1111 tokens（思考吞掉，有效答案仅 ~82）；
	// 显式 disabled 后 1.3~1.6s、completion 82 tokens——约 10 倍提速且纠错输出等价
	// （找出的 edits 与置信度一致，四重门控仍在）。按调用方 opt-in：报告/抽取等可能
	// 受益于推理的长输出调用不设此字段。
	NoThinking bool
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
	// Thinking 端侧思考开关（Ark 扩展参数）。nil=不传（用服务端默认——doubao-seed
	// 系默认开启）；NoThinking 的请求传 {"type":"disabled"}。指针区分「不传」与「显式配置」。
	Thinking *thinkingConfig `json:"thinking,omitempty"`
}

type thinkingConfig struct {
	Type string `json:"type"` // "enabled" | "disabled" | "auto"
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
	if req.NoThinking {
		pl.Thinking = &thinkingConfig{Type: "disabled"}
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
