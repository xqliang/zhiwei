package repo

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"zhiwei/internal/ids"
)

func TestTopicCRUD(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &TopicRepo{DB: db}
	ctx := context.Background()

	// 创建（事务版 CreateExt 与普通 Create 走同一实现）
	tp := &Topic{Name: "Rust 学习", Status: "active", CreatedBy: "user"}
	if err := r.Create(ctx, tp); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tp.ID == 0 {
		t.Fatal("Create 未回填 ID")
	}

	// Get
	got, err := r.Get(ctx, tp.ID)
	if err != nil || got.Name != "Rust 学习" || got.CreatedBy != "user" {
		t.Fatalf("Get: %v %+v", err, got)
	}

	// 按名查找（active/suggested）
	found, err := r.FindActiveByName(ctx, 1, "Rust 学习")
	if err != nil {
		t.Fatalf("FindActiveByName: %v", err)
	}
	if found == nil || found.ID != tp.ID {
		t.Fatalf("found = %+v", found)
	}

	// dismissed 的同名 topic 不参与合并
	other := &Topic{Name: "旧主题", Status: "dismissed", CreatedBy: "ai"}
	_ = r.Create(ctx, other)
	if m, _ := r.FindActiveByName(ctx, 1, "旧主题"); m != nil {
		t.Fatal("dismissed topic 不应被 FindActiveByName 命中")
	}

	// 状态与改名
	if err := r.UpdateStatus(ctx, tp.ID, "dismissed"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if err := r.UpdateName(ctx, tp.ID, "Rust 进阶"); err != nil {
		t.Fatalf("UpdateName: %v", err)
	}
	got2, _ := r.Get(ctx, tp.ID)
	if got2.Name != "Rust 进阶" {
		t.Fatalf("name = %s", got2.Name)
	}
}

func TestTopicListActiveLimit(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	r := &TopicRepo{DB: db}
	ctx := context.Background()

	list, err := r.ListActive(ctx, 1, 30)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, tp := range list {
		if tp.Status == "dismissed" {
			t.Fatal("ListActive 不应包含 dismissed")
		}
	}
}

// TestTopicCreateExtTx 验证 CreateExt 的 *sqlx.Tx 命名插入路径：
// ROLLBACK 后数据不可见，COMMIT 后可见（Task 4/5 事务写入依赖此行为）。
func TestTopicCreateExtTx(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	r := &TopicRepo{DB: db}
	ctx := context.Background()

	// 回滚：CreateExt 在事务内执行，ROLLBACK 后不应落库
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rb := &Topic{Name: "回滚主题", Status: "active", CreatedBy: "ai"}
	if err := r.CreateExt(ctx, tx, rb); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if got, err := r.Get(ctx, rb.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ROLLBACK 后应查不到: got=%v err=%v", got, err)
	}

	// 提交：COMMIT 后应可查到
	tx2, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cm := &Topic{Name: "提交主题", Status: "active", CreatedBy: "ai"}
	if err := r.CreateExt(ctx, tx2, cm); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Get(ctx, cm.ID); err != nil || got.Name != "提交主题" {
		t.Fatalf("COMMIT 后应查到: got=%v err=%v", got, err)
	}
}

// TestTopicFindActiveByNameExt 验证 FindActiveByNameExt 的事务内查询路径：
// 事务开启后能看到其他事务已 COMMIT 的同名行（extract commit 查重依赖此行为），
// 同一事务内插入的行自身可见，无命中返回 nil。
func TestTopicFindActiveByNameExt(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	r := &TopicRepo{DB: db}
	ctx := context.Background()

	// 另一事务提交后：新事务内应查到
	cm := &Topic{Name: "事务查重主题", Status: "suggested", CreatedBy: "ai"}
	tx1, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateExt(ctx, tx1, cm); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}
	tx2, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.FindActiveByNameExt(ctx, tx2, 1, "事务查重主题")
	if err != nil || got == nil || got.ID != cm.ID {
		t.Fatalf("事务内查重应命中: got=%v err=%v", got, err)
	}
	// 同一事务内插入的行自身可见（查重后复用场景）
	if err := r.CreateExt(ctx, tx2, &Topic{Name: "本事务新主题", Status: "suggested", CreatedBy: "ai"}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.FindActiveByNameExt(ctx, tx2, 1, "本事务新主题"); err != nil || got == nil {
		t.Fatalf("本事务内行应可见: got=%v err=%v", got, err)
	}
	if got, err := r.FindActiveByNameExt(ctx, tx2, 1, "不存在的名字"); err != nil || got != nil {
		t.Fatalf("无命中应返回 nil: got=%v err=%v", got, err)
	}
	_ = tx2.Rollback()
}

// TestTopicListWithCounts 验证带计数列表：active memory / confirmed todo 计数正确，
// dismissed 主题不出现。memory/todo DAO 尚未实现，测试里用原生 SQL 直插。
func TestTopicListWithCounts(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	r := &TopicRepo{DB: db}
	ctx := context.Background()

	tp := &Topic{Name: "计数主题", Status: "active", CreatedBy: "ai"}
	if err := r.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	_ = r.Create(ctx, &Topic{Name: "已忽略主题", Status: "dismissed", CreatedBy: "ai"})

	// 直插一条 active memory 与一条 confirmed todo（Task 4/5 前的临时手段）
	sess := newTestSession(ids.New())
	if err := (&SessionRepo{DB: db}).Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO memory (id, user_id, type, title, content, session_id, topic_id, status)
VALUES (?, 1, 'fact', 't', 'c', ?, ?, 'active')`, ids.New().Int64(), sess.ID.Int64(), tp.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO todo (id, user_id, title, topic_id, status)
VALUES (?, 1, 't', ?, 'confirmed')`, ids.New().Int64(), tp.ID.Int64()); err != nil {
		t.Fatal(err)
	}

	list, err := r.ListWithCounts(ctx, 1)
	if err != nil {
		t.Fatalf("ListWithCounts: %v", err)
	}
	for _, w := range list {
		if w.Name == "已忽略主题" {
			t.Fatal("dismissed 主题不应出现在 ListWithCounts")
		}
		if w.Name == "计数主题" && (w.MemoryCount != 1 || w.OpenTodoCount != 1) {
			t.Fatalf("counts = %d/%d, want 1/1", w.MemoryCount, w.OpenTodoCount)
		}
	}
}
