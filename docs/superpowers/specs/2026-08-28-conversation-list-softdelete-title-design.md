# 会话列表页：软删除 + 标题编辑 + 自动生成标题

- 日期：2026-08-28
- 分支：`worktree-agent-soul`（问知微特性线）
- 范围：「问知微」对话列表页（`/api/agent/conversations`）三项增强——软删除、手动编辑标题、模型自动生成简短标题。
- 关联规格：agent-chatbot spec（`docs/superpowers/specs/2026-08-24-agent-chatbot-system-design.md`）。
- 落地文件：`internal/repo/agent_conversation.go`、`internal/repo/agent_message.go`、`internal/agent/handlers.go`、`internal/agent/orchestrator.go`、`migrations/000020_*`、`web/index.html`、`web/app.js`、对应 `_test.go`。

## 1. 需求与决策（已与用户确认）

| 项 | 决策 |
|----|------|
| 软删除语义 | **= archived**：删除即把 `status` 从 `active` 改为 `archived`，会话从列表消失，数据与消息全保留。与现有 `status active\|archived` 字段合一，零迁移。 |
| 区分手动/自动标题 | **加 `title_source` 列**：`''`(未设/占位) / `'manual'`(用户改过) / `'auto'`(模型生成)。 |
| 自动生成时机 | **每轮对话结束后检查**，累计到**第 2 轮**且标题可生成时，**异步**生成一次。前 2 轮留窗口，让用户有一次澄清问题的机会（首轮可能是寒暄/追问，第 2 轮内容更充分）。 |
| 触发条件 | 标题为空 / 等于「新对话」占位 / `title_source='auto'`（即用户从未手动改过）。`title_source='manual'` 永不覆盖。 |
| 生成失败 | **静默**，保留原标题，仅记日志，绝不报错、不中断主流程。 |

## 2. 现状（调研实证，file:line）

- 会话模型 `internal/repo/agent_conversation.go:13` `AgentConversation`：已有 `Status string`（`active|archived`）、`Title string`；`List`（`:50`）只查 `status='active'` → **软删除已半成品**（置 archived 即从列表消失）。
- 现有 repo 方法：`Create / Get / List / Touch / SetDSHSession`（`agent_conversation.go:27-71`）。**缺**：改标题、软删、统计消息数。
- 现有 handler（`internal/agent/handlers.go`）：`createConversation / listConversations / getConversation / postMessage / handleWS`。路由在 `RegisterAgent`（`:33-37`）。**缺**：改标题、删除、生成标题端点。
- 轮次广播器 `internal/agent/hub.go` `runTurn`（`:129`）在独立 goroutine 跑 `orch.RunTurnStream`，收尾 `endTurn`（`:116`）。
- orchestrator `runTurn`（`orchestrator.go:62`）收尾处 `Touch`（`:176`）——自动生成标题的挂载点。
- LLM 现成：`internal/provider/llm.go` `ArkLLM.Chat(ChatRequest{Model,System,User,Temperature})`；main.go 已装配 `llm` + `agentModel`（`cmd/zhiwei-server/main.go:189,412`）。
- 前端列表项 `web/index.html:752-755`：只渲染标题 + 时间，无编辑/删除入口。标题展示 `{{ c.title || '新对话' }}`（`:753`）。
- **多租户/IDOR 地基已就绪**：repo 的 `Get/List` 均 `AND user_id=?`；handler 均 `reqUserID`。新增方法/端点沿用同一模式。
- **迁移号约定**：main 最新为 `000019_agent_config`，本特性新增 **`000020`**。⚠️ 合并回 main 前须再核对 main 最新迁移号重编号（项目已知并行分支撞号坑，见 `[[zhiwei-db-per-feature-convention]]`）。

## 3. 数据层

### 3.1 迁移 `000020_conversation_title.up.sql`
```sql
-- 区分标题来源：''(未设/占位) | 'manual'(用户手动改) | 'auto'(模型生成)。
-- 用于「自动生成」判定：manual 永不覆盖；空/auto 可生成。
ALTER TABLE agent_conversation
  ADD COLUMN title_source VARCHAR(16) NOT NULL DEFAULT '' AFTER title;
```
`.down.sql`：`ALTER TABLE agent_conversation DROP COLUMN title_source;`

### 3.2 repo `agent_conversation.go`
- `AgentConversation` 结构体加 `TitleSource string \`db:"title_source" json:"title_source"\``。
- **`UpdateTitle(ctx, userID, id, title, source) error`**：
  ```sql
  UPDATE agent_conversation SET title=?, title_source=? WHERE id=? AND user_id=?
  ```
  行级 user_id 过滤（IDOR 防护）。`RowsAffected==0` → 返回 `sql.ErrNoRows`（=越权或不存在，handler 转 404）。
- **`Archive(ctx, userID, id) error`**（软删）：
  ```sql
  UPDATE agent_conversation SET status='archived' WHERE id=? AND user_id=? AND status='active'
  ```
  幂等：已是 archived 则 0 行、返回 nil（不报错）。
- **`TitleState(ctx, userID, id) (title, source string, err error)`**：`SELECT title, title_source ... WHERE id=? AND user_id=?`，供自动生成前判状态。
- `Create` 读回行带上 `title_source`（保持 I2 读回惯例，默认空串）。

### 3.3 repo `agent_message.go`
- **`CountByConversation(ctx, userID, convID) (int, error)`**：
  ```sql
  SELECT COUNT(*) FROM agent_message WHERE conversation_id=? AND user_id=? AND role='user'
  ```
  统计该会话用户消息数，判定是否到第 2 轮。带 user_id 过滤。

## 4. API 层（`internal/agent/handlers.go` + 路由）

| 方法 | 路径 | 请求体 | 响应 | 说明 |
|------|------|--------|------|------|
| `PATCH` | `/api/agent/conversations/{cid}` | `{title}` | 200 conversation | 改标题，写 `title_source='manual'` |
| `DELETE` | `/api/agent/conversations/{cid}` | — | 204 | 软删（status→archived），幂等 |
| `POST` | `/api/agent/conversations/{cid}/title/generate` | — | 200 `{title,title_source}` | 手动触发一次自动生成（兜底） |

- 所有端点 `reqUserID` + repo user_id 过滤；越权/不存在 → 404。
- `PATCH`：校验 `title` 非空、截断到 256；`UpdateTitle(uid,cid,title,'manual')`；读回完整行返回。
- `DELETE`：`Archive(uid,cid)`；204 No Content。
- `title/generate`：复用 §5 的生成函数同步跑一次（脱离请求 ctx，带超时）；成功写 `auto` 并返回新标题，失败返回当前原标题（200，不报错）。

## 5. 自动生成标题

### 5.1 挂载点：orchestrator 可选回调
orchestrator 保持对 `provider` 无依赖（测试友好，对齐现有 `Persona`/`Ctx` 可选回调模式）：
```go
// OnTurnComplete 可选：每轮 runTurn 收尾（Touch 之后）调用，供主装配挂「自动生成标题」等
// 每轮副作用。必须快速返回、不得阻塞 runTurn——耗时工作（LLM 生成）由实现方自行起 goroutine。
// nil → 不调用（既有行为/测试不变）。
OnTurnComplete func(ctx context.Context, conv *repo.AgentConversation)
```
`runTurn` 收尾（`Touch` 后、`turn_end` 帧前）`if o.OnTurnComplete != nil { o.OnTurnComplete(ctx, conv) }`。

### 5.2 装配（`cmd/zhiwei-server/main.go`）
给 `orch.OnTurnComplete` 赋一个闭包，内部起 goroutine 跑 `generateTitle(...)`（§5.3），注入 `llm`、`agentModel`、`agentConvs`、`agentMsgs`、`log`。

### 5.3 `generateTitle(ctx, conv)` 判定 + 生成（agent 包新文件 `title.go`）
```
1. state, _ := convs.TitleState(uid, convID)        // 取 title + title_source
2. if state.source == 'manual' → return             // 用户改过，永不覆盖
3. n, _ := msgs.CountByConversation(uid, convID)
   if n < 2 → return                                // 未到第 2 轮
4. if !(title=='' || title=='新对话' || source=='auto') → return  // 不满足触发条件
5. msgs := msgs.ListByConversation(uid, convID)      // 取对话文本（前若干条 user/assistant）
6. resp, err := llm.Chat(ctx, ChatRequest{
       Model: agentModel, System: titlePrompt, User: buildTitleInput(msgs), Temperature: 0.3,
   })
   if err != nil → log + return                      // 失败静默
7. title := sanitize(resp.Content)                   // 去引号/换行/截断 256，限制 ≤15 字
   if title=='' → return
8. convs.UpdateTitle(uid, convID, title, 'auto')
```
- ctx：脱离请求的独立 ctx + 超时（如 30s），异步不阻塞主流程。
- **幂等/竞态**：第 8 步 `UpdateTitle` 前可再读一次 `title_source`，若已被并发改成 `manual` 则放弃（CAS 风格，避免覆盖用户刚改的标题）。实现为读-判-写，最坏重试一次。
- `titlePrompt`：内联常量，要求「根据对话生成一个不超过 15 个中文字符的简短标题，只输出标题本身，不要引号、不要解释、不要标点结尾」。

## 6. 前端（`web/index.html` + `web/app.js`）

- **列表项**（`index.html:752-755`）每行加两个按钮（`@click.stop` 防触发选中）：
  - ✏️ 编辑：行内标题变 `<input>`（`v-model` 临时值），失焦/回车 `PATCH` 成功后重拉列表；Esc 取消。
  - 🗑 删除：`confirm` 后 `DELETE`，从 `agentConversations` 移除（或重拉列表）。
- **删除当前选中会话**：清空 `agentConvId` + `agentMessages`，关闭 WS，提示「已删除」。
- `loadAgentConversations` 在删/改后重拉以同步。
- `app.js` 新增 `editAgentConversation(c)`、`saveAgentTitle(c)`、`deleteAgentConversation(c)`，并 expose 到模板。
- 改完跑 `make hash-web` 更新 `index.html` 引用指纹。

## 7. 测试

- **repo**（`agent_conversation_test.go` / `agent_message_test.go`）：`UpdateTitle`（写 manual、越权 0 行→ErrNoRows、不存在）、`Archive`（active→archived 后 List 不再出现、幂等、越权）、`TitleState`、`CountByConversation`（0/1/2 轮）。用隔离库 `zhiwei_test_agentchat`（`repotest.DSN`）。
- **handler**（`handlers_test.go`）：`PATCH` 改标题→200 且 title_source=manual；`DELETE`→204 且列表查不到；越权→404；`title/generate` 手动触发。
- **自动生成**（`title_test.go`）：用 fake LLM + orchestrator `OnTurnComplete` 回调注入。
  - 第 2 轮 + 标题空 → 生成 `auto` 标题。
  - `title_source='manual'` → 不覆盖。
  - `< 2` 轮 → 不生成。
  - LLM 失败 → 静默，原标题不变。
  - 生成中用户并发改标题为 manual → CAS 放弃，不覆盖。

## 8. 风险与边界

- **迁移撞号**：本特性 `000020`，合并回 main 前核对 main 最新号重编号（`[[zhiwei-db-per-feature-convention]]`）。
- **IDOR**：所有新增 repo 方法带 user_id 过滤，handler 全走 `reqUserID`，沿用 2B-B 多租户地基。
- **自动生成成本**：每会话最多一次 LLM 调用（第 2 轮后），异步不阻塞；失败静默。
- **标题长度**：生成结果截断 256、限 ≤15 字，防 LLM 输出过长。
- **不在本期范围**：回收站/恢复（archived 会话的还原 UI）、跨会话标题去重、删除确认的撤销（undo）。
