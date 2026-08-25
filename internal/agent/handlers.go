package agent

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// AgentHandler 提供对话 REST（本期非流式；WS 流式见 P2c）。
type AgentHandler struct {
	Orch          *Orchestrator
	Conversations *repo.AgentConversationRepo
	Messages      *repo.AgentMessageRepo
}

// RegisterAgent 挂载 /api/agent 路由。
func RegisterAgent(r chi.Router, h *AgentHandler) {
	r.Post("/api/agent/conversations", h.createConversation)
	r.Get("/api/agent/conversations", h.listConversations)
	r.Get("/api/agent/conversations/{cid}", h.getConversation)
	r.Post("/api/agent/conversations/{cid}/messages", h.postMessage)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *AgentHandler) createConversation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	c := &repo.AgentConversation{Title: body.Title}
	if err := h.Conversations.Create(r.Context(), c); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, c)
}

func (h *AgentHandler) listConversations(w http.ResponseWriter, r *http.Request) {
	list, err := h.Conversations.List(r.Context(), 1)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, list)
}

func (h *AgentHandler) getConversation(w http.ResponseWriter, r *http.Request) {
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid cid"})
		return
	}
	msgs, err := h.Messages.ListByConversation(r.Context(), cid)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"conversation_id": cid, "messages": msgs})
}

func (h *AgentHandler) postMessage(w http.ResponseWriter, r *http.Request) {
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
	conv, err := h.Conversations.Get(r.Context(), cid)
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
