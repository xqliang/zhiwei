package memory

// conversation.go 实现「对话转记忆」的编排层（与 extract.go 同层）：
// 把「问知微」对话历史（agent_message 文本行）转成候选记忆，复用录音抽取范式
//（组装块 → Extractor 窗口化 LLM 抽取 → ParseCandidates → ApplyGate → ResolveTopics
// → 单事务落库），只是溯源标记换成 conversation_id、session_id 置 NULL。
// 见 spec §6.3（memory 加 conversation_id + session_id 可空）/ §12（复用抽取范式，
// 产物是候选记忆而非「修改」，不走 agent_proposal 人审闸门）。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// ConversationExtractDeps 是对话转记忆的依赖注入（镜像 pipeline.StageDeps，但按 memory 范围裁剪）。
// 通过注入的 repo 集 + *sqlx.DB 落库；无 import 环（repo 不 import memory）。
type ConversationExtractDeps struct {
	DB            *sqlx.DB
	AgentMessages *repo.AgentMessageRepo
	Topics        *repo.TopicRepo
	Memories      *repo.MemoryRepo
	MemoryTopics  *repo.MemoryTopicRepo

	LLM           provider.LLMProvider
	Model         string     // ZW_AGENT_MODEL 或 LLMFastModel
	Prompt        string     // prompts/conversation_extraction_v1.md 内容
	PromptVersion string     // 进日志/trace
	Window        int        // config.ExtractWindow
	Gate          GateConfig // {MinConf, TodoConf}
}

// ConversationExtractResult 是一次抽取的统计（供 handler 返回 / 日志）。
type ConversationExtractResult struct {
	Messages   int // 参与抽取的文本消息数
	Candidates int // LLM 产出候选数
	Kept       int // 过闸门 + 去重后新落库的 memory 数
	NewTopics  int // 新建的建议 topic 数
	Windows    int // LLM 调用窗口数
	Tokens     int // 累计 token
}

// convTopicPromptLimit 是进入抽取 prompt 的已有主题数上限（与 stage_extract 的 topicPromptLimit 对齐）。
const convTopicPromptLimit = 30

// ExtractConversation 从一段「问知微」对话抽取候选记忆并落库（幂等：按 conversation_id 先删后插）。
// 空对话/无文本消息直接返回零结果（非错误）。产物是候选记忆（走 memory 候选流程，
// 不建 agent_proposal——见计划 D-A）。
func ExtractConversation(ctx context.Context, d ConversationExtractDeps, convID ids.ID) (ConversationExtractResult, error) {
	msgs, err := d.AgentMessages.ListByConversation(ctx, 1, convID) // 阶段1：后台抽取无请求上下文，暂 user-1
	if err != nil {
		return ConversationExtractResult{}, fmt.Errorf("读对话消息: %w", err)
	}
	blocks, baseTime := buildConversationBlocks(msgs)
	if len(blocks) == 0 {
		return ConversationExtractResult{}, nil // 无可抽内容，幂等空跑
	}
	topics, err := d.Topics.ListActive(ctx, 1, convTopicPromptLimit) // user_id=1（单用户 MVP）
	if err != nil {
		return ConversationExtractResult{}, fmt.Errorf("读 topics: %w", err)
	}
	ex := &Extractor{LLM: d.LLM, Model: d.Model, Prompt: d.Prompt, Window: d.Window}
	cands, err := ex.Extract(ctx, blocks, topics, baseTime)
	if err != nil {
		return ConversationExtractResult{}, fmt.Errorf("抽取: %w", err)
	}
	gated := ApplyGate(cands, d.Gate)
	refs, newNames := ResolveTopics(gated, topics)
	kept, err := commitConversation(ctx, d, convID, gated, refs, newNames)
	if err != nil {
		return ConversationExtractResult{}, fmt.Errorf("commit: %w", err)
	}
	res := ConversationExtractResult{
		Messages: countTextMessages(msgs), Candidates: len(cands),
		Kept: kept, NewTopics: len(newNames),
		Windows: ex.Stats().Windows, Tokens: ex.Stats().Tokens,
	}
	log.Printf("[conv-extract] conv=%s ver=%s msgs=%d 候选=%d 落库=%d 新topic=%d",
		convID, d.PromptVersion, res.Messages, res.Candidates, res.Kept, res.NewTopics)
	return res, nil
}

// buildConversationBlocks 把对话消息转成抽取块。跳过工具类消息（tool_call/tool_result/card），
// 只保留 user/assistant 的文本发言（两方都进块：助手文本是理解用户的上下文，交给 prompt 去
// 「只抽用户」）。返回块列表与基准时间（首条文本消息时间）：每块 StartMS = 相对 base 的毫秒偏移，
// 使 Extractor 算出的 EventAt ≈ 该发言时间。对话无 transcript segment，SegmentIDs 留空——
// 溯源靠 conversation_id。
func buildConversationBlocks(msgs []repo.AgentMessage) ([]Block, time.Time) {
	var base time.Time
	var blocks []Block
	for _, m := range msgs {
		if m.Kind != "" && m.Kind != "text" { // 空 kind 兼容旧行，视作 text
			continue
		}
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if base.IsZero() {
			base = m.CreatedAt
		}
		speaker := "用户"
		if m.Role == "assistant" {
			speaker = "知微"
		}
		off := m.CreatedAt.Sub(base).Milliseconds()
		if off < 0 {
			off = 0 // 防御：消息本应按 id(=时间)升序
		}
		blocks = append(blocks, Block{
			SpeakerLabel: speaker, Text: m.Content,
			StartMS: off, EndMS: off, SegmentIDs: nil,
		})
	}
	return blocks, base
}

// countTextMessages 统计参与抽取的文本消息数（与 buildConversationBlocks 的筛选口径一致）。
func countTextMessages(msgs []repo.AgentMessage) int {
	n := 0
	for _, m := range msgs {
		if (m.Kind == "" || m.Kind == "text") && (m.Role == "user" || m.Role == "assistant") && strings.TrimSpace(m.Content) != "" {
			n++
		}
	}
	return n
}

// commitConversation 单事务落库：按 conversation_id 幂等清理后重插，标 conversation_id、session_id 置 NULL。
// dedup 复用 stage_extract 的 D1 佐证逻辑：新候选归一化标题命中已有 active 记忆（或批内已加）→ 不增行、
// 上调 canonical 置信度 + 把候选 topic 并入 canonical。返回新落库记忆数。
//
// 顺序（刻意镜像 pipeline.commitExtract，但按 conversation_id 幂等、聚焦 memory）：
// 删旧关联 → 删旧记忆 → 建议 topic → dedup 决策 → 插新记忆 → 佐证上调 → memory_topic(ai)。
//
// 与 §12 的取舍（明确记录）：
//   - todo 候选：本计划聚焦 memory；todo 可作后续平行扩展（照搬 stage_extract 的 IsTodo/TodoStatus
//   - Todos.InsertExt + TodoTopics，SourceMemoryID 指向对应新记忆），此处不实现以免超范围。
//   - 用户手动 topic 关联的快照/重链：对话记忆当前无「用户手动关联对话记忆到 topic」的入口
//     （前端不在本 scope），故 MVP 不做快照/重链（YAGNI）；若后续开放，照 commitExtract 的
//     SnapshotUserBySessionExt 模式加 ...ByConversationExt 即可。
func commitConversation(ctx context.Context, d ConversationExtractDeps, convID ids.ID,
	gated []Candidate, refs [][]TopicRef, newNames []string) (int, error) {

	tx, err := d.DB.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op

	// 1. 幂等清理（先关联后主表）——本会话旧对话记忆整体删除后重插。
	if err := d.MemoryTopics.DeleteByConversationExt(ctx, tx, convID); err != nil {
		return 0, fmt.Errorf("清理旧 memory_topic: %w", err)
	}
	if err := d.Memories.DeleteByConversationExt(ctx, tx, convID); err != nil {
		return 0, fmt.Errorf("清理旧 memory: %w", err)
	}

	// 2. 新建建议 topic（事务内同名查重兜底，同 stage_extract）。
	nameToID := make(map[string]ids.ID, len(newNames))
	for _, name := range newNames {
		if ex, err := d.Topics.FindActiveByNameExt(ctx, tx, 1, name); err != nil {
			return 0, fmt.Errorf("查重建议 topic %q: %w", name, err)
		} else if ex != nil {
			nameToID[name] = ex.ID
			continue
		}
		tp := &repo.Topic{Name: name, Status: "suggested", CreatedBy: "ai"}
		if err := d.Topics.CreateExt(ctx, tx, tp); err != nil {
			return 0, fmt.Errorf("建建议 topic %q: %w", name, err)
		}
		nameToID[name] = tp.ID
	}
	resolveTopicID := func(ref TopicRef) (ids.ID, bool) {
		if ref.ExistingID != nil {
			return *ref.ExistingID, true
		}
		if id, ok := nameToID[ref.NewName]; ok {
			return id, true
		}
		return 0, false
	}

	// 3. dedup 决策 + 插新记忆（D1 佐证：命中已有 active 标题不增行）。
	//    tx 内读 ListActiveTitlesExt：已 DeleteByConversationExt 删了本会话旧记忆→本会话内不自去重；
	//    跨来源（录音/别的对话）旧记忆仍会命中→佐证（可接受，同 stage_extract）。
	activeTitles, err := d.Memories.ListActiveTitlesExt(ctx, tx, 1)
	if err != nil {
		return 0, fmt.Errorf("读 active 标题: %w", err)
	}
	dupSet := map[string]ids.ID{}
	for _, at := range activeTitles {
		dupSet[repo.NormalizeTitle(at.Title)] = at.ID
	}
	resolvedTids := make([][]ids.ID, len(gated))
	type corrob struct {
		canonID ids.ID
		tids    []ids.ID
	}
	var corrobs []corrob
	var kept []*repo.Memory
	keptOf := make([]*repo.Memory, len(gated)) // 按候选下标，佐证跳过位为 nil
	for i, c := range gated {
		seen := map[ids.ID]bool{}
		for _, ref := range refs[i] {
			if id, ok := resolveTopicID(ref); ok && !seen[id] {
				seen[id] = true
				resolvedTids[i] = append(resolvedTids[i], id)
			}
		}
		nk := repo.NormalizeTitle(c.Title)
		if canon, hit := dupSet[nk]; hit {
			corrobs = append(corrobs, corrob{canonID: canon, tids: resolvedTids[i]})
			continue
		}
		eventAt := c.EventAt // 局部变量取址，避免共享循环变量
		m := &repo.Memory{
			ID:   ids.New(),
			Type: c.Type, Title: c.Title, Content: c.Content,
			EpistemicType: c.EpistemicType,
			Importance:    c.Importance, Confidence: c.Confidence,
			TranscriptSegmentIDs: ids.List(c.SegmentIDs), // 对话来源通常空
			EventAt:              &eventAt, Status: "active",
			// SessionID / ConversationID 由 InsertConversationExt 统一盖
		}
		keptOf[i] = m
		kept = append(kept, m)
		dupSet[nk] = m.ID // 批内去重
	}
	if err := d.Memories.InsertConversationExt(ctx, tx, convID, kept); err != nil {
		return 0, fmt.Errorf("写 memory: %w", err)
	}

	// 4. 佐证上调（kept 已插入，批内 canonical 现已在库）+ 并入候选 topic。
	for _, cr := range corrobs {
		if err := d.Memories.BumpConfidenceExt(ctx, tx, cr.canonID, 0.05); err != nil {
			return 0, fmt.Errorf("佐证上调 %s: %w", cr.canonID, err)
		}
		var rows []*repo.MemoryTopicLink
		for _, tid := range cr.tids {
			rows = append(rows, &repo.MemoryTopicLink{MemoryID: cr.canonID, TopicID: tid, Source: "ai"})
		}
		if err := d.MemoryTopics.InsertExt(ctx, tx, rows); err != nil {
			return 0, fmt.Errorf("并入 topic 到 %s: %w", cr.canonID, err)
		}
	}

	// 5. 新记忆的 memory_topic(ai) 关联。
	var links []*repo.MemoryTopicLink
	for i := range gated {
		if keptOf[i] == nil {
			continue
		}
		for _, tid := range resolvedTids[i] {
			links = append(links, &repo.MemoryTopicLink{MemoryID: keptOf[i].ID, TopicID: tid, Source: "ai"})
		}
	}
	if err := d.MemoryTopics.InsertExt(ctx, tx, links); err != nil {
		return 0, fmt.Errorf("写 memory_topic: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(kept), nil
}
