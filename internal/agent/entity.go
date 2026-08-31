// entity 端点：设置页「专有名词」子区的后端——实体纠错配置 + 手动实体 CRUD。
// 路由经 registerEntityRoutes 挂载（RegisterAgent 调用）；鉴权/JSON 辅助复用 handlers.go。
package agent

import (
	"context"
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

// entityView 是设置页实体列表的响应 DTO。ID 用字符串：manual 是真 snowflake；
// auto 实时聚合无稳定行，用 "auto:<canonical>" 合成 id（PATCH 据此消歧启禁）。
type entityView struct {
	ID        string  `json:"id"`
	Canonical string  `json:"canonical"`
	Kind      string  `json:"kind"`
	Pinyin    *string `json:"pinyin,omitempty"`
	Metaphone *string `json:"metaphone,omitempty"`
	Source    string  `json:"source"` // manual | auto
	SourceRef *string `json:"source_ref,omitempty"`
	Enabled   bool    `json:"enabled"`
	Note      *string `json:"note,omitempty"`
}

// autoIDPrefix 自动实体合成 id 前缀（manual 用真 snowflake，auto 无行 → "auto:<canonical>"）。
const autoIDPrefix = "auto:"

// buildEntityList 组装设置页实体列表（去拷贝化）：manual（entity_kb 全量含禁用）+
// auto（实时聚合，enabled=未被禁用；同名被 manual 覆盖则不重复列出——对齐「同名只留一条」）。
// kind 空串=全部。best-effort：auto 聚合/禁用名单读失败时降级为仅 manual，不报错。
func (h *AgentHandler) buildEntityList(ctx context.Context, uid int64, kind string) ([]entityView, error) {
	manual, err := h.EntityKB.List(ctx, uid, "")
	if err != nil {
		return nil, err
	}
	manualByCanon := map[string]bool{}
	var out []entityView
	for i := range manual {
		e := &manual[i]
		if e.Source != repo.EntitySourceManual {
			continue
		}
		manualByCanon[strings.ToLower(e.Canonical)] = true
		out = append(out, entityView{
			ID: e.ID.String(), Canonical: e.Canonical, Kind: e.Kind, Source: "manual",
			Enabled: e.Enabled, Note: e.Note, Pinyin: e.Pinyin, Metaphone: e.Metaphone, SourceRef: e.SourceRef,
		})
	}
	// auto 实时聚合（需来源 repo 装配；enabled=!禁用；被 manual 同 canonical 覆盖的跳过）。
	if h.EntitySeed.Persons != nil || h.EntitySeed.Speakers != nil || h.EntitySeed.Pets != nil || h.EntitySeed.Topics != nil {
		var sources []string
		if h.EntitySettings != nil {
			if st, gerr := h.EntitySettings.Get(ctx, uid); gerr == nil {
				sources = st.AutoSources
			}
		}
		if auto, aerr := entity.AssembleEntities(ctx, h.EntitySeed, uid, sources); aerr == nil {
			var disabled map[string]bool
			if h.EntityDisabled != nil {
				disabled, _ = h.EntityDisabled.ListDisabled(ctx, uid)
			}
			for i := range auto {
				e := &auto[i]
				if manualByCanon[strings.ToLower(e.Canonical)] {
					continue // manual 同名覆盖，不重复列出
				}
				key := strings.ToLower(e.Canonical)
				out = append(out, entityView{
					ID: autoIDPrefix + e.Canonical, Canonical: e.Canonical, Kind: e.Kind, Source: "auto",
					Enabled: !disabled[key], Pinyin: e.Pinyin, Metaphone: e.Metaphone, SourceRef: e.SourceRef,
				})
			}
		}
	}
	if kind != "" {
		filtered := out[:0]
		for _, v := range out {
			if v.Kind == kind {
				filtered = append(filtered, v)
			}
		}
		out = filtered
	}
	if out == nil {
		out = []entityView{}
	}
	return out, nil
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
	// counts_by_kind：对组装后的实体列表(manual + 实时 auto)按 kind 统计启用数
	// （entity_kb.CountByKind 去拷贝化后只数得到 manual，故改为对白名单计数）。
	list, lerr := h.buildEntityList(r.Context(), uid, "")
	if lerr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": lerr.Error()})
		return
	}
	counts := map[string]int{}
	for _, v := range list {
		if v.Enabled {
			counts[v.Kind]++
		}
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

// listEntities 列实体（?kind= 过滤）：manual(entity_kb) + auto(实时聚合,enabled=!禁用)，
// 同名 manual 覆盖 auto 不重复。设置页据此展示；auto 无稳定行，id 为 "auto:<canonical>"。
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
	list, err := h.buildEntityList(r.Context(), uid, r.URL.Query().Get("kind"))
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
	// 超长守卫：DB VARCHAR(128) 按「字符」计，INSERT IGNORE 会把 Data-too-long 降级为
	// 警告并截断（响应却回显全文，与库内值背离）——与种子层 addSeedEntity 同一上限，显式 400。
	if len([]rune(body.Canonical)) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "canonical 过长（上限 128 字符）"})
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

// patchEntity 改实体启禁。id 分两种：
//   - "auto:<canonical>"：实时聚合的自动实体，只支持 enabled 切换（写 entity_disabled 持久停用）；
//     不可改名（改来源数据去对应平面）。
//   - 真 snowflake：manual 实体，走 entity_kb（enabled 启禁 + canonical/note 改名，重算匹配键）。
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
	raw := chi.URLParam(r, "id")
	var body struct {
		Canonical *string `json:"canonical"`
		Note      *string `json:"note"`
		Enabled   *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	// auto 分支：只支持 enabled 切换（禁用名单持久化）。
	if strings.HasPrefix(raw, autoIDPrefix) {
		if h.EntityDisabled == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
			return
		}
		if body.Canonical != nil || body.Note != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "自动实体不可改名，可禁用或改来源数据"})
			return
		}
		if body.Enabled == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "自动实体只可启禁（enabled）"})
			return
		}
		canonical := strings.TrimPrefix(raw, autoIDPrefix)
		var derr error
		if *body.Enabled {
			derr = h.EntityDisabled.Clear(r.Context(), uid, canonical)
		} else {
			derr = h.EntityDisabled.SetDisabled(r.Context(), uid, canonical)
		}
		if derr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": derr.Error()})
			return
		}
		// 回显该 auto 的最新 view（从组装列表取，保证 kind/pinyin/enabled 一致）。
		if list, lerr := h.buildEntityList(r.Context(), uid, ""); lerr == nil {
			for _, v := range list {
				if v.ID == raw {
					writeJSON(w, http.StatusOK, v)
					return
				}
			}
		}
		writeJSON(w, http.StatusOK, entityView{ID: raw, Canonical: canonical, Source: "auto", Enabled: *body.Enabled})
		return
	}

	// manual 分支：走 entity_kb。
	id, err := ids.ParseID(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
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

// deleteEntity 删除实体。manual 删除即消失；auto("auto:" 前缀)由来源实时重建、删除无意义，
// 返回 400 提示用「禁用」持久停用（前端也已对 auto 隐藏删除按钮）。
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
	raw := chi.URLParam(r, "id")
	if strings.HasPrefix(raw, autoIDPrefix) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "自动实体不可删除，请用「禁用」持久停用"})
		return
	}
	id, err := ids.ParseID(raw)
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
