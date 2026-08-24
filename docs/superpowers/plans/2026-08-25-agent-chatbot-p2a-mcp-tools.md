# Agent Chatbot · P2a MCP 读工具服务 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 dsh agent 能「读我的数据」——在主 `zhiwei-server` 进程内用官方 `modelcontextprotocol/go-sdk` 起一个 **streamable-http MCP server**（挂 chi `/internal/mcp`），暴露 4 个只读工具（`search_memory`/`get_timeline`/`get_topics`/`get_todos`）over 现有 `internal/repo`；dsh 边车（cordis `mcp-client` streamable-http）连回来调用。交付「问→读数据→答」的工具层。

**Architecture:** MCP server 进程内、复用主服务已开的 `*sqlx.DB`/repo（一个 DB 池、同事务语义、无子进程）。go-sdk 的 `StreamableHTTPHandler` 是普通 `http.Handler`，挂在现有 chi 路由。**不含** Go AgentRuntime（Go 驱动 dsh，留 P2b）、WS/前端（P2c）、写-提议（P2d）——本期 dsh 由现有 `services/agent-sidecar/spike/drive.mjs`（Node）驱动来验证工具。

**Tech Stack:** Go 1.26、`github.com/modelcontextprotocol/go-sdk` v1.7.0（spike 已验证与 dsh mcp-client 互通、协议版本 2025-11-25 兼容）、sqlx、chi、雪花 `ids.ID`。dsh 边车 `services/agent-sidecar/`（已装 `@deepseek-ai/dsh-*` 0.1.1-rc.2），模型 `doubao-seed-1-6-250615`（Ark，`ARK_API_KEY`）。

**依据：** spec `docs/superpowers/specs/2026-08-24-agent-chatbot-system-design.md` §5.1/§7/§10；Go↔dsh MCP interop spike（已证 stdio 互通、go-sdk v1.7.0、协议 2025-11-25）；`internal/repo/*` 现有读方法。

**贯穿约定：**
- 工具 handler 签名（go-sdk）：`func(ctx, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)`；`In` 是带 `json`+`jsonschema` tag 的入参 struct（go-sdk 自动推断 JSON Schema 校验）；返回把结果 `json.Marshal` 成一个 `&mcp.TextContent{Text: ...}`（spike 验证过的稳妥形态），第二返回值 `any` 传 `nil`。
- 所有工具限 `user_id=1`（单用户 MVP，与 `cmd/dedup-todos` 一致）；本期**全部只读**，无写。
- `ids.ID` 在 JSON 里是字符串；工具入参里引用 id（如 `topic_id`）用 `string`，handler 里 `ids.ParseID` 转换（空串=不过滤）。
- MCP 端点仅挂 `/internal/mcp`，绑现有服务端口（127.0.0.1，与 REST 同进程）；不做鉴权（单用户本地 MVP，与现有 `/api/*` 一致）。
- 集成测试连独立库：`TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_agentchat_test?parseTime=true&charset=utf8mb4&multiStatements=true"`（共享 zhiwei_test 被并行 worktree 冲，勿用 make test-integration）。红灯=编译失败，绿灯=该 DSN 下目标测试通过。

---

## File Structure

| 文件 | 责任 | 动作 |
|---|---|---|
| `go.mod` / `go.sum` | 加 `github.com/modelcontextprotocol/go-sdk v1.7.0` | Modify |
| `internal/repo/memory.go` | 加 `MemoryRepo.Search`（关键词 LIKE） | Modify |
| `internal/repo/memory_search_test.go` | Search 集成测试 | Create |
| `internal/agent/mcp_server.go` | `MCPDeps` + `NewMCPServer`（注册工具）+ `MCPHandler`（streamable-http `http.Handler`） | Create |
| `internal/agent/mcp_tools.go` | 4 个读工具的 In/Out 结构 + handler（over repo） | Create |
| `internal/agent/mcp_server_test.go` | 工具 handler 集成测试（直接调 handler，断言真实数据） | Create |
| `cmd/zhiwei-server/main.go` | 构造 MCP server（注入 repo）+ 挂 `/internal/mcp` | Modify |
| `services/agent-sidecar/cordis.agent.yml` | 在 spike cordis 基础上加 `mcp-client` streamable-http 行（连 `/internal/mcp`） | Create |

**类型契约：** `agent.MCPDeps{Memory *repo.MemoryRepo; Session *repo.SessionRepo; Transcript *repo.TranscriptRepo; Topic *repo.TopicRepo; Todo *repo.TodoRepo}`；`agent.NewMCPServer(MCPDeps) *mcp.Server`；`agent.MCPHandler(*mcp.Server) http.Handler`。

---

## Task 1: 加 go-sdk 依赖 + streamable-http interop 验证门（ping 工具）

**Files:** `go.mod`/`go.sum`（modify）、`internal/agent/mcp_server.go`（create，先只含 ping）

> 这是本期唯一的**未验证风险**：spike 证的是 stdio 互通；streamable-http 变体用同一 go-sdk + 同一 dsh mcp-client + 同一版本协商，理论同源但未 wire 测。先用一个 ping 工具证 HTTP 互通，通了再堆真工具。**若 HTTP 互通失败**（dsh mcp-client 连不上/协议不符），回退 spike 已证的 **stdio 独立二进制**方案（见本任务末尾「回退」），并把后续任务的挂载方式改为 stdio。

- [ ] **Step 1: 加依赖**

Run: `go get github.com/modelcontextprotocol/go-sdk/mcp@v1.7.0`
然后 `go mod tidy`。
Expected: `go.mod` 出现 `require github.com/modelcontextprotocol/go-sdk v1.7.0`；`go build ./...` 通过（暂无使用者，仅下载）。

- [ ] **Step 2: 写最小 MCP server（仅 ping）+ handler**

Create `internal/agent/mcp_server.go`:

```go
// Package agent 把「读/写我的数据」的能力暴露成 MCP 工具，供 dsh agent 调用。
// MCP server 进程内运行、复用主服务的 repo（一个 DB 池），通过 streamable-http
// 挂在 chi /internal/mcp；dsh 边车用 mcp-client(streamable-http) 连回来。
package agent

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/repo"
)

// MCPDeps 是工具依赖的仓储集合（主服务装配时注入已开库的实例）。
type MCPDeps struct {
	Memory     *repo.MemoryRepo
	Session    *repo.SessionRepo
	Transcript *repo.TranscriptRepo
	Topic      *repo.TopicRepo
	Todo       *repo.TodoRepo
}

// pingArgs：无参工具的入参（空 struct → object schema 无属性）。
type pingArgs struct{}

// NewMCPServer 构造 MCP server 并注册全部工具。
func NewMCPServer(d MCPDeps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "zhiwei", Version: "0.1.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "zhiwei_ping",
		Description: "健康检查：无参，返回固定字符串，用于验证 MCP 连通性。",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ pingArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "pong-zhiwei"}},
		}, nil, nil
	})

	registerReadTools(s, d) // Task 3 实现；Task 1 先留空实现或注释掉本行
	return s
}

// MCPHandler 把 server 包成 streamable-http 的 http.Handler（挂 chi 用）。
func MCPHandler(s *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
}
```

> Task 1 阶段 `registerReadTools` 尚未实现：先把该行注释掉（或在 `mcp_tools.go` 里放一个空的 `func registerReadTools(*mcp.Server, MCPDeps) {}` 占位，Task 3 填充）。为让编译通过，建议直接建一个占位 `mcp_tools.go`：`package agent` + 空的 `registerReadTools`。

- [ ] **Step 3: 临时挂载 + 起服务（验证用）**

在 `cmd/zhiwei-server/main.go` 里，路由装配处加（正式装配在 Task 4 完善；此处先能起即可）：找到 `r := api.NewRouter()`（或等价的 chi mux 变量）之后、`http.ListenAndServe`/`Serve` 之前，加：
```go
	mcpSrv := agent.NewMCPServer(agent.MCPDeps{
		Memory: memories, Session: sessions, Transcript: transcripts,
		Topic: topics, Todo: todos,
	})
	r.Handle("/internal/mcp", agent.MCPHandler(mcpSrv))
	r.Handle("/internal/mcp/*", agent.MCPHandler(mcpSrv))
```
（用 main 里已有的 repo 变量名——先 `grep` 确认：main 里构造 `repo.MemoryRepo{DB:db}` 等的变量名，按实际改。若某 repo 变量 main 里还没构造，就地补 `&repo.XxxRepo{DB: db}`。）

Run: `go build ./cmd/zhiwei-server` → 通过。

- [ ] **Step 4: 起服务 + 用 Node 驱动 dsh 验证 HTTP 互通**

先建 `services/agent-sidecar/cordis.agent.yml`：复制 `services/agent-sidecar/cordis.mcp.yml`，把其中的 `mcp-client` 行换成 streamable-http 指向主服务：
```yaml
- id: mcp-zhiwei
  name: '@deepseek-ai/dsh-mcp-client'
  config:
    transport: streamable-http
    serverName: zhiwei
    url: !!js process.env.ZW_AGENT_MCP_URL ?? 'http://127.0.0.1:8080/internal/mcp'
    failOnStartupError: true
```
（其余行——sdk-jsonrpc-server / llm-deepseek / agent-spine / sessions / token-meter——与 `cordis.mcp.yml` 一致。）

执行验证（**不要用 pipe/变量**，分步跑；MySQL 需起：`make compose-up`）：
1. 起主服务（另一个终端/后台）：`go run ./cmd/zhiwei-server`（需 `ARK_API_KEY` 等环境；服务听 8080，`/internal/mcp` 生效）。
2. 跑驱动：`node services/agent-sidecar/spike/drive.mjs doubao-seed-1-6-250615 services/agent-sidecar/cordis.agent.yml "请调用 zhiwei_ping 工具并把它返回的原文告诉我"`
Expected: `services/agent-sidecar/spike/logs/wire.ndjson` 里出现 `tool/call` `name:"mcp__zhiwei__zhiwei_ping"` 与 `tool/result` 含 `pong-zhiwei`，最终 assistant 文本含 `pong-zhiwei`。**这证明 streamable-http 互通。**

**回退（仅当 Step 4 失败）：** 改用 stdio——把 MCP server 做成独立二进制 `cmd/zhiwei-mcp/`（`server.Run(ctx, &mcp.StdioTransport{})`，spike 已证），cordis `mcp-client` 用 `transport: stdio, command: <二进制绝对路径>`；DB 连接在该二进制内自建（`config.Load()` + `repo.NewDB`，注意 dsh 会 scrub 环境，DSN 需经 cordis `env` 传或二进制自读 .env）。记录失败现象后再切。

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/agent/mcp_server.go internal/agent/mcp_tools.go cmd/zhiwei-server/main.go services/agent-sidecar/cordis.agent.yml
git commit -m "feat(agent-mcp): go-sdk streamable-http MCP server 骨架 + ping 工具, dsh HTTP 互通验证"
```

---

## Task 2: MemoryRepo.Search（关键词检索）

**Files:** `internal/repo/memory.go`（modify）、`internal/repo/memory_search_test.go`（create）

> 现有 `MemoryRepo.List` 只按 type/topic/time 过滤，无关键词。`search_memory` 工具需要按 title/content 关键词检索（spec §10 的 LIKE 检索）。加一个 `Search` 方法。

- [ ] **Step 1: 写失败测试**

Create `internal/repo/memory_search_test.go`:

```go
package repo

import (
	"testing"

	"zhiwei/internal/ids"
)

func TestMemorySearch(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	ctx := t.Context()

	// 造两条不同 session 的 memory，含可检索关键词
	sid := ids.New()
	kw := "量子隧穿实验" // 独特词，避免与库里既有数据碰撞
	ms := []*Memory{
		{Type: "fact", Title: kw + "记录", Content: "今天讨论了" + kw + "的进展", SessionID: sid, Status: "active", Importance: 0.6, Confidence: 0.8},
		{Type: "idea", Title: "无关记忆", Content: "买牛奶", SessionID: sid, Status: "active", Importance: 0.3, Confidence: 0.8},
	}
	if err := mr.InsertExt(ctx, db, ms); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}

	got, err := mr.Search(ctx, 1, kw, "", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var hit bool
	for _, m := range got {
		if m.ID == ms[0].ID {
			hit = true
		}
		if m.ID == ms[1].ID {
			t.Error("Search 命中了不含关键词的记忆")
		}
	}
	if !hit {
		t.Errorf("Search 未命中含关键词 %q 的记忆", kw)
	}

	// type 过滤
	got2, err := mr.Search(ctx, 1, kw, "idea", 20)
	if err != nil {
		t.Fatalf("Search(type=idea): %v", err)
	}
	for _, m := range got2 {
		if m.ID == ms[0].ID {
			t.Error("type=idea 不应命中 fact 记忆")
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run TestMemorySearch` → 编译失败 `mr.Search undefined`。

- [ ] **Step 3: 实现 Search**

在 `internal/repo/memory.go` 的 `ListActive` 方法之后加：

```go
// Search 按关键词（title/content LIKE）检索该用户 active 记忆，可选 type 过滤，
// 按 event_at 倒序。空 query 退化为 ListActive 语义（仅 type 过滤）。limit 默认/上限 50。
// 关键词做 LIKE 转义（% _ \），防止用户词里的通配符改变语义。
func (r *MemoryRepo) Search(ctx context.Context, userID int64, query, typ string, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	conds := []string{"user_id = ?", "status = 'active'"}
	args := []any{userID}
	if q := strings.TrimSpace(query); q != "" {
		esc := escapeLike(q)
		conds = append(conds, "(title LIKE ? OR content LIKE ?)")
		args = append(args, "%"+esc+"%", "%"+esc+"%")
	}
	if typ != "" {
		conds = append(conds, "type = ?")
		args = append(args, typ)
	}
	args = append(args, limit)
	var rows []Memory
	err := r.DB.SelectContext(ctx, &rows, `
SELECT * FROM memory WHERE `+strings.Join(conds, " AND ")+`
ORDER BY event_at DESC, id DESC LIMIT ?`, args...)
	return rows, err
}

// escapeLike 转义 LIKE 通配符，使用户输入按字面量匹配（配合 SQL 默认 \ 转义符）。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}
```
（`strings` 已在 memory.go import。）

- [ ] **Step 4: 跑测试确认通过**

Run: `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_agentchat_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestMemorySearch -v` → PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/memory.go internal/repo/memory_search_test.go
git commit -m "feat(agent-repo): MemoryRepo.Search 关键词检索(title/content LIKE + type 过滤)"
```

---

## Task 3: 4 个读工具（over repo）

**Files:** `internal/agent/mcp_tools.go`（replace 占位）、`internal/agent/mcp_server_test.go`（create）

> handler 直接调 repo，把结果 `json.Marshal` 成 `TextContent` 返回。测试直接调 handler 函数（不经 dsh），断言真实 DB 数据——快速、确定。

- [ ] **Step 1: 写失败测试**

Create `internal/agent/mcp_server_test.go`:

```go
package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func testDeps(t *testing.T) MCPDeps {
	t.Helper()
	dsn := testDSN(t)
	db, err := repo.NewDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return MCPDeps{
		Memory: &repo.MemoryRepo{DB: db}, Session: &repo.SessionRepo{DB: db},
		Transcript: &repo.TranscriptRepo{DB: db}, Topic: &repo.TopicRepo{DB: db},
		Todo: &repo.TodoRepo{DB: db},
	}
}

// firstText 取工具结果里第一段文本（handler 约定返回单个 TextContent）。
func firstText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("空工具结果")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("首段非 TextContent: %T", res.Content[0])
	}
	return tc.Text
}

func TestSearchMemoryTool(t *testing.T) {
	d := testDeps(t)
	ctx := t.Context()
	kw := "工具层检索验证词"
	ms := []*repo.Memory{{Type: "fact", Title: kw, Content: kw, SessionID: ids.New(), Status: "active", Confidence: 0.8}}
	if err := d.Memory.InsertExt(ctx, d.Memory.DB, ms); err != nil {
		t.Fatal(err)
	}
	res, _, err := searchMemoryHandler(d)(ctx, nil, searchMemoryArgs{Query: kw, Limit: 10})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	txt := firstText(t, res)
	var out []memoryOut
	if err := json.Unmarshal([]byte(txt), &out); err != nil {
		t.Fatalf("结果非 JSON 数组: %v; got=%s", err, txt)
	}
	var hit bool
	for _, m := range out {
		if m.Title == kw {
			hit = true
		}
	}
	if !hit {
		t.Errorf("search_memory 未返回关键词记忆; got=%s", txt)
	}
}

func TestGetTodosTool(t *testing.T) {
	d := testDeps(t)
	ctx := t.Context()
	res, _, err := getTodosHandler(d)(ctx, nil, getTodosArgs{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var out []todoOut
	if err := json.Unmarshal([]byte(firstText(t, res)), &out); err != nil {
		t.Fatalf("结果非 JSON 数组: %v", err)
	}
	// 不断言具体条数（库共享），只验证返回结构可解析 + 不报错。
}
```

> 需要一个 `testDSN`：`internal/repo/testutil.go` 的 `TestDSN` 在 repo 包内不可跨包引用。在 `mcp_server_test.go` 里加一个本地等价：
```go
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	return dsn
}
```
（import `os`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestSearchMemoryTool|TestGetTodosTool'` → 编译失败（`searchMemoryHandler`/`searchMemoryArgs`/`memoryOut`/`getTodosHandler` 等 undefined）。

- [ ] **Step 3: 实现工具**

Replace `internal/agent/mcp_tools.go`（Task 1 的占位）with:

```go
package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

const toolUserID = 1 // 单用户 MVP

// registerReadTools 注册全部只读工具到 server。
func registerReadTools(s *mcp.Server, d MCPDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_memory",
		Description: "按关键词检索我的记忆（title/content）。可选 type 过滤(event|fact|decision|idea|problem|preference)。返回记忆列表(含 id/类型/标题/内容/事件时间/重要度/所属话题)。",
	}, searchMemoryHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_timeline",
		Description: "查看我的录音时间线。不带 session_id 返回最近若干条录音会话(概要)；带 session_id 返回该会话的转写分段(说话人+文本+起止毫秒)。",
	}, getTimelineHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_topics",
		Description: "列出我的话题(项目/主题)及其记忆数与未完成待办数。可选 status 过滤(active|suggested)。",
	}, getTopicsHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_todos",
		Description: "列出我的待办。可选 status(suggested|confirmed|done) 与 topic_id 过滤。",
	}, getTodosHandler(d))
}

// ---------- 输出结构（LLM 友好、扁平） ----------

type memoryOut struct {
	ID         ids.ID     `json:"id"`
	Type       string     `json:"type"`
	Title      string     `json:"title"`
	Content    string     `json:"content"`
	EventAt    *time.Time `json:"event_at,omitempty"`
	Importance float64    `json:"importance"`
	Topics     []string   `json:"topics,omitempty"`
}

type sessionOut struct {
	SessionID  ids.ID    `json:"session_id"`
	CreatedAt  time.Time `json:"created_at"`
	Source     string    `json:"source"`
	Filename   string    `json:"filename"`
	DurationMS int64     `json:"duration_ms"`
	Status     string    `json:"status"`
}

type segmentOut struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
}

type topicOut struct {
	ID            ids.ID `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	MemoryCount   int    `json:"memory_count"`
	OpenTodoCount int    `json:"open_todo_count"`
}

type todoOut struct {
	ID     ids.ID     `json:"id"`
	Title  string     `json:"title"`
	Status string     `json:"status"`
	DueAt  *time.Time `json:"due_at,omitempty"`
	Topics []string   `json:"topics,omitempty"`
}

// jsonResult 把任意值 marshal 成单段 TextContent 结果（工具统一返回形态）。
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

// ---------- search_memory ----------

type searchMemoryArgs struct {
	Query string `json:"query" jsonschema:"检索关键词(匹配标题或内容)；留空则按 type 列最近记忆"`
	Type  string `json:"type,omitempty" jsonschema:"可选记忆类型过滤: event|fact|decision|idea|problem|preference"`
	Limit int    `json:"limit,omitempty" jsonschema:"最多返回条数, 默认 20, 上限 50"`
}

func searchMemoryHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, searchMemoryArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a searchMemoryArgs) (*mcp.CallToolResult, any, error) {
		limit := a.Limit
		if limit <= 0 {
			limit = 20
		}
		ms, err := d.Memory.Search(ctx, toolUserID, a.Query, a.Type, limit)
		if err != nil {
			return nil, nil, err
		}
		out := make([]memoryOut, 0, len(ms))
		for _, m := range ms {
			out = append(out, memoryOut{ID: m.ID, Type: m.Type, Title: m.Title, Content: m.Content, EventAt: m.EventAt, Importance: m.Importance})
		}
		return jsonResult(out)
	}
}

// ---------- get_timeline ----------

type getTimelineArgs struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"某条录音会话 id；给出则返回该会话的转写分段"`
	Limit     int    `json:"limit,omitempty" jsonschema:"不带 session_id 时, 返回最近录音条数, 默认 20, 上限 50"`
}

func getTimelineHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, getTimelineArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getTimelineArgs) (*mcp.CallToolResult, any, error) {
		if a.SessionID != "" {
			sid, err := ids.ParseID(a.SessionID)
			if err != nil {
				return nil, nil, err
			}
			tr, err := d.Transcript.GetBySession(ctx, sid)
			if err != nil {
				return nil, nil, err
			}
			segs, err := d.Transcript.ListSegments(ctx, tr.ID)
			if err != nil {
				return nil, nil, err
			}
			out := make([]segmentOut, 0, len(segs))
			for _, s := range segs {
				sp := s.SpeakerLabel
				if sp == "" {
					sp = "未知"
				}
				out = append(out, segmentOut{Speaker: sp, Text: s.Text, StartMS: s.StartMS})
			}
			return jsonResult(out)
		}
		limit := a.Limit
		if limit <= 0 || limit > 50 {
			limit = 20
		}
		ss, err := d.Session.List(ctx, limit, 0)
		if err != nil {
			return nil, nil, err
		}
		out := make([]sessionOut, 0, len(ss))
		for _, s := range ss {
			out = append(out, sessionOut{SessionID: s.ID, CreatedAt: s.CreatedAt, Source: s.Source, Filename: s.Filename, DurationMS: s.DurationMS, Status: s.Status})
		}
		return jsonResult(out)
	}
}

// ---------- get_topics ----------

type getTopicsArgs struct {
	Status string `json:"status,omitempty" jsonschema:"可选状态过滤: active|suggested"`
}

func getTopicsHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, getTopicsArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getTopicsArgs) (*mcp.CallToolResult, any, error) {
		ts, err := d.Topic.ListWithCounts(ctx, toolUserID)
		if err != nil {
			return nil, nil, err
		}
		out := make([]topicOut, 0, len(ts))
		for _, t := range ts {
			if a.Status != "" && t.Status != a.Status {
				continue
			}
			out = append(out, topicOut{ID: t.ID, Name: t.Name, Status: t.Status, MemoryCount: t.MemoryCount, OpenTodoCount: t.OpenTodoCount})
		}
		return jsonResult(out)
	}
}

// ---------- get_todos ----------

type getTodosArgs struct {
	Status  string `json:"status,omitempty" jsonschema:"可选状态过滤: suggested|confirmed|done"`
	TopicID string `json:"topic_id,omitempty" jsonschema:"可选按话题 id 过滤"`
}

func getTodosHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, getTodosArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getTodosArgs) (*mcp.CallToolResult, any, error) {
		var topicID *ids.ID
		if a.TopicID != "" {
			id, err := ids.ParseID(a.TopicID)
			if err != nil {
				return nil, nil, err
			}
			topicID = &id
		}
		rows, err := d.Todo.List(ctx, a.Status, topicID)
		if err != nil {
			return nil, nil, err
		}
		out := make([]todoOut, 0, len(rows))
		for _, r := range rows {
			names := make([]string, 0, len(r.Topics))
			for _, tp := range r.Topics {
				names = append(names, tp.Name)
			}
			out = append(out, todoOut{ID: r.Todo.ID, Title: r.Title, Status: r.Status, DueAt: r.DueAt, Topics: names})
		}
		return jsonResult(out)
	}
}
```

> 注意：`TodoRow.Topics` 元素类型是 `repo.TopicInfo`——确认其字段名（`grep "type TopicInfo" internal/repo`）；上面用 `tp.Name`，若字段名不同按实际改。`memoryOut.Topics` 本期不填充（`search_memory` 用 `MemoryRepo.Search` 返回 `[]Memory` 无 topics；如需可后续换 `List` 版本）。删除 `mcp_server.go` 里 `registerReadTools` 的占位注释，改为正式调用。

- [ ] **Step 4: 跑测试确认通过**

Run: `TEST_MYSQL_DSN="...zhiwei_agentchat_test..." go test ./internal/agent/ -run 'TestSearchMemoryTool|TestGetTodosTool' -v` → PASS。
再 `go build ./...` + `gofmt -l internal/agent internal/repo`（你的文件无输出）+ `go vet ./internal/agent/`。

- [ ] **Step 5: Commit**

```bash
git add internal/agent/mcp_tools.go internal/agent/mcp_server.go internal/agent/mcp_server_test.go
git commit -m "feat(agent-mcp): 4 个只读工具 search_memory/get_timeline/get_topics/get_todos(over repo)"
```

---

## Task 4: 主服务正式装配 MCP 端点

**Files:** `cmd/zhiwei-server/main.go`（modify）

- [ ] **Step 1: 确认 main 里的 repo 变量**

Run: `grep -nE "repo\.(Memory|Session|Transcript|Topic|Todo)Repo|MemoryRepo|NewRouter|RegisterMemory" cmd/zhiwei-server/main.go`
据结果确定：主服务里 `memories`/`sessions`/... 这些 repo 的实际变量名，以及 chi mux 变量名。

- [ ] **Step 2: 正式挂载（替换 Task 1 的临时挂载）**

把 Task 1 Step 3 临时加的挂载整理为正式：在 `api.RegisterXxx(...)` 那一批之后、启动监听之前：
```go
	// MCP 工具端点（仅供本机 dsh 边车经 streamable-http 连回；不对外）。
	mcpSrv := agent.NewMCPServer(agent.MCPDeps{
		Memory:     memories,
		Session:    sessions,
		Transcript: transcripts,
		Topic:      topics,
		Todo:       todos,
	})
	r.Handle("/internal/mcp", agent.MCPHandler(mcpSrv))
	r.Handle("/internal/mcp/*", agent.MCPHandler(mcpSrv))
```
（变量名按 Step 1 实际；确保 `import "zhiwei/internal/agent"`。若某 repo main 里尚未构造，补 `xxx := &repo.XxxRepo{DB: db}`。）

- [ ] **Step 3: 构建 + 起服务冒烟**

Run: `go build ./cmd/zhiwei-server` → 通过。
起服务 `go run ./cmd/zhiwei-server`（需环境变量），Read 日志确认无 panic、监听 8080。（可选：`curl` `/internal/mcp` 属 streamable-http，需 MCP 握手，非普通 GET；不强求手测，交给 Task 5 端到端。）

- [ ] **Step 4: Commit**

```bash
git add cmd/zhiwei-server/main.go
git commit -m "feat(agent-mcp): 主服务挂载 /internal/mcp(进程内, 注入现有 repo)"
```

---

## Task 5: 端到端验证（dsh 经真工具读真数据）

**Files:** 无（验证 + 记录）

- [ ] **Step 1: 备数据 + 起服务**

确保独立库或 dev 库里有些 memory/topic/todo/session 数据（可用现有录音流程，或 Task 2/3 测试插入的残留）。起主服务连该库（`ZW_MYSQL_DSN` 指向有数据的库；MCP 工具用 `user_id=1`）。

- [ ] **Step 2: 驱动 dsh 问一个需要读数据的问题**

Run（分步、无 pipe）：
`node services/agent-sidecar/spike/drive.mjs doubao-seed-1-6-250615 services/agent-sidecar/cordis.agent.yml "用 get_todos 工具看看我有哪些待办，然后用一句话概括。"`
Expected: `spike/logs/wire.ndjson` 出现 `tool/call name:"mcp__zhiwei__get_todos"` + `tool/result` 含真实待办 JSON；最终 assistant 文本基于真实数据作答。再试一句触发 `search_memory`/`get_topics`/`get_timeline` 各一次。

- [ ] **Step 3: 记录验收**

把验证结论（哪些工具被真实调用、返回真数据、模型据此作答）追加到 `docs/superpowers/specs/2026-08-24-agent-chatbot-system-design.md` 末尾一节「P2a 验证记录」或本 plan 末尾。若某工具的 schema 被 dsh/模型误用（参数名不直观等），据实微调工具 description/字段。

- [ ] **Step 4: Commit（若有微调）**

```bash
git add -A
git commit -m "docs(agent-mcp): P2a 端到端验证记录 + 工具描述微调"
```

---

## 收尾验收（P2a 完成标志）

- [ ] `go build ./...`、`go vet ./internal/agent ./internal/repo` 通过；新文件 gofmt 干净。
- [ ] `MemoryRepo.Search` + 4 个工具 handler 的集成测试在独立库全绿。
- [ ] dsh 边车（Node 驱动）经 **streamable-http** 连 `/internal/mcp`，真实调用到 4 个工具、拿到真 DB 数据、据此作答（Task 1 ping + Task 5 真工具）。
- [ ] `/internal/mcp` 挂在主服务、复用现有 repo/DB 池；仅本机。

**下一步（不在本计划内）：** P2b = Go `AgentRuntime`（Go spawn+驱动 dsh 的 JSON-RPC/stdio 客户端 + 事件流 + runtime_fake）+ 对话编排，Go 侧驱动整个循环（替代 Node drive.mjs）。P2c = WS + 聊天前端。P2d = 写-提议 + 确认。
