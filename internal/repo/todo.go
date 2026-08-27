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
	SourceSessionID *ids.ID     `db:"source_session_id" json:"source_session_id,omitempty"`
	Topics          []TopicInfo `json:"topics,omitempty"`
}

// CanTransition 校验 todo 状态流转。
// 合法路径：suggested→confirmed、suggested→done（待确认批量完成，跳过确认）、
// confirmed→done、done→confirmed（已完成重新打开）、任意非 dismissed→dismissed。
// dismissed 是终态，任何状态不可回退（重跑 extract 生成新 suggested 行而非复活旧行）。
func CanTransition(from, to string) bool {
	switch {
	case from == "suggested" && to == "confirmed":
		return true
	case from == "suggested" && to == "done": // 批量完成：待确认跳过确认直接完成
		return true
	case from == "confirmed" && to == "done":
		return true
	case from == "done" && to == "confirmed": // 重新打开：已完成回到进行中
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

// Get 按 id 查待办，并强制 user_id 隔离（多租户越权防护）：SQL 追加 AND user_id = ?，
// 用 userID 读他人待办时命中 0 行，沿用 GetContext 既有语义返回 sql.ErrNoRows（handler 转 404）。
func (r *TodoRepo) Get(ctx context.Context, userID int64, id ids.ID) (*Todo, error) {
	var td Todo
	err := r.DB.GetContext(ctx, &td, `SELECT * FROM todo WHERE id = ? AND user_id = ?`, id.Int64(), userID)
	return &td, err
}

// GetExt 事务内加行锁读取（SELECT ... FOR UPDATE）。写-提议闸门确认 todo_status 时需先锁行
// 读当前状态做 CanTransition 校验再改，锁读避免与并发状态变更竞争（评审 I1/I2）。
func (r *TodoRepo) GetExt(ctx context.Context, q QueryRowxContext, id ids.ID) (*Todo, error) {
	var td Todo
	err := q.QueryRowxContext(ctx, `SELECT * FROM todo WHERE id = ? FOR UPDATE`, id.Int64()).StructScan(&td)
	return &td, err
}

// UpdateStatusExt 是 UpdateStatus 的事务版：状态枚举校验 + SQL 与非事务版一致，
// 只把执行器由 r.DB 换成 ext（传 *sqlx.Tx 即加入调用方事务）。供「写-提议闸门」的
// 确认端点在与 Proposals.Resolve 同一事务内改 todo 状态用（apply-once）。
// 流转合法性（CanTransition）仍由调用方负责。
func (r *TodoRepo) UpdateStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	if !validTodoStatus(status) {
		return fmt.Errorf("非法 todo 状态: %q", status)
	}
	_, err := ext.ExecContext(ctx, `UPDATE todo SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

// UpdateStatus 更新状态。状态值先做合法性校验（防绕过 API 层校验的垃圾值入库）；
// 流转合法性（CanTransition）仍由调用方负责。「不存在或状态未变」返回 nil（MySQL
// 同值 UPDATE 的 RowsAffected 为 0，无法与不存在区分，MVP 接受该语义）。
// 委托 UpdateStatusExt（传 r.DB，非事务），行为与重构前完全一致。
func (r *TodoRepo) UpdateStatus(ctx context.Context, id ids.ID, status string) error {
	return r.UpdateStatusExt(ctx, r.DB, id, status)
}

// UpdateTitle 改待办标题（用户手改）。不做状态校验；状态流转走 UpdateStatus。
// 「不存在」返回 nil（UPDATE 0 行，与 UpdateStatus 同语义）。
func (r *TodoRepo) UpdateTitle(ctx context.Context, id ids.ID, title string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE todo SET title = ? WHERE id = ?`, title, id.Int64())
	return err
}

// Delete 硬删除待办 + 其 todo_topic 关联（单事务级联）。区别于 dismiss（软删，
// 保留行+状态 dismissed）。不存在也不报错（DELETE 返回 0 行）。区别于
// DeleteBySessionExt（按 session 批删派生 todo）。
func (r *TodoRepo) Delete(ctx context.Context, id ids.ID) error {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM todo_topic WHERE todo_id = ?`, id.Int64()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM todo WHERE id = ?`, id.Int64()); err != nil {
		return err
	}
	return tx.Commit()
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

// List 列表。强制 user_id 隔离（WHERE t.user_id = ?），只列该用户的待办。
// status / topicID 为空不过滤；dismissed 永不出现。
// topicID 非空时走关联表 todo_topic 子查询过滤（不走 legacy todo.topic_id）。
func (r *TodoRepo) List(ctx context.Context, userID int64, status string, topicID *ids.ID) ([]TodoRow, error) {
	sql := todoListBase + " WHERE t.user_id = ? AND t.status != 'dismissed'"
	args := []any{userID}
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

// ListDismissed 返回某用户已忽略（status=dismissed）待办，供前端「已忽略」折叠区展示。
// 强制 user_id 隔离（WHERE t.user_id = ?）。dismissed 是终态不可恢复，此处仅供查看 + 硬删。
// 与 List（排除 dismissed）互补。
func (r *TodoRepo) ListDismissed(ctx context.Context, userID int64) ([]TodoRow, error) {
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, todoListBase+`
 WHERE t.user_id = ? AND t.status = 'dismissed' ORDER BY t.id DESC LIMIT 200`, userID)
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

// DedupSuggested 折叠 suggested todo 的归一化标题重复：每组保留 created_at 最旧一条，
// 其余置 dismissed。单事务。返回 dismissed 数。用于 extract 存量一次性清理
// （cmd/dedup-todos）；新数据由 commitExtract 落库去重（T3，ListOpenTitlesExt）兜底。
// 与 T3 互补：T3 防新增重复，本方法清存量重复。
func (r *TodoRepo) DedupSuggested(ctx context.Context, userID int64) (int, error) {
	type row struct {
		ID        ids.ID    `db:"id"`
		Title     string    `db:"title"`
		CreatedAt time.Time `db:"created_at"`
	}
	var rows []row
	if err := r.DB.SelectContext(ctx, &rows,
		`SELECT id, title, created_at FROM todo WHERE user_id = ? AND status = 'suggested' ORDER BY created_at`, userID); err != nil {
		return 0, err
	}
	keep := map[string]bool{} // norm -> 已保留最旧
	var dismiss []ids.ID
	for _, x := range rows {
		k := NormalizeTitle(x.Title)
		if k == "" {
			continue // 空标题不参与去重
		}
		if !keep[k] {
			keep[k] = true // 第一条（ORDER BY created_at 最旧）保留
			continue
		}
		dismiss = append(dismiss, x.ID)
	}
	if len(dismiss) == 0 {
		return 0, nil
	}
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range dismiss {
		if _, err := tx.ExecContext(ctx, `UPDATE todo SET status='dismissed' WHERE id=?`, id.Int64()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(dismiss), nil
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
