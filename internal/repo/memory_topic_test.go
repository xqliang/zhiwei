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
		EpistemicType: "observed", Confidence: 0.9, SessionID: idPtr(ids.New())}
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

// TestMemoryTopicSnapshotUser 直接单测 SnapshotUserBySessionExt：
// 预置 session + memory（带 transcript_segment_ids + title，自然键成分）+ topic，
// AddLink 一条 user 关联，调快照断言返回 1 行且 TopicID/SegmentIDs/Title 正确；
// 再加一条 ai 关联（不同 topic），断言快照仍只返回 user 行（source 过滤）。
// 守护 commitExtract 删旧前抓 user 行成 map 这条路径（spec §6）。
func TestMemoryTopicSnapshotUser(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &MemoryTopicRepo{DB: db}
	mr := &MemoryRepo{DB: db}
	tp := &TopicRepo{DB: db}

	sid := ids.New()
	(&SessionRepo{DB: db}).Create(ctx, repoSessionFix(t, sid))

	// memory 带 transcript_segment_ids + title（快照行的自然键成分）
	segA, segB := ids.New(), ids.New()
	m := &Memory{
		Type: "fact", Title: "给 Tom 发邮件", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Confidence: 0.9, SessionID: &sid,
		TranscriptSegmentIDs: ids.List{segA, segB},
	}
	if err := mr.InsertExt(ctx, db, []*Memory{m}); err != nil {
		t.Fatal(err)
	}
	topic := &Topic{Name: "T快照", Status: "active", CreatedBy: "user"}
	if err := tp.Create(ctx, topic); err != nil {
		t.Fatal(err)
	}

	// 一条 user 关联
	if err := r.AddLink(ctx, m.ID, topic.ID); err != nil {
		t.Fatal(err)
	}

	rows, err := r.SnapshotUserBySessionExt(ctx, db, sid)
	if err != nil {
		t.Fatalf("SnapshotUserBySessionExt: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("快照行数 = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.TopicID != topic.ID {
		t.Fatalf("TopicID = %s, want %s", got.TopicID, topic.ID)
	}
	if len(got.SegmentIDs) != 2 {
		t.Fatalf("SegmentIDs = %v, want 2 个", got.SegmentIDs)
	}
	if got.Title != "给 Tom 发邮件" {
		t.Fatalf("Title = %q, want 给 Tom 发邮件", got.Title)
	}

	// 再加一条 ai 关联（不同 topic，避免 PK 冲突被 IGNORE），快照仍只返 user（source 过滤）
	topicAI := &Topic{Name: "T快照AI", Status: "active", CreatedBy: "ai"}
	if err := tp.Create(ctx, topicAI); err != nil {
		t.Fatal(err)
	}
	if err := r.InsertExt(ctx, db, []*MemoryTopicLink{
		{MemoryID: m.ID, TopicID: topicAI.ID, Source: "ai"},
	}); err != nil {
		t.Fatal(err)
	}
	rows2, _ := r.SnapshotUserBySessionExt(ctx, db, sid)
	if len(rows2) != 1 {
		t.Fatalf("加 ai 关联后快照行数 = %d, want 1（只返 user）", len(rows2))
	}
	if rows2[0].TopicID != topic.ID {
		t.Fatalf("快照行 TopicID = %s, want user topic %s", rows2[0].TopicID, topic.ID)
	}
}
