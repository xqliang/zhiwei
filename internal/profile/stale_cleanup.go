// stale_cleanup.go 实现画像平面的「残留清理」：用户修正 ASR 后重新提取（同 session
// 重跑 profile）时，旧文本独有、未被新事实命中的画像行（如 ASR 把「划船」错识成
//「化妆」，改文本后「化妆」活动行不再被任何事实对应）连同其 change_log 一并删除，
// 让本 session 的画像产物与 memory/todo 一样「以最新 ASR 文本为准」。
//
// 方案是**精准删除**而非全删重插：只删未被本次事实触碰（dedup 命中/refine 目标/
// reaffirm 目标，见 ApplyStats.touch）的本 session 旧行——同键同值 skip、同键变值
// refine（reextract_dedup_test 的契约）与敏感平面 pending-supersedes 语义全部保留。
package profile

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// staleRowTables 残留清理覆盖的画像平面表（均带 session_id 列；person 行不经
// session 产出、不在清理范围——LLM 抽取自动新建的人物见 applyPersonFact）。
var staleRowTables = []string{
	"person_attribute", "person_relationship", "person_event", "person_metric",
	"person_cycle", "person_activity", "person_pet",
}

// snapshotSessionRows 事务内快照本 session 在各画像平面的现有行 id
//（残留=快照 - 白名单；新建行不在快照里，天然不受影响）。
func snapshotSessionRows(ctx context.Context, tx *sqlx.Tx, sessionID ids.ID) (map[string]map[ids.ID]bool, error) {
	out := make(map[string]map[ids.ID]bool, len(staleRowTables))
	for _, table := range staleRowTables {
		rows, err := tx.QueryxContext(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE session_id = ?`, table), sessionID.Int64())
		if err != nil {
			return nil, fmt.Errorf("快照 %s: %w", table, err)
		}
		set := map[ids.ID]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("快照 %s 读 id: %w", table, err)
			}
			set[ids.ID(id)] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("快照 %s: %w", table, err)
		}
		rows.Close()
		out[table] = set
	}
	return out, nil
}

// deleteStaleRows 删除残留行（快照 - 白名单）及其 change_log（按 entity_id 关联；
// 再限定 session_id 双保险，防误删其它 session 指向同一行的日志——理论不会发生，
// entity_id 唯一指向被删行，但防御性收敛删除范围）。返回删除的行数。
//
// 并发安全：与落新事实同一事务（调用方 tx），单语句 DELETE 原子；残留行 id 在
// 快照时已固定，即便并发进程此刻在同表插行也不影响本集合（新行不在快照、不删）。
func deleteStaleRows(ctx context.Context, tx *sqlx.Tx, sessionID ids.ID,
	snapshot, touched map[string]map[ids.ID]bool) (int, error) {
	removed := 0
	for _, table := range staleRowTables {
		var stale []int64
		for id := range snapshot[table] {
			if !touched[table][id] {
				stale = append(stale, id.Int64())
			}
		}
		if len(stale) == 0 {
			continue
		}
		q, args, err := sqlx.In(fmt.Sprintf(`DELETE FROM %s WHERE id IN (?)`, table), stale)
		if err != nil {
			return removed, fmt.Errorf("删除残留 %s: %w", table, err)
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return removed, fmt.Errorf("删除残留 %s: %w", table, err)
		}
		// 级联清理这些行的 change_log（entity_id 关联；session 双保险）。
		lq, largs, err := sqlx.In(
			`DELETE FROM person_change_log WHERE session_id = ? AND entity_id IN (?)`,
			sessionID.Int64(), stale)
		if err != nil {
			return removed, fmt.Errorf("删除残留 %s 的 change_log: %w", table, err)
		}
		if _, err := tx.ExecContext(ctx, lq, largs...); err != nil {
			return removed, fmt.Errorf("删除残留 %s 的 change_log: %w", table, err)
		}
		removed += len(stale)
	}
	return removed, nil
}
