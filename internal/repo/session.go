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
	if s.UserID == 0 {
		s.UserID = 1
	}
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

// Delete 硬删除 session + 全部派生数据（单事务级联）。音频文件由 handler 库外删（best-effort）。
// 顺序：关联子表先于主表（子查询依赖主表行仍存在）；各步 target 表 ≠ 子查询 source 表，
// 无 MySQL「不能在子查询里更新目标表」之限。jobID 非空则一并删 pipeline_job。
// 注意 job 表实际名为 pipeline_job（见 migrations/000001）。
func (r *SessionRepo) Delete(ctx context.Context, id ids.ID, jobID *ids.ID) error {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	steps := []string{
		`DELETE FROM memory_topic WHERE memory_id IN (SELECT id FROM memory WHERE session_id = ?)`,
		`DELETE FROM todo_topic WHERE todo_id IN (SELECT id FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?))`,
		`DELETE FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?)`,
		`DELETE FROM memory WHERE session_id = ?`,
		`DELETE FROM transcript_segment WHERE transcript_id IN (SELECT id FROM transcript WHERE session_id = ?)`,
		`DELETE FROM transcript WHERE session_id = ?`,
		`DELETE FROM audio_session WHERE id = ?`,
	}
	for _, q := range steps {
		if _, err := tx.ExecContext(ctx, q, id.Int64()); err != nil {
			return err
		}
	}
	if jobID != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pipeline_job WHERE id = ?`, jobID.Int64()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
