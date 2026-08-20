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
		appendTrace(j, repo.TraceEntry{
			Stage: "extract:llm", Model: d.LLMModel, MS: msSince(llmBegin),
			Tokens: ex.Stats().Tokens, Windows: ex.Stats().Windows,
			PromptVersion: d.PromptVersion,
		})

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

// commitExtract 在单事务内完成幂等清理与落库（多对多版）。
// 顺序：快照手动关联(source=user) → 删 todo_topic → 删 todo → 删 memory_topic → 删 memory
// → 建建议 topic → 插 memory + memory_topic(ai) + 重链 user → 插 todo + todo_topic(ai) + 重链 user。
// topic 归属仅写关联表 memory_topic/todo_topic，legacy topic_id 列已无人写（T6 删列）。
func commitExtract(ctx context.Context, d StageDeps, sessionID ids.ID, userID int64,
	gated []memory.Candidate, refs [][]memory.TopicRef, newNames []string) error {

	tx, err := d.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后 Rollback 是 no-op

	// 1. 快照手动关联（按自然键 K），source='user' 行稍后按 K 重链
	memSnap := map[string][]ids.ID{}
	todoSnap := map[string][]ids.ID{}
	if links, err := d.MemoryTopics.SnapshotUserBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("快照 memory 手动关联: %w", err)
	} else {
		for _, l := range links {
			k := memory.NaturalKey(l.SegmentIDs, l.Title)
			memSnap[k] = append(memSnap[k], l.TopicID)
		}
	}
	if links, err := d.TodoTopics.SnapshotUserBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("快照 todo 手动关联: %w", err)
	} else {
		for _, l := range links {
			k := memory.NaturalKey(l.SegmentIDs, l.Title)
			todoSnap[k] = append(todoSnap[k], l.TopicID)
		}
	}

	// 2. 幂等清理（关联表依赖主表行存在，须先删关联再删主表）
	if err := d.TodoTopics.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 todo_topic: %w", err)
	}
	if err := d.Todos.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 todo: %w", err)
	}
	if err := d.MemoryTopics.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 memory_topic: %w", err)
	}
	if err := d.Memories.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 memory: %w", err)
	}

	// 3. 新建建议 topic（事务内同名查重兜底，沿用 Sprint 2 §3.5）
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

	// resolveTopicID 把单条 ref 折算成最终 topic id（ExistingID 直用，NewName 查 nameToID）
	resolveTopicID := func(ref memory.TopicRef) (ids.ID, bool) {
		if ref.ExistingID != nil {
			return *ref.ExistingID, true
		}
		if id, ok := nameToID[ref.NewName]; ok {
			return id, true
		}
		return 0, false
	}

	// 4. memory + memory_topic(ai) + 重链 user
	memories := make([]*repo.Memory, len(gated))
	resolvedTids := make([][]ids.ID, len(gated)) // 每候选 resolved topic ids（去重有序）
	for i, c := range gated {
		seen := map[ids.ID]bool{}
		for _, ref := range refs[i] {
			if id, ok := resolveTopicID(ref); ok && !seen[id] {
				seen[id] = true
				resolvedTids[i] = append(resolvedTids[i], id)
			}
		}
		memories[i] = &repo.Memory{
			Type: c.Type, Title: c.Title, Content: c.Content,
			EpistemicType: c.EpistemicType,
			Importance: c.Importance, Confidence: c.Confidence,
			SessionID: sessionID, TranscriptSegmentIDs: ids.List(c.SegmentIDs),
			EventAt: &c.EventAt, Status: "active",
		}
	}
	if err := d.Memories.InsertExt(ctx, tx, memories); err != nil {
		return fmt.Errorf("写 memory: %w", err)
	}
	var memTopicRows []*repo.MemoryTopicLink
	for i, c := range gated {
		k := memory.NaturalKey(c.SegmentIDs, c.Title)
		seen := map[ids.ID]bool{}
		for _, tid := range resolvedTids[i] {
			seen[tid] = true
			memTopicRows = append(memTopicRows, &repo.MemoryTopicLink{MemoryID: memories[i].ID, TopicID: tid, Source: "ai"})
		}
		for _, tid := range memSnap[k] {
			if !seen[tid] {
				memTopicRows = append(memTopicRows, &repo.MemoryTopicLink{MemoryID: memories[i].ID, TopicID: tid, Source: "user"})
			}
		}
	}
	if err := d.MemoryTopics.InsertExt(ctx, tx, memTopicRows); err != nil {
		return fmt.Errorf("写 memory_topic: %w", err)
	}

	// 落库去重：新 suggested todo 若归一化标题命中已有未关闭 todo（或批内已加），跳过。
	// 必须在 tx 内读（ListOpenTitlesExt 传 tx），以看到上文 DeleteBySessionExt 已删的
	// 本 session todo，避免重跑时旧 todo 自去重导致幂等失败。
	openTitles, err := d.Todos.ListOpenTitlesExt(ctx, tx, userID)
	if err != nil {
		return fmt.Errorf("读 open todo 标题: %w", err)
	}
	dupSet := map[string]bool{}
	for _, ti := range openTitles {
		dupSet[repo.NormalizeTitle(ti)] = true
	}

	// 5. todo + todo_topic(ai) + 重链 user
	var todos []*repo.Todo
	type todoPlan struct {
		tids []ids.ID
		key  string
	}
	plans := make([]todoPlan, 0)
	for i, c := range gated {
		if !c.IsTodo || c.TodoStatus == "" {
			continue
		}
		nk := repo.NormalizeTitle(c.Title)
		if dupSet[nk] {
			continue // 命中已有未关闭 todo，跳过（memory/关联仍由上文处理）
		}
		dupSet[nk] = true // 批内去重
		td := &repo.Todo{
			Title: c.Title, SourceMemoryID: &memories[i].ID,
			Status: c.TodoStatus, DueAt: c.TodoDue, Confidence: c.Confidence,
		}
		todos = append(todos, td)
		plans = append(plans, todoPlan{tids: resolvedTids[i], key: memory.NaturalKey(c.SegmentIDs, c.Title)})
	}
	if err := d.Todos.InsertExt(ctx, tx, todos); err != nil {
		return fmt.Errorf("写 todo: %w", err)
	}
	var todoTopicRows []*repo.TodoTopicLink
	for j, td := range todos {
		seen := map[ids.ID]bool{}
		for _, tid := range plans[j].tids {
			seen[tid] = true
			todoTopicRows = append(todoTopicRows, &repo.TodoTopicLink{TodoID: td.ID, TopicID: tid, Source: "ai"})
		}
		for _, tid := range todoSnap[plans[j].key] {
			if !seen[tid] {
				todoTopicRows = append(todoTopicRows, &repo.TodoTopicLink{TodoID: td.ID, TopicID: tid, Source: "user"})
			}
		}
	}
	if err := d.TodoTopics.InsertExt(ctx, tx, todoTopicRows); err != nil {
		return fmt.Errorf("写 todo_topic: %w", err)
	}

	return tx.Commit()
}

// msSince 返回自 begin 以来的毫秒数（trace 记录用）。
func msSince(begin time.Time) int64 {
	return time.Since(begin).Milliseconds()
}
