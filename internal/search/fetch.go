// Package search 提供联网搜索与网页抓取（Phase 2）：
//   - Searcher：web_search 的引擎链（Bing/DuckDuckGo-lite 免 key + Tavily API key，见 search.go）
//   - Fetcher：web_fetch 的 URL 正文提取（本文件）
//
// SSRF 防护是硬约束：仅允许拨向公网 IP（拒绝环回/私网/链路本地等）。校验发生在
// net.Dialer.Control——即「实际拨号时」拿到解析后的真实地址——从根上防 DNS rebinding
// （先解析校验通过、真正连接时换内网 IP 的 TOCTOU）。测试通过注入普通 http.Client
// 绕过（httptest 监听 127.0.0.1）；生产装配只用 NewFetcher/NewSearcher（安全 client）。
package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	fetchTimeout = 10 * time.Second // 单页抓取总超时（含重定向）
	fetchMaxBody = 2 << 20          // 响应体读取上限 2MB（防超大页面拖垮内存）
	fetchMaxText = 8000             // 正文截断上限（rune 数，喂模型的窗口保护）
	// userAgent 常见浏览器 UA：不少站点对无 UA / Go 默认 UA 直接 403。
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
)

// blockedIP 判断该 IP 是否被禁止访问（SSRF 根防线）：环回/私网/链路本地/组播/未指定
// 一律拒绝。Go 的 IsPrivate 同时覆盖 RFC1918（IPv4 私网）与 fc00::/7（IPv6 ULA）。
func blockedIP(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

// safeDialer 在「实际拨号」时校验目标 IP（Control 回调拿到的是解析后的真实地址）。
var safeDialer = &net.Dialer{
	Timeout: 5 * time.Second,
	Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("拆分拨号地址 %q: %w", address, err)
		}
		if ip := net.ParseIP(host); blockedIP(ip) {
			return fmt.Errorf("SSRF 防护：拒绝访问非公网地址 %s", host)
		}
		return nil
	},
}

// safeClient 是带 SSRF 安全拨号 + 超时 + 重定向上限的 http.Client（生产用）。
//
// 已知取舍：Transport 未设 Proxy 字段（刻意不读 HTTP(S)_PROXY 环境变量）——出网恒为直连。
// 若走 ProxyFromEnvironment，拨号目标是本机代理（如 127.0.0.1:7890），会被 SSRF 拨号
// 守卫拒绝，反而全断；故对被墙站点表现为超时而非走代理（如需代理支持属后续需求）。
func safeClient() *http.Client {
	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: &http.Transport{DialContext: safeDialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("重定向次数超限（>5）")
			}
			return checkPublicURL(req.URL)
		},
	}
}

// checkPublicURL 校验 URL 形态：仅 http/https 且 host 非空（IP 级校验由拨号 Control 兜底）。
func checkPublicURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("仅支持 http/https 的完整 URL")
	}
	return nil
}

// Fetcher 抓取网页正文。HTTP 可注入（测试用普通 client 绕过 SSRF 拨号）；
// 留空则现建安全 client。
type Fetcher struct {
	HTTP *http.Client
}

// NewFetcher 构造生产用抓取器（SSRF 安全拨号 + 10s 超时 + ≤5 跳重定向）。
func NewFetcher() *Fetcher { return &Fetcher{HTTP: safeClient()} }

// Page 是一次抓取的结果：最终 URL（重定向后）+ 标题 + 正文纯文本（已截断）。
type Page struct {
	URL   string
	Title string
	Text  string
}

// Fetch 抓取 rawURL 并提取正文纯文本：仅接受 text/html / text/plain 响应；
// HTML 剥掉 script/style 等与全部标签、折叠空白；正文截断到 fetchMaxText 字符。
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Page, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("解析 URL 失败: %w", err)
	}
	if err := checkPublicURL(u); err != nil {
		return nil, err
	}
	client := f.HTTP
	if client == nil {
		client = safeClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d（%s）", resp.StatusCode, u.Host)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.HasPrefix(ct, "text/html") && !strings.HasPrefix(ct, "text/plain") {
		return nil, fmt.Errorf("不支持的 Content-Type: %q（仅 text/html、text/plain）", ct)
	}
	// 限读原始字节（2MB）后按 Content-Type / <meta charset> 转码到 UTF-8：中文站 GBK/GB2312
	// 仍常见，直接按 UTF-8 解会得到乱码。charset.NewReader 对 text/html 嗅探 <meta charset>，
	// 其余用 Content-Type 的 charset；无标注时回落 UTF-8（透传）。
	decR, err := charset.NewReader(io.LimitReader(resp.Body, fetchMaxBody), ct)
	if err != nil {
		return nil, fmt.Errorf("识别字符集失败: %w", err)
	}
	body, err := io.ReadAll(decR)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %w", err)
	}
	title, text := htmlToText(string(body), strings.HasPrefix(ct, "text/html"))
	return &Page{URL: u.String(), Title: title, Text: truncateRunes(text, fetchMaxText)}, nil
}

// htmlToText 把 HTML 转纯文本（isHTML=false 时仅折叠空白）：<title> 提取标题，
// 整棵跳过 script/style/noscript/template，其余按文档序取文本节点、折叠连续空白。
// 块级换行不保留（段落连排）——搜索摘要/正文参考场景够用，MVP 取舍。
func htmlToText(s string, isHTML bool) (title, text string) {
	if !isHTML {
		return "", strings.Join(strings.Fields(s), " ")
	}
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return "", ""
	}
	skipped := map[string]bool{"script": true, "style": true, "noscript": true, "template": true}
	var sb strings.Builder
	titleDone := false // 必须在 walk 闭包定义前声明（闭包引用）
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skipped[n.Data] {
				return // 整棵子树跳过
			}
			if n.Data == "title" && !titleDone {
				title = nodeText(n)
				titleDone = true
				return // 标题不重复进正文
			}
		}
		if n.Type == html.TextNode {
			if t := strings.Join(strings.Fields(n.Data), " "); t != "" {
				sb.WriteString(t)
				sb.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title, strings.TrimSpace(sb.String())
}

// truncateRunes 按字符（rune）截断，避免把多字节中文截成乱码；超限时带省略标记。
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…（已截断）"
}

// nodeText 取元素内全部文本（跳过 script/style），折叠空白。（Task 3 的 search.go
// 亦用此助手；本任务先行提供实现，避免包不完整。）
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.ElementNode && (c.Data == "script" || c.Data == "style") {
			return
		}
		if c.Type == html.TextNode {
			if t := strings.Join(strings.Fields(c.Data), " "); t != "" {
				sb.WriteString(t)
				sb.WriteByte(' ')
			}
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return strings.TrimSpace(sb.String())
}
