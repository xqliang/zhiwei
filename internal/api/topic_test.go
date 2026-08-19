package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// setupTopicAPI 准备 topic 路由 + 一条 suggested 主题，
// 挂一条 memory 和一条 confirmed todo（验证计数与详情）。
// 名称统一用 "API用例主题" 前缀，避免与其他包的 fixture 混淆。
func setupTopicAPI(t *testing.T) (http.Handler, *repo.TopicRepo, *repo.MemoryRepo, *repo.TodoRepo, *repo.Topic) {
	t.Helper()
	_ = ids.Init(1)
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := &repo.TopicRepo{DB: db}
	mr := &repo.MemoryRepo{DB: db}
	tdr := &repo.TodoRepo{DB: db}
	ctx := context.Background()

	tp := &repo.Topic{Name: "API用例主题Rust", Status: "suggested", CreatedBy: "ai"}
	if err := tr.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	// 一条 memory + 一条 confirmed todo 挂上去（验证计数）
	eventAt := time.Now()
	if err := mr.InsertExt(ctx, db, []*repo.Memory{{
		Type: "fact", Title: "API用例记忆学Rust", Content: "用户正在学习 Rust 计划三个月读完一本书",
		EpistemicType: "observed", Confidence: 0.9, TopicID: &tp.ID,
		SessionID: ids.New(), EventAt: &eventAt, Status: "active",
	}}); err != nil {
		t.Fatal(err)
	}
	memRows, err := mr.ListByTopic(ctx, tp.ID)
	if err != nil || len(memRows) != 1 {
		t.Fatalf("fixture memory: %v %d", err, len(memRows))
	}
	if err := tdr.InsertExt(ctx, db, []*repo.Todo{{
		Title: "API用例待办读完Rust书", TopicID: &tp.ID, Status: "confirmed", Confidence: 0.9,
		SourceMemoryID: &memRows[0].ID,
	}}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	RegisterTopic(r, &TopicHandler{Topics: tr, Memories: mr, Todos: tdr})
	return r, tr, mr, tdr, tp
}

func TestTopicListWithCounts(t *testing.T) {
	r, _, _, _, tp := setupTopicAPI(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	// 解析 JSON 后按 ID 精确定位本主题的计数，
	// 避免脏库里其他 fixture 的行导致子串误判。
	var resp struct {
		Topics []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			MemoryCount   int    `json:"memory_count"`
			OpenTodoCount int    `json:"open_todo_count"`
		} `json:"topics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	found := false
	for _, row := range resp.Topics {
		if row.ID == tp.ID.String() {
			found = true
			if row.Name != tp.Name {
				t.Fatalf("name = %s", row.Name)
			}
			if row.MemoryCount != 1 || row.OpenTodoCount != 1 {
				t.Fatalf("counts = %d/%d, want 1/1", row.MemoryCount, row.OpenTodoCount)
			}
		}
	}
	if !found {
		t.Fatalf("topic %s missing: %s", tp.ID, rec.Body.String())
	}
}

func TestTopicCreateAndDuplicate(t *testing.T) {
	r, _, _, _, tp := setupTopicAPI(t)
	// 创建
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/topics",
		strings.NewReader(`{"name":"API用例主题健身","description":"每周三次"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	// 重名 → 409
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/topics",
		strings.NewReader(`{"name":"`+tp.Name+`"}`))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("重名应 409, got %d", rec2.Code)
	}
	// 空名 → 400
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/api/topics", strings.NewReader(`{"name":"  "}`))
	req3.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("空名应 400, got %d", rec3.Code)
	}
}

func TestTopicDetailAndPatch(t *testing.T) {
	r, tr, _, _, tp := setupTopicAPI(t)

	// 详情：topic + memories + todos
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topics/"+tp.ID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"memories"`, `"todos"`, "API用例记忆学Rust"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail 缺 %s: %s", want, body)
		}
	}

	// 确认 suggested→active
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPatch, "/api/topics/"+tp.ID.String(),
		strings.NewReader(`{"status":"active"}`))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("confirm: %d", rec2.Code)
	}
	got, _ := tr.Get(context.Background(), tp.ID)
	if got.Status != "active" {
		t.Fatalf("status = %s", got.Status)
	}

	// 改名
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPatch, "/api/topics/"+tp.ID.String(),
		strings.NewReader(`{"name":"API用例主题Rust进阶"}`))
	req3.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("rename: %d", rec3.Code)
	}
	got2, _ := tr.Get(context.Background(), tp.ID)
	if got2.Name != "API用例主题Rust进阶" {
		t.Fatalf("name = %s", got2.Name)
	}

	// 忽略
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodPatch, "/api/topics/"+tp.ID.String(),
		strings.NewReader(`{"status":"dismissed"}`))
	req4.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Fatalf("dismiss: %d", rec4.Code)
	}

	// 不存在 → 404
	rec5 := httptest.NewRecorder()
	r.ServeHTTP(rec5, httptest.NewRequest(http.MethodGet, "/api/topics/"+ids.New().String(), nil))
	if rec5.Code != http.StatusNotFound {
		t.Fatalf("404: got %d", rec5.Code)
	}
	// PATCH 非法 status → 400
	rec6 := httptest.NewRecorder()
	req6 := httptest.NewRequest(http.MethodPatch, "/api/topics/"+tp.ID.String(),
		strings.NewReader(`{"status":"bogus"}`))
	req6.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec6, req6)
	if rec6.Code != http.StatusBadRequest {
		t.Fatalf("非法 status 应 400, got %d", rec6.Code)
	}
}
