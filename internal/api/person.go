package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
)

// PersonHandler 人物画像 API：名册 CRUD、属性/关系手动管理、修改历史、
// 确认队列（跨平面 pending 并集）、按需回填抽取。
// 读操作直连 repo；一切变更走 profile.Service（保证审计+事务只实现一次）。
type PersonHandler struct {
	Persons       *repo.PersonRepo
	Attributes    *repo.PersonAttributeRepo
	Relationships *repo.PersonRelationshipRepo
	ChangeLogs    *repo.PersonChangeLogRepo
	Service       *profile.Service
}

func RegisterPerson(r chi.Router, h *PersonHandler) {
	r.Get("/api/persons", h.List)
	r.Post("/api/persons", h.Create)
	r.Get("/api/persons/{id}", h.Get)
	r.Patch("/api/persons/{id}", h.Patch)
	r.Delete("/api/persons/{id}", h.Delete)

	r.Post("/api/persons/{id}/attributes", h.AddAttribute)
	r.Patch("/api/persons/{id}/attributes/{aid}", h.PatchAttribute)
	r.Delete("/api/persons/{id}/attributes/{aid}", h.DeleteAttribute)
	r.Post("/api/persons/{id}/relationships", h.AddRelationship)
	r.Delete("/api/persons/{id}/relationships/{rid}", h.DeleteRelationship)
	r.Get("/api/persons/{id}/history", h.History)

	r.Get("/api/profile/pending", h.ListPending)
	r.Post("/api/profile/pending/{kind}/{id}/confirm", h.ConfirmPending)
	r.Post("/api/profile/pending/{kind}/{id}/dismiss", h.DismissPending)
	r.Post("/api/profile/extract", h.Extract)
}

// validPersonStatuses 是 person 状态机的合法取值（Patch 状态流转白名单）。
var validPersonStatuses = map[string]bool{
	"active": true, "pending": true, "merged": true, "dismissed": true,
}

// validPendingKinds 是确认队列 kind 的合法取值（confirm/dismiss 端点白名单）。
var validPendingKinds = map[string]bool{
	"person": true, "attribute": true, "relationship": true,
}

// ---- 名册 ----

func (h *PersonHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.Persons.ListWithPending(r.Context(), 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"persons": list})
}

func (h *PersonHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName string  `json:"display_name"`
		SpeakerID   string  `json:"speaker_id"`
		Summary     *string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		http.Error(w, "display_name 不能为空", http.StatusBadRequest)
		return
	}
	var speakerID *ids.ID
	if req.SpeakerID != "" {
		id, err := ids.ParseID(req.SpeakerID)
		if err != nil {
			http.Error(w, "speaker_id 非法", http.StatusBadRequest)
			return
		}
		speakerID = &id
		// 换绑冲突：声纹已被别人绑定 → 409
		if p, err := h.Persons.GetBySpeaker(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if p != nil {
			http.Error(w, "该声纹已绑定人物「"+p.DisplayName+"」", http.StatusConflict)
			return
		}
	}
	p, err := h.Service.ManualCreatePerson(r.Context(), name, speakerID, req.Summary)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, p)
}

// attrGroup 详情页的属性分区。
type attrGroup struct {
	Group string                 `json:"group"`
	Attrs []repo.PersonAttribute `json:"attrs"`
}

type personDetailResp struct {
	Person           *repo.Person              `json:"person"`
	Groups           []attrGroup               `json:"groups"`
	Relationships    []repo.PersonRelationship `json:"relationships"`
	RecentSessionIDs []ids.ID                  `json:"recent_session_ids"`
	PendingCount     int                       `json:"pending_count"`
}

func (h *PersonHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	p, err := h.Persons.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
		return
	}
	attrs, err := h.Attributes.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rels, err := h.Relationships.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sids, err := h.Persons.RecentSessionIDs(r.Context(), id, 10)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 分组：只展示 active+pending；组顺序按 GroupOrder，目录外 key 的「其他」最后。
	byGroup := map[string][]repo.PersonAttribute{}
	pending := 0
	for _, a := range attrs {
		if a.Status != "active" && a.Status != "pending" {
			continue
		}
		byGroup[profile.Def(a.AttrKey).Group] = append(byGroup[profile.Def(a.AttrKey).Group], a)
		if a.Status == "pending" {
			pending++
		}
	}
	groups := make([]attrGroup, 0, len(byGroup))
	for _, g := range profile.GroupOrder {
		if as := byGroup[g]; len(as) > 0 {
			groups = append(groups, attrGroup{Group: g, Attrs: as})
			delete(byGroup, g)
		}
	}
	relShown := make([]repo.PersonRelationship, 0, len(rels))
	for _, rel := range rels {
		if rel.Status != "active" && rel.Status != "pending" {
			continue
		}
		relShown = append(relShown, rel)
		if rel.Status == "pending" {
			pending++
		}
	}
	writeJSON(w, personDetailResp{
		Person: p, Groups: groups, Relationships: relShown,
		RecentSessionIDs: sids, PendingCount: pending,
	})
}

func (h *PersonHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		DisplayName string  `json:"display_name"`
		SpeakerID   *string `json:"speaker_id"` // nil=不改；""=解绑；"123"=换绑
		Summary     *string `json:"summary"`
		Status      string  `json:"status"` // 传了则走状态流转
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	p, err := h.Persons.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
		return
	}
	if req.Status != "" {
		if !validPersonStatuses[req.Status] {
			http.Error(w, "status 非法（active|pending|merged|dismissed）", http.StatusBadRequest)
			return
		}
		if err := h.Service.ManualSetPersonStatus(r.Context(), id, req.Status); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	name := p.DisplayName
	if req.DisplayName != "" {
		name = strings.TrimSpace(req.DisplayName)
		if name == "" {
			http.Error(w, "display_name 不能为空", http.StatusBadRequest)
			return
		}
	}
	var speakerID *ids.ID
	if req.SpeakerID != nil && *req.SpeakerID != "" {
		sid, err := ids.ParseID(*req.SpeakerID)
		if err != nil {
			http.Error(w, "speaker_id 非法", http.StatusBadRequest)
			return
		}
		if other, err := h.Persons.GetBySpeaker(r.Context(), sid); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if other != nil && other.ID != id {
			http.Error(w, "该声纹已绑定人物「"+other.DisplayName+"」", http.StatusConflict)
			return
		}
		speakerID = &sid
	} else if req.SpeakerID != nil {
		// 传空串 = 解绑
		speakerID = nil
	} else {
		speakerID = p.SpeakerID // 不改
	}
	// summary 部分更新语义（与 display_name/speaker_id 的部分更新保持一致）：
	// 不传（nil）→ 保留现值；传空串 → 显式清空（SQL NULL）；传非空 → 更新。
	summary := p.Summary
	if req.Summary != nil {
		if *req.Summary == "" {
			summary = nil
		} else {
			summary = req.Summary
		}
	}
	if err := h.Service.ManualUpdatePerson(r.Context(), id, name, speakerID, summary); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *PersonHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	if p, err := h.Persons.Get(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
		return
	}
	if err := h.Service.ManualSetPersonStatus(r.Context(), id, "dismissed"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 属性 ----

func (h *PersonHandler) AddAttribute(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		AttrKey string `json:"attr_key"`
		Value   string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(req.AttrKey)
	val := strings.TrimSpace(req.Value)
	if key == "" || val == "" {
		http.Error(w, "attr_key 与 value 必填", http.StatusBadRequest)
		return
	}
	a, err := h.Service.ManualAddAttribute(r.Context(), pid, key, val)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, a)
}

func (h *PersonHandler) PatchAttribute(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	aid, err := ids.ParseID(chi.URLParam(r, "aid"))
	if err != nil {
		http.Error(w, "aid 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		AttrKey string `json:"attr_key"` // 可选；传了必须与目标行一致（否则 400），防改到别的 key 的行
		Value   string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	val := strings.TrimSpace(req.Value)
	if val == "" {
		http.Error(w, "value 必填", http.StatusBadRequest)
		return
	}
	// 校验目标行存在且属于该人物
	a, err := h.Attributes.Get(r.Context(), aid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if a == nil || a.PersonID != pid {
		http.Error(w, "属性不存在", http.StatusNotFound)
		return
	}
	// attr_key 一律以目标行为准；body 若带 attr_key 必须与之一致，否则会 supersede 到
	// 另一个 key 的 active 行、目标行原样不动（静默改错对象）——直接 400 拒绝。
	if k := strings.TrimSpace(req.AttrKey); k != "" && k != a.AttrKey {
		http.Error(w, "attr_key 与目标属性不一致", http.StatusBadRequest)
		return
	}
	// 手动改值 = 同 key 写新值（ManualAddAttribute 内部 supersede 旧 active 行）
	na, err := h.Service.ManualAddAttribute(r.Context(), pid, a.AttrKey, val)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, na)
}

func (h *PersonHandler) DeleteAttribute(w http.ResponseWriter, r *http.Request) {
	aid, err := ids.ParseID(chi.URLParam(r, "aid"))
	if err != nil {
		http.Error(w, "aid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteAttribute(r.Context(), aid); err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "属性不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 关系 ----

func (h *PersonHandler) AddRelationship(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		RelationType    string `json:"relation_type"`
		RelatedPersonID string `json:"related_person_id"`
		Direction       string `json:"direction"`
		OrgName         string `json:"org_name"`
		Label           string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if !profile.ValidRelations[req.RelationType] {
		http.Error(w, "relation_type 非法", http.StatusBadRequest)
		return
	}
	var related *ids.ID
	if req.RelatedPersonID != "" {
		rid, err := ids.ParseID(req.RelatedPersonID)
		if err != nil {
			http.Error(w, "related_person_id 非法", http.StatusBadRequest)
			return
		}
		related = &rid
	}
	rel, err := h.Service.ManualAddRelationship(r.Context(), pid, req.RelationType,
		related, req.Direction, req.OrgName, req.Label)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rel)
}

func (h *PersonHandler) DeleteRelationship(w http.ResponseWriter, r *http.Request) {
	rid, err := ids.ParseID(chi.URLParam(r, "rid"))
	if err != nil {
		http.Error(w, "rid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteRelationship(r.Context(), rid); err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "关系不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 修改历史 ----

func (h *PersonHandler) History(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	logs, err := h.ChangeLogs.ListByPerson(r.Context(), id,
		r.URL.Query().Get("entity_kind"), r.URL.Query().Get("attr_key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"history": logs})
}

// ---- 确认队列（跨平面 pending 并集）----

type pendingItem struct {
	Kind          string  `json:"kind"` // attribute|relationship|person
	ID            ids.ID  `json:"id"`
	PersonID      ids.ID  `json:"person_id"`
	PersonName    string  `json:"person_name"`
	AttrKey       string  `json:"attr_key,omitempty"`
	Value         string  `json:"value,omitempty"`         // attribute:建议值 / relationship:类型 / person:名字
	CurrentValue  string  `json:"current_value,omitempty"` // 冲突时的现值（supersedes 行）
	RelationType  string  `json:"relation_type,omitempty"`
	Label         string  `json:"label,omitempty"`
	Confidence    float64 `json:"confidence"`
	EpistemicType string  `json:"epistemic_type"`
	SessionID     *ids.ID `json:"session_id,omitempty"`
	SupersedesID  *ids.ID `json:"supersedes_id,omitempty"`
}

func (h *PersonHandler) ListPending(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	persons, err := h.Persons.List(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nameOf := make(map[ids.ID]string, len(persons))
	for _, p := range persons {
		nameOf[p.ID] = p.DisplayName
	}
	var items []pendingItem

	attrs, err := h.Attributes.ListPending(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, a := range attrs {
		it := pendingItem{
			Kind: "attribute", ID: a.ID, PersonID: a.PersonID, PersonName: nameOf[a.PersonID],
			AttrKey: a.AttrKey, Value: a.ValueText, Confidence: a.Confidence,
			EpistemicType: a.EpistemicType, SessionID: a.SessionID, SupersedesID: a.SupersedesID,
		}
		if a.SupersedesID != nil {
			if cur, err := h.Attributes.Get(ctx, *a.SupersedesID); err == nil && cur != nil {
				it.CurrentValue = cur.ValueText
			}
		}
		items = append(items, it)
	}

	rels, err := h.Relationships.ListPending(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, rel := range rels {
		label := ""
		if rel.Label != nil {
			label = *rel.Label
		}
		items = append(items, pendingItem{
			Kind: "relationship", ID: rel.ID, PersonID: rel.PersonID, PersonName: nameOf[rel.PersonID],
			RelationType: rel.RelationType, Value: rel.RelationType, Label: label,
			Confidence: rel.Confidence, EpistemicType: rel.EpistemicType,
			SessionID: rel.SessionID, SupersedesID: rel.SupersedesID,
		})
	}

	for _, p := range persons {
		if p.Status == "pending" {
			items = append(items, pendingItem{
				Kind: "person", ID: p.ID, PersonID: p.ID, PersonName: p.DisplayName,
				Value: p.DisplayName, Confidence: 0.5, EpistemicType: "observed",
			})
		}
	}
	writeJSON(w, map[string]any{"items": items})
}

func (h *PersonHandler) ConfirmPending(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	if !validPendingKinds[kind] {
		http.Error(w, "kind 非法（person|attribute|relationship）", http.StatusBadRequest)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ConfirmPending(r.Context(), kind, id); err != nil {
		writePendingErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *PersonHandler) DismissPending(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	if !validPendingKinds[kind] {
		http.Error(w, "kind 非法（person|attribute|relationship）", http.StatusBadRequest)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.DismissPending(r.Context(), kind, id); err != nil {
		writePendingErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func writePendingErr(w http.ResponseWriter, err error) {
	if errors.Is(err, profile.ErrNotFound) {
		http.Error(w, "记录不存在", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// ---- 按需回填抽取 ----

type extractResult struct {
	SessionID  ids.ID `json:"session_id"`
	Facts      int    `json:"facts"`
	Active     int    `json:"active"`
	Pending    int    `json:"pending"`
	Reaffirmed int    `json:"reaffirmed"`
	Conflicts  int    `json:"conflicts"`
	Skipped    int    `json:"skipped"`
	Windows    int    `json:"windows"`
	Tokens     int    `json:"tokens"`
	Error      string `json:"error,omitempty"`
}

// Extract 触发画像抽取：带 session_id 抽单个；不带则全量回填最近的 completed
// session（上限 50，防单请求过久）。同步执行（MVP 规模）。
func (h *PersonHandler) Extract(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // 空 body 合法
	ctx := r.Context()

	var sids []ids.ID
	if req.SessionID != "" {
		sid, err := ids.ParseID(req.SessionID)
		if err != nil {
			http.Error(w, "session_id 非法", http.StatusBadRequest)
			return
		}
		sids = []ids.ID{sid}
	} else {
		var err error
		sids, err = h.Service.Sessions.ListCompletedIDs(ctx, 50)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	results := make([]extractResult, 0, len(sids))
	for _, sid := range sids {
		res, err := h.Service.ExtractSession(ctx, sid)
		er := extractResult{SessionID: sid}
		if err != nil {
			er.Error = err.Error() // 单个失败不中断批量
		} else {
			er.Facts, er.Active, er.Pending = res.Apply.Total, res.Apply.Active, res.Apply.Pending
			er.Reaffirmed, er.Conflicts, er.Skipped = res.Apply.Reaffirmed, res.Apply.Conflicts, res.Apply.Skipped
			er.Windows, er.Tokens = res.Windows, res.Tokens
		}
		results = append(results, er)
	}
	writeJSON(w, map[string]any{"processed": len(results), "results": results})
}
