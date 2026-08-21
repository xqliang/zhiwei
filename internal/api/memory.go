package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// MemoryHandler 处理 memory 查询与修正。
type MemoryHandler struct {
	Memories     *repo.MemoryRepo
	Topics       *repo.TopicRepo       // 校验 topic 存在
	MemoryTopics *repo.MemoryTopicRepo // 手动加/删 memory↔topic 关联

	// LLM 用于 consolidate 提议（merge 不调 LLM）。main.go 注入；测试可传 fake。
	LLM provider.LLMProvider
	// LLMModel 是 fast 模型名（cfg.LLMFastModel）。
	LLMModel string
	// ConsolidatePrompt 是 prompts/memory_consolidate_v1.md 的内容（系统指令）。
	ConsolidatePrompt string
}

// RegisterMemory 挂载 memory 路由。
func RegisterMemory(r chi.Router, h *MemoryHandler) {
	r.Get("/api/memories", h.List)
	r.Patch("/api/memories/{id}", h.Patch)
	r.Post("/api/memories/{id}/topics", h.AddTopic)
	r.Delete("/api/memories/{id}/topics/{topic_id}", h.RemoveTopic)
	r.Post("/api/memories/consolidate", h.Consolidate)
	r.Post("/api/memories/merge", h.Merge)
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

// AddTopic 手动给 memory 加 topic 关联（source='user'，INSERT IGNORE 幂等）。
// 校验顺序：参数合法性（400）→ memory 存在（404）→ topic 存在且非 dismissed（404）。
func (h *MemoryHandler) AddTopic(w http.ResponseWriter, r *http.Request) {
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
	if _, err := h.Memories.Get(r.Context(), id); err != nil {
		http.Error(w, "memory 不存在", http.StatusNotFound)
		return
	}
	tp, err := h.Topics.Get(r.Context(), tid)
	if err != nil || tp.Status == "dismissed" {
		http.Error(w, "topic 不存在", http.StatusNotFound)
		return
	}
	if err := h.MemoryTopics.AddLink(r.Context(), id, tid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// RemoveTopic 移除 memory↔topic 关联。幂等：关联不存在也不报错（DELETE 返回 204）。
func (h *MemoryHandler) RemoveTopic(w http.ResponseWriter, r *http.Request) {
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
	if err := h.MemoryTopics.RemoveLink(r.Context(), id, tid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Consolidate 调 LLM 生成整理提议：输入该用户全部 active 记忆，输出合并组 + 每条记忆
// 的关系判定（merges + adjustments），不改库。LLM 只判关系不给置信度数字；confidence 数字
// 由 Merge 的规则（SQL 原子）算。流程：ListActive → 组 user 消息 → LLM.Chat → 容错解析 → 原样回传。
func (h *MemoryHandler) Consolidate(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		http.Error(w, "LLM 未配置", http.StatusInternalServerError)
		return
	}
	list, err := h.Memories.ListActive(r.Context(), 1, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type memItem struct {
		ID            string  `json:"id"`
		Type          string  `json:"type"`
		Title         string  `json:"title"`
		Content       string  `json:"content"`
		EpistemicType string  `json:"epistemic_type"`
		Confidence    float64 `json:"confidence"`
		EventAt       string  `json:"event_at"`
	}
	items := make([]memItem, 0, len(list))
	for _, m := range list {
		ea := ""
		if m.EventAt != nil {
			ea = m.EventAt.Format(time.RFC3339)
		}
		items = append(items, memItem{
			ID: m.ID.String(), Type: m.Type, Title: m.Title, Content: m.Content,
			EpistemicType: m.EpistemicType, Confidence: m.Confidence, EventAt: ea,
		})
	}
	userMsg, err := json.Marshal(items)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := h.LLM.Chat(r.Context(), provider.ChatRequest{
		Model:  h.LLMModel,
		System: h.ConsolidatePrompt,
		User:   string(userMsg),
	})
	if err != nil {
		http.Error(w, "LLM 调用失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 容错解析：截取首个 { 到末个 }，剥掉前后废话/markdown 围栏（与 candidate.go ParseCandidates 同思路）
	raw := strings.TrimSpace(resp.Content)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var out struct {
		Merges []struct {
			CanonicalID string   `json:"canonical_id"`
			MemberIDs   []string `json:"member_ids"`
		} `json:"merges"`
		Adjustments []struct {
			MemoryID    string   `json:"memory_id"`
			Kind        string   `json:"kind"`
			Reason      string   `json:"reason"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"adjustments"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		http.Error(w, "整理提议解析失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 原样回传提议，不改库（用户确认后走 /api/memories/merge）
	writeJSON(w, map[string]any{"merges": out.Merges, "adjustments": out.Adjustments})
}

// Merge 用户确认后单事务落库整理：body 含 merges + adjustments（id 均为字符串）。
// ids.ParseID 转 []ids.ID 组 repo.ConsolidationReq 交 ApplyConsolidation。先 merges
// （member 关联迁 canonical + member 置 superseded），后 adjustments（跳过已 supersede 的
// member，按 kind 规则算 confidence，SQL 原子）。不调 LLM。
func (h *MemoryHandler) Merge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Merges []struct {
			CanonicalID string   `json:"canonical_id"`
			MemberIDs   []string `json:"member_ids"`
		} `json:"merges"`
		Adjustments []struct {
			MemoryID    string   `json:"memory_id"`
			Kind        string   `json:"kind"`
			Reason      string   `json:"reason"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"adjustments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	cr := repo.ConsolidationReq{
		Merges:      make([]repo.MemoryMerge, 0, len(req.Merges)),
		Adjustments: make([]repo.MemoryAdjustment, 0, len(req.Adjustments)),
	}
	for _, g := range req.Merges {
		canon, err := ids.ParseID(g.CanonicalID)
		if err != nil {
			http.Error(w, "非法 canonical_id: "+g.CanonicalID, http.StatusBadRequest)
			return
		}
		mids := make([]ids.ID, 0, len(g.MemberIDs))
		for _, s := range g.MemberIDs {
			id, err := ids.ParseID(s)
			if err != nil {
				http.Error(w, "非法 member_id: "+s, http.StatusBadRequest)
				return
			}
			mids = append(mids, id)
		}
		cr.Merges = append(cr.Merges, repo.MemoryMerge{CanonicalID: canon, MemberIDs: mids})
	}
	for _, a := range req.Adjustments {
		mid, err := ids.ParseID(a.MemoryID)
		if err != nil {
			http.Error(w, "非法 memory_id: "+a.MemoryID, http.StatusBadRequest)
			return
		}
		eids := make([]ids.ID, 0, len(a.EvidenceIDs))
		for _, s := range a.EvidenceIDs {
			id, err := ids.ParseID(s)
			if err != nil {
				http.Error(w, "非法 evidence_id: "+s, http.StatusBadRequest)
				return
			}
			eids = append(eids, id)
		}
		cr.Adjustments = append(cr.Adjustments, repo.MemoryAdjustment{
			MemoryID: mid, Kind: a.Kind, Reason: a.Reason, EvidenceIDs: eids,
		})
	}
	merged, adjusted, err := h.Memories.ApplyConsolidation(r.Context(), cr)
	if err != nil {
		http.Error(w, "整理失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"applied": true, "merged": merged, "adjusted": adjusted})
}
