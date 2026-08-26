package repo

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

// 纯逻辑：状态机（验证非法流转被拒绝）
func TestTodoCanTransition(t *testing.T) {
	ok := [][2]string{
		{"suggested", "confirmed"},
		{"suggested", "dismissed"},
		{"confirmed", "done"},
		{"confirmed", "dismissed"},
		{"done", "dismissed"},
	}
	for _, c := range ok {
		if !CanTransition(c[0], c[1]) {
			t.Errorf("%s -> %s 应允许", c[0], c[1])
		}
	}
	bad := [][2]string{
		{"suggested", "done"}, // 必须先确认
		{"done", "confirmed"}, // 完成不回退
		{"dismissed", "confirmed"},
		{"confirmed", "suggested"},
		{"", "done"},
	}
	for _, c := range bad {
		if CanTransition(c[0], c[1]) {
			t.Errorf("%s -> %s 应拒绝", c[0], c[1])
		}
	}
}

func TestTodoInsertAndList(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	tr := &TodoRepo{DB: db}
	ctx := t.Context()

	sid := ids.New()
	mem := repoMemoryFixture(t, db, sid)

	due := time.Now().Add(24 * time.Hour)
	// 必须传 *Todo 指针切片，ID 才能回填到调用方（与 Memory DAO 约定一致）。
	tds := []*Todo{{
		Title: "给 Tom 发邮件", SourceMemoryID: &mem.ID, Status: "confirmed",
		DueAt: &due, Confidence: 0.9,
	}}
	if err := tr.InsertExt(ctx, db, tds); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}
	if tds[0].ID == 0 {
		t.Fatal("InsertExt 未回填 ID")
	}
	td := tds[0]

	// 列表联查来源 session
	rows, err := tr.List(ctx, "", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.SourceMemoryID != nil && *row.SourceMemoryID == mem.ID {
			found = true
			if row.SourceSessionID == nil || *row.SourceSessionID != sid {
				t.Fatalf("source_session_id = %v", row.SourceSessionID)
			}
		}
	}
	if !found {
		t.Fatal("未找到刚插入的 todo")
	}

	// 状态更新 + status 过滤
	if err := tr.UpdateStatus(ctx, td.ID, "done"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	// 非法状态值被拒绝（不落库）
	if err := tr.UpdateStatus(ctx, td.ID, "bogus"); err == nil {
		t.Fatal("非法状态 bogus 应返回错误")
	}
	rows2, _ := tr.List(ctx, "done", nil)
	var seen bool
	for _, row := range rows2 {
		if row.ID == td.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatal("status=done 过滤未命中")
	}

	// dismissed 后 List 不再出现（与 ListByTopic/ListBySession 一致排除）
	if err := tr.UpdateStatus(ctx, td.ID, "dismissed"); err != nil {
		t.Fatalf("UpdateStatus dismissed: %v", err)
	}
	rows3, _ := tr.List(ctx, "", nil)
	for _, row := range rows3 {
		if row.ID == td.ID {
			t.Fatal("dismissed 不应出现在 List")
		}
	}
	// dismissed 应出现在 ListDismissed（「已忽略」折叠区取数）
	dismissedRows, _ := tr.ListDismissed(ctx)
	var dismissedSeen bool
	for _, row := range dismissedRows {
		if row.ID == td.ID {
			dismissedSeen = true
		}
	}
	if !dismissedSeen {
		t.Fatal("dismissed 应出现在 ListDismissed")
	}

	// 幂等清理：按来源 session 删除（先删 todo 再删 memory 的顺序由 stage 保证）
	if err := tr.DeleteBySessionExt(ctx, db, sid); err != nil {
		t.Fatalf("DeleteBySessionExt: %v", err)
	}
	_ = mr.DeleteBySessionExt(ctx, db, sid)
	rows4, _ := tr.ListBySession(ctx, sid)
	if len(rows4) != 0 {
		t.Fatalf("清理后仍有 %d 条", len(rows4))
	}
}

// TestTodoDedupSuggested 验证存量 suggested todo 按归一化标题折叠：
// 同一归一化键的组里保留 created_at 最旧一条，其余置 dismissed。
// 用独占 user_id 9527 隔离（迁移显示 todo.user_id 无外键约束，无需 users 行），
// 三条 todo 共享同一 memory fixture 作为 source_memory_id；
// created_at 由显式 UPDATE 设定确定性顺序，保证「保留最旧」结果稳定。
func TestTodoDedupSuggested(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := &TodoRepo{DB: db}
	mr := &MemoryRepo{DB: db}
	ctx := t.Context()

	// 独占 user_id：migrations 显示 todo.user_id 无外键约束（仅 KEY 索引），
	// 用 9527 插数据并只对该 user 跑 DedupSuggested，不与其它测试（user_id=1）互相干扰。
	const uid int64 = 9527
	sid := ids.New()
	mem := repoMemoryFixture(t, db, sid)

	// 三条 suggested：「给Tom」与「给 Tom」归一化后同为 "给tom"（折叠目标），
	// 「学习Rust」归一化为 "学习rust"（独立无重复）。共享同一 source_memory_id。
	tds := []*Todo{
		{UserID: uid, Title: "给Tom", SourceMemoryID: &mem.ID, Status: "suggested", Confidence: 0.8},
		{UserID: uid, Title: "给 Tom", SourceMemoryID: &mem.ID, Status: "suggested", Confidence: 0.8},
		{UserID: uid, Title: "学习Rust", SourceMemoryID: &mem.ID, Status: "suggested", Confidence: 0.8},
	}
	if err := tr.InsertExt(ctx, db, tds); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}

	// created_at 确定性：DB 默认 CURRENT_TIMESTAMP(3) 毫秒级，连续插入可能并排，
	// 故显式 UPDATE 设定递增顺序——给Tom 最旧、给 Tom 次之、学习Rust 最新，
	// 确保「保留最旧」落在「给Tom」上。
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, td := range tds {
		if _, err := db.ExecContext(ctx, `UPDATE todo SET created_at=? WHERE id=?`,
			t0.Add(time.Duration(i)*24*time.Hour), td.ID.Int64()); err != nil {
			t.Fatalf("set created_at[%d]: %v", i, err)
		}
	}

	// 折叠：给Tom(最旧,保留) / 给 Tom(较新,dismissed) / 学习Rust(独立,保留) → dismissed 1 条。
	n, err := tr.DedupSuggested(ctx, uid)
	if err != nil {
		t.Fatalf("DedupSuggested: %v", err)
	}
	if n != 1 {
		t.Fatalf("DedupSuggested dismissed %d，期望 1", n)
	}

	// 逐条断言状态：给Tom 仍 suggested、给 Tom 变 dismissed、学习Rust 仍 suggested。
	want := map[string]string{
		"给Tom":   "suggested",
		"给 Tom":  "dismissed",
		"学习Rust": "suggested",
	}
	for _, td := range tds {
		got, err := tr.Get(ctx, td.ID)
		if err != nil {
			t.Fatalf("Get %q: %v", td.Title, err)
		}
		if got.Status != want[td.Title] {
			t.Errorf("%q 状态=%s，期望 %s", td.Title, got.Status, want[td.Title])
		}
	}

	// 清理：按 session 删 todo + memory，便于重跑（幂等）。
	if err := tr.DeleteBySessionExt(ctx, db, sid); err != nil {
		t.Fatalf("cleanup todos: %v", err)
	}
	_ = mr.DeleteBySessionExt(ctx, db, sid)
}

// repoMemoryFixture 创建一个最小 memory 行（todo 的 source_memory_id 外键数据）。
func repoMemoryFixture(t *testing.T, db *sqlx.DB, sessionID ids.ID) *Memory {
	t.Helper()
	mr := &MemoryRepo{DB: db}
	mem := &Memory{
		Type: "event", Title: "发邮件", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Importance: 0.6, Confidence: 0.9,
		SessionID: sessionID, Status: "active",
	}
	if err := mr.InsertExt(t.Context(), db, []*Memory{mem}); err != nil {
		t.Fatal(err)
	}
	return mem
}
