package profile

import (
	"context"
	"testing"
)

// 本文件锁定 ManualAdd*Ext 事务变体（设计 D1）：把外部 tx 传进去、Commit 后，
// 效果与自持事务的 ManualAddAttribute/ManualAddEvent 完全一致（active 行 + 审计 +
// 单值 supersede + 同值幂等 no-op）。这样 agent 提议确认闸门才能把画像写并进它的
// 单事务，实现 apply-once。原 ManualAddAttribute/Event 的回归由 confirm_test.go /
// service_test.go 覆盖（它们走委托版）。
//
// 共用 zhiwei 测试库、串行跑（-p 1）；用独占的 attr_key / event title + t.Cleanup
// 精确删除本用例产生的行，避免污染其它用例（模式参照 service_test.go）。

// TestManualAddAttributeExt：外部事务里加/改单值属性 → active + supersede；同值幂等 no-op。
func TestManualAddAttributeExt(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	const key = "ext_attr_测试" // 目录外 key → Def 默认 single/text，正好验证单值 supersede
	t.Cleanup(func() {
		cctx := context.Background()
		ownerPK := oid.Int64()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ? AND attr_key = ?`, ownerPK, key)
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND attr_key = ?`, ownerPK, key)
	})

	// ① 无现值 → active/manual/conf=1.0（与 ManualAddAttribute 一致）
	tx, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := svc.ManualAddAttributeExt(ctx, tx, oid, key, "v1EXT")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("ManualAddAttributeExt#1: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit#1: %v", err)
	}
	if a1.Status != "active" || a1.Source != "manual" || a1.Confidence != 1.0 || a1.SupersedesID != nil {
		t.Fatalf("首次加属性行异常: %+v", a1)
	}
	// Commit 后确实落库为当前 active 行
	if cur, _ := svc.Attributes.FindActiveByKey(ctx, oid, key); cur == nil || cur.ID != a1.ID {
		t.Fatalf("Commit 后 active 行应为 a1: %+v", cur)
	}
	// 审计：至少一条 create 条目
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "attribute", key)
	if len(logs) == 0 {
		t.Fatalf("应有 attribute 审计条目")
	}

	// ② 改值 → 旧行 superseded、新行 active 且 supersedes_id 指向旧行（与 ManualAddAttribute 一致）
	tx2, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := svc.ManualAddAttributeExt(ctx, tx2, oid, key, "v2EXT")
	if err != nil {
		_ = tx2.Rollback()
		t.Fatalf("ManualAddAttributeExt#2: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit#2: %v", err)
	}
	if a2.Status != "active" || a2.SupersedesID == nil || *a2.SupersedesID != a1.ID {
		t.Fatalf("改值应 supersede 旧行: %+v", a2)
	}
	if old, _ := svc.Attributes.Get(ctx, a1.ID); old == nil || old.Status != "superseded" {
		t.Fatalf("旧行应 superseded: %+v", old)
	}

	// ③ 同值幂等：再加 v2EXT → 返回现值行、不新增行（Ext 版走 return existing, nil 分支）
	tx3, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	a3, err := svc.ManualAddAttributeExt(ctx, tx3, oid, key, "v2EXT")
	if err != nil {
		_ = tx3.Rollback()
		t.Fatalf("ManualAddAttributeExt#3(no-op): %v", err)
	}
	if err := tx3.Commit(); err != nil { // 空事务 Commit 也应无副作用（no-op 未写任何行）
		t.Fatalf("commit#3: %v", err)
	}
	if a3.ID != a2.ID {
		t.Fatalf("同值幂等应返回现值行 a2: got %+v", a3)
	}
	var activeCnt int
	if err := svc.DB.GetContext(ctx, &activeCnt,
		`SELECT COUNT(*) FROM person_attribute WHERE person_id = ? AND attr_key = ? AND status = 'active'`,
		oid.Int64(), key); err != nil {
		t.Fatal(err)
	}
	if activeCnt != 1 {
		t.Fatalf("同值幂等不应新增 active 行, 期望 1 得 %d", activeCnt)
	}
}

// TestManualAddEventExt：外部事务里加大事记 → active/manual + occurred_at 解析；非法事件类型报错。
func TestManualAddEventExt(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	const title = "EXT测试大事记"
	t.Cleanup(func() {
		cctx := context.Background()
		ownerPK := oid.Int64()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_event WHERE person_id = ? AND title = ?`, ownerPK, title)
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'event'`, ownerPK)
	})

	tx, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	e1, err := svc.ManualAddEventExt(ctx, tx, oid, "健康", title, "长期服药", "2025-06-01", "", "北京协和", nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("ManualAddEventExt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if e1.Status != "active" || e1.Source != "manual" || e1.Confidence != 1.0 || e1.OccurredAt == nil {
		t.Fatalf("手动事件行异常: %+v", e1)
	}
	// Commit 后确实落库
	if got, _ := svc.Events.FindActiveByKeyExt(ctx, svc.DB, oid, "健康", title); got == nil || got.ID != e1.ID {
		t.Fatalf("Commit 后应能查到 active 事件: %+v", got)
	}

	// 非法事件类型：校验先行，直接报错（不写库）。用独立 tx，出错回滚。
	txBad, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ManualAddEventExt(ctx, txBad, oid, "不存在的类型", "X", "", "", "", "", nil); err == nil {
		_ = txBad.Rollback()
		t.Fatalf("非法事件类型应报错")
	}
	_ = txBad.Rollback()
}
