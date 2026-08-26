package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

// TestApplyCycleFacts 覆盖 cycle 平面抽取落库路径（合并对账补回：fork 重写 service_test.go 时丢了
// TestApplyCycleFacts）。验证：① 高置信 observed 新周期 → active；② 同 session 重跑 → 自然键 skip；
// ③ 跨 session 同 type+label 但参数变化 → 冲突 pending（supersedes 指向现值，绝不静默覆盖）。
func TestApplyCycleFacts(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	// 收尾清 owner 的 cycle 行 + cycle 审计，恢复干净基线（本包共用库、不逐个重置）。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_cycle WHERE person_id = ?`, oid.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'cycle'`, oid.Int64())
	})

	sess := ids.New()
	facts := []Fact{
		{Plane: "cycle", Subject: Subject{Kind: "self"}, CycleType: "medication", CycleLabel: "降压药",
			Dosage: "1片", FrequencyText: "每日一次", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != 1 || st.Pending != 0 {
		t.Fatalf("① 新周期应 active: %+v", st)
	}
	list, err := svc.Cycles.ListByPerson(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "active" || list[0].CycleType != "medication" ||
		list[0].Label == nil || *list[0].Label != "降压药" || list[0].Dosage == nil || *list[0].Dosage != "1片" {
		t.Fatalf("① 周期行错误: %+v", list)
	}

	// ② 同 session 重跑 → 自然键 skip（幂等）
	st2, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != 1 || st2.Active != 0 {
		t.Fatalf("② 同 session 重跑应 skip: %+v", st2)
	}

	// ③ 跨 session 同 type+label、剂量变化 → 冲突 pending（不覆盖现值）
	sess2 := ids.New()
	st3, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "cycle", Subject: Subject{Kind: "self"}, CycleType: "medication", CycleLabel: "降压药",
			Dosage: "2片", FrequencyText: "每日两次", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3.Pending != 1 || st3.Conflicts != 1 {
		t.Fatalf("③ 参数变化应冲突 pending: %+v", st3)
	}
	// 现值仍 active（未被静默覆盖），另有一条 pending 指向它
	pend, err := svc.Cycles.FindByNaturalKeyExt(ctx, svc.DB, sess2, oid, "medication", strPtr("降压药"))
	if err != nil {
		t.Fatal(err)
	}
	if pend == nil || pend.Status != "pending" || pend.SupersedesID == nil || *pend.SupersedesID != list[0].ID {
		t.Fatalf("③ 冲突 pending 应指向现值 active 行: pend=%+v 期望 supersedes=%d", pend, list[0].ID)
	}
}

// TestApplyActivityFacts 覆盖 activity 平面抽取落库（测点流语义：append-only、无冲突）。
// ① 高置信 observed 活动 → active；② 同 session 重跑 → 自然键 skip。
func TestApplyActivityFacts(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_activity WHERE person_id = ?`, oid.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'activity'`, oid.Int64())
	})

	sess := ids.New()
	facts := []Fact{
		{Plane: "activity", Subject: Subject{Kind: "self"}, ActivityText: "写代码", Tool: "电脑",
			StartedAt: "2026-07-20", DurationMin: 120, Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != 1 || st.Pending != 0 {
		t.Fatalf("① 新活动应 active: %+v", st)
	}
	list, err := svc.Activities.ListByPerson(ctx, oid, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "active" || list[0].Activity != "写代码" ||
		list[0].Tool == nil || *list[0].Tool != "电脑" || list[0].DurationMin == nil || *list[0].DurationMin != 120 {
		t.Fatalf("① 活动行错误: %+v", list)
	}

	// ② 同 session 重跑 → skip
	st2, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != 1 || st2.Active != 0 {
		t.Fatalf("② 同 session 重跑应 skip: %+v", st2)
	}
}

// TestManualAddEventMultiRelated 覆盖多人事件手动录入（合并对账补回：P2a② 多人 related_people；
// fork 重写 service_test.go 时丢了该覆盖）。两个关联人物 id 都落进 RelatedPersonIDs 数组。
func TestManualAddEventMultiRelated(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	p1, err := svc.ManualCreatePerson(ctx, 1, "张三", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := svc.ManualCreatePerson(ctx, 1, "李四", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person WHERE id IN (?, ?)`, p1.ID.Int64(), p2.ID.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_event WHERE person_id = ?`, oid.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'event'`, oid.Int64())
	})

	// importance=0.5 显式给值（>0 → clamp 采用，不走类型默认）；relatedPersonIDs 两人同行。
	e, err := svc.ManualAddEvent(ctx, 1, oid, "聚会", "同学聚会", "老同学重聚", "2026-01-01", "", "北京",
		[]ids.ID{p1.ID, p2.ID}, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != "active" || e.Importance != 0.5 {
		t.Fatalf("事件基本字段错误: %+v", e)
	}
	if len(e.RelatedPersonIDs) != 2 {
		t.Fatalf("多人事件应落 2 个关联人, got %d: %+v", len(e.RelatedPersonIDs), e.RelatedPersonIDs)
	}
	got := map[ids.ID]bool{}
	for _, rid := range e.RelatedPersonIDs {
		got[rid] = true
	}
	if !got[p1.ID] || !got[p2.ID] {
		t.Fatalf("关联人 id 不全: 期望 %d/%d, got %v", p1.ID, p2.ID, e.RelatedPersonIDs)
	}
	// 从库回读确认持久化（非仅内存返回值）
	reload, err := svc.Events.Get(ctx, e.ID)
	if err != nil || reload == nil {
		t.Fatalf("回读事件失败: %v %v", reload, err)
	}
	if len(reload.RelatedPersonIDs) != 2 {
		t.Fatalf("回读多人事件关联人应为 2: %+v", reload.RelatedPersonIDs)
	}
}
