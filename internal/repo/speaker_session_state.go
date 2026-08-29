package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// SpeakerSessionState 是每会话每说话人的整体情绪/精神状态（audioscene stage 落库；spec §2）。
type SpeakerSessionState struct {
	ID           ids.ID    `db:"id" json:"id"`
	UserID       int64     `db:"user_id" json:"user_id"`
	TranscriptID ids.ID    `db:"transcript_id" json:"transcript_id"`
	SessionID    ids.ID    `db:"session_id" json:"session_id"`
	SpeakerLabel string    `db:"speaker_label" json:"speaker_label"`
	SpeakerID    *ids.ID   `db:"speaker_id" json:"speaker_id,omitempty"`
	Emotion      string    `db:"emotion" json:"emotion"`
	MicroEmotion string    `db:"micro_emotion" json:"micro_emotion"`
	MentalState  string    `db:"mental_state" json:"mental_state"`
	Confidence   float64   `db:"confidence" json:"confidence"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

type SpeakerSessionStateRepo struct{ DB *sqlx.DB }

// InsertBatch 批量插入（生成 ID，UserID 默认 1）。空切片 no-op。
func (r *SpeakerSessionStateRepo) InsertBatch(ctx context.Context, rows []SpeakerSessionState) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		rows[i].ID = ids.New()
		if rows[i].UserID == 0 {
			rows[i].UserID = 1
		}
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO speaker_session_state
  (id, user_id, transcript_id, session_id, speaker_label, speaker_id, emotion, micro_emotion, mental_state, confidence)
VALUES
  (:id, :user_id, :transcript_id, :session_id, :speaker_label, :speaker_id, :emotion, :micro_emotion, :mental_state, :confidence)`, rows)
	return err
}

// ListBySession 返回某会话的说话人情绪（行级 user_id 过滤，IDOR）。
func (r *SpeakerSessionStateRepo) ListBySession(ctx context.Context, userID int64, sessionID ids.ID) ([]SpeakerSessionState, error) {
	var rows []SpeakerSessionState
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM speaker_session_state WHERE session_id=? AND user_id=? ORDER BY id ASC`, sessionID.Int64(), userID)
	return rows, err
}

// DeleteByTranscript 删除某 transcript 的全部说话人情绪行（audioscene stage 重跑前调用，
// 幂等——避免重新识别/重新跑 stage 时 InsertBatch 追加致重复）。带 user_id 过滤。
func (r *SpeakerSessionStateRepo) DeleteByTranscript(ctx context.Context, userID int64, transcriptID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM speaker_session_state WHERE transcript_id=? AND user_id=?`, transcriptID.Int64(), userID)
	return err
}
