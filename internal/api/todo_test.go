package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// setupTodoAPI 准备 todo 路由 + 一条 suggested 待办（来源 memory）。
// 标题用 "API 用例待办" 前缀，避免与其他包的 fixture 混淆。
func setupTodoAPI(t *testing.T) (http.Handler, *repo.TodoRepo, *repo.Todo) {
	t.Helper()
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := &repo.TodoRepo{DB: db}
	mr := &repo.MemoryRepo{DB: db}
	topics := &repo.TopicRepo{DB: db}
	todoTopics := &repo.TodoTopicRepo{DB: db}
	ctx := context.Background()

	mem := &repo.Memory{Type: "event", Title: "API 用例待办来源", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Confidence: 0.9, SessionID: idPtr(ids.New()), Status: "active"}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{mem}); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(24 * time.Hour)
	td := &repo.Todo{Title: "API 用例待办：给 Tom 发邮件", SourceMemoryID: &mem.ID,
		Status: "suggested", Confidence: 0.8, DueAt: &due}
	if err := tr.InsertExt(ctx, db, []*repo.Todo{td}); err != nil {
		t.Fatal(err)
	}

	r := newAuthedRouter()
	RegisterTodo(r, &TodoHandler{Todos: tr, TodoTopics: todoTopics, Topics: topics})
	return r, tr, td
}

func TestTodoList(t *testing.T) {
	r, _, _ := setupTodoAPI(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/todos?status=suggested", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"source_session_id"`) {
		t.Fatalf("响应应含 source_session_id: %s", rec.Body.String())
	}
	// 非法 status → 400
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/todos?status=bogus", nil))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("非法 status 应 400, got %d", rec2.Code)
	}
}

func TestTodoPatchTransitions(t *testing.T) {
	r, _, td := setupTodoAPI(t)
	patch := func(body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/api/todos/"+td.ID.String(),
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	// suggested → done 非法（必须先确认）
	if code := patch(`{"status":"done"}`); code != http.StatusConflict {
		t.Fatalf("suggested→done 应 409, got %d", code)
	}
	// suggested → confirmed
	if code := patch(`{"status":"confirmed"}`); code != http.StatusOK {
		t.Fatalf("confirm: %d", code)
	}
	// confirmed → done
	if code := patch(`{"status":"done"}`); code != http.StatusOK {
		t.Fatalf("done: %d", code)
	}
	// done → confirmed 非法
	if code := patch(`{"status":"confirmed"}`); code != http.StatusConflict {
		t.Fatalf("done→confirmed 应 409, got %d", code)
	}
	// 任意 → dismissed
	if code := patch(`{"status":"dismissed"}`); code != http.StatusOK {
		t.Fatalf("dismiss: %d", code)
	}
	// 不存在 → 404
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch,
		"/api/todos/"+ids.New().String(), strings.NewReader(`{"status":"done"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("404: got %d", rec.Code)
	}
	// 非法 status → 400（校验顺序须在流转检查之前）
	if code := patch(`{"status":"bogus"}`); code != http.StatusBadRequest {
		t.Fatalf("非法 status 应 400, got %d", code)
	}
}

// TestTodoAddRemoveTopic 验证手动加/删 todo↔topic 关联端点：
// POST 幂等（重复 200）、GET 列表反映增删、DELETE 204、不存在 topic_id → 404。
func TestTodoAddRemoveTopic(t *testing.T) {
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mr := &repo.MemoryRepo{DB: db}
	tr := &repo.TodoRepo{DB: db}
	topics := &repo.TopicRepo{DB: db}
	todoTopics := &repo.TodoTopicRepo{DB: db}

	// 建来源 memory + todo + topic
	mem := &repo.Memory{Type: "event", Title: "加删 topic 来源", Content: "描述内容描述",
		EpistemicType: "observed", Confidence: 0.9, SessionID: idPtr(ids.New()), Status: "active"}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{mem}); err != nil {
		t.Fatal(err)
	}
	td := &repo.Todo{Title: "加删 topic 用例待办", SourceMemoryID: &mem.ID,
		Status: "suggested", Confidence: 0.8}
	if err := tr.InsertExt(ctx, db, []*repo.Todo{td}); err != nil {
		t.Fatal(err)
	}
	tp := &repo.Topic{Name: "加删 topic 主题", Status: "active", CreatedBy: "user"}
	if err := topics.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}

	r := newAuthedRouter()
	RegisterTodo(r, &TodoHandler{Todos: tr, TodoTopics: todoTopics, Topics: topics})

	post := func(body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/todos/"+td.ID.String()+"/topics",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	body := `{"topic_id":"` + tp.ID.String() + `"}`

	// POST 加关联 → 200
	if code := post(body); code != http.StatusOK {
		t.Fatalf("add topic: %d", code)
	}
	// 重复 POST → 200（幂等：INSERT IGNORE）
	if code := post(body); code != http.StatusOK {
		t.Fatalf("idempotent add: %d", code)
	}
	// GET 列表 → todo.topics 反映新增
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/todos", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"加删 topic 主题"`) {
		t.Fatalf("列表应含已加 topic: %d %s", rec.Code, rec.Body.String())
	}
	// DELETE 移除关联 → 204
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodDelete,
		"/api/todos/"+td.ID.String()+"/topics/"+tp.ID.String(), nil))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("remove topic: %d", rec2.Code)
	}
	// GET 列表 → todo.topics 为空
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/todos", nil))
	if strings.Contains(rec3.Body.String(), `"加删 topic 主题"`) {
		t.Fatalf("移除后列表不应含 topic: %s", rec3.Body.String())
	}
	// POST 不存在 topic_id → 404
	if code := post(`{"topic_id":"` + ids.New().String() + `"}`); code != http.StatusNotFound {
		t.Fatalf("不存在 topic 应 404, got %d", code)
	}
	// POST 不存在 todo id → 404
	rec4 := httptest.NewRecorder()
	r.ServeHTTP(rec4, httptest.NewRequest(http.MethodPost, "/api/todos/"+ids.New().String()+"/topics",
		strings.NewReader(body)))
	if rec4.Code != http.StatusNotFound {
		t.Fatalf("不存在 todo 应 404, got %d", rec4.Code)
	}
}

// TestTodoEditTitle 验证 PATCH title（无 status）改名成功、状态不变。
func TestTodoEditTitle(t *testing.T) {
	r, tr, td := setupTodoAPI(t)
	ctx := context.Background()
	newTitle := "改名后的待办-" + td.ID.String()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/todos/"+td.ID.String(),
		strings.NewReader(`{"title":"`+newTitle+`"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit title: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := tr.Get(ctx, 1, td.ID)
	if got.Title != newTitle {
		t.Fatalf("title=%s, want %s", got.Title, newTitle)
	}
	if got.Status != td.Status { // title 改动不应碰状态
		t.Fatalf("status changed: %s -> %s", td.Status, got.Status)
	}
}

// TestTodoDelete 验证 DELETE 硬删 todo（不存在也不报错→204），重复删除幂等。
func TestTodoDelete(t *testing.T) {
	r, tr, td := setupTodoAPI(t)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/todos/"+td.ID.String(), nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, err := tr.Get(ctx, 1, td.ID); err == nil {
		t.Fatal("todo 仍存在")
	}
	// 归属校验后：删已不存在的 todo → 404（评审 I2：先 Get 校验归属/存在性，
	// 拿不到即 404，不再是幂等 204）
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodDelete, "/api/todos/"+td.ID.String(), nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("删已不存在应 404, got %d", rec2.Code)
	}
}

// TestTodoPatchTitleStatusBadTransition 验证 title+status 同 body 且 status 流转非法时
// 返回 409 且 title 未被持久化（先校验后变更，避免半成功）。
func TestTodoPatchTitleStatusBadTransition(t *testing.T) {
	r, tr, td := setupTodoAPI(t)
	ctx := context.Background()
	origTitle := td.Title
	// td 是 suggested；suggested→done 非法。同 body 发 title+status:done 应 409，title 不变。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/todos/"+td.ID.String(),
		strings.NewReader(`{"title":"应被回滚的名字","status":"done"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d %s", rec.Code, rec.Body.String())
	}
	got, _ := tr.Get(ctx, 1, td.ID)
	if got.Title != origTitle {
		t.Fatalf("title 被半成功持久化: %s, want %s", got.Title, origTitle)
	}
}
