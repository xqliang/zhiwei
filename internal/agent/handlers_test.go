package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/auth"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// injectUser 是测试中间件：模拟生产 authGate 往请求 ctx 注入已鉴权 userID（2B-B 起 agent
// 端点从 ctx 取 auth.UserID；不注入会 401）。同包内 ws_test 也复用它。
func injectUser(uid int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), ids.ID(uid))))
		})
	}
}

// TestAgentConfigAPI 锁定人设配置端点：PUT 保存 identity/soul → GET 读回一致，且返回注入预览。
func TestAgentConfigAPI(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfgRepo := &repo.AgentConfigRepo{DB: db}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_config WHERE id = 1") })
	h := &AgentHandler{Configs: cfgRepo}
	r := chi.NewRouter()
	r.Use(injectUser(1))
	RegisterAgent(r, h)

	// PUT 保存
	putReq := httptest.NewRequest("PUT", "/api/agent/config",
		strings.NewReader(`{"identity":"我是知微API","soul":"简洁API"}`))
	putRec := httptest.NewRecorder()
	r.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT code=%d body=%s", putRec.Code, putRec.Body.String())
	}

	// GET 读回
	getReq := httptest.NewRequest("GET", "/api/agent/config", nil)
	getRec := httptest.NewRecorder()
	r.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET code=%d", getRec.Code)
	}
	var out struct{ Identity, Soul, Preview string }
	if err := json.Unmarshal(getRec.Body.Bytes(), &out); err != nil {
		t.Fatalf("resp 解析: %v", err)
	}
	if out.Identity != "我是知微API" || out.Soul != "简洁API" {
		t.Errorf("读回不符: %+v", out)
	}
	if !strings.Contains(out.Preview, "我是知微API") || !strings.Contains(out.Preview, "简洁API") {
		t.Errorf("预览应含 identity+soul: %q", out.Preview)
	}
}

func TestPostMessageEndToEndFake(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "API 测试"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}

	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "答复内容"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	h := &AgentHandler{Orch: NewOrchestrator(rtFor(fake), convRepo, msgRepo), Conversations: convRepo, Messages: msgRepo}
	r := chi.NewRouter()
	r.Use(injectUser(1)) // 模拟 authGate 注入 uid=1
	RegisterAgent(r, h)

	req := httptest.NewRequest("POST", "/api/agent/conversations/"+conv.ID.String()+"/messages",
		strings.NewReader(`{"text":"你好"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got repo.AgentMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("resp 非 AgentMessage: %v", err)
	}
	if got.Content != "答复内容" || got.Role != "assistant" {
		t.Errorf("响应异常: %+v", got)
	}
}

// TestCreateConversationBackfill 锁定 I2：建会话响应应带回 DB 默认列
// （status=active、真实时间戳），而非空串 status 和零值时间。
func TestCreateConversationBackfill(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	h := &AgentHandler{Orch: NewOrchestrator(rtFor(&FakeRuntime{}), convRepo, msgRepo), Conversations: convRepo, Messages: msgRepo}
	r := chi.NewRouter()
	r.Use(injectUser(1)) // 模拟 authGate 注入 uid=1
	RegisterAgent(r, h)

	req := httptest.NewRequest("POST", "/api/agent/conversations", strings.NewReader(`{"title":"新对话"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got repo.AgentConversation
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("resp 非 AgentConversation: %v", err)
	}
	if got.Status != "active" {
		t.Errorf("status 应回填 active, got %q", got.Status)
	}
	if got.CreatedAt.IsZero() || got.LastActiveAt.IsZero() {
		t.Errorf("时间戳应回填非零, got created=%v lastActive=%v", got.CreatedAt, got.LastActiveAt)
	}
	if got.Title != "新对话" {
		t.Errorf("title=%q", got.Title)
	}
}
