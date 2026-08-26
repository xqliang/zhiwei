package repo

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"zhiwei/internal/ids"
)

// 本文件是多租户越权读隔离的回归用例（安全修复）。
// 对每个加了 user_id 强制过滤的读方法，造 user=u1 与 user=u2 各一行（同表），断言：
//   - Get(ctx, u1, idOf2) 未命中（越权不可读）；Get(ctx, u1, idOf1) 命中。
//   - List/ListByConversation(ctx, u1, …) 只含 u1 的行、不含 u2。
// 门禁 TEST_MYSQL_DSN；用每次运行唯一的 user_id（int64(ids.New())）隔离，t.Cleanup 按 user_id 清库。
//
// 未命中语义按各方法既有约定断言：
//   - Session/Memory/Todo/Topic/AgentConversation.Get 冒泡 sql.ErrNoRows（handler 转 404）；
//   - Person.Get 返回 (nil, nil)。
// 两者都表示「读不到」，是本安全修复的核心保证。

// twoUsers 返回两个每次运行唯一、互不相同的 user_id，用于隔离断言。
func twoUsers() (int64, int64) {
	return int64(ids.New()), int64(ids.New())
}

func TestIsolationSessionGetList(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &SessionRepo{DB: db}
	ctx := context.Background()
	u1, u2 := twoUsers()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM audio_session WHERE user_id IN (?, ?)`, u1, u2)
	})

	id1, id2 := ids.New(), ids.New()
	s1 := newTestSession(id1)
	s1.UserID = u1
	s2 := newTestSession(id2)
	s2.UserID = u2
	if err := r.Create(ctx, s1); err != nil {
		t.Fatalf("Create s1: %v", err)
	}
	if err := r.Create(ctx, s2); err != nil {
		t.Fatalf("Create s2: %v", err)
	}

	// 命中：u1 读自己的行
	if got, err := r.Get(ctx, u1, id1); err != nil || got.ID != id1 {
		t.Fatalf("owner Get 应命中: err=%v got=%+v", err, got)
	}
	// 越权：u1 读 u2 的行 → sql.ErrNoRows
	if _, err := r.Get(ctx, u1, id2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("越权 Get 应返回 sql.ErrNoRows, got %v", err)
	}

	// List 隔离：u1 只见 id1，不见 id2
	list, err := r.List(ctx, u1, 200, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var see1, see2 bool
	for _, s := range list {
		if s.ID == id1 {
			see1 = true
		}
		if s.ID == id2 {
			see2 = true
		}
	}
	if !see1 || see2 {
		t.Fatalf("List 隔离失败: 见 id1=%v 见 id2=%v（期望 true/false）", see1, see2)
	}
}

func TestIsolationMemoryGetList(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()
	u1, u2 := twoUsers()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM memory WHERE user_id IN (?, ?)`, u1, u2)
	})

	m1 := newTestMemory(ids.New())
	m1.UserID = u1
	m2 := newTestMemory(ids.New())
	m2.UserID = u2
	if err := mr.InsertExt(ctx, db, []*Memory{m1}); err != nil {
		t.Fatalf("InsertExt m1: %v", err)
	}
	if err := mr.InsertExt(ctx, db, []*Memory{m2}); err != nil {
		t.Fatalf("InsertExt m2: %v", err)
	}

	// 命中 / 越权
	if got, err := mr.Get(ctx, u1, m1.ID); err != nil || got.ID != m1.ID {
		t.Fatalf("owner Get 应命中: err=%v got=%+v", err, got)
	}
	if _, err := mr.Get(ctx, u1, m2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("越权 Get 应返回 sql.ErrNoRows, got %v", err)
	}

	// List 隔离：MemoryFilter.UserID=u1 只含 m1、不含 m2
	rows, err := mr.List(ctx, MemoryFilter{UserID: u1, Limit: 200})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var see1, see2 bool
	for _, m := range rows {
		if m.ID == m1.ID {
			see1 = true
		}
		if m.ID == m2.ID {
			see2 = true
		}
	}
	if !see1 || see2 {
		t.Fatalf("List 隔离失败: 见 m1=%v 见 m2=%v（期望 true/false）", see1, see2)
	}

	// 安全默认：UserID=0 视为未指定用户，List 直接返回空（不全表泄漏）
	if rows0, err := mr.List(ctx, MemoryFilter{Limit: 200}); err != nil || rows0 != nil {
		t.Fatalf("UserID=0 应返回 (nil, nil), got rows=%v err=%v", rows0, err)
	}
}

func TestIsolationTodoGetListDismissed(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := &TodoRepo{DB: db}
	ctx := context.Background()
	u1, u2 := twoUsers()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM todo WHERE user_id IN (?, ?)`, u1, u2)
	})

	// 每个用户各一条 confirmed（进 List）+ 一条 dismissed（进 ListDismissed）
	td1 := &Todo{UserID: u1, Title: "iso-todo-u1", Status: "confirmed", Confidence: 0.9}
	td1d := &Todo{UserID: u1, Title: "iso-todo-u1-d", Status: "dismissed", Confidence: 0.9}
	td2 := &Todo{UserID: u2, Title: "iso-todo-u2", Status: "confirmed", Confidence: 0.9}
	td2d := &Todo{UserID: u2, Title: "iso-todo-u2-d", Status: "dismissed", Confidence: 0.9}
	if err := tr.InsertExt(ctx, db, []*Todo{td1, td1d, td2, td2d}); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}

	// 命中 / 越权
	if got, err := tr.Get(ctx, u1, td1.ID); err != nil || got.ID != td1.ID {
		t.Fatalf("owner Get 应命中: err=%v got=%+v", err, got)
	}
	if _, err := tr.Get(ctx, u1, td2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("越权 Get 应返回 sql.ErrNoRows, got %v", err)
	}

	// List(u1)：含 td1，不含 td2/td1d/td2d
	rows, err := tr.List(ctx, u1, "", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[ids.ID]bool{}
	for _, x := range rows {
		seen[x.ID] = true
	}
	if !seen[td1.ID] || seen[td2.ID] || seen[td1d.ID] || seen[td2d.ID] {
		t.Fatalf("List 隔离失败: td1=%v td2=%v td1d=%v td2d=%v（期望 T/F/F/F）",
			seen[td1.ID], seen[td2.ID], seen[td1d.ID], seen[td2d.ID])
	}

	// ListDismissed(u1)：含 td1d，不含 td2d/td1
	drows, err := tr.ListDismissed(ctx, u1)
	if err != nil {
		t.Fatalf("ListDismissed: %v", err)
	}
	dseen := map[ids.ID]bool{}
	for _, x := range drows {
		dseen[x.ID] = true
	}
	if !dseen[td1d.ID] || dseen[td2d.ID] || dseen[td1.ID] {
		t.Fatalf("ListDismissed 隔离失败: td1d=%v td2d=%v td1=%v（期望 T/F/F）",
			dseen[td1d.ID], dseen[td2d.ID], dseen[td1.ID])
	}
}

func TestIsolationTopicGet(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &TopicRepo{DB: db}
	ctx := context.Background()
	u1, u2 := twoUsers()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM topic WHERE user_id IN (?, ?)`, u1, u2)
	})

	tp1 := &Topic{UserID: u1, Name: "iso-topic-u1", Status: "active", CreatedBy: "user"}
	tp2 := &Topic{UserID: u2, Name: "iso-topic-u2", Status: "active", CreatedBy: "user"}
	if err := r.Create(ctx, tp1); err != nil {
		t.Fatalf("Create tp1: %v", err)
	}
	if err := r.Create(ctx, tp2); err != nil {
		t.Fatalf("Create tp2: %v", err)
	}

	if got, err := r.Get(ctx, u1, tp1.ID); err != nil || got.ID != tp1.ID {
		t.Fatalf("owner Get 应命中: err=%v got=%+v", err, got)
	}
	if _, err := r.Get(ctx, u1, tp2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("越权 Get 应返回 sql.ErrNoRows, got %v", err)
	}
}

func TestIsolationAgentConversationGet(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &AgentConversationRepo{DB: db}
	ctx := context.Background()
	u1, u2 := twoUsers()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM agent_conversation WHERE user_id IN (?, ?)`, u1, u2)
	})

	c1 := &AgentConversation{UserID: u1, Title: "iso-conv-u1"}
	c2 := &AgentConversation{UserID: u2, Title: "iso-conv-u2"}
	if err := r.Create(ctx, c1); err != nil {
		t.Fatalf("Create c1: %v", err)
	}
	if err := r.Create(ctx, c2); err != nil {
		t.Fatalf("Create c2: %v", err)
	}

	if got, err := r.Get(ctx, u1, c1.ID); err != nil || got.ID != c1.ID {
		t.Fatalf("owner Get 应命中: err=%v got=%+v", err, got)
	}
	if _, err := r.Get(ctx, u1, c2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("越权 Get 应返回 sql.ErrNoRows, got %v", err)
	}
}

func TestIsolationAgentMessageListByConversation(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cr := &AgentConversationRepo{DB: db}
	mr := &AgentMessageRepo{DB: db}
	ctx := context.Background()
	u1, u2 := twoUsers()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM agent_message WHERE user_id IN (?, ?)`, u1, u2)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM agent_conversation WHERE user_id IN (?, ?)`, u1, u2)
	})

	c1 := &AgentConversation{UserID: u1, Title: "iso"}
	c2 := &AgentConversation{UserID: u2, Title: "iso"}
	if err := cr.Create(ctx, c1); err != nil {
		t.Fatalf("Create c1: %v", err)
	}
	if err := cr.Create(ctx, c2); err != nil {
		t.Fatalf("Create c2: %v", err)
	}
	if err := mr.Append(ctx, &AgentMessage{UserID: u1, ConversationID: &c1.ID, Role: "user", Content: "a"}); err != nil {
		t.Fatalf("Append u1: %v", err)
	}
	if err := mr.Append(ctx, &AgentMessage{UserID: u2, ConversationID: &c2.ID, Role: "user", Content: "b"}); err != nil {
		t.Fatalf("Append u2: %v", err)
	}

	// 命中：u1 读自己会话的消息
	if list, err := mr.ListByConversation(ctx, u1, c1.ID); err != nil || len(list) != 1 {
		t.Fatalf("owner ListByConversation 应有 1 条: err=%v len=%d", err, len(list))
	}
	// 越权：即使拿到 u2 的 convID，u1 也读不到 u2 会话的消息（user_id 不匹配 → 空列表）
	if list, err := mr.ListByConversation(ctx, u1, c2.ID); err != nil || len(list) != 0 {
		t.Fatalf("越权 ListByConversation 应为空: err=%v len=%d", err, len(list))
	}
}

func TestIsolationPersonGet(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	persons := &PersonRepo{DB: db}
	ctx := context.Background()
	u1, u2 := twoUsers()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM person WHERE user_id IN (?, ?)`, u1, u2)
	})

	p1 := &Person{UserID: u1, DisplayName: "iso-person-u1"}
	p2 := &Person{UserID: u2, DisplayName: "iso-person-u2"}
	if err := persons.Create(ctx, p1); err != nil {
		t.Fatalf("Create p1: %v", err)
	}
	if err := persons.Create(ctx, p2); err != nil {
		t.Fatalf("Create p2: %v", err)
	}

	// 命中：u1 读自己的人物
	if got, err := persons.Get(ctx, u1, p1.ID); err != nil || got == nil || got.ID != p1.ID {
		t.Fatalf("owner Get 应命中: err=%v got=%+v", err, got)
	}
	// 越权：u1 读 u2 的人物 → (nil, nil)（PersonRepo.Get 既有约定）
	if got, err := persons.Get(ctx, u1, p2.ID); got != nil || err != nil {
		t.Fatalf("越权 Get 应返回 (nil, nil), got=%+v err=%v", got, err)
	}
}
