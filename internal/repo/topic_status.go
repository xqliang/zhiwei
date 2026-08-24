package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// TopicStatus 是某 topic 的状态快照（进展/todo/风险），追加式历史。
// Content 为可空 JSON，用 *json.RawMessage（NULL→nil；对齐 job.go / review.go）。
type TopicStatus struct {
	ID          ids.ID           `db:"id" json:"id"`
	UserID      int64            `db:"user_id" json:"user_id"`
	TopicID     ids.ID           `db:"topic_id" json:"topic_id"`
	Content     *json.RawMessage `db:"content" json:"content,omitempty"`
	GeneratedAt time.Time        `db:"generated_at" json:"generated_at"`
}

type TopicStatusRepo struct{ DB *sqlx.DB }

// Insert 追加一条快照（不 upsert，保留历史）。UserID 传 0 视为 1。
func (r *TopicStatusRepo) Insert(ctx context.Context, userID int64, topicID ids.ID, content json.RawMessage) error {
	if userID == 0 {
		userID = 1
	}
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO topic_status (id, user_id, topic_id, content)
VALUES (?, ?, ?, ?)`, ids.New().Int64(), userID, topicID.Int64(), []byte(content))
	return err
}

// GetLatest 取某 topic 最新快照；无则返回 (nil, nil)。
func (r *TopicStatusRepo) GetLatest(ctx context.Context, topicID ids.ID) (*TopicStatus, error) {
	var s TopicStatus
	err := r.DB.GetContext(ctx, &s, `
SELECT * FROM topic_status WHERE topic_id = ?
ORDER BY generated_at DESC, id DESC LIMIT 1`, topicID.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}
