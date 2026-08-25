// Package voiceprint 封装声纹 sidecar 的 HTTP 调用。
// sidecar 契约见 docs/superpowers/specs/2026-08-22-speaker-voiceprint-design.md §6.1。
package voiceprint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"zhiwei/internal/ids"
)

// SearchResult 一次 1:N 检索的结果（top-2）。
// Matched 表示 sidecar 是否找到 top-1（库非空），不代表阈值通过——
// 命中判定统一在 Go 侧用 Matched()（两级规则）做。
type SearchResult struct {
	SpeakerID      ids.ID
	Distance       float64 // top-1 相似度（L2 归一向量的内积 = 余弦）
	SecondDistance float64 // top-2 相似度（库中向量 <2 个时为 0），区分性弱命中规则用
	Matched        bool    // 是否找到 top-1（false = 空库）
}

// Client 声纹 sidecar 客户端接口（pipeline/api 注入，测试可 mock）。
type Client interface {
	// Embed 把一段音频抽成 256 维声纹向量。
	Embed(ctx context.Context, audioPath string) ([]float32, error)
	// Search 用向量检索最相近的 top-2 说话人。matched 表示 sidecar 是否找到 top-1，
	// 不代表阈值通过 —— 阈值判定在 Go 侧 pipeline 用 Matched() 比较后决定。
	Search(ctx context.Context, vec []float32) (SearchResult, error)
	// Add 把向量登记到某个说话人名下（自动建档）。
	Add(ctx context.Context, vec []float32, id ids.ID) error
	// Remove 删除某个说话人的全部声纹（删除说话人时调用）。
	Remove(ctx context.Context, id ids.ID) error
}

// httpClient 是 Client 的默认 HTTP 实现。
type httpClient struct {
	BaseURL string
	hc      *http.Client
}

// NewClient 构造一个指向 sidecar BaseURL 的客户端。
// localhost 请求不走系统代理（避免 http_proxy 环境变量导致 sidecar 调用失败）。
func NewClient(baseURL string) Client {
	return &httpClient{
		BaseURL: baseURL,
		hc: &http.Client{
			Transport: &http.Transport{
				Proxy: func(req *http.Request) (*url.URL, error) {
					// sidecar 走本地回环，绕过系统代理
					if strings.HasPrefix(req.URL.Host, "127.0.0.1") || strings.HasPrefix(req.URL.Host, "localhost") {
						return nil, nil
					}
					return http.ProxyFromEnvironment(req)
				},
			},
		},
	}
}

// post 统一处理 JSON 编解码、请求发送与错误封装：
// body 序列化为请求体，out 非空时把响应体反序列化进去；
// HTTP >=300 视为错误并带上响应内容，便于排查 sidecar 侧问题。
func (c *httpClient) post(ctx context.Context, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("voiceprint %s: http %d: %s", path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("voiceprint %s 解析: %w", path, err)
		}
	}
	return nil
}

func (c *httpClient) Embed(ctx context.Context, audioPath string) ([]float32, error) {
	var out struct {
		Vector []float32 `json:"vector"`
	}
	if err := c.post(ctx, "/embed", map[string]string{"audio_path": audioPath}, &out); err != nil {
		return nil, err
	}
	return out.Vector, nil
}

func (c *httpClient) Search(ctx context.Context, vec []float32) (SearchResult, error) {
	// 注意：speaker_id 用 int64 中转，绕过 ids.ID 的自定义 JSON（sidecar 返回裸数字）。
	var out struct {
		SpeakerID     int64   `json:"speaker_id"`
		Distance      float64 `json:"distance"`
		SecondDistance float64 `json:"second_distance"` // 旧版 sidecar 无此字段 → 0（gap 规则退化为仅看 top1）
		Matched       bool    `json:"matched"`
	}
	if err := c.post(ctx, "/search", map[string][]float32{"vector": vec}, &out); err != nil {
		return SearchResult{}, err
	}
	return SearchResult{
		SpeakerID:      ids.ID(out.SpeakerID),
		Distance:       out.Distance,
		SecondDistance: out.SecondDistance,
		Matched:        out.Matched,
	}, nil
}

func (c *httpClient) Add(ctx context.Context, vec []float32, id ids.ID) error {
	return c.post(ctx, "/add", map[string]any{"vector": vec, "speaker_id": id.Int64()}, nil)
}

func (c *httpClient) Remove(ctx context.Context, id ids.ID) error {
	return c.post(ctx, "/remove", map[string]int64{"speaker_id": id.Int64()}, nil)
}
