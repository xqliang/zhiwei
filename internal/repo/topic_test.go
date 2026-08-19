package repo

import (
	"context"
	"testing"
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
