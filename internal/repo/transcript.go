package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

type Transcript struct {
	ID         ids.ID    `db:"id" json:"id"`
	UserID     int64     `db:"user_id" json:"user_id"`
	SessionID  ids.ID    `db:"session_id" json:"session_id"`
	Language   string    `db:"language" json:"language"`
	FullText   *string   `db:"full_text" json:"full_text"`
	Confidence *float64  `db:"confidence" json:"confidence"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type TranscriptSegment struct {
	ID           ids.ID    `db:"id" json:"id"`
	TranscriptID ids.ID    `db:"transcript_id" json:"transcript_id"`
	SequenceNo   int       `db:"sequence_no" json:"sequence_no"`
	SpeakerLabel string    `db:"speaker_label" json:"speaker_label"`
	Text         string    `db:"text" json:"text"`
	StartMS      int64     `db:"start_ms" json:"start_ms"`
	EndMS        int64     `db:"end_ms" json:"end_ms"`
	Confidence   *float64  `db:"confidence" json:"confidence"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

type TranscriptRepo struct{ DB *sqlx.DB }

func (r *TranscriptRepo) Create(ctx context.Context, t *Transcript) error {
	t.ID = ids.New()
	if t.UserID == 0 {
		t.UserID = 1
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO transcript (id, user_id, session_id, language)
VALUES (:id, :user_id, :session_id, :language)`, t)
	return err
}

func (r *TranscriptRepo) InsertSegments(ctx context.Context, segs []TranscriptSegment) error {
	if len(segs) == 0 {
		return nil
	}
	for i := range segs {
		segs[i].ID = ids.New()
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO transcript_segment (id, transcript_id, sequence_no, speaker_label, text, start_ms, end_ms, confidence)
VALUES (:id, :transcript_id, :sequence_no, :speaker_label, :text, :start_ms, :end_ms, :confidence)`, segs)
	return err
}

func (r *TranscriptRepo) GetBySession(ctx context.Context, sessionID ids.ID) (*Transcript, error) {
	var t Transcript
	err := r.DB.GetContext(ctx, &t,
		`SELECT * FROM transcript WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sessionID.Int64())
	return &t, err
}

func (r *TranscriptRepo) ListSegments(ctx context.Context, transcriptID ids.ID) ([]TranscriptSegment, error) {
	var list []TranscriptSegment
	err := r.DB.SelectContext(ctx, &list,
		`SELECT * FROM transcript_segment WHERE transcript_id = ? ORDER BY sequence_no`, transcriptID.Int64())
	return list, err
}

func (r *TranscriptRepo) SetFullText(ctx context.Context, id ids.ID, full string, conf float64) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript SET full_text = ?, confidence = ? WHERE id = ?`, full, conf, id.Int64())
	return err
}

// UpdateSegmentText 更新单段转写文本（ASR 就地编辑用）。
// 带 transcript_id 作用域，防止前端误传其他会话的 segment id 造成跨会话写入；
// 单行 UPDATE 原子，满足并发安全约束。rows=0（id 不属于该 transcript）静默忽略。
func (r *TranscriptRepo) UpdateSegmentText(ctx context.Context, transcriptID, segID ids.ID, text string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET text = ? WHERE id = ? AND transcript_id = ?`,
		text, segID.Int64(), transcriptID.Int64())
	return err
}

// RecomputeFullText 重新汇总 segments 为 transcript.full_text + 平均置信度。
// 算法与 pipeline.stageSegment（internal/pipeline/stage_asr.go）完全一致：
// 非空文本段拼成 "[说话人] 文本" 逐行换行、confidence 取非空段均值（无则 0）。
// ASR 编辑后调用，保证 full_text 与 list 接口的 asr_preview（GROUP_CONCAT(seg.text)）同步。
// 逻辑在此内联而非 import pipeline，避免 repo→pipeline 反向依赖。
func (r *TranscriptRepo) RecomputeFullText(ctx context.Context, transcriptID ids.ID) error {
	segs, err := r.ListSegments(ctx, transcriptID)
	if err != nil {
		return err
	}
	var sb strings.Builder
	var sumConf, n float64
	for _, s := range segs {
		if s.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		if s.SpeakerLabel == "" {
			fmt.Fprintf(&sb, "[未知] %s", s.Text)
		} else {
			fmt.Fprintf(&sb, "[%s] %s", s.SpeakerLabel, s.Text)
		}
		if s.Confidence != nil {
			sumConf += *s.Confidence
			n++
		}
	}
	conf := 0.0
	if n > 0 {
		conf = sumConf / n
	}
	return r.SetFullText(ctx, transcriptID, sb.String(), conf)
}
