package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
)

func TestPersonEventQueries(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	events := &PersonEventRepo{DB: db}

	// 两个人物：甲是大事记主体，乙作为「同场人物」写入 e1 的 related_person_ids。
	a := &Person{DisplayName: "事件测试-甲"}
	b := &Person{DisplayName: "事件测试-乙"}
	if err := persons.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := persons.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	sess := ids.New()

	// 三条事件，occurred_at 覆盖不同时间点以验证倒序：
	// e1 最新(2026-07) > e2 居中(2026-03) > e3 最早(2025-01)。
	// e2 显式给了居中时间——否则若 e2 无时间会沉底，破坏 e1→e2→e3 的期望顺序。
	t1 := time.Date(2026, 7, 20, 15, 0, 0, 0, time.UTC)
	t1End := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	e1 := &PersonEvent{
		PersonID:         a.ID,
		EventType:        "旅行",
		Title:            "去云南旅游",
		Description:      strp("和朋友自驾环游"),
		OccurredAt:       &t1,
		EndAt:            &t1End,
		Location:         strp("云南"),
		RelatedPersonIDs: ids.List{b.ID},
		Status:           "active",
		SessionID:        &sess,
	}
	if err := events.Create(ctx, e1); err != nil {
		t.Fatal(err)
	}
	t2 := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	e2 := &PersonEvent{
		PersonID:   a.ID,
		EventType:  "会议",
		Title:      "周会",
		OccurredAt: &t2,
		Status:     "pending",
		SessionID:  &sess,
	}
	if err := events.Create(ctx, e2); err != nil {
		t.Fatal(err)
	}
	t3 := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	e3 := &PersonEvent{
		PersonID:   a.ID,
		EventType:  "里程碑",
		Title:      "升职",
		OccurredAt: &t3,
		Status:     "active",
		SessionID:  &sess,
	}
	if err := events.Create(ctx, e3); err != nil {
		t.Fatal(err)
	}

	// CreateExt 零值兜底校验：e1 未显式给这些字段，应被兜底为默认值。
	if e1.Importance != 0.5 || e1.Confidence != 0.8 || e1.Version != 1 ||
		e1.Source != "manual" || e1.EpistemicType != "observed" || e1.UserID != 1 {
		t.Fatalf("CreateExt 零值兜底异常: %+v", e1)
	}

	// FindActiveByNormalizedTitleExt：按（主体, 类型, 归一化标题）命中 active 的 e1。
	got, err := events.FindActiveByNormalizedTitleExt(ctx, db, a.ID, "旅行", "去云南旅游")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != e1.ID {
		t.Fatalf("FindActiveByNormalizedTitleExt 未命中 e1: %+v", got)
	}
	// related_person_ids 应能从 JSON 列原样还原为 [乙.ID]（验证 ids.List Scan）。
	if len(got.RelatedPersonIDs) != 1 || got.RelatedPersonIDs[0] != b.ID {
		t.Fatalf("related_person_ids 还原异常: %+v", got.RelatedPersonIDs)
	}
	// 可空列回读：description/end_at/location 三个 NULL-able 列须能原样往返
	//（Task 6 落库/回显依赖此扫描行为，这里钉死）。end_at 用 Equal 比较，避开时区/位置指针差异。
	if got.Description == nil || *got.Description != "和朋友自驾环游" {
		t.Fatalf("description 回读异常: %v", got.Description)
	}
	if got.EndAt == nil || !got.EndAt.Equal(t1End) {
		t.Fatalf("end_at 回读异常: %v (期望 %v)", got.EndAt, t1End)
	}
	if got.Location == nil || *got.Location != "云南" {
		t.Fatalf("location 回读异常: %v", got.Location)
	}
	// P2a③ 归一化命中：字面近重复标题（空格 + 全角标点）归一化后与 e1 同值 → 命中 e1。
	// 「去 云南 旅游！」→ 去标点空格 → "去云南旅游" == NormalizeTitle(e1.Title)。
	normHit, err := events.FindActiveByNormalizedTitleExt(ctx, db, a.ID, "旅行", "去 云南 旅游！")
	if err != nil {
		t.Fatal(err)
	}
	if normHit == nil || normHit.ID != e1.ID {
		t.Fatalf("FindActiveByNormalizedTitleExt 归一化近重复标题应命中 e1: %+v", normHit)
	}
	// 未命中：归一化后仍不同的标题应返回 nil。
	miss, err := events.FindActiveByNormalizedTitleExt(ctx, db, a.ID, "旅行", "去西藏旅游")
	if err != nil {
		t.Fatal(err)
	}
	if miss != nil {
		t.Fatalf("FindActiveByNormalizedTitleExt 应未命中(归一化后仍不同): %+v", miss)
	}

	// Get 未命中路径：不存在的 id 应返回 (nil, nil)，不报 sql.ErrNoRows（调用方按 nil 判空）。
	if g, err := events.Get(ctx, ids.New()); g != nil || err != nil {
		t.Fatalf("Get(不存在 id) 应返回 (nil, nil): g=%+v err=%v", g, err)
	}

	// FindByNaturalKeyExt：任意 status 都能命中，这里命中 pending 的 e2。
	nk, err := events.FindByNaturalKeyExt(ctx, db, sess, a.ID, "会议", "周会")
	if err != nil {
		t.Fatal(err)
	}
	if nk == nil || nk.ID != e2.ID {
		t.Fatalf("FindByNaturalKeyExt 未命中 e2: %+v", nk)
	}
	// 未命中：同 session 同 person 但 title 不存在 → nil（幂等去重不会误命中别的事件）。
	nkMiss, err := events.FindByNaturalKeyExt(ctx, db, sess, a.ID, "会议", "不存在的标题")
	if err != nil {
		t.Fatal(err)
	}
	if nkMiss != nil {
		t.Fatalf("FindByNaturalKeyExt 应未命中(不存在标题): %+v", nkMiss)
	}

	// ListByPerson：3 行，时间倒序 e1 → e2 → e3。
	rows, err := events.ListByPerson(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("ListByPerson 应 3 行: %d", len(rows))
	}
	if rows[0].ID != e1.ID || rows[1].ID != e2.ID || rows[2].ID != e3.ID {
		t.Fatalf("ListByPerson 时间倒序错误: %v %v %v", rows[0].ID, rows[1].ID, rows[2].ID)
	}

	// ListPending 应包含 pending 的 e2。
	pend, err := events.ListPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range pend {
		if e.ID == e2.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("ListPending 未包含 pending 事件 e2")
	}

	// SetStatus e2 → dismissed，再 Get 校验状态已落库。
	if err := events.SetStatus(ctx, e2.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if g, _ := events.Get(ctx, e2.ID); g == nil || g.Status != "dismissed" {
		t.Fatalf("SetStatus 未生效: %+v", g)
	}

	// e4：occurred_at 为 NULL，验证 DESC 排序下 NULL 沉底（排最后）。
	e4 := &PersonEvent{
		PersonID:  a.ID,
		EventType: "其他",
		Title:     "时间未知的事件",
		Status:    "active",
		SessionID: &sess,
	}
	if err := events.Create(ctx, e4); err != nil {
		t.Fatal(err)
	}
	rows2, err := events.ListByPerson(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows2) != 4 {
		t.Fatalf("ListByPerson 应 4 行: %d", len(rows2))
	}
	// NULL occurred_at 的 e4 必须在末位。
	if rows2[len(rows2)-1].ID != e4.ID {
		t.Fatalf("occurred_at=NULL 的 e4 应排最后, 实际末位=%v", rows2[len(rows2)-1].ID)
	}
	// 且前三条仍是时间倒序 e1 → e2 → e3（e2 虽已 dismissed，ListByPerson 全状态返回）。
	if rows2[0].ID != e1.ID || rows2[1].ID != e2.ID || rows2[2].ID != e3.ID {
		t.Fatalf("含 NULL 行后前三顺序错误: %v %v %v", rows2[0].ID, rows2[1].ID, rows2[2].ID)
	}
}
