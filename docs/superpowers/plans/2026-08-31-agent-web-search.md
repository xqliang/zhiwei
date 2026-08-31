# 知微 Agent 联网搜索 + web_fetch 实现计划（Phase 2）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给知微加两个常驻 MCP 工具——`web_search`（免 key 引擎链 Bing→DDG + 可选 Tavily）与 `web_fetch`（抓指定 URL 正文，SSRF 防护），配置共用 `agent_config` 表（迁移 000028），设置页可配引擎与 API key，人设补「查证」引导。

**Architecture:** 新增 `internal/search` 包（`search.go` 引擎链 + `fetch.go` 抓取，均带 SSRF 安全拨号）；MCP 工具在 `internal/agent/mcp_tools.go` 注册、经 `MCPDeps` 注入 `Search`/`Fetch`/`Configs`（每次调用读最新配置，设置页热改即生效）；`PUT /api/agent/config` 改指针合并语义（未传字段保持原值）；main.go 装配（注意：`agentConfigs` 定义需上移到 mcpDeps 装配之前）；前端设置页加「联网搜索」卡。

**Tech Stack:** Go 1.25（module `zhiwei`）、`golang.org/x/net/html`（DOM 解析，成熟方案不造轮子）、MCP go-sdk、原生 testing + httptest。

**Spec:** `docs/superpowers/specs/2026-08-31-agent-web-search-design.md`
**Worktree:** `/Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.claude/worktrees/agent-web-search`（分支 `feat/agent-web-search`，基于 main `ebe049a`，已含 Phase 1）

**测试环境注意：** 集成测试需 live MySQL：`TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true"`。若报 `no migration found for version X`，是共享容器里 `zhiwei_test_<pkg>` 被别的分支搞成更高版本的脏库，`docker exec zhiwei-mvp-mysql mysql -uroot -proot -e "DROP DATABASE IF EXISTS zhiwei_test_<pkg>;"` 后重跑。

---

### Task 1: 依赖 + 迁移 000028 + agent_config 搜索列（repo 层）

**Files:**
- Modify: `go.mod`/`go.sum`（`go get golang.org/x/net`）
- Create: `migrations/000028_agent_search_config.up.sql` / `migrations/000028_agent_search_config.down.sql`
- Modify: `internal/repo/agent_config.go`
- Modify: `internal/repo/agent_config_test.go`

- [ ] **Step 1: 加依赖**

```bash
cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.claude/worktrees/agent-web-search
go get golang.org/x/net@latest
```
若 GOPROXY 被墙：`GOPROXY=https://goproxy.cn,direct go get golang.org/x/net@latest`。预期输出 `go: added golang.org/x/net vX.Y.Z`。后续只 import 其 `html` 子包。

- [ ] **Step 2: 写失败测试（repo 搜索列 roundtrip）**

改 `internal/repo/agent_config_test.go` 的 `TestAgentConfigRepo`——`Upsert` 签名将从 `(ctx, identity, soul)` 改为 `(ctx, AgentConfig)`（Step 4）。先把测试改成目标形态（含搜索列断言）。整函数替换为：

```go
// TestAgentConfigRepo 验证人设配置单例：未配置读到空；Upsert 后读回；再 Upsert 为更新（仍单行）。
// Phase 2 起含搜索列（search_engine/search_api_key）roundtrip：空引擎归一 auto、空 key 存 NULL。
func TestAgentConfigRepo(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &AgentConfigRepo{DB: db}
	// 隔离：清掉可能的历史单行，确保「未配置」初态。
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_config WHERE id = 1") })
	_, _ = db.Exec("DELETE FROM agent_config WHERE id = 1")

	c, err := r.Get(ctx)
	if err != nil {
		t.Fatalf("Get(空): %v", err)
	}
	if c.Identity != "" || c.Soul != "" || c.SearchEngine != "" || c.SearchAPIKey != nil {
		t.Fatalf("未配置应为空, got %+v", c)
	}

	if err := r.Upsert(ctx, AgentConfig{Identity: "我是知微", Soul: "温柔简洁不废话"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	c, _ = r.Get(ctx)
	if c.Identity != "我是知微" || c.Soul != "温柔简洁不废话" {
		t.Fatalf("读回不符: %+v", c)
	}
	if c.SearchEngine != "auto" { // 空 engine 归一为 auto（默认免 key 链）
		t.Fatalf("未给引擎应归一 auto, got %q", c.SearchEngine)
	}

	// 再 Upsert = 更新（不新增行）；搜索列一并写入读回。
	if err := r.Upsert(ctx, AgentConfig{
		Identity: "我是知微v2", Soul: "毒舌",
		SearchEngine: "tavily", SearchAPIKey: strPtr("tvly-test"),
	}); err != nil {
		t.Fatalf("Upsert 更新: %v", err)
	}
	c, _ = r.Get(ctx)
	if c.Identity != "我是知微v2" || c.Soul != "毒舌" {
		t.Fatalf("更新后不符: %+v", c)
	}
	if c.SearchEngine != "tavily" || c.SearchKey() != "tvly-test" {
		t.Fatalf("搜索列读回不符: engine=%q key=%v", c.SearchEngine, c.SearchAPIKey)
	}
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM agent_config"); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应恒为单行, got %d", n)
	}
}
```

并在文件末尾（import 块后加 `"zhiwei/internal/repotest"` 已有；本文件在同包）加测试辅助：

```go
// strPtr 测试辅助：取字符串指针。
func strPtr(s string) *string { return &s }
```

- [ ] **Step 3: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestAgentConfigRepo -v
```
预期 FAIL：编译错误（`AgentConfig` 无 `SearchEngine` 字段 / `Upsert` 参数不匹配 / 无 `SearchKey`）。若编译过了也应在运行时因缺列报错。

- [ ] **Step 4: 迁移 + repo 实现**

创建 `migrations/000028_agent_search_config.up.sql`：

```sql
-- 联网搜索配置（Phase 2）：与 identity/soul 共用 agent_config 单例行。
-- search_engine: auto|bing|duckduckgo|tavily（默认 auto = 免 key 引擎链优先）
-- search_api_key: 可选；tavily 等付费后端用，免 key 后端留空(NULL)
ALTER TABLE agent_config
  ADD COLUMN search_engine  VARCHAR(32) NOT NULL DEFAULT 'auto',
  ADD COLUMN search_api_key TEXT        NULL;
```

创建 `migrations/000028_agent_search_config.down.sql`：

```sql
-- 回滚 Phase 2 搜索配置列。
ALTER TABLE agent_config
  DROP COLUMN search_api_key,
  DROP COLUMN search_engine;
```

改 `internal/repo/agent_config.go`——struct 与 Get/Upsert 整体替换为：

```go
// AgentConfig 是知微 agent 的全局配置（单份，行 id 恒为 1）：
// Identity/Soul 人设（身份定位/性格语气，每轮注入「发给 dsh 的文本」前）；
// SearchEngine/SearchAPIKey 联网搜索配置（Phase 2，web_search 工具每次调用读最新值）。
type AgentConfig struct {
	Identity     string    `db:"identity" json:"identity"`
	Soul         string    `db:"soul" json:"soul"`
	SearchEngine string    `db:"search_engine" json:"search_engine"` // auto|bing|duckduckgo|tavily
	// SearchAPIKey 搜索后端 API key（tavily 用）；NULL=未配置（免 key 引擎）。指针列用指针承接。
	SearchAPIKey *string   `db:"search_api_key" json:"search_api_key,omitempty"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// SearchKey 返回搜索 API key（NULL/nil → 空串），调用方免判空。
func (c *AgentConfig) SearchKey() string {
	if c == nil || c.SearchAPIKey == nil {
		return ""
	}
	return *c.SearchAPIKey
}

// normalizeEngine 空串归一为 auto（默认免 key 引擎链）。
func normalizeEngine(s string) string {
	if v := strings.TrimSpace(s); v != "" {
		return v
	}
	return "auto"
}
```

（import 块加 `"strings"`。）`Get` 的 SELECT 换为：

```go
	err := r.DB.GetContext(ctx, &c, `SELECT identity, soul, search_engine, search_api_key, updated_at FROM agent_config WHERE id = 1`)
```

`Upsert` 整体替换（签名变更：收完整 AgentConfig）：

```go
// Upsert 写全局配置（单例 id=1，存在即更新全部四列，updated_at 由 DB 自动刷新）。
// 调用方（putConfig）负责「读现值→指针合并」；本方法整行覆盖。
func (r *AgentConfigRepo) Upsert(ctx context.Context, c AgentConfig) error {
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO agent_config (id, identity, soul, search_engine, search_api_key)
VALUES (1, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  identity = VALUES(identity), soul = VALUES(soul),
  search_engine = VALUES(search_engine), search_api_key = VALUES(search_api_key)`,
		c.Identity, c.Soul, normalizeEngine(c.SearchEngine), c.SearchAPIKey)
	return err
}
```

**注意**：`Upsert` 签名变更会暂时编译破坏 `internal/agent/handlers.go:133`（putConfig 调旧签名）——Task 5 修复它。为让本任务可独立编译测试，本步先做最小兼容：把 handlers.go:133 的调用临时改为：

```go
	if err := h.Configs.Upsert(r.Context(), repo.AgentConfig{Identity: body.Identity, Soul: body.Soul}); err != nil {
```

（语义与旧版一致：putConfig 目前只写 identity/soul，未传搜索列会整行覆盖为空——**短暂的行为缺陷，Task 5 的指针合并修复**。在 commit message 里注明。）

- [ ] **Step 5: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestAgentConfigRepo -v
```
预期 PASS。再跑 `go build ./...` 确认全仓编译（handlers.go 已临时兼容）。

- [ ] **Step 6: 提交**

```bash
git add go.mod go.sum migrations/000028_agent_search_config.up.sql migrations/000028_agent_search_config.down.sql internal/repo/agent_config.go internal/repo/agent_config_test.go internal/agent/handlers.go
git commit -m "feat(repo): agent_config 加搜索列(000028)+Upsert 改收结构体（Phase 2 web_search 配置底座）"
```

---

### Task 2: internal/search/fetch.go —— SSRF 安全抓取 + HTML→文本

**Files:**
- Create: `internal/search/fetch.go`
- Create: `internal/search/fetch_test.go`

- [ ] **Step 1: 写失败测试**

创建 `internal/search/fetch_test.go`：

```go
package search

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testClient 绕过 SSRF 安全拨号的普通 client（httptest 监听 127.0.0.1；仅测试用，
// 生产装配用 NewFetcher/NewSearcher 的安全 client）。
func testClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

// TestBlockedIP 锁定 SSRF 根防线的 IP 判定表：环回/私网/链路本地/组播/未指定/ULA 全拒，公网放行。
func TestBlockedIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.1.1", "0.0.0.0", "::1", "fe80::1", "fd00::1", "224.0.0.1"} {
		if !blockedIP(net.ParseIP(s)) {
			t.Errorf("%s 应被拦截", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		if blockedIP(net.ParseIP(s)) {
			t.Errorf("%s 不应被拦截", s)
		}
	}
}

// TestFetchHTMLToText：抓 HTML 页提取 <title> 与正文（剥 script/style、折叠空白、按 rune 截断）。
func TestFetchHTMLToText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>示例页</title><style>.x{color:red}</style></head>
<body><script>evil()</script><h1>你好</h1><p>第一段   多余空白</p><p>第二段</p></body></html>`))
	}))
	defer srv.Close()
	f := &Fetcher{HTTP: testClient()}
	p, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "示例页" {
		t.Errorf("title=%q", p.Title)
	}
	for _, want := range []string{"你好", "第一段 多余空白", "第二段"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("正文应含 %q: %q", want, p.Text)
		}
	}
	if strings.Contains(p.Text, "evil") || strings.Contains(p.Text, "color:red") {
		t.Errorf("正文不应含 script/style 内容: %q", p.Text)
	}
}

// TestFetchRejects：非 2xx / 非 HTML Content-Type / 空 URL / 非 http 协议 各自报错。
func TestFetchRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/404":
			w.WriteHeader(404)
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>ok</body></html>`))
		}
	}))
	defer srv.Close()
	f := &Fetcher{HTTP: testClient()}
	for _, tc := range []struct{ name, url string }{
		{"404", srv.URL + "/404"},
		{"json", srv.URL + "/json"},
		{"空", "  "},
		{"ftp", "ftp://example.com/x"},
	} {
		if _, err := f.Fetch(context.Background(), tc.url); err == nil {
			t.Errorf("%s 应报错", tc.name)
		}
	}
}

// TestFetchSSRFBlocked：安全 client（NewFetcher 默认）请求 127.0.0.1 的 httptest → 拨号被拒。
func TestFetchSSRFBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should never reach"))
	}))
	defer srv.Close()
	f := NewFetcher()
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("SSRF 防护应拦截 127.0.0.1 拨号")
	}
}

// TestFetchTruncates：正文超 fetchMaxText 按 rune 截断（带省略标记），不截出半个汉字。
func TestFetchTruncates(t *testing.T) {
	long := strings.Repeat("字", fetchMaxText+50)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>" + long + "</p></body></html>"))
	}))
	defer srv.Close()
	p, err := (&Fetcher{HTTP: testClient()}).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(p.Text)); got > fetchMaxText+10 { // 截断标记「…（已截断）」额外几个字符
		t.Errorf("截断后长度 %d 超限", got)
	}
	if !strings.Contains(p.Text, "已截断") {
		t.Errorf("截断应带省略标记: %q", p.Text[:20])
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/search/ -v
```
预期：编译失败（`blockedIP`/`Fetcher` 未定义）。

- [ ] **Step 3: 实现 fetch.go**

创建 `internal/search/fetch.go`：

```go
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
func safeClient() *http.Client {
	return &http.Client{
		Timeout: fetchTimeout,
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBody))
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
```

- [ ] **Step 4: 跑测试确认通过**

```bash
gofmt -w internal/search/fetch.go internal/search/fetch_test.go
go test ./internal/search/ -v
```
预期：`TestBlockedIP`、`TestFetchHTMLToText`、`TestFetchRejects`、`TestFetchSSRFBlocked`、`TestFetchTruncates` 全 PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/search/fetch.go internal/search/fetch_test.go
git commit -m "feat(search): SSRF 安全网页抓取 Fetcher（拨号期 IP 校验+HTML转文本+截断）"
```

---

### Task 3: internal/search/search.go —— 引擎链（Bing/DDG-lite 免 key + Tavily）

**Files:**
- Create: `internal/search/search.go`
- Create: `internal/search/search_test.go`

- [ ] **Step 1: 写失败测试**

创建 `internal/search/search_test.go`：

```go
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSERP 起一个 httptest 假搜索引擎，按 path 分流：/bing 返回 Bing 形态 SERP，
// /empty 返回无结果页，/ddg 返回 DDG-lite 形态，/tavily 校验 Bearer key 后返回 JSON。
func fakeSERP(t *testing.T, key string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bing":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><ol><li class="b_algo"><h2><a href="https://ex.com/a">结果甲</a></h2><div class="b_caption"><p>摘要甲</p></div></li><li class="b_algo"><h2><a href="https://ex.com/b">结果乙</a></h2><p>摘要乙</p></li></ol></body></html>`)
		case "/empty":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><p>没有结果</p></body></html>`)
		case "/ddg":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><table>
<tr><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fex.com%2Fddg&amp;rut=abc" class="result-link">DDG结果</a></td></tr>
<tr><td class="result-snippet">DDG摘要</td></tr>
</table></body></html>`)
		case "/tavily":
			if got := r.Header.Get("Authorization"); got != "Bearer "+key {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":"bad key"}`))
				return
			}
			var body struct {
				Query      string `json:"query"`
				MaxResults int    `json:"max_results"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Query == "" || body.MaxResults <= 0 {
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"results":[{"title":"Tavily甲","url":"https://ex.com/t1","content":"内容甲"},{"title":"Tavily乙","url":"https://ex.com/t2","content":"内容乙"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

// testSearcher 指向假 SERP 的 Searcher（普通 client 绕过 SSRF 拨号，见 fetch_test.go 注释）。
func testSearcher(srv *httptest.Server) *Searcher {
	return &Searcher{
		HTTP:      &http.Client{Timeout: 5 * time.Second},
		BingURL:   srv.URL + "/bing",
		DDGURL:    srv.URL + "/ddg",
		TavilyURL: srv.URL + "/tavily",
	}
}

func TestSearchBingParse(t *testing.T) {
	srv := fakeSERP(t, "")
	defer srv.Close()
	rs, err := testSearcher(srv).Search(context.Background(), EngineBing, "", "测试", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Title != "结果甲" || rs[0].URL != "https://ex.com/a" || rs[0].Snippet != "摘要甲" {
		t.Fatalf("Bing 解析不符: %+v", rs)
	}
}

func TestSearchDDGParse(t *testing.T) {
	srv := fakeSERP(t, "")
	defer srv.Close()
	rs, err := testSearcher(srv).Search(context.Background(), EngineDDG, "", "测试", 5)
	if err != nil {
		t.Fatal(err)
	}
	// uddg 解包后的真实 URL + 摘要按文档序配对。
	if len(rs) != 1 || rs[0].Title != "DDG结果" || rs[0].URL != "https://ex.com/ddg" || rs[0].Snippet != "DDG摘要" {
		t.Fatalf("DDG 解析不符: %+v", rs)
	}
}

func TestSearchTavily(t *testing.T) {
	srv := fakeSERP(t, "tvly-x")
	defer srv.Close()
	s := testSearcher(srv)
	rs, err := s.Search(context.Background(), EngineTavily, "tvly-x", "测试", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Title != "Tavily甲" || rs[0].Snippet != "内容甲" {
		t.Fatalf("Tavily 解析不符: %+v", rs)
	}
	// key 缺失 / 错 key 都要报错。
	if _, err := s.Search(context.Background(), EngineTavily, "", "测试", 5); err == nil {
		t.Error("缺 key 应报错")
	}
	if _, err := s.Search(context.Background(), EngineTavily, "wrong", "测试", 5); err == nil {
		t.Error("错 key 应报错（401）")
	}
}

// TestSearchAutoChain：auto = Bing 失败（此处指到 /empty）→ 降级 DDG 成功。
func TestSearchAutoChain(t *testing.T) {
	srv := fakeSERP(t, "")
	defer srv.Close()
	s := testSearcher(srv)
	s.BingURL = srv.URL + "/empty" // 首选引擎解析不到结果
	rs, err := s.Search(context.Background(), EngineAuto, "", "测试", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Title != "DDG结果" {
		t.Fatalf("auto 链应降级到 DDG: %+v", rs)
	}
	// 全链失败且无 key → 明确报错（提示配 Tavily key）。
	s.DDGURL = srv.URL + "/empty"
	if _, err := s.Search(context.Background(), EngineAuto, "", "测试", 5); err == nil || !strings.Contains(err.Error(), "Tavily") {
		t.Fatalf("全链失败应报含 Tavily 提示的错误: %v", err)
	}
}

func TestSearchArgValidation(t *testing.T) {
	srv := fakeSERP(t, "")
	defer srv.Close()
	s := testSearcher(srv)
	if _, err := s.Search(context.Background(), EngineBing, "", "  ", 5); err == nil {
		t.Error("空 query 应报错")
	}
	if _, err := s.Search(context.Background(), "nope", "", "x", 5); err == nil {
		t.Error("未知引擎应报错")
	}
	if !ValidEngine(EngineAuto) || !ValidEngine(EngineBing) || !ValidEngine(EngineDDG) || !ValidEngine(EngineTavily) || ValidEngine("nope") {
		t.Error("ValidEngine 判定不符")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/search/ -run TestSearch -v
```
预期：编译失败（`Searcher`/`Engine*` 未定义）。

- [ ] **Step 3: 实现 search.go**

创建 `internal/search/search.go`：

```go
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
	target, tag := found, sub // 复用 walk：在 target 内找 sub
	found = nil
	walk(target)
	return found
}

// nodeText 取元素内全部文本（跳过 script/style），折叠空白。
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
```

**注意**：`firstDesc` 里 `target, tag := found, sub` 后 `tag` 未再使用会被 vet 报 unused？不会——`tag` 是已声明变量被重新赋值，Go 允许；但为清晰，实现时可直接写 `found = nil; walk(target)`，不需要那行 reassign（walk 匹配的是闭包里的 `tag` 变量——重赋 `tag = sub` 才是必要的）。实现时写成：

```go
	if found == nil || sub == "" {
		return found
	}
	tag = sub
	target := found
	found = nil
	walk(target)
	return found
```

- [ ] **Step 4: 跑测试确认通过**

```bash
gofmt -w internal/search/search.go internal/search/search_test.go
go test ./internal/search/ -v
```
预期：本包全部测试 PASS（含 Task 2 的 fetch 测试回归）。

- [ ] **Step 5: 提交**

```bash
git add internal/search/search.go internal/search/search_test.go
git commit -m "feat(search): 引擎链搜索器（Bing/DDG-lite 免 key HTML 解析 + Tavily JSON + auto 降级）"
```

---

### Task 4: MCP 工具 web_search / web_fetch

**Files:**
- Modify: `internal/agent/mcp_server.go`（MCPDeps 加 3 个字段）
- Modify: `internal/agent/mcp_tools.go`（注册 + 两个 handler）
- Create: `internal/agent/web_tools_test.go`

- [ ] **Step 1: 写失败测试**

创建 `internal/agent/web_tools_test.go`：

```go
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zhiwei/internal/repo"
	"zhiwei/internal/search"
)

// webDeps 构造带假 SERP/假页面的 MCPDeps（Configs 指向真库以测引擎路由）。
func webDeps(t *testing.T) (MCPDeps, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/serp":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><ol><li class="b_algo"><h2><a href="https://ex.com/a">搜索结果甲</a></h2><p>摘要甲</p></li></ol></body></html>`)
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><title>页面标题</title></head><body><p>页面正文内容</p></body></html>`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfgRepo := &repo.AgentConfigRepo{DB: db}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_config WHERE id = 1") })
	md, _ := p2dDeps(t)
	md.Configs = cfgRepo
	md.Search = &search.Searcher{
		HTTP:      &http.Client{Timeout: 5 * time.Second},
		BingURL:   srv.URL + "/serp",
		DDGURL:    srv.URL + "/serp",
		TavilyURL: srv.URL + "/serp",
	}
	md.Fetch = &search.Fetcher{HTTP: &http.Client{Timeout: 5 * time.Second}}
	return md, srv
}

// TestWebSearchTool：默认（无配置行）auto 链命中假 SERP → 返回结构化结果；
// Search=nil 时报「未启用」；空 query 报错。
func TestWebSearchTool(t *testing.T) {
	md, _ := webDeps(t)
	ctx := context.Background()
	res, _, err := webSearchHandler(md)(ctx, nil, webSearchArgs{Query: "测试", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	var out []webResultOut
	if err := json.Unmarshal([]byte(mcpText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Title != "搜索结果甲" || out[0].URL != "https://ex.com/a" {
		t.Fatalf("web_search 结果不符: %+v", out)
	}
	// 配置了引擎=bing → 只走 bing（同样命中假 SERP）。
	if err := md.Configs.Upsert(ctx, repo.AgentConfig{SearchEngine: "bing"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := webSearchHandler(md)(ctx, nil, webSearchArgs{Query: "测试"}); err != nil {
		t.Fatalf("指定 bing 引擎不应报错: %v", err)
	}
	// Search 未装配 → tool-error。
	md.Search = nil
	if _, _, err := webSearchHandler(md)(ctx, nil, webSearchArgs{Query: "测试"}); err == nil {
		t.Error("Search=nil 应报「未启用」")
	}
	// 空 query。
	md2, _ := webDeps(t)
	if _, _, err := webSearchHandler(md2)(ctx, nil, webSearchArgs{Query: " "}); err == nil {
		t.Error("空 query 应报错")
	}
}

// TestWebFetchTool：抓假页面返回 title+正文；空 URL 报错；Fetch=nil 报「未启用」。
func TestWebFetchTool(t *testing.T) {
	md, srv := webDeps(t)
	ctx := context.Background()
	res, _, err := webFetchHandler(md)(ctx, nil, webFetchArgs{URL: srv.URL + "/page"})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(mcpText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Title != "页面标题" || !strings.Contains(out.Text, "页面正文内容") {
		t.Fatalf("web_fetch 结果不符: %+v", out)
	}
	if _, _, err := webFetchHandler(md)(ctx, nil, webFetchArgs{URL: " "}); err == nil {
		t.Error("空 URL 应报错")
	}
	md.Fetch = nil
	if _, _, err := webFetchHandler(md)(ctx, nil, webFetchArgs{URL: srv.URL + "/page"}); err == nil {
		t.Error("Fetch=nil 应报「未启用」")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/agent/ -run 'TestWebSearchTool|TestWebFetchTool' -v
```
预期：编译失败（`webSearchHandler` 等未定义）。

- [ ] **Step 3: 实现**

**3a.** `internal/agent/mcp_server.go` 的 `MCPDeps` struct 末尾（`Retrieve` 字段后）追加：

```go
	// ---- 联网搜索（Phase 2：web_search / web_fetch 工具）----
	// Search 联网搜索器；nil 则 web_search 工具报「未启用」。每次调用读 Configs 最新配置
	//（设置页改引擎/API key 热生效，不重启）。装配见 main.go。
	Search *search.Searcher
	// Fetch 网页抓取器（SSRF 安全拨号）；nil 则 web_fetch 工具报「未启用」。
	Fetch *search.Fetcher
	// Configs 全局 agent 配置（web_search 读搜索引擎/API key；与 handlers 的 AgentHandler.Configs 同一实例）。
	Configs *repo.AgentConfigRepo
```

import 块加 `"zhiwei/internal/search"`。

**3b.** `internal/agent/mcp_tools.go` 的 `registerReadTools` 里（`get_todos` 注册之后）追加两个注册：

```go
	mcp.AddTool(s, &mcp.Tool{
		Name: "web_search",
		Description: "联网搜索公开网络信息（搜索引擎）。用于：不确定或可能有时效性的问题、不了解的专业术语/名词、需要外部资料佐证时。" +
			"返回结果列表(标题/链接/摘要)；要看某条结果详情时配合 web_fetch。与用户个人数据无关的通用问题优先用它查证。",
	}, webSearchHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "web_fetch",
		Description: "抓取指定 URL 的网页正文（纯文本）。用于：阅读 web_search 结果中的某个链接、或用户明确给出的网址。仅支持 http/https 公网页面。",
	}, webFetchHandler(d))
```

**3c.** `internal/agent/mcp_tools.go` 文件末尾追加实现：

```go
// ---- web_search / web_fetch（Phase 2 联网工具，全局配置不按 userID 隔离）----

type webResultOut struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type webSearchArgs struct {
	Query string `json:"query" jsonschema:"搜索关键词（自然语言或关键词均可）"`
	Limit int    `json:"limit,omitempty" jsonschema:"最多返回条数, 默认 5, 上限 10"`
}

// webSearchHandler 每次调用读 Configs 最新搜索配置（引擎/API key，设置页热改即生效），
// 无 Configs/无行时默认 auto 引擎链。Search 未装配 → tool-error。
func webSearchHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, webSearchArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a webSearchArgs) (*mcp.CallToolResult, any, error) {
		if d.Search == nil {
			return nil, nil, fmt.Errorf("联网搜索未启用（服务器未装配 search）")
		}
		engine, apiKey := search.EngineAuto, ""
		if d.Configs != nil {
			if c, err := d.Configs.Get(ctx); err == nil {
				engine = c.SearchEngine
				apiKey = c.SearchKey()
			}
		}
		rs, err := d.Search.Search(ctx, engine, apiKey, a.Query, a.Limit)
		if err != nil {
			return nil, nil, err
		}
		out := make([]webResultOut, 0, len(rs))
		for _, r := range rs {
			out = append(out, webResultOut{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
		}
		return jsonResult(out)
	}
}

type webFetchArgs struct {
	URL string `json:"url" jsonschema:"要抓取的网页 URL（http/https）"`
}

// webFetchHandler 抓单页正文；Fetch 未装配 → tool-error。
func webFetchHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, webFetchArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a webFetchArgs) (*mcp.CallToolResult, any, error) {
		if d.Fetch == nil {
			return nil, nil, fmt.Errorf("网页抓取未启用（服务器未装配 fetch）")
		}
		p, err := d.Fetch.Fetch(ctx, a.URL)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(struct {
			URL   string `json:"url"`
			Title string `json:"title,omitempty"`
			Text  string `json:"text"`
		}{URL: p.URL, Title: p.Title, Text: p.Text})
	}
}
```

import 块加 `"zhiwei/internal/search"`（`fmt` 已有）。

- [ ] **Step 4: 跑测试确认通过**

```bash
gofmt -w internal/agent/mcp_server.go internal/agent/mcp_tools.go internal/agent/web_tools_test.go
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/agent/ -run 'TestWebSearchTool|TestWebFetchTool' -v
```
预期：两条 PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/agent/mcp_server.go internal/agent/mcp_tools.go internal/agent/web_tools_test.go
git commit -m "feat(agent): MCP 工具 web_search/web_fetch（每次调用读最新搜索配置）"
```

---

### Task 5: 配置端点（指针合并）+ 人设补查证引导

**Files:**
- Modify: `internal/agent/handlers.go`（getConfig/putConfig）
- Modify: `internal/agent/handlers_test.go`（TestAgentConfigAPI 扩展）
- Modify: `internal/config/config.go:165`（默认 prompt 追加一句）
- Modify: `internal/config/config_test.go`（关键词断言加 web_search）

- [ ] **Step 1: 写失败测试**

**1a.** `internal/agent/handlers_test.go` 的 `TestAgentConfigAPI`——在现有 GET 读回断言之后（`DatetimeHead` 断言后、函数收尾前）追加：

```go
	// Phase 2：只 PUT 搜索字段（指针合并——未传的 identity/soul 必须保持原值）。
	putSearch := httptest.NewRequest("PUT", "/api/agent/config",
		strings.NewReader(`{"search_engine":"tavily","search_api_key":"tvly-test"}`))
	putSearchRec := httptest.NewRecorder()
	r.ServeHTTP(putSearchRec, putSearch)
	if putSearchRec.Code != http.StatusOK {
		t.Fatalf("PUT search code=%d body=%s", putSearchRec.Code, putSearchRec.Body.String())
	}
	getRec2 := httptest.NewRecorder()
	r.ServeHTTP(getRec2, httptest.NewRequest("GET", "/api/agent/config", nil))
	if getRec2.Code != http.StatusOK {
		t.Fatalf("GET2 code=%d", getRec2.Code)
	}
	var out2 struct {
		Identity, Soul, SearchEngine, SearchAPIKey string
	}
	if err := json.Unmarshal(getRec2.Body.Bytes(), &out2); err != nil {
		t.Fatalf("resp2 解析: %v", err)
	}
	if out2.Identity != "我是知微API" || out2.Soul != "简洁API" {
		t.Errorf("指针合并：未传的 identity/soul 应保持原值: %+v", out2)
	}
	if out2.SearchEngine != "tavily" || out2.SearchAPIKey != "tvly-test" {
		t.Errorf("搜索字段应已保存: %+v", out2)
	}

	// 非法引擎 → 400。
	badRec := httptest.NewRecorder()
	r.ServeHTTP(badRec, httptest.NewRequest("PUT", "/api/agent/config",
		strings.NewReader(`{"search_engine":"nope"}`)))
	if badRec.Code != http.StatusBadRequest {
		t.Errorf("非法引擎应 400, got %d", badRec.Code)
	}
```

（GET 响应字段名为 JSON tag 小写：解码 struct 用 `json:"search_engine"` / `json:"search_api_key"`——实现成：

```go
	var out2 struct {
		Identity     string `json:"identity"`
		Soul         string `json:"soul"`
		SearchEngine string `json:"search_engine"`
		SearchAPIKey string `json:"search_api_key"`
	}
```

**1b.** `internal/config/config_test.go` 的 `TestAgentConfigDefaults` 里，关键词列表改为：

```go
	for _, kw := range []string{"分场景", "直接基于你自己的知识", "如实说明", "web_search"} {
```

**1c.** `internal/agent/handlers_test.go` 里 TestAgentConfigAPI 的断言 `out.Preview` 不变；另注意现有 GET 断言用 `out.Identity` 等 JSON tag 小写结构体已存在（`Identity, Soul, Preview string` + `DatetimeHead string json:"datetime_head"`）——直接复用该结构体模式。

- [ ] **Step 2: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/agent/ -run TestAgentConfigAPI -v
go test ./internal/config/ -run TestAgentConfigDefaults -v
```
预期：TestAgentConfigAPI FAIL（PUT search 后 identity/soul 被清空——Task 1 的临时覆盖行为；或 400 分支不符）；TestAgentConfigDefaults FAIL（默认 prompt 尚无 web_search 字样）。

- [ ] **Step 3: 实现**

**3a.** `internal/agent/handlers.go` 的 `getConfig`：`resp := map[string]any{...}` 初始加 `"search_engine": "auto", "search_api_key": ""`；`if h.Configs != nil` 块内（`resp["preview"]` 附近）追加：

```go
		resp["search_engine"] = c.SearchEngine
		if resp["search_engine"] == "" {
			resp["search_engine"] = "auto"
		}
		resp["search_api_key"] = c.SearchKey()
```

**3b.** `putConfig` 整体替换为（指针合并 + 引擎校验 + 读改写）：

```go
// putConfig 保存全局配置。Phase 2 起为指针合并语义：body 里未传（缺省/为 null）的字段
// 保持原值，传了的字段才覆盖——设置页「人设」与「联网搜索」两张卡各自只传自己的字段。
// 每轮注入，下一条消息即时生效（不重启 dsh）。
func (h *AgentHandler) putConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := reqUserID(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.Configs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "人设配置不可用"})
		return
	}
	var body struct {
		Identity      *string `json:"identity"`
		Soul          *string `json:"soul"`
		SearchEngine  *string `json:"search_engine"`
		SearchAPIKey  *string `json:"search_api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	// 读现值做合并基底（无行时零值：identity/soul 空、engine 归一 auto）。
	cur, err := h.Configs.Get(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cfg := repo.AgentConfig{Identity: cur.Identity, Soul: cur.Soul, SearchAPIKey: cur.SearchAPIKey}
	if body.Identity != nil {
		cfg.Identity = *body.Identity
	}
	if body.Soul != nil {
		cfg.Soul = *body.Soul
	}
	if body.SearchEngine != nil {
		e := strings.TrimSpace(*body.SearchEngine)
		if !search.ValidEngine(e) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法搜索引擎: " + e})
			return
		}
		cfg.SearchEngine = e
	}
	if body.SearchAPIKey != nil {
		k := strings.TrimSpace(*body.SearchAPIKey)
		if k == "" {
			cfg.SearchAPIKey = nil // 清空 key 存 NULL
		} else {
			cfg.SearchAPIKey = &k
		}
	}
	if err := h.Configs.Upsert(r.Context(), cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"identity": cfg.Identity, "soul": cfg.Soul,
		"preview":        AssemblePersona(cfg.Identity, cfg.Soul),
		"search_engine":  cfg.SearchEngine,
		"search_api_key": cfg.SearchKey(),
	})
}
```

import 块加 `"strings"` 与 `"zhiwei/internal/search"`（`repo` 已有——若无则加）。

**3c.** `internal/config/config.go:165` 的默认 prompt：在最后一行 `只有在需要用户本人数据时才调用工具；不要臆测用户没有的记忆或数据。` 之后、反引号结束前，追加两行（保持 raw string 内直接换行）：

```go
遇到不确定、有时效性、或需要最新外部资料的问题时，先用 web_search 联网搜索、必要时用 web_fetch 阅读具体网页，再作答；查不到就如实说明。
```

即末尾变为：

```
只有在需要用户本人数据时才调用工具；不要臆测用户没有的记忆或数据。
遇到不确定、有时效性、或需要最新外部资料的问题时，先用 web_search 联网搜索、必要时用 web_fetch 阅读具体网页，再作答；查不到就如实说明。`),
```

- [ ] **Step 4: 跑测试确认通过**

```bash
gofmt -w internal/agent/handlers.go internal/agent/handlers_test.go internal/config/config.go internal/config/config_test.go
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/agent/ -run TestAgentConfigAPI -v
go test ./internal/config/ -run TestAgentConfigDefaults -v
```
预期：两条 PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/agent/handlers.go internal/agent/handlers_test.go internal/config/config.go internal/config/config_test.go
git commit -m "feat(agent): 配置端点指针合并+搜索字段透出；默认人设补联网查证引导"
```

---

### Task 6: main.go 装配 + 设置页「联网搜索」卡

**Files:**
- Modify: `cmd/zhiwei-server/main.go`（agentConfigs 定义上移 + mcpDeps 三字段）
- Modify: `web/index.html`（人设卡后加搜索卡）
- Modify: `web/app.js`（loadAgentConfig 扩展 + saveAgentSearch）

- [ ] **Step 1: main.go 装配**

**1a.** 找到 `cmd/zhiwei-server/main.go:483` 的 `agentConfigs := &repo.AgentConfigRepo{DB: db}`，**剪切**该行，粘贴到 mcpDeps 装配块（`:390` `mcpDeps := agent.MCPDeps{`）**之前**（它只依赖 `db`，此时 db 已开库）。原 483 行位置删除（后面的 `agentConfigs.Get` 引用不受影响）。

**1b.** mcpDeps 结构体字面量里 `Retrieve: retriever,` 之后追加：

```go
		// 联网搜索（Phase 2 web_search/web_fetch）：每次工具调用读 agentConfigs 最新配置。
		Search:  search.NewSearcher(),
		Fetch:   search.NewFetcher(),
		Configs: agentConfigs,
```

**1c.** import 块加 `"zhiwei/internal/search"`。

- [ ] **Step 2: 设置页 index.html**

在 `web/index.html` 的「整体 PROMPT 组装（只读，看全貌）」块（`:1068`）所在的知微人设卡 `</div>` 结束之后、「专有名词」卡注释（`:1073` `<!-- ============ 专有名词（ASR 实体纠错...` 之前，插入新卡：

```html
    <!-- ============ 联网搜索（Phase 2：引擎 + API key；PUT /api/agent/config 指针合并，只传搜索字段） ============ -->
    <div class="card">
      <h2 style="margin:0 0 4px">联网搜索</h2>
      <div class="muted" style="font-size:var(--fs-sm); margin-bottom:16px">
        配置知微的联网搜索（web_search / web_fetch 工具）。默认「自动」用免 key 引擎（Bing → DuckDuckGo，失败自动降级）；
        如需更稳定的结果可配置 Tavily API key（<a href="https://tavily.com" target="_blank" rel="noopener">tavily.com</a> 获取）。
        保存后<b>下一轮对话即生效</b>。
      </div>
      <div style="display:flex; gap:8px; align-items:center; flex-wrap:wrap">
        <label style="font-size:var(--fs-sm); font-weight:600">搜索引擎</label>
        <select class="txt" v-model="agentSearchEngine" style="width:170px">
          <option value="auto">自动（免 key 链）</option>
          <option value="bing">Bing</option>
          <option value="duckduckgo">DuckDuckGo</option>
          <option value="tavily">Tavily（需 API key）</option>
        </select>
        <input class="txt" type="password" v-model="agentSearchKey" placeholder="Tavily API key（可选，留空=免 key）" style="flex:1; min-width:220px">
        <button class="btn primary" :disabled="agentSearchSaving" @click="saveAgentSearch">
          <span v-if="agentSearchSaving" class="spinner"></span>保存
        </button>
        <span v-if="agentSearchSaved" class="muted" style="font-size:var(--fs-sm); color:var(--ok)">✓ 已保存，下条消息生效</span>
      </div>
      <div v-if="agentSearchErr" class="muted" style="font-size:var(--fs-xs); color:var(--danger,#c33); margin-top:6px">{{ agentSearchErr }}</div>
    </div>
```

- [ ] **Step 3: app.js**

**3a.** `web/app.js` 的 `loadAgentConfig`（`:3032`）函数体里（`agentCfgSaved.value = false;` 之前）追加：

```js
      agentSearchEngine.value = (d && d.search_engine) || 'auto';
      agentSearchKey.value = (d && d.search_api_key) || '';
```

**3b.** 在 `saveAgentConfig` 函数（`:3043`）之后新增：

```js
    // ---------- 设置：联网搜索配置（Phase 2 web_search/web_fetch） ----------
    // PUT /api/agent/config 指针合并：只传搜索字段，identity/soul 保持原值不动。
    const agentSearchEngine = ref('auto');
    const agentSearchKey = ref('');
    const agentSearchSaving = ref(false);
    const agentSearchSaved = ref(false);
    const agentSearchErr = ref('');
    async function saveAgentSearch() {
      if (agentSearchSaving.value) return;
      agentSearchSaving.value = true; agentSearchSaved.value = false; agentSearchErr.value = '';
      try {
        await api('PUT', '/api/agent/config', {
          search_engine: agentSearchEngine.value,
          search_api_key: agentSearchKey.value,
        });
        agentSearchSaved.value = true;
      } catch (e) { agentSearchErr.value = (e && e.message) || String(e); }
      finally { agentSearchSaving.value = false; }
    }
```

**注意**：`ref` 等在 `loadAgentConfig` 之前声明使用——Vue setup 的响应式变量须在同层作用域先声明。若 `agentSearchEngine.value` 在声明前被 `loadAgentConfig` 调用会 ReferenceError；实现时把这段「声明 + saveAgentSearch」放到 `loadAgentConfig` **之前**（或与其它 agentCfg ref 声明放一起），`loadAgentConfig` 只做赋值。

- [ ] **Step 4: 编译 + 冒烟**

```bash
go build ./...
```
预期：无错误。手动验证（可选，起服务后）：
```bash
# 起 dev 服务（端口 8081）：make dev 或 go run ./cmd/zhiwei-server
curl -s http://127.0.0.1:8081/api/agent/config | head -c 400   # 应含 search_engine/search_api_key
curl -s -X PUT http://127.0.0.1:8081/api/agent/config -H 'Content-Type: application/json' -d '{"search_engine":"auto"}'
```
（若未起服务跳过手动部分，由回归测试与 e2e 后续覆盖。）

- [ ] **Step 5: 提交**

```bash
git add cmd/zhiwei-server/main.go web/index.html web/app.js
git commit -m "feat(web): 设置页联网搜索卡 + main 装配 search/fetch 进 MCP 工具"
```

---

### Task 7: 全量回归

- [ ] **Step 1: 格式化 + 静态检查**

```bash
gofmt -l internal/ cmd/ | grep -v _test.go; gofmt -l internal/ cmd/
go vet ./internal/search/... ./internal/agent/... ./internal/repo/... ./internal/config/... ./cmd/...
go build ./...
```
预期：gofmt 无输出、vet/build 无错误。

- [ ] **Step 2: 相关包全量测试（live MySQL）**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/search/ ./internal/agent/ ./internal/repo/ ./internal/config/ -count=1
```
预期：全绿。若报 `no migration found for version X`：drop 对应脏库（见顶部「测试环境注意」）后重跑。

- [ ] **Step 3: 若有未提交文件，收尾提交**

```bash
git status --short   # 应干净（各任务已分别提交）
```

---

## Self-Review 记录

**Spec 覆盖：**
- §4 web_search 工具（引擎链/auto 降级/Tavily/limit 归一）→ Task 3 + Task 4 ✓
- §5 web_fetch + SSRF（拨号期 IP 校验、重定向复校、体积/超时/截断）→ Task 2 ✓
- §6 数据模型 000028 + repo 扩展 → Task 1 ✓
- §7 设置页卡 + 人设追加 → Task 5 + Task 6 ✓
- §8 测试（SSRF 拦截表、解析夹具、降级链、指针合并、迁移经 repotest 生效）→ 各任务 Step 1 + Task 7 ✓
- §3 架构（MCPDeps 注入、每调用读最新配置、main 装配）→ Task 4 + Task 6 ✓

**Placeholder 扫描：** 无 TBD；每步含完整代码/命令与预期输出。Task 6 Step 4 的 curl 冒烟标注「可选、未起服务则跳过」——属显式条件而非占位。

**类型一致性：** `search.Result`↔`webResultOut` 映射一致；`repo.AgentConfig{Identity,Soul,SearchEngine,SearchAPIKey}` 在 Task 1 定义、Task 5 使用；`Searcher.Search(ctx, engine, apiKey, query, limit)` 签名 Task 3 定义、Task 4 调用一致；`Fetcher.Fetch(ctx, url)` 同。`Upsert(ctx, AgentConfig)` 在 Task 1 变更签名并临时兼容 handlers，Task 5 重写 putConfig 收敛。

**已知取舍（spec 已确认）：** 免 key HTML 解析脆弱（降级+Tavily 兜底）；putConfig 读改写非事务（全局单行单用户，可接受）；块级换行不保留。
