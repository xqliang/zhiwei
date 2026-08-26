package repo

import (
	"context"
	"math"
	"testing"
	"time"

	"zhiwei/internal/ids"
)

func newTestMemory(sessionID ids.ID) *Memory {
	eventAt := time.Now()
	return &Memory{
		Type: "event", Title: "给 Tom 发邮件", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Importance: 0.6, Confidence: 0.9,
		SessionID: &sessionID, TranscriptSegmentIDs: ids.List{1, 2},
		EventAt: &eventAt, Status: "active",
	}
}

func TestMemoryInsertAndQuery(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	tr := &TopicRepo{DB: db}
	ctx := context.Background()

	topic := &Topic{Name: "工作", Status: "active", CreatedBy: "user"}
	_ = tr.Create(ctx, topic)
	mtr := &MemoryTopicRepo{DB: db}

	sid := ids.New()
	// 跨包隔离：本用例建的 memory（active 的 m + dismissed 的 dm）都挂在 sid 上，收尾按
	// session 删净——否则残留的 active event 行会污染 api 包 TestMemoryListAndFilter 的
	// type=event 计数（repo→api 逆序跑才暴露；make test-integration 字母序 api 在前掩盖）。
	// 用 t.Cleanup 提前注册，任一断言 t.Fatal 提前退出也会清理。
	t.Cleanup(func() {
		_, _ = mr.DB.ExecContext(context.Background(), `DELETE FROM memory WHERE session_id = ?`, sid.Int64())
	})
	m := newTestMemory(sid)
	// 必须传 *Memory 指针切片，ID 才能回填到调用方的 m。
	if err := mr.InsertExt(ctx, db, []*Memory{m}); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("InsertExt 未回填 ID")
	}
	// topic 归属走关联表：建 memory 后 AddLink，List 内联 topics[] 反映
	if err := mtr.AddLink(ctx, m.ID, topic.ID); err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	// 按 session 查询（内联 topics[]）
	rows, err := mr.ListBySession(ctx, sid)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListBySession: %v len=%d", err, len(rows))
	}
	if len(rows[0].Topics) != 1 || rows[0].Topics[0].Name != "工作" {
		t.Fatalf("topics = %+v, want [{工作}]", rows[0].Topics)
	}
	if len(rows[0].TranscriptSegmentIDs) != 2 {
		t.Fatalf("segment_ids = %v", rows[0].TranscriptSegmentIDs)
	}

	// Get
	got, err := mr.Get(ctx, 1, m.ID)
	if err != nil || got.Title != "给 Tom 发邮件" {
		t.Fatalf("Get: %v %+v", err, got)
	}

	// Save：改内容 version+1
	got.Content = "后天给 Tom 发邮件确认设计稿"
	got.Version++
	if err := mr.Save(ctx, got); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got2, _ := mr.Get(ctx, 1, m.ID)
	if got2.Version != 2 || got2.Content != "后天给 Tom 发邮件确认设计稿" {
		t.Fatalf("after save: %+v", got2)
	}

	// 过滤列表：type 过滤命中，错误 type 无结果
	if rows, _ = mr.List(ctx, MemoryFilter{UserID: 1, Type: "event", Limit: 10}); len(rows) < 1 {
		t.Fatal("type=event 应命中")
	}
	if rows, _ = mr.List(ctx, MemoryFilter{UserID: 1, Type: "idea", Limit: 10}); len(rows) != 0 {
		t.Fatal("type=idea 不应命中")
	}

	// dismissed 不出现在列表；offset 超界返回空
	dm := newTestMemory(sid)
	dm.Status = "dismissed"
	if err := mr.InsertExt(ctx, db, []*Memory{dm}); err != nil {
		t.Fatalf("InsertExt dismissed: %v", err)
	}
	if rows, _ = mr.ListBySession(ctx, sid); len(rows) != 1 {
		t.Fatalf("dismissed 应被 ListBySession 排除，got %d", len(rows))
	}
	if rows, _ = mr.List(ctx, MemoryFilter{UserID: 1, Limit: 10, Offset: 9999}); len(rows) != 0 {
		t.Fatalf("offset 越界应返回空，got %d", len(rows))
	}
}

func TestMemoryDeleteBySession(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()

	sid := ids.New()
	m := newTestMemory(sid)
	_ = mr.InsertExt(ctx, db, []*Memory{m})
	if err := mr.DeleteBySessionExt(ctx, db, sid); err != nil {
		t.Fatalf("DeleteBySessionExt: %v", err)
	}
	if rows, _ := mr.ListBySession(ctx, sid); len(rows) != 0 {
		t.Fatalf("删除后仍有 %d 条", len(rows))
	}
}

func TestMemoryListSince(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()

	// 两条不同 event_at 的记忆（标题加 since 前缀隔离共享测试库的脏数据）
	sid := ids.New()
	// 跨包隔离：本用例建的两条 active event 记忆都挂在 sid 上，收尾按 session 删净——否则
	// 残留会污染 api 包 TestMemoryListAndFilter 的 type=event 计数（repo→api 逆序跑才暴露）。
	t.Cleanup(func() {
		_, _ = mr.DB.ExecContext(context.Background(), `DELETE FROM memory WHERE session_id = ?`, sid.Int64())
	})
	// 预清理：共享测试库可能残留历史运行的同名行（脏库重跑），先删掉保证计数断言稳定
	if _, err := mr.DB.ExecContext(ctx,
		`DELETE FROM memory WHERE title IN (?, ?)`, "since 用例-早", "since 用例-晚"); err != nil {
		t.Fatal(err)
	}
	early := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	late := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		title  string
		eventA time.Time
	}{{"since 用例-早", early}, {"since 用例-晚", late}} {
		m := newTestMemory(sid)
		m.Title = tc.title
		m.EventAt = &tc.eventA
		if err := mr.InsertExt(ctx, db, []*Memory{m}); err != nil {
			t.Fatal(err)
		}
	}

	// 下界夹在两条之间 → 含「晚」不含「早」。
	// 注意共享测试库中其他 fixture 行的 event_at 可能落在下界之后，
	// 故断言用「包含/不包含本组两条标题」而非精确计数。
	mid := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	since := mid
	rows, err := mr.List(ctx, MemoryFilter{UserID: 1, Since: &since, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if !hasTitle(rows, "since 用例-晚") || hasTitle(rows, "since 用例-早") {
		t.Fatalf("Since=mid 结果 = %v", titles(rows))
	}
	// 等于 late 本身 → 仍命中（>= 含等于）
	since = late
	if rows, _ = mr.List(ctx, MemoryFilter{UserID: 1, Since: &since, Limit: 200}); !hasTitle(rows, "since 用例-晚") {
		t.Fatalf("Since=late 应命中（>= 含等于），结果 = %v", titles(rows))
	}
	// 零值 Since 不过滤 → 两条都在（Limit 200 容纳共享库其他 fixture 行）
	if rows, _ = mr.List(ctx, MemoryFilter{UserID: 1, Limit: 200}); !hasTitle(rows, "since 用例-早") || !hasTitle(rows, "since 用例-晚") {
		t.Fatalf("无 Since 应两条都在，结果 = %v", titles(rows))
	}
}

func hasTitle(rows []MemoryRow, title string) bool {
	for _, r := range rows {
		if r.Title == title {
			return true
		}
	}
	return false
}

func titles(rows []MemoryRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Title
	}
	return out
}

// TestMemoryListWithTopics 验证 List 返回的行内联 topics[]（关联表多对多）。
// 建 1 条 memory 关联 2 个 topic → List 后 rows[0].Topics 长度=2。
func TestMemoryListWithTopics(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	ctx := context.Background()
	mr := &MemoryRepo{DB: db}
	mtr := &MemoryTopicRepo{DB: db}
	tr := &TopicRepo{DB: db}

	// 预清理：脏库重跑时同名行会让断言不稳定
	if _, err := db.ExecContext(ctx,
		`DELETE FROM memory WHERE title = ?`, "多主题记忆用例"); err != nil {
		t.Fatal(err)
	}

	m := &Memory{Type: "fact", Title: "多主题记忆用例", Content: "足够长的内容描述",
		EpistemicType: "observed", Confidence: 0.9, SessionID: idPtr(ids.New()), Status: "active"}
	if err := mr.InsertExt(ctx, db, []*Memory{m}); err != nil {
		t.Fatal(err)
	}
	t1 := &Topic{Name: "多主题-一", Status: "active", CreatedBy: "user"}
	t2 := &Topic{Name: "多主题-二", Status: "active", CreatedBy: "user"}
	if err := tr.Create(ctx, t1); err != nil {
		t.Fatal(err)
	}
	if err := tr.Create(ctx, t2); err != nil {
		t.Fatal(err)
	}
	if err := mtr.AddLink(ctx, m.ID, t1.ID); err != nil {
		t.Fatal(err)
	}
	if err := mtr.AddLink(ctx, m.ID, t2.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := mr.List(ctx, MemoryFilter{UserID: 1, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var got *MemoryRow
	for i := range rows {
		if rows[i].ID == m.ID {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatalf("未找到刚插入的 memory")
	}
	if len(got.Topics) != 2 {
		t.Fatalf("topics=%d, want 2: %+v", len(got.Topics), got.Topics)
	}
}

func TestMemoryListActiveTitlesExt(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()
	now := time.Now()
	sid := ids.New()
	ms := []*Memory{
		{Type: "fact", Title: "学Rust", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: &sid, EventAt: &now, Status: "active"},
		{Type: "fact", Title: "学Go", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: &sid, EventAt: &now, Status: "active"},
		{Type: "fact", Title: "学Python", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: &sid, EventAt: &now, Status: "superseded"},
	}
	if err := mr.InsertExt(ctx, db, ms); err != nil {
		t.Fatal(err)
	}
	rows, err := mr.ListActiveTitlesExt(ctx, db, 1)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Title] = true
	}
	if !got["学Rust"] || !got["学Go"] || got["学Python"] {
		t.Fatalf("ListActiveTitlesExt = %v, want 学Rust+学Go（不含 superseded）", got)
	}
}

func TestMemoryBumpConfidence(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()
	now := time.Now()
	lo := &Memory{Type: "fact", Title: "佐证Bump低", Content: "x", EpistemicType: "observed", Confidence: 0.80, SessionID: idPtr(ids.New()), EventAt: &now, Status: "active"}
	hi := &Memory{Type: "fact", Title: "佐证Bump高", Content: "x", EpistemicType: "observed", Confidence: 0.97, SessionID: idPtr(ids.New()), EventAt: &now, Status: "active"}
	if err := mr.InsertExt(ctx, db, []*Memory{lo, hi}); err != nil {
		t.Fatal(err)
	}
	// 0.80 + 0.05 → 0.85
	if err := mr.BumpConfidenceExt(ctx, db, lo.ID, 0.05); err != nil {
		t.Fatal(err)
	}
	got, _ := mr.Get(ctx, 1, lo.ID)
	if math.Abs(got.Confidence-0.85) > 0.001 {
		t.Fatalf("confidence = %v, want 0.85", got.Confidence)
	}
	// 0.97 + 0.05 → 封顶 0.99（不超）
	if err := mr.BumpConfidenceExt(ctx, db, hi.ID, 0.05); err != nil {
		t.Fatal(err)
	}
	gotHi, _ := mr.Get(ctx, 1, hi.ID)
	if math.Abs(gotHi.Confidence-0.99) > 0.001 {
		t.Fatalf("confidence = %v, want 0.99（封顶）", gotHi.Confidence)
	}
}

func TestMemoryListActive(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()
	now := time.Now()
	a := &Memory{Type: "fact", Title: "整理ListA", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: idPtr(ids.New()), EventAt: &now, Status: "active"}
	s := &Memory{Type: "fact", Title: "整理ListS", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: idPtr(ids.New()), EventAt: &now, Status: "superseded"}
	if err := mr.InsertExt(ctx, db, []*Memory{a, s}); err != nil {
		t.Fatal(err)
	}
	rows, err := mr.ListActive(ctx, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range rows {
		got[m.Title] = true
	}
	if !got["整理ListA"] || got["整理ListS"] {
		t.Fatalf("ListActive = %v, want 含整理ListA 不含 superseded", got)
	}
}

func TestMemoryApplyConsolidation(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	mtr := &MemoryTopicRepo{DB: db}
	tr := &TopicRepo{DB: db}
	ctx := context.Background()
	_, _ = db.ExecContext(ctx, `UPDATE topic SET status='dismissed' WHERE user_id=1 AND name IN (?,?) AND status IN ('active','suggested')`, "整理靶主题", "整理源主题")
	_, _ = db.ExecContext(ctx, `DELETE FROM memory WHERE title IN (?,?)`, "整理A记忆", "整理B记忆")
	now := time.Now()
	a := &Memory{Type: "fact", Title: "整理A记忆", Content: "A", EpistemicType: "observed", Confidence: 0.80, SessionID: idPtr(ids.New()), EventAt: &now, Status: "active"}
	b := &Memory{Type: "fact", Title: "整理B记忆", Content: "B", EpistemicType: "observed", Confidence: 0.80, SessionID: idPtr(ids.New()), EventAt: &now, Status: "active"}
	if err := mr.InsertExt(ctx, db, []*Memory{a, b}); err != nil {
		t.Fatal(err)
	}
	x := &Topic{Name: "整理靶主题", Status: "active", CreatedBy: "ai"}
	y := &Topic{Name: "整理源主题", Status: "active", CreatedBy: "ai"}
	_ = tr.Create(ctx, x)
	_ = tr.Create(ctx, y)
	_ = mtr.AddLink(ctx, a.ID, x.ID)
	_ = mtr.AddLink(ctx, b.ID, y.ID)

	// merges 优先：B 被 merge 置 superseded；adjustment 指向 B 应被跳过 → adjusted=0
	merged, adjusted, err := mr.ApplyConsolidation(ctx, ConsolidationReq{
		Merges:      []MemoryMerge{{CanonicalID: a.ID, MemberIDs: []ids.ID{a.ID, b.ID}}},
		Adjustments: []MemoryAdjustment{{MemoryID: b.ID, Kind: "corroborate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if merged != 1 || adjusted != 0 {
		t.Fatalf("merged=%d adjusted=%d, want 1/0（B 被 merge supersede，adjustment 跳过）", merged, adjusted)
	}
	bGot, _ := mr.Get(ctx, 1, b.ID)
	if bGot.Status != "superseded" {
		t.Fatalf("B status=%s, want superseded", bGot.Status)
	}
	// A 聚合 X+Y（B 的 Y 迁来）
	aLinks, _ := mtr.ListByMemoryIDs(ctx, []ids.ID{a.ID})
	names := map[string]bool{}
	for _, ti := range aLinks[a.ID] {
		names[ti.Name] = true
	}
	if !names["整理靶主题"] || !names["整理源主题"] {
		t.Fatalf("A topics=%v, want 含整理靶主题+整理源主题", names)
	}
	// B 的 memory_topic 已删
	bLinks, _ := mtr.ListByMemoryIDs(ctx, []ids.ID{b.ID})
	if len(bLinks[b.ID]) != 0 {
		t.Fatalf("B topic 关联=%d, want 0（已迁删）", len(bLinks[b.ID]))
	}
}

// TestMemorySaveExt 验证 SaveExt（事务版 Save）传 db 执行时与 Save 等价：
// 更新 title/content/version 落库、Get 读回一致。确认闸门 memory_update 落库依赖此方法
// （与 Proposals.Resolve 同事务）。
func TestMemorySaveExt(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	ctx := t.Context()

	sid := ids.New()
	// t.Cleanup 里 ctx 已取消，须用 context.Background()。
	t.Cleanup(func() { _ = mr.DeleteBySessionExt(context.Background(), db, sid) })
	m := newTestMemory(sid)
	if err := mr.InsertExt(ctx, db, []*Memory{m}); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}

	// 读回插入后的版本再 +1（不假设 DB 默认版本值），传 db 调 SaveExt
	cur, err := mr.Get(ctx, 1, m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	cur.Title = "SaveExt 改后标题"
	cur.Content = "SaveExt 改后内容"
	cur.Version++
	if err := mr.SaveExt(ctx, db, cur); err != nil {
		t.Fatalf("SaveExt: %v", err)
	}
	got, err := mr.Get(ctx, 1, m.ID)
	if err != nil {
		t.Fatalf("Get after SaveExt: %v", err)
	}
	if got.Title != "SaveExt 改后标题" || got.Content != "SaveExt 改后内容" || got.Version != cur.Version {
		t.Fatalf("SaveExt 效果异常: %+v", got)
	}
}
