# 实施计划：Agent P2 画像接入·补全（关系工具 + 对话上下文注入）

- 日期：2026-08-26
- 分支/worktree：`feat/agent-chatbot`（已含 person-profile + P2 画像接入 adfa5cf）
- 范围：把「画像接入 agent」做全——补 **propose_profile_relationship** 写工具（走 agent 闸门聊天确认，与 attr/event 一致）+ **对话上下文注入**（owner 概要进对话头，让 agent 天然「认识我」）。
- 关联规格：spec §16 P2；§7.2；§10（上下文头）。
- 现状：P2 已实现 get_profile/get_person + propose_profile_attr/event（`mcp_profile_tools.go`）+ applyInTx profile_attr/event（`proposals.go`）+ ManualAddAttributeExt/ManualAddEventExt（`service_manual.go`）。关系用 `ManualAddRelationship(ctx, personID, relationType, relatedPersonID *ids.ID, direction, orgName, label string)`（自 BeginTxx，校验 `ValidRelations`）；`ManualCreatePerson(ctx, name, speakerID, summary)`（自 BeginTxx）；`Persons.FindByNameExt(ctx, ext, userID, name)` 已有（返回 active/pending，owner 优先）。Orchestrator.runTurn 现直接把 userText 发 Prompt。

---

## 关键设计

### D1：关系确认时「解析或新建」关联人（都在 confirm 事务内）
`propose_profile_relationship` 入参用 **related_person_name**（自然，agent 说「我朋友李四」）。确认时：payload 有 related_person_name → `FindByNameExt(tx,1,name)`；命中用其 id，未命中 `ManualCreatePersonExt(tx,name)` 新建人物再用其 id；关系写 `ManualAddRelationshipExt(tx, owner.ID, relationType, &relID, direction, orgName, label)`。org 关系（只给 org_name、无 related_person_name）→ relatedPersonID=nil。全部在 confirm 单事务 → apply-once。需新增 tx 变体 `ManualAddRelationshipExt`、`ManualCreatePersonExt`（原方法委托 BeginTxx→Ext→Commit，签名/行为不变）。

### D2：上下文注入只改「发给 dsh 的文本」，不改落库
Orchestrator 每轮：落库 user 消息 = **原始 userText**；user 流式帧回显 = **原始 userText**；但发给 `Runtime.Prompt` 的文本 = `contextHead + "\n\n" + userText`（head 非空时）。head 由新 `ProfileContext.Head(ctx)` 产出（owner 概要 + 关键属性 + 当天日期）；无 owner/无数据 → 空串则不加前缀。这样 agent 认识我、历史仍干净。（§10 上下文头的轻量版：暂只 owner + date，检索种子/最近摘要留后续。）

---

## 任务 1 — profile.Service tx 变体（`internal/profile/service_manual.go`）
仿 P2 的 Ext 抽法：
- `ManualAddRelationshipExt(ctx, tx *sqlx.Tx, personID ids.ID, relationType string, relatedPersonID *ids.ID, direction, orgName, label string) (*repo.PersonRelationship, error)`：把现 `ManualAddRelationship` 的 body（ValidRelations 校验 + 组 row + Relationships.CreateExt + ChangeLogs.CreateExt）搬入，用传入 tx；原方法委托。
- `ManualCreatePersonExt(ctx, tx *sqlx.Tx, name string, speakerID *ids.ID, summary *string) (*repo.Person, error)`：同理抽 `ManualCreatePerson`；原方法委托。
- 测试（profile 包，隔离库 + t.Cleanup）：两个 Ext 传 `db.BeginTxx` 的 tx，Commit 后与原方法效果一致（active 行 + change_log；非法 relation_type 报错）。原方法既有测试保持绿。

## 任务 2 — propose_profile_relationship 工具（`internal/agent/mcp_profile_tools.go`）
`registerProfileTools` 加：
```
propose_profile_relationship(relation_type, related_person_name?, org_name?, direction?, label?, rationale)
```
- 校验：`profile.ValidRelations[relation_type]`（非法→tool-error）；`related_person_name` 与 `org_name` 至少给一个（都空→tool-error）；direction 若给需 ∈ upstream|downstream|peer。
- `Persons.GetOwner(ctx,1)`（无 owner→tool-error）。为**确认卡展示**，若给 related_person_name 则 `Persons.FindByName` 看是否已存在（存在→提示"关联到已有人物X"，不存在→提示"将新建人物X"），仅用于 rationale/展示，不写库。
- payload `{new:{relation_type, related_person_name?, org_name?, direction?, label?}}`；`Proposals.Create(kind="profile_relationship", target_kind="profile", target_id=&owner.ID, payload, rationale)`。**不写 person/relationship。**
- 测试：propose 建 pending 提议 + owner 关系/人物表未变；非法 relation_type、两名皆空 → tool-error。

## 任务 3 — applyInTx profile_relationship（`internal/agent/proposals.go`）
加 case（confirm tx 内，D1）：
```
case "profile_relationship":
  if p.TargetID == nil { err }
  rt := newStr("relation_type"); if !profile.ValidRelations[rt] { err }   // 双保险(与 propose 端一致, 防未来别的提议源)
  var relID *ids.ID
  if name := newStr("related_person_name"); name != "" {
    ex, err := d.Persons.FindByNameExt(ctx, tx, toolUserID, name); if err {..}
    if ex != nil { id := ex.ID; relID = &id } else {
      np, err := d.Profile.ManualCreatePersonExt(ctx, tx, name, nil, nil); if err {..}; relID = &np.ID
    }
  }
  row, err := d.Profile.ManualAddRelationshipExt(ctx, tx, *p.TargetID, rt, relID, newStr("direction"), newStr("org_name"), newStr("label"))
  if err { return nil, err }
  return &row.ID, nil
```
（`d.Persons` 已在 ProfileDeps；`ProposalDeps.Persons` 之前预留，现启用。）
- 测试（proposals_test.go 追加）：① related_person_name 命中已有人物 → confirm 后 owner 多一条 active 关系指向该人物、proposal=applied；② related_person_name 未命中 → confirm 后新建该人物 + 关系；③ 仅 org_name → 组织关系（relatedPersonID=nil）；④ 重复 confirm apply-once（同一 proposal 第二次幂等，不重复建关系/人物——注：ManualAddRelationship 无同值去重，靠 Resolve CAS 保证 apply-once，第二次 confirm 因 status!=pending 直接 200 不再落库）。

## 任务 4 — 对话上下文注入（新 `internal/agent/context.go` + 改 `orchestrator.go`）
- 新 `context.go`：
```
type ProfileContext struct { Persons *repo.PersonRepo; Attributes *repo.PersonAttributeRepo }
// Head 组装对话上下文头(owner 概要 + 关键属性 + 当天日期); 无 owner/无数据返回 ""。
func (pc *ProfileContext) Head(ctx context.Context, now time.Time) string
```
实现：`GetOwner(1)`；无 owner→""；否则取 owner.Summary + `Attributes.ListByPerson(owner.ID)` 里挑几个关键 active 属性（如 occupation/city/company，或前 N 条 active）拼成一句；前缀「今天是 YYYY-MM-DD。关于用户本人：…（背景信息，自然运用，不必复述）」。
- `orchestrator.go`：`Orchestrator` 加可选字段 `Ctx *ProfileContext`（nil 则不注入）。`runTurn` 里：`sent := userText; if o.Ctx != nil { if h := o.Ctx.Head(ctx, timeNow()); h != "" { sent = h + "\n\n" + userText } }`；**落库 um 与 user 帧仍用原始 userText**；`o.Runtime.Prompt(ctx, conv.DSHSessionID, sent)`。
  - `timeNow()`：orchestrator 用 `time.Now()`（服务端可用；非 harness 限制）。为可测，Head 收 `now time.Time` 参数，runTurn 传 `time.Now()`。
- 测试（FakeRuntime）：构造 Orchestrator{Ctx: ...}，seed owner+属性，RunTurn 后断言 `FakeRuntime.LastText` 含上下文头 + 原始问题；而落库的 user 消息 = 原始 userText（不含头）。无 owner 时 LastText == 原始 userText。

## 任务 5 — 主服务装配（`cmd/zhiwei-server/main.go`）
- `ProposalDeps.Persons` 已装配（P2 已加）；无需再动 applyInTx 依赖。
- Orchestrator 构造处加 `Ctx: &agent.ProfileContext{Persons: persons, Attributes: personAttrs}`（在 `if cfg.AgentEnabled` 块，NewOrchestrator 后设字段，或改 NewOrchestrator 签名——倾向加设字段，避免动既有签名）。
- `go build ./...` + vet 全绿。

## 任务 6 — 前端确认卡加 profile_relationship（`web/app.js`）
- `PROPOSAL_KINDS` 加 `profile_relationship`；`PROPOSAL_TITLES`（如「新增人物关系」）；`PROPOSAL_FIELD_LABELS` 加 relation_type/related_person_name/org_name/direction/label 中文标签。复用 `proposalView` diff + `{{}}`。
- 末尾 `bash scripts/hash-web.sh` + `node --check web/app.js`。

## 约束与验收
- 中文详细注释。propose 绝不写库（测试断言 owner 关系/人物未变）。confirm 单事务 apply-once（关系写 + 可能的建人 + Resolve 同 tx）。kind 串 `profile_relationship` 三处一致（propose/applyInTx/前端）。
- 只改：`internal/profile/service_manual.go`(+test)、`internal/agent/{mcp_profile_tools,proposals,proposals_test,orchestrator,context(新),orchestrator_test}.go`、`cmd/zhiwei-server/main.go`、`web/app.js`。不改迁移。
- 隔离库 `zhiwei_agentchat_test`(已到 000010) + t.Cleanup（person/person_relationship/agent_proposal）。上下文头单测走 FakeRuntime 免真 dsh。

## 自检
对齐 §16 P2（关系提议）/§7.2/§10（上下文头）/§8（只提议+apply-once）。类型/方法名一致：`ManualAddRelationshipExt`/`ManualCreatePersonExt` 签名、`ProfileContext.Head(ctx, now)`、kind 串三处一致、Orchestrator.Ctx 可选不破坏既有 FakeRuntime 测试（不设 Ctx 时行为不变）。
