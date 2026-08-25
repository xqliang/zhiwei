# 实施计划：Agent Chatbot P2d — 写-提议闸门（propose_* 工具 + 确认/放弃）

- 日期：2026-08-26
- 分支 / worktree：`feat/agent-chatbot`
- 范围：**后端**——「帮我改我的信息」的两段式写入闸门。agent 只能**提议**（建 `agent_proposal(pending)`，绝不静默改），用户**确认**后才在单事务内落库（apply-once）。覆盖 memory/topic/todo。前端**确认卡**单列一个跟进 pass（本计划只做后端 + 返回结构供渲染）。
- 关联规格：`docs/superpowers/specs/2026-08-24-agent-chatbot-system-design.md` §7.2（写工具）、§8（提议→确认闸门 + 提示注入防线）、§9.2（确认卡）。
- 现状：`agent_proposal` 表 + `repo.AgentProposalRepo`（`Create` pending / `Get` / `ListPending` / `Resolve(ext, id, status, appliedRef) (bool applied, err)` CAS apply-once，收 `ExecerContext` 供同事务调用）已在 P1 落地。MCP server（`internal/agent/mcp_server.go`）+ 读工具（`mcp_tools.go`）+ 对话/REST（`handlers.go`）已在 P2a/b/c 落地。

---

## 关键设计（务必先读）

### D1：propose_* 工具只建提议，绝不 mutate（§7.2/§8 + 注入防线）
每个写工具：① 读当前值（组 `{old}`）② 组 `payload={old,new,args}` ③ `Proposals.Create(pending)` ④ **返回该 proposal 的结构**（供前端渲染确认卡）。**工具永不改目标行**。这是提示注入的根防线：转写/对话里的「把 X 改成 Y」最多生成一个待确认提议，永远要人点确认。全部限 `user_id=1`。

### D2：确认在单事务内 apply-once（§8）
`POST /confirm`：`Get` 提议 → 若已非 pending 直接返回（幂等 no-op）→ 否则 `BeginTxx` → 按 `kind` 用 **`*Ext`（事务版）** repo 方法落领域改动 → `Proposals.Resolve(tx, id, "applied", appliedRef)` → **若 `Resolve` 返回 `false`（并发/重复确认的输方）则 `Rollback`**（apply-once）→ 否则 `Commit`。领域写与提议置终态**同一事务**，杜绝「改了库但提议没标 applied」→ 重复确认重复改。

### D3：需要新增 `Ext`（事务）变体
现有领域变更方法多为非事务（用 `r.DB`）。apply 必须与 `Resolve` 同事务，故新增（SQL 与非事务版一致，只把 `r.DB` 换成 `ext`；非事务版改为委托 Ext 传 `r.DB`，零行为变化）：
- `MemoryRepo.SaveExt(ctx, ext, *Memory)`（`Save` 委托它）
- `TodoRepo.UpdateStatusExt(ctx, ext, id, status)`（`UpdateStatus` 委托）；`InsertExt` 已有（todo_create 用）
- `TopicRepo.UpdateNameExt(ctx, ext, id, name)` / `UpdateStatusExt(ctx, ext, id, status)`（`UpdateName`/`UpdateStatus` 委托）

---

## 任务 1 — repo 事务变体（`internal/repo/{memory,todo,topic}.go`）

**做法**：加 `*Ext` 方法，原方法委托之。示例（memory）：
```go
// SaveExt 事务版 Save（供确认闸门与 Resolve 同事务落库）。
func (r *MemoryRepo) SaveExt(ctx context.Context, ext ExecerContext, m *Memory) error {
	_, err := ext.ExecContext(ctx, `
UPDATE memory SET title = ?, content = ?, status = ?, version = ? WHERE id = ?`,
		m.Title, m.Content, m.Status, m.Version, m.ID.Int64())
	return err
}
func (r *MemoryRepo) Save(ctx context.Context, m *Memory) error { return r.SaveExt(ctx, r.DB, m) }
```
同法加 `TodoRepo.UpdateStatusExt`（`UPDATE todo SET status=? WHERE id=?`）、`TopicRepo.UpdateNameExt`（`UPDATE topic SET name=? WHERE id=?`，保持 `UpdateName` 原有的规范化/校验逻辑——**先读现有 `UpdateName` 实现，把其 SQL 主体搬进 Ext，非事务版委托**）、`TopicRepo.UpdateStatusExt`。
- **步骤**：先读每个原方法实现，抽出 SQL 到 Ext，原方法委托，`go build ./internal/repo/...` + 现有 repo 测试保持绿（回归：`Save`/`UpdateStatus`/`UpdateName` 行为不变）。
- **测试**：`internal/repo/*_test.go` 各加一条「Ext 版与非事务版等价」断言（传 `db` 调 Ext，效果同原方法）。

## 任务 2 — MCPDeps 加 Proposals + 写工具入参/出参（`internal/agent/mcp_server.go` + 新 `mcp_write_tools.go`）

1. `mcp_server.go`：`MCPDeps` 加 `Proposals *repo.AgentProposalRepo`；`NewMCPServer` 里在 `registerReadTools(s,d)` 后调 `registerWriteTools(s, d)`。
2. 新文件 `internal/agent/mcp_write_tools.go`：`registerWriteTools(s *mcp.Server, d MCPDeps)` 注册 7 个工具。每个入参含 `rationale string`（agent 给用户看的理由）。**统一小工具** `proposeResult(p *repo.AgentProposal) (*mcp.CallToolResult, any, error)`：把 proposal 序列化成 `TextContent`（含 id/kind/target_kind/target_id/payload/rationale/status）返回，供前端确认卡。

工具清单（`ids` 入参用 string + `ids.ParseID`；读当前值失败→返回 tool-error 让模型知道）：
| 工具 | 入参 | kind / target_kind | payload |
|---|---|---|---|
| `propose_memory_edit` | `memory_id, new_title?, new_content?, new_type?, rationale` | memory_update / memory | `{old:{title,content,type}, new:{...仅给出的字段}}` |
| `propose_memory_dismiss` | `memory_id, rationale` | memory_dismiss / memory | `{old:{status}}` |
| `propose_topic_rename` | `topic_id, new_name, rationale` | topic_rename / topic | `{old:{name}, new:{name}}` |
| `propose_topic_confirm` | `topic_id, rationale` | topic_confirm / topic | `{old:{status}, new:{status:"active"}}` |
| `propose_topic_dismiss` | `topic_id, rationale` | topic_dismiss / topic | `{old:{status}, new:{status:"dismissed"}}` |
| `propose_todo_create` | `title, due_at?, topic_id?, rationale` | todo_create / todo（target_id 空） | `{new:{title,due_at,topic_id}}` |
| `propose_todo_status` | `todo_id, new_status, rationale` | todo_status / todo | `{old:{status}, new:{status}}` |

- 每工具体：`ids.ParseID` 目标 → `d.Memory/Topic/Todo.Get` 读现值组 `{old}`（todo_create 无需读）→ 组 `payload`（`json.Marshal`）→ `d.Proposals.Create(&repo.AgentProposal{Kind, TargetKind, TargetID(可空), Payload, Rationale})` → `proposeResult(p)`。**不调任何 Update/Save/Insert**。
- 给 `propose_memory_edit` 写完整代码；其余按同构模式（plan 里逐个给签名 + payload 组装，boilerplate 可参照第一个）。
- **测试**（`mcp_write_tools_test.go`，集成，隔离库 + t.Cleanup）：调 `propose_memory_edit` 后断言 ① 返回的 proposal `status=pending`、payload 含 old/new ② **memory 行未变**（Get 原样）③ `agent_proposal` 多一条 pending。对 topic_rename/todo_status 各一条同类断言。

## 任务 3 — 确认/放弃端点（`internal/agent/proposals.go` 新文件 + `handlers.go` 注册）

`RegisterProposals(r chi.Router, deps ProposalDeps)`，`ProposalDeps{DB *sqlx.DB, Proposals, Memories, Topics, Todos, MemoryTopics}`：
- `POST /api/agent/proposals/{id}/confirm` → `confirmProposal`
- `POST /api/agent/proposals/{id}/dismiss` → `dismissProposal`
- `GET  /api/agent/proposals`（可选）→ `ListPending` JSON

`confirmProposal`（apply-once，D2）——完整代码：
```go
func (d ProposalDeps) confirmProposal(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil { writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"}); return }
	p, err := d.Proposals.Get(r.Context(), id)
	if err != nil { writeJSON(w, http.StatusNotFound, map[string]string{"error": "proposal not found"}); return }
	if p.Status != "pending" { writeJSON(w, http.StatusOK, p); return } // 幂等：已终态直接回

	tx, err := d.DB.BeginTxx(r.Context(), nil)
	if err != nil { writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()}); return }
	defer func() { _ = tx.Rollback() }() // Commit 后 no-op

	appliedRef, err := d.applyInTx(r.Context(), tx, p) // 按 kind 落领域改动, 返回 appliedRef(如新 todo id / memory id)
	if err != nil { writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()}); return }

	applied, err := d.Proposals.Resolve(r.Context(), tx, id, "applied", appliedRef)
	if err != nil { writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()}); return }
	if !applied { // 输掉并发竞争：回滚领域写, apply-once
		writeJSON(w, http.StatusConflict, map[string]string{"error": "提议已被处理"}); return
	}
	if err := tx.Commit(); err != nil { writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()}); return }
	p2, _ := d.Proposals.Get(r.Context(), id)
	writeJSON(w, http.StatusOK, p2)
}
```
`applyInTx(ctx, tx, p)` 按 `p.Kind` switch（解析 `p.Payload` 的 `new`）：
- `memory_update`：`Memories.Get(*p.TargetID)` → 用 payload.new 覆盖 title/content/type(给出的)、`Version+1` → `Memories.SaveExt(tx, m)`；`appliedRef=p.TargetID`。
- `memory_dismiss`：Get → `Status="dismissed"`、`Version+1` → `SaveExt`。
- `topic_rename`：`Topics.UpdateNameExt(tx, *p.TargetID, new.name)`。
- `topic_confirm`：`Topics.UpdateStatusExt(tx, *p.TargetID, "active")`。
- `topic_dismiss`：`Topics.UpdateStatusExt(tx, *p.TargetID, "dismissed")`。
- `todo_create`：构造 `Todo{title,due_at,status:"confirmed"...}`（含 `ids.New()`），`Todos.InsertExt(tx, [t])`；若 payload.new.topic_id 存在，`TodoTopics.InsertExt`；`appliedRef=新 todo id`。
- `todo_status`：`Todos.UpdateStatusExt(tx, *p.TargetID, new.status)`。
- default：`return nil, fmt.Errorf("未知提议 kind: %s", p.Kind)`。

`dismissProposal`：`Proposals.Resolve(r.Context(), d.DB, id, "dismissed", nil)`；`applied==false` 时若 Get 显示已 dismissed 则幂等 200，否则 409。

- **测试**（`proposals_test.go`，集成 + t.Cleanup）：
  1. **memory_update 确认**：建 memory → propose_memory_edit → confirm → memory title/content 变、version+1、proposal=applied、`applied_ref`=memory id。
  2. **apply-once**：同一 proposal confirm 两次，第二次 200 幂等且 memory 不再变（version 不再涨）。
  3. **dismiss**：propose → dismiss → proposal=dismissed、目标行未变。
  4. **todo_create 确认**：propose_todo_create → confirm → todo 表多一行、proposal.applied_ref=新 todo。
  5. **topic_rename 确认**：改名生效。

## 任务 4 — 主服务装配（`cmd/zhiwei-server/main.go`）

- `MCPDeps` 补 `Proposals: agentProposals`（`agentProposals := &repo.AgentProposalRepo{DB: db}`，在 `mcpSrv` 装配处；注意 `mcpSrv` 目前在 `AgentEnabled` 判断之外构造——proposals 表一直在，写工具可常注册，但 Proposals repo 需在 mcpSrv 前建）。
- `if cfg.AgentEnabled` 块内（或与 RegisterAgent 并列）：`agent.RegisterProposals(r, agent.ProposalDeps{DB: db, Proposals: agentProposals, Memories: memories, Topics: topics, Todos: todos, MemoryTopics: memoryTopics, TodoTopics: todoTopics})`。
- `go build ./...` + `go vet` 全绿。

## 约束与验收
- 中文详细注释。写工具**绝不 mutate**（只 Create proposal）——测试须断言目标行未变。确认落库与 Resolve **同事务**、apply-once（Resolve false → rollback）。
- 只改：`internal/repo/{memory,todo,topic}.go`（加 Ext + 委托，保持现有行为/测试绿）、`internal/agent/{mcp_server,mcp_write_tools,proposals,handlers}.go`、`cmd/zhiwei-server/main.go`。**不改** `web/*`（确认卡前端单独 pass）。
- 集成测试用隔离库 `zhiwei_agentchat_test`（已迁移到 000006）+ t.Cleanup（agent_proposal/memory/topic/todo/todo_topic）。
- 迁移：**无**（agent_proposal 已在 000005）。

## 自检
对齐 §7.2（7 工具只建 proposal）/§8（apply-once 事务 + 幂等 + 注入防线）：覆盖、无占位、类型/方法名一致（`SaveExt`/`UpdateStatusExt`/`UpdateNameExt` 签名与任务 1 一致）。
