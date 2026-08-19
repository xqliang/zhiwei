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
	ctx := context.Background()

	topic := &repo.Topic{Name: "API测试工作", Status: "active", CreatedBy: "user"}
	if err := tr.Create(ctx, topic); err != nil {
		t.Fatal(err)
	}
	eventAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	mem := &repo.Memory{Type: "event", Title: "API 用例记忆 A", Content: "事件 A 的完整描述内容",
		EpistemicType: "observed", Confidence: 0.9, TopicID: &topic.ID,
		SessionID: ids.New(), EventAt: &eventAt, Status: "active"}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{mem}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	RegisterMemory(r, &MemoryHandler{Memories: mr, Topics: tr})
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
	if resp.Memories[0].TopicName == nil || *resp.Memories[0].TopicName != "API测试工作" {
		t.Fatalf("topic_name = %v", resp.Memories[0].TopicName)
	}

	// topic_id 过滤命中
	topicID := resp.Memories[0].TopicID
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
