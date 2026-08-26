package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

// idPtr 把 ids.ID 值转成 *ids.ID 指针（测试构造 Memory{SessionID:...} 用）。
// SessionID 改指针后，内联 ids.New() 无法直接取址（&ids.New() 非法），用本 helper 桥接。
func idPtr(id ids.ID) *ids.ID { return &id }

// —— 以下为对话转记忆 repo 层集成测试（Task 6.1）——
// 门禁 TEST_MYSQL_DSN；插入共享表须 t.Cleanup 按 conversation_id/session_id 清理。

// TestInsertConversationRoundTrip 验证对话记忆往返：
// conversation_id 落库、session_id 为 NULL（可空 session_id 往返关键断言），
// 且 safe-mode SELECT * 不因 conversation_id 列报 missing destination。
func TestInsertConversationRoundTrip(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mr := &MemoryRepo{DB: db}

	convID := ids.New()
	t.Cleanup(func() { _ = mr.DeleteByConversationExt(context.Background(), db, convID) })

	ms := []*Memory{{
		Type: "fact", Title: "会话记忆X9Z", Content: "对话里用户提到的独特事实X9Z",
		EpistemicType: "observed", Status: "active", Importance: 0.6, Confidence: 0.8,
	}}
	if err := mr.InsertConversationExt(ctx, db, convID, ms); err != nil {
		t.Fatalf("InsertConversationExt: %v", err)
	}

	// Get 走 SELECT *（safe 模式）：能取回即证明 conversation_id 列有对应结构体字段
	got, err := mr.Get(ctx, 1, ms[0].ID)
	if err != nil {
		t.Fatalf("Get(safe-mode SELECT *): %v", err)
	}
	if got.SessionID != nil {
		t.Errorf("session_id 应为 NULL, got %v", *got.SessionID)
	}
	if got.ConversationID == nil || *got.ConversationID != convID {
		t.Errorf("conversation_id 未落库: %v", got.ConversationID)
	}

	// Search / ListActive / List 也走 SELECT *，回归 safe-mode：应能命中且不报错
	if rows, err := mr.Search(ctx, 1, "X9Z", "", 20); err != nil {
		t.Fatalf("Search(safe-mode): %v", err)
	} else {
		var hit bool
		for _, m := range rows {
			if m.ID == ms[0].ID {
				hit = true
			}
		}
		if !hit {
			t.Errorf("Search 未命中对话记忆")
		}
	}
	if _, err := mr.ListActive(ctx, 1, 50); err != nil {
		t.Fatalf("ListActive(safe-mode): %v", err)
	}
	if _, err := mr.List(ctx, MemoryFilter{UserID: 1, Type: "fact", Limit: 50}); err != nil {
		t.Fatalf("List(safe-mode): %v", err)
	}
}

// TestDeleteByConversationIdempotent 验证按会话幂等删除：
// 插 2 条对话记忆 + 各挂一个 memory_topic，先删关联再删主表后均空；重复删不报错。
func TestDeleteByConversationIdempotent(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mr := &MemoryRepo{DB: db}
	mtr := &MemoryTopicRepo{DB: db}
	tr := &TopicRepo{DB: db}

	convID := ids.New()
	t.Cleanup(func() {
		_ = mtr.DeleteByConversationExt(context.Background(), db, convID)
		_ = mr.DeleteByConversationExt(context.Background(), db, convID)
	})

	ms := []*Memory{
		{Type: "fact", Title: "会话删除用例A", Content: "对话记忆内容A足够长", EpistemicType: "observed", Status: "active", Confidence: 0.8},
		{Type: "fact", Title: "会话删除用例B", Content: "对话记忆内容B足够长", EpistemicType: "observed", Status: "active", Confidence: 0.8},
	}
	if err := mr.InsertConversationExt(ctx, db, convID, ms); err != nil {
		t.Fatalf("InsertConversationExt: %v", err)
	}
	tp := &Topic{Name: "会话删除用例主题", Status: "active", CreatedBy: "ai"}
	if err := tr.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Delete(context.Background(), tp.ID) })
	if err := mtr.InsertExt(ctx, db, []*MemoryTopicLink{
		{MemoryID: ms[0].ID, TopicID: tp.ID, Source: "ai"},
		{MemoryID: ms[1].ID, TopicID: tp.ID, Source: "ai"},
	}); err != nil {
		t.Fatalf("InsertExt links: %v", err)
	}

	// 先删关联（子查 memory、删 memory_topic，不同表合法）再删主表
	if err := mtr.DeleteByConversationExt(ctx, db, convID); err != nil {
		t.Fatalf("MemoryTopics.DeleteByConversationExt: %v", err)
	}
	if err := mr.DeleteByConversationExt(ctx, db, convID); err != nil {
		t.Fatalf("Memories.DeleteByConversationExt: %v", err)
	}
	// 关联应清空
	links, _ := mtr.ListByMemoryIDs(ctx, []ids.ID{ms[0].ID, ms[1].ID})
	if len(links) != 0 {
		t.Fatalf("删除后仍有关联: %v", links)
	}
	// 主表应清空（按 conversation_id 查不到——用 Get 探测第一条应 ErrNoRows）
	if _, err := mr.Get(ctx, 1, ms[0].ID); err == nil {
		t.Fatalf("删除后仍能 Get 到 memory")
	}

	// 幂等：重复删不报错
	if err := mtr.DeleteByConversationExt(ctx, db, convID); err != nil {
		t.Fatalf("重复删关联报错: %v", err)
	}
	if err := mr.DeleteByConversationExt(ctx, db, convID); err != nil {
		t.Fatalf("重复删主表报错: %v", err)
	}
}

// TestSessionPathRegression 验证录音路径不回归：
// InsertExt 传 SessionID=&sid 后 Get，session_id 非空且等于 sid、conversation_id 为 NULL。
func TestSessionPathRegression(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mr := &MemoryRepo{DB: db}

	sid := ids.New()
	t.Cleanup(func() { _ = mr.DeleteBySessionExt(context.Background(), db, sid) })

	ms := []*Memory{{
		Type: "fact", Title: "录音路径回归用例", Content: "录音抽取来源记忆内容足够长",
		EpistemicType: "observed", Status: "active", Importance: 0.6, Confidence: 0.8,
		SessionID: &sid,
	}}
	if err := mr.InsertExt(ctx, db, ms); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}
	got, err := mr.Get(ctx, 1, ms[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SessionID == nil || *got.SessionID != sid {
		t.Errorf("session_id 应为 %v, got %v", sid, got.SessionID)
	}
	if got.ConversationID != nil {
		t.Errorf("录音来源 conversation_id 应为 NULL, got %v", *got.ConversationID)
	}
}
