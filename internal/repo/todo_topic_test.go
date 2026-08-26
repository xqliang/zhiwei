package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestTodoTopicRepo(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &TodoTopicRepo{DB: db}

	sid := ids.New()
	(&SessionRepo{DB: db}).Create(ctx, repoSessionFix(t, sid))
	mr := &MemoryRepo{DB: db}
	tr := &TodoRepo{DB: db}
	tp := &TopicRepo{DB: db}

	m := &Memory{Type: "fact", Title: "t", Content: "足够长的内容描述",
		EpistemicType: "observed", Confidence: 0.9, SessionID: sid}
	mr.InsertExt(ctx, db, []*Memory{m})
	td := &Todo{Title: "td", SourceMemoryID: &m.ID, Status: "suggested", Confidence: 0.9}
	tr.InsertExt(ctx, db, []*Todo{td})
	t1 := &Topic{Name: "T1", Status: "active", CreatedBy: "user"}
	t2 := &Topic{Name: "T2", Status: "active", CreatedBy: "user"}
	tp.Create(ctx, t1)
	tp.Create(ctx, t2)

	r.AddLink(ctx, td.ID, t1.ID)
	r.AddLink(ctx, td.ID, t1.ID) // 幂等
	r.AddLink(ctx, td.ID, t2.ID)

	got, _ := r.ListByTodoIDs(ctx, []ids.ID{td.ID})
	if len(got[td.ID]) != 2 {
		t.Fatalf("topics = %d, want 2", len(got[td.ID]))
	}

	// DeleteBySessionExt 只影响本 session
	if err := r.DeleteBySessionExt(ctx, db, sid); err != nil {
		t.Fatal(err)
	}
	got2, _ := r.ListByTodoIDs(ctx, []ids.ID{td.ID})
	if len(got2[td.ID]) != 0 {
		t.Fatalf("DeleteBySessionExt 后 topics = %d, want 0", len(got2[td.ID]))
	}
}

// TestTodoTopicSnapshotUser 直接单测 SnapshotUserBySessionExt：
// 预置 session + memory（带 transcript_segment_ids + title）+ todo（source_memory_id 指向 memory）
// + topic，AddLink 一条 user 关联到 todo，断言快照返回行带 TopicID/SegmentIDs/Title
// （segment_ids+title 取自 source memory）；再加一条 ai 关联（不同 topic），断言快照只返 user。
// 守护 commitExtract 删旧前抓 todo 的 user 行这条路径（spec §6）。
func TestTodoTopicSnapshotUser(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &TodoTopicRepo{DB: db}
	mr := &MemoryRepo{DB: db}
	tr := &TodoRepo{DB: db}
	tp := &TopicRepo{DB: db}

	sid := ids.New()
	// 跨包隔离：本用例建的 event memory 挂在 sid 上（未显式给 status，插入的是空串 ''，
	// 仍被 List 的 status!='dismissed' 计入），收尾按 session 删净——否则污染 api 包
	// TestMemoryListAndFilter 的 type=event 计数（repo→api 逆序跑才暴露）。
	t.Cleanup(func() {
		_, _ = mr.DB.ExecContext(context.Background(), `DELETE FROM memory WHERE session_id = ?`, sid.Int64())
	})
	(&SessionRepo{DB: db}).Create(ctx, repoSessionFix(t, sid))

	// memory 带 segment_ids + title（快照行经 source_memory_id join 取这些列）
	segA, segB := ids.New(), ids.New()
	m := &Memory{
		Type: "event", Title: "给 Tom 发邮件", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Confidence: 0.9, SessionID: sid,
		TranscriptSegmentIDs: ids.List{segA, segB},
	}
	if err := mr.InsertExt(ctx, db, []*Memory{m}); err != nil {
		t.Fatal(err)
	}
	td := &Todo{Title: "给 Tom 发邮件", SourceMemoryID: &m.ID, Status: "suggested", Confidence: 0.9}
	if err := tr.InsertExt(ctx, db, []*Todo{td}); err != nil {
		t.Fatal(err)
	}
	topic := &Topic{Name: "T待办快照", Status: "active", CreatedBy: "user"}
	if err := tp.Create(ctx, topic); err != nil {
		t.Fatal(err)
	}

	// 一条 user 关联到 todo
	if err := r.AddLink(ctx, td.ID, topic.ID); err != nil {
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
		t.Fatalf("SegmentIDs = %v, want 2 个（取自 source memory）", got.SegmentIDs)
	}
	if got.Title != "给 Tom 发邮件" {
		t.Fatalf("Title = %q, want 给 Tom 发邮件（取自 source memory）", got.Title)
	}

	// 再加一条 ai 关联（不同 topic，避免 PK 冲突被 IGNORE），快照仍只返 user
	topicAI := &Topic{Name: "T待办快照AI", Status: "active", CreatedBy: "ai"}
	if err := tp.Create(ctx, topicAI); err != nil {
		t.Fatal(err)
	}
	if err := r.InsertExt(ctx, db, []*TodoTopicLink{
		{TodoID: td.ID, TopicID: topicAI.ID, Source: "ai"},
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

// repoSessionFix 构造一个最小可用 AudioSession（仅测试用）。
func repoSessionFix(t *testing.T, id ids.ID) *AudioSession {
	t.Helper()
	return &AudioSession{ID: id, Source: "web_upload", Filename: "x.wav", StoragePath: "/tmp/x.wav", Status: "processing"}
}
