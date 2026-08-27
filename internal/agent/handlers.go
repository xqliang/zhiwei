package agent

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/auth"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// AgentHandler 提供对话 REST（本期非流式；WS 流式见 P2c）。
type AgentHandler struct {
	Orch          *Orchestrator
	Conversations *repo.AgentConversationRepo
	Messages      *repo.AgentMessageRepo
	Hub           *turnHub // 每会话轮次广播器（nil 时由 RegisterAgent 惰性初始化）
}

// RegisterAgent 挂载 /api/agent 路由。
func RegisterAgent(r chi.Router, h *AgentHandler) {
	if h.Hub == nil {
		h.Hub = newTurnHub() // 生产/测试都经此入口，main.go 用结构体字面量构造无需感知内部 hub 类型
	}
	r.Post("/api/agent/conversations", h.createConversation)
	r.Get("/api/agent/conversations", h.listConversations)
	r.Get("/api/agent/conversations/{cid}", h.getConversation)
	r.Post("/api/agent/conversations/{cid}/messages", h.postMessage)
	r.Get("/api/agent/conversations/{cid}/ws", h.handleWS) // WS 流式（上行发消息 + 下行流式帧）
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// reqUserID 从请求 ctx 取已鉴权用户 id（authGate 中间件注入）。未注入返回 ok=false，调用方据此
// 401——这些端点生产中都在 authGate 保护内、理论必有 uid；防御性显式取，杜绝「未鉴权却按某个
// 写死用户操作」的越权（2B-B：多用户隔离，一切读写都须绑定当前登录用户）。
func reqUserID(r *http.Request) (int64, bool) {
	id, ok := auth.UserID(r.Context())
	return id.Int64(), ok
}

func (h *AgentHandler) createConversation(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	c := &repo.AgentConversation{Title: body.Title, UserID: uid}
	if err := h.Conversations.Create(r.Context(), c); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// I2：Create 不回填 DB 默认列（status/created_at/last_active_at），直接返回 c 会带出
	// 空 status 和零值时间戳。读回完整行再响应，保证前端拿到 active + 真实时间。
	if full, err := h.Conversations.Get(r.Context(), uid, c.ID); err == nil {
		c = full
	}
	writeJSON(w, 200, c)
}

func (h *AgentHandler) listConversations(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	list, err := h.Conversations.List(r.Context(), uid)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, list)
}

func (h *AgentHandler) getConversation(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid cid"})
		return
	}
	msgs, err := h.Messages.ListByConversation(r.Context(), uid, cid)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"conversation_id": cid, "messages": msgs})
}

func (h *AgentHandler) postMessage(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid cid"})
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "text required"})
		return
	}
	conv, err := h.Conversations.Get(r.Context(), uid, cid)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "conversation not found"})
		return
	}
	final, err := h.Orch.RunTurn(r.Context(), conv, body.Text)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "assistant": final})
		return
	}
	writeJSON(w, 200, final)
}
