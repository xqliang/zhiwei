# 记忆合并/更新 + 置信度演化设计（D）

- 日期：2026-08-20
- 状态：待审阅
- 上游：`docs/superpowers/specs/2026-08-20-todo-topic-quality-design.md`（去重/合并/时间戳，已实现并入 main）
- 范围：D1 抽取时佐证去重、D2 手动 LLM 记忆整理（合并 + 置信度演化）、前端整理 UI。

## 背景

现状（已落 main 的记忆模型）：
- `memory` 表：`confidence`/`importance`(DECIMAL 0-1)、`epistemic_type`(observed|inferred|suggested)、`status`(active|superseded|dismissed)、`version`(int，用户手改 title/content 时 +1)、`event_at`、`session_id`、`transcript_segment_ids`(JSON)。
- `MemoryRepo.Save` 只写 title/content/status/version，**不动 confidence/importance**。
- `status='superseded'` 已是合法值（Patch 允许手置），但**目前无任何自动路径会置 superseded**。
- 无 embedding（Sprint 3 才启用），故"语义相近"判定只能靠 LLM。
- 抽取管线 `commitExtract`：单事务内删本 session 旧 memory→建候选 topic→插 memory+memory_topic→插 todo+todo_topic。重跑靠 `DeleteBySessionExt` 幂等。

本设计补"记忆随新信息演化"这条缺失能力，与 A+B+C 的"规则层 + 手动 LLM 层"双层一致。

---

## D1. 抽取时佐证去重（规则层，commitExtract 内）

跨 session 的近重复 = 同一事实被再次提及 = 佐证。新候选若命中已有 active 记忆，不增行、上调已有记忆置信度、并把候选的 topic 关联并过去。

### D1.1 归一化比对键
- 复用 T2 `internal/repo/normalize.go` 的 `NormalizeTitle(title)`（trim + 小写 + 仅字母/数字）。
- 比对维度仅标题（不引入 type/epistemic_type）；语义近但标题不同的由 D2 覆盖。DRY：与 todo 落库去重（T3）同一归一化函数。

### D1.2 事务内读已有 active 记忆
- 新增 `MemoryRepo.ListActiveTitlesExt(ctx, q QueryerContext, userID) ([]struct{ ID ids.ID; Title string }, error)`：
  `SELECT id, title FROM memory WHERE user_id=? AND status='active'`。
- **必须 tx 内读**（传 `tx`，仿 T3 `ListOpenTitlesExt`）：`commitExtract` 已在本事务内 `DeleteBySessionExt` 删了本 session 旧 memory，tx 内读看不到它们，避免重跑时本 session 旧记忆自去重导致幂等失败。事务外调用传 `r.DB`。

### D1.3 commitExtract 插 memory 段改造
在现有「4. insert memory + memory_topic」段，`InsertExt(memories)` 之前：
1. `openTitles := ListActiveTitlesExt(ctx, tx, userID)` → `dupSet map[string]ids.ID`（normTitle → 已有 active memory id）。
2. 遍历候选（保留候选下标 i 与其 `resolvedTids[i]` 的映射）：
   - `nk := NormalizeTitle(c.Title)`
   - 命中 `dupSet[nk]`（即 `oldID`）→ **跳过**（不进 memories 插入切片）：
     - `BumpConfidenceExt(ctx, tx, oldID, +0.05)`（佐证）。
     - 把候选的 topic 关联并到 old memory：`memory_topic` `INSERT IGNORE`（source='ai'）每个 resolvedTid。
   - 未命中 → 进 memories 插入切片，`dupSet[nk] = 新记忆 id`（批内去重）。
3. `InsertExt(ctx, tx, kept)`；memory_topic 行按 kept 切片同序构建（跳过候选不产生 memory_topic 常规行——其 topic 已在上一步行并到 old memory）。
4. 用户手动关联的重链（NaturalKey 快照）只对未跳过候选生效；被跳过候选无新 memory 行，不重链（其语义由 canonical old memory 承载）。可接受。

### D1.4 置信度上调（SQL 原子，并发安全）
- `MemoryRepo.BumpConfidenceExt(ctx, ext ExecerContext, id ids.ID, delta float64) error`：
  `UPDATE memory SET confidence = LEAST(confidence + ?, 0.99) WHERE id = ?`。
- 佐证 delta = `+0.05`，封顶 0.99。**用 SQL 原子算术**（LEAST），不读-改-写，满足并发安全约束。

### D1.5 测试
- `TestStageExtractMemoryCorroboration`：预置一条 active memory「学Rust」(confidence 0.80)；新 session extract fake LLM 产出候选「学 Rust」(标题归一后同为 `学rust`)→ 跑 extract → 断言：无新 memory 行（仍 1 条）、旧 memory confidence=0.85、旧 memory 获得候选的 topic 关联。
- `TestStageExtractIdempotent`（已存在）应仍绿（tx 内读避开自去重）。

---

## D2. 手动 LLM 记忆整理（on-demand 层）

用户点「记忆整理」→ LLM 审全量 active 记忆 → 输出**合并组 + 每条记忆的关系判定**（不给数字）→ 前端编辑确认 → 单事务落库。LLM 判关系，规则算 confidence 数字（可审计、可复现）。

### D2.1 consolidate（提议，不改库）
- `POST /api/memories/consolidate`（空 body）。
- `MemoryRepo.ListActive(ctx, userID, limit) ([]Memory, error)`：`SELECT * FROM memory WHERE user_id=? AND status='active' ORDER BY event_at DESC LIMIT ?`（仅 active，排除 superseded/dismissed）。
- 组 user 消息：JSON 数组，每项 `{id, type, title, content, epistemic_type, confidence, event_at}`（id 用 `.String()`）。
- `LLM.Chat(ctx, provider.ChatRequest{Model: h.LLMModel, System: h.ConsolidatePrompt, User: <JSON>})` → `resp.Content`。
- 容错解析（照搬 candidate.go/T7 思路，内联在 api 包）：`strings.TrimSpace` → 截首个 `{` 到末个 `}` → `json.Unmarshal`。
- LLM 输出 schema（**LLM 只判关系，不给置信度数字**）：
  ```json
  {"merges":[{"canonical_id":"<mid>","member_ids":["<mid>",...]}],
   "adjustments":[{"memory_id":"<mid>","kind":"corroborate|contradict|outdated","reason":"...","evidence_ids":["<mid>",...]}]}
  ```
  - `merges`：语义同一条事实的组（canonical_id 保留为 active，member_ids 并入后置 superseded）。
  - `adjustments`：每条记忆的关系判定 + 理由 + 证据 memory id（佐证/矛盾/过时）。
  - 无则两个数组皆空。
- 返回 `{"merges":[...],"adjustments":[...]}`，不改库。

### D2.2 merge（落库，单事务）
- `POST /api/memories/merge` body = 用户编辑后的提议（同 D2.1 schema）。handler 把字符串 id 用 `ids.ParseID` 转 `[]ids.ID`，组 `repo.ConsolidationReq`：
  ```go
  type ConsolidationReq struct {
      Merges      []MemoryMerge      `json:"merges"`
      Adjustments []MemoryAdjustment `json:"adjustments"`
  }
  type MemoryMerge struct {
      CanonicalID ids.ID   `json:"canonical_id"`
      MemberIDs   []ids.ID `json:"member_ids"`
  }
  type MemoryAdjustment struct {
      MemoryID    ids.ID   `json:"memory_id"`
      Kind        string   `json:"kind"` // corroborate|contradict|outdated
      Reason      string   `json:"reason"`
      EvidenceIDs []ids.ID `json:"evidence_ids"`
  }
  ```
- `MemoryRepo.ApplyConsolidation(ctx, req) error`，单事务（`BeginTxx` + defer Rollback + Commit）：
  - **先处理 merges**（每组）：`canonical = g.CanonicalID`（必须存在且 active）；各 `member`（≠canonical）：
    - memory_topic 关联迁到 canonical memory（把 member 的 topic 关联行复制成 canonical 的，PK 天然去重）：`INSERT IGNORE INTO memory_topic (memory_id, topic_id, source) SELECT ?, topic_id, source FROM memory_topic WHERE memory_id = ?`（args: canonicalID, memberID）。
    - 删 member 的 `memory_topic` 关联（`DELETE FROM memory_topic WHERE memory_id = ?`）。
    - member `status='superseded'`（`UPDATE memory SET status='superseded' WHERE id=?`）。member 行保留（审计）。
  - **后处理 adjustments**：跳过已被 merge 置 superseded 的 member（merges 优先，避免重复处理）；对其余 active memory 按 kind 规则算 confidence（SQL 原子）：
    - `corroborate`：`UPDATE memory SET confidence = LEAST(confidence + 0.05, 0.99) WHERE id = ?`。
    - `contradict`：`UPDATE memory SET confidence = GREATEST(confidence - 0.10, 0.10) WHERE id = ?`。
    - `outdated`：`UPDATE memory SET status = 'superseded', confidence = GREATEST(confidence * 0.5, 0.05) WHERE id = ?`。
  - 返回 `{"applied": true, "merged": <被 supersede 的 member 数>, "adjusted": <应用的 confidence 调整数>}`。

### D2.3 prompt（`prompts/memory_consolidate_v1.md`）
系统指令要点：你是记忆整理器。输入是该用户全部 active 记忆（JSON 数组，含 id/type/title/content/epistemic_type/confidence/event_at）。任务：找出①语义同一条事实的合并组（canonical_id 取其中最完整/最新的一条 id，member_ids 含其余）；②每条记忆与其它记忆的关系（corroborate=被其它佐证更可信、contradict=被新信息否定、outdated=被新信息取代应 superseded），给 reason + evidence_ids。规则：只判确实语义相近；不合并不同事实；不直接给置信度数字（系统按规则算）；不需要的不列。输出 JSON 无围栏，无则两数组皆空。

### D2.4 handler 接线
- `MemoryHandler` 加字段：`LLM provider.LLMProvider`、`LLMModel string`、`ConsolidatePrompt string`（仿 T7 `TopicHandler`）。
- `RegisterMemory` 加：`r.Post("/api/memories/consolidate", h.Consolidate)`、`r.Post("/api/memories/merge", h.Merge)`。
- `cmd/zhiwei-server/main.go`：读 `prompts/memory_consolidate_v1.md`（仿抽取/合并 prompt 的 `os.ReadFile` + `log.Fatal`），注入 `MemoryHandler{..., LLM: llm, LLMModel: cfg.LLMFastModel, ConsolidatePrompt: string(bytes)}`。

### D2.5 测试
- `TestMemoryConsolidate`（fake LLM，仿 T7 `fakeConsolidateLLM`）：预置 2-3 条 active memory；fake LLM 返回含 merges+adjustments 的 canned JSON（member_ids 用真实预置 id）→ `POST /api/memories/consolidate` → 断言 200 + 结构正确。
- `TestMemoryMerge`：预置 2 条 active memory 各带 memory_topic 关联 → `POST /api/memories/merge {merges:[{canonical_id:A, member_ids:[A,B]}], adjustments:[{memory_id:C, kind:corroborate}, ...]}` → 断言：A 的 memory_topic 聚合（A 原+B 迁来，PK 去重）、B `status=superseded`、B 的 memory_topic 已删、corroborate/contradict/outdated 各验一条 confidence 变化（+0.05/-0.10/×0.5+superseded）。

---

## 前端（仿 T8 topic 智能合并 UI）

- 记忆相关页（时间线详情的 memory 区 / 或独立 memory 列表——以现有前端结构为准）加「记忆整理」按钮。
- 点 → `POST /api/memories/consolidate` → 草稿面板：合并组（可改 canonical、勾选 member）+ 调整项列表（每条显示 memory 标题 + kind + reason，可勾选保留/丢弃）→ 「确认整理」→ `POST /api/memories/merge` → 刷新。
- `web/app.js` 加 `consolidateDraft/startConsolidate/toggleMergeMember/applyConsolidation` 等（member 用 `{id,name,checked}` 对齐，修掉索引错位，同 T8 做法）；`web/index.html` 加按钮 + 草稿面板。
- `node --check web/app.js` + `make hash-web`；手动验收（浏览器）。

---

## 取舍（已拍板）

1. **用 supersede 不用 in-place 内容编辑**：outdated = 旧记忆置 superseded、新记忆留 active（append-only、全审计），不改旧记忆 content。`version` 字段仍只用于用户手改 title/content。
2. **不加 `superseded_by` 列**：靠 member 行保留 + `status='superseded'` + memory_topic 关联迁移到 canonical 表达"并入 canonical"，不新增外键列。
3. **默认 delta**：D1 佐证 +0.05 / D2 corroborate +0.05 / contradict -0.10 / outdated ×0.5。先跑起来，后续可按数据调。
4. **D1 不做语义合并**（只字面标题去重），语义合并只在 D2。
5. **LLM 只判关系不算数字**：confidence 数字由规则（SQL 原子算术）算，可审计可复现。

---

## 顺序与非目标

- 实现顺序：D1（规则层，commitExtract 内 + 2 个 repo 方法 + 测试）→ D2 后端（prompt + ListActive + ApplyConsolidation + consolidate/merge handler + 接线 + 测试）→ D2 前端（整理按钮 + 草稿/确认 UI）。
- 非目标：不做自动定期整理（用户手动触发）；不做 embedding/向量语义（Sprint 3）；不跨用户；D1 不判语义；不改多对多基础模型；不新增 superseded_by 列。
