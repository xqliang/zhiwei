package repo

import (
	"encoding/json"
	"testing"

	"zhiwei/internal/ids"
)

func TestAgentMessageAppendAndList(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cr := &AgentConversationRepo{DB: db}
	mr := &AgentMessageRepo{DB: db}
	ctx := t.Context()

	conv := &AgentConversation{Title: "t"}
	if err := cr.Create(ctx, conv); err != nil {
		t.Fatalf("conv Create: %v", err)
	}

	// 用户消息（纯文本）
	um := &AgentMessage{ConversationID: &conv.ID, Role: "user", Content: "帮我查上周的记忆"}
	if err := mr.Append(ctx, um); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	if um.ID == 0 || um.Kind != "text" {
		t.Errorf("默认字段异常: id=%v kind=%q", um.ID, um.Kind)
	}

	// 助手消息（带 citations JSON）
	cites := json.RawMessage(`[{"memory_id":"123","reason":"相关"}]`)
	am := &AgentMessage{ConversationID: &conv.ID, Role: "assistant", Content: "找到 1 条", Citations: &cites}
	if err := mr.Append(ctx, am); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}

	list, err := mr.ListByConversation(ctx, conv.ID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 条消息, got %d", len(list))
	}
	if list[0].Role != "user" || list[1].Role != "assistant" {
		t.Errorf("顺序应按 id 升序（user 先）: %q,%q", list[0].Role, list[1].Role)
	}
	if list[1].Citations == nil || len(*list[1].Citations) == 0 {
		t.Error("assistant 的 citations 未持久化")
	}
}

// TestAgentMessageListEmpty：无消息的会话返回空列表（非报错）。
func TestAgentMessageListEmpty(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &AgentMessageRepo{DB: db}
	ctx := t.Context()
	list, err := mr.ListByConversation(ctx, ids.New())
	if err != nil {
		t.Fatalf("ListByConversation empty: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("空会话应返回空列表, got %d", len(list))
	}
}
