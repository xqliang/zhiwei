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
	Todos *repo.TodoRepo
}

// RegisterTodo 挂载 todo 路由（router.go 的统一接线在后续任务完成）。
func RegisterTodo(r chi.Router, h *TodoHandler) {
	r.Get("/api/todos", h.List)
	r.Patch("/api/todos/{id}", h.Patch)
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
