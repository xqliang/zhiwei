package repo

import (
	"context"
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
	var tp Topic
	err := r.DB.GetContext(ctx, &tp, `
SELECT * FROM topic
WHERE user_id = ? AND name = ? AND status IN ('active','suggested')
ORDER BY id LIMIT 1`, userID, name)
	if err != nil {
		if err.Error() == "sql: no rows" {
			return nil, nil
		}
		return nil, err
	}
	return &tp, nil
}

// ListWithCounts 列出非 dismissed 主题及关联计数（Topics 页用）。
func (r *TopicRepo) ListWithCounts(ctx context.Context, userID int64) ([]TopicWithCount, error) {
	var list []TopicWithCount
	err := r.DB.SelectContext(ctx, &list, `
SELECT t.*,
  (SELECT COUNT(*) FROM memory m WHERE m.topic_id = t.id AND m.status = 'active') AS memory_count,
  (SELECT COUNT(*) FROM todo td WHERE td.topic_id = t.id AND td.status = 'confirmed') AS open_todo_count
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
