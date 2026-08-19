// stage_extract 实现抽取 stage：对话块聚合 → LLM 抽取 → 质量闸门 →
// Topic 归属 → 单事务提交（memory + todo + 建议 topic）。
// 合并了上游 spec 的 extract/quality/commit 三步，理由见 Sprint 2 设计文档 §2：
// 中间产物无落库位置，质量闸门纯规则无独立重试价值，重跑整段代价可接受（幂等保证不重复）。
package pipeline

import (
	"context"
	"fmt"
	"log"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/repo"
)

// blockGapMS 是对话块聚合的间隔阈值：同说话人相邻段间隔超过此值强制切块。
const blockGapMS = 30000

// topicPromptLimit 是进入抽取 prompt 的已有主题数上限。
const topicPromptLimit = 30

func stageExtract(d StageDeps) Handler {
	return func(ctx context.Context, j *repo.Job, sessionID ids.ID) error {
		s, err := d.Sessions.Get(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读取 session: %w", err)
		}
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读取 transcript: %w", err)
		}
		segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
		if err != nil {
			return fmt.Errorf("读取 segments: %w", err)
		}

		// ① 对话块聚合；无有效文字的会话直接完成（低价值不进抽取）
		blocks := memory.AggregateBlocks(segs, blockGapMS)
		if len(blocks) == 0 {
			appendTrace(j, repo.TraceEntry{Stage: "extract", MS: 0, Error: "无有效文字，跳过抽取"})
			return nil
		}

		// ② + ③ LLM 抽取（窗口切分在 Extractor 内部）
		topics, err := d.Topics.ListActive(ctx, s.UserID, topicPromptLimit)
		if err != nil {
			return fmt.Errorf("读取 topics: %w", err)
		}
		ex := &memory.Extractor{LLM: d.LLM, Model: d.LLMModel, Prompt: d.Prompt, Window: d.ExtractWindow}
		llmBegin := time.Now()
		cands, err := ex.Extract(ctx, blocks, topics, s.CreatedAt)
		if err != nil {
			return fmt.Errorf("抽取: %w", err)
		}
		appendTrace(j, repo.TraceEntry{Stage: "extract:llm", Model: d.LLMModel, MS: msSince(llmBegin)})

		// ④ 质量闸门
		gated := memory.ApplyGate(cands, d.Gate)

		// ⑤ Topic 归属决策（纯逻辑）+ 单事务提交
		refs, newNames := memory.ResolveTopics(gated, topics)
		commitBegin := time.Now()
		err = commitExtract(ctx, d, sessionID, s.UserID, gated, refs, newNames)
		if err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		appendTrace(j, repo.TraceEntry{
			Stage: "extract:commit", MS: msSince(commitBegin),
			Error: fmt.Sprintf("候选=%d 通过=%d 新topic=%d", len(cands), len(gated), len(newNames)),
		})
		log.Printf("[extract] session=%s blocks=%d 候选=%d 通过闸门=%d 新topic=%d",
			sessionID, len(blocks), len(cands), len(gated), len(newNames))
		return nil
	}
}

// commitExtract 在单事务内完成幂等清理与落库。
// 顺序：先删派生 todo（经 source_memory_id 关联）→ 删 memory →
// 建新建议 topic → 插 memory → 插 todo。任一步失败整体回滚。
// 已知 MVP 限制：重跑会重建用户在重试窗口内已 dismiss 的行
// （按 session 硬删，无状态过滤）。
func commitExtract(ctx context.Context, d StageDeps, sessionID ids.ID, userID int64,
	gated []memory.Candidate, refs []memory.TopicRef, newNames []string) error {

	tx, err := d.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后 Rollback 是 no-op

	// 1. 幂等清理：todo 删除依赖 memory 行仍存在（source_memory_id 子查询），必须先删
	if err := d.Todos.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 todo: %w", err)
	}
	if err := d.Memories.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 memory: %w", err)
	}

	// 2. 新建建议 topic。事务内先按名查重（FindActiveByNameExt 走 tx 连接）：
	// ResolveTopics 的查重在 LLM 调用前完成（秒级窗口），并发 extract 可能
	// 同时建议同名 topic 而双双漏判，这里兜底改为复用。
	// 注意：查重只能收窄窗口而非根除竞态（topic.name 无唯一约束）。
	nameToID := make(map[string]ids.ID, len(newNames))
	for _, name := range newNames {
		if existing, err := d.Topics.FindActiveByNameExt(ctx, tx, userID, name); err != nil {
			return fmt.Errorf("查重建议 topic %q: %w", name, err)
		} else if existing != nil {
			nameToID[name] = existing.ID
			continue
		}
		tp := &repo.Topic{Name: name, Status: "suggested", CreatedBy: "ai"}
		if err := d.Topics.CreateExt(ctx, tx, tp); err != nil {
			return fmt.Errorf("创建建议 topic %q: %w", name, err)
		}
		nameToID[name] = tp.ID
	}

	// 3. memory 入库（指针切片，ID 回填供 todo 引用）
	memories := make([]*repo.Memory, len(gated))
	for i, c := range gated {
		memories[i] = &repo.Memory{
			Type: c.Type, Title: c.Title, Content: c.Content,
			EpistemicType: c.EpistemicType,
			Importance:    c.Importance, Confidence: c.Confidence,
			SessionID: sessionID, TranscriptSegmentIDs: ids.List(c.SegmentIDs),
			EventAt: &c.EventAt, Status: "active",
		}
		if ref := refs[i]; ref.ExistingID != nil {
			memories[i].TopicID = ref.ExistingID
		} else if ref.NewName != "" {
			id := nameToID[ref.NewName]
			memories[i].TopicID = &id
		}
	}
	if err := d.Memories.InsertExt(ctx, tx, memories); err != nil {
		return fmt.Errorf("写 memory: %w", err)
	}

	// 4. todo 入库（继承来源 memory 的 topic 归属）
	var todos []*repo.Todo
	for i, c := range gated {
		if !c.IsTodo || c.TodoStatus == "" {
			continue
		}
		todos = append(todos, &repo.Todo{
			Title: c.Title, SourceMemoryID: &memories[i].ID,
			TopicID: memories[i].TopicID, Status: c.TodoStatus,
			DueAt: c.TodoDue, Confidence: c.Confidence,
		})
	}
	if err := d.Todos.InsertExt(ctx, tx, todos); err != nil {
		return fmt.Errorf("写 todo: %w", err)
	}

	return tx.Commit()
}

// msSince 返回自 begin 以来的毫秒数（trace 记录用）。
func msSince(begin time.Time) int64 {
	return time.Since(begin).Milliseconds()
}
