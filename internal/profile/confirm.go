package profile

import (
	"context"
	"fmt"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// ConfirmPending 确认一条 pending（kind ∈ person|attribute|relationship）：
// pending → active；attribute/relationship 若带 supersedes_id，被指向的旧行 → superseded。
// 每步变更记审计（changed_by=user）。非 pending 行确认报错（幂等由前端/状态保证）。
func (s *Service) ConfirmPending(ctx context.Context, kind string, id ids.ID) error {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	switch kind {
	case "person":
		p, err := s.Persons.Get(ctx, id)
		if err != nil {
			return err
		}
		if p == nil {
			return ErrNotFound
		}
		if p.Status != "pending" {
			return fmt.Errorf("仅 pending 状态可确认（当前 %s）", p.Status)
		}
		if err := s.Persons.SetStatus(ctx, id, "active"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: id, EntityKind: "person", EntityID: &id,
			ChangeType: "confirm", ChangedBy: "user", NewValue: snap(p.DisplayName),
			Note: strPtr("确认 LLM 自动新建的人物"),
		}); err != nil {
			return err
		}
	case "attribute":
		a, err := s.Attributes.Get(ctx, id)
		if err != nil {
			return err
		}
		if a == nil {
			return ErrNotFound
		}
		if a.Status != "pending" {
			return fmt.Errorf("仅 pending 状态可确认（当前 %s）", a.Status)
		}
		if a.SupersedesID != nil {
			if err := s.Attributes.SetStatusExt(ctx, tx, *a.SupersedesID, "superseded"); err != nil {
				return err
			}
			if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
				PersonID: a.PersonID, EntityKind: "attribute", EntityID: a.SupersedesID,
				AttrKey: strPtr(a.AttrKey), ChangeType: "supersede", ChangedBy: "user",
				Note: strPtr("冲突确认：旧值被新值替换"),
			}); err != nil {
				return err
			}
		}
		if err := s.Attributes.SetStatusExt(ctx, tx, id, "active"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: a.PersonID, EntityKind: "attribute", EntityID: &id,
			AttrKey: strPtr(a.AttrKey), ChangeType: "confirm", ChangedBy: "user",
			NewValue: snap(a.ValueText), OldValue: snap(""),
			Confidence: fp(a.Confidence), EpistemicType: strPtr(a.EpistemicType),
		}); err != nil {
			return err
		}
	case "relationship":
		rel, err := s.Relationships.Get(ctx, id)
		if err != nil {
			return err
		}
		if rel == nil {
			return ErrNotFound
		}
		if rel.Status != "pending" {
			return fmt.Errorf("仅 pending 状态可确认（当前 %s）", rel.Status)
		}
		if rel.SupersedesID != nil {
			if err := s.Relationships.SetStatusExt(ctx, tx, *rel.SupersedesID, "superseded"); err != nil {
				return err
			}
		}
		if err := s.Relationships.SetStatusExt(ctx, tx, id, "active"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: rel.PersonID, EntityKind: "relationship", EntityID: &id,
			ChangeType: "confirm", ChangedBy: "user", NewValue: snap(rel.RelationType),
			Confidence: fp(rel.Confidence),
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("未知 kind: %s（可选 person|attribute|relationship）", kind)
	}
	return tx.Commit()
}

// DismissPending 放弃一条 pending（或手动 dismiss 任意行）→ dismissed + 审计。
func (s *Service) DismissPending(ctx context.Context, kind string, id ids.ID) error {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	switch kind {
	case "person":
		p, err := s.Persons.Get(ctx, id)
		if err != nil {
			return err
		}
		if p == nil {
			return ErrNotFound
		}
		if err := s.Persons.SetStatus(ctx, id, "dismissed"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: id, EntityKind: "person", EntityID: &id,
			ChangeType: "dismiss", ChangedBy: "user", OldValue: snap(p.DisplayName),
			Note: strPtr("放弃 LLM 自动新建的人物"),
		}); err != nil {
			return err
		}
	case "attribute":
		a, err := s.Attributes.Get(ctx, id)
		if err != nil {
			return err
		}
		if a == nil {
			return ErrNotFound
		}
		if err := s.Attributes.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: a.PersonID, EntityKind: "attribute", EntityID: &id,
			AttrKey: strPtr(a.AttrKey), ChangeType: "dismiss", ChangedBy: "user",
			OldValue: snap(a.ValueText), Confidence: fp(a.Confidence),
		}); err != nil {
			return err
		}
	case "relationship":
		rel, err := s.Relationships.Get(ctx, id)
		if err != nil {
			return err
		}
		if rel == nil {
			return ErrNotFound
		}
		if err := s.Relationships.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: rel.PersonID, EntityKind: "relationship", EntityID: &id,
			ChangeType: "dismiss", ChangedBy: "user", OldValue: snap(rel.RelationType),
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("未知 kind: %s（可选 person|attribute|relationship）", kind)
	}
	return tx.Commit()
}
