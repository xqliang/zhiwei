# 代办/Topic 质量改进设计（去重 / 合并 / 时间戳）

- 日期：2026-08-20
- 状态：待审阅
- 上游：`docs/superpowers/specs/2026-08-20-todo-topic-association-design.md`（多对多基础，已实现）
- 范围：A 代办按名去重、B topic 动态合并、C 代办生成/处理时间显示。

---

## A. 代办按名去重（落库去重 + 存量清理）

### A.1 归一化
- 新增 `internal/repo/normalize.go`：`NormalizeTitle(s) string` = `trim` + `ToLower` + 仅保留 unicode 字母/数字（去标点空格）。纯逻辑 + 单测。
  - 例："给 Tom 发邮件" / "给Tom发邮件" / "给 tom 发邮件" → 均归一为 "给tom发邮件"。
- 放 repo 包（pipeline 与 repo 清理都用，且不破坏 repo↔memory 分层）。

### A.2 落库去重（commitExtract）
- `TodoRepo.ListOpenTitles(ctx, userID) ([]string, error)`：返回未关闭（suggested+confirmed）todo 的 title。
- `commitExtract` 插 todo 前：取该集合，`NormalizeTitle` 后入 set；遍历候选时，新 suggested todo 归一化标题命中 set → **跳过**（其 memory + memory_topic/todo_topic 照常落），不命中则插入并把归一标题加入 set（顺带去批内重复）。
- 范围：只对 suggested 新 todo 去重；比对对象是未关闭 todo。done/dismissed 不挡（已关闭，周期性新实例合理）。
- 边界：被跳过的 todo 若有 todo_topic 手动关联（本 session 重跑快照），不重链（该 todo 不重建）；其语义由保留的 open todo 承载。可接受。

### A.3 存量清理
- `TodoRepo.DedupSuggested(ctx, userID) (int, error)`：取全部 suggested，按 `NormalizeTitle` 分组，每组保留 `created_at` 最旧一条，其余置 `dismissed`。单事务。
- `cmd/dedup-todos/main.go`：连库调一次 `DedupSuggested(1)`，打印清理数。一次性 `go run ./cmd/dedup-todos` 跑一次（需 source .env）。

### A.4 测试
- `NormalizeTitle` 单测（含 CJK、标点、大小写、空格）。
- commit 去重集成：预置一条 open todo「给 Tom 发邮件」→ fake LLM 再产出「给Tom发邮件」→ 断言不新增 todo、memory 照常。
- `DedupSuggested` 集成：多条同名 suggested → 保留最旧、其余 dismissed。

---

## C. 代办生成/处理时间显示（纯前端）

- todo 卡片显示 `生成 {fmtTime(td.created_at)}`；`td.status !== 'suggested'` 时追加 ` · 处理 {fmtTime(td.updated_at)}`。
- `updated_at` = 最后一次状态变更（`UpdateStatus` 是唯一 UPDATE，`ON UPDATE CURRENT_TIMESTAMP` 自动刷新）= 进入当前（非 suggested）状态的时间。suggested 未处理不显处理时间。
- `created_at`/`updated_at` API 已返回，无后端/迁移改动。三组待办卡（待确认/进行中/已完成）均加。

---

## B. topic 动态合并（手动智能合并 + 疑似提示）

### B.1 生成时强化复用（prompt v3）
- 新建 `prompts/extraction_v3.md`：在 topic 归属规则强化——"若候选主题与已有 topic **语义相近**，优先复用其 topic_id；只有确实新主题才给 suggested_name；避免造近重复名（如已有「SDPC俱乐部活动」就别再造「…准备」）"。`main.go` promptPath → v3，版本号记 trace。
- `ResolveTopics` 已有**同名**合并保留；语义复用靠 LLM 选已有 topic_id（不靠规则模糊匹配，避免误并不同主题）。

### B.2 疑似可合并提示（轻量、前端）
- Topics 页对 topic 列表做客户端相似度启发：归一化（复用 A.1 的思路，前端用 `normalizeTitle`）后，两两满足「互为包含」或 Levenshtein 比 > 0.85 → 该 topic 卡片加「疑似可合并」徽标 + 点击进入合并流。
- 纯前端，无后端/LLM；只标记不自动合并。能抓字面近重复（SDPC 那种），语义相近的由手动合并覆盖。

### B.3 手动智能合并（LLM 提议 → 用户确认 → 应用）
- `POST /api/topics/consolidate`（空 body）→ 后端调 LLM（`prompts/topic_consolidate_v1.md`，输入该用户全部 active/suggested topic 列表，输出合并组）→ 返回提议：
  ```json
  {"groups":[{"canonical_name":"SDPC俱乐部活动","member_ids":["<tid1>","<tid2>"]}]}
  ```
  （canonical 优先用某 member 的现名或 LLM 提炼的新名；不在此步改库。）
- 前端展示提议组（可编辑 canonical 名、勾选/取消成员）→ 用户确认 → `POST /api/topics/merge` body：
  ```json
  {"groups":[{"canonical_name":"SDPC俱乐部活动","member_ids":["<tid1>","<tid2>"]}]}
  ```
- **merge 后端单事务**：每组——若 canonical 名命中已有 active/suggested topic 则复用其 id，否则新建 `status=active, created_by=ai` topic；把各 member 的 `memory_topic`/`todo_topic` 关联 `INSERT IGNORE` 迁到 canonical（去重），删 member 的关联行，member topic 置 `dismissed`。
- UI：Topics 页「智能合并」按钮 → consolidate → 展示组（编辑+确认）→ merge → 刷新。

### B.4 测试
- consolidate handler（fake LLM 返回组）→ 返回结构正确。
- merge 集成：2 个 topic 各带 memory/todo 关联 → merge → canonical 聚合所有关联、member dismissed、无重复关联。
- 前端相似度启发（可选，纯 JS `node --check` 或手测）。

---

## 顺序与非目标
- 实现顺序：C（最小）→ A → B1（prompt）→ B2（前端提示）→ B3（后端 consolidate/merge + 前端按钮）。
- 非目标：不做自动定期合并（用户选手动+提示）；不做跨用户；不做 topic 父子层级；不改多对多基础模型。
