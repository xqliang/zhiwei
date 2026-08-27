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
func (s *Service) ManualCreatePerson(ctx context.Context, userID int64, name string, speakerID *ids.ID, summary *string) (*repo.Person, error) {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	p, err := s.ManualCreatePersonExt(ctx, tx, userID, name, speakerID, summary)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// ManualCreatePersonExt 是 ManualCreatePerson 的事务版：全部写走传入的 tx，不自开/自提事务，
// 供调用方（如 agent 关系提议确认闸门）把「建人 + 关系写 + Proposals.Resolve」原子并进同一
// 事务（apply-once）。落库语义与 ManualCreatePerson 完全一致（active/manual + create 审计）。
// 注意：不 tx.Rollback()/Commit()——事务生命周期归调用方。
func (s *Service) ManualCreatePersonExt(ctx context.Context, tx *sqlx.Tx, userID int64, name string, speakerID *ids.ID, summary *string) (*repo.Person, error) {
	p := &repo.Person{UserID: userID, DisplayName: name, SpeakerID: speakerID, Summary: summary, Source: "manual", Status: "active"}
	if err := s.Persons.CreateExt(ctx, tx, p); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: p.ID, EntityKind: "person", EntityID: &p.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(p.DisplayName),
	}); err != nil {
		return nil, err
	}
	// 建档即绑声纹：同步声纹名 = 人物名（绑定不变式，见 syncSpeakerNameExt）
	if err := s.syncSpeakerNameExt(ctx, tx, speakerID, p.DisplayName); err != nil {
		return nil, err
	}
	return p, nil
}

// syncSpeakerNameExt 绑定不变式：声纹绑定人物后，speaker.name 跟随 person.display_name。
// 时间线/转写段/抽取 prompt 各处直接显示 speaker.name，同步后无需逐处改成查 person 表。
// 仅在「绑定/换绑/人物改名」时调用；解绑（speakerID=nil）不回改——历史名保留，回改反而
// 会把已叫开的称呼抹掉。与人物写同事务执行，失败整体回滚，不留「人物已改、声纹没跟」的中间态。
func (s *Service) syncSpeakerNameExt(ctx context.Context, tx *sqlx.Tx, speakerID *ids.ID, name string) error {
	if speakerID == nil {
		return nil
	}
	return s.Speakers.UpdateNameExt(ctx, tx, *speakerID, name)
}

// BindSpeakerToPerson 声纹侧关联管理（名册「关联/换绑/解绑」入口）：把声纹的归属设为
// 目标人物（target=nil 即解绑）。与人物侧 PATCH /api/persons 的差别在语义：人物侧换的是
// 「人物绑哪条声纹」，声纹已被**别人**占用则 409 拒绝；本方法是「声纹归哪个人物」，
// **转移**语义——单事务内先清原持有人的绑定（person.speaker_id 有唯一键，不清必撞），
// 再绑目标人物并同步声纹名=人物名（绑定不变式）。解绑不清声纹名（保留历史名）。
func (s *Service) BindSpeakerToPerson(ctx context.Context, userID int64, speakerID ids.ID, target *ids.ID) error {
	// 目标人物校验：归属登录用户（越权 404）+ 未绑其他声纹（一人至多一声纹，409）。
	var tp *repo.Person
	if target != nil {
		var err error
		tp, err = s.Persons.Get(ctx, userID, *target)
		if err != nil {
			return err
		}
		if tp == nil {
			return ErrNotFound
		}
		if tp.SpeakerID != nil && *tp.SpeakerID != speakerID {
			return fmt.Errorf("%w：「%s」", ErrPersonHasSpeaker, tp.DisplayName)
		}
	}
	holder, err := s.Persons.GetBySpeaker(ctx, speakerID)
	if err != nil {
		return err
	}
	// 幂等：无目标且本就无人持有 / 目标即现持有人 → no-op。
	if (target == nil && holder == nil) || (target != nil && holder != nil && holder.ID == tp.ID) {
		return nil
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// ① 清原持有（解绑 or 转移的前半步）：只动 speaker_id，声纹名保留。
	if holder != nil {
		if err := s.Persons.UpdateExt(ctx, tx, holder.ID, holder.DisplayName, nil, holder.Summary); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: holder.ID, EntityKind: "person", EntityID: &holder.ID,
			ChangeType: "update", ChangedBy: "user",
			OldValue: snap("声纹绑定"), NewValue: snap("已解绑"),
			Note: strPtr("声纹名册侧解除人物关联"),
		}); err != nil {
			return err
		}
	}
	// ② 绑目标 + 声纹名同步为人物名（不变式）。
	if target != nil {
		if err := s.Persons.UpdateExt(ctx, tx, tp.ID, tp.DisplayName, &speakerID, tp.Summary); err != nil {
			return err
		}
		if err := s.syncSpeakerNameExt(ctx, tx, &speakerID, tp.DisplayName); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: tp.ID, EntityKind: "person", EntityID: &tp.ID,
			ChangeType: "update", ChangedBy: "user",
			OldValue: snap("未绑声纹"), NewValue: snap(tp.DisplayName),
			Note: strPtr("声纹名册侧关联人物（声纹名已同步为人物名）"),
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ManualUpdatePerson 手动编辑人物（改名/换绑声纹/改备注）。
func (s *Service) ManualUpdatePerson(ctx context.Context, userID int64, id ids.ID, name string, speakerID *ids.ID, summary *string) error {
	p, err := s.Persons.Get(ctx, userID, id) // 按登录用户过滤：越权命中 0 行 → nil → ErrNotFound
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
	// 绑定不变式同步：换绑/绑定时把新声纹名改成人物名；人物改名时连带改已绑声纹名
	// （解绑传 nil → no-op，声纹名保留）。调用方（api/person.go Patch）传的是「终态」
	// speakerID——不传即保留原绑定，故人物改名场景也会走到这里完成声纹联动。
	if err := s.syncSpeakerNameExt(ctx, tx, speakerID, name); err != nil {
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

// ManualSetPersonStatus 人物状态流转（删除=dismissed 等）。
//
// F5（spec §13）删除级联：status=="dismissed"（删除人物）时，在**同一事务**内把该人物六个平面
// （属性/关系/大事记/指标/周期/活动）上所有 active|pending 的行一并置 dismissed——否则名册里人物
// 虽已隐藏，其平面行仍会进抽取闸门/确认队列，留下孤儿引用。**另加**反向边补充（P6）：他人指向
// 本人的 pending 关系边也一并级联 dismissed（active 反向边刻意保留，那是对端画像）。级联时行
// dismiss 前的状态记入 pre_dismiss_status 列（active|pending），供下面的恢复级联回查。
// 级联行数**汇总**记入 person 的 change_log Note（一行审计），**不逐平面、不逐行**写审计条目，
// 这是刻意取舍：
//   - 删除是「显式用户意图」——用户已明确要清掉这个人的全部画像数据，无需逐行留痕来还原意图；
//   - 六平面可能有成百上千行（metric/activity 是测点流），逐行写审计会让 change_log 爆量、得不偿失。
//     故只在 person 行上留一条带级联计数的汇总审计（可审计「删除时清了多少」，够用）。
//
// 恢复级联（000015 迁移后支持）：**从 dismissed 流转为其他状态**（即恢复，典型 active）时，
// 在同一事务内把六平面上 pre_dismiss_status 非空的级联行翻回原状态并清标记，**以及**被
// DismissPendingReverseExt 清掉的指向本人的反向 pending 边（RestoreReverseArchivedExt）——
// 手动删除的行 pre_dismiss_status 为 NULL，天然不被误恢复。非 dismissed 之间的流转
// （如 active→pending）不触发级联。
func (s *Service) ManualSetPersonStatus(ctx context.Context, userID int64, id ids.ID, status string) error {
	p, err := s.Persons.Get(ctx, userID, id) // 按登录用户过滤：越权命中 0 行 → nil → ErrNotFound
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

	// 默认审计备注；删除/恢复时改写为带六平面级联计数的汇总备注。
	note := "人物状态流转"
	if status == "dismissed" {
		// 六平面各自把该人物的 active/pending 行级联置 dismissed（只动活跃态，终态不动——见各 repo
		// DismissAllByPersonExt 注释）。全部在本事务内执行，任一步失败经 defer Rollback 整体回滚，
		// 不会留下「人物已删除但平面未级联」的中间态。
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
		// 反向边补充（F5，P6）：他人指向本人（related_person_id=id）的 pending 关系边——删除本人后
		// 这些「待确认关系」成了确认队列里的孤儿噪声，一并级联 dismissed。active 反向边刻意不动
		// （那是对端人物画像，删除不替对端做主——见 DismissPendingReverseExt 注释）。行数并入汇总审计。
		nRevRel, err := s.Relationships.DismissPendingReverseExt(ctx, tx, id)
		if err != nil {
			return err
		}
		note = fmt.Sprintf("人物删除：级联 dismissed 属性 %d/关系 %d/大事记 %d/指标 %d/周期 %d/活动 %d 行；反向 pending 关系边 %d 条",
			nAttr, nRel, nEvt, nMet, nCyc, nAct, nRevRel)
	} else if p.Status == "dismissed" {
		// 恢复级联：六平面各自把删除时被级联置 dismissed 的行翻回原状态（pre_dismiss_status）。
		// 手动删的行没有标记、保持 dismissed。同样全在本事务内，任一步失败整体回滚。
		nAttr, err := s.Attributes.RestoreArchivedExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nRel, err := s.Relationships.RestoreArchivedExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nEvt, err := s.Events.RestoreArchivedExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nMet, err := s.Metrics.RestoreArchivedExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nCyc, err := s.Cycles.RestoreArchivedExt(ctx, tx, id)
		if err != nil {
			return err
		}
		nAct, err := s.Activities.RestoreArchivedExt(ctx, tx, id)
		if err != nil {
			return err
		}
		// 反向边对称恢复（P6 反向级联的逆）：被清掉的反向 pending 边翻回 pending，
		// 重新回到确认队列。
		nRevRel, err := s.Relationships.RestoreReverseArchivedExt(ctx, tx, id)
		if err != nil {
			return err
		}
		note = fmt.Sprintf("人物恢复：级联恢复 属性 %d/关系 %d/大事记 %d/指标 %d/周期 %d/活动 %d 行；反向 pending 关系边 %d 条（手动删过的行不恢复）",
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
// 自持事务：BeginTxx → ManualAddAttributeExt → Commit（行为/签名与历史一致，
// 现有 api/person.go 调用面零改）。真正的写逻辑在 Ext 变体里，便于并入他人的事务
// （如 agent 提议确认闸门的单事务 apply-once，见 internal/agent/proposals.go）。
// F4 写入端校验/规范化在 Ext 单写点做（覆盖 API 与 agent 两条路径）。
func (s *Service) ManualAddAttribute(ctx context.Context, userID int64, personID ids.ID, attrKey, value string) (*repo.PersonAttribute, error) {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	row, err := s.ManualAddAttributeExt(ctx, tx, userID, personID, attrKey, value)
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
func (s *Service) ManualAddAttributeExt(ctx context.Context, tx *sqlx.Tx, userID int64, personID ids.ID, attrKey, value string) (*repo.PersonAttribute, error) {
	// IDOR 校验：确认 person 归属登录用户（越权命中 0 行 → nil → ErrNotFound）。
	if p, err := s.Persons.Get(ctx, userID, personID); err != nil {
		return nil, err
	} else if p == nil {
		return nil, ErrNotFound
	}
	d := Def(attrKey)
	// F4 写入端校验/规范化（单点闸，见 validate.go）：手动/agent 路径录入的脏值（gender=「男性」、
	// smokes=「是」、birthday=「八月三号」）在此拦下，error 原样透传 API 层（handler errors.Is
	// ErrInvalidAttrValue → 400）。与 LLM 路径共用 NormalizeAttrValue，规范化后的值贯穿后续
	// existing 查询、幂等比较与落库。放在 Ext 单写点，API（经 ManualAddAttribute）与 agent 提议
	// 确认（直接调 Ext）两条路径都过校验。合并对账（全范围）：从 main 的 ManualAddAttribute 移植而来。
	norm, verr := NormalizeAttrValue(d, value)
	if verr != nil {
		return nil, verr
	}
	value = norm

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
		UserID: userID, PersonID: personID, AttrKey: attrKey, ValueText: value, ValueType: d.ValueType,
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
func (s *Service) ManualDeleteAttribute(ctx context.Context, userID int64, id ids.ID) error {
	a, err := s.Attributes.Get(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrNotFound
	}
	// IDOR 校验：子表行 Get 无 user 过滤，先按行的 person_id 确认归属登录用户。
	if p, err := s.Persons.Get(ctx, userID, a.PersonID); err != nil {
		return err
	} else if p == nil {
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
func (s *Service) ManualAddRelationship(ctx context.Context, userID int64, personID ids.ID, relationType string,
	relatedPersonID *ids.ID, direction, orgName, label string) (*repo.PersonRelationship, error) {

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	row, err := s.ManualAddRelationshipExt(ctx, tx, userID, personID, relationType, relatedPersonID, direction, orgName, label)
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
func (s *Service) ManualAddRelationshipExt(ctx context.Context, tx *sqlx.Tx, userID int64, personID ids.ID, relationType string,
	relatedPersonID *ids.ID, direction, orgName, label string) (*repo.PersonRelationship, error) {

	// IDOR 校验：确认主体 person 归属登录用户（关联对端 relatedPersonID 可能是本事务内刚建的
	// 新人，故不在此校验，仅校验作为写入锚点的 personID）。
	if p, err := s.Persons.Get(ctx, userID, personID); err != nil {
		return nil, err
	} else if p == nil {
		return nil, ErrNotFound
	}
	if !ValidRelations[relationType] {
		return nil, fmt.Errorf("非法关系类型: %s", relationType)
	}
	row := &repo.PersonRelationship{
		UserID: userID, PersonID: personID, RelatedPersonID: relatedPersonID, RelationType: relationType,
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
func (s *Service) ManualDeleteRelationship(ctx context.Context, userID int64, id ids.ID) error {
	rel, err := s.Relationships.Get(ctx, id)
	if err != nil {
		return err
	}
	if rel == nil {
		return ErrNotFound
	}
	// IDOR 校验：子表行 Get 无 user 过滤，先按行的 person_id 确认归属登录用户。
	if p, err := s.Persons.Get(ctx, userID, rel.PersonID); err != nil {
		return err
	} else if p == nil {
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
// ManualAddEvent 手动加大事记（active/manual conf=1.0 + create 审计）。
// relatedPersonIDs 可空/空切片（无同行人物）——P2a② 支持多人同行，逐个落 RelatedPersonIDs 数组；
// occurredAt/endAt 是原始字符串（YYYY-MM-DD/YYYY-MM/RFC3339，parseEventAt 尽力解析，失败存 NULL）；
// importance 为事件人生分量（P2a①）——传 0 走事件类型默认（defaultImportance），>0 clamp 到 (0,1]，
// 不再固定 1.0（手动录入的日常事件不该天然「满分重要」，与 LLM 路径共用同一取值链
// eventImportanceOrDefault）；参数多，调用方为 API handler。
// 自持事务：BeginTxx → ManualAddEventExt → Commit（行为/签名与历史一致）。
func (s *Service) ManualAddEvent(ctx context.Context, userID int64, personID ids.ID, eventType, title,
	description, occurredAt, endAt, location string, relatedPersonIDs []ids.ID, importance float64) (*repo.PersonEvent, error) {

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	row, err := s.ManualAddEventExt(ctx, tx, userID, personID, eventType, title, description, occurredAt, endAt, location, relatedPersonIDs, importance)
	if err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualAddEventExt 是 ManualAddEvent 的事务版：全部写走传入的 tx，不自开/自提事务，
// 供调用方（如 agent 提议确认闸门）把「事件写 + Proposals.Resolve」原子并进同一事务（D1）。
// 校验与落库语义与 ManualAddEvent 完全一致（event_type 合法 + title 非空 + 审计 changed_by=user）。
func (s *Service) ManualAddEventExt(ctx context.Context, tx *sqlx.Tx, userID int64, personID ids.ID, eventType, title,
	description, occurredAt, endAt, location string, relatedPersonIDs []ids.ID, importance float64) (*repo.PersonEvent, error) {

	// IDOR 校验：确认 person 归属登录用户（越权命中 0 行 → nil → ErrNotFound）。
	if p, err := s.Persons.Get(ctx, userID, personID); err != nil {
		return nil, err
	} else if p == nil {
		return nil, ErrNotFound
	}
	if !ValidEventTypes[eventType] {
		return nil, fmt.Errorf("非法事件类型: %s", eventType)
	}
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("title 不能为空")
	}
	row := &repo.PersonEvent{
		UserID: userID, PersonID: personID, EventType: eventType, Title: strings.TrimSpace(title),
		Confidence: 1.0, EpistemicType: "observed", Source: "manual", Status: "active",
		Importance: eventImportanceOrDefault(importance, eventType),
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
	if len(relatedPersonIDs) > 0 {
		row.RelatedPersonIDs = ids.List(relatedPersonIDs)
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
func (s *Service) ManualDeleteEvent(ctx context.Context, userID int64, id ids.ID) error {
	e, err := s.Events.Get(ctx, id)
	if err != nil {
		return err
	}
	if e == nil {
		return ErrNotFound
	}
	// IDOR 校验：子表行 Get 无 user 过滤，先按行的 person_id 确认归属登录用户。
	if p, err := s.Persons.Get(ctx, userID, e.PersonID); err != nil {
		return err
	} else if p == nil {
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
func (s *Service) ManualAddMetric(ctx context.Context, userID int64, personID ids.ID, metricKey string,
	valueNum *float64, valueText, unit string, measuredAt time.Time) (*repo.PersonMetric, error) {

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	row, err := s.ManualAddMetricExt(ctx, tx, userID, personID, metricKey, valueNum, valueText, unit, measuredAt)
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
func (s *Service) ManualAddMetricExt(ctx context.Context, tx *sqlx.Tx, userID int64, personID ids.ID, metricKey string,
	valueNum *float64, valueText, unit string, measuredAt time.Time) (*repo.PersonMetric, error) {

	// IDOR 校验：确认 person 归属登录用户（越权命中 0 行 → nil → ErrNotFound）。
	if p, err := s.Persons.Get(ctx, userID, personID); err != nil {
		return nil, err
	} else if p == nil {
		return nil, ErrNotFound
	}
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
		UserID: userID, PersonID: personID, MetricKey: metricKey,
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
func (s *Service) ManualDeleteMetric(ctx context.Context, userID int64, id ids.ID) error {
	m, err := s.Metrics.Get(ctx, id)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrNotFound
	}
	// IDOR 校验：子表行 Get 无 user 过滤，先按行的 person_id 确认归属登录用户。
	if p, err := s.Persons.Get(ctx, userID, m.PersonID); err != nil {
		return err
	} else if p == nil {
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

// ---- cycle 平面手动 CRUD（P3 周期/日程，敏感）----

// ManualAddCycle 手动加周期/日程（active/manual conf=1.0 + create 审计）。label 空→nil；
// next_predicted_at 经 applyCycleParams 与 LLM 路径共用同一算法（anchor+period）；period/
// duration<=0 不落列（同 LLM 路径「未给不设」）。自持事务：BeginTxx → ManualAddCycleExt → Commit。
func (s *Service) ManualAddCycle(ctx context.Context, userID int64, personID ids.ID, cycleType, label, anchorDate,
	frequency, dosage string, periodDays, durationDays int) (*repo.PersonCycle, error) {

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := s.ManualAddCycleExt(ctx, tx, userID, personID, cycleType, label, anchorDate, frequency, dosage, periodDays, durationDays)
	if err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualAddCycleExt 是 ManualAddCycle 的事务版：全部写走传入的 tx，不自开/自提事务，供调用方
// （如 agent 周期提议确认闸门）把「周期写 + Proposals.Resolve」原子并进同一事务（apply-once，
// 见 proposals.go 的 profile_cycle case）。校验（cycle_type 合法 + IDOR）与落库语义与 ManualAddCycle 一致。
func (s *Service) ManualAddCycleExt(ctx context.Context, tx *sqlx.Tx, userID int64, personID ids.ID, cycleType, label, anchorDate,
	frequency, dosage string, periodDays, durationDays int) (*repo.PersonCycle, error) {

	if !ValidCycleTypes[cycleType] {
		return nil, fmt.Errorf("非法周期类型: %s", cycleType)
	}
	// IDOR 校验：确认 person 归属登录用户（越权命中 0 行 → nil → ErrNotFound）。
	if p, err := s.Persons.Get(ctx, userID, personID); err != nil {
		return nil, err
	} else if p == nil {
		return nil, ErrNotFound
	}
	row := &repo.PersonCycle{
		UserID: userID, PersonID: personID, CycleType: cycleType,
		Confidence: 1.0, EpistemicType: "observed", Source: "manual", Status: "active",
	}
	if l := strings.TrimSpace(label); l != "" {
		row.Label = &l
	}
	applyCycleParams(row, anchorDate, periodDays, durationDays, dosage, frequency)
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
	return row, nil
}

// ManualDeleteCycle 手动删周期 → dismissed + delete 审计。
func (s *Service) ManualDeleteCycle(ctx context.Context, userID int64, id ids.ID) error {
	c, err := s.Cycles.Get(ctx, id)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	// IDOR 校验：子表行 Get 无 user 过滤，先按行的 person_id 确认归属登录用户。
	if p, err := s.Persons.Get(ctx, userID, c.PersonID); err != nil {
		return err
	} else if p == nil {
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
// 没有「对话发生时刻」可依，不知道时间就记当下（区别于 LLM 路径的 fallbackAt——那里能用
// session.created_at）。自持事务：BeginTxx → ManualAddActivityExt → Commit。
func (s *Service) ManualAddActivity(ctx context.Context, userID int64, personID ids.ID, activity, tool, location,
	commuteMode, startedAt string, durationMin int) (*repo.PersonActivity, error) {

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := s.ManualAddActivityExt(ctx, tx, userID, personID, activity, tool, location, commuteMode, startedAt, durationMin)
	if err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualAddActivityExt 是 ManualAddActivity 的事务版：全部写走传入的 tx，供调用方（如 agent 活动
// 提议确认闸门）把「活动写 + Proposals.Resolve」原子并进同一事务（apply-once，见 proposals.go 的
// profile_activity case）。校验（activity 非空 + IDOR）与落库语义与 ManualAddActivity 一致。
func (s *Service) ManualAddActivityExt(ctx context.Context, tx *sqlx.Tx, userID int64, personID ids.ID, activity, tool, location,
	commuteMode, startedAt string, durationMin int) (*repo.PersonActivity, error) {

	act := strings.TrimSpace(activity)
	if act == "" {
		return nil, fmt.Errorf("activity 不能为空")
	}
	// IDOR 校验：确认 person 归属登录用户（越权命中 0 行 → nil → ErrNotFound）。
	if p, err := s.Persons.Get(ctx, userID, personID); err != nil {
		return nil, err
	} else if p == nil {
		return nil, ErrNotFound
	}
	at := time.Now()
	if t, ok := parseEventAt(startedAt); ok {
		at = t
	}
	row := &repo.PersonActivity{
		UserID: userID, PersonID: personID, Activity: act, StartedAt: at,
		Tool:        trimToPtr(tool),
		Location:    trimToPtr(location),
		CommuteMode: trimToPtr(commuteMode),
		Confidence:  1.0, EpistemicType: "observed", Source: "manual", Status: "active",
	}
	if durationMin > 0 {
		dm := durationMin
		row.DurationMin = &dm
	}
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
	return row, nil
}

// ManualDeleteActivity 手动删活动 → dismissed + delete 审计。
func (s *Service) ManualDeleteActivity(ctx context.Context, userID int64, id ids.ID) error {
	a, err := s.Activities.Get(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrNotFound
	}
	// IDOR 校验：子表行 Get 无 user 过滤，先按行的 person_id 确认归属登录用户。
	if p, err := s.Persons.Get(ctx, userID, a.PersonID); err != nil {
		return err
	} else if p == nil {
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
