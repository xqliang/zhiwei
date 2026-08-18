package repo

import (
	"context"
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
