package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
)

func newTestMemory(sessionID, topicID ids.ID) *Memory {
	eventAt := time.Now()
	return &Memory{
		Type: "event", Title: "给 Tom 发邮件", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Importance: 0.6, Confidence: 0.9,
		TopicID: &topicID, SessionID: sessionID, TranscriptSegmentIDs: ids.List{1, 2},
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

	sid := ids.New()
	m := newTestMemory(sid, topic.ID)
	// 必须传 *Memory 指针切片，ID 才能回填到调用方的 m。
	if err := mr.InsertExt(ctx, db, []*Memory{m}); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("InsertExt 未回填 ID")
	}

	// 按 session 查询（联查 topic 名称）
	rows, err := mr.ListBySession(ctx, sid)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListBySession: %v len=%d", err, len(rows))
	}
	if rows[0].TopicName == nil || *rows[0].TopicName != "工作" {
		t.Fatalf("topic_name = %v", rows[0].TopicName)
	}
	if len(rows[0].TranscriptSegmentIDs) != 2 {
		t.Fatalf("segment_ids = %v", rows[0].TranscriptSegmentIDs)
	}

	// Get
	got, err := mr.Get(ctx, m.ID)
	if err != nil || got.Title != "给 Tom 发邮件" {
		t.Fatalf("Get: %v %+v", err, got)
	}

	// Save：改内容 version+1
	got.Content = "后天给 Tom 发邮件确认设计稿"
	got.Version++
	if err := mr.Save(ctx, got); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got2, _ := mr.Get(ctx, m.ID)
	if got2.Version != 2 || got2.Content != "后天给 Tom 发邮件确认设计稿" {
		t.Fatalf("after save: %+v", got2)
	}

	// 过滤列表：type 过滤命中，错误 type 无结果
	if rows, _ = mr.List(ctx, MemoryFilter{Type: "event", Limit: 10}); len(rows) < 1 {
		t.Fatal("type=event 应命中")
	}
	if rows, _ = mr.List(ctx, MemoryFilter{Type: "idea", Limit: 10}); len(rows) != 0 {
		t.Fatal("type=idea 不应命中")
	}

	// dismissed 不出现在列表；offset 超界返回空
	dm := newTestMemory(sid, topic.ID)
	dm.Status = "dismissed"
	if err := mr.InsertExt(ctx, db, []*Memory{dm}); err != nil {
		t.Fatalf("InsertExt dismissed: %v", err)
	}
	if rows, _ = mr.ListBySession(ctx, sid); len(rows) != 1 {
		t.Fatalf("dismissed 应被 ListBySession 排除，got %d", len(rows))
	}
	if rows, _ = mr.List(ctx, MemoryFilter{Limit: 10, Offset: 9999}); len(rows) != 0 {
		t.Fatalf("offset 越界应返回空，got %d", len(rows))
	}
}

func TestMemoryDeleteBySession(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()

	sid := ids.New()
	m := newTestMemory(sid, 1)
	m.TopicID = nil
	_ = mr.InsertExt(ctx, db, []*Memory{m})
	if err := mr.DeleteBySessionExt(ctx, db, sid); err != nil {
		t.Fatalf("DeleteBySessionExt: %v", err)
	}
	if rows, _ := mr.ListBySession(ctx, sid); len(rows) != 0 {
		t.Fatalf("删除后仍有 %d 条", len(rows))
	}
}
