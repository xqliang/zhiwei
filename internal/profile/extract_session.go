package profile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/repo"
)

// blockGapMS 是对话块聚合的间隔阈值：同说话人相邻段间隔超过此值强制切块。
// 与 pipeline.blockGapMS（internal/pipeline/stage_extract.go）保持一致——profile
// 包不能 import pipeline（会形成 pipeline→profile→pipeline 循环依赖），故常量本地
// 写死并注释出处；两处若要调整需同步。
const blockGapMS = 30000

// ExtractResult 是一次 ExtractSession 的统计（trace/日志用）。
type ExtractResult struct {
	Apply   ApplyStats // 落库决策统计（active/pending/reaffirm/skip 等）
	Windows int        // LLM 调用次数（窗口数）
	Tokens  int        // 累计 token 用量
}

// ExtractSession 对一个 session 跑完整画像抽取：读转写段（说话人名替换）→
// 聚合块 → LLM 抽取 → ApplyFacts 落库。pipeline profile stage（Task 14）与 API
// 回填端点（Task 16）共用此入口，保证「说话人名替换 + 聚合 + 抽取 + 落库」这条
// 链路只实现一次。
//
// 边界处理：
//   - session 不存在 → ErrNotFound（API 回填端点将其记入批次结果条目；单查接口映射 404）；
//   - session 无转写（还没跑 ASR / 空录音）或无有效文字 → 返回零值 res 且不报错
//     （低价值不进抽取，与 pipeline extract stage 一致；回填端点批量重放历史
//     session 时可安全跳过这类 session，不因个别缺转写而整批失败）。
func (s *Service) ExtractSession(ctx context.Context, sessionID ids.ID) (ExtractResult, error) {
	var res ExtractResult
	ss, err := s.Sessions.Get(ctx, sessionID)
	if err != nil {
		// SessionRepo.Get 用 sqlx GetContext，行不存在返回 sql.ErrNoRows。
		if errors.Is(err, sql.ErrNoRows) {
			return res, ErrNotFound
		}
		return res, fmt.Errorf("读取 session: %w", err)
	}

	tr, err := s.Transcripts.GetBySession(ctx, sessionID)
	if err != nil {
		// 无转写（GetBySession 走 GetContext，无行即 sql.ErrNoRows）：按「无有效文字」
		// 处理，直接返回零值不报错——回填端点重放历史 session 时能安全跳过。
		if errors.Is(err, sql.ErrNoRows) {
			return res, nil
		}
		return res, fmt.Errorf("读取 transcript: %w", err)
	}
	segs, err := s.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return res, fmt.Errorf("读取 segments: %w", err)
	}

	// 说话人名替换（同 extract stage）：把已回填 speaker_id 的段的 ASR 标签换成已登记
	// 声纹名，LLM 抽取时才能区分是谁说的。speaker stage 已回填 SpeakerID；未解析的段
	// （SpeakerID 为 NULL）保持原 ASR 标签。名册读取失败不阻断抽取，退化为用原标签。
	if speakerNames, err := speakerNameMap(ctx, s.Speakers); err == nil {
		for i := range segs {
			if segs[i].SpeakerID != nil {
				if name, ok := speakerNames[*segs[i].SpeakerID]; ok {
					segs[i].SpeakerLabel = name
				}
			}
		}
	}

	// 对话块聚合；无有效文字的 session 直接返回零值（低价值不进抽取，同 extract stage）。
	blocks := memory.AggregateBlocks(segs, blockGapMS)
	if len(blocks) == 0 {
		return res, nil
	}

	// 已知人物名单（稳定引用，避免 LLM 每次为同一个人发明新指代）。
	persons, err := s.Persons.List(ctx, ss.UserID)
	if err != nil {
		return res, fmt.Errorf("读取人物名单: %w", err)
	}
	refs := make([]PersonRef, 0, len(persons))
	for _, p := range persons {
		refs = append(refs, PersonRef{ID: p.ID, Name: p.DisplayName, IsOwner: p.IsOwner})
	}

	// 每次抽取各自 new 一个 Extractor（stats 不并发共享）。窗口切分在 Extractor 内部。
	ex := &Extractor{LLM: s.LLM, Model: s.Model, Prompt: s.Prompt, Window: s.Window}
	facts, err := ex.Extract(ctx, blocks, refs)
	if err != nil {
		return res, fmt.Errorf("LLM 抽取: %w", err)
	}
	stats := ex.Stats()
	res.Windows, res.Tokens = stats.Windows, stats.Tokens

	// 落库：人物归属解析 → 闸门 → 单事务写入（含 change_log）。幂等靠自然键去重，
	// 同 session 重跑不重复建 pending / 不重复 bump。
	st, err := s.ApplyFacts(ctx, sessionID, ss.UserID, facts)
	if err != nil {
		return res, fmt.Errorf("落库画像事实: %w", err)
	}
	res.Apply = st
	return res, nil
}

// speakerNameMap 建「speaker_id → 声纹名」映射，供转写段的说话人名替换用。
// 逻辑等价于 pipeline.buildSpeakerNameMap（internal/pipeline/stage_extract.go）——
// profile 包不能 import pipeline（循环依赖），故在本包重写这个小函数。
// SpeakerRepo.List 只返回 active 说话人，已 dismissed 的不参与改名。
func speakerNameMap(ctx context.Context, speakers *repo.SpeakerRepo) (map[ids.ID]string, error) {
	list, err := speakers.List(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[ids.ID]string, len(list))
	for _, sp := range list {
		m[sp.ID] = sp.Name
	}
	return m, nil
}
