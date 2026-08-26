# 实施计划：Agent P2 — 画像接入（profile 读工具 + propose_profile_* 写闸门）

- 日期：2026-08-26
- 分支 / worktree：`feat/agent-chatbot`（已合入 main 的 person-profile 子系统）
- 范围：让「问知微」agent **读画像** + **提议改画像**，确认走**已有的 agent 提议闸门（agent_proposal + 聊天内联确认卡，用户选 B）**，确认时落 person-profile。第一刀覆盖**属性 + 大事记**；关系（需解析关联人）随后。上下文注入（owner 概要进对话头）本期不做（context.go 仍延后）。
- 关联规格：spec §16 P2「画像接入」；§7.1/7.2（工具目录）；§8（提议→确认闸门）。
- 现状（已测绘）：person-profile 有 owner「我」(`Persons.GetOwner`, is_owner=1)、四平面 repo（Person/Attribute/Relationship/Event，均 `status=active|pending|...` + `CreateExt/SetStatusExt` 等 Ext）、`profile.Service` 的 `ManualAdd*`（各自 `s.DB.BeginTxx`，active/conf=1.0/审计 changed_by=user，属性单值型自动 supersede）、catalog 校验（`profile.Def(key)`/`profile.All()`/`ValidEventTypes`/`ValidRelations`）。agent 侧 `agent_proposal` 闸门 + `applyInTx`（proposals.go）+ 聊天确认卡（web/app.js `asProposal`/`PROPOSAL_KINDS`）已在 P2d 建好。MCP server 当前**未注入**任何 person repo / profile.Service。

---

## 关键设计

### D1：确认原子性——给 Service 加 Ext 变体（复用 ManualAdd* 逻辑，落进 confirm 事务）
`ManualAddAttribute/ManualAddEvent` 各自 `BeginTxx`，无法直接并入 `confirmProposal` 的事务。为保持 apply-once（领域写 + `Proposals.Resolve` 同一事务，终审确认过的性质），**抽出 Ext 变体**（body 不变，只把 `tx := s.DB.BeginTxx()` 换成传入的 `*sqlx.Tx`），原方法委托：
```go
// service_manual.go
func (s *Service) ManualAddAttribute(ctx, personID, attrKey, value) (*repo.PersonAttribute, error) {
	tx, err := s.DB.BeginTxx(ctx, nil); if err != nil { return nil, err }
	defer func(){ _ = tx.Rollback() }()
	row, err := s.ManualAddAttributeExt(ctx, tx, personID, attrKey, value)
	if err != nil { return nil, err }
	return row, tx.Commit()
}
func (s *Service) ManualAddAttributeExt(ctx context.Context, tx *sqlx.Tx, personID ids.ID, attrKey, value string) (*repo.PersonAttribute, error) {
	// ← 原 ManualAddAttribute 的 body（去掉 BeginTxx/Commit/Rollback），用 tx 调 FindActiveByKeyExt/SetStatusExt/CreateExt/ChangeLogs.CreateExt
	// 保留：单值 supersede、同值幂等 no-op（同值时直接 return existing, nil——注意 Ext 版不能 Rollback，改为直接返回）
}
```
同法 `ManualAddEventExt`。**注意**：Ext 版内的「同值幂等」分支原来 `return existing, tx.Rollback()`——Ext 版不持有 tx 生命周期，改为 `return existing, nil`（调用方 confirm 事务仍会 Commit，无副作用）。确认时 `ChangedBy` 仍记 `"user"`（用户点了确认，语义等同手动）。

### D2：propose_* 只建 agent_proposal，绝不写 profile（§8 注入根防线，与 P2d 一致）
每个 `propose_profile_*` 工具：校验（catalog）→ 读 owner 现值组 `{old}` → `Proposals.Create(pending)` → 返回提议。target_kind=`profile`，**target_id = owner person id**（`Persons.GetOwner(1).ID`，confirm 时作 personID）。

---

## 任务 1 — Service Ext 变体（`internal/profile/service_manual.go`）
- 加 `ManualAddAttributeExt(ctx, tx *sqlx.Tx, personID ids.ID, attrKey, value string) (*repo.PersonAttribute, error)` 与 `ManualAddEventExt(ctx, tx, personID, eventType, title, description, occurredAt, endAt, location string, relatedPersonID *ids.ID) (*repo.PersonEvent, error)`；原 `ManualAddAttribute`/`ManualAddEvent` 委托（BeginTxx→Ext→Commit），行为与签名不变（现有 API handler 调用面零改）。
- import 需要 `github.com/jmoiron/sqlx`。
- **测试**（profile 包，隔离库 + t.Cleanup）：`ManualAddAttributeExt` 传一个 `db.BeginTxx` 的 tx，Commit 后与旧 `ManualAddAttribute` 效果一致（active 行 + change_log + 单值 supersede）；`ManualAddEventExt` 同理。原 `ManualAddAttribute`/`Event` 既有测试保持绿（回归）。

## 任务 2 — MCPDeps + ProposalDeps 注入 profile 依赖（`internal/agent/mcp_server.go` + `proposals.go`）
- `MCPDeps` 加：`Persons *repo.PersonRepo`、`PersonAttributes *repo.PersonAttributeRepo`、`PersonEvents *repo.PersonEventRepo`（读工具 + propose 读现值用）。
- `ProposalDeps` 加：`Profile *profile.Service`、`Persons *repo.PersonRepo`（applyInTx 用 GetOwner + ManualAdd*Ext）。
- `NewMCPServer` 在 `registerWriteTools` 后调 `registerProfileTools(s, d)`（读 + propose 都放新文件，见任务 3/4）。

## 任务 3 — 画像读工具（`internal/agent/mcp_profile_tools.go` 新文件）
`registerProfileTools` 里注册：
- `get_profile`（无参）：`Persons.GetOwner(ctx,1)` → owner；`PersonAttributes.ListByPerson(owner.ID)` + `PersonEvents.ListByPerson(owner.ID)`（过滤 status∈{active,pending}）→ JSON `{display_name, summary, attributes:[{key,value,epistemic_type,status}], events:[{event_type,title,occurred_at,status}]}`。owner 不存在（未 bootstrap）→ 返回空画像不报错。
- `get_person`（入参 `name`）：`Persons.FindByName(ctx,1,name)` → 同结构；找不到返回 `{found:false}`。
- 复用 `jsonResult`。In/Out struct + `jsonschema` tag。
- **测试**：seed owner + 属性/事件 → get_profile 返回含之；get_person("张三") 命中/不命中。

## 任务 4 — propose 写工具（`internal/agent/mcp_write_tools.go` 追加，或放 profile_tools 文件）
- `propose_profile_attr`（入参 `attr_key, value, rationale`）：校验 `attr_key` 是合法 catalog key（`profile.Def(attrKey)` + 用 `profile.All()`/一个 `profile.IsValidKey` 判断；非法→tool-error）；`Persons.GetOwner(1)`（无 owner→tool-error 提示先建）；读现值 `PersonAttributes.FindActiveByKey(owner.ID, attr_key)` 组 `{old:{value}}`；payload `{old, new:{attr_key, value}}`；`Proposals.Create(kind="profile_attr", target_kind="profile", target_id=&owner.ID, payload, rationale)`。**不写 profile。**
- `propose_profile_event`（入参 `event_type, title, occurred_at?, rationale`）：校验 `ValidEventTypes[event_type]` + title 非空；`GetOwner`；payload `{new:{event_type,title,occurred_at}}`；`Proposals.Create(kind="profile_event", target_kind="profile", target_id=&owner.ID, ...)`。
- 复用 P2d 的 `proposeAndReturn`/`proposeResult`。
- **测试**：propose_profile_attr 建 pending 提议、**owner 属性未变**；非法 attr_key/event_type 报 tool-error。

## 任务 5 — applyInTx 确认落库（`internal/agent/proposals.go`）
`applyInTx` 加两个 case（在 confirm 的 tx 内，走 D1 的 Ext）：
- `profile_attr`：`if p.TargetID==nil { err }`；`row, err := d.Profile.ManualAddAttributeExt(ctx, tx, *p.TargetID, newStr("attr_key"), newStr("value"))`；`return &row.ID, nil`。
- `profile_event`：解析 `new` 的 event_type/title/occurred_at → `d.Profile.ManualAddEventExt(ctx, tx, *p.TargetID, eventType, title, "", occurredAt, "", "", nil)`；`return &row.ID, nil`。
- payload 的 `new` 已是 `map[string]any`（现有 `proposalPayload`）；attr_key/value/event_type/title/occurred_at 用 `newStr` 取。
- **测试**（proposals_test.go 追加）：propose_profile_attr → confirm → owner 多一条 active 属性、proposal=applied、applied_ref 指向新属性；重复 confirm 幂等（属性同值不重复叠加）；propose_profile_event → confirm → 新 active event。

## 任务 6 — 主服务装配（`cmd/zhiwei-server/main.go`）
- `MCPDeps` 追加 `Persons: persons, PersonAttributes: personAttrs, PersonEvents: personEvents`（这些 repo main 已装配在 main.go）。
- `ProposalDeps` 追加 `Profile: profileSvc, Persons: persons`。
- `go build ./...` + `go vet` 全绿。

## 任务 7 — 前端确认卡支持 profile kind（`web/app.js` + 如需 `index.html`）
- `PROPOSAL_KINDS` 加 `profile_attr`、`profile_event`；`PROPOSAL_TITLES` 加（如「更新画像属性」「记录大事记」）；`PROPOSAL_FIELD_LABELS` 加 attr_key/value/event_type/title/occurred_at 的中文标签。`proposalView` 的 old→new diff 复用现有逻辑（payload.old/new）。
- 跑 `bash scripts/hash-web.sh`（末尾）+ `node --check web/app.js`。**不重定义** escape/markdown；提议值走 `{{}}` 自动转义。

## 约束与验收
- 中文详细注释。propose_* **绝不写 profile**（测试断言 owner 未变）。confirm 单事务 apply-once（Ext 变体落进 tx + Resolve）。全部限 `user_id=1`、owner。
- 只改：`internal/profile/service_manual.go`(+test)、`internal/agent/{mcp_server,mcp_profile_tools(新),mcp_write_tools,proposals,proposals_test}.go`、`cmd/zhiwei-server/main.go`、`web/app.js`(+`index.html`)。**不改**迁移（person 表已在 main 的 000006_person）。
- 隔离库 `zhiwei_agentchat_test`（已迁到 000010）+ t.Cleanup（person/attribute/event/agent_proposal）。单测 mock 免 DB 的纯逻辑；DB 相关走集成。
- 关系工具（propose_profile_relationship）+ 上下文注入（owner 概要进对话头）本期不做，代码留 TODO 注记。

## 自检
对齐 §16 P2 / §7 / §8：读工具 + propose_* 只提议 + 复用 agent 闸门（用户选 B）+ apply-once 原子。类型/方法名一致（ManualAdd*Ext 签名、kind 串 profile_attr/profile_event 三处一致：propose 工具 / applyInTx / 前端 PROPOSAL_KINDS）。
