package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TodoHandler 处理待办查询与状态流转。
type TodoHandler struct {
	Todos      *repo.TodoRepo
	TodoTopics *repo.TodoTopicRepo // 手动加/删 todo↔topic 关联
	Topics     *repo.TopicRepo      // 校验 topic 存在
}

// RegisterTodo 挂载 todo 路由（router.go 的统一接线在后续任务完成）。
func RegisterTodo(r chi.Router, h *TodoHandler) {
	r.Get("/api/todos", h.List)
	r.Patch("/api/todos/{id}", h.Patch)
	r.Post("/api/todos/{id}/topics", h.AddTopic)
	r.Delete("/api/todos/{id}/topics/{topic_id}", h.RemoveTopic)
}

// List 返回待办列表（排除 dismissed），支持 status/topic_id 过滤。
// 列表行附带 source_session_id（前端「跳转时间线」用）。
func (h *TodoHandler) List(w http.ResponseWriter, r *http.Request) {
	// 校验顺序：先参数合法性（400），再查库
	status := r.URL.Query().Get("status")
	if status != "" && !validTodoStatus(status) {
		http.Error(w, "status 取值非法", http.StatusBadRequest)
		return
	}
	var topicID *ids.ID
	if v := r.URL.Query().Get("topic_id"); v != "" {
		tid, err := ids.ParseID(v)
		if err != nil {
			http.Error(w, "topic_id 非法", http.StatusBadRequest)
			return
		}
		topicID = &tid
	}
	rows, err := h.Todos.List(r.Context(), status, topicID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"todos": rows})
}

// Patch 变更待办状态，按状态机校验：
// suggested→confirmed、confirmed→done、任意非 dismissed→dismissed。
// 校验顺序：解码+枚举校验（400）→ 存在性（404）→ 流转合法性（409）。
func (h *TodoHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validTodoStatus(req.Status) {
		http.Error(w, "status 取值非法", http.StatusBadRequest)
		return
	}
	td, err := h.Todos.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "todo 不存在", http.StatusNotFound)
		return
	}
	if !repo.CanTransition(td.Status, req.Status) {
		http.Error(w, "不允许的状态流转: "+td.Status+" → "+req.Status, http.StatusConflict)
		return
	}
	if err := h.Todos.UpdateStatus(r.Context(), id, req.Status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	td.Status = req.Status
	writeJSON(w, map[string]any{"todo": td})
}

// validTodoStatus 校验 todo 状态枚举（与 repo 层枚举保持一致）。
func validTodoStatus(s string) bool {
	switch s {
	case "suggested", "confirmed", "done", "dismissed":
		return true
	}
	return false
}

// AddTopic 手动给 todo 加 topic 关联（source='user'，INSERT IGNORE 幂等）。
// 校验顺序：参数合法性（400）→ todo 存在（404）→ topic 存在且非 dismissed（404）。
func (h *TodoHandler) AddTopic(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		TopicID string `json:"topic_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TopicID == "" {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	tid, err := ids.ParseID(req.TopicID)
	if err != nil {
		http.Error(w, "topic_id 非法", http.StatusBadRequest)
		return
	}
	if _, err := h.Todos.Get(r.Context(), id); err != nil {
		http.Error(w, "todo 不存在", http.StatusNotFound)
		return
	}
	tp, err := h.Topics.Get(r.Context(), tid)
	if err != nil || tp.Status == "dismissed" {
		http.Error(w, "topic 不存在", http.StatusNotFound)
		return
	}
	if err := h.TodoTopics.AddLink(r.Context(), id, tid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// RemoveTopic 移除 todo↔topic 关联。幂等：关联不存在也不报错（DELETE 返回 204）。
func (h *TodoHandler) RemoveTopic(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	tid, err := ids.ParseID(chi.URLParam(r, "topic_id"))
	if err != nil {
		http.Error(w, "invalid topic_id", http.StatusBadRequest)
		return
	}
	if err := h.TodoTopics.RemoveLink(r.Context(), id, tid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
