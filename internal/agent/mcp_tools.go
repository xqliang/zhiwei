package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/search"
)

// toolUserID 是单用户 MVP 的用户 id。2B-A 起 MCP 工具（本文件 + mcp_write_tools.go +
// mcp_profile_tools.go）不再直接用它，改用 NewMCPServer 注入、逐层透传的 userID；此常量仍供
// 尚未多用户化的 HTTP/编排侧（context.go / proposals.go / handlers.go / ws.go）与测试使用（2B-B 再收敛）。
const toolUserID = 1

// registerReadTools 注册全部只读工具到 server。userID 由 NewMCPServer 注入，透传给各 handler 工厂。
func registerReadTools(s *mcp.Server, d MCPDeps, userID int64) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_memory",
		Description: "按语义/关键词检索我的记忆（title/content）。可选 type 过滤(event|fact|decision|idea|problem|preference)。返回记忆列表(含 id/类型/标题/内容/事件时间/重要度)。",
	}, searchMemoryHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_timeline",
		Description: "查看我的录音时间线。不带 session_id 返回最近若干条录音会话(概要)；带 session_id 返回该会话的转写分段(说话人+文本+起止毫秒)。",
	}, getTimelineHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_topics",
		Description: "列出我的话题(项目/主题)及其记忆数与未完成待办数。可选 status 过滤(active|suggested)。",
	}, getTopicsHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_todos",
		Description: "列出我的待办。可选 status(suggested|confirmed|done) 与 topic_id 过滤。",
	}, getTodosHandler(d, userID))

	mcp.AddTool(s, &mcp.Tool{
		Name: "web_search",
		Description: "联网搜索公开网络信息（搜索引擎）。用于：不确定或可能有时效性的问题、不了解的专业术语/名词、需要外部资料佐证时。" +
			"返回结果列表(标题/链接/摘要)；要看某条结果详情时配合 web_fetch。与用户个人数据无关的通用问题优先用它查证。" +
			"组词技巧：不要只搜原词——歧义缩写/新词要结合对话语境加领域关键词（如聊智能体时搜「ASL 智能体 安全」而非只搜「ASL」）；" +
			"若首轮结果与对话语境明显不符，换更具体的关键词（加领域词/疑似全称）再搜 1-2 次，不要硬用不符的结果作答。",
	}, webSearchHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "web_fetch",
		Description: "抓取指定 URL 的网页正文（纯文本）。用于：阅读 web_search 结果中的某个链接、或用户明确给出的网址。仅支持 http/https 公网页面。",
	}, webFetchHandler(d))
}

type memoryOut struct {
	ID         ids.ID     `json:"id"`
	Type       string     `json:"type"`
	Title      string     `json:"title"`
	Content    string     `json:"content"`
	EventAt    *time.Time `json:"event_at,omitempty"`
	Importance float64    `json:"importance"`
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
	EndMS   int64  `json:"end_ms"`
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

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

type searchMemoryArgs struct {
	Query string `json:"query,omitempty" jsonschema:"检索关键词(匹配标题或内容)；留空则按 type 列最近记忆"`
	Type  string `json:"type,omitempty" jsonschema:"可选记忆类型: event|fact|decision|idea|problem|preference"`
	Limit int    `json:"limit,omitempty" jsonschema:"最多返回条数, 默认 20, 上限 50"`
	// ID 兼容字段：dsh 模型偶尔会在 search_memory 实参里幻觉多发一个 id（实测形如
	// {"query":"划船经历","id":11}，id 可能是整数也可能是字符串）。go-sdk 从结构体推断
	// InputSchema 时对 struct 固定设 additionalProperties:false（见 google/jsonschema-go
	// infer.go），不收未知字段会使整个工具调用校验失败（server.go 报
	// validating "arguments": …unexpected additional properties ["id"]）。声明此 any 字段让
	// schema 放行且不限类型；handler 忽略它（按 query/type 检索，从不按 id 直取）。
	ID any `json:"id,omitempty" jsonschema:"忽略此字段（兼容模型误传的记忆 id）；检索始终按 query 进行"`
}

func searchMemoryHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, searchMemoryArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a searchMemoryArgs) (*mcp.CallToolResult, any, error) {
		limit := a.Limit
		if limit <= 0 {
			limit = 20
		}
		var ms []repo.Memory
		// 有向量检索且给了 query：优先「向量+关键词」混合；结果为空则退回关键词 LIKE。
		if d.Retrieve != nil && a.Query != "" {
			hit, err := d.Retrieve.Search(ctx, userID, a.Query, a.Type, limit)
			if err == nil && len(hit) > 0 {
				ms = hit
			}
		}
		if ms == nil {
			var err error
			ms, err = d.Memory.Search(ctx, userID, a.Query, a.Type, limit)
			if err != nil {
				return nil, nil, err
			}
		}
		out := make([]memoryOut, 0, len(ms))
		for _, m := range ms {
			out = append(out, memoryOut{ID: m.ID, Type: m.Type, Title: m.Title, Content: m.Content, EventAt: m.EventAt, Importance: m.Importance})
		}
		return jsonResult(out)
	}
}

type getTimelineArgs struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"某条录音会话 id；给出则返回该会话的转写分段"`
	Limit     int    `json:"limit,omitempty" jsonschema:"不带 session_id 时返回最近录音条数, 默认 20, 上限 50"`
}

func getTimelineHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, getTimelineArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getTimelineArgs) (*mcp.CallToolResult, any, error) {
		if a.SessionID != "" {
			sid, err := ids.ParseID(a.SessionID)
			if err != nil {
				return nil, nil, err
			}
			// I1 IDOR 修复：带 session_id 读转写前，先按 userID 校验会话归属（SessionRepo.Get 带
			// user 过滤，越权/不存在返回 sql.ErrNoRows）。绝不凭 session_id 直读他人转写分段。
			if _, err := d.Session.Get(ctx, userID, sid); err != nil {
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
				out = append(out, segmentOut{Speaker: sp, Text: s.Text, StartMS: s.StartMS, EndMS: s.EndMS})
			}
			return jsonResult(out)
		}
		limit := a.Limit
		if limit <= 0 {
			limit = 20
		}
		if limit > 50 {
			limit = 50
		}
		ss, err := d.Session.List(ctx, userID, limit, 0)
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

type getTopicsArgs struct {
	Status string `json:"status,omitempty" jsonschema:"可选状态过滤: active|suggested"`
}

func getTopicsHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, getTopicsArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getTopicsArgs) (*mcp.CallToolResult, any, error) {
		ts, err := d.Topic.ListWithCounts(ctx, userID)
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

type getTodosArgs struct {
	Status  string `json:"status,omitempty" jsonschema:"可选状态过滤: suggested|confirmed|done"`
	TopicID string `json:"topic_id,omitempty" jsonschema:"可选按话题 id 过滤"`
}

func getTodosHandler(d MCPDeps, userID int64) func(context.Context, *mcp.CallToolRequest, getTodosArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a getTodosArgs) (*mcp.CallToolResult, any, error) {
		var topicID *ids.ID
		if a.TopicID != "" {
			id, err := ids.ParseID(a.TopicID)
			if err != nil {
				return nil, nil, err
			}
			topicID = &id
		}
		rows, err := d.Todo.List(ctx, userID, a.Status, topicID)
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

// ---- web_search / web_fetch（Phase 2 联网工具，全局配置不按 userID 隔离）----

type webResultOut struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type webSearchArgs struct {
	Query string `json:"query" jsonschema:"搜索关键词（自然语言或关键词均可）"`
	Limit int    `json:"limit,omitempty" jsonschema:"最多返回条数, 默认 8, 上限 10"`
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

// webPageOut 是 web_fetch 的单页结果（与 webResultOut 同为具名输出类型，便于测试复用）。
type webPageOut struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`
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
		return jsonResult(webPageOut{URL: p.URL, Title: p.Title, Text: p.Text})
	}
}
