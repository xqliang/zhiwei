package profile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// 手动操作（spec §5.1）：立即 active、source=manual、confidence=1.0、记审计
// （changed_by=user）。手动改值 = 旧行 superseded + 新行（supersedes_id 指向旧行）。

// ManualCreatePerson 手动新建人物（active/manual + create 审计）。
// 自持事务：BeginTxx → ManualCreatePersonExt → Commit（行为/签名与历史一致，
// 现有 api/person.go 调用面零改）。真正的写逻辑在 Ext 变体里，便于并入他人的事务
// （如 agent 关系提议确认时「未命中则新建关联人」，与关系写并进同一 confirm 事务）。
func (s *Service) ManualCreatePerson(ctx context.Context, name string, speakerID *ids.ID, summary *string) (*repo.Person, error) {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	p, err := s.ManualCreatePersonExt(ctx, tx, name, speakerID, summary)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// ManualCreatePersonExt 是 ManualCreatePerson 的事务版：全部写走传入的 tx，不自开/自提事务，
// 供调用方（如 agent 关系提议确认闸门）把「建人 + 关系写 + Proposals.Resolve」原子并进同一
// 事务（apply-once）。落库语义与 ManualCreatePerson 完全一致（active/manual + create 审计）。
// 注意：不 tx.Rollback()/Commit()——事务生命周期归调用方。
func (s *Service) ManualCreatePersonExt(ctx context.Context, tx *sqlx.Tx, name string, speakerID *ids.ID, summary *string) (*repo.Person, error) {
	p := &repo.Person{DisplayName: name, SpeakerID: speakerID, Summary: summary, Source: "manual", Status: "active"}
	if err := s.Persons.CreateExt(ctx, tx, p); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: p.ID, EntityKind: "person", EntityID: &p.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(p.DisplayName),
	}); err != nil {
		return nil, err
	}
	return p, nil
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
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: id, EntityKind: "person", EntityID: &id,
		ChangeType: "update", ChangedBy: "user", OldValue: snap(p.Status), NewValue: snap(status),
		Note: strPtr("人物状态流转"),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ManualAddAttribute 手动加/改属性：单值型已有 active 时旧行 superseded、
// 新行 supersedes_id 指向旧行（即手动改值）；列表型纯叠加新行。
// 自持事务：BeginTxx → ManualAddAttributeExt → Commit（行为/签名与历史一致，
// 现有 api/person.go 调用面零改）。真正的写逻辑在 Ext 变体里，便于并入他人的事务
// （如 agent 提议确认闸门的单事务 apply-once，见 internal/agent/proposals.go）。
func (s *Service) ManualAddAttribute(ctx context.Context, personID ids.ID, attrKey, value string) (*repo.PersonAttribute, error) {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	row, err := s.ManualAddAttributeExt(ctx, tx, personID, attrKey, value)
	if err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualAddAttributeExt 是 ManualAddAttribute 的事务版：全部读写走传入的 tx，
// 不自开/自提事务，供调用方把「领域写 + 其它写（如 Proposals.Resolve）」原子并进同一事务。
// 语义与 ManualAddAttribute 完全一致：单值 supersede、同值幂等 no-op、审计 changed_by=user。
// 注意：同值幂等分支这里直接 return existing, nil（不 tx.Rollback）——tx 生命周期归调用方，
// 事务里没写任何行，调用方照常 Commit 也无副作用（对齐设计 D1）。
func (s *Service) ManualAddAttributeExt(ctx context.Context, tx *sqlx.Tx, personID ids.ID, attrKey, value string) (*repo.PersonAttribute, error) {
	d := Def(attrKey)

	var existing *repo.PersonAttribute
	var err error
	if d.Cardinality == CardinalityList {
		existing, err = s.Attributes.FindActiveByKeyValueExt(ctx, tx, personID, attrKey, value)
	} else {
		existing, err = s.Attributes.FindActiveByKeyExt(ctx, tx, personID, attrKey)
	}
	if err != nil {
		return nil, err
	}
	// 同值已存在：幂等返回旧行（不重复叠加）。Ext 版不持有 tx 生命周期，直接返回 nil，
	// 不做 tx.Rollback（那会毁掉调用方事务里的其它写）。
	if existing != nil && repo.NormalizeTitle(existing.ValueText) == repo.NormalizeTitle(value) {
		return existing, nil // no-op
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
	return row, nil
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
// 自持事务：BeginTxx → ManualAddRelationshipExt → Commit（行为/签名与历史一致，
// 现有 api/person.go 调用面零改）。真正的写逻辑在 Ext 变体里，便于并入他人的事务
// （如 agent 关系提议确认闸门的单事务 apply-once，见 internal/agent/proposals.go）。
func (s *Service) ManualAddRelationship(ctx context.Context, personID ids.ID, relationType string,
	relatedPersonID *ids.ID, direction, orgName, label string) (*repo.PersonRelationship, error) {

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	row, err := s.ManualAddRelationshipExt(ctx, tx, personID, relationType, relatedPersonID, direction, orgName, label)
	if err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualAddRelationshipExt 是 ManualAddRelationship 的事务版：全部写走传入的 tx，不自开/自提
// 事务，供调用方（如 agent 关系提议确认闸门）把「关系写 +（可能的）建关联人 + Proposals.Resolve」
// 原子并进同一事务（apply-once，见 internal/agent/proposals.go）。校验与落库语义与
// ManualAddRelationship 完全一致（ValidRelations 校验 + active/manual conf=1.0 + create 审计）。
// 注意：不 tx.Rollback()/Commit()——事务生命周期归调用方；非法 relation_type 校验先行、
// 直接返回错误（未写任何行，调用方回滚即可）。
func (s *Service) ManualAddRelationshipExt(ctx context.Context, tx *sqlx.Tx, personID ids.ID, relationType string,
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
	return row, nil
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
// 自持事务：BeginTxx → ManualAddEventExt → Commit（行为/签名与历史一致）。
func (s *Service) ManualAddEvent(ctx context.Context, personID ids.ID, eventType, title,
	description, occurredAt, endAt, location string, relatedPersonID *ids.ID) (*repo.PersonEvent, error) {

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	row, err := s.ManualAddEventExt(ctx, tx, personID, eventType, title, description, occurredAt, endAt, location, relatedPersonID)
	if err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualAddEventExt 是 ManualAddEvent 的事务版：全部写走传入的 tx，不自开/自提事务，
// 供调用方（如 agent 提议确认闸门）把「事件写 + Proposals.Resolve」原子并进同一事务（D1）。
// 校验与落库语义与 ManualAddEvent 完全一致（event_type 合法 + title 非空 + 审计 changed_by=user）。
func (s *Service) ManualAddEventExt(ctx context.Context, tx *sqlx.Tx, personID ids.ID, eventType, title,
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
	return row, nil
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

// ---- metric 平面手动 CRUD（P3 时序个人指标）----

// ManualAddMetric 手动加一个测点（active/manual conf=1.0 + create 审计）。
// valueNum 可空（类别型指标传 nil）；valueText/unit 可空；measuredAt 必须非零（列 NOT NULL）。
// 自持事务：BeginTxx → ManualAddMetricExt → Commit（行为/签名与 event 平面手动入口一致）。
func (s *Service) ManualAddMetric(ctx context.Context, personID ids.ID, metricKey string,
	valueNum *float64, valueText, unit string, measuredAt time.Time) (*repo.PersonMetric, error) {

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	row, err := s.ManualAddMetricExt(ctx, tx, personID, metricKey, valueNum, valueText, unit, measuredAt)
	if err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualAddMetricExt 是 ManualAddMetric 的事务版：全部写走传入的 tx，不自开/自提事务，
// 供调用方把「测点写 + 其它写」原子并进同一事务（对齐 event 平面的 ManualAddEventExt）。
//
// 校验（对齐 metric 硬约束）：
//   - metric_key 必须在目录内（ValidMetricKey），否则报错；
//   - Numeric 指标必须给 valueNum（否则报错）；非 Numeric 指标必须给 valueText（否则报错）；
//   - measuredAt 零值报错（measured_at 列 NOT NULL，硬约束 4）。
//
// 落库：confidence=1.0 / epistemic=observed / source=manual / status=active（手动即定，spec §5.1）；
// unit 空则回退目录单位；value_text/unit 空存 NULL（textPtr）；审计 changed_by=user、
// new_value=值摘要（metricSummary）。append-only：手动加点同样不 supersede（硬约束 1）。
func (s *Service) ManualAddMetricExt(ctx context.Context, tx *sqlx.Tx, personID ids.ID, metricKey string,
	valueNum *float64, valueText, unit string, measuredAt time.Time) (*repo.PersonMetric, error) {

	if !ValidMetricKey(metricKey) {
		return nil, fmt.Errorf("非法指标键: %s", metricKey)
	}
	valueText = strings.TrimSpace(valueText)
	// Numeric 指标要有 value_num（曲线可画，硬约束 6）；类别指标要有 value_text。
	if MetricDefOf(metricKey).Numeric {
		if valueNum == nil {
			return nil, fmt.Errorf("数值指标 %s 必须提供 value_num", metricKey)
		}
	} else if valueText == "" {
		return nil, fmt.Errorf("类别指标 %s 必须提供 value_text", metricKey)
	}
	if measuredAt.IsZero() {
		return nil, fmt.Errorf("measured_at 不能为零值（列 NOT NULL）")
	}

	if strings.TrimSpace(unit) == "" {
		unit = MetricDefOf(metricKey).Unit
	}
	row := &repo.PersonMetric{
		PersonID: personID, MetricKey: metricKey,
		ValueNum: valueNum, ValueText: textPtr(valueText), Unit: textPtr(strings.TrimSpace(unit)),
		MeasuredAt: measuredAt,
		Confidence: 1.0, EpistemicType: "observed", Source: "manual", Status: "active",
	}
	if err := s.Metrics.CreateExt(ctx, tx, row); err != nil {
		return nil, err
	}
	// 手动路径无 session/溯源：change_log 不带 session_id（nil→NULL），故不复用 createMetricLog。
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "metric", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(metricSummary(row)),
		Confidence: fp(1.0), EpistemicType: strPtr("observed"),
	}); err != nil {
		return nil, err
	}
	return row, nil
}

// ManualDeleteMetric 手动删测点 → dismissed + delete 审计（对齐 ManualDeleteEvent；
// append-only 表不真删行，置 dismissed 即从 ListByPerson/FindByPointExt 隐去）。
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
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: m.PersonID, EntityKind: "metric", EntityID: &id,
		ChangeType: "delete", ChangedBy: "user", OldValue: snap(metricSummary(m)),
	}); err != nil {
		return err
	}
	return tx.Commit()
}
