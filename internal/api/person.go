package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
	"person": true, "attribute": true, "relationship": true, "event": true, "metric": true,
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

// metricGroup 详情页/指标列表按 metric_key 聚合的一组测点（时序曲线的一条线）。
// Label/Unit/Numeric 取自指标目录（profile.MetricDefOf）；Points 按 measured_at 升序。
type metricGroup struct {
	Key     string        `json:"key"`
	Label   string        `json:"label"`
	Unit    string        `json:"unit"`
	Numeric bool          `json:"numeric"`
	Points  []metricPoint `json:"points"` // 按 measured_at 升序
}

// metricPoint 单个测点（曲线上的一个点）。ValueText/Unit 在 repo 里是 *string，
// 这里转成 string（nil→""），前端拿到稳定的字符串而非 null。
type metricPoint struct {
	ID         ids.ID    `json:"id"` // 测点行 id，供前端 DELETE /metrics/{mid}
	MeasuredAt time.Time `json:"measured_at"`
	ValueNum   *float64  `json:"value_num,omitempty"`
	ValueText  string    `json:"value_text,omitempty"`
	Status     string    `json:"status"`
}

type personDetailResp struct {
	Person           *repo.Person              `json:"person"`
	Groups           []attrGroup               `json:"groups"`
	Relationships    []repo.PersonRelationship `json:"relationships"`
	Events           []repo.PersonEvent        `json:"events"`
	Metrics          []metricGroup             `json:"metrics"`
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
	// 时序指标（metric 平面）：只展示 active+pending，按 metric_key 分组、组内 measured_at
	// 升序（ListByPerson 已排序）；pending 测点也计入详情页的 pending 计数。
	metrics, err := h.Metrics.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	metricGroups, metricPending := buildMetricGroups(metrics)
	pending += metricPending
	writeJSON(w, personDetailResp{
		Person: p, Groups: groups, Relationships: relShown, Events: evShown,
		Metrics: metricGroups, RecentSessionIDs: sids, PendingCount: pending,
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
func (h *PersonHandler) AddEvent(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		EventType       string `json:"event_type"`
		Title           string `json:"title"`
		Description     string `json:"description"`
		OccurredAt      string `json:"occurred_at"`
		EndAt           string `json:"end_at"`
		Location        string `json:"location"`
		RelatedPersonID string `json:"related_person_id"`
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
	var related *ids.ID
	if req.RelatedPersonID != "" {
		rid, err := ids.ParseID(req.RelatedPersonID)
		if err != nil {
			http.Error(w, "related_person_id 非法", http.StatusBadRequest)
			return
		}
		related = &rid
	}
	e, err := h.Service.ManualAddEvent(r.Context(), pid, req.EventType, req.Title,
		req.Description, req.OccurredAt, req.EndAt, req.Location, related)
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

// ---- 时序指标（metric 平面）----

// buildMetricGroups 把测点序列按 metric_key 分组：ListByPerson 已按 (metric_key, measured_at)
// 排序，故顺序遍历、遇到 key 变化就新开一组即可（无需 map）。每组的 Label/Unit/Numeric 取自
// 指标目录（profile.MetricDefOf）。返回分组结果 + pending 测点数（供详情页并入 pending 计数）。
// 防御性再过滤一次 active+pending（ListByPerson 已保证，双保险）。
func buildMetricGroups(metrics []repo.PersonMetric) ([]metricGroup, int) {
	groups := make([]metricGroup, 0)
	pending := 0
	idx := -1 // 当前分组在 groups 中的下标；用下标而非指针，避免 append 扩容后指针失效
	for _, m := range metrics {
		if m.Status != "active" && m.Status != "pending" {
			continue
		}
		if m.Status == "pending" {
			pending++
		}
		if idx < 0 || groups[idx].Key != m.MetricKey {
			def := profile.MetricDefOf(m.MetricKey)
			groups = append(groups, metricGroup{
				Key: m.MetricKey, Label: def.Label, Unit: def.Unit, Numeric: def.Numeric,
				Points: []metricPoint{},
			})
			idx = len(groups) - 1
		}
		vt := ""
		if m.ValueText != nil {
			vt = *m.ValueText
		}
		groups[idx].Points = append(groups[idx].Points, metricPoint{
			ID: m.ID, MeasuredAt: m.MeasuredAt, ValueNum: m.ValueNum, ValueText: vt, Status: m.Status,
		})
	}
	return groups, pending
}

// metricValueText 生成测点值的展示串（确认队列 Value 用）：数值型取 value_num（带单位，
// 如 70kg / 0.8），类别型取 value_text（如 火锅）；两者都空时返回 ""。数值用最短十进制
// 表示（strconv -1 精度，避免 70 打成 70.000），与 profile.metricSummary 的数值格式一致。
func metricValueText(m *repo.PersonMetric) string {
	if m.ValueNum != nil {
		s := strconv.FormatFloat(*m.ValueNum, 'f', -1, 64)
		if m.Unit != nil {
			s += *m.Unit
		}
		return s
	}
	if m.ValueText != nil {
		return *m.ValueText
	}
	return ""
}

// parseMeasuredAt 解析测点时间串，保留时刻精度（对齐 profile.parseMetricAt 的意图——metric 是
// 连续时序，同一天多次测量靠时刻区分，勿抹平到当天零点）。空串或全部解析失败 → time.Now()
// （保证 measured_at 列 NOT NULL 非零，Service 侧也会拒绝零值）。
func parseMeasuredAt(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now()
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Now()
}

// ListMetrics 人物时序指标（分组结构，每组按 measured_at 升序）。只返回 active+pending
// （repo ListByPerson 已过滤）。?metric_key=weight 只看单一指标（可选）。
func (h *PersonHandler) ListMetrics(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	list, err := h.Metrics.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if key := r.URL.Query().Get("metric_key"); key != "" {
		filtered := make([]repo.PersonMetric, 0, len(list))
		for _, m := range list {
			if m.MetricKey == key {
				filtered = append(filtered, m)
			}
		}
		list = filtered
	}
	groups, _ := buildMetricGroups(list)
	writeJSON(w, map[string]any{"metrics": groups})
}

// AddMetric 手动加一个测点（走 Service：active/manual/conf=1.0 + 审计）。
// metric_key 目录校验先行（→400）；value_num 缺省（body 不带）→ nil，交由 Service 按指标
// 类型校验（数值型必须有 value_num、类别型必须有 value_text）；measured_at 空 → 当前时间，
// 否则 RFC3339 / "2006-01-02" 等尽力解析。
func (h *PersonHandler) AddMetric(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		MetricKey  string   `json:"metric_key"`
		ValueNum   *float64 `json:"value_num"`
		ValueText  string   `json:"value_text"`
		Unit       string   `json:"unit"`
		MeasuredAt string   `json:"measured_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if !profile.ValidMetricKey(req.MetricKey) {
		http.Error(w, "metric_key 非法", http.StatusBadRequest)
		return
	}
	m, err := h.Service.ManualAddMetric(r.Context(), pid, req.MetricKey,
		req.ValueNum, req.ValueText, req.Unit, parseMeasuredAt(req.MeasuredAt))
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
	Kind          string     `json:"kind"` // attribute|relationship|person|event|metric
	ID            ids.ID     `json:"id"`
	PersonID      ids.ID     `json:"person_id"`
	PersonName    string     `json:"person_name"`
	AttrKey       string     `json:"attr_key,omitempty"`
	Value         string     `json:"value,omitempty"`         // attribute:建议值 / relationship:类型 / person:名字 / metric:值
	CurrentValue  string     `json:"current_value,omitempty"` // 冲突时的现值（supersedes 行）
	RelationType  string     `json:"relation_type,omitempty"`
	EventType     string     `json:"event_type,omitempty"`
	MetricKey     string     `json:"metric_key,omitempty"`
	OccurredAt    *time.Time `json:"occurred_at,omitempty"`
	MeasuredAt    *time.Time `json:"measured_at,omitempty"`
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
		measuredAt := m.MeasuredAt // 取地址前先拷贝到迭代内局部变量，指针稳定
		items = append(items, pendingItem{
			Kind: "metric", ID: m.ID, PersonID: m.PersonID, PersonName: nameOf[m.PersonID],
			MetricKey: m.MetricKey, Value: metricValueText(&m), MeasuredAt: &measuredAt,
			Confidence: m.Confidence, EpistemicType: m.EpistemicType,
			SessionID: m.SessionID, SupersedesID: m.SupersedesID,
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
		http.Error(w, "kind 非法（person|attribute|relationship|event|metric）", http.StatusBadRequest)
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
		http.Error(w, "kind 非法（person|attribute|relationship|event|metric）", http.StatusBadRequest)
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
