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

// setupMemoryAPI 准备 memory 路由 + 一条带 topic 的记忆（type=event）。
// 标题用 "API 用例记忆" 前缀，避免与其他包的 fixture 混淆。
func setupMemoryAPI(t *testing.T) (http.Handler, *repo.MemoryRepo, *repo.TopicRepo, *repo.Memory) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.Init(1); err != nil {
		t.Fatal(err)
	}
	mr := &repo.MemoryRepo{DB: db}
	tr := &repo.TopicRepo{DB: db}
	mtr := &repo.MemoryTopicRepo{DB: db}
	ctx := context.Background()

	topic := &repo.Topic{Name: "API测试工作", Status: "active", CreatedBy: "user"}
	if err := tr.Create(ctx, topic); err != nil {
		t.Fatal(err)
	}
	eventAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	mem := &repo.Memory{Type: "event", Title: "API 用例记忆 A", Content: "事件 A 的完整描述内容",
		EpistemicType: "observed", Confidence: 0.9,
		SessionID: ids.New(), EventAt: &eventAt, Status: "active"}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{mem}); err != nil {
		t.Fatal(err)
	}
	// topic 归属走关联表：建 memory 后 AddLink
	if err := mtr.AddLink(ctx, mem.ID, topic.ID); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	RegisterMemory(r, &MemoryHandler{Memories: mr, Topics: tr, MemoryTopics: mtr})
	return r, mr, tr, mem
}

func TestMemoryListAndFilter(t *testing.T) {
	r, mr, tr, mem := setupMemoryAPI(t)
	_ = tr
	_ = mem
	ctx := context.Background()
	// 再插一条不同 type 的记忆，验证 type 过滤
	if err := mr.InsertExt(ctx, mr.DB, []*repo.Memory{{Type: "fact", Title: "API 用例记忆 B",
		Content: "事实 B 的完整描述内容", EpistemicType: "observed", Confidence: 0.9,
		SessionID: ids.New(), Status: "active"}}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/memories?type=event", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Memories []repo.MemoryRow `json:"memories"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Memories) != 1 {
		t.Fatalf("type=event 过滤后 = %d, want 1", len(resp.Memories))
	}
	if len(resp.Memories[0].Topics) != 1 || resp.Memories[0].Topics[0].Name != "API测试工作" {
		t.Fatalf("topics = %+v, want [{API测试工作}]", resp.Memories[0].Topics)
	}

	// topic_id 过滤命中（走关联表子查询）
	topicID := resp.Memories[0].Topics[0].ID
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet,
		"/api/memories?topic_id="+topicID.String(), nil))
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "API 用例记忆 A") {
		t.Fatalf("topic filter: %d %s", rec2.Code, rec2.Body.String())
	}
	// 非法 topic_id → 400
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/memories?topic_id=abc", nil))
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("非法 topic_id 应 400, got %d", rec3.Code)
	}
	// 非法 type → 400
	rec4 := httptest.NewRecorder()
	r.ServeHTTP(rec4, httptest.NewRequest(http.MethodGet, "/api/memories?type=bogus", nil))
	if rec4.Code != http.StatusBadRequest {
		t.Fatalf("非法 type 应 400, got %d", rec4.Code)
	}
}

func TestMemoryListSince(t *testing.T) {
	r, mr, _, mem := setupMemoryAPI(t)
	ctx := context.Background()
	// 再插一条 event_at 更早的记忆（A 是 2026-08-19 12:00 UTC）
	early := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	if err := mr.InsertExt(ctx, mr.DB, []*repo.Memory{{Type: "event", Title: "API 用例记忆 C（早）",
		Content: "事件 C 的完整描述内容", EpistemicType: "observed", Confidence: 0.9,
		SessionID: ids.New(), EventAt: &early, Status: "active"}}); err != nil {
		t.Fatal(err)
	}

	// since 夹在两者之间 → 早的 C 不出现、晚的 A 出现。
	// （共享测试库里 TestMemoryListAndFilter 也建了一条同名 A，故不断言精确计数）
	mid := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/memories?since="+mid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("since: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Memories []repo.MemoryRow `json:"memories"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	hasA, hasC := false, false
	for _, m := range resp.Memories {
		if m.Title == mem.Title {
			hasA = true
		}
		if m.Title == "API 用例记忆 C（早）" {
			hasC = true
		}
	}
	if !hasA || hasC {
		t.Fatalf("since 过滤后 = %v, want 含 %q 且不含早的 C", titlesOf(resp.Memories), mem.Title)
	}

	// 日期-only 格式（YYYY-MM-DD，本地零点）→ 同样只返回 A
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/memories?since=2026-08-10", nil))
	if rec2.Code != http.StatusOK || strings.Contains(rec2.Body.String(), "API 用例记忆 C（早）") {
		t.Fatalf("since=date-only: %d %s", rec2.Code, rec2.Body.String())
	}

	// 非法 since → 400
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/memories?since=notadate", nil))
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("非法 since 应 400, got %d", rec3.Code)
	}
}

func titlesOf(rows []repo.MemoryRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Title
	}
	return out
}

// TestMemoryAddRemoveTopic 验证手动加/删 memory↔topic 关联端点：
// POST 幂等、GET 列表反映增删、DELETE 204、不存在 topic_id → 404。
func TestMemoryAddRemoveTopic(t *testing.T) {
	_ = ids.Init(1)
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mr := &repo.MemoryRepo{DB: db}
	topics := &repo.TopicRepo{DB: db}
	mtr := &repo.MemoryTopicRepo{DB: db}

	mem := &repo.Memory{Type: "fact", Title: "加删 topic 用例记忆", Content: "足够长的内容描述",
		EpistemicType: "observed", Confidence: 0.9, SessionID: ids.New(), Status: "active"}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{mem}); err != nil {
		t.Fatal(err)
	}
	tp := &repo.Topic{Name: "记忆加删主题", Status: "active", CreatedBy: "user"}
	if err := topics.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	RegisterMemory(r, &MemoryHandler{Memories: mr, Topics: topics, MemoryTopics: mtr})

	post := func(body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/memories/"+mem.ID.String()+"/topics",
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
	// 重复 POST → 200（幂等）
	if code := post(body); code != http.StatusOK {
		t.Fatalf("idempotent add: %d", code)
	}
	// GET 列表 → memory.topics 反映新增
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/memories", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"记忆加删主题"`) {
		t.Fatalf("列表应含已加 topic: %d %s", rec.Code, rec.Body.String())
	}
	// DELETE 移除关联 → 204
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodDelete,
		"/api/memories/"+mem.ID.String()+"/topics/"+tp.ID.String(), nil))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("remove topic: %d", rec2.Code)
	}
	// GET 列表 → memory.topics 为空
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/memories", nil))
	if strings.Contains(rec3.Body.String(), `"记忆加删主题"`) {
		t.Fatalf("移除后列表不应含 topic: %s", rec3.Body.String())
	}
	// POST 不存在 topic_id → 404
	if code := post(`{"topic_id":"` + ids.New().String() + `"}`); code != http.StatusNotFound {
		t.Fatalf("不存在 topic 应 404, got %d", code)
	}
	// POST 不存在 memory id → 404
	rec4 := httptest.NewRecorder()
	r.ServeHTTP(rec4, httptest.NewRequest(http.MethodPost, "/api/memories/"+ids.New().String()+"/topics",
		strings.NewReader(body)))
	if rec4.Code != http.StatusNotFound {
		t.Fatalf("不存在 memory 应 404, got %d", rec4.Code)
	}
}

func TestMemoryPatch(t *testing.T) {
	r, mr, _, mem := setupMemoryAPI(t)
	ctx := context.Background()

	// 修正内容 → version+1
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/memories/"+mem.ID.String(),
		strings.NewReader(`{"content":"修正后的内容描述"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := mr.Get(ctx, mem.ID)
	if got.Content != "修正后的内容描述" || got.Version != 2 {
		t.Fatalf("after patch: %+v", got)
	}

	// dismiss 后列表不出现
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPatch, "/api/memories/"+mem.ID.String(),
		strings.NewReader(`{"status":"dismissed"}`))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("dismiss: %d", rec2.Code)
	}
	rows, _ := mr.ListBySession(ctx, mem.SessionID)
	if len(rows) != 0 {
		t.Fatalf("dismissed 后列表不应出现, got %d", len(rows))
	}

	// 不存在 → 404（雪花 ID 不会撞 123）
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodPatch,
		"/api/memories/123", strings.NewReader(`{"status":"dismissed"}`)))
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("404: got %d", rec3.Code)
	}
	// 非法 status → 400
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodPatch, "/api/memories/123",
		strings.NewReader(`{"status":"bogus"}`))
	req4.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusBadRequest {
		t.Fatalf("非法 status 应 400, got %d", rec4.Code)
	}
}
