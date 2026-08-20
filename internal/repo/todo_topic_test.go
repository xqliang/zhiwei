package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestTodoTopicRepo(t *testing.T) {
	db, err := NewDB(TestDSN(t))
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

// repoSessionFix 构造一个最小可用 AudioSession（仅测试用）。
func repoSessionFix(t *testing.T, id ids.ID) *AudioSession {
	t.Helper()
	return &AudioSession{ID: id, Source: "web_upload", Filename: "x.wav", StoragePath: "/tmp/x.wav", Status: "processing"}
}
