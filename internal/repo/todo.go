package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Todo 是从对话中提取的待办。suggested 需用户确认后转 confirmed。
// topic 归属走关联表 todo_topic（多对多），本结构不再承载单值 topic_id。
type Todo struct {
	ID             ids.ID     `db:"id" json:"id"`
	UserID         int64      `db:"user_id" json:"user_id"`
	Title          string     `db:"title" json:"title"`
	SourceMemoryID *ids.ID    `db:"source_memory_id" json:"source_memory_id,omitempty"`
	Status         string     `db:"status" json:"status"` // suggested|confirmed|done|dismissed
	DueAt          *time.Time `db:"due_at" json:"due_at,omitempty"`
	Confidence     float64    `db:"confidence" json:"confidence"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
}

// TodoRow 是带来源会话与 topics 的列表视图（待办页「跳转时间线」用）。
// Topics 由 attachTopics 在查询后填充（无 db tag，不参与 SQL 映射）。
type TodoRow struct {
	Todo
	SourceSessionID *ids.ID    `db:"source_session_id" json:"source_session_id,omitempty"`
	Topics           []TopicInfo `json:"topics,omitempty"`
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
INSERT INTO todo (id, user_id, title, source_memory_id, status, due_at, confidence)
VALUES (:id, :user_id, :title, :source_memory_id, :status, :due_at, :confidence)`, todos)
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

// UpdateStatus 更新状态。状态值先做合法性校验（防绕过 API 层校验的垃圾值入库）；
// 流转合法性（CanTransition）仍由调用方负责。「不存在或状态未变」返回 nil（MySQL
// 同值 UPDATE 的 RowsAffected 为 0，无法与不存在区分，MVP 接受该语义）。
func (r *TodoRepo) UpdateStatus(ctx context.Context, id ids.ID, status string) error {
	if !validTodoStatus(status) {
		return fmt.Errorf("非法 todo 状态: %q", status)
	}
	_, err := r.DB.ExecContext(ctx, `UPDATE todo SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

// validTodoStatus 是 todo 状态的枚举校验。
func validTodoStatus(s string) bool {
	switch s {
	case "suggested", "confirmed", "done", "dismissed":
		return true
	}
	return false
}

const todoListBase = `
SELECT t.*, m.session_id AS source_session_id
FROM todo t LEFT JOIN memory m ON t.source_memory_id = m.id`

// List 列表。status / topicID 为空不过滤；dismissed 永不出现。
// topicID 非空时走关联表 todo_topic 子查询过滤（不走 legacy todo.topic_id）。
func (r *TodoRepo) List(ctx context.Context, status string, topicID *ids.ID) ([]TodoRow, error) {
	sql := todoListBase + " WHERE t.status != 'dismissed'"
	var args []any
	if status != "" {
		sql += " AND t.status = ?"
		args = append(args, status)
	}
	if topicID != nil {
		sql += " AND t.id IN (SELECT todo_id FROM todo_topic WHERE topic_id = ?)"
		args = append(args, topicID.Int64())
	}
	sql += " ORDER BY t.id DESC LIMIT 200"
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, sql, args...)
	if err != nil {
		return nil, err
	}
	if err := r.attachTopics(ctx, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// ListByTopic 是 Topic 详情页的 todo 列表（含已完成，不含 dismissed）。
func (r *TodoRepo) ListByTopic(ctx context.Context, topicID ids.ID) ([]TodoRow, error) {
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, todoListBase+`
 WHERE t.id IN (SELECT todo_id FROM todo_topic WHERE topic_id = ?)
   AND t.status != 'dismissed' ORDER BY t.id DESC`, topicID.Int64())
	if err != nil {
		return nil, err
	}
	if err := r.attachTopics(ctx, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// ListBySession 是时间线详情页的 todo 列表（含已完成，不含 dismissed）。
func (r *TodoRepo) ListBySession(ctx context.Context, sessionID ids.ID) ([]TodoRow, error) {
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, todoListBase+`
 WHERE m.session_id = ? AND t.status != 'dismissed' ORDER BY t.id DESC`, sessionID.Int64())
	if err != nil {
		return nil, err
	}
	if err := r.attachTopics(ctx, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// ListOpenTitlesExt 返回未关闭（suggested+confirmed）todo 的标题，落库去重比对用。
// 事务内调用传 tx（能看到本事务内 DeleteBySessionExt 已删的本 session todo，
// 避免重跑时旧 todo 自去重导致幂等失败），事务外调用传 r.DB。
func (r *TodoRepo) ListOpenTitlesExt(ctx context.Context, q QueryerContext, userID int64) ([]string, error) {
	var titles []string
	err := q.SelectContext(ctx, &titles,
		`SELECT title FROM todo WHERE user_id = ? AND status IN ('suggested','confirmed')`, userID)
	return titles, err
}

// attachTopics 给列表行内联 topics[]（走关联表多对多聚合，空列表安全）。
func (r *TodoRepo) attachTopics(ctx context.Context, rows []TodoRow) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]ids.ID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	m, err := (&TodoTopicRepo{DB: r.DB}).ListByTodoIDs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range rows {
		rows[i].Topics = m[rows[i].ID]
	}
	return nil
}
