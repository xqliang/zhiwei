package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

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
	Events        *repo.PersonEventRepo
	Metrics       *repo.PersonMetricRepo
	Cycles        *repo.PersonCycleRepo
	Activities    *repo.PersonActivityRepo
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
	r.Get("/api/persons/{id}/events", h.ListEvents)
	r.Post("/api/persons/{id}/events", h.AddEvent)
	r.Delete("/api/persons/{id}/events/{eid}", h.DeleteEvent)
	r.Get("/api/persons/{id}/metrics", h.ListMetrics)
	r.Post("/api/persons/{id}/metrics", h.AddMetric)
	r.Delete("/api/persons/{id}/metrics/{mid}", h.DeleteMetric)
	r.Get("/api/persons/{id}/cycles", h.ListCycles)
	r.Post("/api/persons/{id}/cycles", h.AddCycle)
	r.Delete("/api/persons/{id}/cycles/{cid}", h.DeleteCycle)
	r.Get("/api/persons/{id}/activities", h.ListActivities)
	r.Post("/api/persons/{id}/activities", h.AddActivity)
	r.Delete("/api/persons/{id}/activities/{aid}", h.DeleteActivity)
	r.Get("/api/persons/{id}/history", h.History)

	r.Get("/api/profile/catalog", h.Catalog)
	r.Get("/api/profile/pending", h.ListPending)
	r.Post("/api/profile/pending/{kind}/{id}/confirm", h.ConfirmPending)
	r.Post("/api/profile/pending/{kind}/{id}/dismiss", h.DismissPending)
	r.Post("/api/profile/extract", h.Extract)
}

// ---- 属性目录（F4 前端配套：受控输入元数据）----

// catalogAttr 是 GET /api/profile/catalog 的单条目录项：把 profile.AttrDef 的导出形态
// 转成 snake_case JSON 契约，供前端按 value_type 切换值输入控件（enum→select /
// bool→是否 / date→日期选择器）。非 enum 项的 enum_options 为 null（前端只在 enum
// 分支遍历它，null 天然安全）。
type catalogAttr struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Group       string   `json:"group"`
	ValueType   string   `json:"value_type"`
	EnumOptions []string `json:"enum_options"`
	Cardinality string   `json:"cardinality"`
}

// Catalog 返回属性目录全集（静态数据，无 DB 查询）。前端「加属性 / 就地改值」表单
// 据此把自由文本输入切换为受控控件——Task 1 写入端上闸后（enum 须精确命中、bool 只认
// true/false、date 须可解析），受控输入保证提交值天然合法，避免用户手输脏值被 400。
func (h *PersonHandler) Catalog(w http.ResponseWriter, r *http.Request) {
	defs := profile.All()
	out := make([]catalogAttr, 0, len(defs))
	for _, d := range defs {
		out = append(out, catalogAttr{
			Key: d.Key, Label: d.Label, Group: d.Group,
			ValueType: d.ValueType, EnumOptions: d.EnumOptions, Cardinality: d.Cardinality,
		})
	}
	writeJSON(w, map[string]any{"catalog": out})
}

// validPersonStatuses 是 person 状态机的合法取值（Patch 状态流转白名单）。
var validPersonStatuses = map[string]bool{
	"active": true, "pending": true, "merged": true, "dismissed": true,
}

// validPendingKinds 是确认队列 kind 的合法取值（confirm/dismiss 端点白名单）。
var validPendingKinds = map[string]bool{
	"person": true, "attribute": true, "relationship": true, "event": true,
	"metric": true, "cycle": true, "activity": true,
}

// ---- 名册 ----

// List 名册（active+pending）+ 每人 pending 角标计数。
// ?dismissed=1 返回已删除人物（软删行，折叠区查看/恢复）；默认返回非 dismissed（活跃名册）。
// 对齐 topics 的 ?dismissed=1 约定：两个视图分离，避免活跃名册混入已删数据。
func (h *PersonHandler) List(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("dismissed") == "1" {
		list, err := h.Persons.ListDismissed(r.Context(), 1)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"persons": list})
		return
	}
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
	Events           []repo.PersonEvent        `json:"events"`
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
	// 大事记（event 平面）：只展示 active+pending，时间倒序保持（ListByPerson 已按
	// occurred_at DESC 排）；pending 事件也计入详情页的 pending 计数。
	events, err := h.Events.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	evShown := make([]repo.PersonEvent, 0, len(events))
	for _, e := range events {
		if e.Status != "active" && e.Status != "pending" {
			continue
		}
		evShown = append(evShown, e)
		if e.Status == "pending" {
			pending++
		}
	}
	// metric/cycle/activity 平面的 pending 也计入详情页角标（确认队列已含这三类，名册/详情角标须一致）。
	// 详情不展示 metric/cycle/activity 列表（时序/轨迹数据量大、按需查询），故用轻量 COUNT 而非拉全表过滤。
	mp, err := h.Metrics.CountPendingByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cp, err := h.Cycles.CountPendingByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ap, err := h.Activities.CountPendingByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pending += mp + cp + ap
	writeJSON(w, personDetailResp{
		Person: p, Groups: groups, Relationships: relShown, Events: evShown,
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
		// F4 校验错误（值域不合法）→ 400（对齐 metric 枚举校验的 400 口径）；其余 → 500。
		if errors.Is(err, profile.ErrInvalidAttrValue) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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
		// F4 校验错误（值域不合法）→ 400（对齐 AddAttribute）；其余 → 500。
		if errors.Is(err, profile.ErrInvalidAttrValue) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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

// ---- 大事记（event 平面）----

// ListEvents 人物大事记（全状态，时间倒序——repo ListByPerson 已排序）。
// ?status=active 只看已生效（前端时间线默认态可过滤）。
func (h *PersonHandler) ListEvents(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	list, err := h.Events.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if st := r.URL.Query().Get("status"); st != "" {
		filtered := make([]repo.PersonEvent, 0, len(list))
		for _, e := range list {
			if e.Status == st {
				filtered = append(filtered, e)
			}
		}
		list = filtered
	}
	writeJSON(w, map[string]any{"events": list})
}

// AddEvent 手动加大事记（走 Service：active/manual/conf=1.0 + 审计）。
// event_type 9 枚举校验；occurred_at/endAt 原始串由 Service 的 parseEventAt 尽力解析。
// importance 可选（P2a①）：缺省/0 走事件类型默认，>0 由 Service clamp 到 (0,1]。
// 同行人物（P2a②）：related_person_ids 数组为主，旧单字段 related_person_id 非空时并入
// （向后兼容旧前端/调用方）；两者都空=无同行。任一 id 解析失败 → 400。
func (h *PersonHandler) AddEvent(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		EventType        string   `json:"event_type"`
		Title            string   `json:"title"`
		Description      string   `json:"description"`
		OccurredAt       string   `json:"occurred_at"`
		EndAt            string   `json:"end_at"`
		Location         string   `json:"location"`
		RelatedPersonID  string   `json:"related_person_id"`  // 单字段，向后兼容保留（P2a② 前）
		RelatedPersonIDs []string `json:"related_person_ids"` // P2a②：多人同行数组（为主）
		Importance       float64  `json:"importance"`         // P2a①：可选，0/缺省=事件类型默认，>0 clamp 到 (0,1]
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if !profile.ValidEventTypes[req.EventType] {
		http.Error(w, "event_type 非法", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		http.Error(w, "title 必填", http.StatusBadRequest)
		return
	}
	// 同行人物解析（P2a②）：数组为主，旧单字段非空时并入；两者都空 → related 为 nil（无同行）。
	var related []ids.ID
	for _, s := range req.RelatedPersonIDs {
		if strings.TrimSpace(s) == "" {
			continue // 容忍数组里的空串项
		}
		rid, err := ids.ParseID(s)
		if err != nil {
			http.Error(w, "related_person_ids 含非法 id", http.StatusBadRequest)
			return
		}
		related = append(related, rid)
	}
	if req.RelatedPersonID != "" {
		rid, err := ids.ParseID(req.RelatedPersonID)
		if err != nil {
			http.Error(w, "related_person_id 非法", http.StatusBadRequest)
			return
		}
		related = append(related, rid)
	}
	e, err := h.Service.ManualAddEvent(r.Context(), pid, req.EventType, req.Title,
		req.Description, req.OccurredAt, req.EndAt, req.Location, related, req.Importance)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, e)
}

// DeleteEvent 手动删事件 → dismissed + delete 审计。
func (h *PersonHandler) DeleteEvent(w http.ResponseWriter, r *http.Request) {
	eid, err := ids.ParseID(chi.URLParam(r, "eid"))
	if err != nil {
		http.Error(w, "eid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteEvent(r.Context(), eid); err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "事件不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 指标（metric 平面 · P3 时序）----

// parseDateParam 解析 metrics 时间窗查询参数（YYYY-MM-DD）→ 当日 00:00 UTC 指针。
// 空串 → (nil, nil)（不限该边界）；格式非法 → 返回错误（handler 转 400）。与 repo 半开区间
// [from, to) 配合：from 当日 00:00 起（含）、to 当日 00:00 止（不含）；测点 measured_at 也
// 锚定 UTC 当日零点（parseEventAt），故此处同锚 UTC，避免时区偏移导致边界串日期。
func parseDateParam(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// time.Parse 该 layout 无时区 → 直接得 UTC 当日零点，无需再 In(UTC)。
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListMetrics 时序指标（全状态，按测点时间升序——repo ListByPerson 已排序）。
// query：metric_key= 限定指标（空=全部）；from=/to=（YYYY-MM-DD）时间窗，半开区间 [from,to)
// 由 repo 定义——to 传当日 00:00 即「不含 to 当日」（前端要含 to 当日须传次日）；from/to 解析
// 失败 → 400。?status= 可选只看某状态（同 events 时间线默认态过滤）。
func (h *PersonHandler) ListMetrics(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	key := r.URL.Query().Get("metric_key")
	from, err := parseDateParam(r.URL.Query().Get("from"))
	if err != nil {
		http.Error(w, "from 非法（YYYY-MM-DD）", http.StatusBadRequest)
		return
	}
	to, err := parseDateParam(r.URL.Query().Get("to"))
	if err != nil {
		http.Error(w, "to 非法（YYYY-MM-DD）", http.StatusBadRequest)
		return
	}
	list, err := h.Metrics.ListByPerson(r.Context(), id, key, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if st := r.URL.Query().Get("status"); st != "" {
		filtered := make([]repo.PersonMetric, 0, len(list))
		for _, m := range list {
			if m.Status == st {
				filtered = append(filtered, m)
			}
		}
		list = filtered
	}
	writeJSON(w, map[string]any{"metrics": list})
}

// AddMetric 手动加测点（走 Service：active/manual/conf=1.0 + 审计）。
// metric_key 6 枚举校验；value 空 400；measured_at 原始串透传 Service（parseEventAt 尽力
// 解析，空/失败落 time.Now()）；数值/类别由 Service 分流 value_num/value_text。
func (h *PersonHandler) AddMetric(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		MetricKey  string `json:"metric_key"`
		Value      string `json:"value"`
		Unit       string `json:"unit"`
		MeasuredAt string `json:"measured_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if !profile.ValidMetricKeys[req.MetricKey] {
		http.Error(w, "metric_key 非法", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Value) == "" {
		http.Error(w, "value 必填", http.StatusBadRequest)
		return
	}
	m, err := h.Service.ManualAddMetric(r.Context(), pid, req.MetricKey, req.Value, req.Unit, req.MeasuredAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, m)
}

// DeleteMetric 手动删测点 → dismissed + delete 审计。
func (h *PersonHandler) DeleteMetric(w http.ResponseWriter, r *http.Request) {
	mid, err := ids.ParseID(chi.URLParam(r, "mid"))
	if err != nil {
		http.Error(w, "mid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteMetric(r.Context(), mid); err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "测点不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 周期/日程（cycle 平面 · P3，敏感）----

// cyclesDisclaimer 周期「下次预测」免责文案（spec §9）：next_predicted_at 是按历史周期
// （anchor+period）的纯时间估算，非医疗建议。随列表下发（响应体 note 字段），前端须展示。
const cyclesDisclaimer = "周期下次时间为按历史周期估算，仅供参考，非医疗建议"

// ListCycles 周期/日程列表（repo 按 cycle_type 分组、组内 id 排序）。默认只展示 active+pending
// （对齐详情 events 的过滤语义——单值语义下 superseded/dismissed 历史版本混入会干扰）；
// ?status= 显式过滤某状态（照 ListMetrics/ListEvents 的 status 写法）。响应体带 note 免责文案
// （spec §9），前端展示「下次预测」时须一并呈现。
func (h *PersonHandler) ListCycles(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	list, err := h.Cycles.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 默认只展示 active+pending（单值语义的历史版本 superseded/dismissed 混入会干扰）；
	// ?status= 显式过滤（对齐 ListMetrics/ListEvents）。
	if st := r.URL.Query().Get("status"); st != "" {
		filtered := make([]repo.PersonCycle, 0, len(list))
		for _, c := range list {
			if c.Status == st {
				filtered = append(filtered, c)
			}
		}
		list = filtered
	} else {
		filtered := make([]repo.PersonCycle, 0, len(list))
		for _, c := range list {
			if c.Status == "active" || c.Status == "pending" {
				filtered = append(filtered, c)
			}
		}
		list = filtered
	}
	writeJSON(w, map[string]any{"cycles": list, "note": cyclesDisclaimer})
}

// AddCycle 手动加周期/日程（走 Service：active/manual/conf=1.0 + 审计）。
// cycle_type 4 枚举校验；label 空→nil；anchor+period 齐时 Service 算 next_predicted_at。
// 注意 Service 签名 (…, frequency, dosage, …) 的形参顺序与 body 字段顺序相反，勿传错位。
func (h *PersonHandler) AddCycle(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		CycleType    string `json:"cycle_type"`
		Label        string `json:"label"`
		AnchorDate   string `json:"anchor_date"`
		PeriodDays   int    `json:"period_days"`
		DurationDays int    `json:"duration_days"`
		Dosage       string `json:"dosage"`
		Frequency    string `json:"frequency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if !profile.ValidCycleTypes[req.CycleType] {
		http.Error(w, "cycle_type 非法", http.StatusBadRequest)
		return
	}
	c, err := h.Service.ManualAddCycle(r.Context(), pid, req.CycleType, req.Label,
		req.AnchorDate, req.Frequency, req.Dosage, req.PeriodDays, req.DurationDays)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, c)
}

// DeleteCycle 手动删周期 → dismissed + delete 审计。
func (h *PersonHandler) DeleteCycle(w http.ResponseWriter, r *http.Request) {
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		http.Error(w, "cid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteCycle(r.Context(), cid); err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "周期不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 生活轨迹（activity 平面 · P4 测点流）----

// ListActivities 生活轨迹时间线（全状态，按开始时间升序——repo ListByPerson 已排序）。
// query：from=/to=（YYYY-MM-DD）时间窗，半开区间 [from,to)（与 ListMetrics 同 parseDateParam
// 语义——to 传当日 00:00 即「不含 to 当日」）；解析失败 → 400。?status= 可选只看某状态。
// 全状态返回（含 dismissed）：前端时间线用 status!==dismissed 客户端过滤（对齐 metric），软删行
// 仍随列表下发。注意 activity 无 metric_key 那样的二级维度——活动本身即类别，不再细分。
func (h *PersonHandler) ListActivities(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	from, err := parseDateParam(r.URL.Query().Get("from"))
	if err != nil {
		http.Error(w, "from 非法（YYYY-MM-DD）", http.StatusBadRequest)
		return
	}
	to, err := parseDateParam(r.URL.Query().Get("to"))
	if err != nil {
		http.Error(w, "to 非法（YYYY-MM-DD）", http.StatusBadRequest)
		return
	}
	list, err := h.Activities.ListByPerson(r.Context(), id, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if st := r.URL.Query().Get("status"); st != "" {
		filtered := make([]repo.PersonActivity, 0, len(list))
		for _, a := range list {
			if a.Status == st {
				filtered = append(filtered, a)
			}
		}
		list = filtered
	}
	writeJSON(w, map[string]any{"activities": list})
}

// AddActivity 手动加活动（走 Service：active/manual/conf=1.0 + 审计）。
// activity 空 400；tool/location/commute_mode 可空（Service trim 空→NULL）；started_at 原始串
// 透传 Service（parseEventAt 尽力解析，空/失败落 time.Now()）；duration_min 用 json int 的
// 0=未给（Service ≤0 不落列，不臆造 0 分钟）。返回裸行（对齐 AddMetric/AddCycle/AddEvent；列表端点才有 {activities:[]} 封套）。
func (h *PersonHandler) AddActivity(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		Activity    string `json:"activity"`
		Tool        string `json:"tool"`
		Location    string `json:"location"`
		CommuteMode string `json:"commute_mode"`
		StartedAt   string `json:"started_at"`
		DurationMin int    `json:"duration_min"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Activity) == "" {
		http.Error(w, "activity 必填", http.StatusBadRequest)
		return
	}
	a, err := h.Service.ManualAddActivity(r.Context(), pid, req.Activity, req.Tool,
		req.Location, req.CommuteMode, req.StartedAt, req.DurationMin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, a) // 返回裸行，对齐 AddMetric/AddCycle/AddEvent（列表端点才有 {activities:[...]} 包络）
}

// DeleteActivity 手动删活动 → dismissed + delete 审计（软删；全状态 GET 仍返回该行）。
func (h *PersonHandler) DeleteActivity(w http.ResponseWriter, r *http.Request) {
	aid, err := ids.ParseID(chi.URLParam(r, "aid"))
	if err != nil {
		http.Error(w, "aid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteActivity(r.Context(), aid); err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "活动不存在", http.StatusNotFound)
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
	Kind          string     `json:"kind"` // attribute|relationship|person|event|metric|cycle|activity
	ID            ids.ID     `json:"id"`
	PersonID      ids.ID     `json:"person_id"`
	PersonName    string     `json:"person_name"`
	AttrKey       string     `json:"attr_key,omitempty"`
	Value         string     `json:"value,omitempty"`         // attribute:建议值 / relationship:类型 / person:名字
	CurrentValue  string     `json:"current_value,omitempty"` // 冲突时的现值（supersedes 行）
	RelationType  string     `json:"relation_type,omitempty"`
	EventType     string     `json:"event_type,omitempty"`
	MetricKey     string     `json:"metric_key,omitempty"`
	CycleType     string     `json:"cycle_type,omitempty"`
	OccurredAt    *time.Time `json:"occurred_at,omitempty"`
	Label         string     `json:"label,omitempty"`
	Confidence    float64    `json:"confidence"`
	EpistemicType string     `json:"epistemic_type"`
	SessionID     *ids.ID    `json:"session_id,omitempty"`
	SupersedesID  *ids.ID    `json:"supersedes_id,omitempty"`
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

	events, err := h.Events.ListPending(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, e := range events {
		items = append(items, pendingItem{
			Kind: "event", ID: e.ID, PersonID: e.PersonID, PersonName: nameOf[e.PersonID],
			EventType: e.EventType, Value: e.Title, OccurredAt: e.OccurredAt,
			Confidence: e.Confidence, EpistemicType: e.EpistemicType,
			SessionID: e.SessionID, SupersedesID: e.SupersedesID,
		})
	}

	metrics, err := h.Metrics.ListPending(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, m := range metrics {
		v := ""
		if m.ValueText != nil {
			v = *m.ValueText
		}
		// MeasuredAt 是值类型（time.Time NOT NULL），取址局部副本填 OccurredAt（队列展示测点时刻）。
		mt := m.MeasuredAt
		items = append(items, pendingItem{
			Kind: "metric", ID: m.ID, PersonID: m.PersonID, PersonName: nameOf[m.PersonID],
			MetricKey: m.MetricKey, Value: v, OccurredAt: &mt,
			Confidence: m.Confidence, EpistemicType: m.EpistemicType,
			SessionID: m.SessionID,
		})
	}

	cycles, err := h.Cycles.ListPending(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, c := range cycles {
		v := c.CycleType
		if c.Label != nil {
			v = c.CycleType + "·" + *c.Label
		}
		items = append(items, pendingItem{
			Kind: "cycle", ID: c.ID, PersonID: c.PersonID, PersonName: nameOf[c.PersonID],
			CycleType: c.CycleType, Value: v,
			Confidence: c.Confidence, EpistemicType: c.EpistemicType,
			SessionID: c.SessionID, SupersedesID: c.SupersedesID,
		})
	}

	activities, err := h.Activities.ListPending(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, a := range activities {
		// StartedAt 是值类型（time.Time NOT NULL），取址局部副本填 OccurredAt（队列展示活动开始时刻，
		// 前端时间线依赖，对齐 metric 段的 MeasuredAt 处理）。value 取 activity 串（活动本身即身份）。
		// activity 无 supersedes（测点流独立记录，无版本取代语义），故不填 SupersedesID。
		at := a.StartedAt
		items = append(items, pendingItem{
			Kind: "activity", ID: a.ID, PersonID: a.PersonID, PersonName: nameOf[a.PersonID],
			Value: a.Activity, OccurredAt: &at,
			Confidence: a.Confidence, EpistemicType: a.EpistemicType,
			SessionID: a.SessionID,
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
		http.Error(w, "kind 非法（person|attribute|relationship|event|metric|cycle|activity）", http.StatusBadRequest)
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
		http.Error(w, "kind 非法（person|attribute|relationship|event|metric|cycle|activity）", http.StatusBadRequest)
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
