package repo

import (
	"encoding/json"
	"testing"

	"zhiwei/internal/ids"
)

func TestValidProposalStatus(t *testing.T) {
	for _, s := range []string{"pending", "applied", "dismissed", "expired"} {
		if !ValidProposalStatus(s) {
			t.Errorf("%q 应合法", s)
		}
	}
	for _, s := range []string{"", "confirmed", "foo"} {
		if ValidProposalStatus(s) {
			t.Errorf("%q 应非法", s)
		}
	}
}

func TestAgentProposalCRUDAndResolve(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	pr := &AgentProposalRepo{DB: db}
	ctx := t.Context()

	target := ids.New()
	p := &AgentProposal{
		UserID:     1, // C1: UserID 必填（Create 不再静默默认 1）
		Kind:       "memory_update",
		TargetKind: "memory",
		TargetID:   &target,
		Payload:    json.RawMessage(`{"old":{"content":"旧"},"new":{"content":"新"}}`),
		Rationale:  "用户口述订正",
	}
	if err := pr.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.ID == 0 || p.Status != "pending" {
		t.Errorf("默认字段异常: id=%v status=%q", p.ID, p.Status)
	}

	pend, err := pr.ListPending(ctx, 1)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pend) == 0 {
		t.Fatal("ListPending 应含新建提议")
	}

	// 非法状态被拒
	if _, err := pr.Resolve(ctx, db, p.ID, "confirmed", nil); err == nil {
		t.Error("Resolve 非法状态应报错")
	}

	// 合法：applied + 回填 applied_ref
	ref := ids.New()
	applied, err := pr.Resolve(ctx, db, p.ID, "applied", &ref)
	if err != nil {
		t.Fatalf("Resolve applied: %v", err)
	}
	if !applied {
		t.Error("首次 applied 应返回 true")
	}
	got, err := pr.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "applied" {
		t.Errorf("status 应 applied, got %q", got.Status)
	}
	if got.AppliedRef == nil || *got.AppliedRef != ref {
		t.Error("applied_ref 未回填")
	}
	if got.ResolvedAt == nil {
		t.Error("resolved_at 未设置")
	}

	// apply-once：已 applied 的提议再 resolve 应返回 (false, nil)（CAS 未命中）
	again, err := pr.Resolve(ctx, db, p.ID, "dismissed", nil)
	if err != nil || again {
		t.Errorf("已终态提议再 resolve 应为 (false,nil), got (%v,%v)", again, err)
	}

	// applied 后不再出现在 pending
	pend2, _ := pr.ListPending(ctx, 1)
	for _, x := range pend2 {
		if x.ID == p.ID {
			t.Error("applied 提议不应再在 pending 列表")
		}
	}
}

// TestAgentProposalCreateRequiresUserID 锁定 C1 加固：Create 对 UserID==0 直接报错，
// 绝不再静默落 user 1（静默默认 1 正是「agent 提议未设 UserID → 全部误挂 user 1」跨租户
// 泄漏 bug 被长期掩盖的根因）。设了非 0 UserID 才放行。
func TestAgentProposalCreateRequiresUserID(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	pr := &AgentProposalRepo{DB: db}
	ctx := t.Context()

	// UserID==0（未设）→ 报错，且不落库
	p0 := &AgentProposal{Kind: "todo_create", TargetKind: "todo", Rationale: "缺 UserID"}
	if err := pr.Create(ctx, p0); err == nil {
		t.Fatal("Create 对 UserID==0 应报错（不再静默默认 1）")
	}
	if p0.ID != 0 {
		// 未落库：不应生成可查询的行（即便本地赋了 ID 也无 INSERT）
		t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_proposal WHERE id = ?", p0.ID.Int64()) })
	}

	// 设了非 0 UserID → 正常落库
	p1 := &AgentProposal{UserID: 7, Kind: "todo_create", TargetKind: "todo", Rationale: "有 UserID"}
	if err := pr.Create(ctx, p1); err != nil {
		t.Fatalf("Create 带 UserID 应成功: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_proposal WHERE id = ?", p1.ID.Int64()) })
	got, err := pr.Get(ctx, p1.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != 7 {
		t.Errorf("落库 UserID 应为 7, got %d", got.UserID)
	}
}
