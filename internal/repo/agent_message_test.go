package repo

import (
	"encoding/json"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestAgentMessageAppendAndList(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
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

	list, err := mr.ListByConversation(ctx, 1, conv.ID)
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
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &AgentMessageRepo{DB: db}
	ctx := t.Context()
	list, err := mr.ListByConversation(ctx, 1, ids.New())
	if err != nil {
		t.Fatalf("ListByConversation empty: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("空会话应返回空列表, got %d", len(list))
	}
}

func TestCountByConversation(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &AgentConversationRepo{DB: db}
	msgRepo := &AgentMessageRepo{DB: db}
	ctx := t.Context()

	c := &AgentConversation{Title: "计数用"}
	if err := convRepo.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	cid := &c.ID

	// 2 条 user + 1 条 assistant
	for _, m := range []*AgentMessage{
		{ConversationID: cid, Role: "user", Content: "问1"},
		{ConversationID: cid, Role: "assistant", Content: "答1"},
		{ConversationID: cid, Role: "user", Content: "问2"},
	} {
		if err := msgRepo.Append(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	n, err := msgRepo.CountByConversation(ctx, 1, c.ID)
	if err != nil {
		t.Fatalf("CountByConversation: %v", err)
	}
	if n != 2 {
		t.Errorf("user 消息数应为 2, got %d", n)
	}

	// 越权：user_id=2 看不到 user_id=1 的消息 → 0
	n2, err := msgRepo.CountByConversation(ctx, 2, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("越权计数应为 0, got %d", n2)
	}
}
