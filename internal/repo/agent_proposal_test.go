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
