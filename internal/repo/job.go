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

// TraceEntry 记录一个 stage 的执行信息，对应 spec 的可观测性要求。
type TraceEntry struct {
	Stage string    `json:"stage"`
	Model string    `json:"model,omitempty"`
	MS    int64     `json:"ms"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`

	// spec §3.3/§3.5：trace 需记录 prompt 版本、token 用量、窗口数。
	PromptVersion string `json:"prompt_version,omitempty"` // prompt 版本（文件名，如 extraction_v1）
	Tokens        int    `json:"tokens,omitempty"`         // LLM token 用量（本阶段累计）
	Windows       int    `json:"windows,omitempty"`        // 抽取窗口数（LLM 调用次数）
}

type Job struct {
	ID        ids.ID           `db:"id" json:"id"`
	UserID    int64            `db:"user_id" json:"user_id"`
	SessionID ids.ID           `db:"session_id" json:"session_id"`
	Stage     string           `db:"stage" json:"stage"`
	Status    string           `db:"status" json:"status"`
	Attempt   int              `db:"attempt" json:"attempt"`
	LastError *string          `db:"last_error" json:"last_error,omitempty"`
	Trace     *json.RawMessage `db:"trace" json:"trace,omitempty"`
	CreatedAt time.Time        `db:"created_at" json:"created_at"`
	UpdatedAt time.Time        `db:"updated_at" json:"updated_at"`
}

type JobRepo struct{ DB *sqlx.DB }

func (r *JobRepo) Create(ctx context.Context, j *Job) error {
	j.ID = ids.New()
	if j.UserID == 0 {
		j.UserID = 1
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO pipeline_job (id, user_id, session_id, stage, status)
VALUES (:id, :user_id, :session_id, :stage, :status)`, j)
	return err
}

func (r *JobRepo) Get(ctx context.Context, id ids.ID) (*Job, error) {
	var j Job
	err := r.DB.GetContext(ctx, &j, `SELECT * FROM pipeline_job WHERE id = ?`, id.Int64())
	return &j, err
}

// ClaimNext 原子领取最老的 pending 任务并置为 running。
// 单进程内多 worker 竞争由 UPDATE 条件更新保证不重复领取。
func (r *JobRepo) ClaimNext(ctx context.Context) (*Job, bool, error) {
	var id int64
	err := r.DB.GetContext(ctx, &id, `
SELECT id FROM (
  SELECT id FROM pipeline_job WHERE status = 'pending' ORDER BY id LIMIT 1
) t`)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	res, err := r.DB.ExecContext(ctx,
		`UPDATE pipeline_job SET status = 'running' WHERE id = ? AND status = 'pending'`, id)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, false, nil // 被其他 worker 抢走
	}
	j, err := r.Get(ctx, ids.ID(id))
	return j, true, err
}

func (r *JobRepo) Save(ctx context.Context, j *Job) error {
	trace := []byte("[]")
	if j.Trace != nil && len(*j.Trace) > 0 {
		trace = *j.Trace
	}
	_, err := r.DB.ExecContext(ctx, `
UPDATE pipeline_job SET stage = ?, status = ?, attempt = ?, last_error = ?, trace = ?
WHERE id = ?`,
		j.Stage, j.Status, j.Attempt, j.LastError, trace, j.ID.Int64())
	return err
}

// ResetRunning 把所有 running 任务重置为 pending（服务重启时调用，任务不丢）。
func (r *JobRepo) ResetRunning(ctx context.Context) (int64, error) {
	res, err := r.DB.ExecContext(ctx, `UPDATE pipeline_job SET status = 'pending' WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
