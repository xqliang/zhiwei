package profile

import (
	"context"
	"strings"
	"testing"
)

// TestManualSetPersonStatusCascade 覆盖 F5（spec §13）人物归档级联六平面：
// 造一个独立人物 + 其六平面各一行（属性/关系/大事记/指标/周期/活动，其中指标行故意置 pending
// 以验证 active 与 pending 两种活跃态都被级联）→ ManualSetPersonStatus dismissed → 断言六行全部
// 级联 dismissed，且 person change_log 有一条带各平面级联计数的汇总审计（Note）；再把人物流转回
// active → 六行仍 dismissed（不自动回滚——恢复语义留跟进，见 service_manual.go 的取舍注释）。
//
// 用独立测试人物（非共享 owner），避免污染其它用例对 owner 平面的断言。跨包非自隔离：
// t.Cleanup 删掉该人物 + 其六平面行 + 审计行，恢复干净基线（模式参照 confirm_test.go）。
func TestManualSetPersonStatusCascade(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	p, err := svc.ManualCreatePerson(ctx, "归档级联测试", nil, nil)
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
	evt, err := svc.ManualAddEvent(ctx, pid, "里程碑", "归档级联测试-升职", "", "2026-01-15", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	met, err := svc.ManualAddMetric(ctx, pid, "weight", "70.5", "kg", "2026-08-20")
	if err != nil {
		t.Fatal(err)
	}
	cyc, err := svc.ManualAddCycle(ctx, pid, "medication", "归档级联测试-降压药", "2026-08-01", "", "", 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	act, err := svc.ManualAddActivity(ctx, pid, "归档级联测试-写代码", "电脑", "", "", "2026-08-20", 0)
	if err != nil {
		t.Fatal(err)
	}

	// 把指标行手动置 pending：验证级联对 active 与 pending 两种活跃态都生效（IN 子句），
	// 且计数仍为 1 行（改状态不新增行）。
	if err := svc.Metrics.SetStatus(ctx, met.ID, "pending"); err != nil {
		t.Fatal(err)
	}

	// ---- 归档人物 → 触发六平面级联 ----
	if err := svc.ManualSetPersonStatus(ctx, pid, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if b, _ := svc.Persons.Get(ctx, pid); b == nil || b.Status != "dismissed" {
		t.Fatalf("归档后人物应 dismissed: %+v", b)
	}

	// 六平面行全部级联为 dismissed
	if a, _ := svc.Attributes.Get(ctx, attr.ID); a == nil || a.Status != "dismissed" {
		t.Fatalf("属性应级联 dismissed: %+v", a)
	}
	if r, _ := svc.Relationships.Get(ctx, rel.ID); r == nil || r.Status != "dismissed" {
		t.Fatalf("关系应级联 dismissed: %+v", r)
	}
	if e, _ := svc.Events.Get(ctx, evt.ID); e == nil || e.Status != "dismissed" {
		t.Fatalf("大事记应级联 dismissed: %+v", e)
	}
	if m, _ := svc.Metrics.Get(ctx, met.ID); m == nil || m.Status != "dismissed" {
		t.Fatalf("指标（pending）应级联 dismissed: %+v", m)
	}
	if c, _ := svc.Cycles.Get(ctx, cyc.ID); c == nil || c.Status != "dismissed" {
		t.Fatalf("周期应级联 dismissed: %+v", c)
	}
	if ac, _ := svc.Activities.Get(ctx, act.ID); ac == nil || ac.Status != "dismissed" {
		t.Fatalf("活动应级联 dismissed: %+v", ac)
	}

	// person change_log 应有一条带级联计数的汇总审计（Note）。
	logs, err := svc.ChangeLogs.ListByPerson(ctx, pid, "person", "")
	if err != nil {
		t.Fatal(err)
	}
	var cascade *string
	for i := range logs {
		if logs[i].Note != nil && strings.Contains(*logs[i].Note, "人物归档：级联 dismissed") {
			cascade = logs[i].Note
		}
	}
	if cascade == nil {
		t.Fatalf("应有一条归档级联汇总审计: %+v", logs)
	}
	// 六平面各 1 行 → 汇总计数串应含各平面「X 1」。
	for _, want := range []string{"属性 1", "关系 1", "大事记 1", "指标 1", "周期 1", "活动 1"} {
		if !strings.Contains(*cascade, want) {
			t.Fatalf("级联审计计数缺「%s」: %q", want, *cascade)
		}
	}

	// ---- 恢复人物 active → 六平面行仍 dismissed（不自动回滚，见 service_manual.go 取舍注释）----
	if err := svc.ManualSetPersonStatus(ctx, pid, "active"); err != nil {
		t.Fatal(err)
	}
	if b, _ := svc.Persons.Get(ctx, pid); b == nil || b.Status != "active" {
		t.Fatalf("恢复后人物应 active: %+v", b)
	}
	if a, _ := svc.Attributes.Get(ctx, attr.ID); a == nil || a.Status != "dismissed" {
		t.Fatalf("恢复人物后属性仍应 dismissed（不回滚）: %+v", a)
	}
	if m, _ := svc.Metrics.Get(ctx, met.ID); m == nil || m.Status != "dismissed" {
		t.Fatalf("恢复人物后指标仍应 dismissed（不回滚）: %+v", m)
	}

	// 恢复流转（非 dismissed）不应再写级联审计——change_log 里带「人物归档：级联」的仍只有 1 条。
	logs2, err := svc.ChangeLogs.ListByPerson(ctx, pid, "person", "")
	if err != nil {
		t.Fatal(err)
	}
	cascadeCount := 0
	for i := range logs2 {
		if logs2[i].Note != nil && strings.Contains(*logs2[i].Note, "人物归档：级联") {
			cascadeCount++
		}
	}
	if cascadeCount != 1 {
		t.Fatalf("级联汇总审计应恰好 1 条（恢复流转不级联/不重复写），实得 %d: %+v", cascadeCount, logs2)
	}
}
