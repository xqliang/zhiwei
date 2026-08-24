package repo

import (
	"testing"
)

func TestAgentConversationCRUD(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &AgentConversationRepo{DB: db}
	ctx := t.Context()

	c := &AgentConversation{Title: "关于项目A的对话"}
	if err := r.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == 0 {
		t.Fatal("Create 未回填 ID")
	}
	if c.UserID != 1 {
		t.Errorf("UserID 应默认 1, got %d", c.UserID)
	}
	if c.DSHSessionID == "" {
		t.Error("DSHSessionID 应默认非空（回退用会话 ID 字符串）")
	}

	got, err := r.Get(ctx, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "关于项目A的对话" || got.Status != "active" {
		t.Errorf("Get 结果异常: %+v", got)
	}

	if err := r.SetDSHSession(ctx, c.ID, "sess-xyz"); err != nil {
		t.Fatalf("SetDSHSession: %v", err)
	}
	if err := r.Touch(ctx, c.ID); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	list, err := r.List(ctx, 1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, x := range list {
		if x.ID == c.ID {
			found = true
			if x.DSHSessionID != "sess-xyz" {
				t.Errorf("SetDSHSession 未生效: %q", x.DSHSessionID)
			}
		}
	}
	if !found {
		t.Error("List 未包含新建会话")
	}
}
