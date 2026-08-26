package api

// 多租户越权回归测试（评审 M4）：这是防止 C2/I2 类隔离缺口回潮的关键护栏。
// 统一以 newAuthedRouter() 注入登录用户 1（uid=1），另造 user_id=2 的数据行（直接用
// repo Insert/Create，令 UserID=2 而非默认 1），断言 user1 经 HTTP handler：
//   - 列表不返回 user2 的行（C2：GET /api/sessions；memory List 回归）；
//   - 删除 user2 的行 → 404 且该行仍在（I2：topic/todo Delete 归属校验，不泄漏存在性）。
// 共享测试库脏数据靠「按 id 精确断言 + t.Cleanup 清 user2/user1 造的行」两手消解。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestSessionListIsolation 验证 C2：库里有 user2 的 session，登录 user1 请求
// GET /api/sessions → 列表含 user1 自己的 session、不含 user2 的 session。
// 先造 user2 行再造 user1 行，使 user1 的 id 最大（ORDER BY s.id DESC 稳居首），
// 正向断言不受默认 LIMIT 50 与脏库影响。
func TestSessionListIsolation(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	memories := &repo.MemoryRepo{DB: db}
	todos := &repo.TodoRepo{DB: db}

	// user2 的 session（UserID=2，绕过默认 1）
	sid2 := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid2, UserID: 2, Source: "web_upload", Filename: "u2.wav",
		StoragePath: "/tmp/u2.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	// user1 的 session（UserID 默认 1），后造 → id 更大，DESC 排序稳居前列
	sid1 := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid1, Source: "web_upload", Filename: "u1.wav",
		StoragePath: "/tmp/u1.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM audio_session WHERE id IN (?, ?)`, sid1.Int64(), sid2.Int64())
	})

	r := newAuthedRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts, Memories: memories, Todos: todos,
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	have := map[string]bool{}
	for _, s := range resp.Sessions {
		have[s.ID] = true
	}
	if !have[sid1.String()] {
		t.Fatalf("user1 应看到自己的 session %s: %s", sid1, rec.Body.String())
	}
	if have[sid2.String()] {
		t.Fatalf("越权：user1 的列表不应含 user2 的 session %s", sid2)
	}
}

// TestMemoryListIsolation 验证 memory List 已隔离的回归：user1 List 不含 user2 的记忆。
// 造一个 topic 并把 user1/user2 各一条记忆挂上去，用 ?topic_id= 过滤把候选收窄到这两条
// （规避默认 LIMIT 50 + 脏库），断言 user1 只看到自己那条。
func TestMemoryListIsolation(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mr := &repo.MemoryRepo{DB: db}
	tr := &repo.TopicRepo{DB: db}
	mtr := &repo.MemoryTopicRepo{DB: db}

	tp := &repo.Topic{Name: "越权用例主题-" + ids.New().String(), Status: "active", CreatedBy: "user"}
	if err := tr.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	eventAt := time.Now()
	mem1 := &repo.Memory{Type: "fact", Title: "越权用例-user1记忆", Content: "user1 的记忆内容",
		EpistemicType: "observed", Confidence: 0.9, SessionID: idPtr(ids.New()), EventAt: &eventAt, Status: "active"}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{mem1}); err != nil {
		t.Fatal(err)
	}
	mem2 := &repo.Memory{UserID: 2, Type: "fact", Title: "越权用例-user2记忆", Content: "user2 的记忆内容",
		EpistemicType: "observed", Confidence: 0.9, SessionID: idPtr(ids.New()), EventAt: &eventAt, Status: "active"}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{mem2}); err != nil {
		t.Fatal(err)
	}
	if err := mtr.AddLink(ctx, mem1.ID, tp.ID); err != nil {
		t.Fatal(err)
	}
	if err := mtr.AddLink(ctx, mem2.ID, tp.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.ExecContext(bg, `DELETE FROM memory_topic WHERE topic_id = ?`, tp.ID.Int64())
		_, _ = db.ExecContext(bg, `DELETE FROM memory WHERE id IN (?, ?)`, mem1.ID.Int64(), mem2.ID.Int64())
		_, _ = db.ExecContext(bg, `DELETE FROM topic WHERE id = ?`, tp.ID.Int64())
	})

	r := newAuthedRouter()
	RegisterMemory(r, &MemoryHandler{Memories: mr, Topics: tr, MemoryTopics: mtr})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/memories?topic_id="+tp.ID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Memories []struct {
			ID string `json:"id"`
		} `json:"memories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	have := map[string]bool{}
	for _, m := range resp.Memories {
		have[m.ID] = true
	}
	if !have[mem1.ID.String()] {
		t.Fatalf("user1 应看到自己的记忆 %s: %s", mem1.ID, rec.Body.String())
	}
	if have[mem2.ID.String()] {
		t.Fatalf("越权：user1 的列表不应含 user2 的记忆 %s", mem2.ID)
	}
}

// TestTopicDeleteIsolation 验证 I2：user1 删 user2 的 topic → 404，且 user2 的 topic 仍在。
// 不泄漏存在性（返回 404 而非 403），归属校验拦在 repo 越权删除之前。
func TestTopicDeleteIsolation(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr := &repo.TopicRepo{DB: db}
	mr := &repo.MemoryRepo{DB: db}
	tdr := &repo.TodoRepo{DB: db}

	// user2 的 topic（UserID=2）
	tp2 := &repo.Topic{UserID: 2, Name: "越权用例-user2主题-" + ids.New().String(), Status: "active", CreatedBy: "user"}
	if err := tr.Create(ctx, tp2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM topic WHERE id = ?`, tp2.ID.Int64())
	})

	r := newAuthedRouter() // 登录 user1
	RegisterTopic(r, &TopicHandler{Topics: tr, Memories: mr, Todos: tdr})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/topics/"+tp2.ID.String(), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("user1 删 user2 的 topic 应 404, got %d %s", rec.Code, rec.Body.String())
	}
	// user2 的 topic 仍在（未被越权删）
	if _, err := tr.Get(ctx, 2, tp2.ID); err != nil {
		t.Fatalf("user2 的 topic 不应被删除: %v", err)
	}
}

// TestTodoDeleteIsolation 验证 I2：user1 删 user2 的 todo → 404，且 user2 的 todo 仍在。
func TestTodoDeleteIsolation(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tdr := &repo.TodoRepo{DB: db}
	topics := &repo.TopicRepo{DB: db}
	todoTopics := &repo.TodoTopicRepo{DB: db}

	// user2 的 todo（UserID=2；source_memory_id 可空，直接置 nil）
	td2 := &repo.Todo{UserID: 2, Title: "越权用例-user2待办-" + ids.New().String(),
		Status: "suggested", Confidence: 0.8}
	if err := tdr.InsertExt(ctx, db, []*repo.Todo{td2}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM todo WHERE id = ?`, td2.ID.Int64())
	})

	r := newAuthedRouter() // 登录 user1
	RegisterTodo(r, &TodoHandler{Todos: tdr, TodoTopics: todoTopics, Topics: topics})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/todos/"+td2.ID.String(), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("user1 删 user2 的 todo 应 404, got %d %s", rec.Code, rec.Body.String())
	}
	// user2 的 todo 仍在（未被越权删）
	if _, err := tdr.Get(ctx, 2, td2.ID); err != nil {
		t.Fatalf("user2 的 todo 不应被删除: %v", err)
	}
}
