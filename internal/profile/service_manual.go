package profile

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// 手动操作（spec §5.1）：立即 active、source=manual、confidence=1.0、记审计
// （changed_by=user）。手动改值 = 旧行 superseded + 新行（supersedes_id 指向旧行）。

// ManualCreatePerson 手动新建人物（active/manual + create 审计）。
func (s *Service) ManualCreatePerson(ctx context.Context, name string, speakerID *ids.ID, summary *string) (*repo.Person, error) {
	p := &repo.Person{DisplayName: name, SpeakerID: speakerID, Summary: summary, Source: "manual", Status: "active"}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Persons.CreateExt(ctx, tx, p); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: p.ID, EntityKind: "person", EntityID: &p.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(p.DisplayName),
	}); err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// ManualUpdatePerson 手动编辑人物（改名/换绑声纹/改备注）。
func (s *Service) ManualUpdatePerson(ctx context.Context, id ids.ID, name string, speakerID *ids.ID, summary *string) error {
	p, err := s.Persons.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Persons.UpdateExt(ctx, tx, id, name, speakerID, summary); err != nil {
		return err
	}
	old := snap(p.DisplayName)
	newV := snap(name)
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: id, EntityKind: "person", EntityID: &id,
		ChangeType: "update", ChangedBy: "user", OldValue: old, NewValue: newV,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ManualSetPersonStatus 人物状态流转（归档=dismissed 等）。
//
// F5（spec §13）归档级联：status=="dismissed"（归档人物）时，在**同一事务**内把该人物六个平面
// （属性/关系/大事记/指标/周期/活动）上所有 active|pending 的行一并置 dismissed——否则名册里人物
// 虽已隐藏，其平面行仍会进抽取闸门/确认队列，留下孤儿引用。**另加**反向边补充（P6）：他人指向
// 本人的 pending 关系边也一并级联 dismissed（active 反向边刻意保留，那是对端画像）。级联行数
// **汇总**记入 person 的 change_log Note（一行审计），**不逐平面、不逐行**写审计条目，这是刻意取舍：
//   - 归档是「显式用户意图」——用户已明确要清掉这个人的全部画像数据，无需逐行留痕来还原意图；
//   - 六平面可能有成百上千行（metric/activity 是测点流），逐行写审计会让 change_log 爆量、得不偿失。
//     故只在 person 行上留一条带级联计数的汇总审计（可审计「归档时清了多少」，够用）。
//
// 另一取舍：非 dismissed 流转（恢复 active / 置 pending 等）**不触发级联**；且**恢复人物不自动
// 恢复**已被级联 dismissed 的平面行——因为「哪些行是归档时级联下去的、哪些是本来就 dismissed 的」
// 无法仅凭状态区分（汇总审计不逐行记 id），自动回滚既语义复杂、又可能误恢复用户此前手动删掉的行。
// 故恢复语义留作跟进：恢复人物后其平面数据由用户按需手动重新录入/确认。
func (s *Service) ManualSetPersonStatus(ctx context.Context, id ids.ID, status string) error {
	p, err := s.Persons.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Persons.SetStatusExt(ctx, tx, id, status); err != nil {
		return err
	}

	// 默认审计备注；仅归档（dismissed）时改写为带六平面级联计数的汇总备注。
	note := "人物状态流转"
	if status == "dismissed" {
		// 六平面各自把该人物的 active/pending 行级联置 dismissed（只动活跃态，终态不动——见各 repo
		// DismissAllByPersonExt 注释）。全部在本事务内执行，任一步失败经 defer Rollback 整体回滚，
		// 不会留下「人物已归档但平面未级联」的中间态。
		nAttr, err := s.Attributes.DismissAllByPersonExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nRel, err := s.Relationships.DismissAllByPersonExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nEvt, err := s.Events.DismissAllByPersonExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nMet, err := s.Metrics.DismissAllByPersonExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nCyc, err := s.Cycles.DismissAllByPersonExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nAct, err := s.Activities.DismissAllByPersonExt(ctx, tx, id)
		if err != nil {
			return err
		}
		// 反向边补充（F5，P6）：他人指向本人（related_person_id=id）的 pending 关系边——归档本人后
		// 这些「待确认关系」成了确认队列里的孤儿噪声，一并级联 dismissed。active 反向边刻意不动
		// （那是对端人物画像，归档不替对端做主——见 DismissPendingReverseExt 注释）。行数并入汇总审计。
		nRevRel, err := s.Relationships.DismissPendingReverseExt(ctx, tx, id)
		if err != nil {
			return err
		}
		note = fmt.Sprintf("人物归档：级联 dismissed 属性 %d/关系 %d/大事记 %d/指标 %d/周期 %d/活动 %d 行；反向 pending 关系边 %d 条",
			nAttr, nRel, nEvt, nMet, nCyc, nAct, nRevRel)
	}

	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: id, EntityKind: "person", EntityID: &id,
		ChangeType: "update", ChangedBy: "user", OldValue: snap(p.Status), NewValue: snap(status),
		Note: strPtr(note),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ManualAddAttribute 手动加/改属性：单值型已有 active 时旧行 superseded、
// 新行 supersedes_id 指向旧行（即手动改值）；列表型纯叠加新行。
func (s *Service) ManualAddAttribute(ctx context.Context, personID ids.ID, attrKey, value string) (*repo.PersonAttribute, error) {
	d := Def(attrKey)
	// F4 写入端校验/规范化（单点闸，见 validate.go）：手动录入的脏值直接报错，error 原样
	// 透传 API 层——handler 用 errors.Is(err, ErrInvalidAttrValue) 命中后回 400（对齐 metric
	// 枚举校验的 400 口径），而非 500。与 LLM 路径共用 NormalizeAttrValue，规范化后的值贯穿
	// 后续 existing 查询、幂等比较与落库。
	norm, err := NormalizeAttrValue(d, value)
	if err != nil {
		return nil, err
	}
	value = norm

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var existing *repo.PersonAttribute
	if d.Cardinality == CardinalityList {
		existing, err = s.Attributes.FindActiveByKeyValueExt(ctx, tx, personID, attrKey, value)
	} else {
		existing, err = s.Attributes.FindActiveByKeyExt(ctx, tx, personID, attrKey)
	}
	if err != nil {
		return nil, err
	}
	// 同值已存在：幂等返回旧行（不重复叠加）
	if existing != nil && repo.NormalizeTitle(existing.ValueText) == repo.NormalizeTitle(value) {
		return existing, tx.Rollback() // no-op
	}

	var sup *ids.ID
	changeType := "create"
	if existing != nil {
		idv := existing.ID
		sup = &idv
		changeType = "update"
		if err := s.Attributes.SetStatusExt(ctx, tx, existing.ID, "superseded"); err != nil {
			return nil, err
		}
	}
	row := &repo.PersonAttribute{
		PersonID: personID, AttrKey: attrKey, ValueText: value, ValueType: d.ValueType,
		Confidence: 1.0, EpistemicType: "observed", Source: "manual",
		Status: "active", SupersedesID: sup,
	}
	if err := s.Attributes.CreateExt(ctx, tx, row); err != nil {
		return nil, err
	}
	l := &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "attribute", EntityID: &row.ID,
		AttrKey: strPtr(attrKey), ChangeType: changeType, ChangedBy: "user",
		NewValue: snap(value), Confidence: fp(1.0), EpistemicType: strPtr("observed"),
	}
	if existing != nil {
		l.OldValue = snap(existing.ValueText)
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, l); err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualDeleteAttribute 手动删属性 → dismissed + delete 审计。
func (s *Service) ManualDeleteAttribute(ctx context.Context, id ids.ID) error {
	a, err := s.Attributes.Get(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Attributes.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: a.PersonID, EntityKind: "attribute", EntityID: &id,
		AttrKey: strPtr(a.AttrKey), ChangeType: "delete", ChangedBy: "user",
		OldValue: snap(a.ValueText),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ManualAddRelationship 手动加关系边（active/manual + create 审计）。
// relatedPersonID 可空（组织关系）；direction/orgName/label 可选。
func (s *Service) ManualAddRelationship(ctx context.Context, personID ids.ID, relationType string,
	relatedPersonID *ids.ID, direction, orgName, label string) (*repo.PersonRelationship, error) {

	if !ValidRelations[relationType] {
		return nil, fmt.Errorf("非法关系类型: %s", relationType)
	}
	row := &repo.PersonRelationship{
		PersonID: personID, RelatedPersonID: relatedPersonID, RelationType: relationType,
		Confidence: 1.0, EpistemicType: "observed", Source: "manual", Status: "active",
	}
	if direction != "" {
		row.Direction = strPtr(direction)
	}
	if orgName != "" {
		row.OrgName = strPtr(orgName)
	}
	if label != "" {
		row.Label = strPtr(label)
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Relationships.CreateExt(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "relationship", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(relationType),
		Confidence: fp(1.0),
	}); err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualDeleteRelationship 手动删关系 → dismissed + delete 审计。
func (s *Service) ManualDeleteRelationship(ctx context.Context, id ids.ID) error {
	rel, err := s.Relationships.Get(ctx, id)
	if err != nil {
		return err
	}
	if rel == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Relationships.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: rel.PersonID, EntityKind: "relationship", EntityID: &id,
		ChangeType: "delete", ChangedBy: "user", OldValue: snap(rel.RelationType),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- event 平面手动 CRUD（P2 大事记）----

// ManualAddEvent 手动加大事记（active/manual conf=1.0 + create 审计）。
// relatedPersonID 可空；occurredAt/endAt 是原始字符串（YYYY-MM-DD/YYYY-MM/RFC3339，
// parseEventAt 尽力解析，失败存 NULL）；参数多，调用方为 API handler。
func (s *Service) ManualAddEvent(ctx context.Context, personID ids.ID, eventType, title,
	description, occurredAt, endAt, location string, relatedPersonID *ids.ID) (*repo.PersonEvent, error) {

	if !ValidEventTypes[eventType] {
		return nil, fmt.Errorf("非法事件类型: %s", eventType)
	}
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("title 不能为空")
	}
	row := &repo.PersonEvent{
		PersonID: personID, EventType: eventType, Title: strings.TrimSpace(title),
		Confidence: 1.0, EpistemicType: "observed", Source: "manual", Status: "active",
		Importance: 1.0,
	}
	if strings.TrimSpace(description) != "" {
		row.Description = strPtr(strings.TrimSpace(description))
	}
	if t, ok := parseEventAt(occurredAt); ok {
		row.OccurredAt = &t
	}
	if t, ok := parseEventAt(endAt); ok {
		row.EndAt = &t
	}
	if strings.TrimSpace(location) != "" {
		row.Location = strPtr(strings.TrimSpace(location))
	}
	if relatedPersonID != nil {
		row.RelatedPersonIDs = ids.List{*relatedPersonID}
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Events.CreateExt(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "event", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(row.Title),
		Confidence: fp(1.0),
	}); err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualDeleteEvent 手动删事件 → dismissed + delete 审计。
func (s *Service) ManualDeleteEvent(ctx context.Context, id ids.ID) error {
	e, err := s.Events.Get(ctx, id)
	if err != nil {
		return err
	}
	if e == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Events.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: e.PersonID, EntityKind: "event", EntityID: &id,
		ChangeType: "delete", ChangedBy: "user", OldValue: snap(e.Title),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- metric 平面手动 CRUD（P3 时序指标）----

// ManualAddMetric 手动加测点（active/manual conf=1.0 + create 审计）。
// 数值/类别分流同 LLM 路径（formatMetricValue 单点格式化，value_num/value_text 双存）；
// measuredAt 解析失败 → time.Now() 兜底：手动录入没有「对话发生时刻」可依，不知道时间就记
// 当下（区别于 LLM 路径 applyMetricFact 的 sessionTime——那里能用 session.created_at）。
func (s *Service) ManualAddMetric(ctx context.Context, personID ids.ID, metricKey, value, unit, measuredAt string) (*repo.PersonMetric, error) {
	if !ValidMetricKeys[metricKey] {
		return nil, fmt.Errorf("非法指标类型: %s", metricKey)
	}
	valueText := strings.TrimSpace(value)
	if valueText == "" {
		return nil, fmt.Errorf("value 不能为空")
	}
	var valueNum *float64
	if n, err := strconv.ParseFloat(valueText, 64); err == nil {
		vn := n
		valueNum = &vn
		valueText = formatMetricValue(n)
	}
	at := time.Now()
	if t, ok := parseEventAt(measuredAt); ok {
		at = t
	}
	row := &repo.PersonMetric{
		PersonID: personID, MetricKey: metricKey,
		ValueText: &valueText, ValueNum: valueNum, MeasuredAt: at,
		Confidence: 1.0, EpistemicType: "observed", Source: "manual", Status: "active",
	}
	if u := strings.TrimSpace(unit); u != "" {
		row.Unit = &u
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Metrics.CreateExt(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "metric", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(*row.ValueText),
		Confidence: fp(1.0),
	}); err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualDeleteMetric 手动删测点 → dismissed + delete 审计。
func (s *Service) ManualDeleteMetric(ctx context.Context, id ids.ID) error {
	m, err := s.Metrics.Get(ctx, id)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Metrics.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
		return err
	}
	old := ""
	if m.ValueText != nil {
		old = *m.ValueText
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: m.PersonID, EntityKind: "metric", EntityID: &id,
		ChangeType: "delete", ChangedBy: "user", OldValue: snap(old),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- cycle 平面手动 CRUD（P3 周期/日程，敏感）----

// ManualAddCycle 手动加周期/日程（active/manual conf=1.0 + create 审计）。label 空→nil；
// next_predicted_at 经 applyCycleParams 与 LLM 路径共用同一算法（anchor+period）；period/
// duration<=0 不落列（同 LLM 路径「未给不设」）。参数多，调用方为 API handler。
func (s *Service) ManualAddCycle(ctx context.Context, personID ids.ID, cycleType, label, anchorDate,
	frequency, dosage string, periodDays, durationDays int) (*repo.PersonCycle, error) {

	if !ValidCycleTypes[cycleType] {
		return nil, fmt.Errorf("非法周期类型: %s", cycleType)
	}
	row := &repo.PersonCycle{
		PersonID: personID, CycleType: cycleType,
		Confidence: 1.0, EpistemicType: "observed", Source: "manual", Status: "active",
	}
	if l := strings.TrimSpace(label); l != "" {
		row.Label = &l
	}
	applyCycleParams(row, anchorDate, periodDays, durationDays, dosage, frequency)
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Cycles.CreateExt(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "cycle", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(row.CycleType),
		Confidence: fp(1.0),
	}); err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualDeleteCycle 手动删周期 → dismissed + delete 审计。
func (s *Service) ManualDeleteCycle(ctx context.Context, id ids.ID) error {
	c, err := s.Cycles.Get(ctx, id)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Cycles.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: c.PersonID, EntityKind: "cycle", EntityID: &id,
		ChangeType: "delete", ChangedBy: "user", OldValue: snap(c.CycleType),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- activity 平面手动 CRUD（P4 生活轨迹，测点流语义）----

// ManualAddActivity 手动加活动（active/manual conf=1.0 + create 审计）。activity trim 非空校验；
// tool/location/commuteMode trim 空→nil（走 repo <=> NULL 匹配，同 LLM 路径 applyActivityFact）；
// duration>0 才落（≤0 视为未给，不臆造 0 分钟）；startedAt 解析失败 → time.Now() 兜底：手动录入
// 没有「对话发生时刻」可依，不知道时间就记当下（区别于 LLM 路径的 sessionTime——那里能用
// session.created_at）。参数多，调用方为 API handler。
func (s *Service) ManualAddActivity(ctx context.Context, personID ids.ID, activity, tool, location,
	commuteMode, startedAt string, durationMin int) (*repo.PersonActivity, error) {

	act := strings.TrimSpace(activity)
	if act == "" {
		return nil, fmt.Errorf("activity 不能为空")
	}
	at := time.Now()
	if t, ok := parseEventAt(startedAt); ok {
		at = t
	}
	row := &repo.PersonActivity{
		PersonID: personID, Activity: act, StartedAt: at,
		Tool:        trimToPtr(tool),
		Location:    trimToPtr(location),
		CommuteMode: trimToPtr(commuteMode),
		Confidence:  1.0, EpistemicType: "observed", Source: "manual", Status: "active",
	}
	if durationMin > 0 {
		dm := durationMin
		row.DurationMin = &dm
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Activities.CreateExt(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "activity", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(row.Activity),
		Confidence: fp(1.0),
	}); err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualDeleteActivity 手动删活动 → dismissed + delete 审计。
func (s *Service) ManualDeleteActivity(ctx context.Context, id ids.ID) error {
	a, err := s.Activities.Get(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Activities.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: a.PersonID, EntityKind: "activity", EntityID: &id,
		ChangeType: "delete", ChangedBy: "user", OldValue: snap(a.Activity),
	}); err != nil {
		return err
	}
	return tx.Commit()
}
