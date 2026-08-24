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

// DailyReview / WeeklyReview 是结构化报告的持久化行（content 为可空 JSON）。
// Content 用 *json.RawMessage（可空 JSON 用指针，NULL→nil；对齐 job.go / agent_message）。
type DailyReview struct {
	ID         ids.ID           `db:"id" json:"id"`
	UserID     int64            `db:"user_id" json:"user_id"`
	ReviewDate time.Time        `db:"review_date" json:"review_date"`
	Content    *json.RawMessage `db:"content" json:"content,omitempty"`
	Status     string           `db:"status" json:"status"`
	CreatedAt  time.Time        `db:"created_at" json:"created_at"`
}

type WeeklyReview struct {
	ID        ids.ID           `db:"id" json:"id"`
	UserID    int64            `db:"user_id" json:"user_id"`
	WeekStart time.Time        `db:"week_start" json:"week_start"`
	WeekEnd   time.Time        `db:"week_end" json:"week_end"`
	Content   *json.RawMessage `db:"content" json:"content,omitempty"`
	Status    string           `db:"status" json:"status"`
	CreatedAt time.Time        `db:"created_at" json:"created_at"`
}

type ReviewRepo struct{ DB *sqlx.DB }

// UpsertDaily 按 (user_id, review_date) upsert：存在则覆盖 content/status。
// content 需为合法 JSON（调用方保证；报告生成器产出合法 JSON）。
func (r *ReviewRepo) UpsertDaily(ctx context.Context, userID int64, date time.Time, content json.RawMessage, status string) error {
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO daily_review (id, user_id, review_date, content, status)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE content = VALUES(content), status = VALUES(status)`,
		ids.New().Int64(), userID, date, []byte(content), status)
	return err
}

// GetDaily 取某天日报；不存在返回 (nil, nil)。
func (r *ReviewRepo) GetDaily(ctx context.Context, userID int64, date time.Time) (*DailyReview, error) {
	var d DailyReview
	err := r.DB.GetContext(ctx, &d,
		`SELECT * FROM daily_review WHERE user_id = ? AND review_date = ?`, userID, date)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// UpsertWeekly 按 (user_id, week_start) upsert。
func (r *ReviewRepo) UpsertWeekly(ctx context.Context, userID int64, start, end time.Time, content json.RawMessage, status string) error {
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO weekly_review (id, user_id, week_start, week_end, content, status)
VALUES (?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE week_end = VALUES(week_end), content = VALUES(content), status = VALUES(status)`,
		ids.New().Int64(), userID, start, end, []byte(content), status)
	return err
}

// GetWeekly 取某周周报；不存在返回 (nil, nil)。
func (r *ReviewRepo) GetWeekly(ctx context.Context, userID int64, start time.Time) (*WeeklyReview, error) {
	var w WeeklyReview
	err := r.DB.GetContext(ctx, &w,
		`SELECT * FROM weekly_review WHERE user_id = ? AND week_start = ?`, userID, start)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}
