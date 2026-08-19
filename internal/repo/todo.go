package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Todo 是从对话中提取的待办。suggested 需用户确认后转 confirmed。
type Todo struct {
	ID             ids.ID     `db:"id" json:"id"`
	UserID         int64      `db:"user_id" json:"user_id"`
	Title          string     `db:"title" json:"title"`
	SourceMemoryID *ids.ID    `db:"source_memory_id" json:"source_memory_id,omitempty"`
	TopicID        *ids.ID    `db:"topic_id" json:"topic_id,omitempty"`
	Status         string     `db:"status" json:"status"` // suggested|confirmed|done|dismissed
	DueAt          *time.Time `db:"due_at" json:"due_at,omitempty"`
	Confidence     float64    `db:"confidence" json:"confidence"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
}

// TodoRow 是带来源会话的列表视图（待办页「跳转时间线」用）。
type TodoRow struct {
	Todo
	SourceSessionID *ids.ID `db:"source_session_id" json:"source_session_id,omitempty"`
}

// CanTransition 校验 todo 状态流转。
// 合法路径：suggested→confirmed、confirmed→done、任意非 dismissed→dismissed。
// dismissed 是终态，任何状态不可回退（重跑 extract 生成新 suggested 行而非复活旧行）。
func CanTransition(from, to string) bool {
	switch {
	case from == "suggested" && to == "confirmed":
		return true
	case from == "confirmed" && to == "done":
		return true
	case (from == "suggested" || from == "confirmed" || from == "done") && to == "dismissed":
		return true
	}
	return false
}

type TodoRepo struct{ DB *sqlx.DB }

// InsertExt 批量插入（ext 传 *sqlx.Tx 即加入事务；必须传 *Todo 指针切片以接收回填 ID）。
func (r *TodoRepo) InsertExt(ctx context.Context, ext ExecerContext, todos []*Todo) error {
	if len(todos) == 0 {
		return nil
	}
	for i := range todos {
		todos[i].ID = ids.New()
		if todos[i].UserID == 0 {
			todos[i].UserID = 1
		}
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO todo (id, user_id, title, source_memory_id, topic_id, status, due_at, confidence)
VALUES (:id, :user_id, :title, :source_memory_id, :topic_id, :status, :due_at, :confidence)`, todos)
	return err
}

// DeleteBySessionExt 删除派生自某 session 全部 memory 的 todo（经 source_memory_id
// 子查询关联）。extract 重跑幂等用：必须与 MemoryRepo 的删除/插入共用同一 *sqlx.Tx，
// 且在删 memory 之前调用（子查询依赖 memory 行仍存在）。
func (r *TodoRepo) DeleteBySessionExt(ctx context.Context, ext ExecerContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx, `
DELETE FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?)`,
		sessionID.Int64())
	return err
}

func (r *TodoRepo) Get(ctx context.Context, id ids.ID) (*Todo, error) {
	var td Todo
	err := r.DB.GetContext(ctx, &td, `SELECT * FROM todo WHERE id = ?`, id.Int64())
	return &td, err
}

// UpdateStatus 更新状态。调用方（service 层）应先用 CanTransition 校验流转合法性。
func (r *TodoRepo) UpdateStatus(ctx context.Context, id ids.ID, status string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE todo SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

const todoListBase = `
SELECT t.*, m.session_id AS source_session_id
FROM todo t LEFT JOIN memory m ON t.source_memory_id = m.id`

// List 列表。status / topicID 为空不过滤；dismissed 永不出现。
func (r *TodoRepo) List(ctx context.Context, status string, topicID *ids.ID) ([]TodoRow, error) {
	sql := todoListBase + " WHERE t.status != 'dismissed'"
	var args []any
	if status != "" {
		sql += " AND t.status = ?"
		args = append(args, status)
	}
	if topicID != nil {
		sql += " AND t.topic_id = ?"
		args = append(args, topicID.Int64())
	}
	sql += " ORDER BY t.id DESC LIMIT 200"
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, sql, args...)
	return rows, err
}

// ListByTopic 是 Topic 详情页的 todo 列表（含已完成，不含 dismissed）。
func (r *TodoRepo) ListByTopic(ctx context.Context, topicID ids.ID) ([]TodoRow, error) {
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, todoListBase+`
 WHERE t.topic_id = ? AND t.status != 'dismissed' ORDER BY t.id DESC`, topicID.Int64())
	return rows, err
}

// ListBySession 是时间线详情页的 todo 列表（含已完成，不含 dismissed）。
func (r *TodoRepo) ListBySession(ctx context.Context, sessionID ids.ID) ([]TodoRow, error) {
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, todoListBase+`
 WHERE m.session_id = ? AND t.status != 'dismissed' ORDER BY t.id DESC`, sessionID.Int64())
	return rows, err
}
