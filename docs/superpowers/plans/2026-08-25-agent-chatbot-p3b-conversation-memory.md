# 实施计划：Agent Chatbot P3b — 对话转记忆（Conversation → Memory）

- 日期：2026-08-25
- 分支 / worktree：`feat/agent-chatbot`（`/Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.worktrees/agent-chatbot`）
- 范围：**仅后端（Track C）**。把「问知微」对话历史（`agent_message` 文本行）转成候选记忆，**复用现有录音抽取范式**（窗口化 LLM 抽取 → 解析 → 质量闸门 → dedup → 落库）。无前端。
- 关联规格：`docs/superpowers/specs/2026-08-24-agent-chatbot-system-design.md`
  - §6.3（memory 加 `conversation_id` + `session_id` 放宽可空，溯源）
  - §12（对话转记忆：复用抽取范式；产物是**候选记忆**，非「修改」）
  - §7.2 / §8（写入提议→确认闸门；本计划用作**对比说明**，明确对话转记忆**不**走此路径）
- 现状：P1 已落地（`agent_conversation` / `agent_message` / `agent_proposal` / `weekly_review` / `topic_status` + 对应 repo）。迁移 `000005_agent.up.sql` 顶部已注明「memory 的 `session_id` 可空化 + `conversation_id` 列留到 Plan 3（对话转记忆）再加」——**本计划即该 Plan**。

---

## 关键决策（务必先读）

### D-A：抽取产物走「候选记忆直插」，**不**走「提议→确认闸门」（依据 §12）

§12 明确区分两类写：
- **抽取产出候选记忆** → 走「现有 memory 候选流程」（与录音抽取 `stage_extract` 完全同构：`memory` 表直接落 `status='active'`、`epistemic_type` 由 LLM 给），只是溯源标记换成 `conversation_id`。
- **修改我已有的信息** → 只经 §8 的 `agent_proposal`（pending→confirmed）人审闸门。

对话转记忆属于**前者**（从对话里"发现"新事实/偏好/决定），不是对既有行的 mutate，因此**直接插 memory**、复用同一 `GateConfig` 质量闸门与 dedup，**不建 `agent_proposal` 行**。这与「reuse extraction paradigm」的目标一致：录音抽取怎么落，对话抽取就怎么落。

> 注：提示注入防线不受影响——对话里若出现「帮我把 X 改成 Y」，那属于「修改」，仍只能由 agent 的 `propose_*` 工具经 §8 闸门生成待确认提议；对话转记忆这条链路**只新增候选、从不改写既有行**，天然无法被注入劫持成静默修改。

### D-B：`session_id` 可空 = 结构体字段改指针 `*ids.ID`（依据 §6.3，取"前者"方案）

§6.3 给两个方案，**倾向前者**（`conversation_id` 列 + `session_id` 可空，可溯源到具体对话）。本计划采纳前者。Go 侧机制：
- 迁移把 `memory.session_id` 由 `NOT NULL` 放宽为 `NULL`，并新增 `conversation_id BIGINT NULL`。
- `Memory` 结构体 `SessionID` 由值类型 `ids.ID` 改为**指针 `*ids.ID`**，新增 `ConversationID *ids.ID`。**理由**：
  1. `NewDB` 开启 sqlx **safe 模式**（见 `internal/repo/db.go` 注释）——一旦 `memory` 真出现 `session_id IS NULL` 的行，`SELECT *`（`Get`/`List`/`Search`/`ListActive`/`ListByTopic`）扫描 NULL 进非指针 `ids.ID`（底层 `int64`）会直接报错。必须用指针（NULL→nil）才安全。
  2. 指针是本仓已确立的**可空标量约定**：`agent_message.go` 的 `ConversationID *ids.ID`、`agent_proposal.go` 的 `MessageID/TargetID/AppliedRef *ids.ID`、`memory.go` 自身的 `EventAt *time.Time`。
  3. **爆炸半径已核实极小**：全仓非测试代码**无任何** `m.SessionID` 值读取（`.SessionID` 命中的都是 `Job`/`Prompt`/`sessionOut` 等别的结构体）；`Memory.SessionID` 唯一的非测试写入点是 `internal/pipeline/stage_extract.go:226`（改 `SessionID: &sessionID` 一行）。其余为测试文件的机械 `&`。
  4. **合并协调点**：结构体字段类型变更会被 import `repo` 的并行分支感知（若它们按值读 `m.SessionID`）——与迁移号一样，属已知合并协调点，见任务 2 提示。

---

## 约束与复用清单

- **迁移号 000006 会与并行分支撞号**（person-profile / speaker-name-inference），合并时统一重编号——任务 1 顶部醒目标注。
- **保持 `package repo` 始终可编译**（其他 track import 它）；**session 路径必须保持绿**（若重构共享代码，补 session 回归测试）。
- 可空 JSON → `*json.RawMessage`；可空标量 → 指针（对齐 `memory.go`/`agent_message.go`）。
- 复用既有纯逻辑：`memory.Extractor`（源无关，吃 `[]Block`）、`memory.ApplyGate` + `GateConfig`、`memory.ResolveTopics`、`memory.NaturalKey`、`repo.NormalizeTitle`、`MemoryRepo.ListActiveTitlesExt`/`BumpConfidenceExt`、`MemoryTopicRepo.InsertExt`。
- **幂等**：re-run 不得重复——按 `conversation_id` 先 `DELETE` 再 `INSERT`（同 `stage_extract` 的按 session 幂等），叠加 `NormalizeTitle` 跨源 dedup。
- 并发安全：落库全程单事务（`BeginTxx`），佐证上调走 SQL 原子算术（`BumpConfidenceExt` 的 `LEAST`）。
- 测试：单测 mock LLM、不碰 MySQL；repo 集成测试用 `TEST_MYSQL_DSN`（`make test-integration` → `zhiwei_test`），插入共享表（`memory`/`memory_topic`）**必须 `t.Cleanup`** 按 `conversation_id`/`session_id` 清理。
- 中文详细注释（新人可读）。

---

## 任务列表（仅标题）

1. 迁移 `000006_conversation_memory`（up/down）：memory 加 `conversation_id`+索引、`session_id` 放宽 NULL（含并行迁移号冲突醒目提示）
2. `package repo` 扩展：`Memory` 结构体加字段 + `InsertConversationExt` / `DeleteByConversationExt`（memory & memory_topic），改 `stage_extract` 唯一写入点，session 路径保持绿
3. Prompt `prompts/conversation_extraction_v1.md`（版本化、中文、严格 JSON 输出）
4. `internal/memory/conversation.go`（package memory）：`ExtractConversation` + 对话块组装 + `commitConversation`（gate / dedup / 单事务落库）
5. 单元测试（mock LLM，无 MySQL）：对话块组装 / 解析 / 闸门 / dedup 纯逻辑 + 假 LLM 编排
6. repo 集成测试（`TEST_MYSQL_DSN`）：可空 `session_id` 往返、`conversation_id` 落库、按会话幂等删除、safe-mode `SELECT *`
7. COORDINATOR INTEGRATION（说明，不改代码）：暴露 `ExtractConversation`，协调者接 `POST /api/agent/conversations/{cid}/extract` + 夜间批跑

---

## 任务详情

### 任务 1 — 迁移 `000006_conversation_memory`

**文件**：`migrations/000006_conversation_memory.up.sql`、`migrations/000006_conversation_memory.down.sql`

> ⚠️ **迁移号协调点（醒目）**：`000006` 与并行分支（person-profile / speaker-name-inference）**很可能撞号**。合并前保持 `000006`，**合并时统一重编号**（沿用 000005 的已知协调惯例，见 spec §17）。重编号只改文件名前缀，SQL 内容不变。

**背景（已核实）**：`memory` 现为 `session_id BIGINT NOT NULL` + `KEY idx_session (session_id)`，**无外键**（`000001_init.up.sql`），故 `MODIFY` 无 FK 牵连；legacy `topic_id` 已被 `000003` 删除。多语句/文件沿用 `000005` 风格（一个 up 文件多条 ALTER）。

**up.sql**（中文注释详版）：

```sql
-- 对话转记忆（Plan 3b）数据层：见 spec §6.3 / §12。
-- 承接 000005_agent 顶部预告：memory 加 conversation_id + session_id 放宽可空。
-- 【合并注意】迁移号 000006 与 person-profile / speaker-name-inference 并行分支撞号，
--            合并时统一重编号（项目已知协调点）。

-- 1) 新增对话溯源列（可空；录音来源的记忆此列为 NULL）。
ALTER TABLE memory ADD COLUMN conversation_id BIGINT NULL AFTER session_id;

-- 2) 放宽 session_id 为可空（对话来源的记忆此列为 NULL；录音来源仍写 session_id）。
--    无 FK，直接 MODIFY；保持 BIGINT 类型与其余属性不变。
ALTER TABLE memory MODIFY COLUMN session_id BIGINT NULL;

-- 3) conversation_id 检索/幂等删除用索引（按会话删旧记忆、按会话查）。
ALTER TABLE memory ADD KEY idx_mem_conversation (conversation_id);
```

**down.sql**（仅开发环境；MySQL 无 `DROP COLUMN IF EXISTS`）：

```sql
-- 反向回滚，仅开发环境。
-- 注意：把 session_id 收回 NOT NULL 前，必须先清掉 session_id IS NULL 的对话记忆，
--       否则 MODIFY ... NOT NULL 会因存在 NULL 行失败。
ALTER TABLE memory DROP KEY idx_mem_conversation;
DELETE FROM memory WHERE conversation_id IS NOT NULL OR session_id IS NULL; -- 清对话来源记忆
ALTER TABLE memory DROP COLUMN conversation_id;
ALTER TABLE memory MODIFY COLUMN session_id BIGINT NOT NULL;
```

**验收**：`make init-testdb`（对 `zhiwei_test` 跑 `migrate up`）通过；`DESCRIBE memory` 显示 `session_id` 可空、`conversation_id` 存在且可空、`idx_mem_conversation` 存在。

### 任务 2 — `package repo` 扩展（`internal/repo/memory.go` + 一处 pipeline 改动）

**目标**：加溯源字段 + 对话专用 Insert/Delete；**session 路径行为不变、包始终可编译**。

#### 2.1 `Memory` 结构体加字段（`internal/repo/memory.go`）

`SessionID` 改指针（可空），新增 `ConversationID`。**safe-mode 强约束**：`conversation_id` 列一旦加入表，`SELECT *` 会返回它，结构体**必须**有对应字段，否则所有 memory 查询报 `missing destination name`。

```go
// SessionID 改为可空指针：对话来源的记忆此列为 NULL（见 spec §6.3）。
// 录音来源仍写 session_id（stage_extract 传 &sessionID）。
// 用指针而非值类型：sqlx safe 模式下 SELECT * 扫描 NULL 进非指针 int64 会报错。
SessionID *ids.ID `db:"session_id" json:"session_id,omitempty"`
// ConversationID 对话溯源（可空）：仅对话转记忆写此列，录音来源为 NULL。
ConversationID *ids.ID `db:"conversation_id" json:"conversation_id,omitempty"`
```

（放在原 `SessionID` 行位置替换 + 紧随其后加 `ConversationID`；其余字段不动。）

#### 2.2 `InsertExt` 保持不变，`session_id` 绑定天然兼容指针

现有 `InsertExt` 的 `NamedExecContext` 里 `:session_id` 对 `*ids.ID`：非 nil → 解引用为 int64；nil → NULL（`go-sql-driver` 的 `DefaultParameterConverter` 处理指针）。**SQL 与列清单不改**，session 路径零改动（除调用方传指针，见 2.5）。录音记忆的 `conversation_id` 靠 DB 默认（未列出即 NULL）——无需改 `InsertExt`。

#### 2.3 新增 `InsertConversationExt`（对话专用 Insert 变体）

独立方法（不动 `InsertExt`，最小风险满足「session-based Insert MUST keep working」）：为每条记忆盖上 `conversation_id`、强制 `session_id = NULL`，其余与 `InsertExt` 同构（生成 id、默认 user_id）。

```go
// InsertConversationExt 批量插入对话来源的记忆：统一盖 conversation_id、session_id 置 NULL。
// 与 InsertExt 同构（ext 传 *sqlx.Tx 入事务；预置非零 id 被尊重，供批内 dedup/佐证引用）。
// 单独一条 INSERT（含 conversation_id 列、不含 session_id 列→默认 NULL），
// 保持 InsertExt 原样不变（session 路径不受影响）。
func (r *MemoryRepo) InsertConversationExt(ctx context.Context, ext ExecerContext, convID ids.ID, ms []*Memory) error {
	if len(ms) == 0 {
		return nil
	}
	for i := range ms {
		if ms[i].ID == 0 {
			ms[i].ID = ids.New()
		}
		if ms[i].UserID == 0 {
			ms[i].UserID = 1
		}
		cid := convID // 每条都指向同一对话；用局部变量取地址避免共享循环变量
		ms[i].ConversationID = &cid
		ms[i].SessionID = nil // 对话来源无 session
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO memory (id, user_id, type, title, content, epistemic_type,
  importance, confidence, conversation_id, transcript_segment_ids, event_at, status)
VALUES (:id, :user_id, :type, :title, :content, :epistemic_type,
  :importance, :confidence, :conversation_id, :transcript_segment_ids, :event_at, :status)`, ms)
	return err
}
```

（注：`session_id` 不在列清单 → 落 NULL；`transcript_segment_ids` 对话来源通常为空 `ids.List{}`，溯源靠 `conversation_id`。）

#### 2.4 新增 `DeleteByConversationExt`（幂等重跑用）

`MemoryRepo` 与 `MemoryTopicRepo` 各加一个，镜像 `DeleteBySessionExt`：

```go
// MemoryRepo：删一个 conversation 的全部对话记忆（对话抽取重跑幂等；须与 Insert 同 tx）。
func (r *MemoryRepo) DeleteByConversationExt(ctx context.Context, ext ExecerContext, convID ids.ID) error {
	_, err := ext.ExecContext(ctx, `DELETE FROM memory WHERE conversation_id = ?`, convID.Int64())
	return err
}

// MemoryTopicRepo：删该对话全部记忆的 topic 关联（须先于删 memory；关联表依赖主表行）。
func (r *MemoryTopicRepo) DeleteByConversationExt(ctx context.Context, ext ExecerContext, convID ids.ID) error {
	_, err := ext.ExecContext(ctx,
		`DELETE FROM memory_topic WHERE memory_id IN (SELECT id FROM memory WHERE conversation_id = ?)`,
		convID.Int64())
	return err
}
```

> ⚠️ MySQL 不允许在 `DELETE ... WHERE ... IN (SELECT ... FROM 同表)` 里直接子查询**被删表**；但此处子查询的是 `memory`、删的是 `memory_topic`（不同表），合法。删除顺序：先 `MemoryTopics.DeleteByConversationExt` 再 `Memories.DeleteByConversationExt`（见任务 4 commit 顺序）。

#### 2.5 改 session 路径唯一写入点（`internal/pipeline/stage_extract.go`）

`SessionID` 变指针后，`stage_extract.go` 里构造 `&repo.Memory{... SessionID: sessionID ...}`（约 226 行）改为 `SessionID: &sessionID`。这是**全仓唯一**需要跟着改的非测试写入点（已核实）。

#### 2.6 保持 session 路径绿（回归）

`repo` 与 `pipeline` 的现有测试若以 `SessionID: sid` 字面量构造 `Memory`，机械改 `&sid`（局部变量取址）。**新增/保留一个回归断言**：录音抽取（`stage_extract` 或 `MemoryRepo.InsertExt` + `ListBySession`）落库后 `session_id` 非空、`conversation_id` 为 NULL，证明加列后 session 路径不回归。

**验收**：`go build ./...` 通过；`go vet ./...` 通过；session 相关 repo/pipeline 测试全绿。

### 任务 3 — Prompt `prompts/conversation_extraction_v1.md`

**文件**：`prompts/conversation_extraction_v1.md`（版本化，同 `extraction_v3.md` 的加载/版本约定；版本号进 trace）。

与 `extraction_v3` 同一 JSON schema（`candidates[]`：`type/title/content/epistemic_type/importance/confidence/is_todo/todo_due/topics/block_index`），保证 `ParseCandidates` 可**原样复用**。差异只在"输入是**用户↔知微的对话**、抽取对象是**用户**"这一语境。要点：

- **只抽关于用户的记忆**：用户说出的事实/偏好/决定/事件/问题/想法。**忽略**"知微(助手)"自己的话、寒暄、工具调用说明、对用户的建议——除非用户明确认可（认可后按 `observed` 记）。
- `epistemic_type`：用户明确说 = `observed`；从对话推断 = `inferred`；助手建议且用户未确认 = 不抽（或标 `suggested` 但压低 `confidence`）。
- 对话中用户做出的承诺/待办置 `is_todo=true`（`todo_due` 用 ISO 8601 含时区，无则 null）。
- topics：优先复用「已有主题列表」的 `topic_id`；确无相近才给 `suggested_name`；可 0~N 个。
- 每个对话块最多 2 条、整批最多 10 条，宁缺毋滥；每条给 `block_index`。
- **严格 JSON**：只输出 JSON，无代码围栏无多余文字；无可记则 `{"candidates":[]}`。

**建议正文骨架**：

```markdown
# 知微「对话转记忆」抽取 prompt（版本：conversation_extraction_v1）

你是个人 AI「知微」的记忆抽取器。输入是**用户与知微的一段对话**（已按发言聚合为对话块，每块标注说话人：用户 / 知微，带序号）。你的任务：从对话中提取**关于用户、值得长期记住**的记忆候选，并归入已有主题或建议新主题。

## 抽取规则
1. 只抽**关于用户**的信息：用户说出口的事实、偏好、决定、计划、遇到的问题、想法。
2. **忽略"知微"（助手）自己的发言**：解释、反问、建议、工具调用说明都不抽；除非用户在随后明确认可某条助手建议，才按用户认可内容抽取。
3. 不要推测用户没表达的内容；每条 content 独立可读，含必要主语与时间。
4. type 仅取：event / fact / decision / idea / problem / preference。
5. epistemic_type：用户明确说=observed；由对话合理推断=inferred；助手提出且用户未确认=不抽。
6. importance 0~1：琐事<0.3；对用户有意义 0.5~0.7；影响计划/关系/健康>0.8。
7. confidence 0~1：用户表述清晰=0.9+；有歧义/推断成分高则降低。
8. 用户做出的承诺/待办置 is_todo=true，尽量给 todo_due（ISO 8601 含时区，如 2026-08-26T10:00:00+08:00），无明确时间则 null。
9. topics：优先复用「已有主题列表」中语义相近的 topic_id，不造近重复名；确无相近才给 suggested_name（简短名词短语）；一条可归 0~N 个主题。
10. 每个对话块最多 2 条候选，整批最多 10 条；每条输出 block_index（对应输入块序号）。

## 输出格式
只输出 JSON，无任何其他文字或代码围栏。无可记忆内容时输出 {"candidates":[]}。

{"candidates":[{"type":"preference","title":"偏好晨间深度工作","content":"用户说自己习惯早上做需要专注的工作","epistemic_type":"observed","importance":0.6,"confidence":0.9,"is_todo":false,"todo_due":null,"topics":[{"topic_id":"<已有主题id>"}],"block_index":2}]}
```

**验收**：`ParseCandidates` 对该 prompt 的样例输出解析通过（单测里以样例串直喂，见任务 5）。

### 任务 4 — `internal/memory/conversation.go`（package memory）

**目标**：`ExtractConversation(ctx, deps, convID)`：`ListByConversation` → 组装对话块 → **复用 `Extractor`**（窗口化 LLM 抽取，用 `ExtractWindow`）→ `ParseCandidates` → `ApplyGate`（复用 `GateConfig`）→ `ResolveTopics` → **单事务落库**（按 `conversation_id` 幂等 + `NormalizeTitle` dedup，标 `conversation_id`）。

> 包定位说明：`internal/memory` 的 `block/candidate/topic/naturalkey` 是纯逻辑；`extract.go` 已是"注入 LLM 接口的编排层"。`conversation.go` 与 `extract.go` 同层（编排），通过注入的 repo + `*sqlx.DB` 落库。无 import 环（`repo` 不 import `memory`）。

#### 4.1 依赖与入口

```go
package memory

// ConversationExtractDeps 是对话转记忆的依赖注入（镜像 pipeline.StageDeps，但按 memory 范围裁剪）。
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

// ExtractConversation 从一段「问知微」对话抽取候选记忆并落库（幂等：按 conversation_id 先删后插）。
// 空对话/无文本消息直接返回零结果（非错误）。产物是候选记忆（走 memory 候选流程，
// 不建 agent_proposal——见计划 D-A）。
func ExtractConversation(ctx context.Context, d ConversationExtractDeps, convID ids.ID) (ConversationExtractResult, error) {
	msgs, err := d.AgentMessages.ListByConversation(ctx, convID)
	if err != nil {
		return ConversationExtractResult{}, fmt.Errorf("读对话消息: %w", err)
	}
	blocks, baseTime := buildConversationBlocks(msgs)
	if len(blocks) == 0 {
		return ConversationExtractResult{}, nil // 无可抽内容，幂等空跑
	}
	topics, err := d.Topics.ListActive(ctx, 1, 30) // user_id=1（单用户 MVP），同 stage_extract 的 topicPromptLimit
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
	log.Printf("[conv-extract] conv=%s msgs=%d 候选=%d 落库=%d 新topic=%d",
		convID, res.Messages, res.Candidates, res.Kept, res.NewTopics)
	return res, nil
}
```

#### 4.2 对话块组装（`agent_message` → `[]Block`）

只取 `role∈{user,assistant}` 且 `kind=="text"` 的消息；**两方都进块**（助手文本是理解用户的上下文），说话人标 `用户`/`知微`，交给 prompt 去"只抽用户"。`baseTime = 首条消息 CreatedAt`，每块 `StartMS = msg.CreatedAt - baseTime`，使 `Extractor` 算出的 `EventAt` ≈ 该发言时间。对话无 transcript segment，`SegmentIDs` 留空——溯源靠 `conversation_id`。

```go
// buildConversationBlocks 把对话消息转成抽取块。跳过工具类消息（tool_call/tool_result/card），
// 只保留 user/assistant 的文本发言。返回块列表与基准时间（首条文本消息时间）。
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

func countTextMessages(msgs []repo.AgentMessage) int {
	n := 0
	for _, m := range msgs {
		if (m.Kind == "" || m.Kind == "text") && (m.Role == "user" || m.Role == "assistant") && strings.TrimSpace(m.Content) != "" {
			n++
		}
	}
	return n
}
```

> `ListByConversation` 已按 `id ASC`（=时间序）返回，块顺序即对话顺序；`Extractor.SplitWindows` 按 `Window` 切窗多次调用 LLM，与录音抽取一致。

#### 4.3 单事务落库 `commitConversation`（幂等 + dedup + 佐证）

**刻意镜像** `pipeline.commitExtract`，但按 `conversation_id` 幂等、聚焦 memory（不含 todo，见下方 §12 说明）。顺序：删旧关联 → 删旧记忆 → 建议 topic → dedup 决策 → 插新记忆 → memory_topic(ai) → 佐证上调。

```go
// commitConversation 单事务落库：按 conversation_id 幂等清理后重插，标 conversation_id、session_id 置 NULL。
// dedup 复用 stage_extract 的 D1 佐证逻辑：新候选归一化标题命中已有 active 记忆（或批内已加）→ 不增行、
// 上调 canonical 置信度 + 把候选 topic 并入 canonical。返回新落库记忆数。
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
		m := &repo.Memory{
			ID:   ids.New(),
			Type: c.Type, Title: c.Title, Content: c.Content,
			EpistemicType: c.EpistemicType,
			Importance:    c.Importance, Confidence: c.Confidence,
			TranscriptSegmentIDs: ids.List(c.SegmentIDs), // 对话来源通常空
			EventAt:              &c.EventAt, Status: "active",
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
```

#### 4.4 与 §12 的取舍（明确记录）

- **todo 候选**：§12 提到"memory 候选 + todo 候选"。本计划**聚焦 memory**（任务/文件 scope 为 conversation→memory）。todo 可作为**后续平行扩展**，完全照搬 `stage_extract` 的 `IsTodo/TodoStatus` + `Todos.InsertExt` + `TodoTopics` 处理（`SourceMemoryID` 指向对应新记忆）；此处不实现以免超范围，代码里以注释标注扩展点。
- **用户手动 topic 关联的快照/重链**：`stage_extract` 会快照 `source='user'` 的 `memory_topic` 再重链。对话记忆当前无"用户手动关联对话记忆到 topic"的入口（前端不在本 scope），故 MVP **不做**快照/重链（YAGNI）；若后续开放该入口，照 `commitExtract` 的 `SnapshotUserBySessionExt` 模式加 `...ByConversationExt` 即可。

**验收**：`go build ./...` 通过；任务 5/6 测试全绿。

### 任务 5 — 单元测试（mock LLM，无 MySQL）

**文件**：`internal/memory/conversation_test.go`（package memory，可直接调未导出函数；复用 `extract_test.go` 里已有的 `fakeLLM`）。

覆盖**不依赖 DB** 的纯逻辑与"块组装 + Extractor 复用"链路（`commitConversation` 落库放任务 6）：

1. **`TestBuildConversationBlocks`**：构造若干 `repo.AgentMessage`（`CreatedAt` 递增），断言：
   - `role=user/assistant` 且 `kind=text` 的进块；`kind=tool_call/tool_result/card`、空 `content`、其他 role 被跳过；
   - 说话人标签：user→`用户`、assistant→`知微`；
   - `baseTime` = 首条文本消息 `CreatedAt`；各块 `StartMS` = 相对 base 的毫秒偏移（首块 0，后续 >0）；
   - `SegmentIDs` 为空（对话无 transcript 段）。

```go
func TestBuildConversationBlocks(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	cid := ids.New()
	msgs := []repo.AgentMessage{
		{ConversationID: &cid, Role: "user", Kind: "text", Content: "我最近在学 Rust", CreatedAt: t0},
		{ConversationID: &cid, Role: "assistant", Kind: "tool_call", Content: `{"name":"search_memory"}`, CreatedAt: t0.Add(1 * time.Second)},
		{ConversationID: &cid, Role: "assistant", Kind: "text", Content: "了解，学多久了？", CreatedAt: t0.Add(2 * time.Second)},
		{ConversationID: &cid, Role: "user", Kind: "text", Content: "  ", CreatedAt: t0.Add(3 * time.Second)}, // 空白跳过
	}
	blocks, base := buildConversationBlocks(msgs)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2（跳过 tool_call 与空白）", len(blocks))
	}
	if !base.Equal(t0) {
		t.Fatalf("base = %v, want %v", base, t0)
	}
	if blocks[0].SpeakerLabel != "用户" || blocks[1].SpeakerLabel != "知微" {
		t.Fatalf("speaker = %q,%q", blocks[0].SpeakerLabel, blocks[1].SpeakerLabel)
	}
	if blocks[0].StartMS != 0 || blocks[1].StartMS != 2000 {
		t.Fatalf("StartMS = %d,%d, want 0,2000", blocks[0].StartMS, blocks[1].StartMS)
	}
	if len(blocks[0].SegmentIDs) != 0 {
		t.Fatalf("对话块不应有 segment 溯源: %v", blocks[0].SegmentIDs)
	}
}
```

2. **`TestConversationExtractorReuse`**：把 `buildConversationBlocks` 的产物喂给 `Extractor{LLM: fakeLLM}`，断言 LLM 收到的 `User` 消息含 `用户`/`知微` 标签、候选被解析、`EventAt` = `base + StartMS`。证明源无关的 `Extractor` 对对话块开箱即用。

3. **`TestConversationCandidateParse`**：用**任务 3 prompt 的样例输出串**喂 `ParseCandidates`，断言字段（`type=preference`、`is_todo`、`topics`）解析正确——锁定 prompt 与解析器契约。

4. **`TestConversationGate`**：对话候选过 `ApplyGate`（`GateConfig{MinConf:0.6, TodoConf:0.85}`），断言低置信被丢、短内容被丢、todo 按阈值定 `suggested/confirmed`——复用同一闸门。

> 说明：`ExtractConversation` 全链路（含 `commitConversation` 落库）依赖具体 repo（持 `*sqlx.DB`，非接口），按本仓惯例走**集成测试**（任务 6），不在此处 mock。纯逻辑在此覆盖，符合"单测 mock LLM、不碰 MySQL"。

**验收**：`make test`（无 MySQL，mock provider）通过；上述用例全绿。

### 任务 6 — 集成测试（真 MySQL，`TEST_MYSQL_DSN`）

**门禁**：`repo.TestDSN(t)`（未设 `TEST_MYSQL_DSN` 自动 Skip）；跑法 `make test-integration`（起 docker MySQL + `migrate up` 到 `zhiwei_test` + 设 DSN）。**插入共享表 `memory`/`memory_topic` 必须 `t.Cleanup`**（按 `conversation_id`/`session_id` 清理），防串扰。

#### 6.1 repo 层（`internal/repo/memory_conversation_test.go`，package repo）

1. **`TestInsertConversationRoundTrip`**：`InsertConversationExt(tx=db, convID, []*Memory{...})` 后 `Get(id)`：
   - `m.ConversationID != nil && *m.ConversationID == convID`；
   - `m.SessionID == nil`（**可空 session_id 往返关键断言**——证明 NULL 正确扫进指针，safe-mode 不报错）；
   - `Search`/`ListActive`/`List` 能命中该记忆且 `SELECT *` 不因 `conversation_id` 列报 `missing destination`（**safe-mode 回归**）。
   ```go
   sid_unused := ids.New() // 仅用于 cleanup 复用；实际不写
   convID := ids.New()
   mr := &MemoryRepo{DB: db}
   t.Cleanup(func() { _ = mr.DeleteByConversationExt(context.Background(), db, convID) })
   ms := []*Memory{{Type: "fact", Title: "会话记忆X9Z", Content: "对话里用户提到的独特事实X9Z",
       Status: "active", Importance: 0.6, Confidence: 0.8}}
   if err := mr.InsertConversationExt(ctx, db, convID, ms); err != nil { t.Fatal(err) }
   got, err := mr.Get(ctx, ms[0].ID)
   if err != nil { t.Fatalf("Get(safe-mode SELECT *): %v", err) }
   if got.SessionID != nil { t.Errorf("session_id 应为 NULL, got %v", *got.SessionID) }
   if got.ConversationID == nil || *got.ConversationID != convID { t.Errorf("conversation_id 未落库: %v", got.ConversationID) }
   _ = sid_unused
   ```

2. **`TestDeleteByConversationIdempotent`**：插 2 条对话记忆 + 各挂一个 `memory_topic`，`MemoryTopics.DeleteByConversationExt` 再 `Memories.DeleteByConversationExt` 后，`ListByConversation`/关联均空；**重复删**不报错（幂等）。断言 `memory_topic` 子查询删除正确（删的是关联、子查的是 memory，不同表合法）。

3. **`TestSessionPathRegression`**（session 路径保持绿）：`InsertExt(db, []*Memory{{SessionID: &sid, ...}})` 后 `Get`：`m.SessionID != nil && *m.SessionID == sid` 且 `m.ConversationID == nil`。证明加列 + 字段改指针后**录音路径不回归**。`t.Cleanup` 按 `sid` 调 `DeleteBySessionExt`。

#### 6.2 端到端（可选，`internal/memory/conversation_e2e_test.go`，package memory）

`repo.TestDSN(t)` 门禁 + `fakeLLM` 预置一段候选 JSON，构造 `ConversationExtractDeps`（真 repo + 真 DB + fakeLLM），先写入若干 `agent_message`（一条对话），调 `ExtractConversation(ctx, deps, convID)`：
- 断言 `res.Kept >= 1`、落库记忆 `ConversationID == convID`、`SessionID == nil`；
- **幂等**：`ExtractConversation` 跑第二次，`ListByConversation`（或按 conversation_id 查）记忆条数不翻倍（先删后插生效）；
- `t.Cleanup` 删该 `convID` 的 memory/memory_topic 与 agent_message、agent_conversation。

> 该 e2e 覆盖"对话 → 候选 → 闸门 → dedup → 落库 + 幂等"全链路，是 §12 的验收级测试；LLM 仍用 fake，不触真 Ark。真 LLM 手动 spike 见任务 7 说明。

**验收**：`make test-integration` 通过；上述用例全绿；`memory` 表无测试残留（cleanup 生效）。

### 任务 7 — COORDINATOR INTEGRATION（说明，本计划**不改**这些文件）

> 本 Track 的交付边界止于 `package memory` 暴露 `ExtractConversation` + `package repo` 的落库能力 + prompt 文件 + 迁移。**不改** `cmd/zhiwei-server/main.go`、`internal/agent/*`、`internal/api/*`、`web/*`。以下是给协调者（Coordinator）接线用的对接说明。

**协调者需做的接线**（不属本计划实现范围）：

1. **加载 prompt**（`main.go`，仿现有 `promptPath` 常量与 `os.ReadFile`）：
   ```go
   const conversationPromptPath = "prompts/conversation_extraction_v1.md"
   convPromptBytes, _ := os.ReadFile(conversationPromptPath) // 启动期读入
   // convPromptVersion := "conversation_extraction_v1"
   ```

2. **组装 deps + 暴露调用**：用已装配的 repo 集 + `provider.NewArkLLM`（复用现有 `llm`）构造 `memory.ConversationExtractDeps`：
   - `Model`：`cfg.AgentModel`（为空回退 `cfg.LLMFastModel`）；
   - `Prompt`/`PromptVersion`：上面读入的对话 prompt 与版本；
   - `Window`：`cfg.ExtractWindow`；`Gate`：`memory.GateConfig{MinConf: cfg.QualityMinConf, TodoConf: cfg.QualityTodoConf}`。
   - **无需新增 config 项**（全部复用现有 §14 配置）。

3. **HTTP 端点** `POST /api/agent/conversations/{cid}/extract`（协调者在 `internal/api`/`internal/agent` 注册路由 + handler）：
   - 解析 `cid`（`ids.ParseID`）→ 调 `memory.ExtractConversation(ctx, deps, cid)` → 返回 `ConversationExtractResult` JSON（`{messages,candidates,kept,new_topics,windows,tokens}`）。
   - 幂等：重复 POST 同一 `cid` **不重复落库**（`commitConversation` 先按 `conversation_id` 删后插 + `NormalizeTitle` dedup）——协调者无需加锁去重，但**同一 `cid` 的并发抽取**应串行（handler 层可用 per-conversation 轻量互斥，避免两个事务同时 delete+insert 打架）。

4. **夜间批跑**（§12「每晚随日报批跑」）：协调者的日报 cron（`ZW_REVIEW_DAILY_CRON`）里，对当日活跃的 `agent_conversation` 逐个调 `ExtractConversation`。因幂等，批跑与手动 POST 叠加安全。

5. **真 LLM spike（手动，不进 CI）**：建议加 `make spike-conv-extract`（仿 `spike-llm`）：起真 Ark DeepSeek，对一条真实对话跑 `ExtractConversation`，肉眼校验候选质量与 prompt 效果。

**对接契约摘要**（协调者只依赖这三点，其余为内部实现）：
- `func memory.ExtractConversation(ctx, memory.ConversationExtractDeps, ids.ID) (memory.ConversationExtractResult, error)`
- `memory.ConversationExtractDeps` 字段（见任务 4.1）
- 幂等语义：按 `conversation_id` 先删后插；产物是 `memory` 候选（`status=active`，`epistemic_type` 由 LLM 给），**不产生 `agent_proposal`**（见 D-A）。

---

## 覆盖与自检清单（对齐 §6.3 / §12）

- [x] **§6.3 迁移**：`memory` 加 `conversation_id BIGINT NULL` + `idx_mem_conversation`，`session_id` 放宽 `NULL`（任务 1）。
- [x] **§6.3 溯源**：对话记忆用 `conversation_id` 溯源到具体对话（采纳 spec "倾向前者"方案）。
- [x] **§6.3 类型一致**：可空标量走指针（`SessionID`/`ConversationID *ids.ID`），对齐 `agent_message.go`/`EventAt`；可空 JSON 仍 `*json.RawMessage`（本计划未新增 JSON 列）。
- [x] **§12 范式复用**：`ListByConversation → 组装块 → Extractor(窗口/ExtractWindow) → ParseCandidates → ApplyGate(GateConfig) → ResolveTopics → 落库`，与 `stage_extract` 同构（任务 4）。
- [x] **§12 候选而非修改**：直插 `memory`（候选流程），**不建 `agent_proposal`**（决策 D-A）；「修改」仍只经 §8 闸门——含提示注入防线论证。
- [x] **幂等/dedup**：按 `conversation_id` 先删后插 + `NormalizeTitle` 跨源佐证去重（任务 4.3 / 6）。
- [x] **并发安全**：单事务落库；佐证上调走 SQL 原子 `LEAST`（`BumpConfidenceExt`）；协调者串行化同 `cid` 抽取（任务 7）。
- [x] **包始终可编译 / session 路径绿**：`InsertExt` 不动，仅新增 `InsertConversationExt`；唯一非测试写入点 `stage_extract.go` 改一行；补 session 回归测试（任务 2.6 / 6.1-3）。
- [x] **safe-mode 一致性**：加列同时加结构体字段，避免 `SELECT *` 破裂（任务 2.1 / 6.1-1）。
- [x] **测试分层**：单测 mock LLM 无 MySQL（任务 5）；repo/e2e 用 `TEST_MYSQL_DSN` + `t.Cleanup`（任务 6）。
- [x] **迁移号协调点**：000006 撞号提示醒目（任务 1 顶部）。
- [x] **不越界**：不改 `main.go`/`internal/agent`/`internal/api`/`web`；端点/cron 作为 Coordinator 对接说明（任务 7）。

## 交付物清单

| 文件 | 动作 | 任务 |
|---|---|---|
| `migrations/000006_conversation_memory.up.sql` / `.down.sql` | 新增 | 1 |
| `internal/repo/memory.go` | 改（字段指针化 + `InsertConversationExt` + `DeleteByConversationExt`） | 2 |
| `internal/repo/memory_topic.go` | 改（`DeleteByConversationExt`） | 2 |
| `internal/pipeline/stage_extract.go` | 改一行（`SessionID: &sessionID`） | 2 |
| `prompts/conversation_extraction_v1.md` | 新增 | 3 |
| `internal/memory/conversation.go` | 新增（`ExtractConversation` + 组装 + `commitConversation`） | 4 |
| `internal/memory/conversation_test.go` | 新增（单测） | 5 |
| `internal/repo/memory_conversation_test.go` | 新增（集成） | 6 |
| `internal/memory/conversation_e2e_test.go` | 新增（可选 e2e） | 6 |

## 执行顺序建议

1 → 2 → 3（可与 2 并行）→ 4 → 5 → 6 → 7（对接说明，随时可读）。每步 `go build ./...` + 相关测试后再进下一步；任务 2 完成即应先跑 `make test-integration` 确认 session 路径不回归，再叠加对话路径。

