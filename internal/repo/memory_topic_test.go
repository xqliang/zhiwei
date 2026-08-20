package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestMemoryTopicRepo(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &MemoryTopicRepo{DB: db}

	// 预置 1 memory + 2 topic（memory 的 session_id 随机生成，无外键约束即可）
	m := &Memory{Type: "fact", Title: "t", Content: "足够长的内容描述",
		EpistemicType: "observed", Confidence: 0.9, SessionID: ids.New()}
	if err := (&MemoryRepo{DB: db}).InsertExt(ctx, db, []*Memory{m}); err != nil {
		t.Fatal(err)
	}
	tp1 := &Topic{Name: "T1", Status: "active", CreatedBy: "user"}
	tp2 := &Topic{Name: "T2", Status: "active", CreatedBy: "user"}
	(&TopicRepo{DB: db}).Create(ctx, tp1)
	(&TopicRepo{DB: db}).Create(ctx, tp2)

	// AddLink 幂等：两次加同一关联不报错、不重复
	if err := r.AddLink(ctx, m.ID, tp1.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.AddLink(ctx, m.ID, tp1.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.AddLink(ctx, m.ID, tp2.ID); err != nil {
		t.Fatal(err)
	}

	// ListByMemoryIDs 聚合
	got, err := r.ListByMemoryIDs(ctx, []ids.ID{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[m.ID]) != 2 {
		t.Fatalf("topics = %d, want 2", len(got[m.ID]))
	}
	for _, ti := range got[m.ID] {
		if ti.Source != "user" {
			t.Fatalf("source = %s, want user", ti.Source)
		}
	}

	// InsertExt 批量（ai）幂等去重
	if err := r.InsertExt(ctx, db, []*MemoryTopicLink{
		{MemoryID: m.ID, TopicID: tp1.ID, Source: "ai"}, // 已有 user 行，PK 冲突 IGNORE
		{MemoryID: m.ID, TopicID: tp1.ID, Source: "ai"},
	}); err != nil {
		t.Fatal(err)
	}
	got2, _ := r.ListByMemoryIDs(ctx, []ids.ID{m.ID})
	if len(got2[m.ID]) != 2 { // 仍 2 条（PK 去重，source 不变）
		t.Fatalf("InsertExt 后 topics = %d, want 2", len(got2[m.ID]))
	}

	// RemoveLink
	if err := r.RemoveLink(ctx, m.ID, tp2.ID); err != nil {
		t.Fatal(err)
	}
	got3, _ := r.ListByMemoryIDs(ctx, []ids.ID{m.ID})
	if len(got3[m.ID]) != 1 {
		t.Fatalf("RemoveLink 后 topics = %d, want 1", len(got3[m.ID]))
	}
}
