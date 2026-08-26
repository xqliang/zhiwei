package profile

import (
	"context"
	"errors"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestProfileUserIsolation 锁定多用户阶段 2A：画像手动写/抽取按登录用户隔离 + 堵子表 IDOR。
//
//	① IDOR：u2 对 u1 的 person 加属性 → ErrNotFound（不写入、不泄漏存在性）；
//	② 写行归属：u1 加测点 → row.UserID == u1；
//	③ 抽取归属：ApplyFacts(userID=u2) 的 self 主体解析到 u2 的 owner（非 u1），写入行 UserID==u2。
//
// 本包共用 zhiwei_agentchat_test 库、串行跑（-p 1）：newTestService 已 bootstrap u1 owner；
// 这里再确保 u2 owner。收尾删掉 u2 的全部画像行 + 本用例在 u1 侧新建的独立 person 及其派生行，
// 恢复干净基线（模式参照 service_test.go / metric_service_test.go）。
func TestProfileUserIsolation(t *testing.T) {
	svc := newTestService(t) // bootstrap u1（user_id=1）owner
	ctx := context.Background()
	const u1 = int64(1)
	const u2 = int64(2)

	if err := repo.EnsureOwnerForUser(ctx, svc.Persons, u2); err != nil {
		t.Fatal(err)
	}
	ownerU1, err := svc.Persons.GetOwner(ctx, u1)
	if err != nil || ownerU1 == nil {
		t.Fatalf("u1 owner 缺失: %v %v", ownerU1, err)
	}
	ownerU2, err := svc.Persons.GetOwner(ctx, u2)
	if err != nil || ownerU2 == nil {
		t.Fatalf("u2 owner 缺失: %v %v", ownerU2, err)
	}
	if ownerU1.ID == ownerU2.ID {
		t.Fatalf("两用户 owner 应各自独立: u1=%d u2=%d", ownerU1.ID, ownerU2.ID)
	}

	// u1 侧独立 person（承载 ② 的测点，避免与 owner-metric 断言的其它用例串扰）
	p1, err := svc.ManualCreatePerson(ctx, u1, "ISO_u1_person", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cctx := context.Background()
		// u1 侧：删本用例建的独立 person 的派生行 + person 自身
		p1PK := p1.ID.Int64()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_metric WHERE person_id = ?`, p1PK)
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ?`, p1PK)
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person WHERE id = ?`, p1PK)
		// u2 侧：删该用户 owner 的派生画像行（本用例经抽取写入），再删 user_id=2 的 person
		if o, err := svc.Persons.GetOwner(cctx, u2); err == nil && o != nil {
			oPK := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ?`, oPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ?`, oPK)
		}
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person WHERE user_id = 2`)
	})

	// ① IDOR：u2 越权对 u1 的 owner 加属性 → ErrNotFound，且不写入 u1。
	const idorKey = "occupation"
	const idorVal = "ISO_间谍值"
	if _, err := svc.ManualAddAttribute(ctx, u2, ownerU1.ID, idorKey, idorVal); !errors.Is(err, ErrNotFound) {
		t.Fatalf("u2 越权对 u1 person 加属性应 ErrNotFound, got %v", err)
	}
	if a, _ := svc.Attributes.FindActiveByKey(ctx, ownerU1.ID, idorKey); a != nil && a.ValueText == idorVal {
		t.Fatalf("越权不应写入 u1 的属性: %+v", a)
	}

	// ② 写行归属：u1 给自己的 person 加测点 → row.UserID == u1。
	measuredAt := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	m, err := svc.ManualAddMetric(ctx, u1, p1.ID, "weight", fp(70), "", "kg", measuredAt)
	if err != nil {
		t.Fatal(err)
	}
	if m.UserID != u1 {
		t.Fatalf("手动测点 row.UserID 应为 u1(%d)，实得 %d", u1, m.UserID)
	}

	// ③ 抽取归属：ApplyFacts(userID=u2) 的 self 主体应解析到 u2 的 owner（非 u1）。
	const extractVal = "ISO_u2_专属_职业"
	sess := ids.New()
	st, err := svc.ApplyFacts(ctx, sess, u2, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: extractVal, Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != 1 {
		t.Fatalf("高置信 self 属性应 active: %+v", st)
	}
	// u2 owner 拿到该属性，且行 UserID==u2、PersonID==u2 owner。
	got, _ := svc.Attributes.FindActiveByKey(ctx, ownerU2.ID, "occupation")
	if got == nil || got.ValueText != extractVal {
		t.Fatalf("self 事实应落到 u2 owner: %+v", got)
	}
	if got.UserID != u2 || got.PersonID != ownerU2.ID {
		t.Fatalf("抽取写入行归属错误: UserID=%d PersonID=%d（期望 u2=%d owner=%d）",
			got.UserID, got.PersonID, u2, ownerU2.ID)
	}
	// u1 owner 不应拿到 u2 的抽取属性。
	if a, _ := svc.Attributes.FindActiveByKey(ctx, ownerU1.ID, "occupation"); a != nil && a.ValueText == extractVal {
		t.Fatalf("u2 的 self 事实不应落到 u1 owner: %+v", a)
	}
}
