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
	ID           ids.ID `db:"id" json:"id"`
	TranscriptID ids.ID `db:"transcript_id" json:"transcript_id"`
	SequenceNo   int    `db:"sequence_no" json:"sequence_no"`
	SpeakerLabel string `db:"speaker_label" json:"speaker_label"`
	// SpeakerID 解析到的已登记说话人（speaker stage 回填，此前为 NULL）。
	// 000004 迁移给 transcript_segment 加了 speaker_id 列，NewDB 走 sqlx safe 模式
	// （无对应字段的列会扫描报错），故此处同步加字段，保 SELECT * 可扫描。
	SpeakerID *ids.ID `db:"speaker_id" json:"speaker_id,omitempty"`
	// CorrectedFromSpeakerID 幽灵历史声纹纠正（000017 迁移加列）：非 NULL = 该段被 speaker stage
	// 的纠正 pass 自动改判过，值为被顶掉的原历史说话人 id（前端"已修改"徽章 + 审计 + 手动改回依据）。
	// 手动换人 / 整人改判 / 重新识别时清 NULL。存量 / 未纠正段为 NULL。
	CorrectedFromSpeakerID *ids.ID `db:"corrected_from_speaker_id" json:"corrected_from_speaker_id,omitempty"`
	// CorrectedReason 自动纠正原因（000021 迁移加列）：'phantom'=幽灵历史声纹改判（配 CorrectedFromSpeakerID）；
	// 'short'=过短噪声段并入最近在场说话人（CorrectedFromSpeakerID 为 NULL）。nil=未纠正。
	// 与 speaker_id 一同被手动换人/整人改判/重新识别清空。
	CorrectedReason *string `db:"corrected_reason" json:"corrected_reason,omitempty"`
	// Embedding 该段的 256 维声纹向量 BLOB（000007 迁移加列；speaker stage 逐段
	// 提取后落库，供详情页按段展示与声纹库的相似度 top-N）。json:"-" 不外泄，
	// API 层按需转成 top-N 明文列表。存量会话（新列前处理）为 NULL。
	Embedding  []byte    `db:"embedding" json:"-"`
	Text       string    `db:"text" json:"text"`
	StartMS    int64     `db:"start_ms" json:"start_ms"`
	EndMS      int64     `db:"end_ms" json:"end_ms"`
	Confidence *float64  `db:"confidence" json:"confidence"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
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

// GetSegment 按 id 读单段转写段（从转写段音频录入声纹时取 start/end_ms 用）。
// 不存在返回 sql.ErrNoRows。不带 transcript 作用域——调用方自行校验归属。
func (r *TranscriptRepo) GetSegment(ctx context.Context, segID ids.ID) (*TranscriptSegment, error) {
	var s TranscriptSegment
	err := r.DB.GetContext(ctx, &s, `SELECT * FROM transcript_segment WHERE id = ?`, segID.Int64())
	return &s, err
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

// MergeSegments 把若干连续段合并成一条：保留 keeper，其 text=各段按序拼接、
// start_ms=min、end_ms=max、speaker_id=target；其余段删除。单事务原子（中途失败不留半合并）。
// 用于 timeline「合并连续同人段成一条」（纠正 ASR 把同人连续发言拆成多段）。调用方负责
// 已按 sequence_no 排好序 + 算好拼接文本/时间，并保证 keeper ∈ 待合并段集合。
func (r *TranscriptRepo) MergeSegments(ctx context.Context, transcriptID, keeperID ids.ID, otherIDs []ids.ID, text string, startMS, endMS int64, speakerID ids.ID) error {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // Commit 成功后为 no-op；失败回滚
	if _, err := tx.ExecContext(ctx,
		`UPDATE transcript_segment SET text = ?, start_ms = ?, end_ms = ?, speaker_id = ? WHERE id = ? AND transcript_id = ?`,
		text, startMS, endMS, speakerID.Int64(), keeperID.Int64(), transcriptID.Int64()); err != nil {
		return err
	}
	if len(otherIDs) > 0 {
		ids := make([]int64, len(otherIDs))
		for i, id := range otherIDs {
			ids[i] = id.Int64()
		}
		q, args, err := sqlx.In(`DELETE FROM transcript_segment WHERE id IN (?) AND transcript_id = ?`, ids, transcriptID.Int64())
		if err != nil {
			return err
		}
		// MySQL 占位符即 ?，sqlx.In 已展开为 IN (?,?,...)，无需 Rebind
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetSegmentSpeaker 按 speaker_label 批量回填本 transcript 内段的 speaker_id。
// 带 transcript_id 作用域防跨会话；单条 UPDATE 原子写，并发安全。rows=0 静默。
// 仅回填 speaker_id IS NULL 的段，保留用户已手动换人（SetSegmentSpeakerByID）的纠正，不被重跑覆盖。
func (r *TranscriptRepo) SetSegmentSpeaker(ctx context.Context, transcriptID ids.ID, speakerLabel string, speakerID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ? WHERE transcript_id = ? AND speaker_label = ? AND speaker_id IS NULL`,
		speakerID.Int64(), transcriptID.Int64(), speakerLabel)
	return err
}

// ClearSegmentSpeakers 清空本 transcript 所有段的 speaker_id（置 NULL）。
// 用于 timeline「重新识别」：清空后 speaker stage 不再幂等跳过，会重新提向+按最新声纹库 1:N 匹配。
// 注意：会覆盖手动纠正的换人（调用方需在 UI 二次确认）。
func (r *TranscriptRepo) ClearSegmentSpeakers(ctx context.Context, transcriptID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = NULL, corrected_from_speaker_id = NULL, corrected_reason = NULL WHERE transcript_id = ?`, transcriptID.Int64())
	return err
}

// SetSegmentSpeakerByID 单段换人（前端"换人"下拉用）。带 transcript_id 作用域防跨会话误写。
func (r *TranscriptRepo) SetSegmentSpeakerByID(ctx context.Context, transcriptID, segID, speakerID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = NULL, corrected_reason = NULL WHERE id = ? AND transcript_id = ?`,
		speakerID.Int64(), segID.Int64(), transcriptID.Int64())
	return err
}

// CorrectSegmentSpeaker 幽灵历史声纹纠正：把本 transcript 内某 speaker_label 的全部段
// 从原历史说话人 fromID 改判给 toID，并记录 corrected_from_speaker_id=fromID（前端"已修改"
// 徽章 + 审计）。纠正 pass 以「组=speaker_label」为单位，故按 label 定位；带 transcript_id
// 作用域防跨会话误写；WHERE 再限定 speaker_id=fromID，只改「当前确实归原说话人」的段
// （与 SetSegmentSpeaker 只碰 NULL 段同一纪律：不越界踩用户手动改过的段）；
// 单条 UPDATE 原子写、并发安全。
func (r *TranscriptRepo) CorrectSegmentSpeaker(ctx context.Context, transcriptID ids.ID, speakerLabel string, fromID, toID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = ?, corrected_reason = 'phantom'
		 WHERE transcript_id = ? AND speaker_label = ? AND speaker_id = ?`,
		toID.Int64(), fromID.Int64(), transcriptID.Int64(), speakerLabel, fromID.Int64())
	return err
}

// MergeShortGroup 过短噪声段并入（2026-08-28 需求）：把本 transcript 内某 speaker_label 下
// **尚未回填**（speaker_id IS NULL）的段整组并入目标在场说话人 toID，并标记 corrected_reason='short'
// （无原判定说话人，corrected_from_speaker_id 显式置 NULL）。这类组因总时长<3s 未登记独立声纹，
// 其段在 speaker stage pass2 被留 NULL、pass3 并入。带 transcript_id 作用域；单条 UPDATE 原子写。
func (r *TranscriptRepo) MergeShortGroup(ctx context.Context, transcriptID ids.ID, speakerLabel string, toID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_reason = 'short', corrected_from_speaker_id = NULL
		 WHERE transcript_id = ? AND speaker_label = ? AND speaker_id IS NULL`,
		toID.Int64(), transcriptID.Int64(), speakerLabel)
	return err
}

// SaveSegmentEmbeddings 批量落库逐段声纹向量 BLOB（speaker stage 提取后调用）。
// 带 transcript_id 作用域防跨会话误写；逐行 UPDATE 原子写（段数=会话内句数，量小）。
// 用于详情页按段展示与声纹库的相似度 top-N（一句话可能混多人，段级才能审计切分）。
func (r *TranscriptRepo) SaveSegmentEmbeddings(ctx context.Context, transcriptID ids.ID, blobs map[ids.ID][]byte) error {
	for segID, blob := range blobs {
		if _, err := r.DB.ExecContext(ctx,
			`UPDATE transcript_segment SET embedding = ? WHERE id = ? AND transcript_id = ?`,
			blob, segID.Int64(), transcriptID.Int64()); err != nil {
			return err
		}
	}
	return nil
}

// ReassignSpeakerSegments 把本 transcript 内某说话人的全部段一键改判给目标说话人
// （timeline 说话人 chip「切换声纹」：纠正声纹/识别错误，逐段下拉太繁琐）。
// 带 transcript_id 作用域防跨会话波及；单条 UPDATE 原子写，并发安全。
// 只改段归属，不动说话人名册/声纹（错误登记的说话人可另行删除或合并）。
// 返回受影响段数（0 = 本会话没有该说话人的段）。
func (r *TranscriptRepo) ReassignSpeakerSegments(ctx context.Context, transcriptID, fromID, toID ids.ID) (int, error) {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = NULL, corrected_reason = NULL WHERE transcript_id = ? AND speaker_id = ?`,
		toID.Int64(), transcriptID.Int64(), fromID.Int64())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // 理论不可达：拿不到行数按 0 处理，不影响改判本身已生效
	}
	return int(n), nil
}

// ReassignSpeakerInTranscript 把本 transcript 内所有 speaker_id = fromID 的段改判为 toID，返回改动行数。
// 带 transcript_id 作用域，只影响本会话——同一 speaker 在其他会话的段不动。单条 UPDATE 原子写、并发安全。
// 用于 timeline「用此段录音纹」：录入新说话人后，把该说话人在本会话的全部段一并改判到新说话人。
func (r *TranscriptRepo) ReassignSpeakerInTranscript(ctx context.Context, transcriptID, fromID, toID ids.ID) (int, error) {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = NULL, corrected_reason = NULL WHERE transcript_id = ? AND speaker_id = ?`,
		toID.Int64(), transcriptID.Int64(), fromID.Int64())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListSpeakersForTranscript 本 transcript 解析到的说话人聚合（说话人面板用）。
// 按 sequence_no 升序的首段定序，保证面板说话人顺序与转写一致。
func (r *TranscriptRepo) ListSpeakersForTranscript(ctx context.Context, transcriptID ids.ID) ([]SpeakerInSegment, error) {
	var list []SpeakerInSegment
	err := r.DB.SelectContext(ctx, &list, `
SELECT s.id AS speaker_id, s.name, s.source, COUNT(seg.id) AS segment_count
FROM transcript_segment seg
JOIN speaker s ON s.id = seg.speaker_id
WHERE seg.transcript_id = ? AND seg.speaker_id IS NOT NULL
GROUP BY s.id, s.name, s.source
ORDER BY MIN(seg.sequence_no)`, transcriptID.Int64())
	return list, err
}

// SpeakerInSegment 面板用的说话人聚合视图（非表）。ColorIndex 由 API 层按序号填充。
type SpeakerInSegment struct {
	SpeakerID    ids.ID `db:"speaker_id" json:"speaker_id"`
	Name         string `db:"name" json:"name"`
	Source       string `db:"source" json:"source"`
	SegmentCount int    `db:"segment_count" json:"segment_count"`
	ColorIndex   int    `db:"-" json:"color_index"`
}

// ListSegmentsBySpeaker 跨 session 取该说话人出现的所有片段（声纹 tab「点开看关联录音」用）。
// JOIN transcript→audio_session 拿 session_id/filename/created_at（音频经
// GET /api/sessions/{session_id}/audio 播放，不外泄 storage_path）。
// 按录音时间倒序、段序升序，便于前端按录音分组展示。
func (r *TranscriptRepo) ListSegmentsBySpeaker(ctx context.Context, speakerID ids.ID) ([]SpeakerSegmentOccurrence, error) {
	var list []SpeakerSegmentOccurrence
	err := r.DB.SelectContext(ctx, &list, `
SELECT seg.id AS segment_id, tr.session_id, seg.text, seg.start_ms, seg.end_ms, seg.sequence_no,
       s.filename, s.created_at
FROM transcript_segment seg
JOIN transcript tr ON tr.id = seg.transcript_id
JOIN audio_session s ON s.id = tr.session_id
WHERE seg.speaker_id = ?
ORDER BY s.created_at DESC, seg.sequence_no`, speakerID.Int64())
	return list, err
}

// SpeakerSegmentOccurrence 一个说话人片段的跨 session 出现记录（声纹 tab 用）。
type SpeakerSegmentOccurrence struct {
	SegmentID  ids.ID    `db:"segment_id" json:"segment_id"`
	SessionID  ids.ID    `db:"session_id" json:"session_id"`
	Text       string    `db:"text" json:"text"`
	StartMS    int64     `db:"start_ms" json:"start_ms"`
	EndMS      int64     `db:"end_ms" json:"end_ms"`
	SequenceNo int       `db:"sequence_no" json:"sequence_no"`
	Filename   string    `db:"filename" json:"filename"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

// WallClockSegment 跨 session 墙钟时间窗口内的一条发言（speakername stage 上下文用）。
// WallTime = session.created_at + start_ms，由 SQL 计算返回。
// SpeakerName 经 LEFT JOIN speaker 取（已确认真名/随机名原样；NULL 段为 nil）。
type WallClockSegment struct {
	SegmentID   ids.ID    `db:"segment_id"`
	SessionID   ids.ID    `db:"session_id"`
	SpeakerID   *ids.ID   `db:"speaker_id"`
	SpeakerName *string   `db:"speaker_name"`
	Text        string    `db:"text"`
	StartMS     int64     `db:"start_ms"`
	EndMS       int64     `db:"end_ms"`
	WallTime    time.Time `db:"wall_time"`
}

// ListSegmentsInWallClockWindow 跨 session 取墙钟时间落在 [from,to] 的全部段，
// 按墙钟**正序**返回；limit 超限时保留**最近**的（靠近 to 的）——当前录音的段
// 是窗口内最新的，天然优先保留。user 维度过滤。
// 实现：SQL DESC + LIMIT 取最近 N，Go 侧反转回正序。
// 次级键 seg.id DESC：墙钟毫秒相同（跨 session 撞毫秒）时定序，保证排序稳定、
// LIMIT 截断到底保留哪几条可复现（雪花 id 单调递增，DESC 即“更晚生成的优先”）。
// speakername stage 用它拼「当前录音全文 + 前 N 分钟跨录音对话」上下文。
func (r *TranscriptRepo) ListSegmentsInWallClockWindow(ctx context.Context, userID int64, from, to time.Time, limit int) ([]WallClockSegment, error) {
	if limit <= 0 {
		limit = 400
	}
	var desc []WallClockSegment
	err := r.DB.SelectContext(ctx, &desc, `
SELECT seg.id AS segment_id, tr.session_id, seg.speaker_id, sp.name AS speaker_name,
       seg.text, seg.start_ms, seg.end_ms,
       (s.created_at + INTERVAL seg.start_ms * 1000 MICROSECOND) AS wall_time
FROM transcript_segment seg
JOIN transcript tr      ON tr.id = seg.transcript_id
JOIN audio_session s    ON s.id = tr.session_id
LEFT JOIN speaker sp    ON sp.id = seg.speaker_id
WHERE tr.user_id = ?
  AND (s.created_at + INTERVAL seg.start_ms * 1000 MICROSECOND) BETWEEN ? AND ?
ORDER BY wall_time DESC, seg.id DESC
LIMIT ?`, userID, from, to, limit)
	if err != nil {
		return nil, err
	}
	// DESC → 正序（原地反转，避免再分配）
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	return desc, nil
}
