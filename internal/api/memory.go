package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// MemoryHandler 处理 memory 查询与修正。
type MemoryHandler struct {
	Memories *repo.MemoryRepo
	Topics   *repo.TopicRepo
}

// RegisterMemory 挂载 memory 路由。
func RegisterMemory(r chi.Router, h *MemoryHandler) {
	r.Get("/api/memories", h.List)
	r.Patch("/api/memories/{id}", h.Patch)
}

// List 返回记忆列表（排除 dismissed），支持 type/topic_id/since/limit/offset
// 过滤分页。since 是事件时间下界（spec §4）：RFC3339 或 YYYY-MM-DD（当日零点）。
func (h *MemoryHandler) List(w http.ResponseWriter, r *http.Request) {
	f := repo.MemoryFilter{
		Type:   r.URL.Query().Get("type"),
		Limit:  intQuery(r, "limit", 50),
		Offset: intOffset(r),
	}
	if v := r.URL.Query().Get("topic_id"); v != "" {
		tid, err := ids.ParseID(v)
		if err != nil {
			http.Error(w, "topic_id 非法", http.StatusBadRequest)
			return
		}
		f.TopicID = &tid
	}
	if v := r.URL.Query().Get("since"); v != "" {
		ts, err := parseSince(v)
		if err != nil {
			http.Error(w, "since 取值非法（RFC3339 或 YYYY-MM-DD）", http.StatusBadRequest)
			return
		}
		f.Since = &ts
	}
	if f.Type != "" && !validMemoryType(f.Type) {
		http.Error(w, "type 取值非法", http.StatusBadRequest)
		return
	}
	rows, err := h.Memories.List(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"memories": rows})
}

// parseSince 解析 since 参数：优先 RFC3339（带时区），
// 失败再试日期格式 YYYY-MM-DD（按本地时区当日零点解释）。
func parseSince(v string) (time.Time, error) {
	if ts, err := time.Parse(time.RFC3339, v); err == nil {
		return ts, nil
	}
	return time.ParseInLocation("2006-01-02", v, time.Local)
}

// Patch 修正记忆内容或 dismiss。改 title/content 则 version+1（乐观并发用）。
func (h *MemoryHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Title   *string `json:"title"`
		Content *string `json:"content"`
		Status  *string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if req.Status != nil && *req.Status != "active" && *req.Status != "dismissed" && *req.Status != "superseded" {
		http.Error(w, "status 取值非法", http.StatusBadRequest)
		return
	}
	m, err := h.Memories.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "memory 不存在", http.StatusNotFound)
		return
	}
	// 改内容（title/content）则 version+1；status 单独变更不加版本
	contentChanged := false
	if req.Title != nil && strings.TrimSpace(*req.Title) != "" {
		m.Title = *req.Title
		contentChanged = true
	}
	if req.Content != nil && strings.TrimSpace(*req.Content) != "" {
		m.Content = *req.Content
		contentChanged = true
	}
	if contentChanged {
		m.Version++
	}
	if req.Status != nil {
		m.Status = *req.Status
	}
	if err := h.Memories.Save(r.Context(), m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"memory": m})
}

// validMemoryType 校验记忆类型枚举（与 extract 抽取类型一致）。
func validMemoryType(t string) bool {
	switch t {
	case "event", "fact", "decision", "idea", "problem", "preference":
		return true
	}
	return false
}
