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
	"time"

	"zhiwei/internal/entity"
	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// correctionEdit LLM 输出的一条纠错建议（asr_correction_v1 契约）。
type correctionEdit struct {
	Orig       string  `json:"orig"`       // 段内原片段（门控：必须原样出现在段文本里）
	Corrected  string  `json:"corrected"`  // 替换目标（门控：必须逐字等于白名单 canonical）
	EntityID   string  `json:"entity_id"`  // 白名单条目 id（门控：必须在白名单内）
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
	// 刷新 auto 实体（失败不阻断：用库内旧实体继续纠错——旧库总比不纠好）。
	if err := entity.RefreshAuto(ctx, d.EntitySeed, s.UserID, st.AutoSources); err != nil {
		log.Printf("[correct] session=%s 实体库刷新失败（降级用旧库）: %v", sessionID, err)
		appendTrace(j, repo.TraceEntry{Stage: "correct", Error: fmt.Sprintf("实体库刷新失败（降级）: %v", err)})
	}
	entities, err := d.EntityKB.ListEnabled(ctx, s.UserID)
	if err != nil {
		return fmt.Errorf("读实体库: %w", err)
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
	// 也是上百次串行调用——token 花费随录音长度线性无界。达上限后余下段本轮跳过
	// （不落标记，下轮重跑会自然续上——幂等跳过已纠正段，从第一个未纠正候选段继续）。
	maxLLMCalls := d.CorrectMaxLLMCalls
	if maxLLMCalls <= 0 {
		maxLLMCalls = 500
	}
	llmCalls := 0

	changed := false
	for i := range segs {
		sg := &segs[i]
		if sg.CorrectedReason != nil && *sg.CorrectedReason == "entity" {
			continue // 幂等：已纠正段跳过（显式跳过比「召回为空」更省）
		}
		if strings.TrimSpace(sg.Text) == "" {
			continue
		}
		cands := entity.RecallCandidates(sg.Text, entities, topK, minSim)
		if len(cands) == 0 {
			continue // 白名单为空 → 不调 LLM（省成本 + 避免无约束改写）
		}
		if llmCalls >= maxLLMCalls {
			appendTrace(j, repo.TraceEntry{Stage: "correct", Error: fmt.Sprintf("LLM 调用达会话上限 %d，余下段本轮跳过（成本护栏，非错误）", maxLLMCalls)})
			break
		}
		edits := correctSegment(ctx, d, j, sessionID, sg, cands, segs, i, window, st.ConfidenceThreshold)
		llmCalls++
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
// orig 原样在段内、corrected/entity_id 在白名单内）→ 返回通过的 edits。
// LLM/解析失败：log + trace + 返回 nil（best-effort，不 fail session）。
func correctSegment(ctx context.Context, d StageDeps, j *repo.Job, sessionID ids.ID,
	sg *repo.TranscriptSegment, cands []entity.Candidate, segs []repo.TranscriptSegment,
	i, window int, threshold float64) []correctionEdit {

	// 白名单索引（门控校验用）：entity_id → canonical。门控要求 corrected 与
	// entity_id 指向**同一个候选**（分立双 map 会放过「corrected 取自候选 A、
	// entity_id 取自候选 B」的拼接错位——虽无害但不严谨）。
	idToCanon := make(map[string]string, len(cands))
	var sb strings.Builder
	sb.WriteString("合法实体白名单（corrected 只能取自这里）：\n")
	for _, c := range cands {
		fmt.Fprintf(&sb, "- id=%s canonical=%s kind=%s\n", c.EntityID, c.Canonical, c.Kind)
		idToCanon[c.EntityID.String()] = c.Canonical
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
	// 双重门控：阈值 + orig 在段内 + corrected/entity_id 同属白名单内同一候选。
	var pass []correctionEdit
	for _, e := range edits {
		if e.Confidence < threshold {
			continue
		}
		if !strings.Contains(sg.Text, e.Orig) {
			continue // 幻觉 orig：段里根本没有这个片段
		}
		if canon, ok := idToCanon[e.EntityID]; !ok || canon != e.Corrected {
			continue // 幻觉实体：白名单里没有，或 corrected 与 entity_id 指向不同候选（拼接错位）
		}
		pass = append(pass, e)
	}
	return pass
}

// stageCorrect 是 pool 用的 Handler 包装。
func stageCorrect(d StageDeps) Handler {
	return func(ctx context.Context, j *repo.Job, sessionID ids.ID) error {
		return runCorrectStage(ctx, d, j, sessionID)
	}
}
