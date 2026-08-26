package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// SpeakerEmbedding 一条声纹样本（2026-08-26 多条声纹需求）：一个人可有多条，
// 各自带备注/创建时间；speaker.embedding 是全部样本的聚合代表（FAISS 1:N 用），
// 本表是样本明细 + 聚合重算的数据源。
type SpeakerEmbedding struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	SpeakerID   ids.ID    `db:"speaker_id" json:"speaker_id"`
	Note        *string   `db:"note" json:"note,omitempty"`       // 备注（可空）：如「安静房间录」
	Embedding   []byte    `db:"embedding" json:"-"`               // 256×float32=1024B，不外泄
	SampleCount int       `db:"sample_count" json:"sample_count"` // 该条聚合的段向量数（手动录=1）
	Source      string    `db:"source" json:"source"`             // manual=手动录 | auto=抽取自动登记 | merge=合并迁入
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

type SpeakerEmbeddingRepo struct{ DB *sqlx.DB }

// Create 生成雪花 ID 并插入。UserID/Source 留零值兜底（同 SpeakerRepo.Create）。
func (r *SpeakerEmbeddingRepo) Create(ctx context.Context, e *SpeakerEmbedding) error {
	e.ID = ids.New()
	if e.UserID == 0 {
		e.UserID = 1
	}
	if e.Source == "" {
		e.Source = "manual"
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO speaker_embedding (id, user_id, speaker_id, note, embedding, sample_count, source)
VALUES (:id, :user_id, :speaker_id, :note, :embedding, :sample_count, :source)`, e)
	return err
}

// ListBySpeaker 某说话人的全部样本，新录在前（id 倒序≈创建时间倒序）。
func (r *SpeakerEmbeddingRepo) ListBySpeaker(ctx context.Context, speakerID ids.ID) ([]SpeakerEmbedding, error) {
	var list []SpeakerEmbedding
	err := r.DB.SelectContext(ctx, &list,
		`SELECT * FROM speaker_embedding WHERE speaker_id = ? ORDER BY id DESC`, speakerID.Int64())
	return list, err
}

// ListBySpeakers 批量取多说话人的样本（声纹名册一次富化，避免 N+1），按说话人分组返回。
func (r *SpeakerEmbeddingRepo) ListBySpeakers(ctx context.Context, speakerIDs []ids.ID) (map[ids.ID][]SpeakerEmbedding, error) {
	out := map[ids.ID][]SpeakerEmbedding{}
	if len(speakerIDs) == 0 {
		return out, nil
	}
	ph := make([]string, len(speakerIDs))
	args := make([]any, len(speakerIDs))
	for i, sid := range speakerIDs {
		ph[i] = "?"
		args[i] = sid.Int64()
	}
	var rows []SpeakerEmbedding
	q := `SELECT * FROM speaker_embedding WHERE speaker_id IN (` +
		joinPlaceholders(ph) + `) ORDER BY id DESC`
	if err := r.DB.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, err
	}
	for _, e := range rows {
		out[e.SpeakerID] = append(out[e.SpeakerID], e)
	}
	return out, nil
}

// UpdateNote 改某条样本的备注。note 传 nil = 清空。单行按 ID 原子写。
func (r *SpeakerEmbeddingRepo) UpdateNote(ctx context.Context, id ids.ID, note *string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE speaker_embedding SET note = ? WHERE id = ?`, note, id.Int64())
	return err
}

// Get 按 ID 读单条；不存在返回 (nil, nil)（与其他 repo 风格一致，调用方判 nil）。
func (r *SpeakerEmbeddingRepo) Get(ctx context.Context, id ids.ID) (*SpeakerEmbedding, error) {
	var e SpeakerEmbedding
	err := r.DB.GetContext(ctx, &e, `SELECT * FROM speaker_embedding WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Delete 删单条样本（幂等）。说话人的聚合代表由调用方随后重算。
func (r *SpeakerEmbeddingRepo) Delete(ctx context.Context, id ids.ID) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM speaker_embedding WHERE id = ?`, id.Int64())
	return err
}

// DeleteBySpeaker 删某说话人的全部样本（说话人删除时清孤儿，幂等）。
func (r *SpeakerEmbeddingRepo) DeleteBySpeaker(ctx context.Context, speakerID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM speaker_embedding WHERE speaker_id = ?`, speakerID.Int64())
	return err
}

// MigrateToSpeakerExt 合并说话人用：把若干源的样本行改挂到目标（source 标 merge），
// 迁移而非删除——「合并 = 声纹累加」：目标的聚合代表重算后会包含这些样本。
// 在 MergeInto 同一事务外独立执行也可（行本身无跨表约束），传 ext 以支持并入事务。
func (r *SpeakerEmbeddingRepo) MigrateToSpeakerExt(ctx context.Context, ext ExecerContext, targetID ids.ID, sourceIDs []ids.ID) (int64, error) {
	if len(sourceIDs) == 0 {
		return 0, nil
	}
	ph := make([]string, len(sourceIDs))
	args := make([]any, 0, len(sourceIDs)+1)
	args = append(args, targetID.Int64())
	for i, sid := range sourceIDs {
		ph[i] = "?"
		args = append(args, sid.Int64())
	}
	res, err := ext.ExecContext(ctx,
		`UPDATE speaker_embedding SET speaker_id = ?, source = 'merge' WHERE speaker_id IN (`+
			joinPlaceholders(ph) + `)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EnsureSpeakerEmbeddingBootstrap 启动幂等回填（对齐 EnsurePersonBootstrap 模式）：
// 把既有 speaker.embedding（单向量时代）物化成首条样本行，source 沿用说话人来源
// （enrolled→manual、auto→auto）、sample_count 沿用。
// 只补「有向量但无样本行」的说话人——已迁移过的不会重复插（幂等），新逻辑写的样本行
// 天然跳过。返回回填条数（仅供日志）。
func (r *SpeakerEmbeddingRepo) EnsureSpeakerEmbeddingBootstrap(ctx context.Context) (int, error) {
	// 有 embedding 但无样本行的说话人（LEFT JOIN … IS NULL）
	var rows []struct {
		ID          ids.ID    `db:"id"`
		UserID      int64     `db:"user_id"`
		Source      string    `db:"source"`
		Embedding   []byte    `db:"embedding"`
		SampleCount int       `db:"sample_count"`
		CreatedAt   time.Time `db:"created_at"`
	}
	err := r.DB.SelectContext(ctx, &rows, `
SELECT s.id, s.user_id, s.source, s.embedding, s.sample_count, s.created_at
FROM speaker s
LEFT JOIN speaker_embedding e ON e.speaker_id = s.id
WHERE s.embedding IS NOT NULL AND e.id IS NULL`)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		src := "manual"
		if row.Source == "auto" {
			src = "auto"
		}
		if row.SampleCount == 0 {
			row.SampleCount = 1 // 旧数据兜底：至少代表 1 个样本
		}
		e := &SpeakerEmbedding{
			UserID: row.UserID, SpeakerID: row.ID, Note: nil,
			Embedding: row.Embedding, SampleCount: row.SampleCount,
			Source: src, CreatedAt: row.CreatedAt,
		}
		// Create 生成新雪花 id；created_at 走 DB 默认值（≈启动时刻，非原说话人创建时间——
		// INSERT 列表不含 created_at，保留「样本行创建时刻」语义即可，备注为回填来源可辨）。
		if err := r.Create(ctx, e); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// joinPlaceholders ["?","?"] → "?,?"（小工具，避免逐处 strings.Join 引入）。
func joinPlaceholders(ph []string) string {
	out := ""
	for i, p := range ph {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
