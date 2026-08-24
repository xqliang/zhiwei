package repo

import (
	"database/sql"
	"errors"
	"testing"

	"zhiwei/internal/ids"
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

// TestAgentConversationListOrdering 验证 List 按 last_active_at 倒序；用唯一 user_id 隔离本用例数据。
func TestAgentConversationListOrdering(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &AgentConversationRepo{DB: db}
	ctx := t.Context()
	uid := int64(ids.New()) // 每次运行唯一，隔离本用例

	older := &AgentConversation{UserID: uid, Title: "老"}
	if err := r.Create(ctx, older); err != nil {
		t.Fatalf("Create older: %v", err)
	}
	newer := &AgentConversation{UserID: uid, Title: "新"}
	if err := r.Create(ctx, newer); err != nil {
		t.Fatalf("Create newer: %v", err)
	}
	// Touch older 使其 last_active_at 变为最新 → 应排到最前
	if err := r.Touch(ctx, older.ID); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	list, err := r.List(ctx, uid)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 条, got %d", len(list))
	}
	if list[0].ID != older.ID {
		t.Errorf("Touch 后 older 应排最前, got 首条 %v", list[0].ID)
	}
}

// TestAgentConversationMissing 验证缺失 id 的语义：Get 冒泡 ErrNoRows，Touch/SetDSHSession 返回 nil。
func TestAgentConversationMissing(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &AgentConversationRepo{DB: db}
	ctx := t.Context()
	missing := ids.New()

	if _, err := r.Get(ctx, missing); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get 缺失应返回 sql.ErrNoRows, got %v", err)
	}
	if err := r.Touch(ctx, missing); err != nil {
		t.Errorf("Touch 缺失应返回 nil, got %v", err)
	}
	if err := r.SetDSHSession(ctx, missing, "x"); err != nil {
		t.Errorf("SetDSHSession 缺失应返回 nil, got %v", err)
	}
}
