package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// 引擎标识（agent_config.search_engine 的合法值）。
const (
	EngineAuto   = "auto"       // 免 key 链 Bing→DDG；配了 key 再兜底 Tavily
	EngineBing   = "bing"       // Bing 网页版 SERP（免 key）
	EngineDDG    = "duckduckgo" // DuckDuckGo lite SERP（免 key）
	EngineTavily = "tavily"     // Tavily API（需 search_api_key）
)

// ValidEngine 判断引擎标识合法（handler 校验入参用）。
func ValidEngine(s string) bool {
	return s == EngineAuto || s == EngineBing || s == EngineDDG || s == EngineTavily
}

// Result 是一条搜索结果（web_search 工具的返回项）。
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Searcher 联网搜索器。三个 URL 字段默认指向真实引擎，测试可覆写指向 httptest 假 SERP；
// HTTP 注入普通 client 可绕过 SSRF 拨号（仅测试；生产用 NewSearcher 的安全 client）。
type Searcher struct {
	HTTP      *http.Client
	BingURL   string // 默认 https://www.bing.com/search
	DDGURL    string // 默认 https://lite.duckduckgo.com/lite/
	TavilyURL string // 默认 https://api.tavily.com/search
}

// NewSearcher 构造生产用搜索器（复用 SSRF 安全 client）。
func NewSearcher() *Searcher {
	return &Searcher{
		HTTP:      safeClient(),
		BingURL:   "https://www.bing.com/search",
		DDGURL:    "https://lite.duckduckgo.com/lite/",
		TavilyURL: "https://api.tavily.com/search",
	}
}

// client 返回可用 http.Client（未注入时现建安全 client）。
func (s *Searcher) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return safeClient()
}

// Search 按引擎搜索。limit 归一到 [1,10]（默认 5）。
//   - 指定引擎：只跑该引擎，失败即返回错误。
//   - auto：Bing→DDG（免 key 链，任一成功即返回）；都失败且给了 apiKey 再试 Tavily。
//
// 免 key 引擎解析 HTML SERP——页面结构改版会失配（已知取舍：失败即降级/报错，
// 配 Tavily key 走稳定 JSON API 兜底）。
func (s *Searcher) Search(ctx context.Context, engine, apiKey, query string, limit int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query 不能为空")
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	switch engine {
	case EngineBing:
		return s.bing(ctx, query, limit)
	case EngineDDG:
		return s.ddg(ctx, query, limit)
	case EngineTavily:
		return s.tavily(ctx, apiKey, query, limit)
	case "", EngineAuto:
		if rs, err := s.bing(ctx, query, limit); err == nil && len(rs) > 0 {
			return rs, nil
		}
		if rs, err := s.ddg(ctx, query, limit); err == nil && len(rs) > 0 {
			return rs, nil
		}
		if apiKey != "" {
			return s.tavily(ctx, apiKey, query, limit)
		}
		return nil, fmt.Errorf("免 key 搜索引擎均失败（可稍后重试，或在设置里配置 Tavily API key）")
	default:
		return nil, fmt.Errorf("未知搜索引擎: %q", engine)
	}
}

// bing 抓 Bing 网页版 SERP：解析 <li class="b_algo">，h2>a 取标题+链接，其后首个 p 取摘要。
func (s *Searcher) bing(ctx context.Context, query string, limit int) ([]Result, error) {
	u := s.BingURL + "?q=" + url.QueryEscape(query) + "&count=10&mkt=zh-CN"
	doc, err := s.fetchDoc(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("bing: %w", err)
	}
	var out []Result
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "li" && hasClassToken(n, "b_algo") && len(out) < limit {
			var r Result
			if a := firstDesc(n, "h2", "a"); a != nil {
				r.Title = nodeText(a)
				r.URL = attrOf(a, "href")
			}
			if p := firstDesc(n, "p", ""); p != nil {
				r.Snippet = nodeText(p)
			}
			if strings.HasPrefix(r.URL, "http") {
				out = append(out, r)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if len(out) == 0 {
		return nil, fmt.Errorf("bing 未解析到结果（页面结构可能已变）")
	}
	return out, nil
}

// ddg 抓 DuckDuckGo lite SERP：a.result-link 取标题+链接（解开 uddg 跳转包装），
// 其后（文档序）td.result-snippet 取摘要。
func (s *Searcher) ddg(ctx context.Context, query string, limit int) ([]Result, error) {
	u := s.DDGURL + "?q=" + url.QueryEscape(query)
	doc, err := s.fetchDoc(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo: %w", err)
	}
	var out []Result
	pendingIdx := -1 // 最近一条 result-link 的下标，等待其摘要
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch {
			case n.Data == "a" && hasClassToken(n, "result-link"):
				if len(out) >= limit {
					return
				}
				r := Result{Title: nodeText(n), URL: unwrapDDG(attrOf(n, "href"))}
				out = append(out, r)
				pendingIdx = len(out) - 1
			case pendingIdx >= 0 && (n.Data == "td" || n.Data == "a") && hasClassToken(n, "result-snippet"):
				if out[pendingIdx].Snippet == "" {
					out[pendingIdx].Snippet = nodeText(n)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if len(out) == 0 {
		return nil, fmt.Errorf("duckduckgo 未解析到结果（页面结构可能已变）")
	}
	return out, nil
}

// tavily 走 Tavily JSON API（Authorization: Bearer <key>），结果字段直接映射。
func (s *Searcher) tavily(ctx context.Context, apiKey, query string, limit int) ([]Result, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("tavily 需要在设置里配置 API key")
	}
	body, _ := json.Marshal(map[string]any{"query": query, "max_results": limit})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.TavilyURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("tavily HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var tr struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, fetchMaxBody)).Decode(&tr); err != nil {
		return nil, fmt.Errorf("tavily 响应解析失败: %w", err)
	}
	out := make([]Result, 0, len(tr.Results))
	for _, r := range tr.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tavily 返回 0 结果")
	}
	return out, nil
}

// fetchDoc GET 一个 SERP 并解析成 DOM（带 UA、2MB 限读、状态码校验）。
func (s *Searcher) fetchDoc(ctx context.Context, rawURL string) (*html.Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBody))
	if err != nil {
		return nil, err
	}
	return html.Parse(strings.NewReader(string(body)))
}

// unwrapDDG 解开 DuckDuckGo 的跳转包装（uddg=<encoded> 参数），非包装链接原样返回。
func unwrapDDG(raw string) string {
	if !strings.Contains(raw, "uddg=") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Scheme == "" { // lite 页里常是协议相对 //duckduckgo.com/l/...
		u.Scheme = "https"
	}
	if target := u.Query().Get("uddg"); target != "" {
		if dec, err := url.QueryUnescape(target); err == nil {
			return dec
		}
		return target
	}
	return raw
}

// ---- DOM 小工具（x/net/html 无选择器，自写最小遍历）----

// hasClassToken 判断 class 属性含指定 token（class 常是 "b_algo b_xxx" 多类并列）。
func hasClassToken(n *html.Node, class string) bool {
	for _, f := range strings.Fields(attrOf(n, "class")) {
		if f == class {
			return true
		}
	}
	return false
}

// attrOf 取元素属性值（无则空串）。
func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// firstDesc 在 n 的后代里找第一个 tag 元素；sub 非空时再在其内找第一个 sub 元素。
func firstDesc(n *html.Node, tag, sub string) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if found != nil {
			return
		}
		if c.Type == html.ElementNode && c.Data == tag {
			found = c
			return
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	if found == nil || sub == "" {
		return found
	}
	tag = sub
	target := found
	found = nil
	walk(target)
	return found
}
