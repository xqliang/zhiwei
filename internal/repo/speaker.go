package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Speaker 说话人声纹名册。声纹向量实际存 FAISS（Python sidecar），
// 这里的 embedding LONGBLOB 只作灾备/可重建索引备份，与 memory.embedding 模式一致，
// 不通过 JSON 外泄（json:"-"）。
type Speaker struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	Name        string    `db:"name" json:"name"`
	Source      string    `db:"source" json:"source"` // enrolled=用户登记 | auto=自动聚类
	Status      string    `db:"status" json:"status"` // active | dismissed
	Embedding   []byte    `db:"embedding" json:"-"`   // 256×float32=1024B 声纹备份，不外泄
	SampleCount int       `db:"sample_count" json:"sample_count"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

type SpeakerRepo struct{ DB *sqlx.DB }

// Create 生成雪花 ID 并插入。UserID/Source/Status 留零值时兜底默认
// （单用户 MVP user_id 固定 1，与其他 repo 一致）。回填 s.ID 供调用方使用。
func (r *SpeakerRepo) Create(ctx context.Context, s *Speaker) error {
	s.ID = ids.New()
	if s.UserID == 0 {
		s.UserID = 1
	}
	if s.Source == "" {
		s.Source = "auto"
	}
	if s.Status == "" {
		s.Status = "active"
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO speaker (id, user_id, name, source, status, embedding, sample_count)
VALUES (:id, :user_id, :name, :source, :status, :embedding, :sample_count)`, s)
	return err
}

// Get 按 ID 读取单行；不存在返回 sql.ErrNoRows（sqlx GetContext 语义）。
func (r *SpeakerRepo) Get(ctx context.Context, id ids.ID) (*Speaker, error) {
	var s Speaker
	err := r.DB.GetContext(ctx, &s, `SELECT * FROM speaker WHERE id = ?`, id.Int64())
	return &s, err
}

// List 返回全部 active 说话人（名册页用），按 ID 倒序（近建在前）。
func (r *SpeakerRepo) List(ctx context.Context) ([]Speaker, error) {
	var list []Speaker
	err := r.DB.SelectContext(ctx, &list,
		`SELECT * FROM speaker WHERE status = 'active' ORDER BY id DESC`)
	return list, err
}

// UpdateName 改说话人名（用户认领/重命名）。单行按 ID 原子写，天然并发安全。
func (r *SpeakerRepo) UpdateName(ctx context.Context, id ids.ID, name string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE speaker SET name = ? WHERE id = ?`, name, id.Int64())
	return err
}

// UpdateEmbedding 覆盖声纹灾备 BLOB（样本增量后重建向量时回写）。单行按 ID 原子写。
func (r *SpeakerRepo) UpdateEmbedding(ctx context.Context, id ids.ID, emb []byte) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE speaker SET embedding = ? WHERE id = ?`, emb, id.Int64())
	return err
}

// Delete 硬删除说话人。不存在也不报错（幂等）。
func (r *SpeakerRepo) Delete(ctx context.Context, id ids.ID) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM speaker WHERE id = ?`, id.Int64())
	return err
}

// MergeInto 把若干源说话人并入目标：先把各源关联的转写段 speaker_id 改指向目标
// （跨所有 session，无 transcript 作用域——说话人级合并），再删源说话人行。
// 源==目标的项跳过。单事务保证「段改指 + 删源」原子，中途失败不留半合并。
// 用于声纹页「手动合并」纠正 ASR 把同人拆成多个说话人。返回改指的段数。
func (r *SpeakerRepo) MergeInto(ctx context.Context, targetID ids.ID, sourceIDs []ids.ID) (int, error) {
	if len(sourceIDs) == 0 {
		return 0, nil
	}
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // Commit 成功后为 no-op；失败回滚不留半合并
	merged := 0
	for _, src := range sourceIDs {
		if src == targetID {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE transcript_segment SET speaker_id = ? WHERE speaker_id = ?`,
			targetID.Int64(), src.Int64())
		if err != nil {
			return merged, err
		}
		if n, err := res.RowsAffected(); err == nil {
			merged += int(n)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM speaker WHERE id = ?`, src.Int64()); err != nil {
			return merged, err
		}
	}
	if err := tx.Commit(); err != nil {
		return merged, err
	}
	return merged, nil
}
