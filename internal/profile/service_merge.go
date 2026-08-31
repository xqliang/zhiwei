// service_merge 人物合并（「作为别名并入」，2026-08-31 需求）：
// LLM 按别名误建的人物（如已给解保功配别名「老保」、抽取提到「老保」仍新建 pending 人物——
// 根因已由 FindByNameOrAliasExt 修复，存量误建行靠本合并收口）并入目标人物：
// 源名字成为目标别名、名下八平面数据全量转移、源置 merged 保留审计。
package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

var (
	// ErrSamePerson 并入目标不能是人物自身。
	ErrSamePerson = errors.New("不能并入人物自身")
	// ErrBadMergeTarget 目标人物不可用（已删除/已并入他人）。
	ErrBadMergeTarget = errors.New("并入目标不可用（已删除或已并入他人）")
	// ErrOwnerUnmergeable owner「我」不能被并入他人（画像主体消失）。
	ErrOwnerUnmergeable = errors.New("「我」不能并入他人")
)

// ManualMergeAsAlias 把 source 并入 target，source 的名字转为 target 的别名。
// 单事务（模式对齐 ManualSetPersonStatus / SpeakerRepo.MergeInto）：任一步失败整体回滚，
// 不留「部分转移」中间态。转移后行 status 不动——source 名下的 pending 项照旧进 target 的
// 确认队列，由用户逐条确认；change_log 双向留痕。
func (s *Service) ManualMergeAsAlias(ctx context.Context, userID int64, sourceID, targetID ids.ID) error {
	if sourceID == targetID {
		return ErrSamePerson
	}
	src, err := s.Persons.Get(ctx, userID, sourceID) // 行级 user 隔离：越权 nil → ErrNotFound
	if err != nil {
		return err
	}
	if src == nil {
		return ErrNotFound
	}
	if src.IsOwner {
		return ErrOwnerUnmergeable
	}
	tgt, err := s.Persons.Get(ctx, userID, targetID)
	if err != nil {
		return err
	}
	if tgt == nil {
		return ErrNotFound
	}
	if tgt.Status == "dismissed" || tgt.Status == "merged" {
		return ErrBadMergeTarget
	}

	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // Commit 成功后为 no-op

	// 1) 八平面 person_id 全量改指（含 memory.person_id 记忆归属，迁移 000028）。
	// person_id 为雪花 id 全局唯一，不会跨用户；仍统一带 user_id 过滤做防御。
	for _, tbl := range []string{
		"person_attribute", "person_relationship", "person_event", "person_metric",
		"person_cycle", "person_activity", "person_pet", "memory",
	} {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET person_id = ? WHERE person_id = ? AND user_id = ?`, tbl),
			targetID.Int64(), sourceID.Int64(), userID); err != nil {
			return fmt.Errorf("转移 %s: %w", tbl, err)
		}
	}
	// 2) 关系对端反向改指（「A 与 source 是同事」→「A 与 target 是同事」）；
	//    改指后出现 person_id == related_person_id 的自环行（target 与自己有关系）→ 删除。
	//    同向可能的重复行（source 和 target 原本都与 X 有关系）不在此去重——通常是
	//    pending 行，留给确认队列人工取舍。
	if _, err := tx.ExecContext(ctx,
		`UPDATE person_relationship SET related_person_id = ? WHERE related_person_id = ? AND user_id = ?`,
		targetID.Int64(), sourceID.Int64(), userID); err != nil {
		return fmt.Errorf("转移关系对端: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM person_relationship WHERE person_id = ? AND related_person_id = ?`,
		targetID.Int64(), targetID.Int64()); err != nil {
		return fmt.Errorf("清理自环关系: %w", err)
	}
	// 3) event.related_person_ids JSON 数组里的 source id → target（Go 侧改写：SQL 改 JSON 数组
	//    元素笨拙）。改写后去重、剔除指向事件主人自己的项（同场人物不该含本人）；空数组置 NULL。
	if err := rewriteEventRelatedIDs(ctx, tx, sourceID, targetID); err != nil {
		return err
	}
	// 4) speaker 绑定：source 有绑定且 target 没有则移过去（并按人物↔声纹名不变式同步
	//    speaker.name = target.display_name）；target 已有绑定则把 source 的绑定摘掉
	//    （防止 merged 人物继续持有绑定）。注意 person.speaker_id 有 UNIQUE 约束——
	//    移动须先摘 source 再挂 target，顺序反了会撞唯一键。
	if src.SpeakerID != nil {
		if tgt.SpeakerID == nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE person SET speaker_id = NULL WHERE id = ?`, sourceID.Int64()); err != nil {
				return fmt.Errorf("摘除源声纹绑定: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE person SET speaker_id = ? WHERE id = ?`, src.SpeakerID.Int64(), targetID.Int64()); err != nil {
				return fmt.Errorf("移动声纹绑定: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE speaker SET name = ? WHERE id = ?`, tgt.DisplayName, src.SpeakerID.Int64()); err != nil {
				return fmt.Errorf("同步声纹名: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx,
			`UPDATE person SET speaker_id = NULL WHERE id = ?`, sourceID.Int64()); err != nil {
			return fmt.Errorf("摘除源声纹绑定: %w", err)
		}
	}
	// 5) 别名行：source 的名字成为 target 的 aliases 属性（active/manual——用户显式操作）。
	//    target 已有同名别名（此前手动加过）则跳过，不产生重复行。
	var nAlias int
	if err := tx.GetContext(ctx, &nAlias,
		`SELECT COUNT(*) FROM person_attribute
		 WHERE person_id = ? AND attr_key = 'aliases' AND value_text = ?`,
		targetID.Int64(), src.DisplayName); err != nil {
		return fmt.Errorf("查重别名: %w", err)
	}
	if nAlias == 0 {
		if err := s.Attributes.CreateExt(ctx, tx, &repo.PersonAttribute{
			UserID: userID, PersonID: targetID, AttrKey: "aliases", ValueText: src.DisplayName,
			ValueType: "text", Source: "manual", Status: "active",
		}); err != nil {
			return fmt.Errorf("写别名行: %w", err)
		}
	}
	// 6) source 置 merged（行保留做审计；List/ListWithPending 均排除，不再出现在名册/队列）。
	if err := s.Persons.SetStatusExt(ctx, tx, sourceID, "merged"); err != nil {
		return fmt.Errorf("置 merged: %w", err)
	}
	// 7) 双向审计。
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: targetID, EntityKind: "person", EntityID: &targetID,
		ChangeType: "merge", ChangedBy: "user", NewValue: snap(src.DisplayName),
		Note: strPtr(fmt.Sprintf("并入人物「%s」（#%s），其名转为别名", src.DisplayName, sourceID)),
	}); err != nil {
		return fmt.Errorf("写目标审计: %w", err)
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: sourceID, EntityKind: "person", EntityID: &sourceID,
		ChangeType: "merge", ChangedBy: "user", NewValue: snap(tgt.DisplayName),
		Note: strPtr(fmt.Sprintf("已并入「%s」（#%s），名字成为其别名", tgt.DisplayName, targetID)),
	}); err != nil {
		return fmt.Errorf("写源审计: %w", err)
	}
	return tx.Commit()
}

// rewriteEventRelatedIDs 把 target 名下 event 行 related_person_ids JSON 数组中的 source id
// 替换为 target：去重、剔除自指（数组含事件主人自己）、空数组置 NULL。只在有变化时写回。
func rewriteEventRelatedIDs(ctx context.Context, tx *sqlx.Tx, sourceID, targetID ids.ID) error {
	rows, err := tx.QueryxContext(ctx,
		`SELECT id, related_person_ids FROM person_event
		 WHERE person_id = ? AND related_person_ids IS NOT NULL`, targetID.Int64())
	if err != nil {
		return fmt.Errorf("读 event 同场人物: %w", err)
	}
	defer rows.Close()
	type upd struct {
		id  ids.ID
		val any // []ids.ID 或 nil（空数组置 NULL）
	}
	var updates []upd
	for rows.Next() {
		var (
			evID ids.ID
			raw  []byte
		)
		if err := rows.Scan(&evID, &raw); err != nil {
			return fmt.Errorf("扫 event 行: %w", err)
		}
		var list []int64
		if err := json.Unmarshal(raw, &list); err != nil {
			continue // 历史脏 JSON：跳过不改写（不因单行脏数据卡死整个合并）
		}
		changed := false
		seen := map[int64]bool{}
		var out []int64
		for _, v := range list {
			nv := v
			if v == sourceID.Int64() {
				nv = targetID.Int64()
				changed = true
			}
			if nv == targetID.Int64() {
				// 剔除自指（同场人物不该含事件主人）——source id 本就等于替换后的 target，故一并覆盖
				changed = true
				continue
			}
			if seen[nv] {
				changed = true // 去重掉的也算变化
				continue
			}
			seen[nv] = true
			out = append(out, nv)
		}
		if !changed {
			continue
		}
		if len(out) == 0 {
			updates = append(updates, upd{id: evID, val: nil})
			continue
		}
		b, err := json.Marshal(out)
		if err != nil {
			return fmt.Errorf("序列化同场人物: %w", err)
		}
		updates = append(updates, upd{id: evID, val: string(b)})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历 event 行: %w", err)
	}
	for _, u := range updates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE person_event SET related_person_ids = ? WHERE id = ?`, u.val, u.id.Int64()); err != nil {
			return fmt.Errorf("写回同场人物: %w", err)
		}
	}
	return nil
}
