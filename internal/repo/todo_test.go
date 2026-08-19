package repo

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
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
	db, err := NewDB(TestDSN(t))
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
