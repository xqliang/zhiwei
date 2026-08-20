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

func (r *TopicRepo) UpdateStatus(ctx context.Context, id ids.ID, status string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE topic SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *TopicRepo) UpdateName(ctx context.Context, id ids.ID, name string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE topic SET name = ? WHERE id = ?`, name, id.Int64())
	return err
}
