// entity 端点：设置页「专有名词」子区的后端——实体纠错配置 + 手动实体 CRUD。
// 路由经 registerEntityRoutes 挂载（RegisterAgent 调用）；鉴权/JSON 辅助复用 handlers.go。
package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/entity"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// registerEntityRoutes 挂实体端点（RegisterAgent 调用；测试单独装配也走这里）。
func registerEntityRoutes(r chi.Router, h *AgentHandler) {
	r.Get("/api/agent/entity-settings", h.getEntitySettings)
	r.Put("/api/agent/entity-settings", h.putEntitySettings)
	r.Get("/api/agent/entities", h.listEntities)
	r.Post("/api/agent/entities", h.createEntity)
	r.Patch("/api/agent/entities/{id}", h.patchEntity)
	r.Delete("/api/agent/entities/{id}", h.deleteEntity)
}

// getEntitySettings 返回纠错配置 + 各 kind 实体数汇总（设置页一次拉齐）。
func (h *AgentHandler) getEntitySettings(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntitySettings == nil || h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	st, err := h.EntitySettings.Get(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	counts, err := h.EntityKB.CountByKind(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"correction_enabled":   st.CorrectionEnabled,
		"confidence_threshold": st.ConfidenceThreshold,
		"auto_sources":         st.AutoSources,
		"counts_by_kind":       counts,
	})
}

// putEntitySettings 保存纠错配置。按指针合并：请求里未传的字段保持现值；
// threshold 越界 [0,1] → 400。
func (h *AgentHandler) putEntitySettings(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntitySettings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	var body struct {
		CorrectionEnabled   *bool     `json:"correction_enabled"`
		ConfidenceThreshold *float64  `json:"confidence_threshold"`
		AutoSources         *[]string `json:"auto_sources"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	cur, err := h.EntitySettings.Get(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	enabled, threshold, sources := cur.CorrectionEnabled, cur.ConfidenceThreshold, cur.AutoSources
	if body.CorrectionEnabled != nil {
		enabled = *body.CorrectionEnabled
	}
	if body.ConfidenceThreshold != nil {
		if *body.ConfidenceThreshold < 0 || *body.ConfidenceThreshold > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confidence_threshold 须在 [0,1]"})
			return
		}
		threshold = *body.ConfidenceThreshold
	}
	if body.AutoSources != nil {
		sources = *body.AutoSources
	}
	if err := h.EntitySettings.Upsert(r.Context(), uid, enabled, threshold, sources); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"correction_enabled": enabled, "confidence_threshold": threshold, "auto_sources": sources,
	})
}

// listEntities 列实体（?kind= 过滤；含 auto+manual+禁用行，设置页分组展示）。
func (h *AgentHandler) listEntities(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	list, err := h.EntityKB.List(r.Context(), uid, r.URL.Query().Get("kind"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entities": list})
}

// createEntity 手动新增专有名词（拼音/音素键服务端算，客户端不传）。kind 缺省 custom。
func (h *AgentHandler) createEntity(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	var body struct {
		Canonical string `json:"canonical"`
		Kind      string `json:"kind"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	body.Canonical = strings.TrimSpace(body.Canonical)
	if body.Canonical == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "canonical required"})
		return
	}
	if body.Kind == "" {
		body.Kind = repo.EntityKindCustom
	}
	e := &repo.Entity{UserID: uid, Canonical: body.Canonical, Kind: body.Kind, Enabled: true}
	py := entity.NormalizePinyin(body.Canonical)
	if py != "" {
		e.Pinyin = &py
	}
	if lt := entity.NormalizeLatin(body.Canonical); lt != "" && lt != py {
		e.Metaphone = &lt
	}
	if body.Note != "" {
		e.Note = &body.Note
	}
	if err := h.EntityKB.CreateManual(r.Context(), e); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// patchEntity 改实体：enabled 对 manual/auto 都可改；canonical/note 只许 manual
// （auto 由刷新重建，改名会被覆盖——想调整来源数据去对应平面改）。改名时服务端重算匹配键。
func (h *AgentHandler) patchEntity(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var body struct {
		Canonical *string `json:"canonical"`
		Note      *string `json:"note"`
		Enabled   *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if body.Enabled != nil {
		if err := h.EntityKB.SetEnabled(r.Context(), uid, id, *body.Enabled); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
			return
		}
	}
	if body.Canonical != nil || body.Note != nil {
		cur, err := h.EntityKB.Get(r.Context(), uid, id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
			return
		}
		if cur.Source != repo.EntitySourceManual {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "自动同步的实体不可改名，可禁用或改来源数据"})
			return
		}
		canonical, note := cur.Canonical, ""
		if cur.Note != nil {
			note = *cur.Note
		}
		if body.Canonical != nil {
			canonical = strings.TrimSpace(*body.Canonical)
			if canonical == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "canonical required"})
				return
			}
		}
		if body.Note != nil {
			note = *body.Note
		}
		// canonical 变更须重算匹配键（旧键会让召回失配）；note-only 变更也按现名重算，
		// 值不变即幂等无感。
		py := entity.NormalizePinyin(canonical)
		lt := entity.NormalizeLatin(canonical)
		var pyPtr, ltPtr *string
		if py != "" {
			pyPtr = &py
		}
		if lt != "" && lt != py {
			ltPtr = &lt
		}
		if err := h.EntityKB.UpdateManual(r.Context(), uid, id, canonical, note, pyPtr, ltPtr); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	full, err := h.EntityKB.Get(r.Context(), uid, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
		return
	}
	writeJSON(w, http.StatusOK, full)
}

// deleteEntity 删除实体（manual 删除即消失；auto 删除后下次刷新会回来——想持久
// 不参与纠错用 PATCH enabled=false）。
func (h *AgentHandler) deleteEntity(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := h.EntityKB.Delete(r.Context(), uid, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
