package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// TopicHandler 处理主题的增查改，以及 T7 智能合并（consolidate LLM 提议 + merge 落库）。
// LLM/LLMModel/ConsolidatePrompt 仅 Consolidate 用；Merge 纯 DB 事务，不调 LLM。
type TopicHandler struct {
	Topics   *repo.TopicRepo
	Memories *repo.MemoryRepo
	Todos    *repo.TodoRepo

	// LLM 用于 consolidate 提议（merge 不调 LLM）。main.go 注入；测试可传 fake。
	LLM provider.LLMProvider
	// LLMModel 是 fast 模型名（cfg.LLMFastModel）。
	LLMModel string
	// ConsolidatePrompt 是 prompts/topic_consolidate_v1.md 的内容（系统指令）。
	ConsolidatePrompt string
}

// RegisterTopic 挂载 topic 路由（router.go 的统一接线在后续任务完成）。
// consolidate/merge 是 /api/topics 下的 POST 子路径，chi 精确匹配，
// 与现有 GET/POST /api/topics、GET/PATCH /api/topics/{id} 不冲突。
func RegisterTopic(r chi.Router, h *TopicHandler) {
	r.Get("/api/topics", h.List)
	r.Post("/api/topics", h.Create)
	r.Post("/api/topics/consolidate", h.Consolidate)
	r.Post("/api/topics/merge", h.Merge)
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

// Consolidate 调 LLM 生成合并提议：输入该用户全部 active/suggested topic，
// 输出合并组提议（canonical_name + member_ids），不改库。前端拿到后展示给用户确认。
//
// 流程：ListActive → 组 user 消息（JSON 数组）→ LLM.Chat → 容错解析 → 原样回传。
// 容错解析照搬 memory/candidate.go 思路：模型可能输出前后废话/围栏，截取首个 { 到末个 }。
func (h *TopicHandler) Consolidate(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		http.Error(w, "LLM 未配置", http.StatusInternalServerError)
		return
	}
	// 取该用户全部 active/suggested 主题（合并提议输入）
	list, err := h.Topics.ListActive(r.Context(), 1, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 组 user 消息：JSON 数组，每项 {id, name, status}，id 用字符串（雪花 ID 精度安全）
	type topicItem struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	items := make([]topicItem, 0, len(list))
	for _, tp := range list {
		items = append(items, topicItem{ID: tp.ID.String(), Name: tp.Name, Status: tp.Status})
	}
	userMsg, err := json.Marshal(items)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 调 LLM（fast 模型，系统指令 = 合并 prompt）
	resp, err := h.LLM.Chat(r.Context(), provider.ChatRequest{
		Model:  h.LLMModel,
		System: h.ConsolidatePrompt,
		User:   string(userMsg),
	})
	if err != nil {
		http.Error(w, "LLM 调用失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 容错解析：截取首个 { 到末个 }，剥掉模型可能输出的前后废话/markdown 围栏。
	// 内联在此处（不 import memory 包），与 candidate.go ParseCandidates 同思路。
	raw := strings.TrimSpace(resp.Content)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var out struct {
		Groups []struct {
			CanonicalName string   `json:"canonical_name"`
			MemberIDs     []string `json:"member_ids"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		http.Error(w, "合并提议解析失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 原样回传提议，不改库（用户确认后走 /api/topics/merge）
	writeJSON(w, map[string]any{"groups": out.Groups})
}

// Merge 用户确认后单事务落库合并：body 含合并组（canonical_name + member_ids 字符串）。
// member_ids 用 ids.ParseID 转 []ids.ID，交 TopicRepo.MergeGroups 在单事务内迁关联 +
// 删 member 行 + member 置 dismissed。空 groups 也接受（直接返回 merged:true）。
func (h *TopicHandler) Merge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Groups []struct {
			CanonicalName string   `json:"canonical_name"`
			MemberIDs     []string `json:"member_ids"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	// 转换为 repo.MergeGroup：member_ids 字符串 → ids.ID
	groups := make([]repo.MergeGroup, 0, len(req.Groups))
	for _, g := range req.Groups {
		mids := make([]ids.ID, 0, len(g.MemberIDs))
		for _, s := range g.MemberIDs {
			id, err := ids.ParseID(s)
			if err != nil {
				http.Error(w, "非法 member_id: "+s, http.StatusBadRequest)
				return
			}
			mids = append(mids, id)
		}
		groups = append(groups, repo.MergeGroup{
			CanonicalName: g.CanonicalName,
			MemberIDs:     mids,
		})
	}
	if err := h.Topics.MergeGroups(r.Context(), groups); err != nil {
		http.Error(w, "合并失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"merged": true})
}
