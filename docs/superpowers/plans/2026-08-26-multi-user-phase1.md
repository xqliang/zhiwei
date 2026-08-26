# 多用户 阶段1（数据层多租户 + cookie/session 鉴权）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: 用 superpowers:subagent-driven-development 逐任务执行；步骤用 `- [ ]` 跟踪。

**Goal:** 引入 cookie+session 鉴权，把 REST/数据层从单用户 user_id=1 改为按登录用户隔离；补齐 10 个无 user_id 过滤的越权读。

**Architecture:** 新 `internal/auth` 包（bcrypt + 服务端 session 表 + 中间件把 userID 注入 ctx）；handler 从 ctx 取 userID **显式传参**给 repo/service（与现有 30 个 `userID int64` 参数风格一致）；无 user_id 列的表 JOIN 父表校验。**agent/dsh/MCP/对话链暂留 user-1**（MCP 无用户上下文是硬阻塞，归阶段2）——阶段1 明确不动 `internal/agent` 的 `toolUserID`。

**Tech Stack:** Go(chi/sqlx/MySQL8)、golang.org/x/crypto/bcrypt、Vue3 CDN。

关联设计：`docs/superpowers/specs/2026-08-26-multi-user-design.md`（阶段划分 + 5 岔口，鉴权=cookie+session 已定）。

**⚠️ 阶段1 边界（明确不做）**：`internal/agent/*`（MCP 工具/对话/WS agent/orchestrator/context）、`internal/review` 的 report 生成里 agent 相关部分**暂留 user-1**。REST 数据面（timeline/memory/todo/topic/person/review 读写）多租户化。`/internal/mcp`（dsh 回连，loopback）**不加用户鉴权**。

**执行波次**：Wave0(迁移+apply) → WaveA(auth 包, 独立) + (repo 越权读补隔离, 独立) 并行 → WaveB(api/review 贯穿 userID, 依赖 auth+repo) → WaveC(router/WS/main 装配 + 前端登录)。

约束：各任务只改各自包、只 test 自己包、不跑 git；隔离库 `zhiwei_agentchat_test`（Wave0 后到 000012）+ t.Cleanup。中文注释。

---

## Task 1 — 迁移 000012_auth（协调者 Wave0）

**Files:** Create `migrations/000012_auth.up.sql`, `.down.sql`

- [ ] up.sql：
```sql
-- 用户表（多租户主体）。现有数据全 user_id=1 → 播种一个 id=1 的 owner 用户，存量数据归它。
CREATE TABLE app_user (
  id BIGINT PRIMARY KEY,
  username VARCHAR(64) NOT NULL,
  password_hash VARCHAR(100) NOT NULL DEFAULT '',  -- bcrypt；空=未设密码(需首登设置/引导)
  display_name VARCHAR(64) NOT NULL DEFAULT '',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_user_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 播种 owner（id=1，与存量 user_id=1 对齐）。password_hash 留空 → 首次由 ZW_OWNER_PASSWORD 引导设置。
INSERT INTO app_user (id, username, display_name) VALUES (1, 'owner', '我');

-- 服务端会话（cookie 存 token，服务端查此表定 userID）。
CREATE TABLE user_session (
  token CHAR(64) PRIMARY KEY,          -- 随机 32 字节 hex
  user_id BIGINT NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  expires_at DATETIME(3) NOT NULL,
  KEY idx_user_session_user (user_id),
  KEY idx_user_session_exp (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```
- [ ] down.sql：`DROP TABLE IF EXISTS user_session; DROP TABLE IF EXISTS app_user;`
- [ ] 协调者：apply 到 `zhiwei_agentchat_test`（→000012）+ fresh 库 000001–000012 验证。

---

## Task 2 — auth 包（WaveA，独立）

**Files:** Create `internal/auth/auth.go`, `internal/auth/session.go`, `internal/auth/middleware.go`, `internal/auth/auth_test.go`。依赖 Task1。

- [ ] `internal/repo/user.go`（放 repo 包，或 auth 包自持——为与既有 repo 风格一致，放 `internal/repo`）：`AppUser` 结构体 + `UserRepo{DB}`：`GetByUsername(ctx, username) (*AppUser, error)`、`Get(ctx, id) (*AppUser, error)`、`SetPasswordHash(ctx, id, hash) error`、`Create(ctx, *AppUser) error`。`UserSessionRepo{DB}`：`Create(ctx, token, userID, expiresAt) error`、`GetValid(ctx, token) (userID int64, ok bool, err error)`（`WHERE token=? AND expires_at>NOW()`）、`Delete(ctx, token) error`、`DeleteExpired(ctx) error`。
- [ ] `auth.go`：`HashPassword(pw string) (string, error)`（bcrypt DefaultCost）、`CheckPassword(hash, pw string) bool`。`newToken() string`（crypto/rand 32 字节 → hex；**不可用 math/rand**）。
- [ ] `middleware.go`：
  - `ctxKey` 私有类型 + `WithUserID(ctx, id)` / `UserID(ctx) (int64, bool)` 助手。
  - `Middleware(sessions *repo.UserSessionRepo) func(http.Handler) http.Handler`：读 cookie `zw_session` → `GetValid` → `WithUserID` 注入 → next；无/失效 → 401 JSON。
  - cookie 常量：名 `zw_session`、`HttpOnly`、`SameSite=Lax`、`Secure`（由 `ZW_COOKIE_SECURE` 配，默认 true；本地 http 调试可关）、`Path=/`、`MaxAge` 与 session 过期一致（如 30 天）。
- [ ] handlers（放 `internal/api/auth.go`，用 auth 包）：`POST /api/auth/login {username,password}`（校验 bcrypt → 建 session → Set-Cookie → 200 {user}）、`POST /api/auth/logout`（删 session + 过期 cookie）、`GET /api/auth/me`（返回当前 user 或 401）。**register**：MVP 可先只支持 owner 引导（见 Task6 main：若 user1 password_hash 空且配了 `ZW_OWNER_PASSWORD` 则启动设置），不开放注册端点；或加 `POST /api/auth/register`（本期可选，先不做）。
- [ ] 测试（隔离库）：HashPassword/CheckPassword 往返 + 错密码 false；session Create→GetValid 命中、过期不命中、Delete 后不命中；Middleware：无 cookie→401、有效 cookie→注入 userID、放行。newToken 唯一性/长度。
- [ ] 验证：`go build ./internal/auth/ ./internal/repo/`、test、gofmt。

## Task 3 — repo 越权读补 user_id 隔离（WaveA，独立，与 Task2 不同文件）

**Files:** Modify `internal/repo/session.go`、`memory.go`、`todo.go`、`topic.go`、`agent_conversation.go`、`agent_message.go`、`person.go`；扩相应 `_test.go`。

对 10 个无 user_id 过滤的读方法补隔离（**签名加 `userID int64` 或复用已有**）：
- [ ] `SessionRepo.Get(ctx, id)` → `Get(ctx, userID, id)` 加 `AND user_id=?`；`List(ctx, limit, offset)` → 加 userID 参数 + `WHERE user_id=?`。
- [ ] `MemoryFilter` 加 `UserID int64` 字段；`MemoryRepo.List`/`listWhere` 用它加 `m.user_id=?`；`Get(ctx,id)` → `Get(ctx, userID, id)` 加 `AND user_id=?`。
- [ ] `TodoRepo.Get`/`List`/`ListDismissed` 加 userID + `WHERE user_id=?`。
- [ ] `TopicRepo.Get` 加 userID + `AND user_id=?`。
- [ ] `AgentConversationRepo.Get` 加 userID + `AND user_id=?`（List 已有过滤）。
- [ ] `AgentMessageRepo.ListByConversation` → 加 userID + `AND user_id=?`（agent_message 有 user_id 列）。
- [ ] `PersonRepo.Get` 加 userID + `AND user_id=?`。
- [ ] 无 user_id 列的间接隔离（若被 Get 直取）：`transcript_segment`/关联表——本期这些走父键，父行已按 userID Get 校验即够（如先 `SessionRepo.Get(userID,sid)` 命中才 ListSegments）。
- [ ] 测试：造 user=1 与 user=2 各一行，断言 user=1 的 Get/List 不返回 user=2 的行（越权隔离）。
- [ ] 注意：**改这些签名会波及 api/agent/review 调用方**——本任务只改 repo + repo 测试；调用方在 Task4/5 改（届时 build 才全绿；本任务 `go build ./internal/repo/` + repo 测试通过即可，整模块 build 暂时红是预期，Task4/5 收口）。
- [ ] 验证：`go build ./internal/repo/`、`go test ./internal/repo/ -run '你的越权用例'`、gofmt。

## Task 4 — api handler/service 贯穿 userID（WaveB，依赖 T2/T3）

**Files:** Modify `internal/api/*.go`（person/topic/memory/review/audio/query/handlers 等）；`internal/memory/conversation.go`；扩测试。

- [ ] 所有需 userID 的 handler：`uid, ok := auth.UserID(r.Context()); if !ok { 401 }`，把 `uid` 传给 repo/service（替换字面量 1：person.go 5+ 处、topic.go 5 处、memory.go、review.go 2 处、audio.go 上传写 uid、query/timeline 的 SessionRepo.Get(uid,…)）。
- [ ] 详情类端点（Get by id）：传 uid → repo 的 `Get(ctx, uid, id)`，越权返回 404（不泄漏存在性）。
- [ ] `internal/memory/conversation.go` 的 `ListActive(ctx,1,…)` 等：本期若属对话抽取（agent 域）→ 暂留 1 并注释「阶段2」；若属通用数据 → 也可先留 1（该模块由抽取 pipeline 调，非用户请求上下文，无 ctx userID）。**判定原则**：有 HTTP 请求上下文的走 uid；后台 pipeline/抽取无请求上下文的暂留 1（阶段2 再随 job.user_id 贯穿）。
- [ ] 验证：`go build ./...`（此时应恢复全绿）、api 测试（造带 session 的请求）、gofmt。

## Task 5 — review 包 reviewUserID 运行期化（WaveB，依赖 T3）

**Files:** Modify `internal/review/generator.go`、`gather.go`、`api/review.go`；扩测试。
- [ ] `Generator` 的 `reviewUserID` 常量改为方法参数/字段：报告生成入口带 userID（api/review.go 从 ctx 取 uid 传入）。gather 的 8 处引用改用传入 userID。
- [ ] cron 定时报告（无请求上下文）：本期对「所有用户」或「owner」生成——MVP 先只给 user 1 生成（注释阶段2 遍历用户）。
- [ ] 验证：`go build ./internal/review/`、test、gofmt。

## Task 6 — router/WS/main 装配 + owner 引导（协调者 WaveC）

**Files:** Modify `internal/api/router.go`、`cmd/zhiwei-server/main.go`、`internal/agent/ws.go`（WS 取 cookie）。
- [ ] router：auth 中间件包住业务路由；豁免 `/api/health`、`/api/auth/login`、`/api/auth/me`(自身判 401)、静态 `/`、`/app/*`。`/internal/mcp*` **不挂**用户鉴权（loopback）。
- [ ] WS（`/api/agent/conversations/{cid}/ws`）：升级前读 cookie → session → uid；本期 agent 仍 user-1，但**校验登录态**（未登录拒绝 WS）。
- [ ] main：装配 `UserRepo`/`UserSessionRepo`/auth handlers/中间件；启动时 owner 引导：若 user1 `password_hash==''` 且 `ZW_OWNER_PASSWORD` 非空 → `SetPasswordHash(1, Hash(env))`（首次设密码，便于登录）；起 session 过期清理 goroutine（复用 backfill sweep 模式，或 GetValid 时惰性跳过）。
- [ ] config：`ZW_OWNER_PASSWORD`(引导用)、`ZW_COOKIE_SECURE`(默认 true)、`ZW_SESSION_TTL_DAYS`(默认 30)。
- [ ] 验证：`go build ./...` + vet 全绿；fresh 库 000001–000012 migrate。

## Task 7 — 前端登录（协调者 WaveC，最后）

**Files:** Modify `web/app.js`、`web/index.html`（+ hash-web）。
- [ ] 启动时 `GET /api/auth/me`：401 → 显示登录页（用户名/密码表单 → `POST /api/auth/login` → 成功重载）；200 → 正常进主界面。
- [ ] 401 拦截：api 助手遇 401 → 跳登录页。登出按钮 → `POST /api/auth/logout`。
- [ ] cookie 由浏览器自动带（fetch 需 `credentials:'same-origin'`，检查现有 api() 助手是否带上）。
- [ ] `node --check` + `bash scripts/hash-web.sh`。

---

## 验收 & 自检
- 未登录访问 `/api/*`（非 auth）→ 401；登录后 cookie 生效、可访问。
- user=1 与 user=2 数据隔离：A 的 memory/todo/topic/person/session 详情与列表都不返回 B 的行；按 id 越权 Get → 404。
- owner 用 `ZW_OWNER_PASSWORD` 登录成功；存量数据（user_id=1）归 owner 可见。
- bcrypt 存储、cookie HttpOnly/SameSite/Secure、session 过期生效、token crypto/rand。
- **阶段1 边界守住**：`internal/agent` 的 `toolUserID` 未动（agent/对话仍 user-1，注释标阶段2）；`/internal/mcp` 无用户鉴权。
- `go build ./...`+vet+各包 test + fresh 000001–000012 全过。
- **安全自检**：无 user_id 过滤的 10 个读全部补齐；越权 Get 不泄漏存在性（404 非 403）；cookie 无 XSS 可读（HttpOnly）；密码不日志。
