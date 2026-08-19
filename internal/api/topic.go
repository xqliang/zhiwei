package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TopicHandler 处理主题的增查改。
type TopicHandler struct {
	Topics   *repo.TopicRepo
	Memories *repo.MemoryRepo
	Todos    *repo.TodoRepo
}

// RegisterTopic 挂载 topic 路由（router.go 的统一接线在后续任务完成）。
func RegisterTopic(r chi.Router, h *TopicHandler) {
	r.Get("/api/topics", h.List)
	r.Post("/api/topics", h.Create)
	r.Get("/api/topics/{id}", h.Get)
	r.Patch("/api/topics/{id}", h.Patch)
}

// List 返回非 dismissed 主题及关联计数（active memory 数 / confirmed todo 数），
// 按计数倒序，供前端 Topics 页展示。
func (h *TopicHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.Topics.ListWithCounts(r.Context(), 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"topics": list})
}

// Create 手动创建主题：name 去空白后必填，与现有 active/suggested 重名则 409。
func (h *TopicHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, "name 不能为空", http.StatusBadRequest)
		return
	}
	// 与现有 active/suggested 重名 → 409
	if dup, err := h.Topics.FindActiveByName(r.Context(), 1, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if dup != nil {
		http.Error(w, "同名主题已存在", http.StatusConflict)
		return
	}
	tp := &repo.Topic{Name: name, Status: "active", CreatedBy: "user"}
	if req.Description != "" {
		tp.Description = &req.Description
	}
	if err := h.Topics.Create(r.Context(), tp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"topic": tp})
}

// Get 返回主题详情：topic 本体 + 挂在该主题下的 memories 与 todos。
func (h *TopicHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	tp, err := h.Topics.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "topic 不存在", http.StatusNotFound)
		return
	}
	memories, err := h.Memories.ListByTopic(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	todos, err := h.Todos.ListByTopic(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"topic": tp, "memories": memories, "todos": todos})
}

// Patch 更新主题：status 仅允许 active|dismissed（确认/忽略），name 为改名。
// 校验顺序：解码+参数校验（400）→ 存在性（404）→ 重名（409）。
func (h *TopicHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Status *string `json:"status"`
		Name   *string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if (req.Status == nil && req.Name == nil) ||
		(req.Status != nil && *req.Status != "active" && *req.Status != "dismissed") {
		http.Error(w, "status 取值非法（active|dismissed）", http.StatusBadRequest)
		return
	}
	tp, err := h.Topics.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "topic 不存在", http.StatusNotFound)
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			http.Error(w, "name 不能为空", http.StatusBadRequest)
			return
		}
		// 改成与自身相同名字时跳过查重，避免误报 409
		if name != tp.Name {
			if dup, err := h.Topics.FindActiveByName(r.Context(), 1, name); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else if dup != nil {
				http.Error(w, "同名主题已存在", http.StatusConflict)
				return
			}
		}
		if err := h.Topics.UpdateName(r.Context(), id, name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.Status != nil {
		if err := h.Topics.UpdateStatus(r.Context(), id, *req.Status); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	got, err := h.Topics.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"topic": got})
}
