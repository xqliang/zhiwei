package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
)

const toolUserID = 1 // 单用户 MVP

// registerReadTools 注册全部只读工具到 server。
func registerReadTools(s *mcp.Server, d MCPDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_memory",
		Description: "按关键词检索我的记忆（title/content）。可选 type 过滤(event|fact|decision|idea|problem|preference)。返回记忆列表(含 id/类型/标题/内容/事件时间/重要度)。",
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

type getTimelineArgs struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"某条录音会话 id；给出则返回该会话的转写分段"`
	Limit     int    `json:"limit,omitempty" jsonschema:"不带 session_id 时返回最近录音条数, 默认 20, 上限 50"`
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
