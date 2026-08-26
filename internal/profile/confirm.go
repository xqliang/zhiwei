package profile

import (
	"context"
	"fmt"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// ConfirmPending 确认一条 pending（kind ∈ person|attribute|relationship|event|metric|cycle）：
// pending → active；attribute/relationship/event/cycle 若带 supersedes_id，被指向的旧行 → superseded
// （metric 无 supersedes，测点是独立采样、无版本取代语义）。每步变更记审计（changed_by=user）。
// 非 pending 行确认报错（幂等由前端/状态保证）。
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
		if err := s.Persons.SetStatusExt(ctx, tx, id, "active"); err != nil {
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
			NewValue:   snap(a.ValueText),
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
			if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
				PersonID: rel.PersonID, EntityKind: "relationship", EntityID: rel.SupersedesID,
				ChangeType: "supersede", ChangedBy: "user",
				Note: strPtr("冲突确认：旧关系被新关系替换"),
			}); err != nil {
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
	case "event":
		e, err := s.Events.Get(ctx, id)
		if err != nil {
			return err
		}
		if e == nil {
			return ErrNotFound
		}
		if e.Status != "pending" {
			return fmt.Errorf("仅 pending 状态可确认（当前 %s）", e.Status)
		}
		// 事件平面当前无冲突路径（DecideEvent 只有 reaffirm/create），SupersedesID 一般为 nil；
		// 这里与 attribute/relationship 确认分支保持一致，防御性处理带 supersedes 的情况：
		// 旧行置 superseded 并补一条 supersede 审计，避免旧行状态被静默改写而无审计痕迹。
		if e.SupersedesID != nil {
			if err := s.Events.SetStatusExt(ctx, tx, *e.SupersedesID, "superseded"); err != nil {
				return err
			}
			if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
				PersonID: e.PersonID, EntityKind: "event", EntityID: e.SupersedesID,
				ChangeType: "supersede", ChangedBy: "user",
				Note: strPtr("冲突确认：旧事件被新事件替换"),
			}); err != nil {
				return err
			}
		}
		if err := s.Events.SetStatusExt(ctx, tx, id, "active"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: e.PersonID, EntityKind: "event", EntityID: &id,
			ChangeType: "confirm", ChangedBy: "user", NewValue: snap(e.Title),
			Confidence: fp(e.Confidence),
		}); err != nil {
			return err
		}
	case "metric":
		m, err := s.Metrics.Get(ctx, id)
		if err != nil {
			return err
		}
		if m == nil {
			return ErrNotFound
		}
		if m.Status != "pending" {
			return fmt.Errorf("仅 pending 状态可确认（当前 %s）", m.Status)
		}
		// metric 无 supersedes_id（测点是独立采样，无「新版本取代旧版本」语义，见 repo 说明）——
		// 故无冲突分支，直接 pending → active + confirm 审计。value_text 可空（类别型有值、
		// 数值型存 fmt 串），空则记空串（与 ManualDeleteMetric 的 OldValue 兜底一致）。
		if err := s.Metrics.SetStatusExt(ctx, tx, id, "active"); err != nil {
			return err
		}
		mv := ""
		if m.ValueText != nil {
			mv = *m.ValueText
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: m.PersonID, EntityKind: "metric", EntityID: &id,
			ChangeType: "confirm", ChangedBy: "user", NewValue: snap(mv),
			Confidence: fp(m.Confidence),
		}); err != nil {
			return err
		}
	case "cycle":
		c, err := s.Cycles.Get(ctx, id)
		if err != nil {
			return err
		}
		if c == nil {
			return ErrNotFound
		}
		if c.Status != "pending" {
			return fmt.Errorf("仅 pending 状态可确认（当前 %s）", c.Status)
		}
		// cycle 有 supersedes_id（单值语义：用药/周期调整走冲突 pending 指向当前 active 行）——
		// 确认时旧 active 行 → superseded 并补 supersede 审计（与 attribute/relationship 同构）。
		if c.SupersedesID != nil {
			if err := s.Cycles.SetStatusExt(ctx, tx, *c.SupersedesID, "superseded"); err != nil {
				return err
			}
			if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
				PersonID: c.PersonID, EntityKind: "cycle", EntityID: c.SupersedesID,
				ChangeType: "supersede", ChangedBy: "user",
				Note: strPtr("冲突确认：旧周期被新周期替换"),
			}); err != nil {
				return err
			}
		}
		if err := s.Cycles.SetStatusExt(ctx, tx, id, "active"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: c.PersonID, EntityKind: "cycle", EntityID: &id,
			ChangeType: "confirm", ChangedBy: "user", NewValue: snap(c.CycleType),
			Confidence: fp(c.Confidence),
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("未知 kind: %s（可选 person|attribute|relationship|event|metric|cycle）", kind)
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
		if err := s.Persons.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
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
	case "event":
		e, err := s.Events.Get(ctx, id)
		if err != nil {
			return err
		}
		if e == nil {
			return ErrNotFound
		}
		if err := s.Events.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: e.PersonID, EntityKind: "event", EntityID: &id,
			ChangeType: "dismiss", ChangedBy: "user", OldValue: snap(e.Title),
		}); err != nil {
			return err
		}
	case "metric":
		m, err := s.Metrics.Get(ctx, id)
		if err != nil {
			return err
		}
		if m == nil {
			return ErrNotFound
		}
		if err := s.Metrics.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
			return err
		}
		mv := ""
		if m.ValueText != nil {
			mv = *m.ValueText
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: m.PersonID, EntityKind: "metric", EntityID: &id,
			ChangeType: "dismiss", ChangedBy: "user", OldValue: snap(mv),
		}); err != nil {
			return err
		}
	case "cycle":
		c, err := s.Cycles.Get(ctx, id)
		if err != nil {
			return err
		}
		if c == nil {
			return ErrNotFound
		}
		if err := s.Cycles.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: c.PersonID, EntityKind: "cycle", EntityID: &id,
			ChangeType: "dismiss", ChangedBy: "user", OldValue: snap(c.CycleType),
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("未知 kind: %s（可选 person|attribute|relationship|event|metric|cycle）", kind)
	}
	return tx.Commit()
}
