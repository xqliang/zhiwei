package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestPersonMetricQueries(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	metrics := &PersonMetricRepo{DB: db}

	// 挂在独立人物下，避免污染其他包读到的公共数据；测点仍默认 user_id=1（owner），
	// 故收尾按 person_id 删净本用例建的 person_metric 行——否则残留的 pending 行会污染
	// 其他包 ListPending(user=1) 的计数（对齐 memory_test 的跨包隔离清理模式）。
	// t.Cleanup 提前注册：任一断言 t.Fatal 提前退出也会执行清理。
	p := &Person{DisplayName: "指标测试-甲"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM person_metric WHERE person_id = ?`, p.ID.Int64())
	})
	sess := ids.New()

	// 四个测点，measured_at 覆盖不同日期以验证时序；**故意打乱插入顺序**（w2→w1→w3→e1），
	// 靠 ListByPerson 的 ORDER BY measured_at ASC 归位，证明升序来自 SQL 而非插入顺序。
	tw1 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)  // weight 最早
	te1 := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)  // emotion 居中偏早
	tw2 := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)  // weight 居中偏晚
	tw3 := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC) // weight 最晚（用作 pending 点）

	// w2：数值型体重测点（双存：value_num=73.0 且 value_text="73.0"）。先插它验证乱序归位。
	w2 := &PersonMetric{PersonID: p.ID, MetricKey: "weight", ValueNum: fp(73.0), ValueText: strp("73.0"), Unit: strp("kg"), MeasuredAt: tw2, SessionID: &sess}
	if err := metrics.Create(ctx, w2); err != nil {
		t.Fatal(err)
	}
	// w1：数值型体重测点（双存 72.5 / "72.5"）。未显式给横切字段，用于校验零值兜底。
	w1 := &PersonMetric{PersonID: p.ID, MetricKey: "weight", ValueNum: fp(72.5), ValueText: strp("72.5"), Unit: strp("kg"), MeasuredAt: tw1, SessionID: &sess}
	if err := metrics.Create(ctx, w1); err != nil {
		t.Fatal(err)
	}
	// w3：数值型体重测点（双存 71.0 / "71.0"），状态 pending——供 ListPending / SetStatus 用。
	w3 := &PersonMetric{PersonID: p.ID, MetricKey: "weight", ValueNum: fp(71.0), ValueText: strp("71.0"), Unit: strp("kg"), MeasuredAt: tw3, Status: "pending", SessionID: &sess}
	if err := metrics.Create(ctx, w3); err != nil {
		t.Fatal(err)
	}
	// e1：类别型情绪测点（value_num 为 NULL，只有 value_text="焦虑"）。
	e1 := &PersonMetric{PersonID: p.ID, MetricKey: "emotion", ValueText: strp("焦虑"), MeasuredAt: te1, SessionID: &sess}
	if err := metrics.Create(ctx, e1); err != nil {
		t.Fatal(err)
	}

	// CreateExt 零值兜底校验：w1 未显式给这些字段，应被兜底为默认值。
	if w1.Confidence != 0.8 || w1.Version != 1 || w1.Source != "manual" ||
		w1.EpistemicType != "observed" || w1.Status != "active" || w1.UserID != 1 {
		t.Fatalf("CreateExt 零值兜底异常: %+v", w1)
	}

	// 双存约定回读：数值型 w1 两列都应还原（value_num=72.5 且 value_text="72.5"），unit 也回读。
	got := mustGet(t, metrics, w1.ID)
	if got.ValueNum == nil || *got.ValueNum != 72.5 {
		t.Fatalf("数值型 value_num 回读异常: %v", got.ValueNum)
	}
	if got.ValueText == nil || *got.ValueText != "72.5" {
		t.Fatalf("数值型 value_text（fmt 串）回读异常: %v", got.ValueText)
	}
	if got.Unit == nil || *got.Unit != "kg" {
		t.Fatalf("unit 回读异常: %v", got.Unit)
	}
	if !got.MeasuredAt.Equal(tw1) {
		t.Fatalf("measured_at 回读异常: %v (期望 %v)", got.MeasuredAt, tw1)
	}
	// 类别型 e1：value_num 应为 NULL，value_text="焦虑"。
	gotE := mustGet(t, metrics, e1.ID)
	if gotE.ValueNum != nil {
		t.Fatalf("类别型 value_num 应为 NULL: %v", *gotE.ValueNum)
	}
	if gotE.ValueText == nil || *gotE.ValueText != "焦虑" {
		t.Fatalf("类别型 value_text 回读异常: %v", gotE.ValueText)
	}

	// ListByPerson 全指标、不限时间：应按 measured_at 升序 w1→e1→w2→w3（打乱插入后靠 SQL 归位）。
	all, err := metrics.ListByPerson(ctx, p.ID, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("ListByPerson(全指标) 应 4 行: %d", len(all))
	}
	if all[0].ID != w1.ID || all[1].ID != e1.ID || all[2].ID != w2.ID || all[3].ID != w3.ID {
		t.Fatalf("ListByPerson ASC 排序错误: %v %v %v %v", all[0].ID, all[1].ID, all[2].ID, all[3].ID)
	}

	// metric_key 过滤：只取 weight，应为 w1→w2→w3（emotion 的 e1 被排除），仍升序。
	ws, err := metrics.ListByPerson(ctx, p.ID, "weight", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 3 || ws[0].ID != w1.ID || ws[1].ID != w2.ID || ws[2].ID != w3.ID {
		t.Fatalf("ListByPerson(weight 过滤) 异常: %+v", idsOf(ws))
	}

	// 半开区间 [from, to)：from=tw1（含）、to=tw3（不含），限 weight。
	// 期望命中 w1（==from，左闭含）与 w2，排除 w3（==to，右开不含）——一次验证两个边界。
	from, to := tw1, tw3
	win, err := metrics.ListByPerson(ctx, p.ID, "weight", &from, &to)
	if err != nil {
		t.Fatal(err)
	}
	if len(win) != 2 || win[0].ID != w1.ID || win[1].ID != w2.ID {
		t.Fatalf("半开区间 [from,to) 边界错误（应含 from=w1、不含 to=w3）: %+v", idsOf(win))
	}

	// 自然键命中（数值型按串）：同 session/person/key + value_text="72.5" + measured_at=tw1 → w1。
	nk, err := metrics.FindByNaturalKeyExt(ctx, db, sess, p.ID, "weight", strp("72.5"), tw1)
	if err != nil {
		t.Fatal(err)
	}
	if nk == nil || nk.ID != w1.ID {
		t.Fatalf("FindByNaturalKeyExt 数值型未命中 w1: %+v", nk)
	}
	// 自然键命中（类别型）：value_text="焦虑" + measured_at=te1 → e1。
	nkE, err := metrics.FindByNaturalKeyExt(ctx, db, sess, p.ID, "emotion", strp("焦虑"), te1)
	if err != nil {
		t.Fatal(err)
	}
	if nkE == nil || nkE.ID != e1.ID {
		t.Fatalf("FindByNaturalKeyExt 类别型未命中 e1: %+v", nkE)
	}
	// 自然键未命中（同值不同时刻是两个测点）：value_text="72.5" 但 measured_at=tw2 → nil。
	nkMiss, err := metrics.FindByNaturalKeyExt(ctx, db, sess, p.ID, "weight", strp("72.5"), tw2)
	if err != nil {
		t.Fatal(err)
	}
	if nkMiss != nil {
		t.Fatalf("FindByNaturalKeyExt 应未命中(同值不同时刻): %+v", nkMiss)
	}

	// Get 未命中：不存在的 id 应返回 (nil, nil)。
	if g, err := metrics.Get(ctx, ids.New()); g != nil || err != nil {
		t.Fatalf("Get(不存在 id) 应返回 (nil, nil): g=%+v err=%v", g, err)
	}

	// ListPending 应包含 pending 的 w3。
	pend, err := metrics.ListPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range pend {
		if m.ID == w3.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("ListPending 未包含 pending 测点 w3")
	}

	// SetStatus w3 → dismissed，再 Get 校验已落库。
	if err := metrics.SetStatus(ctx, w3.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if g := mustGet(t, metrics, w3.ID); g.Status != "dismissed" {
		t.Fatalf("SetStatus 未生效: %+v", g)
	}
}

// mustGet 取行并断言存在（测试内联小工具，避免每处重复判空）。
func mustGet(t *testing.T, r *PersonMetricRepo, id ids.ID) *PersonMetric {
	t.Helper()
	m, err := r.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatalf("Get(%v) 应命中却为 nil", id)
	}
	return m
}

// idsOf 提取 id 列表，便于断言失败时打印可读的顺序。
func idsOf(list []PersonMetric) []ids.ID {
	out := make([]ids.ID, len(list))
	for i := range list {
		out[i] = list[i].ID
	}
	return out
}
