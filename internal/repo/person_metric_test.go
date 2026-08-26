package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
)

// cleanupMetrics 注册按 person_id 删除的清理钩子。各用例用独占（ids.New 生成、每次运行唯一）
// 的 person_id，因此按 person_id 删只会清掉本用例的行，天然隔离、互不干扰。
func cleanupMetrics(t *testing.T, r *PersonMetricRepo, pid ids.ID) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = r.DB.ExecContext(context.Background(),
			`DELETE FROM person_metric WHERE person_id = ?`, pid.Int64())
	})
}

// TestPersonMetricCreateGet 建两条形态不同的测点（数值型 + 文本型），验证 Get 往返：
// 指针字段的「空/非空」都要原样还原，且 CreateExt 的零值兜底正确。
func TestPersonMetricCreateGet(t *testing.T) {
	db := testDB(t)
	r := &PersonMetricRepo{DB: db}
	ctx := context.Background()
	pid := ids.New() // 本用例独占的主体
	cleanupMetrics(t, r, pid)

	sess := ids.New()
	t1 := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)

	// m1：数值型测点——体重 70.5kg，value_text 为空（NULL）。显式给 confidence 验证往返。
	m1 := &PersonMetric{
		PersonID:   pid,
		MetricKey:  "weight",
		ValueNum:   fp(70.5),
		Unit:       strp("kg"),
		MeasuredAt: t1,
		Confidence: 0.9,
		SessionID:  idPtr(sess),
		Note:       strp("晨起空腹"),
	}
	if err := r.CreateExt(ctx, db, m1); err != nil {
		t.Fatal(err)
	}

	// m2：文本型测点——饮食='火锅'，value_num / unit 均为空（NULL）。
	t2 := time.Date(2026, 8, 1, 19, 0, 0, 0, time.UTC)
	m2 := &PersonMetric{
		PersonID:   pid,
		MetricKey:  "diet",
		ValueText:  strp("火锅"),
		MeasuredAt: t2,
	}
	if err := r.CreateExt(ctx, db, m2); err != nil {
		t.Fatal(err)
	}

	// CreateExt 零值兜底：m2 未显式给这些字段，应被兜底为默认值。
	if m2.UserID != 1 || m2.Status != "active" || m2.Source != "manual" ||
		m2.EpistemicType != "observed" || m2.Version != 1 {
		t.Fatalf("CreateExt 零值兜底异常: %+v", m2)
	}
	// TranscriptSegmentIDs nil 应被兜底为空数组（非 nil），而非留 nil。
	if m2.TranscriptSegmentIDs == nil {
		t.Fatal("TranscriptSegmentIDs 应兜底为空数组 ids.List{}，实际为 nil")
	}

	// 回读 m1：数值/单位非空须原样还原，value_text 须为 nil（NULL 往返）。
	g1, err := r.Get(ctx, m1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g1 == nil {
		t.Fatal("Get(m1) 返回 nil")
	}
	if g1.MetricKey != "weight" {
		t.Fatalf("metric_key 回读异常: %s", g1.MetricKey)
	}
	if g1.ValueNum == nil || *g1.ValueNum != 70.5 {
		t.Fatalf("value_num 回读异常: %v", g1.ValueNum)
	}
	if g1.Unit == nil || *g1.Unit != "kg" {
		t.Fatalf("unit 回读异常: %v", g1.Unit)
	}
	if g1.ValueText != nil {
		t.Fatalf("value_text 应为 NULL(nil)，实际 %v", *g1.ValueText)
	}
	if !g1.MeasuredAt.Equal(t1) {
		t.Fatalf("measured_at 回读异常: %v (期望 %v)", g1.MeasuredAt, t1)
	}
	if g1.Confidence != 0.9 {
		t.Fatalf("confidence 回读异常: %v", g1.Confidence)
	}
	if g1.Note == nil || *g1.Note != "晨起空腹" {
		t.Fatalf("note 回读异常: %v", g1.Note)
	}
	if g1.SessionID == nil || *g1.SessionID != sess {
		t.Fatalf("session_id 回读异常: %v", g1.SessionID)
	}

	// 回读 m2：文本非空须原样还原，value_num / unit 须为 nil（NULL 往返）。
	g2, err := r.Get(ctx, m2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g2 == nil {
		t.Fatal("Get(m2) 返回 nil")
	}
	if g2.ValueText == nil || *g2.ValueText != "火锅" {
		t.Fatalf("value_text 回读异常: %v", g2.ValueText)
	}
	if g2.ValueNum != nil {
		t.Fatalf("value_num 应为 NULL(nil)，实际 %v", *g2.ValueNum)
	}
	if g2.Unit != nil {
		t.Fatalf("unit 应为 NULL(nil)，实际 %v", *g2.Unit)
	}

	// Get 未命中：不存在的 id 返回 (nil, nil)，不报 sql.ErrNoRows。
	if g, err := r.Get(ctx, ids.New()); g != nil || err != nil {
		t.Fatalf("Get(不存在 id) 应返回 (nil, nil): g=%+v err=%v", g, err)
	}
}

// TestPersonMetricAppendOnly 锁定 append-only：同一 person + metric_key 的两个不同
// measured_at 各插一行，ListByPerson 两条都在（测点序列不塌缩成一条）。
func TestPersonMetricAppendOnly(t *testing.T) {
	db := testDB(t)
	r := &PersonMetricRepo{DB: db}
	ctx := context.Background()
	pid := ids.New()
	cleanupMetrics(t, r, pid)

	tEarly := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	m1 := &PersonMetric{PersonID: pid, MetricKey: "weight", ValueNum: fp(70.0), Unit: strp("kg"), MeasuredAt: tEarly}
	m2 := &PersonMetric{PersonID: pid, MetricKey: "weight", ValueNum: fp(69.5), Unit: strp("kg"), MeasuredAt: tLate}
	if err := r.CreateExt(ctx, db, m1); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateExt(ctx, db, m2); err != nil {
		t.Fatal(err)
	}
	// 两条 id 必须不同（各自新行），append-only 的前提。
	if m1.ID == m2.ID {
		t.Fatalf("两次 CreateExt 应生成不同 id: %v", m1.ID)
	}

	rows, err := r.ListByPerson(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("append-only：同 person+metric_key 两个测点应各占一行，实际 %d 行", len(rows))
	}
	// 组内按 measured_at 升序：早的在前。
	if !rows[0].MeasuredAt.Equal(tEarly) || !rows[1].MeasuredAt.Equal(tLate) {
		t.Fatalf("ListByPerson 应按 measured_at 升序: [0]=%v [1]=%v", rows[0].MeasuredAt, rows[1].MeasuredAt)
	}
}

// TestPersonMetricFindByPoint 验证自然键去重（含时间 + 值）与 <=> 的 NULL 安全比较：
//   - 完全同点命中；值不同 / 时间不同不命中；
//   - value_text-only 点（value_num=NULL）用 nil 参数经 <=> 命中，坐实 NULL 安全等号；
//   - dismissed 行被 status!='dismissed' 排除。
func TestPersonMetricFindByPoint(t *testing.T) {
	db := testDB(t)
	r := &PersonMetricRepo{DB: db}
	ctx := context.Background()
	pid := ids.New()
	cleanupMetrics(t, r, pid)

	t1 := time.Date(2026, 8, 1, 7, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 8, 2, 7, 0, 0, 0, time.UTC)

	// 数值型测点：weight@t1 = 70。
	w := &PersonMetric{PersonID: pid, MetricKey: "weight", ValueNum: fp(70), MeasuredAt: t1}
	if err := r.CreateExt(ctx, db, w); err != nil {
		t.Fatal(err)
	}

	// 完全同点（同 key/时间/数值，value_text 两侧均 NULL）→ 命中。
	got, err := r.FindByPointExt(ctx, db, pid, "weight", t1, fp(70), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != w.ID {
		t.Fatalf("FindByPointExt 完全同点应命中 w: %+v", got)
	}
	// 数值不同（71 vs 70）→ 不命中。
	if miss, err := r.FindByPointExt(ctx, db, pid, "weight", t1, fp(71), nil); err != nil || miss != nil {
		t.Fatalf("value_num 不同应不命中: miss=%+v err=%v", miss, err)
	}
	// measured_at 不同 → 不命中。
	if miss, err := r.FindByPointExt(ctx, db, pid, "weight", t2, fp(70), nil); err != nil || miss != nil {
		t.Fatalf("measured_at 不同应不命中: miss=%+v err=%v", miss, err)
	}
	// 传 valueNum=nil 去比一个 value_num 非空的行 → value_num<=>NULL 为不等，不命中。
	if miss, err := r.FindByPointExt(ctx, db, pid, "weight", t1, nil, nil); err != nil || miss != nil {
		t.Fatalf("valueNum=nil 比非空行应不命中(<=> 一侧 NULL): miss=%+v err=%v", miss, err)
	}

	// 文本型测点：diet@t1，value_num=NULL，value_text='火锅'。验证 NULL 安全命中路径。
	d := &PersonMetric{PersonID: pid, MetricKey: "diet", ValueText: strp("火锅"), MeasuredAt: t1}
	if err := r.CreateExt(ctx, db, d); err != nil {
		t.Fatal(err)
	}
	// valueNum=nil（<=> NULL 命中）+ valueText='火锅'（<=> 命中）→ 命中 d，坐实 <=> NULL 安全。
	got2, err := r.FindByPointExt(ctx, db, pid, "diet", t1, nil, strp("火锅"))
	if err != nil {
		t.Fatal(err)
	}
	if got2 == nil || got2.ID != d.ID {
		t.Fatalf("value_text-only 点应经 <=> NULL 安全比较命中 d: %+v", got2)
	}
	// value_text 不同 → 不命中。
	if miss, err := r.FindByPointExt(ctx, db, pid, "diet", t1, nil, strp("烧烤")); err != nil || miss != nil {
		t.Fatalf("value_text 不同应不命中: miss=%+v err=%v", miss, err)
	}

	// dismissed 排除：把 w 置 dismissed 后，同点查询应不再命中（可作为新候选重新提出）。
	if err := r.SetStatus(ctx, w.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if miss, err := r.FindByPointExt(ctx, db, pid, "weight", t1, fp(70), nil); err != nil || miss != nil {
		t.Fatalf("dismissed 行应被 status!='dismissed' 排除: miss=%+v err=%v", miss, err)
	}
}
