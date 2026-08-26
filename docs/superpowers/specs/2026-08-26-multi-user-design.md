# 多用户（多租户）设计

- 日期：2026-08-26
- 分支/worktree：`feat/agent-chatbot`
- 范围：从「单用户硬编码 `user_id=1`」演进到真多租户。**这是 spec 自身延后的大型横切工程**（§13.x / §17：多用户排最后），本文档是**分期设计 + 决策岔口**，非可立即执行的实现计划。
- 关联规格：person-profile spec §多用户；agent-chatbot spec §17。

## 现状（调研实证，file:line）
- **零鉴权**：`internal/api/router.go` 注释直言「MVP 单用户免登录，无认证中间件」；全仓 chi 中间件 0 次；无 cookie/jwt/login/`context.WithValue` 的「当前用户」概念。handler 用 `chi.URLParam` 取资源 id 直喂 repo；需 userID 的列表端点**硬编码字面量 1**（api 层 8 处 + `agent/handlers.go:54`）。
- **两个硬编码常量**：`toolUserID=1`（`agent/mcp_tools.go:14`，16 处引用）、`reviewUserID=1`（`review/generator.go:13`，8 处）。
- **schema 80% 就绪**：19/23 表带 `user_id BIGINT NOT NULL DEFAULT 1`（多带 `(user_id,…)` 复合索引）；4 表（`transcript_segment`/`memory_topic`/`todo_topic`/`speaker_name_candidate`）无 user_id、靠父键间接隔离。`owner`「我」已 per-user（`person.user_id + is_owner=1`）。
- **⚠️ 10 个读方法无 user_id 过滤（越权直取洞）**：`SessionRepo.Get/List`、`MemoryRepo.Get/List`(+`MemoryFilter` 无 UserID 字段)、`TodoRepo.Get/List/ListDismissed`、`TopicRepo.Get`、`AgentConversationRepo.Get`（其 List 有过滤但 Get 没有）、`AgentMessageRepo.ListByConversation`、`PersonRepo.Get`——handler 用 URL 里的雪花 id 直调，无归属校验。**这批是多租户的地基，不补则鉴权也挡不住按 id 越权读**。
- **30 个 repo 方法已带 `userID int64` 参数**（只是被传 1）——加 userID 只需改调用方传真值。

## 最硬约束：dsh 单进程 + MCP 工具无用户上下文
- dsh 是**单个长驻进程**（`runtime.go:107-149` `startMu` 保证只 spawn 一次），`DSH_SYSTEM_PROMPT` 是**进程级 env**（`runtime.go:164`）——persona 对所有用户共享，无法按 session 区分。
- `MCPHandler` 工厂 `func(*http.Request) *mcp.Server { return s }`（`mcp_server.go:66`）对任何请求返回**同一共享 server**；所有工具用常量 `toolUserID=1`；dsh 经固定 `ZW_AGENT_MCP_URL` 回连，**回调链无任何字段标识「这轮是哪个用户」**。→ 「边车这轮读谁的数据」无法靠现链路解决，是核心改造难点。
- `ProfileContext.Head`（`context.go:43`）已 per-turn 注入 owner 画像头，是承载「每用户画像侧写」的现成载体，但写死 `toolUserID`；且它只能补「关于用户的背景」，覆盖不了进程级行为指令。

---

## 推荐：分两阶段（数据层多租户 → agent 多租户）

### 阶段 1：HTTP/数据层多租户（不含 agent 边车）
让 REST + 数据隔离先多租户化，agent 暂仍单用户（或登录用户即 owner 的单 agent）。
1. **鉴权中间件（全新）**：`router.go` 加认证中间件，解析出 userID 注入 `ctx`；所有 `RegisterXxx` 路由套上。
2. **userID 贯穿**：中间件把 userID 放 `context.Value` + 提供 `userIDFrom(ctx)` 助手；handler 取出后**显式传参**给 repo/service（与现有 30 个 `userID int64` 参数风格统一，避免隐式 ctx 漏传）。替换 api 层 8 处 + `handlers.go:54` 字面量 1。
3. **补 10 个越权读查询（安全必做）**：加 `WHERE user_id=?`（本表有 user_id 列的）或 JOIN 父表校验（`transcript_segment` 等间接隔离的）。**即使不做多用户，这也是应补的隔离地基**。
4. **写路径补 user**：`audio.go:81` 上传、`Session/AgentConversation.Create` 的默认补 1 改为写真实 userID。
5. **常量退场（数据侧）**：`reviewUserID`（review 引擎 8 处）改运行期传入。

### 阶段 2：agent/dsh 多租户
6. **dsh 每用户进程池**（推荐方案，见岔口 3）：`runtime` 从单进程改为 `map[userID]*dshProc`；每进程用该用户的 `DSH_SYSTEM_PROMPT`（解 persona）+ 携带用户令牌的 `ZW_AGENT_MCP_URL`（解 MCP 上下文）。
7. **MCP 用户上下文**：每用户 dsh 回连带签名令牌的 MCP 端点（如 `/internal/mcp?tok=<signed>` 或 per-user path），`MCPHandler` 工厂从请求解析 userID → 构造绑定该 userID 的 server；`toolUserID`（16 处）全改为从 server 上下文取。
8. **ProfileContext / 上下文头**按真实 userID 注入（`context.go:47/79`）。

## 五个决策岔口（需你定夺）
1. **鉴权机制**：① cookie+服务端 session（自包含、标准、需 session 存储）② JWT（无状态、需签发/校验）③ 反代注入 header（`X-User-Id`，最省事，前提是部署在可信 auth 网关后）。**推荐**：若有 auth 网关→③ 最省；否则 ① cookie+session（个人 app 标准）。
2. **userID 传递**：`context.Value` 隐式 vs 显式参数贯穿。**推荐**：中间件放 ctx + handler 取出**显式传参**给 repo（与现有 30 方法风格一致，编译期可查漏）。
3. **dsh 多租户**：① 每用户一 dsh 进程（隔离彻底、解 persona+MCP 上下文，资源成本高）② 单进程+每 prompt 带 persona（复用进程，但行为级 prompt 无法区分、MCP 上下文仍难解）③ 单进程+sessionId↔userID 映射让 MCP 反查（省进程，但 MCP client 连接是进程级、反查需额外协议字段）。**推荐 ①**：唯一同时干净解决 persona + MCP 用户上下文的方案，代价是进程数（可加空闲回收/上限）。
4. **MCP 用户上下文注入**：随岔口 3——①下每用户端点带令牌最干净。
5. **无 user_id 表隔离**：保持「查询 JOIN 父表校验」（不加列、省迁移）vs 冗余加 user_id 列（查询简单、需迁移+回填）。**推荐**：保持 JOIN 父表（4 张表都从属父行，冗余列易不一致）。

## 规模与风险
- **规模**：大。阶段 1 ≈ 鉴权中间件 + ~40 处 userID 贯穿 + 10 个查询补隔离 + 测试；阶段 2 ≈ dsh 进程池重构 + MCP 令牌化 + persona per-user，触碰运行时最核心部分。
- **风险**：越权洞若漏补 = 数据泄漏（安全）；dsh 进程池 = 资源/生命周期复杂度 + 触碰已验证的单进程运行时（回归风险高）；鉴权从零引入影响所有端点。
- **建议**：阶段 1 可独立交付且价值明确（数据隔离 + 可登录）；阶段 2 依赖岔口 3 的重决策，建议 1 落地后单独立项。**即使不做多用户，阶段 1 第 3 步（补 10 个越权读）也值得单独做**（隔离地基/安全）。

## 需你定夺（先于实现）
- 是否现在做？做的话先阶段 1 还是整体？
- 五个岔口的选型（尤其 1 鉴权机制、3 dsh 方案）。
- 或：只先做「补 10 个越权读查询」这个安全地基（小、独立、无需选鉴权）？

## 阶段1 实现偏离登记（2026-08-26，安全评审 I4）
阶段1 实现中，**person/profile 端点（`/api/persons/*`、`/api/profile/*`）暂留 user-1**（硬编码 `1`），未随登录用户隔离——这偏离了本计划 Task4「person 也隔离」的字面要求。原因：person/profile 与 agent/owner/MCP 深度耦合（owner 概念、画像抽取、agent 画像工具全绑 user-1），完整隔离属阶段2 dsh/agent 多租户的一部分。
- **当前不可利用**：阶段1 只有 owner(id=1) 能登录（无注册端点、其余 app_user 无口令），user2 够不到这些端点。
- **⚠️ 阶段2 硬前置**：一旦放开第二个可登录用户（注册/建号），**必须在同一改动里**把 person/profile（读+写）切到 `auth.UserID`，否则新用户可读且可写 owner 的整份画像（越权写）。此项列为「解锁第二用户」的阻塞前置。
- 同理 agent/对话/MCP（`toolUserID=1`）、后台抽取/report cron 亦暂留 user-1，均随阶段2 一并多租户化。
