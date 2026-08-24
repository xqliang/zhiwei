package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// SpeakerNameCandidate 说话人名字候选：speakername stage 用 LLM 从对话上下文
// 推断的称呼建议（名称+置信度+证据）。仅作建议不改 speaker.name；
// 用户采纳（改名）后 DeleteBySpeaker 清空整组。
type SpeakerNameCandidate struct {
	ID              ids.ID    `db:"id" json:"id"`
	SpeakerID       ids.ID    `db:"speaker_id" json:"speaker_id"`
	Name            string    `db:"name" json:"name"`
	Confidence      float64   `db:"confidence" json:"confidence"`
	Evidence        string    `db:"evidence" json:"evidence"`
	SourceSessionID *ids.ID   `db:"source_session_id" json:"source_session_id,omitempty"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
}

type SpeakerNameCandidateRepo struct{ DB *sqlx.DB }

// Upsert 插入或更新候选（跨 session 累积）：命中 (speaker_id,name) 唯一键时
// 置信度取两者最高（多段录音复现=更强信号，不因低质量录音被拉低）、
// 证据与来源会话取最新。ON DUPLICATE KEY 单行原子写，并发安全。
// sourceSessionID 传 0 时存 NULL。
//
// 并发语义：confidence 用 GREATEST 聚合（与写入先后无关，谁高留谁），而
// evidence/source_session_id 是 last-writer-wins（谁最后写就覆盖成谁）。
// 因此并发下最终保留的最高置信度，可能与最终留下的证据/来源会话不来自同一次
// 写入（例如高置信那次的证据被随后一次低置信写入覆盖）。候选仅作建议展示，
// 此错配可接受——无需加锁或事务串行化。
func (r *SpeakerNameCandidateRepo) Upsert(ctx context.Context, speakerID ids.ID, name string, confidence float64, evidence string, sourceSessionID ids.ID) error {
	var src interface{}
	if sourceSessionID != 0 {
		src = sourceSessionID.Int64()
	}
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO speaker_name_candidate (id, speaker_id, name, confidence, evidence, source_session_id)
VALUES (?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  confidence = GREATEST(confidence, VALUES(confidence)),
  evidence = VALUES(evidence),
  source_session_id = VALUES(source_session_id)`,
		ids.New().Int64(), speakerID.Int64(), name, confidence, evidence, src)
	return err
}

// ListBySpeakers 批量取若干说话人的全部候选，按置信度倒序（次键 id 正序保稳定）。
// 说话人面板/名册富化用：一次查询避免逐 speaker N+1。speakerIDs 为空返回空。
func (r *SpeakerNameCandidateRepo) ListBySpeakers(ctx context.Context, speakerIDs []ids.ID) ([]SpeakerNameCandidate, error) {
	if len(speakerIDs) == 0 {
		return nil, nil
	}
	int64s := make([]int64, len(speakerIDs))
	for i, id := range speakerIDs {
		int64s[i] = id.Int64()
	}
	q, args, err := sqlx.In(`
SELECT id, speaker_id, name, confidence, COALESCE(evidence, '') AS evidence,
       source_session_id, created_at, updated_at
FROM speaker_name_candidate
WHERE speaker_id IN (?)
ORDER BY confidence DESC, id ASC`, int64s)
	if err != nil {
		return nil, err
	}
	var list []SpeakerNameCandidate
	err = r.DB.SelectContext(ctx, &list, q, args...)
	return list, err
}

// DeleteBySpeaker 清空某说话人全部候选（用户采纳候选改名后调用：
// 名字已确认、不再是随机名，后续也不再重跑推断）。
func (r *SpeakerNameCandidateRepo) DeleteBySpeaker(ctx context.Context, speakerID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM speaker_name_candidate WHERE speaker_id = ?`, speakerID.Int64())
	return err
}

// DeleteOne 删除单条候选（前端「忽略」按钮）。幂等：不存在也不报错。
func (r *SpeakerNameCandidateRepo) DeleteOne(ctx context.Context, speakerID ids.ID, name string) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM speaker_name_candidate WHERE speaker_id = ? AND name = ?`,
		speakerID.Int64(), name)
	return err
}
