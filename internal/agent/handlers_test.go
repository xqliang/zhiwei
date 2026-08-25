package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/repo"
)

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
	h := &AgentHandler{Orch: NewOrchestrator(fake, convRepo, msgRepo), Conversations: convRepo, Messages: msgRepo}
	r := chi.NewRouter()
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
	h := &AgentHandler{Orch: NewOrchestrator(&FakeRuntime{}, convRepo, msgRepo), Conversations: convRepo, Messages: msgRepo}
	r := chi.NewRouter()
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
