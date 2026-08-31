// stage_emotionprofile 实现 emotionprofile stage（P2 人物情绪汇总，spec §5）。
// 确定性聚合（不调 LLM）：读本会话 speaker_session_state，把每个已识别说话人
// （speaker_id→person）的类别情绪映射 valence，写 PersonMetric(emotion, source=auto)。
// 跳过未识别（speaker_id 空/找不到 person）；幂等防 stage 重跑重复；全程降级不阻断。
package pipeline

import (
	"context"
	"log"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
)

func stageEmotionProfile(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
		if !d.EmotionProfileEnabled || d.PersonMetrics == nil || d.Persons == nil || d.SpeakerStates == nil {
			return nil // 开关关闭或缺依赖：no-op
		}
		s, err := d.Sessions.Get(ctx, 1, sessionID)
		if err != nil {
			log.Printf("[emotionprofile] 读 session 失败(降级): %v", err)
			return nil
		}
		states, err := d.SpeakerStates.ListBySession(ctx, 1, sessionID)
		if err != nil {
			log.Printf("[emotionprofile] 读说话人情绪失败(降级): %v", err)
			return nil
		}
		if len(states) == 0 {
			return nil
		}
		// 同人去重（2026-08-31）：情绪行按 ASR 标签存储，碎片在场归并后同一真人的多个标签
		// 解析到同一 speaker——不去重会给一个人一次录音写多个情绪测点（污染情绪平面时序）。
		// 保留发言时长最大标签的一行；未装配 Transcripts / 段读取失败则退化为不去重
		//（保持旧行为，不阻断——测试装配与线上降级同路径）。
		if d.Transcripts != nil {
			if tr, err := d.Transcripts.GetBySession(ctx, sessionID); err == nil {
				if segs, serr := d.Transcripts.ListSegments(ctx, tr.ID); serr == nil {
					states = repo.DedupStatesBySpeaker(states, repo.LabelDurations(segs))
				}
			}
		}
		for i := range states {
			st := &states[i]
			if st.SpeakerID == nil {
				continue // 未识别说话人：跳过
			}
			person, err := d.Persons.GetBySpeaker(ctx, *st.SpeakerID)
			if err != nil || person == nil {
				continue // 找不到绑定 person（未建档/未关联）或查询降级：跳过
			}
			// 幂等：同 person+emotion+measured_at(会话时间)+值 已写过则跳过（防 stage 重跑重复）。
			valence := profile.EmotionToValence(st.Emotion)
			vn, vt := valence, st.Emotion
			ex, err := d.PersonMetrics.FindByPointExt(ctx, d.DB, person.ID, "emotion", s.CreatedAt, &vn, &vt)
			if err == nil && ex != nil {
				continue // 已写过：幂等跳过
			}
			row := &repo.PersonMetric{
				UserID: 1, PersonID: person.ID, MetricKey: "emotion",
				ValueNum: &vn, ValueText: &vt,
				MeasuredAt:           s.CreatedAt,
				Confidence:           st.Confidence,
				EpistemicType:        "observed",
				Source:               "auto",
				Status:               "active",
				TranscriptSegmentIDs: nil, // Create 内部兜底为空数组
			}
			if err := d.PersonMetrics.Create(ctx, row); err != nil {
				log.Printf("[emotionprofile] 写 emotion metric 失败(person=%s, 跳过): %v", person.ID, err)
				continue
			}
		}
		return nil
	}
}
