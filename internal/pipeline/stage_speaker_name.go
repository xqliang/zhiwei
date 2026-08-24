// stage_speaker_name 实现 speakername stage：对「名字仍是自动随机名」的说话人，
// 用 LLM 从跨录音墙钟窗口（当前录音全文 + 前 N 分钟）的对话转写推断真实称呼，
// 候选（名称+置信度+证据）写入 speaker_name_candidate。仅作建议，不改名。
// 设计见 docs/superpowers/specs/2026-08-24-speaker-name-inference-design.md。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// autoNamePattern 自动登记的默认名：说话人 + 5 位 [a-z0-9]（stage_speaker.go rand5 产物）。
// 用户改名后不再命中 → 不再重复推断（比 source=auto 判定准：source 不随改名变）。
// 注意与显示回退「说话人 N」（带空格）区分——那个从不落库为 speaker.name。
var autoNamePattern = regexp.MustCompile(`^说话人[a-z0-9]{5}$`)

// isAutoName 判断说话人名是否仍是自动登记的随机名（= 待识别）。
func isAutoName(name string) bool { return autoNamePattern.MatchString(name) }

// nameCandidate LLM 输出的一条候选名。
type nameCandidate struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
}

// nameInferResult LLM 输出整体：每个待识别人物（ref=占位符）一组候选。
type nameInferResult struct {
	Speakers []struct {
		Ref        string          `json:"ref"`
		Candidates []nameCandidate `json:"candidates"`
	} `json:"speakers"`
}

// ParseNameCandidates 解析 LLM 输出为 ref→候选列表（纯函数，可单测）。
// 容错同 memory.ParseCandidates：截取首 { 到末 }，剥掉围栏与前后废话；
// 彻底非法 JSON 返回 error（由 stage 走重试）。
// 候选内清洗：空名丢弃、置信度 clamp 到 [0,1]、按置信度倒排。
func ParseNameCandidates(raw string) (map[string][]nameCandidate, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out nameInferResult
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("名字推断结果解析失败: %w", err)
	}
	m := make(map[string][]nameCandidate, len(out.Speakers))
	for _, sp := range out.Speakers {
		cands := make([]nameCandidate, 0, len(sp.Candidates))
		for _, c := range sp.Candidates {
			if strings.TrimSpace(c.Name) == "" {
				continue // 无名候选丢弃
			}
			// 按 rune 截断到 DB VARCHAR 上限：MySQL STRICT_TRANS_TABLES 下超长写入会
			// Data too long 报错，令 Upsert 失败 → stage 返回 error，违背 speakername
			// 「尽力而为、不阻塞 session」不变量。宁可截断建议，也不让流水线因此中断。
			c.Name = truncateRunes(strings.TrimSpace(c.Name), maxCandidateNameRunes)
			c.Confidence = clampConfidence(c.Confidence)
			c.Evidence = truncateRunes(c.Evidence, maxCandidateEvidenceRunes)
			cands = append(cands, c)
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].Confidence > cands[j].Confidence })
		// ref 先 TrimSpace 再作 key：模型输出带杂散空格时仍能与本 stage 分配的占位符匹配，
		// 不至于因空白差异静默丢弃整组候选（M4）。
		m[strings.TrimSpace(sp.Ref)] = cands
	}
	return m, nil
}

// clampConfidence 置信度越界归位 [0,1]。
func clampConfidence(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// 候选名 / 证据的存储上限（rune 数），对齐 migration 000005 的 VARCHAR 列宽。
// utf8mb4 下 VARCHAR(N) 按「字符」计，故用 rune 数而非字节数裁剪。
const (
	maxCandidateNameRunes     = 128 // speaker_name_candidate.name    VARCHAR(128)
	maxCandidateEvidenceRunes = 512 // speaker_name_candidate.evidence VARCHAR(512)
)

// truncateRunes 把字符串按 rune（字符）安全截断到 max 个字符。
// 按 rune 而非字节切，避免把多字节中文拦腰截成半个字符（乱码 / 非法 UTF-8）。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// runSpeakerNameStage 是 speakername stage 的可测核心（避开 pool），由 stageSpeakerName 包装。
//
// 流程：本 session 段解析到的说话人中筛「名字仍是随机名」= 待识别 T
// → 取跨录音墙钟窗口上下文（当前录音全文 + 前 W 分钟）→ 待识别者分配占位符
// → 单次 LLM 调用（批处理：T 共享同一上下文，模型可跨说话人联合推理）
// → ref 映射回 speaker_id，逐候选 upsert（GREATEST 累积置信度，幂等）。
// LLM/候选 repo 未装配时 no-op（兼容旧装配/纯 ASR 测试）。
//
// 尽力而为语义（I2）：LLM 调用失败 / 候选解析失败**不返回 error**，只 log + trace 后
// 返回 nil。理由：候选仅是建议，说话人仍是随机名时下一段录音会自然重试；不该让建议性
// 功能 fail 整个 session、阻塞下游 extract。真基础设施问题（读 session/segments/speakers、
// 读上下文段、写候选等 DB 操作）仍返回 error，交 pool 重试。
func runSpeakerNameStage(ctx context.Context, d StageDeps, j *repo.Job, sessionID ids.ID, tr *repo.Transcript) error {
	if d.LLM == nil || d.SpeakerNameCandidates == nil {
		return nil // 依赖未装配（测试/降级）→ no-op
	}
	s, err := d.Sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("读 session: %w", err)
	}
	segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return fmt.Errorf("读 segments: %w", err)
	}
	if len(segs) == 0 {
		return nil
	}
	// 1) 待识别集合 T：本 session 段解析到的说话人中名字仍是随机名者。
	//    按 session 内首次出现顺序分配占位符（A/B…，说话人数 ≤26 足够）。
	speakerList, err := d.Speakers.List(ctx)
	if err != nil {
		return fmt.Errorf("读 speakers: %w", err)
	}
	nameByID := make(map[ids.ID]string, len(speakerList))
	for _, sp := range speakerList {
		nameByID[sp.ID] = sp.Name
	}
	pending := map[ids.ID]bool{}
	var order []ids.ID
	var durationMS int64 // 本录音时长 = 段最大 end_ms（上下文窗口上界偏移）
	for _, sg := range segs {
		if sg.EndMS > durationMS {
			durationMS = sg.EndMS
		}
		if sg.SpeakerID == nil || pending[*sg.SpeakerID] {
			continue
		}
		if isAutoName(nameByID[*sg.SpeakerID]) {
			pending[*sg.SpeakerID] = true
			order = append(order, *sg.SpeakerID)
		}
	}
	if len(pending) == 0 {
		return nil // 无待识别 → 不调 LLM（省 token）
	}
	refOf := make(map[ids.ID]string, len(order))
	refToID := make(map[string]ids.ID, len(order))
	for i, spID := range order {
		ref := fmt.Sprintf("待识别人物%c", 'A'+i)
		if i >= 26 {
			// 防御性兜底：'A'+i 越过 'Z'（i≥26）会产出非字母字符，破坏 prompt 契约里
			// 「待识别人物X」的形态。实际单次录音不可能出现 26+ 说话人，仅防越界。
			ref = fmt.Sprintf("待识别人物%d", i+1)
		}
		refOf[spID] = ref
		refToID[ref] = spID
	}

	// 2) 上下文：墙钟窗口 [S.created_at − W, S.created_at + 本录音时长]。
	//    DESC+LIMIT 裁剪保留最近——本 session 段是窗口内最新的，天然优先保留。
	windowMin := d.NameInferWindowMin
	if windowMin <= 0 {
		windowMin = 10
	}
	maxSegs := d.NameInferMaxSegments
	if maxSegs <= 0 {
		maxSegs = 400
	}
	from := s.CreatedAt.Add(-time.Duration(windowMin) * time.Minute)
	to := s.CreatedAt.Add(time.Duration(durationMS) * time.Millisecond)
	ctxSegs, err := d.Transcripts.ListSegmentsInWallClockWindow(ctx, s.UserID, from, to, maxSegs)
	if err != nil {
		return fmt.Errorf("读上下文段: %w", err)
	}

	// 3) 组 user message：待识别清单 + 对话（时间|说话人 token|文本）。
	//    token 稳定可指认：待识别→占位符；已确认→真名；随机名非本 session 者按原随机名
	//    （区别于占位符，模型不会为其产候选——prompt 只认清单内的 ref）；未解析→未知。
	var sb strings.Builder
	sb.WriteString("待识别人物清单（只为清单内的占位符推断名字）：\n")
	for _, spID := range order {
		fmt.Fprintf(&sb, "- %s\n", refOf[spID])
	}
	sb.WriteString("\n对话转写（格式：时间|说话人|文本，按时间正序）：\n")
	for _, cs := range ctxSegs {
		token := "未知"
		if cs.SpeakerID != nil {
			if ref, ok := refOf[*cs.SpeakerID]; ok {
				token = ref
			} else if cs.SpeakerName != nil && *cs.SpeakerName != "" {
				token = *cs.SpeakerName // 已确认真名 或 非本 session 的随机名（原样区分）
			}
		}
		fmt.Fprintf(&sb, "%s|%s|%s\n", cs.WallTime.Format("15:04:05"), token, cs.Text)
	}

	// 4) 单次 LLM 调用（批处理）+ 解析。LLM/解析失败按尽力而为吞错（log + trace，返回 nil）。
	begin := time.Now()
	resp, err := d.LLM.Chat(ctx, provider.ChatRequest{
		Model:  d.LLMModel,
		System: d.NameInferPrompt,
		User:   sb.String(),
	})
	if err != nil {
		log.Printf("[speakername] session=%s LLM 调用失败（尽力而为，不阻塞）: %v", sessionID, err)
		appendTrace(j, repo.TraceEntry{Stage: "speakername", Error: fmt.Sprintf("名字推断失败（尽力而为，不阻塞）: %v", err)})
		return nil
	}
	appendTrace(j, repo.TraceEntry{Stage: "speakername:llm", Model: d.LLMModel, MS: msSince(begin), Tokens: resp.TotalTokens})
	parsed, err := ParseNameCandidates(resp.Content)
	if err != nil {
		log.Printf("[speakername] session=%s 解析名字候选失败（尽力而为，不阻塞）: %v", sessionID, err)
		appendTrace(j, repo.TraceEntry{Stage: "speakername", Error: fmt.Sprintf("名字推断失败（尽力而为，不阻塞）: %v", err)})
		return nil
	}
	// 5) ref → speaker_id 回填候选；未知 ref（模型编造的占位符）忽略。
	//    Upsert 是 DB 写，失败返回 error 走 pool 重试（真基础设施问题，非建议性）。
	for ref, cands := range parsed {
		spID, ok := refToID[ref]
		if !ok {
			continue
		}
		for _, c := range cands {
			if err := d.SpeakerNameCandidates.Upsert(ctx, spID, c.Name, c.Confidence, c.Evidence, sessionID); err != nil {
				return fmt.Errorf("写候选 %q: %w", c.Name, err)
			}
		}
	}
	return nil
}

// stageSpeakerName 是 pool 用的 Handler 包装。
func stageSpeakerName(d StageDeps) Handler {
	return func(ctx context.Context, j *repo.Job, sessionID ids.ID) error {
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读 transcript: %w", err)
		}
		return runSpeakerNameStage(ctx, d, j, sessionID, tr)
	}
}
