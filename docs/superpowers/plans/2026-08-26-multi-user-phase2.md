# 多用户 阶段2（person 切登录用户 + dsh 多租户）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development；步骤用 `- [ ]` 跟踪。

**Goal:** 完成多租户：person/profile 切登录用户 + agent/dsh 按用户隔离（每用户 dsh 进程 + 每用户 MCP 端点 + owner 概念随用户）。

**Architecture:** 沿用阶段1 的 `auth.UserID(ctx)`。profile.Service 方法加显式 `userID` 形参（方案A）。dsh 从单进程改为**每用户进程池**（每进程 spawn 时 MCPURL=`/internal/mcp/{mcpToken}` 携带用户身份 → MCPHandler 解析 token→userID→绑定该用户的 MCP server），这是单进程无法解 MCP 用户上下文的唯一根治路径。

**Tech Stack:** Go(chi/sqlx/MySQL8)、dsh headless Node 边车、go-sdk streamable-http（每请求 getServer）。

关联：`docs/superpowers/specs/2026-08-26-multi-user-design.md`（阶段2 硬前置 + 5 岔口）。阶段1 已实现（鉴权 + memory/todo/topic/timeline 隔离）。

**已定决策（7 岔口）**：①Service 加 userID 形参 ②手动写设 row.UserID ③每用户进程池(带上限+闲置回收) ④专用 MCP token(非 cookie) ⑤CLI 建号 ⑥persona 本期全局共享(仅 MCPURL per-user) ⑦pipeline/review 后台**划出本期**(仍 user-1，注释标注)。

**依赖顺序**：2C → 2A → 2B。

约束：隔离库 `zhiwei_agentchat_test`（已 000012；2C 若加迁移则续号）+ t.Cleanup；`go build ./...`+vet+fresh 库全过；中文注释。**each 波跑前重置测试库**（避污染全局断言）。

---

## 阶段 2C：用户创建 + per-user owner bootstrap（最小，先做）

**Files:** `internal/auth/store.go`(+CreateUser)、`internal/repo/person.go`(EnsurePersonBootstrap 按 userID)、新建 `cmd/zhiwei-adduser/main.go`、`cmd/zhiwei-server/main.go`(启动 bootstrap 保持 owner)、tests。

- [ ] `auth.Store.CreateUser(ctx, username, passwordHash, displayName string) (ids.ID, error)`：`id=ids.New()`，INSERT app_user；用户名重复返回可辨错误。
- [ ] `repo.EnsureOwnerForUser(ctx, persons, userID int64) error`：幂等——`GetOwner(ctx,userID)` 无则建 `{UserID:userID, DisplayName:"我", IsOwner:true}`。把现 `EnsurePersonBootstrap` 里写死 `GetOwner(ctx,1)`/建 owner 的部分抽成按 userID；启动仍为 user 1 调用（speaker→person 回填保持 user-1 域）。
- [ ] `cmd/zhiwei-adduser/main.go`：读 flag `-u username -p password [-n displayName]` → `auth.HashPassword` → `Store.CreateUser` → `EnsureOwnerForUser(newID)` → 打印新 user id。复用 config.Load 的 DSN。
- [ ] 测试：CreateUser 建号 + 重名报错；EnsureOwnerForUser 幂等（跑两次仍一个 owner）；新用户有独立 owner「我」。
- [ ] 验证：`go build ./...`、auth/repo test、gofmt。

## 阶段 2A：person/profile 切登录用户（cookie 域，中等，面广）

**Files:** `internal/api/person.go`、`internal/profile/{service,service_manual,confirm,extract_session}.go`、tests。

### 2A-1 profile.Service 方法加 userID 形参（方案A）
- [ ] 给这些方法加 `userID int64` 首参（或紧跟 ctx）：`ManualCreatePerson`/`ManualUpdatePerson`/`ManualSetPersonStatus`/`ManualAddAttribute(+Ext)`/`ManualDeleteAttribute`/`ManualAddRelationship(+Ext)`/`ManualDeleteRelationship`/`ManualAddEvent(+Ext)`/`ManualDeleteMetric`/`ManualAddMetric(+Ext)`/`ManualDeleteEvent`/`ConfirmPending`/`DismissPending`。手动写行时 `row.UserID = userID`（修「默认挂 1」坑，决策②）。
- [ ] **owner 解析下推 userID**：`ownerID`/`personByOwnerRelation`/`resolveOrCreateByName`（`service.go:401-512`）加 userID 参数，把内部 `GetOwnerExt(ctx,tx,1)`/`FindByNameExt(...,1,...)` 改用传入 userID。`ApplyFacts` 已有 userID，下推即可。
- [ ] **IDOR 校验**（堵子表越权）：ConfirmPending/DismissPending/ManualDelete*/ManualUpdate* 等「按子表行 id 操作」的方法，取到 row 后 `Persons.Get(ctx, userID, row.PersonID)`，nil → 返回 `ErrNotFound`（handler 转 404）。attribute/rel/event/metric 的 Get 先拿 person_id 再校验。
- [ ] `ExtractSession`（`extract_session.go`）：`Sessions.Get(ctx, ss.UserID, sessionID)`（已有 ss.UserID）；`ApplyFacts` 传 `ss.UserID`。

### 2A-2 person.go handler 取 auth.UserID
- [ ] 加 `import "zhiwei/internal/auth"`。每个 handler：`uid, ok := auth.UserID(r.Context()); if !ok { 401 }`；把 `uid.Int64()` 传给 repo（List/ListWithPending/Get/ListPending 的字面量 1 全换）与 Service（新 userID 形参）。
- [ ] 详情/子资源 Get（PatchAttribute/DeleteAttribute/Delete*(rel/event/metric)）：越权 → 404（靠 2A-1 的 Service IDOR 校验）。
- [ ] `Create`：`ManualCreatePerson(uid, ...)`（新建 person 设 user_id=uid）。
- [ ] 测试（api）：user1/user2 各建 person + 子表行；断言 user1 Get/List/Pending 只见自己；user1 改/删/确认 user2 的 person 或子表行 → 404（IDOR 堵住）；user2 抽取的事实挂 user2 的 owner（非 user1）。
- [ ] 验证：`go build ./...`、profile + api test（fresh 库）、gofmt。

## 阶段 2B：dsh 多租户（每用户进程池 + 每用户 MCP 端点，最大/最高风险）

**Files:** `internal/agent/{runtime,mcp_server,mcp_tools,mcp_write_tools,mcp_profile_tools,proposals,context,handlers,ws,orchestrator}.go`、`cmd/zhiwei-server/main.go`、tests。

### 2B-1 MCP server 按 userID 绑定（去 toolUserID 常量）
- [ ] `NewMCPServer(d MCPDeps, userID int64)`：把 userID 注入 server 构造；工具闭包改用该 userID（替换全部 `toolUserID` 引用：mcp_tools ~5、mcp_write_tools ~5、mcp_profile_tools ~8、context ~2、proposals ~2）。`toolUserID` 常量删除或仅保留给 review（review 独立常量，本期不动）。
- [ ] `context.go`(ProfileContext.Head/Seeds)、`proposals.go`(listProposals/confirm 的 FindByNameExt) 改用注入的 userID。
- [ ] proposals confirm/dismiss 端点（cookie 域，非 MCP）：`AgentProposalRepo.Get` 无 user 过滤 → 加 IDOR 校验（取 proposal 后按 `auth.UserID` 校验归属，或 Get 加 userID）。

### 2B-2 每用户 MCP 端点 + token
- [ ] MCP token 表/映射：新增 `mcp_token(token CHAR(64) PK, user_id, created_at, expires_at?)`（迁移 000013），或复用内存 map（进程重启失效可接受，因 dsh 也随之重启）。**倾向内存 map**（`map[token]userID` + mint 时写、进程生命周期内有效），免迁移。
- [ ] `MCPHandler` 工厂：`NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { token := 从 URL path/query 取; uid, ok := lookup(token); if !ok {return nil}; return perUserServer(uid) }, nil)`。per-user server 缓存 `map[userID]*mcp.Server`（懒构造 `NewMCPServer(d, uid)`）。
- [ ] 路由：`/internal/mcp/{token}`（已有 `/internal/mcp/*`）。authGate 保留 loopback 校验（前缀 `/internal/mcp/` 已覆盖）+ 工厂内 token 校验（无效→nil→400）。

### 2B-3 每用户 dsh 进程池
- [ ] `runtime` 单例 → `RuntimePool`：`Get(userID) AgentRuntime`（懒 spawn 该用户的 dshRuntime，MCPURL=`/internal/mcp/{mintToken(userID)}`、SystemPrompt=共享 persona、SessionRoot 按 user 分子目录）。池上限 `ZW_AGENT_MAX_USERS`（默认如 8）+ LRU 闲置回收（关最久未用的 runtime）。`Close()` 逐个关。
- [ ] `Orchestrator` 持 `Pool`（或 `func(userID)AgentRuntime`）+ 按 `conv.UserID` 选 runtime；`ProfileContext` 也按 conv.UserID 取 owner（Head/Seeds 传 conv.UserID）。`RunTurn(ctx, conv, text)` 已带 conv.UserID。
- [ ] `handlers.go`/`ws.go`：`createConversation` 设 `c.UserID = auth.UserID(ctx)`；listConversations/getConversation/postMessage/ws 的 `toolUserID`/字面量 1 换 `auth.UserID(ctx)`（WS 在 Upgrade 前从 cookie 取 uid、闭包捕获，因每轮用 background ctx）。
- [ ] main：`rt`（单例）→ pool 装配；`orch.Ctx` 的 ProfileContext 改为按 conv.UserID 解析（或 Orchestrator 内部持 Persons/Attributes/Retrieve，按 conv.UserID 构造 head）。
- [ ] 测试：FakeRuntime pool（按 userID 返回不同 fake）；两个 user 各自会话隔离；MCP token→userID 解析单测；越权（user1 的 dsh 拿到的 MCP server 只读 user1 数据）。真 dsh 冒烟留手动（需 .env）。

### 验收 & 自检（阶段2 整体）
- 建两个用户（CLI），各自登录 → REST（person/画像/memory/todo/timeline）+ agent 对话彼此隔离；越权 id 操作 → 404。
- 每用户 owner「我」独立；抽取事实挂对用户；手动写 row.user_id 正确。
- dsh 每用户独立进程 + 独立 MCP 端点；MCP 工具读的是「该 dsh 所属用户」的数据（token→userID）；loopback 校验保留。
- 池上限 + 闲置回收生效；进程僵死不拖垮他人。
- `go build ./...`+vet+各包 test（fresh 库）+ 迁移全过。
- **划出本期**（注释标注 + 记设计）：pipeline 抽取（stage_asr/extract/speaker_name 的 `Sessions.Get(ctx,1)`）、`memory/conversation.go`、`review` 全域仍 user-1（后台无请求上下文；随 job.user_id 贯穿属更大范围）。persona 全局共享（未做 per-user）。
