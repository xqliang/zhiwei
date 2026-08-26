package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
)

func TestPersonActivityQueries(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	activities := &PersonActivityRepo{DB: db}

	// 挂在独立人物下，避免污染其他包读到的公共数据；活动仍默认 user_id=1（owner），
	// 故收尾按 person_id 删净本用例建的 person_activity 行——否则残留的 pending 行会污染
	// 其他包 ListPending(user=1) 的计数（对齐 person_metric_test 的跨包隔离清理模式）。
	// t.Cleanup 提前注册：任一断言 t.Fatal 提前退出也会执行清理。
	p := &Person{DisplayName: "活动测试-甲"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM person_activity WHERE person_id = ?`, p.ID.Int64())
	})
	sess := ids.New()

	// 四条活动，started_at 覆盖不同日期以验证时间线；**故意打乱插入顺序**（a2→a1→a3→am），
	// 靠 ListByPerson 的 ORDER BY started_at ASC 归位，证明升序来自 SQL 而非插入顺序。
	ta1 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)  // 最早（可空列全 NULL，验 NULL 往返与 <=> 命中）
	ta2 := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)  // 居中偏早（部分可空列有值）
	ta3 := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)  // 居中偏晚（四个可空列全有值，验满字段往返）
	ta4 := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC) // 最晚（pending，供 ListPending/SetStatus 用）

	// a2：满字段活动（tool/location/commute_mode/duration_min 全非空）。先插它验证乱序归位。
	a2 := &PersonActivity{
		PersonID: p.ID, Activity: "打球",
		Tool: strp("健身房"), Location: strp("体育馆"), CommuteMode: strp("步行"), DurationMin: intp(90),
		StartedAt: ta3, SessionID: &sess,
	}
	if err := activities.Create(ctx, a2); err != nil {
		t.Fatal(err)
	}
	// a1：极简活动——仅 activity，四个可空列全 NULL（「早上通勤」没提工具地点也是有效活动）。
	// 未显式给横切字段，用于校验零值兜底。
	a1 := &PersonActivity{PersonID: p.ID, Activity: "通勤", StartedAt: ta1, SessionID: &sess}
	if err := activities.Create(ctx, a1); err != nil {
		t.Fatal(err)
	}
	// a3：pending 活动，供 ListPending / SetStatus / 计数用。
	a3 := &PersonActivity{PersonID: p.ID, Activity: "开会", Location: strp("会议室"), StartedAt: ta4, Status: "pending", SessionID: &sess}
	if err := activities.Create(ctx, a3); err != nil {
		t.Fatal(err)
	}
	// am：部分可空列有值（tool/location 有、commute_mode/duration 无）。
	am := &PersonActivity{PersonID: p.ID, Activity: "写代码", Tool: strp("电脑"), Location: strp("公司"), StartedAt: ta2, SessionID: &sess}
	if err := activities.Create(ctx, am); err != nil {
		t.Fatal(err)
	}

	// CreateExt 零值兜底校验：a1 未显式给这些字段，应被兜底为默认值。
	if a1.Confidence != 0.8 || a1.Version != 1 || a1.Source != "manual" ||
		a1.EpistemicType != "observed" || a1.Status != "active" || a1.UserID != 1 {
		t.Fatalf("CreateExt 零值兜底异常: %+v", a1)
	}

	// ---- 场景 1：Create+Get 往返（含可空列 NULL）----
	// a1 极简行：activity 回读，四个可空列应全为 NULL，started_at 精确还原。
	got1 := mustGetActivity(t, activities, a1.ID)
	if got1.Activity != "通勤" {
		t.Fatalf("activity 回读异常: %q", got1.Activity)
	}
	if got1.Tool != nil || got1.Location != nil || got1.CommuteMode != nil || got1.DurationMin != nil {
		t.Fatalf("可空列应全为 NULL: tool=%v location=%v commute=%v duration=%v",
			got1.Tool, got1.Location, got1.CommuteMode, got1.DurationMin)
	}
	if !got1.StartedAt.Equal(ta1) {
		t.Fatalf("started_at 回读异常: %v (期望 %v)", got1.StartedAt, ta1)
	}
	// a2 满字段行：四个可空列都应还原为对应值。
	got2 := mustGetActivity(t, activities, a2.ID)
	if got2.Tool == nil || *got2.Tool != "健身房" {
		t.Fatalf("tool 回读异常: %v", got2.Tool)
	}
	if got2.Location == nil || *got2.Location != "体育馆" {
		t.Fatalf("location 回读异常: %v", got2.Location)
	}
	if got2.CommuteMode == nil || *got2.CommuteMode != "步行" {
		t.Fatalf("commute_mode 回读异常: %v", got2.CommuteMode)
	}
	if got2.DurationMin == nil || *got2.DurationMin != 90 {
		t.Fatalf("duration_min 回读异常: %v", got2.DurationMin)
	}

	// Get 未命中：不存在的 id 应返回 (nil, nil)。
	if g, err := activities.Get(ctx, ids.New()); g != nil || err != nil {
		t.Fatalf("Get(不存在 id) 应返回 (nil, nil): g=%+v err=%v", g, err)
	}

	// ---- 场景 2：ListByPerson 升序 + 半开区间边界 ----
	// 不限时间：应按 started_at 升序 a1→am→a2→a3（打乱插入后靠 SQL ASC 归位）。
	all, err := activities.ListByPerson(ctx, p.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("ListByPerson 应 4 行: %d", len(all))
	}
	if all[0].ID != a1.ID || all[1].ID != am.ID || all[2].ID != a2.ID || all[3].ID != a3.ID {
		t.Fatalf("ListByPerson ASC 排序错误: %v %v %v %v", all[0].ID, all[1].ID, all[2].ID, all[3].ID)
	}
	// 半开区间 [from, to)：from=ta1（含）、to=ta4（不含）。
	// 期望命中 a1（==from，左闭含）、am、a2，排除 a3（==to，右开不含）——一次验证两个边界。
	from, to := ta1, ta4
	win, err := activities.ListByPerson(ctx, p.ID, &from, &to)
	if err != nil {
		t.Fatal(err)
	}
	if len(win) != 3 || win[0].ID != a1.ID || win[1].ID != am.ID || win[2].ID != a2.ID {
		t.Fatalf("半开区间 [from,to) 边界错误（应含 from=a1、不含 to=a3）: %+v", activityIDsOf(win))
	}

	// ---- 场景 3：FindByNaturalKeyExt 四个可空列全 NULL 命中（<=> 验证）----
	// a1 的 tool/location/commute_mode/duration 全 NULL：四个可空参数传 nil 应命中它，
	// 证明 <=> 能把「绑定 NULL」匹配到「该列 IS NULL」的行（普通 = NULL 永不命中）。
	nk1, err := activities.FindByNaturalKeyExt(ctx, db, sess, p.ID, strp("通勤"), nil, nil, nil, ta1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nk1 == nil || nk1.ID != a1.ID {
		t.Fatalf("FindByNaturalKeyExt 全 NULL 可空列未命中 a1: %+v", nk1)
	}
	// 满字段命中：四个可空参数全传非 nil + duration 命中 a2。
	nk2, err := activities.FindByNaturalKeyExt(ctx, db, sess, p.ID, strp("打球"), strp("健身房"), strp("体育馆"), strp("步行"), ta3, intp(90))
	if err != nil {
		t.Fatal(err)
	}
	if nk2 == nil || nk2.ID != a2.ID {
		t.Fatalf("FindByNaturalKeyExt 满字段未命中 a2: %+v", nk2)
	}
	// 未命中（同活动不同开始时刻是两条记录）：activity="通勤" 但 started_at=ta3 → nil。
	if miss, err := activities.FindByNaturalKeyExt(ctx, db, sess, p.ID, strp("通勤"), nil, nil, nil, ta3, nil); err != nil {
		t.Fatal(err)
	} else if miss != nil {
		t.Fatalf("FindByNaturalKeyExt 应未命中(同活动不同时刻): %+v", miss)
	}
	// 未命中（<=> 严格区分 NULL 与非 NULL）：a1.tool 为 NULL，传非 nil 的 tool="地铁" 不应命中它——
	// 证明 <=> 不是「忽略该列」而是精确区分 NULL/非 NULL（对齐 cycle 测试的严格性校验）。
	if miss, err := activities.FindByNaturalKeyExt(ctx, db, sess, p.ID, strp("通勤"), strp("地铁"), nil, nil, ta1, nil); err != nil {
		t.Fatal(err)
	} else if miss != nil {
		t.Fatalf("FindByNaturalKeyExt(tool 非nil) 不应命中 tool 为 NULL 的 a1: %+v", miss)
	}

	// ---- 场景 5（先查计数，SetStatus 前）：ListPending / CountPendingByPerson ----
	// ListPending 应包含 pending 的 a3。
	pend, err := activities.ListPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range pend {
		if a.ID == a3.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("ListPending 未包含 pending 活动 a3")
	}
	// 本人物 pending 计数：仅 a3 一条。
	if n, err := activities.CountPendingByPerson(ctx, p.ID); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("CountPendingByPerson 应为 1(仅 a3): %d", n)
	}

	// ---- 场景 4：SetStatus ----
	// SetStatus a3 → dismissed，再 Get 校验已落库。
	if err := activities.SetStatus(ctx, a3.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if g := mustGetActivity(t, activities, a3.ID); g.Status != "dismissed" {
		t.Fatalf("SetStatus 未生效: %+v", g)
	}
	// dismiss 后 pending 计数应归零（a3 已不再 pending）。
	if n, err := activities.CountPendingByPerson(ctx, p.ID); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("SetStatus 后 CountPendingByPerson 应为 0: %d", n)
	}
}

// mustGetActivity 取行并断言存在（测试内联小工具，避免每处重复判空）。
func mustGetActivity(t *testing.T, r *PersonActivityRepo, id ids.ID) *PersonActivity {
	t.Helper()
	a, err := r.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatalf("Get(%v) 应命中却为 nil", id)
	}
	return a
}

// activityIDsOf 提取 id 列表，便于断言失败时打印可读的顺序。
func activityIDsOf(list []PersonActivity) []ids.ID {
	out := make([]ids.ID, len(list))
	for i := range list {
		out[i] = list[i].ID
	}
	return out
}
