package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// registerWriteTools 注册全部写-提议工具（propose_*）。每个工具只读现值 + 建一条 pending
// agent_proposal（{old,new} 载荷），绝不直接改领域行；用户经 /api/agent/proposals/{id}/confirm
// 确认后才在单事务落库（见 proposals.go）。这是提示注入的根防线（spec §8）：对话/转写里的
// 「把 X 改成 Y」最多生成一个待确认提议，永远要人点确认。全部限 user_id=1。
func registerWriteTools(s *mcp.Server, d MCPDeps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_memory_edit",
		Description: "提议修改一条记忆的标题/内容（不立即生效，返回待确认提议；用户确认后才落库）。至少给 new_title 或 new_content 之一。",
	}, proposeMemoryEditHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_memory_dismiss",
		Description: "提议忽略(dismiss)一条记忆（不立即生效，返回待确认提议）。",
	}, proposeMemoryDismissHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_topic_rename",
		Description: "提议给话题改名（不立即生效，返回待确认提议）。",
	}, proposeTopicRenameHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_topic_confirm",
		Description: "提议把建议话题确认为正式话题(status=active)（不立即生效，返回待确认提议）。",
	}, proposeTopicStatusHandler(d, "topic_confirm", "active"))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_topic_dismiss",
		Description: "提议忽略(dismiss)一个话题（不立即生效，返回待确认提议）。",
	}, proposeTopicStatusHandler(d, "topic_dismiss", "dismissed"))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_todo_create",
		Description: "提议新建一条待办（不立即生效，返回待确认提议；用户确认后才入库为 confirmed）。",
	}, proposeTodoCreateHandler(d))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "propose_todo_status",
		Description: "提议改变一条待办的状态(confirmed|done|dismissed)（不立即生效，返回待确认提议）。",
	}, proposeTodoStatusHandler(d))
}

// proposeResult 把落库的 pending 提议序列化返回，供前端渲染确认卡（old/new 并排 + 确认/放弃）。
func proposeResult(p *repo.AgentProposal) (*mcp.CallToolResult, any, error) {
	return jsonResult(p)
}

// ---- propose_memory_edit（memory_update）----
type proposeMemoryEditArgs struct {
	MemoryID   string `json:"memory_id" jsonschema:"要修改的记忆 id"`
	NewTitle   string `json:"new_title,omitempty" jsonschema:"新标题(可选)"`
	NewContent string `json:"new_content,omitempty" jsonschema:"新内容(可选)"`
	Rationale  string `json:"rationale,omitempty" jsonschema:"给用户看的修改理由"`
}

func proposeMemoryEditHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, proposeMemoryEditArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeMemoryEditArgs) (*mcp.CallToolResult, any, error) {
		id, err := ids.ParseID(a.MemoryID)
		if err != nil {
			return nil, nil, err
		}
		m, err := d.Memory.Get(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		// new 只含实际给出的字段；两者都空则不构成有效修改。
		newFields := map[string]any{}
		if a.NewTitle != "" {
			newFields["title"] = a.NewTitle
		}
		if a.NewContent != "" {
			newFields["content"] = a.NewContent
		}
		if len(newFields) == 0 {
			return nil, nil, fmt.Errorf("propose_memory_edit 需至少给出 new_title 或 new_content")
		}
		payload, _ := json.Marshal(map[string]any{
			"old": map[string]any{"title": m.Title, "content": m.Content},
			"new": newFields,
		})
		return proposeAndReturn(ctx, d, &repo.AgentProposal{
			Kind: "memory_update", TargetKind: "memory", TargetID: &id,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// ---- propose_memory_dismiss（memory_dismiss）----
type proposeMemoryDismissArgs struct {
	MemoryID  string `json:"memory_id" jsonschema:"要忽略的记忆 id"`
	Rationale string `json:"rationale,omitempty" jsonschema:"忽略理由"`
}

func proposeMemoryDismissHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, proposeMemoryDismissArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeMemoryDismissArgs) (*mcp.CallToolResult, any, error) {
		id, err := ids.ParseID(a.MemoryID)
		if err != nil {
			return nil, nil, err
		}
		m, err := d.Memory.Get(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		payload, _ := json.Marshal(map[string]any{
			"old": map[string]any{"status": m.Status, "title": m.Title},
			"new": map[string]any{"status": "dismissed"},
		})
		return proposeAndReturn(ctx, d, &repo.AgentProposal{
			Kind: "memory_dismiss", TargetKind: "memory", TargetID: &id,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// ---- propose_topic_rename（topic_rename）----
type proposeTopicRenameArgs struct {
	TopicID   string `json:"topic_id" jsonschema:"要改名的话题 id"`
	NewName   string `json:"new_name" jsonschema:"新话题名"`
	Rationale string `json:"rationale,omitempty" jsonschema:"改名理由"`
}

func proposeTopicRenameHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, proposeTopicRenameArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeTopicRenameArgs) (*mcp.CallToolResult, any, error) {
		if a.NewName == "" {
			return nil, nil, fmt.Errorf("propose_topic_rename 需给出 new_name")
		}
		id, err := ids.ParseID(a.TopicID)
		if err != nil {
			return nil, nil, err
		}
		t, err := d.Topic.Get(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		payload, _ := json.Marshal(map[string]any{
			"old": map[string]any{"name": t.Name},
			"new": map[string]any{"name": a.NewName},
		})
		return proposeAndReturn(ctx, d, &repo.AgentProposal{
			Kind: "topic_rename", TargetKind: "topic", TargetID: &id,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// ---- propose_topic_confirm / propose_topic_dismiss（共用，只差 kind 与目标 status）----
type proposeTopicStatusArgs struct {
	TopicID   string `json:"topic_id" jsonschema:"目标话题 id"`
	Rationale string `json:"rationale,omitempty" jsonschema:"理由"`
}

func proposeTopicStatusHandler(d MCPDeps, kind, newStatus string) func(context.Context, *mcp.CallToolRequest, proposeTopicStatusArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeTopicStatusArgs) (*mcp.CallToolResult, any, error) {
		id, err := ids.ParseID(a.TopicID)
		if err != nil {
			return nil, nil, err
		}
		t, err := d.Topic.Get(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		payload, _ := json.Marshal(map[string]any{
			"old": map[string]any{"status": t.Status, "name": t.Name},
			"new": map[string]any{"status": newStatus},
		})
		return proposeAndReturn(ctx, d, &repo.AgentProposal{
			Kind: kind, TargetKind: "topic", TargetID: &id,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// ---- propose_todo_create（todo_create；无 target_id）----
type proposeTodoCreateArgs struct {
	Title     string `json:"title" jsonschema:"待办标题"`
	DueAt     string `json:"due_at,omitempty" jsonschema:"截止时间, RFC3339 含时区(如 2026-08-26T10:00:00+08:00)"`
	TopicID   string `json:"topic_id,omitempty" jsonschema:"可选归属话题 id"`
	Rationale string `json:"rationale,omitempty" jsonschema:"新建理由"`
}

func proposeTodoCreateHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, proposeTodoCreateArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeTodoCreateArgs) (*mcp.CallToolResult, any, error) {
		if a.Title == "" {
			return nil, nil, fmt.Errorf("propose_todo_create 需给出 title")
		}
		newFields := map[string]any{"title": a.Title}
		if a.DueAt != "" {
			if _, err := time.Parse(time.RFC3339, a.DueAt); err != nil {
				return nil, nil, fmt.Errorf("due_at 需 RFC3339 含时区: %w", err)
			}
			newFields["due_at"] = a.DueAt
		}
		if a.TopicID != "" {
			if _, err := ids.ParseID(a.TopicID); err != nil {
				return nil, nil, fmt.Errorf("topic_id 非法: %w", err)
			}
			newFields["topic_id"] = a.TopicID
		}
		payload, _ := json.Marshal(map[string]any{"new": newFields})
		return proposeAndReturn(ctx, d, &repo.AgentProposal{
			Kind: "todo_create", TargetKind: "todo", // TargetID 空：新建类无目标行
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// ---- propose_todo_status（todo_status）----
type proposeTodoStatusArgs struct {
	TodoID    string `json:"todo_id" jsonschema:"目标待办 id"`
	NewStatus string `json:"new_status" jsonschema:"新状态: confirmed|done|dismissed"`
	Rationale string `json:"rationale,omitempty" jsonschema:"理由"`
}

func proposeTodoStatusHandler(d MCPDeps) func(context.Context, *mcp.CallToolRequest, proposeTodoStatusArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a proposeTodoStatusArgs) (*mcp.CallToolResult, any, error) {
		switch a.NewStatus {
		case "confirmed", "done", "dismissed":
		default:
			return nil, nil, fmt.Errorf("new_status 需为 confirmed|done|dismissed, got %q", a.NewStatus)
		}
		id, err := ids.ParseID(a.TodoID)
		if err != nil {
			return nil, nil, err
		}
		td, err := d.Todo.Get(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		payload, _ := json.Marshal(map[string]any{
			"old": map[string]any{"status": td.Status, "title": td.Title},
			"new": map[string]any{"status": a.NewStatus},
		})
		return proposeAndReturn(ctx, d, &repo.AgentProposal{
			Kind: "todo_status", TargetKind: "todo", TargetID: &id,
			Payload: json.RawMessage(payload), Rationale: a.Rationale,
		})
	}
}

// proposeAndReturn 落一条 pending 提议并返回给前端渲染确认卡。工具永不 mutate 领域行。
func proposeAndReturn(ctx context.Context, d MCPDeps, p *repo.AgentProposal) (*mcp.CallToolResult, any, error) {
	if err := d.Proposals.Create(ctx, p); err != nil {
		return nil, nil, err
	}
	return proposeResult(p)
}
