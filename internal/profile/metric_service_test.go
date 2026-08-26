package profile

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// cleanupOwnerMetrics 收尾删掉 owner 的 person_metric 行 + metric 审计条目，恢复干净基线。
// 本包共用同一 zhiwei_agentchat_test 库、串行跑；metric 各用例都写 owner（user_id=1），
// 用此清理避免污染彼此与后续 -count=1 重跑（模式参照 service_test.go / confirm_test.go）。
func cleanupOwnerMetrics(t *testing.T, svc *Service) {
	t.Helper()
	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			ownerPK := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_metric WHERE person_id = ?`, ownerPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'metric'`, ownerPK)
		}
	})
}

// TestApplyMetricFactsRerunIdempotent 锁定评审 C1：measured_at 缺省时回退「本 session 的
// created_at」（稳定），故【不注入固定时钟】下同 session 重跑抽取也幂等——不重复插测点。
// 回归此前缺陷：回退用 time.Now()，重跑时 measured_at 变化 → FindByPointExt 不命中 → 重复插。
func TestApplyMetricFactsRerunIdempotent(t *testing.T) {
	svc := newTestService(t) // Sessions 已装配
	ctx := context.Background()
	oid := ownerID(t, svc)
	cleanupOwnerMetrics(t, svc)
	// 关键：不注入 svc.Now（走生产墙钟路径），靠 session.created_at 稳定回退保证幂等。

	sess := &repo.AudioSession{ID: ids.New(), Source: "upload", Filename: "c1.wav",
		StoragePath: "/tmp/c1.wav", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = svc.DB.ExecContext(ctx, `DELETE FROM audio_session WHERE id=?`, sess.ID.Int64()) })

	// 缺 measured_at 的指标（口语「今天精力充沛」这类占多数）→ 回退 session.created_at。
	facts := []Fact{
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "mood_energy", ValueNum: fp(0.7),
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
	}
	if _, err := svc.ApplyFacts(ctx, sess.ID, 1, facts); err != nil {
		t.Fatal(err)
	}
	// 同 session 重跑（模拟「历史回填」/ stage 重试）：应幂等跳过，不重复插。
	st2, err := svc.ApplyFacts(ctx, sess.ID, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != 1 || st2.Active != 0 {
		t.Fatalf("重跑应幂等跳过(不重复插): %+v", st2)
	}
	rows, err := svc.Metrics.ListByPerson(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.MetricKey == "mood_energy" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("重跑不应重复插 mood_energy，期望 1 得 %d", n)
	}
}

// TestApplyMetricFacts 锁定 metric 抽取落库的 6 条硬约束：
//   - append-only（同 key 不同 measured_at 各占一行，绝不 supersede）；
//   - 恒 create、命中完全同点幂等跳过（自然键含 measured_at + 值）；
//   - measured_at 保留时刻、缺省回退 s.now()（非零）；
//   - confidence 存抽取确定性、与 value 主载荷分离；
//   - 高 conf→active、低 conf→pending。
func TestApplyMetricFacts(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)
	cleanupOwnerMetrics(t, svc)

	// 注入固定「当前时间」：让缺省 measured_at 的回退可预测，从而幂等重跑也命中同点。
	fixedNow := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return fixedNow }

	sess := ids.New()
	facts := []Fact{
		// ① 体重测点@08-01 09:00 = 70kg，高置信 → active
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "weight", ValueNum: fp(70),
			Unit: "kg", MeasuredAt: "2026-08-01 09:00", Confidence: 0.9, EpistemicType: "observed",
			SegmentIDs: []ids.ID{1}},
		// ② 体重测点@08-02 09:00 = 69.5kg，高置信 → active（不同 measured_at → append，不塌缩）
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "weight", ValueNum: fp(69.5),
			Unit: "kg", MeasuredAt: "2026-08-02 09:00", Confidence: 0.9, EpistemicType: "observed",
			SegmentIDs: []ids.ID{1}},
		// ③ 饮食测点@08-01 19:00 = 火锅（类别型），低置信 → pending
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "diet", MetricValueText: "火锅",
			MeasuredAt: "2026-08-01 19:00", Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ④ 精力测点（缺 measured_at）= 0.8，高置信 → active；measured_at 应回退 fixedNow（非零）
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "mood_energy", ValueNum: fp(0.8),
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
	}

	st, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != 3 || st.Pending != 1 || st.Skipped != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	rows, err := svc.Metrics.ListByPerson(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("应 4 个测点各占一行: %d", len(rows))
	}

	// 收集分组，逐条校验形态。
	var weights []float64
	var dietStatus, moodStatus string
	var moodMeasuredAt time.Time
	var sawDietText string
	var weightConf float64
	var weightSource string
	for _, r := range rows {
		switch r.MetricKey {
		case "weight":
			if r.ValueNum == nil || r.Unit == nil || *r.Unit != "kg" {
				t.Fatalf("weight 行形态错误: %+v", r)
			}
			weights = append(weights, *r.ValueNum)
			weightConf = r.Confidence
			weightSource = r.Source
		case "diet":
			dietStatus = r.Status
			// 硬约束 6/类别型：value_num 必须为 NULL、value_text 承载读数
			if r.ValueNum != nil {
				t.Fatalf("diet 类别测点 value_num 应为 NULL: %+v", r.ValueNum)
			}
			if r.ValueText != nil {
				sawDietText = *r.ValueText
			}
		case "mood_energy":
			moodStatus = r.Status
			moodMeasuredAt = r.MeasuredAt
		}
	}
	// append-only：两条 weight 都在（70 与 69.5），不塌缩成一条
	if len(weights) != 2 {
		t.Fatalf("append-only：weight 应 2 行，实得 %d (%v)", len(weights), weights)
	}
	// 硬约束 5：confidence 存抽取确定性(0.9)、与 value 主载荷分离；source=extract
	if weightConf != 0.9 {
		t.Fatalf("weight confidence 应为 0.9（抽取确定性，与 value 分离）: %v", weightConf)
	}
	if weightSource != "extract" {
		t.Fatalf("抽取路径 source 应为 extract: %s", weightSource)
	}
	if dietStatus != "pending" {
		t.Fatalf("低置信 diet 应 pending: %s", dietStatus)
	}
	if sawDietText != "火锅" {
		t.Fatalf("diet value_text 应为火锅: %q", sawDietText)
	}
	if moodStatus != "active" {
		t.Fatalf("高置信 mood_energy 应 active: %s", moodStatus)
	}
	// 硬约束 4：缺省 measured_at 回退 fixedNow（非零）
	if moodMeasuredAt.IsZero() || !moodMeasuredAt.Equal(fixedNow) {
		t.Fatalf("mood_energy measured_at 应回退 fixedNow(%v)，实得 %v", fixedNow, moodMeasuredAt)
	}

	// 审计：4 条 create（entity_kind=metric）
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "metric", "")
	if len(logs) < 4 {
		t.Fatalf("metric 审计不足: %d", len(logs))
	}

	// 幂等（硬约束 3）：同 session 重跑全部命中同点 → skip，不新增行（含缺省 measured_at 的 ④，
	// 因 s.now() 固定为 fixedNow，回退仍命中）。
	st2, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != 4 || st2.Active != 0 || st2.Pending != 0 {
		t.Fatalf("重跑应全部 skip: %+v", st2)
	}
	if rows2, _ := svc.Metrics.ListByPerson(ctx, oid); len(rows2) != 4 {
		t.Fatalf("幂等重跑不应加行: %d", len(rows2))
	}

	// append-only 再证（硬约束 1）：新 measured_at 的第 3 个体重测点入库，前两条 weight 仍 active，
	// 绝不被 supersede。
	sess2 := ids.New()
	st3, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "weight", ValueNum: fp(69.0),
			Unit: "kg", MeasuredAt: "2026-08-03 09:00", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3.Active != 1 {
		t.Fatalf("第三个体重测点应 active: %+v", st3)
	}
	var activeWeights int
	if err := svc.DB.GetContext(ctx, &activeWeights,
		`SELECT COUNT(*) FROM person_metric WHERE person_id = ? AND metric_key = 'weight' AND status = 'active'`,
		oid.Int64()); err != nil {
		t.Fatal(err)
	}
	if activeWeights != 3 {
		t.Fatalf("append-only：三个体重测点应全 active（无 supersede），实得 %d", activeWeights)
	}
}

// TestManualMetric 锁定手动 CRUD：Ext 事务版加点 active/manual conf=1.0 + 审计；
// 非法 key / Numeric 缺值 / 类别缺值 / measured_at 零值 → err；ManualAddMetric 包装 + 删。
func TestManualMetric(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)
	cleanupOwnerMetrics(t, svc)

	measuredAt := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	// ① Ext 事务版：数值型 weight，unit 传空 → 回退目录单位 kg。
	tx, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	m1, err := svc.ManualAddMetricExt(ctx, tx, oid, "weight", fp(72.5), "", "", measuredAt)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("ManualAddMetricExt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if m1.Status != "active" || m1.Source != "manual" || m1.Confidence != 1.0 || m1.EpistemicType != "observed" {
		t.Fatalf("手动测点横切字段异常: %+v", m1)
	}
	if m1.ValueNum == nil || *m1.ValueNum != 72.5 || m1.Unit == nil || *m1.Unit != "kg" {
		t.Fatalf("手动测点 value/unit 异常（unit 空应回退 kg）: %+v", m1)
	}
	if m1.MeasuredAt.IsZero() {
		t.Fatalf("measured_at 不应为零")
	}
	// Commit 后确实落库
	if got, _ := svc.Metrics.Get(ctx, m1.ID); got == nil || got.ID != m1.ID {
		t.Fatalf("Commit 后应能查到测点")
	}
	// 审计：至少一条 metric create
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "metric", "")
	if len(logs) == 0 {
		t.Fatalf("应有 metric 审计条目")
	}

	// ② 校验错误（各用独立 tx，出错回滚）：非法 key / Numeric 缺 value_num / measured_at 零值 / 类别缺 value_text
	type badCase struct {
		name       string
		key        string
		valueNum   *float64
		valueText  string
		measuredAt time.Time
	}
	for _, bc := range []badCase{
		{"非法key", "bogus", fp(1), "", measuredAt},
		{"Numeric缺value_num", "weight", nil, "", measuredAt},
		{"measured_at零值", "weight", fp(70), "", time.Time{}},
		{"类别缺value_text", "diet", nil, "", measuredAt},
	} {
		txBad, err := svc.DB.BeginTxx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ManualAddMetricExt(ctx, txBad, oid, bc.key, bc.valueNum, bc.valueText, "", bc.measuredAt); err == nil {
			_ = txBad.Rollback()
			t.Fatalf("%s 应报错", bc.name)
		}
		_ = txBad.Rollback()
	}

	// ③ ManualAddMetric 自持事务包装：类别型 diet=火锅（valueNum=nil），active/manual。
	m2, err := svc.ManualAddMetric(ctx, oid, "diet", nil, "火锅", "", time.Date(2026, 8, 1, 19, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if m2.Status != "active" || m2.ValueText == nil || *m2.ValueText != "火锅" || m2.ValueNum != nil {
		t.Fatalf("手动类别测点异常: %+v", m2)
	}

	// ④ ManualDeleteMetric → dismissed + delete 审计
	if err := svc.ManualDeleteMetric(ctx, m1.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Metrics.Get(ctx, m1.ID); d == nil || d.Status != "dismissed" {
		t.Fatalf("删除应 dismissed: %+v", d)
	}
	// 删不存在的 → ErrNotFound
	if err := svc.ManualDeleteMetric(ctx, ids.New()); err == nil {
		t.Fatalf("删不存在测点应报错")
	}
}

// TestConfirmDismissMetric 锁定确认队列：pending metric → active（无 supersede 旧行）/ dismissed。
func TestConfirmDismissMetric(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cleanupOwnerMetrics(t, svc)

	// 造两条低置信 pending 测点（LLM 路径）。
	sess := ids.New()
	if _, err := svc.ApplyFacts(ctx, sess, 1, []Fact{
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "weight", ValueNum: fp(68),
			Unit: "kg", MeasuredAt: "2026-09-01 08:00", Confidence: 0.5, EpistemicType: "observed"},
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "weight", ValueNum: fp(67),
			Unit: "kg", MeasuredAt: "2026-09-02 08:00", Confidence: 0.5, EpistemicType: "observed"},
	}); err != nil {
		t.Fatal(err)
	}

	pend, _ := svc.Metrics.ListPending(ctx, 1)
	var id68, id67 ids.ID
	for _, m := range pend {
		if m.ValueNum == nil {
			continue
		}
		switch *m.ValueNum {
		case 68:
			id68 = m.ID
		case 67:
			id67 = m.ID
		}
	}
	if id68 == 0 || id67 == 0 {
		t.Fatalf("两条 pending 测点未生成: 68=%d 67=%d", id68, id67)
	}

	// 确认 68 → active
	if err := svc.ConfirmPending(ctx, "metric", id68); err != nil {
		t.Fatal(err)
	}
	if m, _ := svc.Metrics.Get(ctx, id68); m == nil || m.Status != "active" {
		t.Fatalf("确认后应 active: %+v", m)
	}
	// 放弃 67 → dismissed
	if err := svc.DismissPending(ctx, "metric", id67); err != nil {
		t.Fatal(err)
	}
	if m, _ := svc.Metrics.Get(ctx, id67); m == nil || m.Status != "dismissed" {
		t.Fatalf("放弃后应 dismissed: %+v", m)
	}

	// 非 pending 再确认 → 报错；不存在 id → 报错
	if err := svc.ConfirmPending(ctx, "metric", id68); err == nil {
		t.Fatalf("非 pending 再确认应报错")
	}
	if err := svc.ConfirmPending(ctx, "metric", ids.New()); err == nil {
		t.Fatalf("不存在 id 应报错")
	}
}
