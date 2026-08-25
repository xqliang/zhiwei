package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Topic 是记忆的组织层：AI 抽取时自动归类/建议，用户可确认、改名、忽略。
type Topic struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	Name        string    `db:"name" json:"name"`
	Description *string   `db:"description" json:"description,omitempty"`
	Status      string    `db:"status" json:"status"`         // suggested|active|dismissed
	CreatedBy   string    `db:"created_by" json:"created_by"` // ai|user
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// TopicWithCount 是列表接口的带计数视图。
type TopicWithCount struct {
	Topic
	MemoryCount   int `db:"memory_count" json:"memory_count"`       // active memory 数
	OpenTodoCount int `db:"open_todo_count" json:"open_todo_count"` // confirmed（未完成）todo 数
}

type TopicRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务，传 r.DB 即独立执行）。
func (r *TopicRepo) CreateExt(ctx context.Context, ext ExecerContext, tp *Topic) error {
	tp.ID = ids.New()
	if tp.UserID == 0 {
		tp.UserID = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO topic (id, user_id, name, description, status, created_by)
VALUES (:id, :user_id, :name, :description, :status, :created_by)`, tp)
	return err
}

func (r *TopicRepo) Create(ctx context.Context, tp *Topic) error {
	return r.CreateExt(ctx, r.DB, tp)
}

func (r *TopicRepo) Get(ctx context.Context, id ids.ID) (*Topic, error) {
	var tp Topic
	err := r.DB.GetContext(ctx, &tp, `SELECT * FROM topic WHERE id = ?`, id.Int64())
	return &tp, err
}

// ListActive 返回 active + suggested 的主题（抽取 prompt 输入 / 合并查重用），
// 按更新时间倒序，最多 limit 条。
func (r *TopicRepo) ListActive(ctx context.Context, userID int64, limit int) ([]Topic, error) {
	var list []Topic
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM topic
WHERE user_id = ? AND status IN ('active','suggested')
ORDER BY updated_at DESC LIMIT ?`, userID, limit)
	return list, err
}

// FindActiveByName 按名称精确查找 active/suggested 主题（同名合并用）；无命中返回 nil。
func (r *TopicRepo) FindActiveByName(ctx context.Context, userID int64, name string) (*Topic, error) {
	return r.FindActiveByNameExt(ctx, r.DB, userID, name)
}

// FindActiveByNameExt 与 FindActiveByName 同语义，但可在事务连接上执行
// （ext 传 *sqlx.Tx）。extract commit 事务内对建议 topic 查重用：
// 事务内首个一致性读建立快照，此重查须在事务内 DELETE 之前没有普通
// SELECT 的前提下才可靠（并发窗口已收窄而非消除，见 stage_extract 注释）。
func (r *TopicRepo) FindActiveByNameExt(ctx context.Context, ext QueryRowxContext, userID int64, name string) (*Topic, error) {
	var tp Topic
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM topic
WHERE user_id = ? AND name = ? AND status IN ('active','suggested')
ORDER BY id LIMIT 1`, userID, name).StructScan(&tp)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &tp, nil
}

// ListWithCounts 列出非 dismissed 主题及关联计数（Topics 页用）。
// 计数走关联表 memory_topic/todo_topic（多对多），不再依赖 legacy topic_id。
func (r *TopicRepo) ListWithCounts(ctx context.Context, userID int64) ([]TopicWithCount, error) {
	var list []TopicWithCount
	err := r.DB.SelectContext(ctx, &list, `
SELECT t.*,
  (SELECT COUNT(*) FROM memory_topic mt JOIN memory m ON mt.memory_id=m.id
     WHERE mt.topic_id = t.id AND m.status='active') AS memory_count,
  (SELECT COUNT(*) FROM todo_topic tt JOIN todo td ON tt.todo_id=td.id
     WHERE tt.topic_id = t.id AND td.status='confirmed') AS open_todo_count
FROM topic t
WHERE t.user_id = ? AND t.status != 'dismissed'
ORDER BY memory_count DESC, open_todo_count DESC, t.updated_at DESC`, userID)
	return list, err
}

// ListDismissed 列出已忽略（dismissed）主题及关联计数，供「已忽略主题」折叠区查看/恢复。
// 与 ListWithCounts 互补：后者排除 dismissed，本方法只取 dismissed，按更新时间倒序。
func (r *TopicRepo) ListDismissed(ctx context.Context, userID int64) ([]TopicWithCount, error) {
	var list []TopicWithCount
	err := r.DB.SelectContext(ctx, &list, `
SELECT t.*,
  (SELECT COUNT(*) FROM memory_topic mt JOIN memory m ON mt.memory_id=m.id
     WHERE mt.topic_id = t.id AND m.status='active') AS memory_count,
  (SELECT COUNT(*) FROM todo_topic tt JOIN todo td ON tt.todo_id=td.id
     WHERE tt.topic_id = t.id AND td.status='confirmed') AS open_todo_count
FROM topic t
WHERE t.user_id = ? AND t.status = 'dismissed'
ORDER BY t.updated_at DESC`, userID)
	return list, err
}

// UpdateStatusExt 是 UpdateStatus 的事务版：SQL 与非事务版一致，只把执行器由 r.DB
// 换成 ext（传 *sqlx.Tx 即加入调用方事务）。供确认闸门在与 Proposals.Resolve 同一事务内
// 改 topic 状态用（topic_confirm→active / topic_dismiss→dismissed）。
func (r *TopicRepo) UpdateStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx, `UPDATE topic SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

// UpdateStatus 委托 UpdateStatusExt（传 r.DB，非事务），行为与重构前完全一致。
func (r *TopicRepo) UpdateStatus(ctx context.Context, id ids.ID, status string) error {
	return r.UpdateStatusExt(ctx, r.DB, id, status)
}

// UpdateNameExt 是 UpdateName 的事务版：SQL 与非事务版一致（原 UpdateName 无额外
// 规范化/校验，仅一条 UPDATE topic SET name），只把执行器由 r.DB 换成 ext。
// 供确认闸门在与 Proposals.Resolve 同一事务内改名用（topic_rename）。
func (r *TopicRepo) UpdateNameExt(ctx context.Context, ext ExecerContext, id ids.ID, name string) error {
	_, err := ext.ExecContext(ctx, `UPDATE topic SET name = ? WHERE id = ?`, name, id.Int64())
	return err
}

// UpdateName 委托 UpdateNameExt（传 r.DB，非事务），行为与重构前完全一致。
func (r *TopicRepo) UpdateName(ctx context.Context, id ids.ID, name string) error {
	return r.UpdateNameExt(ctx, r.DB, id, name)
}

// MergeGroup 一组合并：canonical_name 是规范名（命中已有 active/suggested 同名则复用，
// 否则新建 active/ai topic）；member_ids 是被并入的 topic。各 member 的关联迁到 canonical
// 后置 dismissed。用于 T7 智能合并：Consolidate（LLM 提议）+ Merge（用户确认后落库）。
type MergeGroup struct {
	CanonicalName string   `json:"canonical_name"`
	MemberIDs     []ids.ID `json:"member_ids"`
}

// MergeGroups 单事务合并多组 topic：每组找/建 canonical，把各 member 的 memory_topic/
// todo_topic 关联 INSERT IGNORE 迁到 canonical（PK 天然去重），删 member 关联行，member
// 置 dismissed。member==canonical 跳过。userID 固定 1（单用户 MVP，与 CreateExt 一致）。
//
// 事务设计：全部操作在同一 tx 内完成，任一步出错整体 ROLLBACK，保证关联不丢。
// INSERT IGNORE 迁移 → DELETE 旧关联 → UPDATE member dismissed，顺序保证即使 member
// 与 canonical 共享某些 owner（memory/todo），迁移后旧关联删除也不影响 canonical 行。
func (r *TopicRepo) MergeGroups(ctx context.Context, groups []MergeGroup) error {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, g := range groups {
		if len(g.MemberIDs) == 0 {
			continue
		}
		// 找/建 canonical：命中已有 active/suggested 同名则复用，否则新建 active/ai
		var cid ids.ID
		if ex, err := r.FindActiveByNameExt(ctx, tx, 1, g.CanonicalName); err != nil {
			return err
		} else if ex != nil {
			cid = ex.ID
		} else {
			tp := &Topic{Name: g.CanonicalName, Status: "active", CreatedBy: "ai"}
			if err := r.CreateExt(ctx, tx, tp); err != nil {
				return err
			}
			cid = tp.ID
		}
		for _, mid := range g.MemberIDs {
			if mid == cid {
				continue // member==canonical 跳过
			}
			// 迁 memory_topic：INSERT IGNORE 把 member 的关联复制到 canonical（PK 去重）
			if _, err := tx.ExecContext(ctx,
				`INSERT IGNORE INTO memory_topic (memory_id, topic_id, source)
				 SELECT memory_id, ?, source FROM memory_topic WHERE topic_id = ?`,
				cid.Int64(), mid.Int64()); err != nil {
				return err
			}
			// 删 member 的 memory_topic 关联（迁移后清理旧指针）
			if _, err := tx.ExecContext(ctx, `DELETE FROM memory_topic WHERE topic_id = ?`, mid.Int64()); err != nil {
				return err
			}
			// 迁 todo_topic：同样 INSERT IGNORE + DELETE
			if _, err := tx.ExecContext(ctx,
				`INSERT IGNORE INTO todo_topic (todo_id, topic_id, source)
				 SELECT todo_id, ?, source FROM todo_topic WHERE topic_id = ?`,
				cid.Int64(), mid.Int64()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM todo_topic WHERE topic_id = ?`, mid.Int64()); err != nil {
				return err
			}
			// member 置 dismissed（保留行，便于审计/撤销；不再出现在 active/suggested 列表）
			if _, err := tx.ExecContext(ctx, `UPDATE topic SET status='dismissed' WHERE id = ?`, mid.Int64()); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Delete 硬删除 topic + 其 memory_topic/todo_topic 关联（单事务级联）。区别于 dismiss
// （PATCH dismissed 软删，保留行）。不存在也不报错。与 MergeGroups 互补：MergeGroups
// 迁移关联+保留 member 行（审计），Delete 彻底清 topic+关联。
func (r *TopicRepo) Delete(ctx context.Context, id ids.ID) error {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_topic WHERE topic_id = ?`, id.Int64()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM todo_topic WHERE topic_id = ?`, id.Int64()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM topic WHERE id = ?`, id.Int64()); err != nil {
		return err
	}
	return tx.Commit()
}
