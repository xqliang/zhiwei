package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestPersonCycleQueries(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	cycles := &PersonCycleRepo{DB: db}

	// 同 metric 测试：挂独立人物 + 收尾按 person_id 删净，避免 pending 行污染其他包 ListPending。
	p := &Person{DisplayName: "周期测试-甲"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM person_cycle WHERE person_id = ?`, p.ID.Int64())
	})
	sess := ids.New()

	// c1：生理期周期，label 为 NULL（无标签）；带 DATE 型锚点/预测日，校验 DATE 列往返。
	// 未显式给横切字段，用于零值兜底校验。
	anchor := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	next := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	c1 := &PersonCycle{
		PersonID:        p.ID,
		CycleType:       "menstrual",
		AnchorDate:      datep(anchor),
		PeriodDays:      intp(28),
		NextPredictedAt: datep(next),
		SessionID:       &sess,
	}
	if err := cycles.Create(ctx, c1); err != nil {
		t.Fatal(err)
	}
	// c2：服药周期，label="降压药"（非 NULL），带 dosage/frequency/duration。
	c2 := &PersonCycle{
		PersonID:      p.ID,
		CycleType:     "medication",
		Label:         strp("降压药"),
		Dosage:        strp("5mg"),
		FrequencyText: strp("每日一次"),
		DurationDays:  intp(30),
		SessionID:     &sess,
	}
	if err := cycles.Create(ctx, c2); err != nil {
		t.Fatal(err)
	}
	// c3：复诊周期，pending——供 ListPending / SetStatus / 「自然键任意 status 命中」用。
	c3 := &PersonCycle{
		PersonID:  p.ID,
		CycleType: "followup",
		Label:     strp("复诊"),
		Status:    "pending",
		SessionID: &sess,
	}
	if err := cycles.Create(ctx, c3); err != nil {
		t.Fatal(err)
	}

	// CreateExt 零值兜底校验：c1 未显式给这些字段，应被兜底为默认值。
	if c1.Confidence != 0.8 || c1.Version != 1 || c1.Source != "manual" ||
		c1.EpistemicType != "observed" || c1.Status != "active" || c1.UserID != 1 {
		t.Fatalf("CreateExt 零值兜底异常: %+v", c1)
	}

	// DATE 列往返 + NULL label 回读：c1 的 anchor/next 应原样还原（用 Equal 避免时区/位置差异），
	// period_days=28，label 为 NULL（*string 应为 nil）。
	gotC1 := mustGetCycle(t, cycles, c1.ID)
	if gotC1.AnchorDate == nil || !gotC1.AnchorDate.Equal(anchor) {
		t.Fatalf("anchor_date（DATE 列）回读异常: %v (期望 %v)", gotC1.AnchorDate, anchor)
	}
	if gotC1.NextPredictedAt == nil || !gotC1.NextPredictedAt.Equal(next) {
		t.Fatalf("next_predicted_at（DATE 列）回读异常: %v (期望 %v)", gotC1.NextPredictedAt, next)
	}
	if gotC1.PeriodDays == nil || *gotC1.PeriodDays != 28 {
		t.Fatalf("period_days 回读异常: %v", gotC1.PeriodDays)
	}
	if gotC1.Label != nil {
		t.Fatalf("c1 label 应为 NULL: %v", *gotC1.Label)
	}
	// c2 非 NULL 字段回读：label/dosage/frequency/duration。
	gotC2 := mustGetCycle(t, cycles, c2.ID)
	if gotC2.Label == nil || *gotC2.Label != "降压药" {
		t.Fatalf("c2 label 回读异常: %v", gotC2.Label)
	}
	if gotC2.Dosage == nil || *gotC2.Dosage != "5mg" || gotC2.FrequencyText == nil || *gotC2.FrequencyText != "每日一次" {
		t.Fatalf("c2 dosage/frequency 回读异常: %+v", gotC2)
	}

	// FindActiveByKeyExt：NULL label 匹配——传 nil 命中 label IS NULL 的 c1（生理期）。
	got, err := cycles.FindActiveByKeyExt(ctx, db, p.ID, "menstrual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != c1.ID {
		t.Fatalf("FindActiveByKeyExt(menstrual,nil) 未命中 NULL label 的 c1: %+v", got)
	}
	// 非 NULL label 匹配：传 "降压药" 命中 c2。
	got2, err := cycles.FindActiveByKeyExt(ctx, db, p.ID, "medication", strp("降压药"))
	if err != nil {
		t.Fatal(err)
	}
	if got2 == nil || got2.ID != c2.ID {
		t.Fatalf("FindActiveByKeyExt(medication,'降压药') 未命中 c2: %+v", got2)
	}
	// 未命中(标签值不同)：medication 但 label="阿司匹林" → nil。
	if miss, err := cycles.FindActiveByKeyExt(ctx, db, p.ID, "medication", strp("阿司匹林")); err != nil {
		t.Fatal(err)
	} else if miss != nil {
		t.Fatalf("FindActiveByKeyExt 应未命中(标签不同): %+v", miss)
	}
	// 未命中(<=> 严格区分 NULL 与非 NULL)：menstrual 的 active 行 label 为 NULL，
	// 传非 nil 标签不应命中它——证明 <=> 不是「忽略 label」而是精确区分 NULL/非 NULL。
	if miss, err := cycles.FindActiveByKeyExt(ctx, db, p.ID, "menstrual", strp("生理期")); err != nil {
		t.Fatal(err)
	} else if miss != nil {
		t.Fatalf("FindActiveByKeyExt(menstrual,非nil) 不应命中 NULL label 行: %+v", miss)
	}

	// FindByNaturalKeyExt 任意 status 命中：c3 是 pending，仍应被自然键命中（幂等去重不看 status）。
	nk, err := cycles.FindByNaturalKeyExt(ctx, db, sess, p.ID, "followup", strp("复诊"))
	if err != nil {
		t.Fatal(err)
	}
	if nk == nil || nk.ID != c3.ID {
		t.Fatalf("FindByNaturalKeyExt 未命中 pending 的 c3: %+v", nk)
	}
	// 自然键 NULL label 命中：传 nil 命中 c1。
	nk1, err := cycles.FindByNaturalKeyExt(ctx, db, sess, p.ID, "menstrual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if nk1 == nil || nk1.ID != c1.ID {
		t.Fatalf("FindByNaturalKeyExt(menstrual,nil) 未命中 c1: %+v", nk1)
	}

	// ListByPerson 全状态，按 cycle_type 升序：followup(c3) < medication(c2) < menstrual(c1)。
	rows, err := cycles.ListByPerson(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("ListByPerson 应 3 行(含 pending): %d", len(rows))
	}
	if rows[0].ID != c3.ID || rows[1].ID != c2.ID || rows[2].ID != c1.ID {
		t.Fatalf("ListByPerson 按 cycle_type 排序错误: %v %v %v", rows[0].CycleType, rows[1].CycleType, rows[2].CycleType)
	}

	// Get 未命中：不存在的 id 应返回 (nil, nil)。
	if g, err := cycles.Get(ctx, ids.New()); g != nil || err != nil {
		t.Fatalf("Get(不存在 id) 应返回 (nil, nil): g=%+v err=%v", g, err)
	}

	// ListPending 应包含 pending 的 c3。
	pend, err := cycles.ListPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range pend {
		if c.ID == c3.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("ListPending 未包含 pending 周期 c3")
	}

	// SetStatus c3 → dismissed，再 Get 校验已落库。
	if err := cycles.SetStatus(ctx, c3.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if g := mustGetCycle(t, cycles, c3.ID); g.Status != "dismissed" {
		t.Fatalf("SetStatus 未生效: %+v", g)
	}
}

// mustGetCycle 取行并断言存在（测试内联小工具）。
func mustGetCycle(t *testing.T, r *PersonCycleRepo, id ids.ID) *PersonCycle {
	t.Helper()
	c, err := r.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatalf("Get(%v) 应命中却为 nil", id)
	}
	return c
}

// intp/datep：*int 与 *time.Time 取址小工具（本包尚无，测试专用；strp/fp 已在别处定义故复用）。
func intp(i int) *int              { return &i }
func datep(t time.Time) *time.Time { return &t }
