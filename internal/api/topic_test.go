package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
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
	// topic 归属走关联表：先建主表行，再 AddLink
	eventAt := time.Now()
	mem := &repo.Memory{
		Type: "fact", Title: "API用例记忆学Rust", Content: "用户正在学习 Rust 计划三个月读完一本书",
		EpistemicType: "observed", Confidence: 0.9,
		SessionID: ids.New(), EventAt: &eventAt, Status: "active",
	}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{mem}); err != nil {
		t.Fatal(err)
	}
	if err := (&repo.MemoryTopicRepo{DB: db}).AddLink(ctx, mem.ID, tp.ID); err != nil {
		t.Fatal(err)
	}
	memRows, err := mr.ListByTopic(ctx, tp.ID)
	if err != nil || len(memRows) != 1 {
		t.Fatalf("fixture memory: %v %d", err, len(memRows))
	}
	td := &repo.Todo{
		Title: "API用例待办读完Rust书", Status: "confirmed", Confidence: 0.9,
		SourceMemoryID: &memRows[0].ID,
	}
	if err := tdr.InsertExt(ctx, db, []*repo.Todo{td}); err != nil {
		t.Fatal(err)
	}
	if err := (&repo.TodoTopicRepo{DB: db}).AddLink(ctx, td.ID, tp.ID); err != nil {
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

// fakeConsolidateLLM 是 Consolidate 测试用的假 LLM：返回预设响应，不真正联网。
// 实现 provider.LLMProvider 接口（只有 Chat 一个方法），让 Consolidate handler
// 走完「调用 LLM → 容错解析 → 回传提议」全路径，不依赖外部服务。
type fakeConsolidateLLM struct {
	resp string
}

func (f *fakeConsolidateLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{Content: f.resp}, nil
}

// setupMergeFixtures 准备 2 个 active topic（A「合并靶」/B「合并源」），
// 各挂 1 条 active memory + 1 条 confirmed todo，用于 merge/consolidate 测试。
// 名称用「合并」前缀，避免与其他 fixture 混淆。
// 预清理同名 active/suggested 旧行：重跑时 FindActiveByNameExt 会命中旧行（ORDER BY id
// LIMIT 1，旧 id 更小），导致 merge 把关联迁到错误 topic，测试误判。仿
// TestTopicFindActiveByNameExt 的预清理模式。
func setupMergeFixtures(t *testing.T) (*repo.TopicRepo, *repo.MemoryRepo, *repo.TodoRepo, *repo.Topic, *repo.Topic) {
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

	// 预清理：重跑时历史 fixture 的同名 active topic 会让 FindActiveByNameExt 命中旧行
	for _, name := range []string{"合并靶", "合并源"} {
		if _, err := db.ExecContext(ctx, `
UPDATE topic SET status='dismissed'
WHERE user_id = 1 AND name = ? AND status IN ('active','suggested')`, name); err != nil {
			t.Fatal(err)
		}
	}

	a := &repo.Topic{Name: "合并靶", Status: "active", CreatedBy: "ai"}
	if err := tr.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := &repo.Topic{Name: "合并源", Status: "active", CreatedBy: "ai"}
	if err := tr.Create(ctx, b); err != nil {
		t.Fatal(err)
	}

	eventAt := time.Now()
	// A 挂 1 memory + 1 confirmed todo
	memA := &repo.Memory{
		Type: "fact", Title: "合并A记忆", Content: "合并靶主题下的记忆内容",
		EpistemicType: "observed", Confidence: 0.9,
		SessionID: ids.New(), EventAt: &eventAt, Status: "active",
	}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{memA}); err != nil {
		t.Fatal(err)
	}
	if err := (&repo.MemoryTopicRepo{DB: db}).AddLink(ctx, memA.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	todoA := &repo.Todo{
		Title: "合并A待办", Status: "confirmed", Confidence: 0.9,
		SourceMemoryID: &memA.ID,
	}
	if err := tdr.InsertExt(ctx, db, []*repo.Todo{todoA}); err != nil {
		t.Fatal(err)
	}
	if err := (&repo.TodoTopicRepo{DB: db}).AddLink(ctx, todoA.ID, a.ID); err != nil {
		t.Fatal(err)
	}

	// B 挂 1 memory + 1 confirmed todo
	memB := &repo.Memory{
		Type: "fact", Title: "合并B记忆", Content: "合并源主题下的记忆内容",
		EpistemicType: "observed", Confidence: 0.9,
		SessionID: ids.New(), EventAt: &eventAt, Status: "active",
	}
	if err := mr.InsertExt(ctx, db, []*repo.Memory{memB}); err != nil {
		t.Fatal(err)
	}
	if err := (&repo.MemoryTopicRepo{DB: db}).AddLink(ctx, memB.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	todoB := &repo.Todo{
		Title: "合并B待办", Status: "confirmed", Confidence: 0.9,
		SourceMemoryID: &memB.ID,
	}
	if err := tdr.InsertExt(ctx, db, []*repo.Todo{todoB}); err != nil {
		t.Fatal(err)
	}
	if err := (&repo.TodoTopicRepo{DB: db}).AddLink(ctx, todoB.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	return tr, mr, tdr, a, b
}

// TestTopicMerge 验证 merge 关联迁移事务：canonical_name「合并靶」命中已有 A，
// member B 的 memory_topic/todo_topic 关联 INSERT IGNORE 迁到 A（PK 去重），
// 删 B 关联行，B 置 dismissed。不调 LLM（纯 DB 事务）。
func TestTopicMerge(t *testing.T) {
	tr, mr, tdr, a, b := setupMergeFixtures(t)

	r := chi.NewRouter()
	// Merge 不调 LLM，LLM 留 nil
	RegisterTopic(r, &TopicHandler{Topics: tr, Memories: mr, Todos: tdr})

	body := fmt.Sprintf(`{"groups":[{"canonical_name":"合并靶","member_ids":["%s","%s"]}]}`,
		a.ID.String(), b.ID.String())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/topics/merge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body.String())
	}

	ctx := context.Background()
	// A 的 memory_topic 关联数 = 2（A 原 1 + B 迁来 1，PK 去重后无重复）
	memA, err := mr.ListByTopic(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(memA) != 2 {
		t.Fatalf("A memory count = %d, want 2", len(memA))
	}
	// A 的 todo_topic 关联数 = 2（A 原 1 + B 迁来 1）
	todoA, err := tdr.ListByTopic(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(todoA) != 2 {
		t.Fatalf("A todo count = %d, want 2", len(todoA))
	}
	// B status = dismissed
	gotB, err := tr.Get(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotB.Status != "dismissed" {
		t.Fatalf("B status = %s, want dismissed", gotB.Status)
	}
	// B 的关联已删（ListByTopic 返空：查 memory_topic/todo_topic WHERE topic_id=B 无行）
	memB, err := mr.ListByTopic(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(memB) != 0 {
		t.Fatalf("B memory count = %d, want 0", len(memB))
	}
	todoB, err := tdr.ListByTopic(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(todoB) != 0 {
		t.Fatalf("B todo count = %d, want 0", len(todoB))
	}
}

// TestTopicConsolidate 验证 consolidate 提议路径：fake LLM 返回 canned 响应，
// handler 调 ListActive → LLM.Chat → 容错解析 → 原样回传提议（不改库）。
// 用真实 A/B id 填进 canned 响应，断言响应 JSON 含合并组与两个 member_ids。
func TestTopicConsolidate(t *testing.T) {
	tr, mr, tdr, a, b := setupMergeFixtures(t)

	// 用真实 id 填进 canned 响应，模拟 LLM 按输入 topic id 给出合并提议
	canned := fmt.Sprintf(`{"groups":[{"canonical_name":"合并后","member_ids":["%s","%s"]}]}`,
		a.ID.String(), b.ID.String())
	r := chi.NewRouter()
	RegisterTopic(r, &TopicHandler{
		Topics: tr, Memories: mr, Todos: tdr,
		LLM: &fakeConsolidateLLM{resp: canned}, LLMModel: "test", ConsolidatePrompt: "sys",
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/topics/consolidate", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("consolidate: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Groups []struct {
			CanonicalName string   `json:"canonical_name"`
			MemberIDs     []string `json:"member_ids"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	if len(resp.Groups) != 1 {
		t.Fatalf("groups len = %d, want 1: %s", len(resp.Groups), rec.Body.String())
	}
	if resp.Groups[0].CanonicalName != "合并后" {
		t.Fatalf("canonical = %s, want 合并后", resp.Groups[0].CanonicalName)
	}
	// member_ids 含 A、B 两个 id 字符串
	want := map[string]bool{a.ID.String(): false, b.ID.String(): false}
	for _, mid := range resp.Groups[0].MemberIDs {
		if _, ok := want[mid]; ok {
			want[mid] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Fatalf("member_ids 缺 %s: %v", k, resp.Groups[0].MemberIDs)
		}
	}
}

// TestTopicDelete 验证 DELETE 硬删 topic + 其 memory_topic/todo_topic 关联（单事务级联），
// member B 完好（关联不误删）。区别于 dismiss（PATCH dismissed 软删）。重复删除幂等。
func TestTopicDelete(t *testing.T) {
	tr, mr, tdr, a, b := setupMergeFixtures(t)
	r := chi.NewRouter()
	RegisterTopic(r, &TopicHandler{Topics: tr, Memories: mr, Todos: tdr}) // Delete 不调 LLM
	ctx := context.Background()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/topics/"+a.ID.String(), nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := tr.Get(ctx, a.ID); err == nil {
		t.Fatal("topic a 仍存在")
	}
	// a 的关联已级联删
	var n int
	if err := tr.DB.GetContext(ctx, &n, `SELECT COUNT(*) FROM memory_topic WHERE topic_id = ?`, a.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a memory_topic 残留 %d", n)
	}
	if err := tr.DB.GetContext(ctx, &n, `SELECT COUNT(*) FROM todo_topic WHERE topic_id = ?`, a.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a todo_topic 残留 %d", n)
	}
	// b 完好（未被误删/误改 dismissed）
	gotB, err := tr.Get(ctx, b.ID)
	if err != nil || gotB.Status == "dismissed" {
		t.Fatalf("b 被误删/误改: err=%v status=%s", err, gotB.Status)
	}
	// 重复删除幂等（204）
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodDelete, "/api/topics/"+a.ID.String(), nil))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("idempotent delete: %d", rec2.Code)
	}
}
