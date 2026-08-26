package profile

import (
	"context"
	"strings"
	"testing"
)

// TestManualSetPersonStatusCascade 覆盖 F5（spec §13）人物删除级联六平面 + 恢复级联（000011 后）：
// 造一个独立人物 + 其六平面各一行（属性/关系/大事记/指标/周期/活动，其中指标行故意置 pending
// 以验证 active 与 pending 两种活跃态都被级联）；活动行在删除人物**之前**先手动删掉
// （pre_dismiss_status=NULL）以验证「手动删的行恢复时不被误恢复」。
// 流程：ManualSetPersonStatus dismissed → 断言存活行全部级联 dismissed 且记原状态（pre_dismiss_status），
// person change_log 有带各平面级联计数的汇总审计（Note）；再流转回 active → 级联行翻回**原状态**
// （attr→active、met→pending）且标记清空，手动删过的活动行保持 dismissed，并新增一条恢复级联汇总审计。
//
// 用独立测试人物（非共享 owner），避免污染其它用例对 owner 平面的断言。跨包非自隔离：
// t.Cleanup 删掉该人物 + 其六平面行 + 审计行，恢复干净基线（模式参照 confirm_test.go）。
func TestManualSetPersonStatusCascade(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	p, err := svc.ManualCreatePerson(ctx, "删除级联测试", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pid := p.ID

	// 提前用 t.Cleanup 注册，保证任一断言 t.Fatal 提前退出时也会清理。
	t.Cleanup(func() {
		cctx := context.Background()
		pk := pid.Int64()
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_attribute WHERE person_id = ?", pk)
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_relationship WHERE person_id = ?", pk)
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_event WHERE person_id = ?", pk)
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_metric WHERE person_id = ?", pk)
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_cycle WHERE person_id = ?", pk)
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_activity WHERE person_id = ?", pk)
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_change_log WHERE person_id = ?", pk)
		_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person WHERE id = ?", pk)
	})

	// 六平面各造一行（都挂到该测试人物；Manual* 路径 → active/manual/conf=1.0）。
	attr, err := svc.ManualAddAttribute(ctx, pid, "city", "北京")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := svc.ManualAddRelationship(ctx, pid, "朋友", nil, "", "", "老友")
	if err != nil {
		t.Fatal(err)
	}
	evt, err := svc.ManualAddEvent(ctx, pid, "里程碑", "删除级联测试-升职", "", "2026-01-15", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	met, err := svc.ManualAddMetric(ctx, pid, "weight", "70.5", "kg", "2026-08-20")
	if err != nil {
		t.Fatal(err)
	}
	cyc, err := svc.ManualAddCycle(ctx, pid, "medication", "删除级联测试-降压药", "2026-08-01", "", "", 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	act, err := svc.ManualAddActivity(ctx, pid, "删除级联测试-写代码", "电脑", "", "", "2026-08-20", 0)
	if err != nil {
		t.Fatal(err)
	}

	// 把指标行手动置 pending：验证级联对 active 与 pending 两种活跃态都生效（IN 子句），
	// 且恢复时该行翻回的是 pending（pre_dismiss_status 记的原状态），而非一律 active。
	if err := svc.Metrics.SetStatus(ctx, met.ID, "pending"); err != nil {
		t.Fatal(err)
	}

	// 删除人物**之前**先手动删掉活动行：手动删走 SetStatusExt（不写 pre_dismiss_status），
	// 恢复人物时它应保持 dismissed——这是「手动删 vs 级联删」区分标记的核心断言点。
	if err := svc.ManualDeleteActivity(ctx, act.ID); err != nil {
		t.Fatal(err)
	}
	if a, _ := svc.Activities.Get(ctx, act.ID); a == nil || a.Status != "dismissed" || a.PreDismissStatus != nil {
		t.Fatalf("手动删的活动应 dismissed 且无级联标记: %+v", a)
	}

	// ---- 删除人物 → 触发六平面级联（活动行已是终态，不在级联之列）----
	if err := svc.ManualSetPersonStatus(ctx, pid, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if b, _ := svc.Persons.Get(ctx, pid); b == nil || b.Status != "dismissed" {
		t.Fatalf("删除后人物应 dismissed: %+v", b)
	}

	// 存活五行全部级联为 dismissed，且 pre_dismiss_status 记下各自 dismiss 前的状态。
	if a, _ := svc.Attributes.Get(ctx, attr.ID); a == nil || a.Status != "dismissed" || a.PreDismissStatus == nil || *a.PreDismissStatus != "active" {
		t.Fatalf("属性应级联 dismissed 且标记原状态 active: %+v", a)
	}
	if r, _ := svc.Relationships.Get(ctx, rel.ID); r == nil || r.Status != "dismissed" || r.PreDismissStatus == nil || *r.PreDismissStatus != "active" {
		t.Fatalf("关系应级联 dismissed 且标记原状态 active: %+v", r)
	}
	if e, _ := svc.Events.Get(ctx, evt.ID); e == nil || e.Status != "dismissed" || e.PreDismissStatus == nil || *e.PreDismissStatus != "active" {
		t.Fatalf("大事记应级联 dismissed 且标记原状态 active: %+v", e)
	}
	if m, _ := svc.Metrics.Get(ctx, met.ID); m == nil || m.Status != "dismissed" || m.PreDismissStatus == nil || *m.PreDismissStatus != "pending" {
		t.Fatalf("指标（原 pending）应级联 dismissed 且标记原状态 pending: %+v", m)
	}
	if c, _ := svc.Cycles.Get(ctx, cyc.ID); c == nil || c.Status != "dismissed" || c.PreDismissStatus == nil || *c.PreDismissStatus != "active" {
		t.Fatalf("周期应级联 dismissed 且标记原状态 active: %+v", c)
	}
	if a, _ := svc.Activities.Get(ctx, act.ID); a == nil || a.Status != "dismissed" || a.PreDismissStatus != nil {
		t.Fatalf("手动删的活动行删除人物时不应被动（仍无标记）: %+v", a)
	}

	// person change_log 应有一条带级联计数的汇总审计（Note）。
	logs, err := svc.ChangeLogs.ListByPerson(ctx, pid, "person", "")
	if err != nil {
		t.Fatal(err)
	}
	var cascade *string
	for i := range logs {
		if logs[i].Note != nil && strings.Contains(*logs[i].Note, "人物删除：级联 dismissed") {
			cascade = logs[i].Note
		}
	}
	if cascade == nil {
		t.Fatalf("应有一条删除级联汇总审计: %+v", logs)
	}
	// 五个平面各 1 行、活动 0 行（删除前已手动删）→ 汇总计数串应含对应计数。
	for _, want := range []string{"属性 1", "关系 1", "大事记 1", "指标 1", "周期 1", "活动 0"} {
		if !strings.Contains(*cascade, want) {
			t.Fatalf("级联审计计数缺「%s」: %q", want, *cascade)
		}
	}

	// ---- 恢复人物 active → 级联行翻回原状态，手动删过的活动行保持 dismissed ----
	if err := svc.ManualSetPersonStatus(ctx, pid, "active"); err != nil {
		t.Fatal(err)
	}
	if b, _ := svc.Persons.Get(ctx, pid); b == nil || b.Status != "active" {
		t.Fatalf("恢复后人物应 active: %+v", b)
	}
	if a, _ := svc.Attributes.Get(ctx, attr.ID); a == nil || a.Status != "active" || a.PreDismissStatus != nil {
		t.Fatalf("恢复后属性应翻回 active 且标记清空: %+v", a)
	}
	if r, _ := svc.Relationships.Get(ctx, rel.ID); r == nil || r.Status != "active" || r.PreDismissStatus != nil {
		t.Fatalf("恢复后关系应翻回 active 且标记清空: %+v", r)
	}
	if e, _ := svc.Events.Get(ctx, evt.ID); e == nil || e.Status != "active" || e.PreDismissStatus != nil {
		t.Fatalf("恢复后大事记应翻回 active 且标记清空: %+v", e)
	}
	// 指标行删除前是 pending → 恢复到 pending（原状态），不是一律 active。
	if m, _ := svc.Metrics.Get(ctx, met.ID); m == nil || m.Status != "pending" || m.PreDismissStatus != nil {
		t.Fatalf("恢复后指标应翻回原状态 pending 且标记清空: %+v", m)
	}
	if c, _ := svc.Cycles.Get(ctx, cyc.ID); c == nil || c.Status != "active" || c.PreDismissStatus != nil {
		t.Fatalf("恢复后周期应翻回 active 且标记清空: %+v", c)
	}
	// 手动删过的活动行不被误恢复。
	if a, _ := svc.Activities.Get(ctx, act.ID); a == nil || a.Status != "dismissed" {
		t.Fatalf("恢复后手动删的活动行应保持 dismissed（不误恢复）: %+v", a)
	}

	// 恢复级联也应有一条带计数的汇总审计（五行恢复、活动不恢复）。
	logs2, err := svc.ChangeLogs.ListByPerson(ctx, pid, "person", "")
	if err != nil {
		t.Fatal(err)
	}
	var restore *string
	for i := range logs2 {
		if logs2[i].Note != nil && strings.Contains(*logs2[i].Note, "人物恢复：级联恢复") {
			restore = logs2[i].Note
		}
	}
	if restore == nil {
		t.Fatalf("应有一条恢复级联汇总审计: %+v", logs2)
	}
	for _, want := range []string{"属性 1", "关系 1", "大事记 1", "指标 1", "周期 1"} {
		if !strings.Contains(*restore, want) {
			t.Fatalf("恢复审计计数缺「%s」: %q", want, *restore)
		}
	}
	// 删除级联审计仍恰好 1 条（恢复流转写的是恢复审计，不重复写删除审计）。
	cascadeCount := 0
	for i := range logs2 {
		if logs2[i].Note != nil && strings.Contains(*logs2[i].Note, "人物删除：级联") {
			cascadeCount++
		}
	}
	if cascadeCount != 1 {
		t.Fatalf("删除级联汇总审计应恰好 1 条，实得 %d: %+v", cascadeCount, logs2)
	}

	// ---- 二次删除→恢复：验证标记清空后循环可用（pre_dismiss_status 不残留）----
	if err := svc.ManualSetPersonStatus(ctx, pid, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ManualSetPersonStatus(ctx, pid, "active"); err != nil {
		t.Fatal(err)
	}
	if a, _ := svc.Attributes.Get(ctx, attr.ID); a == nil || a.Status != "active" || a.PreDismissStatus != nil {
		t.Fatalf("二次删除→恢复后属性应回到 active 且标记清空: %+v", a)
	}
}

// TestManualSetPersonStatusReverseCascade 覆盖 F5 反向边补充（spec §13 / P6）：归档人物 A 时，
// 他人 C 指向 A 的 **pending** 反向关系边（related_person_id=A）应被级联 dismissed——清确认队列里
// 「对着一半已归档的人让用户确认关系」的孤儿噪声；而 C 指向 A 的 **active** 反向边刻意保留——那是
// C 的画像数据，归档 A 不替对端做主篡改（P5 决策，本任务不改）。同时校验汇总审计 Note 带
// 「反向 pending 关系边 N 条」计数。
//
// 造两个独立测试人物 A、C（非共享 owner），t.Cleanup 删两人的属性/关系行 + 审计 + person 行，
// 恢复干净基线（跨包非自隔离，模式参照 TestManualSetPersonStatusCascade）。
func TestManualSetPersonStatusReverseCascade(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	pa, err := svc.ManualCreatePerson(ctx, "反向级联-被归档A", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	aid := pa.ID
	pc, err := svc.ManualCreatePerson(ctx, "反向级联-对端C", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cid := pc.ID

	t.Cleanup(func() {
		cctx := context.Background()
		for _, pk := range []int64{aid.Int64(), cid.Int64()} {
			_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_attribute WHERE person_id = ?", pk)
			_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_relationship WHERE person_id = ?", pk)
			_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person_change_log WHERE person_id = ?", pk)
			_, _ = svc.DB.ExecContext(cctx, "DELETE FROM person WHERE id = ?", pk)
		}
	})

	// A 自身一条 active 属性——顺带验证正向级联仍生效（归档后应 dismissed）。
	attr, err := svc.ManualAddAttribute(ctx, aid, "city", "上海")
	if err != nil {
		t.Fatal(err)
	}

	// C→A 反向边两条（person_id=C、related_person_id=A）：一条 pending（应被级联清掉）、
	// 一条 active（应保留）。ManualAddRelationship 建 active/manual；pending 那条建完再 SetStatus
	// 翻成 pending。用不同 relation_type 让两条共存（自然键含类型）。
	relPending, err := svc.ManualAddRelationship(ctx, cid, "朋友", &aid, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Relationships.SetStatus(ctx, relPending.ID, "pending"); err != nil {
		t.Fatal(err)
	}
	relActive, err := svc.ManualAddRelationship(ctx, cid, "同事", &aid, "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// ---- 归档 A ----
	if err := svc.ManualSetPersonStatus(ctx, aid, "dismissed"); err != nil {
		t.Fatal(err)
	}

	// A 自身属性正向级联 dismissed。
	if a, _ := svc.Attributes.Get(ctx, attr.ID); a == nil || a.Status != "dismissed" {
		t.Fatalf("A 自身属性应正向级联 dismissed: %+v", a)
	}
	// C→A 的 pending 反向边被级联 dismissed。
	if r, _ := svc.Relationships.Get(ctx, relPending.ID); r == nil || r.Status != "dismissed" {
		t.Fatalf("pending 反向边应级联 dismissed: %+v", r)
	}
	// C→A 的 active 反向边保留不动（对端画像不篡改）。
	if r, _ := svc.Relationships.Get(ctx, relActive.ID); r == nil || r.Status != "active" {
		t.Fatalf("active 反向边应保留 active（归档不篡改对端）: %+v", r)
	}

	// 汇总审计 Note 应含反向 pending 计数（本例恰 1 条）。
	logs, err := svc.ChangeLogs.ListByPerson(ctx, aid, "person", "")
	if err != nil {
		t.Fatal(err)
	}
	var cascade *string
	for i := range logs {
		if logs[i].Note != nil && strings.Contains(*logs[i].Note, "人物删除：级联 dismissed") {
			cascade = logs[i].Note
		}
	}
	if cascade == nil {
		t.Fatalf("应有删除级联汇总审计: %+v", logs)
	}
	if !strings.Contains(*cascade, "反向 pending 关系边 1 条") {
		t.Fatalf("级联审计应含「反向 pending 关系边 1 条」: %q", *cascade)
	}
}
