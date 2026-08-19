package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// setupTodoAPI 准备 todo 路由 + 一条 suggested 待办（来源 memory）。
// 标题用 "API 用例待办" 前缀，避免与其他包的 fixture 混淆。
func setupTodoAPI(t *testing.T) (http.Handler, *repo.TodoRepo, *repo.Todo) {
	t.Helper()
	_ = ids.Init(1)
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := &repo.TodoRepo{DB: db}
	mr := &repo.MemoryRepo{DB: db}
	ctx := context.Background()

	mem := &repo.Memory{Type: "event", Title: "API 用例待办来源", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Confidence: 0.9, SessionID: ids.New(), Status: "active"}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{mem}); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(24 * time.Hour)
	td := &repo.Todo{Title: "API 用例待办：给 Tom 发邮件", SourceMemoryID: &mem.ID,
		Status: "suggested", Confidence: 0.8, DueAt: &due}
	if err := tr.InsertExt(ctx, db, []*repo.Todo{td}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	RegisterTodo(r, &TodoHandler{Todos: tr})
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
