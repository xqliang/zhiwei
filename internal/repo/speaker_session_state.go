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

// LabelDurations 统计每个 ASR 说话人标签在本 session 的发言总时长（ms）。
// 供 DedupStatesBySpeaker 判定「同人多个标签中谁代表这个人的整体情绪」——
// 一个人主要发言部分的情绪才是他的整体情绪，碎片标签（几秒的过度切分残片）读数应让位。
func LabelDurations(segs []TranscriptSegment) map[string]int64 {
	m := map[string]int64{}
	for _, s := range segs {
		m[s.SpeakerLabel] += s.EndMS - s.StartMS
	}
	return m
}

// DedupStatesBySpeaker 同一人多标签的情绪行去重（2026-08-31）。
// 背景：speaker_session_state 按 **ASR 标签** 存行（audio-insight 模型按 diarization 标签
// 出每人情绪）；声纹「碎片在场归并」上线后，同一真人的多个标签会解析到同一 speaker_id
// ——展示（详情页情绪药丸）与汇总（emotionprofile 人物情绪指标）都应按「人」呈现，
// 否则一个人显示 N 个药丸、一次录音写 N 个情绪测点。
// 规则：speaker_id 非空且相同的行里，保留「标签发言总时长最大」的一行（时长并列取先出现
// ——入参须按稳定顺序，ListBySession 按 id ASC / 落库按标签序）；speaker_id 为空（未解析，
// 无法判定是否同人）的行原样保留。纯函数，可单测。
func DedupStatesBySpeaker(states []SpeakerSessionState, durByLabel map[string]int64) []SpeakerSessionState {
	if len(states) < 2 {
		return states
	}
	keep := make([]bool, len(states))
	best := map[ids.ID]int{} // speaker_id → 当前保留行的下标
	for i := range states {
		keep[i] = true
		if states[i].SpeakerID == nil {
			continue // 未解析：不参与去重
		}
		sid := *states[i].SpeakerID
		if j, ok := best[sid]; !ok {
			best[sid] = i
		} else if durByLabel[states[i].SpeakerLabel] > durByLabel[states[j].SpeakerLabel] {
			// 后来者标签时长更大：替换保留行（同人的整体情绪以主要发言部分为准）
			keep[j] = false
			best[sid] = i
		} else {
			keep[i] = false
		}
	}
	out := make([]SpeakerSessionState, 0, len(states))
	for i := range states {
		if keep[i] {
			out = append(out, states[i])
		}
	}
	return out
}
