package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

type AudioSession struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	Source      string    `db:"source" json:"source"`
	Filename    string    `db:"filename" json:"filename"`
	StoragePath string    `db:"storage_path" json:"-"`
	DurationMS  int64     `db:"duration_ms" json:"duration_ms"`
	Mime        string    `db:"mime" json:"mime"`
	Status      string    `db:"status" json:"status"`
	JobID       *ids.ID   `db:"job_id" json:"job_id,omitempty"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

type SessionRepo struct{ DB *sqlx.DB }

func (r *SessionRepo) Create(ctx context.Context, s *AudioSession) error {
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO audio_session (id, user_id, source, filename, storage_path, duration_ms, mime, status)
VALUES (:id, :user_id, :source, :filename, :storage_path, :duration_ms, :mime, :status)`, s)
	return err
}

func (r *SessionRepo) Get(ctx context.Context, id ids.ID) (*AudioSession, error) {
	var s AudioSession
	err := r.DB.GetContext(ctx, &s, `SELECT * FROM audio_session WHERE id = ?`, id.Int64())
	return &s, err
}

func (r *SessionRepo) List(ctx context.Context, limit, offset int) ([]AudioSession, error) {
	var list []AudioSession
	err := r.DB.SelectContext(ctx, &list,
		`SELECT * FROM audio_session ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	return list, err
}

func (r *SessionRepo) UpdateStatus(ctx context.Context, id ids.ID, status string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE audio_session SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *SessionRepo) SetJobID(ctx context.Context, id ids.ID, jobID ids.ID) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE audio_session SET job_id = ? WHERE id = ?`, jobID.Int64(), id.Int64())
	return err
}
