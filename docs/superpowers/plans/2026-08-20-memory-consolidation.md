# 记忆合并/更新 + 置信度演化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: 用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现。Steps use `- [ ]`。逐任务串行（不并行 implementer）。每步跑对应命令验证后再勾。

**Goal:** D1 抽取时佐证去重（跨 session 近重复=佐证，上调已有记忆置信度+迁移 topic 关联）+ D2 手动 LLM 记忆整理（合并组+关系判定→用户确认→单事务落库，confidence 由规则算）+ 前端整理 UI。

**Architecture:** 见 `docs/superpowers/specs/2026-08-20-memory-consolidation-design.md`。D1 在 `commitExtract` 单事务内：`ListActiveTitlesExt`（tx 内读，避开本 session 自去重）→ 归一化标题命中已有 active 记忆则跳过插行、`BumpConfidenceExt`(+0.05 封顶 0.99)、候选 topic `INSERT IGNORE` 并入 old memory；佐证处理延迟到 kept 插入后（批内 canonical 此刻才落库）。D2 后端：`ListActive` → LLM（只判 merges+adjustments 关系，不给数字）→ 容错解析回传；`ApplyConsolidation` 单事务先 merges（member 的 memory_topic 迁 canonical、member 置 superseded）后 adjustments（跳过已 supersede 的 member，按 corroborate/contradict/outdated 规则 SQL 原子算 confidence）。前端仿 T8 topic 智能合并：记忆整理按钮 + 草稿面板（合并组可改 canonical/勾选 member + 调整项勾选保留）→ 确认 → merge → 刷新。

**Tech Stack:** Go+chi+sqlx+MySQL、Vue 3 CDN。测试：`make test`（纯逻辑）/ `make test-integration`（DB）。集成单测快路径：`make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run <TestName> ./internal/<pkg> -v`。`node --check web/app.js` 验前端语法；`make hash-web` 重算 app.js hash。

**DAG:** T1(D1 repo)→T2(D1 pipeline，依赖 T1) → T3(D2 repo)→T4(D2 prompt)→T5(D2 api+wiring，依赖 T3+T4) → T6(D2 前端，依赖 T5)。

---

## Task 1 (D1.2/D1.4): MemoryRepo 佐证去重方法 + InsertExt 尊重预置 id

**Files:** Modify `internal/repo/memory.go`、`internal/repo/memory_test.go`

**为何改 InsertExt：** D1 批内去重要在 `InsertExt(kept)` 之前就知道新记忆的 id（`memDupSet[nk]=新记忆id` 供后续同标题候选命中佐证）。故 kept 候选预生成 `ids.New()`，`InsertExt` 改为「仅当 id==0 才生成」，向后兼容（现有调用方传 id=0 → 照常生成）。

- [ ] **Step 1: 写失败测试** — `internal/repo/memory_test.go` 末尾追加（需 `math`，加入 import）：

```go
func TestMemoryListActiveTitlesExt(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil { t.Fatal(err) }
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()
	now := time.Now()
	sid := ids.New()
	ms := []*Memory{
		{Type: "fact", Title: "学Rust", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: sid, EventAt: &now, Status: "active"},
		{Type: "fact", Title: "学Go", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: sid, EventAt: &now, Status: "active"},
		{Type: "fact", Title: "学Python", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: sid, EventAt: &now, Status: "superseded"},
	}
	if err := mr.InsertExt(ctx, db, ms); err != nil { t.Fatal(err) }
	rows, err := mr.ListActiveTitlesExt(ctx, db, 1)
	if err != nil { t.Fatal(err) }
	got := map[string]bool{}
	for _, r := range rows { got[r.Title] = true }
	if !got["学Rust"] || !got["学Go"] || got["学Python"] {
		t.Fatalf("ListActiveTitlesExt = %v, want 学Rust+学Go（不含 superseded）", got)
	}
}

func TestMemoryBumpConfidence(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil { t.Fatal(err) }
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()
	now := time.Now()
	lo := &Memory{Type: "fact", Title: "佐证Bump低", Content: "x", EpistemicType: "observed", Confidence: 0.80, SessionID: ids.New(), EventAt: &now, Status: "active"}
	hi := &Memory{Type: "fact", Title: "佐证Bump高", Content: "x", EpistemicType: "observed", Confidence: 0.97, SessionID: ids.New(), EventAt: &now, Status: "active"}
	if err := mr.InsertExt(ctx, db, []*Memory{lo, hi}); err != nil { t.Fatal(err) }
	// 0.80 + 0.05 → 0.85
	if err := mr.BumpConfidenceExt(ctx, db, lo.ID, 0.05); err != nil { t.Fatal(err) }
	got, _ := mr.Get(ctx, lo.ID)
	if math.Abs(got.Confidence-0.85) > 0.001 { t.Fatalf("confidence = %v, want 0.85", got.Confidence) }
	// 0.97 + 0.05 → 封顶 0.99（不超）
	if err := mr.BumpConfidenceExt(ctx, db, hi.ID, 0.05); err != nil { t.Fatal(err) }
	gotHi, _ := mr.Get(ctx, hi.ID)
	if math.Abs(gotHi.Confidence-0.99) > 0.001 { t.Fatalf("confidence = %v, want 0.99（封顶）", gotHi.Confidence) }
}
```

> import 块（`internal/repo/memory_test.go` 现为 `context/testing/time/zhiwei/internal/ids`）加 `"math"`：
> ```go
> import (
> 	"context"
> 	"math"
> 	"testing"
> 	"time"
> 	"zhiwei/internal/ids"
> )
> ```

- [ ] **Step 2: 跑确认失败** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestMemoryListActiveTitlesExt|TestMemoryBumpConfidence' ./internal/repo -v` → FAIL `undefined: ListActiveTitlesExt` / `undefined: BumpConfidenceExt`。

- [ ] **Step 3: 实现 repo 方法** — `internal/repo/memory.go`，在 `DeleteBySessionExt` 后、`Get` 前插入两个方法：

```go
// ListActiveTitlesExt 返回该用户全部 active memory 的 id 与标题（D1 佐证去重比对用）。
// 事务内调用传 tx（能看到本事务内 DeleteBySessionExt 已删的本 session 旧 memory，
// 避免重跑时本 session 旧记忆自去重导致幂等失败），事务外调用传 r.DB。
// 与 TodoRepo.ListOpenTitlesExt 同构（T3）。
func (r *MemoryRepo) ListActiveTitlesExt(ctx context.Context, q QueryerContext, userID int64) ([]struct {
	ID    ids.ID
	Title string
}, error) {
	var rows []struct {
		ID    ids.ID `db:"id"`
		Title string `db:"title"`
	}
	err := q.SelectContext(ctx, &rows,
		`SELECT id, title FROM memory WHERE user_id = ? AND status = 'active'`, userID)
	return rows, err
}

// BumpConfidenceExt 原子上调 memory 置信度（佐证 +delta，封顶 0.99）。
// SQL 原子算术（LEAST），不读-改-写，满足并发安全约束。ext 传 tx 即加入事务。
func (r *MemoryRepo) BumpConfidenceExt(ctx context.Context, ext ExecerContext, id ids.ID, delta float64) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE memory SET confidence = LEAST(confidence + ?, 0.99) WHERE id = ?`, delta, id.Int64())
	return err
}
```

- [ ] **Step 4: 改 InsertExt 尊重预置 id** — `internal/repo/memory.go` 的 `InsertExt`（`:55-73`），把：

```go
	for i := range ms {
		ms[i].ID = ids.New()
		if ms[i].UserID == 0 {
			ms[i].UserID = 1
		}
	}
```

改为：

```go
	for i := range ms {
		if ms[i].ID == 0 { // 尊重调用方预置 id（D1 佐证去重需在插入前知道新记忆 id）
			ms[i].ID = ids.New()
		}
		if ms[i].UserID == 0 {
			ms[i].UserID = 1
		}
	}
```

并把方法注释「ID 在此生成并回填」改为「ID 在此生成（若调用方未预置）并回填」。

- [ ] **Step 5: 跑通过** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestMemoryListActiveTitlesExt|TestMemoryBumpConfidence|TestMemoryInsertAndQuery|TestMemoryDeleteBySession|TestMemoryListSince|TestMemoryListWithTopics' ./internal/repo -v` → 全 PASS（InsertExt 改动向后兼容，既有 repo 测试应仍绿）。

- [ ] **Step 6: 提交** — `git add internal/repo/memory.go internal/repo/memory_test.go && git commit -m "feat: MemoryRepo ListActiveTitlesExt+BumpConfidenceExt(D1 佐证去重) + InsertExt 尊重预置 id"`

---

## Task 2 (D1.3): commitExtract 佐证去重 + todo 守卫

**Files:** Modify `internal/pipeline/stage_extract.go`、`internal/pipeline/stage_extract_test.go`

佐证处理**延迟到 kept 插入后**：批内 canonical 在 `InsertExt(kept)` 之前尚未落库，先 bump 会命中 0 行；跨 session canonical 本就在库，延后处理同样正确。故决策循环只「记录佐证（canonID + tids）」，`InsertExt(kept)` 后统一 bump+迁 topic。todo 段对佐证跳过的候选（`memories[i]==nil`）`continue` 不产 todo（其语义由 canonical old memory 承载，见 spec D1.3 注4「可接受」）。

- [ ] **Step 1: 写失败测试** — `internal/pipeline/stage_extract_test.go` 末尾追加 fake LLM 与测试：

```go
// fakeCorroborateLLM 专供 D1 佐证去重测试：产出 1 条候选「学 Rust」(is_todo，标题归一后
// ="学rust"，与预置 active memory「学Rust」撞) + 建议新主题「Rust 进阶（佐证fixture）」。
// 独立于 fakeExtractLLM，避免污染其它 extract 测试。
type fakeCorroborateLLM struct{}

func (f *fakeCorroborateLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{Content: `{"candidates":[
	  {"type":"fact","title":"学 Rust","content":"用户在学 Rust 打算三个月读完一本书",
	   "epistemic_type":"observed","importance":0.7,"confidence":0.9,
	   "is_todo":true,"todo_due":null,"topics":[{"suggested_name":"Rust 进阶（佐证fixture）"}],"block_index":1}
	]}`, TotalTokens: 500}, nil
}

// TestStageExtractMemoryCorroboration 验证 D1：预置 active memory「学Rust」(confidence 0.80)，
// 新 session 抽取候选「学 Rust」(归一后同为 学rust) → 不增 memory 行、旧 memory confidence=0.85、
// 旧 memory 获候选 topic 关联；且佐证候选(is_todo)不产 todo（todo 守卫）。
// 必须用不同 session 预置 old memory（本 session 旧 memory 已在 tx 内被 DeleteBySessionExt 删）。
func TestStageExtractMemoryCorroboration(t *testing.T) {
	llm := &fakeCorroborateLLM{}
	d := newExtractDeps(t, llm)
	sidB, _ := setupExtractFixture(t, &d) // session B：被抽取会话（含预清理）
	ctx := context.Background()

	// 预清理：脏库重跑时残留的 active「学Rust」记忆 / open「学 Rust」todo /
	// 「Rust 进阶（佐证fixture）」topic 会让断言不稳，先统一 dismiss/delete。
	if _, err := d.Memories.DB.ExecContext(ctx,
		`UPDATE memory SET status='dismissed' WHERE user_id=1 AND title='学Rust' AND status='active'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Todos.DB.ExecContext(ctx,
		`UPDATE todo SET status='dismissed' WHERE user_id=1 AND title='学 Rust' AND status IN ('suggested','confirmed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Topics.DB.ExecContext(ctx,
		`UPDATE topic SET status='dismissed' WHERE user_id=1 AND name='Rust 进阶（佐证fixture）' AND status IN ('active','suggested')`); err != nil {
		t.Fatal(err)
	}

	// 独立 session A：预置 active memory「学Rust」(confidence 0.80)，不挂 topic。
	// 必须用不同 session：本 session 旧 memory 已在 tx 内 DeleteBySessionExt 删，tx 内读不到。
	sidA := ids.New()
	if err := d.Sessions.Create(ctx, &repo.AudioSession{
		ID: sidA, Source: "web_upload", Filename: "corr.wav",
		StoragePath: "/tmp/corr.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	oldMem := &repo.Memory{
		Type: "fact", Title: "学Rust", Content: "用户在学 Rust",
		EpistemicType: "observed", Importance: 0.7, Confidence: 0.80,
		SessionID: sidA, Status: "active",
	}
	if err := d.Memories.InsertExt(ctx, d.DB, []*repo.Memory{oldMem}); err != nil {
		t.Fatal(err)
	}

	// 跑 extract（session B）——候选「学 Rust」命中 old memory「学Rust」(归一 = 学rust)
	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sidB, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sidB); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// 断言 1：session B 无新 memory 行（候选被佐证跳过，未插）
	mems, _ := d.Memories.ListBySession(ctx, sidB)
	if len(mems) != 0 {
		t.Fatalf("session B memories = %d, want 0（候选佐证并入 old memory，不增行）", len(mems))
	}
	// 断言 2：old memory confidence 0.80 → 0.85（佐证 +0.05）
	got, _ := d.Memories.Get(ctx, oldMem.ID)
	if math.Abs(got.Confidence-0.85) > 0.001 {
		t.Fatalf("old memory confidence = %v, want 0.85", got.Confidence)
	}
	// 断言 3：old memory 获候选的 topic 关联（Rust 进阶（佐证fixture））
	links, _ := d.MemoryTopics.ListByMemoryIDs(ctx, []ids.ID{oldMem.ID})
	hitTopic := false
	for _, ti := range links[oldMem.ID] {
		if ti.Name == "Rust 进阶（佐证fixture）" {
			hitTopic = true
		}
	}
	if !hitTopic {
		t.Fatalf("old memory topics = %+v, want 含 Rust 进阶（佐证fixture）", links[oldMem.ID])
	}
	// 断言 4：佐证候选(is_todo)不产 todo（守卫 memories[i]==nil → continue）
	todos, _ := d.Todos.ListBySession(ctx, sidB)
	if len(todos) != 0 {
		t.Fatalf("session B todos = %d, want 0（佐证跳过的候选不产 todo）", len(todos))
	}
}
```

> 需在 `stage_extract_test.go` import 块加 `"math"`（现为 `context/encoding/json/fmt/testing/zhiwei/internal/ids/zhiwei/internal/memory/zhiwei/internal/provider/zhiwei/internal/repo`）：
> ```go
> import (
> 	"context"
> 	"encoding/json"
> 	"fmt"
> 	"math"
> 	"testing"
> 	"zhiwei/internal/ids"
> 	"zhiwei/internal/memory"
> 	"zhiwei/internal/provider"
> 	"zhiwei/internal/repo"
> )
> ```

- [ ] **Step 2: 跑确认失败** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run TestStageExtractMemoryCorroboration ./internal/pipeline -v` → FAIL（候选未佐证：session B memories=1 not 0；或 todo 段 `memories[i]` 解引用 nil panic）。

- [ ] **Step 3: 改造 commitExtract 的 memory 段** — `internal/pipeline/stage_extract.go`，把「4. memory + memory_topic(ai) + 重链 user」整段（即 `// 4. memory + memory_topic(ai) + 重链 user` 注释起到 `if err := d.MemoryTopics.InsertExt(ctx, tx, memTopicRows); err != nil {` 的 `}` 止，约 `:157-195`）替换为：

```go
	// 4. memory + memory_topic(ai) + 重链 user（含 D1 佐证去重）
	// D1：跨 session 近重复 = 同一事实被再次提及 = 佐证。新候选若归一化标题命中
	// 已有 active 记忆（或批内已加候选），不增行、上调 canonical 置信度(+0.05 封顶
	// 0.99)、并把候选 topic 关联并到 canonical。佐证处理延迟到 kept 插入后——批内
	// canonical 此刻才落库，先 bump 会命中 0 行。必须 tx 内读（ListActiveTitlesExt
	// 传 tx）：本事务已 DeleteBySessionExt 删了本 session 旧 memory，tx 内读看不到它们，
	// 避免重跑时本 session 旧记忆自去重致幂等失败（跨 session 旧记忆仍会命中→佐证，可接受）。
	activeTitles, err := d.Memories.ListActiveTitlesExt(ctx, tx, userID)
	if err != nil {
		return fmt.Errorf("读 active memory 标题: %w", err)
	}
	memDupSet := map[string]ids.ID{} // normTitle → 已有/批内 canonical memory id
	for _, at := range activeTitles {
		memDupSet[repo.NormalizeTitle(at.Title)] = at.ID
	}
	memories := make([]*repo.Memory, len(gated)) // 按候选下标；佐证跳过位为 nil
	resolvedTids := make([][]ids.ID, len(gated))  // 每候选 resolved topic ids（去重有序）
	type corroboration struct {
		canonID ids.ID
		tids    []ids.ID
	}
	corroborations := make([]corroboration, 0) // 延迟到 kept 插入后处理
	var kept []*repo.Memory
	for i, c := range gated {
		seen := map[ids.ID]bool{}
		for _, ref := range refs[i] {
			if id, ok := resolveTopicID(ref); ok && !seen[id] {
				seen[id] = true
				resolvedTids[i] = append(resolvedTids[i], id)
			}
		}
		nk := repo.NormalizeTitle(c.Title)
		if canonID, hit := memDupSet[nk]; hit {
			// 命中已有 active 记忆或批内已加候选：不增行，记录佐证（延迟处理）
			corroborations = append(corroborations, corroboration{canonID: canonID, tids: resolvedTids[i]})
			continue
		}
		// 未命中：建新 memory，预生成 id（供批内去重 dupSet 与延迟佐证引用；InsertExt 尊重非零 id）
		m := &repo.Memory{
			ID: ids.New(),
			Type: c.Type, Title: c.Title, Content: c.Content,
			EpistemicType: c.EpistemicType,
			Importance: c.Importance, Confidence: c.Confidence,
			SessionID: sessionID, TranscriptSegmentIDs: ids.List(c.SegmentIDs),
			EventAt: &c.EventAt, Status: "active",
		}
		memories[i] = m
		kept = append(kept, m)
		memDupSet[nk] = m.ID // 批内去重：后续同标题候选命中此 id
	}
	if err := d.Memories.InsertExt(ctx, tx, kept); err != nil {
		return fmt.Errorf("写 memory: %w", err)
	}
	// 佐证处理（kept 已插入，批内 canonical 现已存在；跨 session canonical 本就在库）：
	// 上调 canonical 置信度 + 把候选 topic 关联并入 canonical（INSERT IGNORE，PK 去重）
	for _, cor := range corroborations {
		if err := d.Memories.BumpConfidenceExt(ctx, tx, cor.canonID, 0.05); err != nil {
			return fmt.Errorf("佐证上调 memory %s: %w", cor.canonID, err)
		}
		var rows []*repo.MemoryTopicLink
		for _, tid := range cor.tids {
			rows = append(rows, &repo.MemoryTopicLink{MemoryID: cor.canonID, TopicID: tid, Source: "ai"})
		}
		if err := d.MemoryTopics.InsertExt(ctx, tx, rows); err != nil {
			return fmt.Errorf("并入候选 topic 到 memory %s: %w", cor.canonID, err)
		}
	}
	var memTopicRows []*repo.MemoryTopicLink
	for i, c := range gated {
		if memories[i] == nil {
			continue // 佐证跳过的候选不产生常规 memory_topic 行（其 topic 已并入 canonical）
		}
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
```

- [ ] **Step 4: 改造 todo 段（重命名 dupSet + 守卫）** — 同文件，把「落库去重」到「5. todo + todo_topic」段中 todo 循环（约 `:197-231`）。先把：

```go
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
```

改为（`dupSet`→`todoDupSet` 避免与 `memDupSet` 混淆；加佐证候选守卫）：

```go
	todoDupSet := map[string]bool{}
	for _, ti := range openTitles {
		todoDupSet[repo.NormalizeTitle(ti)] = true
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
		if memories[i] == nil {
			continue // D1 佐证跳过的候选不产 todo（其语义由 canonical old memory 承载，spec D1.3 注4「可接受」）
		}
		nk := repo.NormalizeTitle(c.Title)
		if todoDupSet[nk] {
			continue // 命中已有未关闭 todo，跳过（memory/关联仍由上文处理）
		}
		todoDupSet[nk] = true // 批内去重
		td := &repo.Todo{
			Title: c.Title, SourceMemoryID: &memories[i].ID,
			Status: c.TodoStatus, DueAt: c.TodoDue, Confidence: c.Confidence,
		}
		todos = append(todos, td)
		plans = append(plans, todoPlan{tids: resolvedTids[i], key: memory.NaturalKey(c.SegmentIDs, c.Title)})
	}
```

- [ ] **Step 5: 跑通过** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run TestStageExtractMemoryCorroboration ./internal/pipeline -v` → PASS。

- [ ] **Step 6: 回归既有 extract 测试** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestStageExtract' ./internal/pipeline -v` → 全 PASS（Commit/Idempotent/DedupTodoByTitle 等不受 D1 影响：它们的候选标题不命中预置 active memory，dupSet 为空，行为同前）。

- [ ] **Step 7: 提交** — `git add internal/pipeline/stage_extract.go internal/pipeline/stage_extract_test.go && git commit -m "feat: commitExtract 抽取时佐证去重(跨 session 近重复=佐证,上调置信度+迁移topic)"`

---

## Task 3 (D2.2): MemoryRepo ListActive + ApplyConsolidation + 类型

**Files:** Modify `internal/repo/memory.go`、`internal/repo/memory_test.go`

- [ ] **Step 1: 写失败测试** — `internal/repo/memory_test.go` 追加（`math` 已在 T1 加入）：

```go
func TestMemoryListActive(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil { t.Fatal(err) }
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()
	now := time.Now()
	a := &Memory{Type: "fact", Title: "整理ListA", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: ids.New(), EventAt: &now, Status: "active"}
	s := &Memory{Type: "fact", Title: "整理ListS", Content: "x", EpistemicType: "observed", Confidence: 0.8, SessionID: ids.New(), EventAt: &now, Status: "superseded"}
	if err := mr.InsertExt(ctx, db, []*Memory{a, s}); err != nil { t.Fatal(err) }
	rows, err := mr.ListActive(ctx, 1, 500)
	if err != nil { t.Fatal(err) }
	got := map[string]bool{}
	for _, m := range rows { got[m.Title] = true }
	if !got["整理ListA"] || got["整理ListS"] {
		t.Fatalf("ListActive = %v, want 含整理ListA 不含 superseded", got)
	}
}

func TestMemoryApplyConsolidation(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil { t.Fatal(err) }
	mr := &MemoryRepo{DB: db}
	mtr := &MemoryTopicRepo{DB: db}
	tr := &TopicRepo{DB: db}
	ctx := context.Background()
	_, _ = db.ExecContext(ctx, `UPDATE topic SET status='dismissed' WHERE user_id=1 AND name IN (?,?) AND status IN ('active','suggested')`, "整理靶主题", "整理源主题")
	_, _ = db.ExecContext(ctx, `DELETE FROM memory WHERE title IN (?,?)`, "整理A记忆", "整理B记忆")
	now := time.Now()
	a := &Memory{Type: "fact", Title: "整理A记忆", Content: "A", EpistemicType: "observed", Confidence: 0.80, SessionID: ids.New(), EventAt: &now, Status: "active"}
	b := &Memory{Type: "fact", Title: "整理B记忆", Content: "B", EpistemicType: "observed", Confidence: 0.80, SessionID: ids.New(), EventAt: &now, Status: "active"}
	if err := mr.InsertExt(ctx, db, []*Memory{a, b}); err != nil { t.Fatal(err) }
	x := &Topic{Name: "整理靶主题", Status: "active", CreatedBy: "ai"}
	y := &Topic{Name: "整理源主题", Status: "active", CreatedBy: "ai"}
	_ = tr.Create(ctx, x)
	_ = tr.Create(ctx, y)
	_ = mtr.AddLink(ctx, a.ID, x.ID)
	_ = mtr.AddLink(ctx, b.ID, y.ID)

	// merges 优先：B 被 merge 置 superseded；adjustment 指向 B 应被跳过 → adjusted=0
	merged, adjusted, err := mr.ApplyConsolidation(ctx, ConsolidationReq{
		Merges: []MemoryMerge{{CanonicalID: a.ID, MemberIDs: []ids.ID{a.ID, b.ID}}},
		Adjustments: []MemoryAdjustment{{MemoryID: b.ID, Kind: "corroborate"}},
	})
	if err != nil { t.Fatal(err) }
	if merged != 1 || adjusted != 0 {
		t.Fatalf("merged=%d adjusted=%d, want 1/0（B 被 merge supersede，adjustment 跳过）", merged, adjusted)
	}
	bGot, _ := mr.Get(ctx, b.ID)
	if bGot.Status != "superseded" { t.Fatalf("B status=%s, want superseded", bGot.Status) }
	// A 聚合 X+Y（B 的 Y 迁来）
	aLinks, _ := mtr.ListByMemoryIDs(ctx, []ids.ID{a.ID})
	names := map[string]bool{}
	for _, ti := range aLinks[a.ID] { names[ti.Name] = true }
	if !names["整理靶主题"] || !names["整理源主题"] {
		t.Fatalf("A topics=%v, want 含整理靶主题+整理源主题", names)
	}
	// B 的 memory_topic 已删
	bLinks, _ := mtr.ListByMemoryIDs(ctx, []ids.ID{b.ID})
	if len(bLinks[b.ID]) != 0 { t.Fatalf("B topic 关联=%d, want 0（已迁删）", len(bLinks[b.ID])) }
}
```

- [ ] **Step 2: 跑确认失败** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestMemoryListActive|TestMemoryApplyConsolidation' ./internal/repo -v` → FAIL `undefined: ListActive` / `ApplyConsolidation` / `ConsolidationReq` 等。

- [ ] **Step 3: 实现类型与方法** — `internal/repo/memory.go`，在 `attachTopics` 后追加：

```go
// ListActive 返回该用户全部 active 记忆（排除 superseded/dismissed），按 event_at 倒序，
// 供 D2 整理 LLM 输入。limit 上限保护（默认/上限 500）。
func (r *MemoryRepo) ListActive(ctx context.Context, userID int64, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var rows []Memory
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM memory WHERE user_id = ? AND status = 'active' ORDER BY event_at DESC LIMIT ?`,
		userID, limit)
	return rows, err
}

// ConsolidationReq 是 D2 整理落库请求（用户编辑后的 LLM 提议）。
type ConsolidationReq struct {
	Merges      []MemoryMerge      `json:"merges"`
	Adjustments []MemoryAdjustment `json:"adjustments"`
}

// MemoryMerge：语义同一条事实的组。CanonicalID 保留 active，MemberIDs 并入后置 superseded。
type MemoryMerge struct {
	CanonicalID ids.ID   `json:"canonical_id"`
	MemberIDs   []ids.ID `json:"member_ids"`
}

// MemoryAdjustment：每条记忆的关系判定 + 理由 + 证据 memory id。
// Kind: corroborate(被佐证更可信)|contradict(被新信息否定)|outdated(被新信息取代应 superseded)。
type MemoryAdjustment struct {
	MemoryID    ids.ID   `json:"memory_id"`
	Kind        string   `json:"kind"`
	Reason      string   `json:"reason"`
	EvidenceIDs []ids.ID `json:"evidence_ids"`
}

// ApplyConsolidation 单事务落库整理：先 merges（member 的 memory_topic 关联迁到 canonical、
// 删 member 关联、member 置 superseded），后 adjustments（跳过已被 merge 置 superseded 的
// member；对其余 active 按 kind 规则算 confidence，SQL 原子）。merges 优先避免重复处理。
// 返回 (被 supersede 的 member 数, 应用的 confidence 调整数)。LLM 只判关系，confidence 数字
// 由规则算（可审计可复现）。
func (r *MemoryRepo) ApplyConsolidation(ctx context.Context, req ConsolidationReq) (merged, adjusted int, err error) {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 先 merges：记录被 supersede 的 member，adjustments 跳过它们
	superseded := map[ids.ID]bool{}
	for _, g := range req.Merges {
		canon := g.CanonicalID
		for _, mid := range g.MemberIDs {
			if mid == canon {
				continue
			}
			// member 的 memory_topic 关联迁到 canonical（INSERT IGNORE，PK 去重）
			if _, err := tx.ExecContext(ctx,
				`INSERT IGNORE INTO memory_topic (memory_id, topic_id, source)
				 SELECT ?, topic_id, source FROM memory_topic WHERE memory_id = ?`,
				canon.Int64(), mid.Int64()); err != nil {
				return 0, 0, err
			}
			// 删 member 的 memory_topic 关联
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM memory_topic WHERE memory_id = ?`, mid.Int64()); err != nil {
				return 0, 0, err
			}
			// member 置 superseded（行保留审计）
			if _, err := tx.ExecContext(ctx,
				`UPDATE memory SET status = 'superseded' WHERE id = ?`, mid.Int64()); err != nil {
				return 0, 0, err
			}
			superseded[mid] = true
			merged++
		}
	}
	// 后 adjustments：跳过已被 merge 置 superseded 的 member，对其余 active 按 kind 算 confidence
	for _, a := range req.Adjustments {
		if superseded[a.MemoryID] {
			continue
		}
		switch a.Kind {
		case "corroborate":
			if _, err := tx.ExecContext(ctx,
				`UPDATE memory SET confidence = LEAST(confidence + 0.05, 0.99) WHERE id = ?`,
				a.MemoryID.Int64()); err != nil {
				return 0, 0, err
			}
		case "contradict":
			if _, err := tx.ExecContext(ctx,
				`UPDATE memory SET confidence = GREATEST(confidence - 0.10, 0.10) WHERE id = ?`,
				a.MemoryID.Int64()); err != nil {
				return 0, 0, err
			}
		case "outdated":
			if _, err := tx.ExecContext(ctx,
				`UPDATE memory SET status = 'superseded', confidence = GREATEST(confidence * 0.5, 0.05) WHERE id = ?`,
				a.MemoryID.Int64()); err != nil {
				return 0, 0, err
			}
		default:
			continue // 未知 kind 跳过
		}
		adjusted++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return merged, adjusted, nil
}
```

- [ ] **Step 4: 跑通过** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestMemoryListActive|TestMemoryApplyConsolidation' ./internal/repo -v` → PASS。

- [ ] **Step 5: 提交** — `git add internal/repo/memory.go internal/repo/memory_test.go && git commit -m "feat: MemoryRepo ListActive + ApplyConsolidation(D2 整理落库单事务+置信度规则)"`

---

## Task 4 (D2.3): memory_consolidate_v1.md prompt

**Files:** Create `prompts/memory_consolidate_v1.md`

- [ ] **Step 1: 写 prompt** — 创建 `prompts/memory_consolidate_v1.md`：

```markdown
# 知微 记忆整理提议（版本：memory_consolidate_v1）

你是记忆整理器。输入是该用户全部 active 记忆（JSON 数组，每项含 id/type/title/content/epistemic_type/confidence/event_at）。任务：找出①语义同一条事实的合并组（canonical_id 取其中最完整/最新的一条 id，member_ids 含其余）；②每条记忆与其它记忆的关系（corroborate=被其它佐证更可信、contradict=被新信息否定、outdated=被新信息取代应 superseded），给 reason + evidence_ids。

## 规则

1. 只判确实语义相近的；不合并/关联不同事实。
2. canonical_id 必须用输入里真实的 memory id 字符串；member_ids 同理。
3. 不直接给置信度数字——系统按 corroborate/contradict/outdated 规则算（可审计可复现）。
4. 不需要的不列。无则 merges 与 adjustments 皆空数组。

## 输出格式

只输出 JSON，无围栏。
{"merges":[{"canonical_id":"<mid>","member_ids":["<mid>",...]}],
 "adjustments":[{"memory_id":"<mid>","kind":"corroborate|contradict|outdated","reason":"...","evidence_ids":["<mid>",...]}]}
无则 {"merges":[],"adjustments":[]}。
```

- [ ] **Step 2: 验证** — `test -f prompts/memory_consolidate_v1.md && head -1 prompts/memory_consolidate_v1.md`（输出首行）。

- [ ] **Step 3: 提交** — `git add prompts/memory_consolidate_v1.md && git commit -m "feat: 记忆整理 prompt memory_consolidate_v1"`

---

## Task 5 (D2.1/D2.2/D2.4): consolidate/merge handler + 接线 + 测试

**Files:** Modify `internal/api/memory.go`、`cmd/zhiwei-server/main.go`、`internal/api/memory_test.go`

- [ ] **Step 1: 写失败测试** — `internal/api/memory_test.go` 追加（import 块加 `"fmt"`、`"math"`；现有为 `context/encoding/json/net-http/net/http/httptest/strings/testing/time/chi/ids/repo`）。先加 fixture 与两个测试：

```go
// setupMemoryConsolidateFixtures 预置 5 条 active memory：A/B 各带 1 个 topic（整理靶主题 X /
// 整理源主题 Y，验证 merge 关联迁移），C/D/E 裸 memory（confidence 0.80，验证
// corroborate/contradict/outdated 置信度演化）。名称用「整理」前缀避免与其他 fixture 混淆。
func setupMemoryConsolidateFixtures(t *testing.T) (*repo.MemoryRepo, *repo.MemoryTopicRepo, *repo.TopicRepo, *repo.Memory, *repo.Memory, *repo.Memory, *repo.Memory, *repo.Memory) {
	t.Helper()
	if err := ids.Init(1); err != nil { t.Fatal(err) }
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil { t.Fatal(err) }
	mr := &repo.MemoryRepo{DB: db}
	mtr := &repo.MemoryTopicRepo{DB: db}
	tr := &repo.TopicRepo{DB: db}
	ctx := context.Background()
	for _, name := range []string{"整理靶主题", "整理源主题"} {
		_, _ = db.ExecContext(ctx, `UPDATE topic SET status='dismissed' WHERE user_id=1 AND name=? AND status IN ('active','suggested')`, name)
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM memory WHERE title IN (?, ?, ?, ?, ?)`,
		"整理A记忆", "整理B记忆", "整理C记忆", "整理D记忆", "整理E记忆")
	eventAt := time.Now()
	mk := func(title string) *repo.Memory {
		return &repo.Memory{Type: "fact", Title: title, Content: title + "的内容描述",
			EpistemicType: "observed", Confidence: 0.80, SessionID: ids.New(), EventAt: &eventAt, Status: "active"}
	}
	a, b, c, d, e := mk("整理A记忆"), mk("整理B记忆"), mk("整理C记忆"), mk("整理D记忆"), mk("整理E记忆")
	for _, m := range []*repo.Memory{a, b, c, d, e} {
		if err := mr.InsertExt(ctx, db, []*repo.Memory{m}); err != nil { t.Fatal(err) }
	}
	x := &repo.Topic{Name: "整理靶主题", Status: "active", CreatedBy: "ai"}
	y := &repo.Topic{Name: "整理源主题", Status: "active", CreatedBy: "ai"}
	if err := tr.Create(ctx, x); err != nil { t.Fatal(err) }
	if err := tr.Create(ctx, y); err != nil { t.Fatal(err) }
	if err := mtr.AddLink(ctx, a.ID, x.ID); err != nil { t.Fatal(err) }
	if err := mtr.AddLink(ctx, b.ID, y.ID); err != nil { t.Fatal(err) }
	return mr, mtr, tr, a, b, c, d, e
}

// TestMemoryConsolidate 验证整理提议路径：fake LLM 返回 canned merges+adjustments，
// handler 调 ListActive → LLM.Chat → 容错解析 → 原样回传提议（不改库）。
// fakeConsolidateLLM 复用 topic_test.go（同包 api）。
func TestMemoryConsolidate(t *testing.T) {
	mr, mtr, tr, a, b, _, _, _ := setupMemoryConsolidateFixtures(t)
	canned := fmt.Sprintf(`{"merges":[{"canonical_id":"%s","member_ids":["%s","%s"]}],"adjustments":[{"memory_id":"%s","kind":"corroborate","reason":"B 佐证 A","evidence_ids":["%s"]}]}`,
		a.ID.String(), a.ID.String(), b.ID.String(), b.ID.String(), a.ID.String())
	r := chi.NewRouter()
	RegisterMemory(r, &MemoryHandler{
		Memories: mr, Topics: tr, MemoryTopics: mtr,
		LLM: &fakeConsolidateLLM{resp: canned}, LLMModel: "test", ConsolidatePrompt: "sys",
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/memories/consolidate", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("consolidate: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Merges []struct {
			CanonicalID string   `json:"canonical_id"`
			MemberIDs   []string `json:"member_ids"`
		} `json:"merges"`
		Adjustments []struct {
			MemoryID string `json:"memory_id"`
			Kind     string `json:"kind"`
		} `json:"adjustments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	if len(resp.Merges) != 1 || resp.Merges[0].CanonicalID != a.ID.String() {
		t.Fatalf("merges = %+v, want 1 组 canonical=%s", resp.Merges, a.ID)
	}
	if len(resp.Adjustments) != 1 || resp.Adjustments[0].MemoryID != b.ID.String() || resp.Adjustments[0].Kind != "corroborate" {
		t.Fatalf("adjustments = %+v, want 1 条 {B, corroborate}", resp.Adjustments)
	}
}

// TestMemoryMerge 验证整理落库事务：merge（A canonical，B member → B 的 topic 关联迁到 A、
// B 置 superseded）+ adjustments（corroborate +0.05 / contradict -0.10 / outdated ×0.5+superseded）。
// merges 优先：adjustments 跳过已被 merge supersede 的 member。不调 LLM（纯 DB 事务）。
func TestMemoryMerge(t *testing.T) {
	mr, mtr, tr, a, b, c, d, e := setupMemoryConsolidateFixtures(t)
	r := chi.NewRouter()
	RegisterMemory(r, &MemoryHandler{Memories: mr, Topics: tr, MemoryTopics: mtr}) // Merge 不调 LLM

	body := fmt.Sprintf(`{"merges":[{"canonical_id":"%s","member_ids":["%s","%s"]}],"adjustments":[{"memory_id":"%s","kind":"corroborate","reason":"","evidence_ids":[]},{"memory_id":"%s","kind":"contradict","reason":"","evidence_ids":[]},{"memory_id":"%s","kind":"outdated","reason":"","evidence_ids":[]}]}`,
		a.ID.String(), a.ID.String(), b.ID.String(), c.ID.String(), d.ID.String(), e.ID.String())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/memories/merge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Applied  bool `json:"applied"`
		Merged   int  `json:"merged"`
		Adjusted int  `json:"adjusted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	if !resp.Applied || resp.Merged != 1 || resp.Adjusted != 3 {
		t.Fatalf("resp = %+v, want applied merged=1 adjusted=3", resp)
	}
	ctx := context.Background()
	// A 聚合：A 原 X(整理靶主题) + B 迁来 Y(整理源主题)
	aLinks, _ := mtr.ListByMemoryIDs(ctx, []ids.ID{a.ID})
	gotTopics := map[string]bool{}
	for _, ti := range aLinks[a.ID] { gotTopics[ti.Name] = true }
	if !gotTopics["整理靶主题"] || !gotTopics["整理源主题"] {
		t.Fatalf("A topics = %+v, want 含整理靶主题+整理源主题", gotTopics)
	}
	// B superseded，B 的 memory_topic 已删
	bGot, _ := mr.Get(ctx, b.ID)
	if bGot.Status != "superseded" { t.Fatalf("B status=%s, want superseded", bGot.Status) }
	bLinks, _ := mtr.ListByMemoryIDs(ctx, []ids.ID{b.ID})
	if len(bLinks[b.ID]) != 0 { t.Fatalf("B topic 关联=%d, want 0（已迁删）", len(bLinks[b.ID])) }
	// corroborate C 0.80→0.85；contradict D 0.80→0.70；outdated E 0.80→0.40 且 superseded
	cGot, _ := mr.Get(ctx, c.ID)
	if math.Abs(cGot.Confidence-0.85) > 0.001 { t.Fatalf("C conf=%v, want 0.85", cGot.Confidence) }
	dGot, _ := mr.Get(ctx, d.ID)
	if math.Abs(dGot.Confidence-0.70) > 0.001 { t.Fatalf("D conf=%v, want 0.70", dGot.Confidence) }
	eGot, _ := mr.Get(ctx, e.ID)
	if eGot.Status != "superseded" || math.Abs(eGot.Confidence-0.40) > 0.001 {
		t.Fatalf("E = %+v, want superseded conf=0.40", eGot)
	}
}
```

> import 块改为：
> ```go
> import (
> 	"context"
> 	"encoding/json"
> 	"fmt"
> 	"math"
> 	"net/http"
> 	"net/http/httptest"
> 	"strings"
> 	"testing"
> 	"time"
> 	"github.com/go-chi/chi/v5"
> 	"zhiwei/internal/ids"
> 	"zhiwei/internal/repo"
> )
> ```

- [ ] **Step 2: 跑确认失败** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestMemoryConsolidate|TestMemoryMerge' ./internal/api -v` → FAIL（路由 `/api/memories/consolidate` 未注册 / `MemoryHandler` 无 LLM 字段）。

- [ ] **Step 3: 加 handler 字段 + 路由** — `internal/api/memory.go`。import 块加 `"zhiwei/internal/provider"`：
> ```go
> import (
> 	"encoding/json"
> 	"net/http"
> 	"strings"
> 	"time"
> 	"github.com/go-chi/chi/v5"
> 	"zhiwei/internal/ids"
> 	"zhiwei/internal/provider"
> 	"zhiwei/internal/repo"
> )
> ```
`MemoryHandler` 结构体加 3 个 LLM 字段（仿 `TopicHandler`），把：
```go
type MemoryHandler struct {
	Memories      *repo.MemoryRepo
	Topics        *repo.TopicRepo       // 校验 topic 存在
	MemoryTopics  *repo.MemoryTopicRepo // 手动加/删 memory↔topic 关联
}
```
改为：
```go
type MemoryHandler struct {
	Memories      *repo.MemoryRepo
	Topics        *repo.TopicRepo       // 校验 topic 存在
	MemoryTopics  *repo.MemoryTopicRepo // 手动加/删 memory↔topic 关联

	// LLM 用于 consolidate 提议（merge 不调 LLM）。main.go 注入；测试可传 fake。
	LLM provider.LLMProvider
	// LLMModel 是 fast 模型名（cfg.LLMFastModel）。
	LLMModel string
	// ConsolidatePrompt 是 prompts/memory_consolidate_v1.md 的内容（系统指令）。
	ConsolidatePrompt string
}
```
`RegisterMemory` 加 2 路由：
```go
func RegisterMemory(r chi.Router, h *MemoryHandler) {
	r.Get("/api/memories", h.List)
	r.Patch("/api/memories/{id}", h.Patch)
	r.Post("/api/memories/{id}/topics", h.AddTopic)
	r.Delete("/api/memories/{id}/topics/{topic_id}", h.RemoveTopic)
	r.Post("/api/memories/consolidate", h.Consolidate)
	r.Post("/api/memories/merge", h.Merge)
}
```

- [ ] **Step 4: 实现 Consolidate/Merge handler** — `internal/api/memory.go` 末尾（`RemoveTopic` 后）追加：

```go
// Consolidate 调 LLM 生成整理提议：输入该用户全部 active 记忆，输出合并组 + 每条记忆
// 的关系判定（merges + adjustments），不改库。LLM 只判关系不给置信度数字；confidence 数字
// 由 Merge 的规则（SQL 原子）算。流程：ListActive → 组 user 消息 → LLM.Chat → 容错解析 → 原样回传。
func (h *MemoryHandler) Consolidate(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		http.Error(w, "LLM 未配置", http.StatusInternalServerError)
		return
	}
	list, err := h.Memories.ListActive(r.Context(), 1, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type memItem struct {
		ID            string  `json:"id"`
		Type          string  `json:"type"`
		Title         string  `json:"title"`
		Content       string  `json:"content"`
		EpistemicType string  `json:"epistemic_type"`
		Confidence    float64 `json:"confidence"`
		EventAt       string  `json:"event_at"`
	}
	items := make([]memItem, 0, len(list))
	for _, m := range list {
		ea := ""
		if m.EventAt != nil {
			ea = m.EventAt.Format(time.RFC3339)
		}
		items = append(items, memItem{
			ID: m.ID.String(), Type: m.Type, Title: m.Title, Content: m.Content,
			EpistemicType: m.EpistemicType, Confidence: m.Confidence, EventAt: ea,
		})
	}
	userMsg, err := json.Marshal(items)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := h.LLM.Chat(r.Context(), provider.ChatRequest{
		Model:  h.LLMModel,
		System: h.ConsolidatePrompt,
		User:   string(userMsg),
	})
	if err != nil {
		http.Error(w, "LLM 调用失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 容错解析：截取首个 { 到末个 }，剥掉前后废话/markdown 围栏（与 candidate.go ParseCandidates 同思路）
	raw := strings.TrimSpace(resp.Content)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var out struct {
		Merges []struct {
			CanonicalID string   `json:"canonical_id"`
			MemberIDs   []string `json:"member_ids"`
		} `json:"merges"`
		Adjustments []struct {
			MemoryID    string   `json:"memory_id"`
			Kind        string   `json:"kind"`
			Reason      string   `json:"reason"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"adjustments"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		http.Error(w, "整理提议解析失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 原样回传提议，不改库（用户确认后走 /api/memories/merge）
	writeJSON(w, map[string]any{"merges": out.Merges, "adjustments": out.Adjustments})
}

// Merge 用户确认后单事务落库整理：body 含 merges + adjustments（id 均为字符串）。
// ids.ParseID 转 []ids.ID 组 repo.ConsolidationReq 交 ApplyConsolidation。先 merges
// （member 关联迁 canonical + member 置 superseded），后 adjustments（跳过已 supersede 的
// member，按 kind 规则算 confidence，SQL 原子）。不调 LLM。
func (h *MemoryHandler) Merge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Merges []struct {
			CanonicalID string   `json:"canonical_id"`
			MemberIDs   []string `json:"member_ids"`
		} `json:"merges"`
		Adjustments []struct {
			MemoryID    string   `json:"memory_id"`
			Kind        string   `json:"kind"`
			Reason      string   `json:"reason"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"adjustments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	cr := repo.ConsolidationReq{
		Merges:      make([]repo.MemoryMerge, 0, len(req.Merges)),
		Adjustments: make([]repo.MemoryAdjustment, 0, len(req.Adjustments)),
	}
	for _, g := range req.Merges {
		canon, err := ids.ParseID(g.CanonicalID)
		if err != nil {
			http.Error(w, "非法 canonical_id: "+g.CanonicalID, http.StatusBadRequest)
			return
		}
		mids := make([]ids.ID, 0, len(g.MemberIDs))
		for _, s := range g.MemberIDs {
			id, err := ids.ParseID(s)
			if err != nil {
				http.Error(w, "非法 member_id: "+s, http.StatusBadRequest)
				return
			}
			mids = append(mids, id)
		}
		cr.Merges = append(cr.Merges, repo.MemoryMerge{CanonicalID: canon, MemberIDs: mids})
	}
	for _, a := range req.Adjustments {
		mid, err := ids.ParseID(a.MemoryID)
		if err != nil {
			http.Error(w, "非法 memory_id: "+a.MemoryID, http.StatusBadRequest)
			return
		}
		eids := make([]ids.ID, 0, len(a.EvidenceIDs))
		for _, s := range a.EvidenceIDs {
			id, err := ids.ParseID(s)
			if err != nil {
				http.Error(w, "非法 evidence_id: "+s, http.StatusBadRequest)
				return
			}
			eids = append(eids, id)
		}
		cr.Adjustments = append(cr.Adjustments, repo.MemoryAdjustment{
			MemoryID: mid, Kind: a.Kind, Reason: a.Reason, EvidenceIDs: eids,
		})
	}
	merged, adjusted, err := h.Memories.ApplyConsolidation(r.Context(), cr)
	if err != nil {
		http.Error(w, "整理失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"applied": true, "merged": merged, "adjusted": adjusted})
}
```

- [ ] **Step 5: main.go 接线** — `cmd/zhiwei-server/main.go`。在「topic 合并 prompt」读文件块后（`consolidateBytes` 那段后）加读 memory 整理 prompt：
```go
	// memory 整理 prompt（版本化文件，MemoryHandler.Consolidate 用）
	memoryConsolidateBytes, err := os.ReadFile("prompts/memory_consolidate_v1.md")
	if err != nil {
		log.Fatal("读取记忆整理 prompt 失败: ", err)
	}
```
把 `api.RegisterMemory(r, &api.MemoryHandler{Memories: memories, Topics: topics, MemoryTopics: memoryTopics})` 改为：
```go
	api.RegisterMemory(r, &api.MemoryHandler{
		Memories: memories, Topics: topics, MemoryTopics: memoryTopics,
		LLM: llm, LLMModel: cfg.LLMFastModel, ConsolidatePrompt: string(memoryConsolidateBytes),
	})
```

- [ ] **Step 6: 跑通过** — `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestMemoryConsolidate|TestMemoryMerge|TestMemoryListAndFilter|TestMemoryPatch|TestMemoryAddRemoveTopic' ./internal/api -v` → 全 PASS（既有 memory API 测试应仍绿：未注入 LLM 字段时为 nil，Consolidate 不被它们调用）。

- [ ] **Step 7: 全量构建** — `go build ./...` → 净。

- [ ] **Step 8: 提交** — `git add internal/api/memory.go cmd/zhiwei-server/main.go internal/api/memory_test.go && git commit -m "feat: 记忆整理 consolidate(LLM提议)+merge(单事务落库+置信度演化) handler + 接线"`

---

## Task 6 (前端): 记忆整理按钮 + 草稿/确认 UI

**Files:** Modify `web/app.js`、`web/index.html`

仿 T8 topic 智能合并：member 用 `{id,name,checked}` 对齐（修掉索引错位，同 T8 做法）。整理草稿含**合并组**（下拉选 canonical + 勾选 member）+ **调整项列表**（标题 + kind + reason，勾选保留/丢弃）。canonical_id 是 memory id（用 `<select>` 选 member，非文本输入）。点按钮先 `loadMemories()`（GET /api/memories，解析全部 active 记忆标题）再 consolidate。

- [ ] **Step 1: app.js 加 memories + 整理方法** — `web/app.js`。在「---------- Topics 智能合并」段（`const mergeDraft = ref(null)` 前，约 `:252`）插入记忆整理段：

```js
    // ---------- 记忆整理（D2 LLM 提议 → 用户编辑确认 → 应用） ----------
    // 仿 T8 topic 智能合并：member 用 {id,name,checked} 对齐。canonical_id 是 memory id，
    // 用 <select> 选 member（非文本输入）。点按钮先 loadMemories 解析全部 active 记忆标题。
    const memories = ref([]);
    async function loadMemories() {
      try {
        const d = await api('GET', '/api/memories');
        memories.value = d.memories || [];
      } catch (e) { showError(e); }
    }
    const memoryDraft = ref(null); // {merges:[{canonical_id, members:[{id,name,checked}]}], adjustments:[{memory_id,title,kind,reason,evidence_ids,evidenceText,checked}]}
    async function startMemoryConsolidate() {
      try {
        await loadMemories();
        const d = await api('POST', '/api/memories/consolidate', {});
        const titleOf = id => { const m = memories.value.find(x => x.id === id); return m ? m.title : id; };
        memoryDraft.value = {
          merges: (d.merges || []).map(g => ({
            canonical_id: g.canonical_id || '',
            members: (g.member_ids || []).map(id => ({ id, name: titleOf(id), checked: true })),
          })),
          adjustments: (d.adjustments || []).map(a => ({
            memory_id: a.memory_id, title: titleOf(a.memory_id),
            kind: a.kind, reason: a.reason,
            evidence_ids: a.evidence_ids || [],
            evidenceText: (a.evidence_ids || []).map(titleOf).join('、'),
            checked: true,
          })),
        };
        if (!memoryDraft.value.merges.length && !memoryDraft.value.adjustments.length) toast.value = '暂无需要整理的记忆';
      } catch (e) { showError(e); }
    }
    function toggleMemoryMember(g, id) {
      const m = g.members.find(x => x.id === id);
      if (m) m.checked = !m.checked;
    }
    function toggleMemoryAdjustment(a) { a.checked = !a.checked; }
    // 只提交「canonical 非空 + 勾选 ≥2 member」的合并组 + 勾选的调整项
    async function applyMemoryConsolidation() {
      const d = memoryDraft.value || {};
      const merges = (d.merges || [])
        .map(g => ({ canonical_id: g.canonical_id, member_ids: g.members.filter(m => m.checked).map(m => m.id) }))
        .filter(g => g.canonical_id && g.member_ids.length >= 2);
      const adjustments = (d.adjustments || []).filter(a => a.checked)
        .map(a => ({ memory_id: a.memory_id, kind: a.kind, reason: a.reason, evidence_ids: a.evidence_ids }));
      if (!merges.length && !adjustments.length) { memoryDraft.value = null; return; }
      try {
        await api('POST', '/api/memories/merge', { merges, adjustments });
        memoryDraft.value = null;
        await reloadSession(detail.value.session.id);
        await loadMemories();
      } catch (e) { showError(e); }
    }
```

- [ ] **Step 2: app.js return 暴露** — `web/app.js` 的 return 块（约 `:355-365`），在 `topics, topicDetail, ...` 行后、`todos, ...` 行前加一行：
> 找到：
> ```js
>       loadTopics, openTopic, closeTopicDetail, confirmTopic, dismissTopic, startRename, commitRename, createTopic, suspectOf, mergeDraft, startConsolidate, toggleMergeMember, applyMerge,
>       todos, doneCollapsed, suggestedTodos, activeTodos, doneTodos,
> ```
> 改为（中间插一行）：
> ```js
>       loadTopics, openTopic, closeTopicDetail, confirmTopic, dismissTopic, startRename, commitRename, createTopic, suspectOf, mergeDraft, startConsolidate, toggleMergeMember, applyMerge,
>       memories, loadMemories, memoryDraft, startMemoryConsolidate, toggleMemoryMember, toggleMemoryAdjustment, applyMemoryConsolidation,
>       todos, doneCollapsed, suggestedTodos, activeTodos, doneTodos,
> ```

- [ ] **Step 3: index.html 加按钮 + 草稿面板** — `web/index.html`。把 memory 区标题行（约 `:111`）：
> ```html
>          <div class="todo-group-title">提取的记忆</div>
> ```
> 改为（标题 + 整理按钮，仿 Topics 列表 `:172-177`）：
> ```html
>          <div class="kv" style="margin-top:6px">
>            <div class="todo-group-title" style="margin:0">提取的记忆</div>
>            <button class="mini" @click="startMemoryConsolidate">记忆整理</button>
>          </div>
> ```
> 然后在 memory 卡片 `</template>`（约 `:133`）后、`<!-- todo 卡片 -->`（约 `:135`）前插入草稿面板：
> ```html
>        <!-- 记忆整理草稿（D2） -->
>        <div class="card" v-if="memoryDraft">
>          <div class="todo-group-title">记忆整理提议（编辑后确认）</div>
>          <div v-if="!memoryDraft.merges.length && !memoryDraft.adjustments.length" class="muted">暂无可整理项</div>
>          <div v-if="memoryDraft.merges.length" class="muted" style="margin-bottom:4px">合并组</div>
>          <div v-for="(g, gi) in memoryDraft.merges" :key="'mg'+gi" style="margin-bottom:12px; padding-bottom:10px; border-bottom:1px dashed var(--border)">
>            <select class="mini" v-model="g.canonical_id" style="margin-bottom:6px">
>              <option v-for="m in g.members" :key="m.id" :value="m.id">保留: {{ m.name }}</option>
>            </select>
>            <div v-for="m in g.members" :key="m.id" class="muted" style="margin:2px 0">
>              <label><input type="checkbox" :checked="m.checked" @change="toggleMemoryMember(g, m.id)"> {{ m.name }}</label>
>            </div>
>          </div>
>          <div v-if="memoryDraft.adjustments.length" class="muted" style="margin-bottom:4px">置信度调整</div>
>          <div v-for="(a, ai) in memoryDraft.adjustments" :key="'ma'+ai" class="muted" style="margin:2px 0">
>            <label><input type="checkbox" :checked="a.checked" @change="toggleMemoryAdjustment(a)"> {{ a.title }} · {{ a.kind }}<span v-if="a.reason"> — {{ a.reason }}</span></label>
>          </div>
>          <div style="display:flex; gap:8px; margin-top:8px">
>            <button class="primary" @click="applyMemoryConsolidation">确认整理</button>
>            <button class="mini" @click="memoryDraft = null">取消</button>
>          </div>
>        </div>
> ```

- [ ] **Step 4: 验证** — `node --check web/app.js`（语法 OK）；`make hash-web`（app.js 改了→重算 hash，index.html 的 script src 自动改写）；`curl -s http://localhost:8080/ | grep -c 'startMemoryConsolidate'` ≥1（需先 `make dev-restart` 起服务；本地起不来则跳过 curl，仅靠 node --check + hash-web）。

- [ ] **Step 5: 手动验收（浏览器）** — `make dev-restart` → 浏览器打开 → 时间线展开一个有记忆的会话 → 记忆区点「记忆整理」→ 看合并组 + 调整项 → 编辑/确认 → merge → 记忆列表刷新（被合并的 memory 不再显示，canonical 置信度变化）。

- [ ] **Step 6: 提交** — `git add web/app.js web/index.html web/app.*.js && git commit -m "feat(web): 记忆整理按钮 + 合并组/调整项草稿确认 UI(仿 T8)"`

> 注：`web/app.<hash>.js` 是 `make hash-web` 生成的副本，需一并 add（index.html 的 script src 指向它）。

---

## 自检

- **spec 覆盖**：D1.1 归一化键（复用 `NormalizeTitle`，T2 已有）→ T2 用；D1.2 `ListActiveTitlesExt`（tx 内读）→ T1；D1.3 commitExtract 佐证去重 → T2；D1.4 `BumpConfidenceExt`（SQL 原子 LEAST）→ T1；D1.5 `TestStageExtractMemoryCorroboration` → T2（+ `TestStageExtractIdempotent` 回归 T2 Step6）。D2.1 consolidate（`ListActive`+LLM+容错解析+schema）→ T3(ListActive)+T5(Consolidate)；D2.2 merge（`ApplyConsolidation` 单事务+merges 优先+adjustments 规则）→ T3(ApplyConsolidation)+T5(Merge)；D2.3 prompt → T4；D2.4 handler 接线（LLM/LLMModel/ConsolidatePrompt 字段+路由+main.go 读 prompt 注入）→ T5；D2.5 `TestMemoryConsolidate`+`TestMemoryMerge` → T5。前端 → T6。无遗漏。
- **占位**：无 TBD/TODO；每步含实际代码与命令。
- **类型一致**：`ListActiveTitlesExt`/`BumpConfidenceExt`（T1）在 T2 引用一致；`ConsolidationReq`/`MemoryMerge`/`MemoryAdjustment`（T3）在 T5(api↔repo)字段一致（`canonical_id`/`member_ids`/`memory_id`/`kind`/`reason`/`evidence_ids`）；`ListActive`/`ApplyConsolidation`（T3）在 T5 引用一致；前端 `memoryDraft` 的 merges/adjustments 结构与 `/api/memories/consolidate` 返回、`/api/memories/merge` 入参一致。
- **取舍落地**：①用 supersede 不 in-place 编辑（outdated=superseded，T3/T5）；②不加 `superseded_by` 列（靠 member 行保留+status+关联迁移，T3）；③默认 delta（佐证+0.05/corroborate+0.05/contradict-0.10/outdated×0.5，T1/T3）；④D1 只字面标题去重（语义合并只在 D2，T2 用 `NormalizeTitle`）；⑤LLM 只判关系不算数字（confidence 由 SQL 原子算，T5 Consolidate 不回传数字、T3 ApplyConsolidation 规则算）。
- **已知取舍（偏离 spec 字面，已拍板）**：D1 批内去重「佐证处理延迟到 kept 插入后」——spec D1.3 字面是遍历内 inline bump，但批内 canonical 此刻未落库会命中 0 行；延后处理对跨 session（本就在库）与批内（kept 已插）都正确。D1 佐证跳过的候选不产 todo（spec D1.3 注4「被跳过候选…可接受」延展）——避免 `memories[i]==nil` 解引用 panic，且佐证=已有事实的再提及，不重复派生 todo；跨 session 佐证在重跑时会再次 +0.05（封顶 0.99，spec D1.2 tx 内读只避开本 session 自去重，跨 session 仍命中，可接受）。
