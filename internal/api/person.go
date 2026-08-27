package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/auth"
	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
)

// PersonHandler 人物画像 API：名册 CRUD、属性/关系手动管理、修改历史、
// 确认队列（跨平面 pending 并集）、按需回填抽取。
// 读操作直连 repo；一切变更走 profile.Service（保证审计+事务只实现一次）。
type PersonHandler struct {
	Persons       *repo.PersonRepo
	Speakers      *repo.SpeakerRepo // 详情页按 speaker_id 查声纹名（Speakers 为 nil 时降级返回空名）
	Attributes    *repo.PersonAttributeRepo
	Relationships *repo.PersonRelationshipRepo
	Events        *repo.PersonEventRepo
	Metrics       *repo.PersonMetricRepo
	Cycles        *repo.PersonCycleRepo
	Activities    *repo.PersonActivityRepo
	Pets          *repo.PersonPetRepo
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
	r.Get("/api/persons/{id}/pets", h.ListPets)
	r.Post("/api/persons/{id}/pets", h.AddPet)
	r.Patch("/api/persons/{id}/pets/{petid}", h.PatchPet)
	r.Delete("/api/persons/{id}/pets/{petid}", h.DeletePet)
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
	"metric": true, "cycle": true, "activity": true, "pet": true,
}

// ---- 名册 ----

// List 名册（active+pending）+ 每人 pending 角标计数。
// ?dismissed=1 返回已删除人物（软删行，折叠区查看/恢复）；默认返回非 dismissed（活跃名册）。
// 对齐 topics 的 ?dismissed=1 约定：两个视图分离，避免活跃名册混入已删数据。
func (h *PersonHandler) List(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Query().Get("dismissed") == "1" {
		list, err := h.Persons.ListDismissed(r.Context(), uid.Int64())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"persons": list})
		return
	}
	list, err := h.Persons.ListWithPending(r.Context(), uid.Int64())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"persons": list})
}

func (h *PersonHandler) Create(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	p, err := h.Service.ManualCreatePerson(r.Context(), uid.Int64(), name, speakerID, req.Summary)
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
	SpeakerName      string                    `json:"speaker_name,omitempty"` // 绑定声纹的显示名（详情页徽标直接展示，免前端二次查表）
	Groups           []attrGroup               `json:"groups"`
	Relationships    []repo.PersonRelationship `json:"relationships"`
	Events           []repo.PersonEvent        `json:"events"`
	Metrics          []metricGroup             `json:"metrics"`
	Pets             []repo.PersonPet          `json:"pets"`
	RecentSessionIDs []ids.ID                  `json:"recent_session_ids"`
	PendingCount     int                       `json:"pending_count"`
}

func (h *PersonHandler) Get(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	p, err := h.Persons.Get(r.Context(), uid.Int64(), id) // 按登录用户过滤：越权命中 0 行 → nil → 404
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
	// 宠物（pet 平面）：只展示 active+pending（单值语义的历史版本不混入）；
	// 宠物每人通常几只、量小，详情直接内嵌列表（对齐 relationships 的做法）。
	petRows, err := h.Pets.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	petShown := make([]repo.PersonPet, 0, len(petRows))
	for _, pt := range petRows {
		if pt.Status != "active" && pt.Status != "pending" {
			continue
		}
		petShown = append(petShown, pt)
		if pt.Status == "pending" {
			pending++
		}
	}
	// cycle/activity 平面的 pending 计入详情页角标（确认队列已含这两类，名册/详情角标须一致）；
	// 详情不展示 cycle/activity 列表（时序/轨迹数据量大、有独立 GET 端点按需查询），故用轻量 COUNT。
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
	pending += cp + ap
	// 绑定声纹的显示名（详情页徽标用）。声纹行被删时 Get 返回 ErrNoRows，
	// 降级为空串——悬挂外键只影响徽标展示，不该让整个详情 500。
	speakerName := ""
	if p.SpeakerID != nil && h.Speakers != nil {
		if sp, err := h.Speakers.Get(r.Context(), *p.SpeakerID); err == nil && sp != nil {
			speakerName = sp.Name
		}
	}
	writeJSON(w, personDetailResp{
		Person: p, SpeakerName: speakerName, Groups: groups, Relationships: relShown, Events: evShown,
		Metrics: metricGroups, Pets: petShown, RecentSessionIDs: sids, PendingCount: pending,
	})
}

func (h *PersonHandler) Patch(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	p, err := h.Persons.Get(r.Context(), uid.Int64(), id) // 按登录用户过滤：越权命中 0 行 → nil → 404
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
		if err := h.Service.ManualSetPersonStatus(r.Context(), uid.Int64(), id, req.Status); err != nil {
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
	if err := h.Service.ManualUpdatePerson(r.Context(), uid.Int64(), id, name, speakerID, summary); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *PersonHandler) Delete(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	if p, err := h.Persons.Get(r.Context(), uid.Int64(), id); err != nil { // 按登录用户过滤：越权命中 0 行 → nil → 404
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
		return
	}
	if err := h.Service.ManualSetPersonStatus(r.Context(), uid.Int64(), id, "dismissed"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 属性 ----

func (h *PersonHandler) AddAttribute(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	a, err := h.Service.ManualAddAttribute(r.Context(), uid.Int64(), pid, key, val)
	if err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "人物不存在", http.StatusNotFound)
			return
		}
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	na, err := h.Service.ManualAddAttribute(r.Context(), uid.Int64(), pid, a.AttrKey, val)
	if err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "属性不存在", http.StatusNotFound)
			return
		}
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	aid, err := ids.ParseID(chi.URLParam(r, "aid"))
	if err != nil {
		http.Error(w, "aid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteAttribute(r.Context(), uid.Int64(), aid); err != nil {
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	rel, err := h.Service.ManualAddRelationship(r.Context(), uid.Int64(), pid, req.RelationType,
		related, req.Direction, req.OrgName, req.Label)
	if err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "人物不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rel)
}

func (h *PersonHandler) DeleteRelationship(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rid, err := ids.ParseID(chi.URLParam(r, "rid"))
	if err != nil {
		http.Error(w, "rid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteRelationship(r.Context(), uid.Int64(), rid); err != nil {
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	// 子表按 person_id 查询无 user 过滤，先确认 person 归属登录用户（越权 → 404）。
	if p, err := h.Persons.Get(r.Context(), uid.Int64(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	e, err := h.Service.ManualAddEvent(r.Context(), uid.Int64(), pid, req.EventType, req.Title,
		req.Description, req.OccurredAt, req.EndAt, req.Location, related, req.Importance)
	if err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "人物不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, e)
}

// DeleteEvent 手动删事件 → dismissed + delete 审计。
func (h *PersonHandler) DeleteEvent(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	eid, err := ids.ParseID(chi.URLParam(r, "eid"))
	if err != nil {
		http.Error(w, "eid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteEvent(r.Context(), uid.Int64(), eid); err != nil {
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

// parseDateParam 解析时间窗查询参数（YYYY-MM-DD）→ 当日 00:00 UTC 指针。空串 → (nil, nil)
// （不限该边界）；格式非法 → 返回错误（handler 转 400）。与 repo 半开区间 [from, to) 配合。
// activity 平面时间窗查询用（metric 用 feat 分组契约、不走窗口）。
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

// ListMetrics 人物时序指标（分组结构，每组按 measured_at 升序）。只返回 active+pending
// （repo ListByPerson 已过滤）。?metric_key=weight 只看单一指标（可选）。
func (h *PersonHandler) ListMetrics(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	// 子表按 person_id 查询无 user 过滤，先确认 person 归属登录用户（越权 → 404）。
	if p, err := h.Persons.Get(r.Context(), uid.Int64(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	m, err := h.Service.ManualAddMetric(r.Context(), uid.Int64(), pid, req.MetricKey,
		req.ValueNum, req.ValueText, req.Unit, parseMeasuredAt(req.MeasuredAt))
	if err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "人物不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, m)
}

// DeleteMetric 手动删测点 → dismissed + delete 审计。
func (h *PersonHandler) DeleteMetric(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	mid, err := ids.ParseID(chi.URLParam(r, "mid"))
	if err != nil {
		http.Error(w, "mid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteMetric(r.Context(), uid.Int64(), mid); err != nil {
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
// ?status= 显式过滤某状态。响应体带 note 免责文案（spec §9），前端展示「下次预测」时须一并呈现。
func (h *PersonHandler) ListCycles(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	// 子表按 person_id 查询无 user 过滤，先确认 person 归属登录用户（越权 → 404）。
	if p, err := h.Persons.Get(r.Context(), uid.Int64(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	c, err := h.Service.ManualAddCycle(r.Context(), uid.Int64(), pid, req.CycleType, req.Label,
		req.AnchorDate, req.Frequency, req.Dosage, req.PeriodDays, req.DurationDays)
	if err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "人物不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, c)
}

// DeleteCycle 手动删周期 → dismissed + delete 审计。
func (h *PersonHandler) DeleteCycle(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		http.Error(w, "cid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteCycle(r.Context(), uid.Int64(), cid); err != nil {
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
// query：from=/to=（YYYY-MM-DD）时间窗，半开区间 [from,to)（与 parseDateParam 语义——to 传当日
// 00:00 即「不含 to 当日」）；解析失败 → 400。?status= 可选只看某状态。全状态返回（含 dismissed）：
// 前端时间线用 status!==dismissed 客户端过滤，软删行仍随列表下发。
func (h *PersonHandler) ListActivities(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	// 子表按 person_id 查询无 user 过滤，先确认 person 归属登录用户（越权 → 404）。
	if p, err := h.Persons.Get(r.Context(), uid.Int64(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
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
// 0=未给（Service ≤0 不落列）。返回裸行（对齐 AddMetric/AddCycle/AddEvent）。
func (h *PersonHandler) AddActivity(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
	a, err := h.Service.ManualAddActivity(r.Context(), uid.Int64(), pid, req.Activity, req.Tool,
		req.Location, req.CommuteMode, req.StartedAt, req.DurationMin)
	if err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "人物不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, a) // 返回裸行，对齐 AddMetric/AddCycle/AddEvent（列表端点才有 {activities:[...]} 包络）
}

// DeleteActivity 手动删活动 → dismissed + delete 审计（软删；全状态 GET 仍返回该行）。
func (h *PersonHandler) DeleteActivity(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	aid, err := ids.ParseID(chi.URLParam(r, "aid"))
	if err != nil {
		http.Error(w, "aid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteActivity(r.Context(), uid.Int64(), aid); err != nil {
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	// 变更历史按 person_id 查询无 user 过滤，先确认 person 归属登录用户（越权 → 404）。
	if p, err := h.Persons.Get(r.Context(), uid.Int64(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
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
	Value         string     `json:"value,omitempty"`         // attribute:建议值 / relationship:类型 / person:名字 / metric:值
	CurrentValue  string     `json:"current_value,omitempty"` // 冲突时的现值（supersedes 行）
	RelationType  string     `json:"relation_type,omitempty"`
	EventType     string     `json:"event_type,omitempty"`
	MetricKey     string     `json:"metric_key,omitempty"`
	CycleType     string     `json:"cycle_type,omitempty"`
	OccurredAt    *time.Time `json:"occurred_at,omitempty"`
	MeasuredAt    *time.Time `json:"measured_at,omitempty"`
	Label         string     `json:"label,omitempty"`
	Confidence    float64    `json:"confidence"`
	EpistemicType string     `json:"epistemic_type"`
	SessionID     *ids.ID    `json:"session_id,omitempty"`
	SupersedesID  *ids.ID    `json:"supersedes_id,omitempty"`
}

// petPendingLabel 宠物 pending 项的摘要标签（类别·品种·性别·年龄），供确认队列一行直读。
func petPendingLabel(p *repo.PersonPet) string {
	parts := []string{p.Species}
	if p.Breed != nil {
		parts = append(parts, *p.Breed)
	}
	if p.Gender != nil {
		parts = append(parts, *p.Gender)
	}
	if p.AgeText != nil {
		parts = append(parts, *p.AgeText)
	}
	return strings.Join(parts, "·")
}

func (h *PersonHandler) ListPending(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	persons, err := h.Persons.List(ctx, uid.Int64())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nameOf := make(map[ids.ID]string, len(persons))
	for _, p := range persons {
		nameOf[p.ID] = p.DisplayName
	}
	var items []pendingItem

	attrs, err := h.Attributes.ListPending(ctx, uid.Int64())
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

	rels, err := h.Relationships.ListPending(ctx, uid.Int64())
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

	events, err := h.Events.ListPending(ctx, uid.Int64())
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

	metrics, err := h.Metrics.ListPending(ctx, uid.Int64())
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

	cycles, err := h.Cycles.ListPending(ctx, uid.Int64())
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

	activities, err := h.Activities.ListPending(ctx, uid.Int64())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, a := range activities {
		// StartedAt 是值类型（time.Time NOT NULL），取址局部副本填 OccurredAt（队列展示活动开始时刻）。
		// activity 无 supersedes（测点流独立记录，无版本取代语义），故不填 SupersedesID。
		at := a.StartedAt
		items = append(items, pendingItem{
			Kind: "activity", ID: a.ID, PersonID: a.PersonID, PersonName: nameOf[a.PersonID],
			Value: a.Activity, OccurredAt: &at,
			Confidence: a.Confidence, EpistemicType: a.EpistemicType,
			SessionID: a.SessionID,
		})
	}

	petRows, err := h.Pets.ListPending(ctx, uid.Int64())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, pt := range petRows {
		it := pendingItem{
			Kind: "pet", ID: pt.ID, PersonID: pt.PersonID, PersonName: nameOf[pt.PersonID],
			Value: pt.Name, Label: petPendingLabel(&pt),
			Confidence: pt.Confidence, EpistemicType: pt.EpistemicType,
			SessionID: pt.SessionID, SupersedesID: pt.SupersedesID,
		}
		if pt.SupersedesID != nil {
			if cur, err := h.Pets.Get(ctx, *pt.SupersedesID); err == nil && cur != nil {
				it.CurrentValue = cur.Name
			}
		}
		items = append(items, it)
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	kind := chi.URLParam(r, "kind")
	if !validPendingKinds[kind] {
		http.Error(w, "kind 非法（person|attribute|relationship|event|metric|cycle|activity|pet）", http.StatusBadRequest)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ConfirmPending(r.Context(), uid.Int64(), kind, id); err != nil {
		writePendingErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *PersonHandler) DismissPending(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	kind := chi.URLParam(r, "kind")
	if !validPendingKinds[kind] {
		http.Error(w, "kind 非法（person|attribute|relationship|event|metric|cycle|activity|pet）", http.StatusBadRequest)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.DismissPending(r.Context(), uid.Int64(), kind, id); err != nil {
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

// ---- 宠物（pet 平面）----

// petReq 是 POST/PATCH 共用的请求体（整只提交；PATCH 即整只替换，未提到的字段被清空——
// 前端编辑表单始终回填现值，天然全量）。
type petReq struct {
	Name     string `json:"name"`
	Nickname string `json:"nickname"`
	Species  string `json:"species"`
	Breed    string `json:"breed"`
	Gender   string `json:"gender"`
	AgeText  string `json:"age_text"`
	Birthday string `json:"birthday"`
	Likes    string `json:"likes"`
}

// ListPets 宠物列表：默认只展示 active+pending（单值语义的历史版本 superseded/dismissed
// 混入会干扰）；?status= 显式过滤（对齐 ListCycles）。
func (h *PersonHandler) ListPets(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	// 子表按 person_id 查询无 user 过滤，先确认 person 归属登录用户（越权 → 404）。
	if p, err := h.Persons.Get(r.Context(), uid.Int64(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
		return
	}
	list, err := h.Pets.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if st := r.URL.Query().Get("status"); st != "" {
		filtered := make([]repo.PersonPet, 0, len(list))
		for _, pt := range list {
			if pt.Status == st {
				filtered = append(filtered, pt)
			}
		}
		list = filtered
	} else {
		filtered := make([]repo.PersonPet, 0, len(list))
		for _, pt := range list {
			if pt.Status == "active" || pt.Status == "pending" {
				filtered = append(filtered, pt)
			}
		}
		list = filtered
	}
	writeJSON(w, map[string]any{"pets": list})
}

// AddPet 手动加宠物（走 Service：active/manual/conf=1.0 + 审计）。
// name 与 birthday 必填（手动录入生日须为准确 YYYY-MM-DD，设计决策）；species 缺省收敛「其他」。
func (h *PersonHandler) AddPet(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req petReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name 必填", http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006-01-02", strings.TrimSpace(req.Birthday)); err != nil {
		http.Error(w, "birthday 必填且须为 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	pt, err := h.Service.ManualAddPet(r.Context(), uid.Int64(), pid, req.Name, req.Nickname,
		req.Species, req.Breed, req.Gender, req.AgeText, req.Birthday, req.Likes)
	if err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "人物不存在", http.StatusNotFound)
			return
		}
		if errors.Is(err, profile.ErrPetNameExists) {
			http.Error(w, "同名宠物已存在，请用编辑修改", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, pt)
}

// PatchPet 手动编辑宠物 = 整只替换（旧行 superseded、新行全量）。改新名撞其他 active
// 同名宠物 → 409；编辑历史版本行 → 404（Service 状态闸门）。
func (h *PersonHandler) PatchPet(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, err := ids.ParseID(chi.URLParam(r, "id")); err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	petID, err := ids.ParseID(chi.URLParam(r, "petid"))
	if err != nil {
		http.Error(w, "petid 非法", http.StatusBadRequest)
		return
	}
	var req petReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name 必填", http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006-01-02", strings.TrimSpace(req.Birthday)); err != nil {
		http.Error(w, "birthday 必填且须为 YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	// 资源归属以 petID 反查为准（对齐 DeleteCycle 不二次校验路径 id 的模式）。
	pt, err := h.Service.ManualUpdatePet(r.Context(), uid.Int64(), petID, req.Name, req.Nickname,
		req.Species, req.Breed, req.Gender, req.AgeText, req.Birthday, req.Likes)
	if err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "宠物不存在", http.StatusNotFound)
			return
		}
		if errors.Is(err, profile.ErrPetNameExists) {
			http.Error(w, "同名宠物已存在", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, pt)
}

// DeletePet 手动删宠物 → dismissed + delete 审计。
func (h *PersonHandler) DeletePet(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	petID, err := ids.ParseID(chi.URLParam(r, "petid"))
	if err != nil {
		http.Error(w, "petid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeletePet(r.Context(), uid.Int64(), petID); err != nil {
		if errors.Is(err, profile.ErrNotFound) {
			http.Error(w, "宠物不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
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
	// 鉴权闸门（与其它 person/profile 端点一致）；ExtractSession 的 session 归属切换属 2B/后台
	// 范畴（本阶段 pipeline 仍 user-1），故此处仅校验登录态、不改 ExtractSession 签名。
	if _, ok := auth.UserID(r.Context()); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
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
