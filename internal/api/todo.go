package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/auth"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TodoHandler 处理待办查询与状态流转。
type TodoHandler struct {
	Todos      *repo.TodoRepo
	TodoTopics *repo.TodoTopicRepo // 手动加/删 todo↔topic 关联
	Topics     *repo.TopicRepo     // 校验 topic 存在
}

// RegisterTodo 挂载 todo 路由（router.go 的统一接线在后续任务完成）。
func RegisterTodo(r chi.Router, h *TodoHandler) {
	r.Get("/api/todos", h.List)
	r.Patch("/api/todos/{id}", h.Patch)
	r.Delete("/api/todos/{id}", h.Delete)
	r.Post("/api/todos/{id}/topics", h.AddTopic)
	r.Delete("/api/todos/{id}/topics/{topic_id}", h.RemoveTopic)
}

// List 返回待办列表（默认排除 dismissed），支持 status/topic_id 过滤。
// ?dismissed=1 返回已忽略待办（折叠区查看，终态不可恢复，仅供查看+硬删）。
// 列表行附带 source_session_id（前端「跳转时间线」用）。
func (h *TodoHandler) List(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// ?dismissed=1 单独取已忽略待办（与默认 List 的「排除 dismissed」互补）
	if r.URL.Query().Get("dismissed") == "1" {
		rows, err := h.Todos.ListDismissed(r.Context(), uid.Int64())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"todos": rows})
		return
	}
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
	rows, err := h.Todos.List(r.Context(), uid.Int64(), status, topicID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"todos": rows})
}

// Patch 变更待办：title（改名）和/或 status（状态机流转）。至少一个非空（400）。
// 校验顺序：解码（400）→ title/status 非空（400）→ status 枚举（400）→ 存在性（404）
// → 流转合法性（409，先校验后变更，避免 title 已改但 status 409 的半成功）→ 变更（先 title 后 status）。
// CanTransition 用 Get 出的原始 td.Status（title 变更不影响状态判断）。title 不做 CanTransition，与状态独立。
func (h *TodoHandler) Patch(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" && req.Status == "" {
		http.Error(w, "title 或 status 至少一个非空", http.StatusBadRequest)
		return
	}
	if req.Status != "" && !validTodoStatus(req.Status) {
		http.Error(w, "status 取值非法", http.StatusBadRequest)
		return
	}
	td, err := h.Todos.Get(r.Context(), uid.Int64(), id)
	if err != nil {
		http.Error(w, "todo 不存在", http.StatusNotFound)
		return
	}
	// 先校验流转再变更：title+status 同 body 且 status 非法时，不留下 title 已改的半成品。
	if req.Status != "" && !repo.CanTransition(td.Status, req.Status) {
		http.Error(w, "不允许的状态流转: "+td.Status+" → "+req.Status, http.StatusConflict)
		return
	}
	if title != "" {
		if err := h.Todos.UpdateTitle(r.Context(), id, title); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		td.Title = title
	}
	if req.Status != "" {
		if err := h.Todos.UpdateStatus(r.Context(), id, req.Status); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		td.Status = req.Status
	}
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	if _, err := h.Todos.Get(r.Context(), uid.Int64(), id); err != nil {
		http.Error(w, "todo 不存在", http.StatusNotFound)
		return
	}
	tp, err := h.Topics.Get(r.Context(), uid.Int64(), tid)
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
// 归属校验（评审 I2）：先按登录用户 Get todo，拿不到（他人/不存在）→ 404，
// 不泄漏存在性；通过再解绑，杜绝跨租户改他人 todo 的关联。
func (h *TodoHandler) RemoveTopic(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	if _, err := h.Todos.Get(r.Context(), uid.Int64(), id); err != nil {
		http.Error(w, "todo 不存在", http.StatusNotFound)
		return
	}
	if err := h.TodoTopics.RemoveLink(r.Context(), id, tid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Delete 硬删除待办 + 关联（单事务级联，2 步确认由前端）。
// 归属校验（评审 I2）：先按登录用户 Get，拿不到（他人/不存在）→ 404，不泄漏存在性；
// 通过再删。故删不存在/他人待办返回 404（不再是幂等 204）。
func (h *TodoHandler) Delete(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if _, err := h.Todos.Get(r.Context(), uid.Int64(), id); err != nil {
		http.Error(w, "todo 不存在", http.StatusNotFound)
		return
	}
	if err := h.Todos.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
