// stage_correct 实现 correct stage（ASR 实体纠错）：拼音/音素召回候选白名单 →
// LLM 一程裁决（只改白名单内实体）→ 双重门控后局部替换并标记 corrected_reason='entity'。
// 设计见 docs/superpowers/specs/2026-08-29-asr-entity-correction-design.md。
// 本文件含 prompt 契约与输出解析（ParseCorrectionEdits）+ stage 主体（runCorrectStage/stageCorrect）。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"zhiwei/internal/entity"
	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// defaultCorrectConcurrency 逐段 LLM 调用的默认并发数：6——关思考后单次 ~1.4s，
// 6 并发把长录音（实测 47 段有候选）的 correct 墙钟从 ~29s 压到 ~7s；对 Ark flash
// 档限速是温和压力。可用 ZW_ENTITY_CORRECT_CONCURRENCY 调（1=退回串行）。
const defaultCorrectConcurrency = 6

// correctionEdit LLM 输出的一条纠错建议（asr_correction_v1 契约）。
// 去拷贝化后改为 canonical 唯一门控：corrected 逐字等于白名单 canonical 即充分
// （原 entity_id 二次校验随实时实体无稳定 id 而去掉，替换文本本就是 corrected）。
type correctionEdit struct {
	Orig       string  `json:"orig"`       // 段内原片段（门控：必须原样出现在段文本里）
	Corrected  string  `json:"corrected"`  // 替换目标（门控：必须逐字等于白名单 canonical）
	Confidence float64 `json:"confidence"` // 0~1，clamp
	Reason     string  `json:"reason"`     // 简短依据（存 entity_edits 供前端 tooltip）
}

// correctionResult LLM 输出整体。
type correctionResult struct {
	Edits []correctionEdit `json:"edits"`
}

// ParseCorrectionEdits 解析 LLM 纠错输出（纯函数，可单测）。
// 容错同 ParseNameCandidates：截取首 { 到末 }，剥掉围栏与前后废话；
// 彻底非法 JSON 返回 error（调用方吞错跳过该段，不 fail session）。
// 清洗：置信度 clamp 到 [0,1]；空 orig/corrected 丢弃（无法应用）。
func ParseCorrectionEdits(raw string) ([]correctionEdit, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out correctionResult
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("纠错输出解析失败: %w", err)
	}
	edits := make([]correctionEdit, 0, len(out.Edits))
	for _, e := range out.Edits {
		if strings.TrimSpace(e.Orig) == "" || strings.TrimSpace(e.Corrected) == "" {
			continue
		}
		e.Confidence = clampConfidence(e.Confidence) // 复用 stage_speaker_name.go 的 clamp
		edits = append(edits, e)
	}
	return edits, nil
}

// runCorrectStage 是 correct stage 的可测核心（避开 pool），由 stageCorrect 包装。
//
// 流程：读设置（关→no-op）→ 刷新实体库（失败降级用旧库）→ 读段（跳过已 entity 纠正段=幂等）
// → 逐段：召回白名单（空→跳过 LLM）→ 组上下文 → LLM → 解析 → 门控应用 → 落库。
// 全程 best-effort（同 speakername）：LLM/解析失败 log+trace 后继续，不 fail session；
// 真 DB 错误（读段/写段/读设置）返回 error 交 pool 重试。
func runCorrectStage(ctx context.Context, d StageDeps, j *repo.Job, sessionID ids.ID) error {
	if !d.CorrectEnabled {
		return nil // env 总开关关闭 → no-op（stage 常驻流水线，见 StageDeps.CorrectEnabled 注释）
	}
	if d.EntityKB == nil || d.EntitySettings == nil || d.LLM == nil {
		return nil // 依赖未装配（测试/降级）→ no-op
	}
	s, err := d.Sessions.Get(ctx, 1, sessionID) // 阶段1：后台流水线无请求上下文，暂 user-1
	if err != nil {
		return fmt.Errorf("读 session: %w", err)
	}
	st, err := d.EntitySettings.Get(ctx, s.UserID)
	if err != nil {
		return fmt.Errorf("读实体纠错设置: %w", err)
	}
	if !st.CorrectionEnabled {
		return nil
	}
	// 实时组装纠错白名单（去拷贝化）：auto 从源表实时聚合(AssembleEntities) + entity_kb 的
	// manual 启用条目 − entity_disabled 禁用名单。不再 RefreshAuto 全删全落拷贝。
	// auto/manual 读失败降级为「仅用另一来源」(best-effort)；disabled/manual 的 DB 真错误才 return。
	var entities []repo.Entity
	if d.EntitySeed.Persons != nil || d.EntitySeed.Speakers != nil || d.EntitySeed.Pets != nil || d.EntitySeed.Topics != nil {
		auto, err := entity.AssembleEntities(ctx, d.EntitySeed, s.UserID, st.AutoSources)
		if err != nil {
			log.Printf("[correct] session=%s 实时聚合失败（降级仅用 manual）: %v", sessionID, err)
			appendTrace(j, repo.TraceEntry{Stage: "correct", Error: fmt.Sprintf("实时聚合失败（降级）: %v", err)})
		} else {
			entities = append(entities, auto...)
		}
	}
	if d.EntityKB != nil {
		manual, err := d.EntityKB.ListManualEnabled(ctx, s.UserID)
		if err != nil {
			return fmt.Errorf("读 manual 实体: %w", err)
		}
		entities = entity.MergeWhitelist(entities, manual, loadDisabled(ctx, d, s.UserID))
	}
	if len(entities) == 0 {
		return nil // 空库无事可做
	}
	tr, err := d.Transcripts.GetBySession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("读 transcript: %w", err)
	}
	segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return fmt.Errorf("读 segments: %w", err)
	}

	window := d.CorrectWindow
	if window <= 0 {
		window = 2
	}
	topK := d.CorrectTopK
	if topK <= 0 {
		topK = 5
	}
	minSim := d.CorrectMinSim
	if minSim <= 0 {
		minSim = 0.6 // 召回下限默认（实测同音错≈0.95、无关≈0.57）
	}
	// 成本护栏：逐段 LLM 调用的会话级上限。长录音（数百段）即便三成段有候选，
	// 也是上百次调用——token 花费随录音长度线性无界。达上限后余下段本轮跳过
	// （不落标记，下轮重跑会自然续上——幂等跳过已纠正段，从第一个未纠正候选段继续）。
	maxLLMCalls := d.CorrectMaxLLMCalls
	if maxLLMCalls <= 0 {
		maxLLMCalls = 500
	}

	changed := false
	// 预筛（纯 CPU 无 IO）：幂等跳过 + 空文本 + 召回候选，先算出全部要调 LLM 的段。
	// 成本护栏也在此截断（保序取前 maxLLMCalls 个有候选段，与旧串行版 break 语义一致）。
	type segWork struct {
		idx   int // 段在 segs 里的下标（correctSegment 组上下文窗口用）
		seg   *repo.TranscriptSegment
		cands []entity.Candidate
	}
	var work []segWork
	for i := range segs {
		sg := &segs[i]
		if (sg.CorrectedReason != nil && *sg.CorrectedReason == "entity") || len(sg.EntityEdits) > 0 {
			// 幂等：已纠正段跳过。除 corrected_reason=='entity' 外还认 entity_edits 非空——
			// corrected_reason 是共享列，后续 speaker stage 的 mismatch/short 改判会覆写掉
			// 'entity'，若只看 reason 会对已纠正文本重复召回+调 LLM（浪费；门控仍防错改）。
			continue
		}
		if strings.TrimSpace(sg.Text) == "" {
			continue
		}
		cands := entity.RecallCandidates(sg.Text, entities, topK, minSim)
		if len(cands) == 0 {
			continue // 白名单为空 → 不调 LLM（省成本 + 避免无约束改写）
		}
		work = append(work, segWork{idx: i, seg: sg, cands: cands})
	}
	if len(work) > maxLLMCalls {
		appendTrace(j, repo.TraceEntry{Stage: "correct", Error: fmt.Sprintf("LLM 调用达会话上限 %d，余下段本轮跳过（成本护栏，非错误）", maxLLMCalls)})
		work = work[:maxLLMCalls]
	}

	// 并行调 LLM（2026-09-02 优化）：段间相互独立——上下文窗口读的是各段原始文本
	//（应用阶段只写 DB、从不回写内存 segs，串行版同样如此），并行不改判定语义；
	// 完成顺序无关，结果按下标对齐，最终状态确定。关思考后单次 ~1.4s，串行 47 段
	// 仍要 ~29s（实测 3m40s 那条 session），并发把墙钟压到 ~1/并发数。
	if len(work) > 0 {
		concurrency := d.CorrectConcurrency
		if concurrency <= 0 {
			concurrency = defaultCorrectConcurrency
		}
		concurrency = min(concurrency, len(work))
		results := make([][]correctionEdit, len(work))
		var wg sync.WaitGroup
		sem := make(chan struct{}, concurrency)
		for wi := range work {
			wg.Add(1)
			go func(wi int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				w := work[wi]
				results[wi] = correctSegment(ctx, d, j, sessionID, w.seg, w.cands, segs, w.idx, window, st.ConfidenceThreshold)
			}(wi)
		}
		wg.Wait()

		// 串行应用（门控后的替换 + 落库）：与旧实现逐字一致，写库顺序确定。
		for wi := range work {
			sg := work[wi].seg
			edits := results[wi]
			if len(edits) == 0 {
				continue
			}
			// 门控通过的 edits 逐个应用到段文本副本（首次出现处替换，最小改动）。
			text := sg.Text
			var applied []appliedEdit
			for _, e := range edits {
				if !strings.Contains(text, e.Orig) {
					continue // 前一个替换改变了文本后可能不再包含（位置竞争），跳过
				}
				text = strings.Replace(text, e.Orig, e.Corrected, 1)
				applied = append(applied, appliedEdit{
					Orig: e.Orig, Corrected: e.Corrected,
					Canonical: e.Corrected, Confidence: e.Confidence, Reason: e.Reason,
				})
			}
			if len(applied) == 0 {
				continue
			}
			raw, err := json.Marshal(applied)
			if err != nil {
				log.Printf("[correct] session=%s 序列化 edits 失败: %v", sessionID, err)
				continue
			}
			if err := d.Transcripts.ApplyEntityCorrections(ctx, tr.ID, sg.ID, text, raw); err != nil {
				return fmt.Errorf("落库纠错段 %d: %w", sg.SequenceNo, err) // DB 写失败：真基础设施问题，交 pool 重试
			}
			changed = true
		}
	}
	if changed {
		if err := d.Transcripts.RecomputeFullText(ctx, tr.ID); err != nil {
			return fmt.Errorf("重算 full_text: %w", err)
		}
	}
	return nil
}

// appliedEdit 落库到 entity_edits 的明细（前端对照展示用）。
type appliedEdit struct {
	Orig       string  `json:"orig"`      // 原片段（删除线展示）
	Corrected  string  `json:"corrected"` // 纠正后（=白名单 canonical）
	Canonical  string  `json:"canonical"` // 命中的实体规范名（与 corrected 相同，冗余存便于前端直接用）
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason,omitempty"`
}

// correctSegment 单段纠错：组上下文 + 白名单 → LLM → 解析 → 门控（置信度 ≥ threshold、
// orig 原样在段内、corrected 逐字等于白名单 canonical）→ 返回通过的 edits。
// LLM/解析失败：log + trace + 返回 nil（best-effort，不 fail session）。
func correctSegment(ctx context.Context, d StageDeps, j *repo.Job, sessionID ids.ID,
	sg *repo.TranscriptSegment, cands []entity.Candidate, segs []repo.TranscriptSegment,
	i, window int, threshold float64) []correctionEdit {

	// 白名单 canonical 集合（门控校验用）：corrected 必须逐字等于其中某条。
	canonSet := make(map[string]bool, len(cands))
	var sb strings.Builder
	sb.WriteString("合法实体白名单（corrected 只能逐字等于这里）：\n")
	for _, c := range cands {
		fmt.Fprintf(&sb, "- canonical=%s kind=%s\n", c.Canonical, c.Kind)
		canonSet[c.Canonical] = true
	}
	sb.WriteString("\n对话转写（【本段】是要纠错的段落，其余为上下文参考）：\n")
	for k := i - window; k <= i+window; k++ {
		if k < 0 || k >= len(segs) || k == i {
			continue
		}
		fmt.Fprintf(&sb, "【前文/后文】%s\n", segs[k].Text)
	}
	fmt.Fprintf(&sb, "【本段】%s\n", sg.Text)

	begin := time.Now()
	resp, err := d.LLM.Chat(ctx, provider.ChatRequest{
		Model: d.LLMModel, System: d.CorrectPrompt, User: sb.String(), Temperature: 0.1,
		// 实体纠错=白名单内近音改写（四重门控兜底），不需要隐性推理：2026-09-02 实测
		// doubao-seed 默认思考下单次 9~26s、~1000 思考 tokens 纯浪费，disabled 后
		// 1.3~1.6s 且输出等价（见 provider.ChatRequest.NoThinking 注释）。
		NoThinking: true,
	})
	if err != nil {
		log.Printf("[correct] session=%s 段%d LLM 失败（尽力而为）: %v", sessionID, sg.SequenceNo, err)
		appendTrace(j, repo.TraceEntry{Stage: "correct", Error: fmt.Sprintf("段%d LLM 失败（尽力而为）: %v", sg.SequenceNo, err)})
		return nil
	}
	appendTrace(j, repo.TraceEntry{Stage: "correct:llm", Model: d.LLMModel, MS: msSince(begin), Tokens: resp.TotalTokens, PromptVersion: d.CorrectPromptVersion})
	edits, err := ParseCorrectionEdits(resp.Content)
	if err != nil {
		log.Printf("[correct] session=%s 段%d 解析失败（尽力而为）: %v", sessionID, sg.SequenceNo, err)
		appendTrace(j, repo.TraceEntry{Stage: "correct", Error: fmt.Sprintf("段%d 解析失败（尽力而为）: %v", sg.SequenceNo, err)})
		return nil
	}
	// 门控：阈值 + orig 在段内 + corrected 逐字等于白名单某 canonical。
	var pass []correctionEdit
	for _, e := range edits {
		if e.Confidence < threshold {
			continue
		}
		if !strings.Contains(sg.Text, e.Orig) {
			continue // 幻觉 orig：段里根本没有这个片段
		}
		if !canonSet[e.Corrected] {
			continue // 幻觉实体：corrected 不在白名单 canonical 内
		}
		pass = append(pass, e)
	}
	return pass
}

// loadDisabled 读用户禁用名单（持久停用的自动实体名）。repo 为 nil（未装配）或读失败时
// 降级为空 map（best-effort：禁用名单缺失仅导致被禁用名仍参与纠错，属可接受的过包含）。
func loadDisabled(ctx context.Context, d StageDeps, userID int64) map[string]bool {
	if d.EntityDisabled == nil {
		return nil
	}
	m, err := d.EntityDisabled.ListDisabled(ctx, userID)
	if err != nil {
		return nil
	}
	return m
}

// stageCorrect 是 pool 用的 Handler 包装。
func stageCorrect(d StageDeps) Handler {
	return func(ctx context.Context, j *repo.Job, sessionID ids.ID) error {
		return runCorrectStage(ctx, d, j, sessionID)
	}
}
