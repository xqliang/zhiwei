# 知微 Agent 行为路由 + 检索种子门控 设计（Phase 1/3）

- 日期：2026-08-31
- 分支/worktree：`worktree-agent-behavior-routing`
- 范围：修「未知/专业/常识问题被生硬关联用户数据」的 bug——改默认 system prompt 加**分场景路由**；给每轮自动注入的检索种子加**个人信号门控**并**改写措辞**。**不含**联网搜索（Phase 2）与设置页插件管理（Phase 3）。
- 用户痛点（截图实证）：问「ASL/ATH/AIP 是什么」→「展开讲讲」时，模型自主调用 `search_memory`/`get_metrics`，翻出一堆 Claude、体重、C2 调用等无关记忆。用户诉求：未知词应触发联网搜索、不要生硬关联用户数据。

## 1. 目标与决策（已与用户确认）

| 项 | 决策 |
|----|------|
| 人设引导方式 | **只改默认 system prompt**（`DSH_SYSTEM_PROMPT` 默认值），不往 `agent_config` 播种可编辑引导 |
| 检索种子 | **加个人信号门控 + 改写措辞**（去掉「我的」归属暗示） |
| 联网搜索 | **不在本期**；Phase 2 单独 spec（先 spike `dsh-free-search`，失败回退原生 Go `web_search`，免 key 抓取 + 配置页可填 API key） |
| 分期 | Phase 1（本设计）独立可发布；Phase 2/3 视情况拆分 spec |

## 2. 现状（调研实证，file:line）

- **无联网搜索**：全仓库无任何 web_search/联网工具（已 grep 确认）。模型只有读用户数据的工具：`search_memory`、`get_metrics`、`get_timeline`/`get_topics`/`get_todos`、画像等。
- **默认人设把模型往用户数据带**：`internal/config/config.go:165` `DSH_SYSTEM_PROMPT` 默认 = "你是知微(zhiwei)个人智能体，基于用户的记忆/时间线/话题/待办用简体中文亲切、简洁地回答；需要时调用工具读取用户数据，不要编造。"
- **每轮自动注入检索种子**：`internal/agent/context.go:107` `Seeds()`——每轮对用户问题跑 `Retrieve.Search`，命中就注入 `可能相关的我的记忆（供参考，不必逐条复述）：\n- <title>`。仅当配了 `ARK_AUDIO_API_KEY`（`Retrieve != nil`）时触发。种子块本就「不预览」（`web/app.js:3002`）。
- **工具调用是模型自主**：`search_memory`/`get_metrics` 由 dsh 模型据工具描述自行决定调用，无 Go 代码强制。
- **设置页预览自动跟随**：`internal/agent/handlers.go:92` `getConfig` 返回 `system_prompt: h.SystemPrompt`（进程级，只读）；前端 `web/app.js:2996`/`3011` 展示。故改 `config.go` 默认值，预览自动更新，**无需改前端**。**无测试断言该默认串的精确文本**（已确认）。

## 3. 改动一：默认 system prompt 加分场景路由

**文件**：`internal/config/config.go:165`

**现状**：
```
你是知微(zhiwei)个人智能体，基于用户的记忆/时间线/话题/待办用简体中文亲切、简洁地回答；需要时调用工具读取用户数据，不要编造。
```

**改为**（加路由，保留简体中文亲切简洁 + 不编造）：
```
你是知微(zhiwei)，用户的个人助理，用简体中文亲切、简洁地回答。
请按问题类型分场景处理：
1) 一般知识、专业术语、名词解释、常识等问题：直接基于你自己的知识回答，不要调用读取用户数据的工具，也不要生硬地关联到用户的记忆或指标。
2) 只有问题明确关于用户本人（含「我/我的」或涉及其日程/记录/指标/待办等）时，才调用工具读取该用户的数据作答。
3) 不确定或不懂时：如实说明，不要编造，也不要用用户的数据拼凑答案。
只有在需要用户本人数据时才调用工具；不要臆测用户没有的记忆或数据。
```

> 关键效果：名词解释/常识题（如「ASL 是什么」）→ 直接自答，不调工具、不关联记忆；个人问题才调工具；不懂就直说。这直接消除截图中「展开讲讲」去翻记忆的行为。

## 4. 改动二：检索种子加个人信号门控 + 改写措辞

**文件**：`internal/agent/context.go` `Seeds()`（`:107-121`）

**门控**：仅当 query 命中个人信号才跑召回+注入；否则返回 `""`（顺带省一次 embedding 调用）。
```go
// personalSignal 命中「问题关于用户本人」的信号；仅此时才召回并注入种子。
// 常识/名词解释题（如「ASL 是什么」）不含这些词 → 不注入，从源头避免「啥都跟你数据有关」的误导。
var personalSignal = regexp.MustCompile(`我|咱|自己|本人`)
```
在 `Seeds()` 里，`Retrieve.Search` 之前加：`if !personalSignal.MatchString(query) { return "" }`。

**措辞**：注入头由
`可能相关的我的记忆（供参考，不必逐条复述）：`
改为
`与该问题可能相关的背景记忆（仅供参考，不相关请忽略）：`
（去掉「我的」归属暗示；种子块不预览，改措辞不影响设置页。）

**取舍（已确认接受）**：极少数不含「我/我的」的个人问法（如「体检报告怎么看」「推荐适合我的…」）不会自动注入种子；但模型仍可在确属个人问题时**自主**调用 `search_memory`（人设未禁止），影响轻微。后续可加「相似度兜底」门控（需给 `Retriever.Search` 增加返回分数的变体）——不在本期。

## 5. 前端

**无需改动**。`system_prompt` 预览自动跟随 `config.go` 默认值；种子块本就「不预览」。

## 6. 测试

1. **`internal/agent/retrieval_wire_test.go` `TestOrchestratorSeedsInjection`**：原 query `"猫应该怎么养"` 无个人信号，门控后会**不再注入**，需改为含个人信号的 query（如 `"我的猫应该怎么养"`）以保持「命中即注入」的正例断言；并断言新措辞头存在、旧措辞头（「可能相关的我的记忆」）不存在。
2. **新增负例**（同文件或 `context_test.go`）：`ProfileContext.Seeds` 对无个人信号的 query（如 `"ASL 是什么"`、`"量子计算是什么"`）即使有相关记忆也返回 `""`（不注入）。
3. **默认 system prompt**：`internal/config` 或 `internal/agent/handlers_test.go` 加一条断言——默认 `DSHSystemPrompt` 含路由关键词（「分场景」「直接基于你自己的知识」「如实说明」），锁定行为意图（不断言整串，避免脆弱）。
4. 回归：`go test ./internal/agent/... ./internal/config/...` 全绿。

## 7. 风险

- **门控过严漏种子**：已确认为可接受的轻微取舍；模型可自主 `search_memory` 兜底。
- **人设措辞被模型弱化**：模型可能仍偶发关联记忆；路由指令已尽量明确（「不要调用读取用户数据的工具」）。若实测仍不理想，属 Phase 2 引入联网后进一步收敛的范畴。
- **无回归风险到持久化**：两处改动都只影响「发给 dsh 的文本」，不改落库（沿用 D2 约束）。

## 8. 不在本期（后续 Phase）

- **Phase 2**：联网搜索。先 spike `dsh-free-search`（无头 `bin.js` + 生成式 cordis 能否加载、模型能否拿到搜索工具）；失败回退原生 Go `web_search` MCP 工具，**免 key 抓取（Bing/DDG/SearXNG）+ 配置页可填 API key**。
- **Phase 3**：设置页统一管理技能/插件/MCP（启禁）。插件在 dsh 启动时加载，启禁需重启运行时（异于 MCP 热插拔）。
