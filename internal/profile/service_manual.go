package profile

import (
	"context"
	"fmt"
	"strings"

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
func (s *Service) ManualAddAttribute(ctx context.Context, personID ids.ID, attrKey, value string) (*repo.PersonAttribute, error) {
	d := Def(attrKey)
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
