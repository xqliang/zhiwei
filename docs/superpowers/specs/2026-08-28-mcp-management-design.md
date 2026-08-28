# MCP 服务管理（一期）— 设计规格

- 日期：2026-08-28
- 状态：待评审
- 分支（建议）：`feat/agent-mcp-manage`

## 1. Context（为什么做）

用户（开发者/运维）要在 zhiwei 网页设置页里管理「知微个人智能体」全局能连接的 MCP 服务：**手动添加 / 查看 / 启用 / 禁用 / 删除**，且启用/禁用要**真正影响 dsh 运行时**（agent 实际能不能用那个服务的工具），无需手动重启进程/服务器。

现状（调研确认）：

- dsh 边车通过**固定的** `services/agent-sidecar/cordis.agent.yml` 启动，MCP 只有**一个写死的**内置 `mcp-zhiwei`（streamable-http 连回主服务 `/internal/mcp`）。cordis 配置是插件列表，每个 MCP 服务是一个 `@deepseek-ai/dsh-mcp-client` 条目（`transport: streamable-http|stdio` + `serverName` + `url` 或 `command`/`args`）。
- 每登录用户一个 dsh 子进程（`RuntimePool`，`internal/agent/pool.go`），首次 prompt 惰性 spawn；per-user MCP 回连地址经 `ZW_AGENT_MCP_URL` 环境变量注入（配置里 `!!js process.env.ZW_AGENT_MCP_URL`）。
- 项目**没有** skill 概念；skills.sh/skillhub 是 **skill 注册表（SKILL.md）**，与 MCP 无关 → 归二期。

一期只做 **MCP 服务管理**，全局配置（类似现有 `agent_config` 单例思路，但是「一组」服务）。

## 2. Goals / Non-Goals

**Goals**
- 全局 MCP 服务表 + 增删改查启禁的 REST API + 设置页 UI（手动添加）。
- 启用/禁用/增删**运行时生效**：**主路径 = 进程内热插拔（无 respawn）**；仅当热插拔报错/异常时才降级为「自动 respawn」。
- 内置 `zhiwei` 服务受保护（不可删、不可禁）。
- 一个坏的外部服务**不得拖垮**所有用户的 agent。

**Non-Goals（二期或不做）**
- skill 管理、skills.sh/skillhub 搜索与安装。
- MCP 在线注册中心搜索（一期只手动添加）。
- 按用户隔离的 MCP 配置（一期全局）。
- 改造 dsh 上游发版（只用本仓既有的 patch-package 方式打补丁）。

## 3. 架构总览

```
设置页(Vue)  ──REST──▶  /api/agent/mcp (handlers.go)  ──▶  mcp_server 表(000022)
                                   │
                                   ├─▶ cordisgen: DB → 生成 cordis.generated.yml（供新 spawn 的进程读取）
                                   └─▶ 生效: 对每个在用 dsh 运行时下发 mcp/apply（热插拔）
                                              └─(兜底) RuntimePool.EvictIdle → 下一轮自动 respawn
```

两条生效路径**互补**：`cordisgen` 保证「将来新 spawn 的进程」拿到正确配置；`mcp/apply` 保证「当前活着的进程」立即生效。

## 4. 数据模型 — 迁移 `000022_mcp_server`

沿用 golang-migrate 成对 up/down、InnoDB/utf8mb4；迁移号 000022（main 已有 000020_conversation_title 与 000021_segment_corrected_reason，原 000021 重排避让）。

```sql
CREATE TABLE mcp_server (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  server_key  VARCHAR(64)  NOT NULL,           -- cordis serverName；命名空间 mcp__<key>__*；须匹配 ^[A-Za-z0-9_]+$
  display_name VARCHAR(128) NOT NULL,
  transport   VARCHAR(32)  NOT NULL,           -- 'streamable-http' | 'stdio'
  url         TEXT NULL,                        -- streamable-http 用
  command     VARCHAR(255) NULL,                -- stdio 用
  args        JSON NULL,                        -- stdio 参数数组
  env         JSON NULL,                        -- stdio 额外环境变量（对象）
  enabled     TINYINT(1) NOT NULL DEFAULT 1,
  builtin     TINYINT(1) NOT NULL DEFAULT 0,    -- 内置 zhiwei=1，不可删/禁
  created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_mcp_server_key (server_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- 种入内置行（幂等）：
INSERT INTO mcp_server (server_key, display_name, transport, url, enabled, builtin)
VALUES ('zhiwei', '知微内置工具', 'streamable-http', '', 1, 1);
```

内置行的 `url` 为空——它的实际地址是 per-user 的（`ZW_AGENT_MCP_URL`），不由本表提供；cordisgen/热插拔对内置服务特殊处理（见 §5、§6）。

Repo：新增 `internal/repo/mcp_server.go`，仿 `AgentConfigRepo` 模式（`List` / `Get` / `Create` / `Update` / `SetEnabled` / `Delete`；`Delete`/`SetEnabled(false)` 对 `builtin=1` 返回错误）。

## 5. cordis 配置生成（cordisgen）

新增 `internal/agent/cordisgen`（或 `services` 侧生成脚本，倾向 Go 侧）：

- **以现有 `cordis.agent.yml` 为基模板原样保留**（含 `sdk-jsonrpc-server`、`llm-deepseek`、`agent-spine`、内置 `mcp-zhiwei`、`sessions`、`token-meter`，以及所有 `!!js process.env.*` 环境替换——**一律不动**，per-user MCP URL 继续走 env）。
- 对每个 `enabled=1 且 builtin=0` 的外部服务，追加一个 `- id: mcp-<server_key>` 的 `dsh-mcp-client` 列表块（字面量 url / command / args / env）。
- 外部服务块统一 `failOnStartupError: false`（**关键安全项**：坏服务被跳过，agent 仍能起；内置 zhiwei 保持 `true`）。
- 写出 `cordis.generated.yml`（放运行时可写目录）；`RuntimePool.base.CordisConfig` 指向它。启动时若表为空/仅内置，生成结果等价于原始文件。

> 选此「字符串追加列表块」而非「JSON 配置/每用户单独文件」：保留 `!!js` 环境替换、外部服务全局同构、实现最简。

## 6. 运行时生效（主：热插拔；异常兜底：自动 respawn）

### 6a. 主路径 — 进程内热插拔（无 respawn）【先做】
可行性已验证：cordis 支持运行时 `ctx.plugin(McpClient, cfg)` fork/dispose；`dsh-mcp-client` 是干净的 fork/dispose 插件（dispose 关 transport、注销 `ctx.tools`）；加 JSON-RPC 方法有先例（本仓 `patches/@deepseek-ai__dsh-sdk-jsonrpc-server` 已加过 `session/cancel`）。

- **边车侧**：打补丁给 `dsh-sdk-jsonrpc-server` 增加方法 `mcp/apply`（dispatch switch 加 `case`，与现有 cancel 补丁同构），参数为「期望的外部 MCP 服务集」`[{serverName, transport, url|command,args,env}]`。处理器用根 `ctx` 维护 `Map<serverName, fork>`：新增的 `ctx.plugin(McpClient, cfg)`、移除的 `fork.dispose()`、变更的先 dispose 再 plugin。返回每个服务的 `{serverName, ok, error?}`。
- **Go 侧**：`dshRuntime` 增 `ApplyMCP(ctx, servers)` 走 `call(ctx,"mcp/apply",...)`；`RuntimePool` 增 `ApplyMCPAll(servers)` 遍历在用运行时下发。配置变更后：`cordisgen` 重写文件（给将来新进程）+ `ApplyMCPAll`（给当前进程）。
- **待 spike 的风险**：模型「本轮可见工具清单」是否在下一轮刷新（`ctx.tools` 看似 use-time 查询，但需实测）。若不刷新 → 对该运行时开新 dsh session（历史已持久化）或降级到 6b。

### 6b. 异常兜底 — 自动 respawn（仅热插拔报错时触发）
当 `ApplyMCP` 对某运行时**返回错误/超时/连接已断**时，对该运行时调用新增的 `RuntimePool.EvictIdle()`（复用现有两段式 evict：锁内摘表、锁外 Close；只 Close 无活跃轮次的运行时）。用户**下一条消息**时 dsh 用（已由 cordisgen 重写的）新配置自动 spawn。无需手动重启；仅可能打断极少数正在生成的轮次。

> 决策（用户拍板）：**直接先做 6a 热插拔为主路径**，6b 只作 6a 报错/异常时的自动降级。实现顺序：先 `mcp/apply` 补丁 + spike 验证工具刷新（§10）；同一批把 cordisgen（保证新 spawn 正确）与 6b 降级路径补齐。ApplyMCPAll 逐运行时下发，任一失败即对该运行时走 6b。

## 7. API — 挂 `/api/agent/mcp`（`internal/agent/handlers.go` 的 `RegisterAgent`，走现有 authGate）

- `GET  /api/agent/mcp` → 列表（含 builtin 标记、enabled）。
- `POST /api/agent/mcp` → 新增（校验 server_key 唯一且匹配 `^[A-Za-z0-9_]+$`、transport 合法、http 必须有 url、stdio 必须有 command）。
- `PUT  /api/agent/mcp/{id}` → 编辑（builtin 行仅允许改 display_name）。
- `PATCH /api/agent/mcp/{id}` `{enabled}` → 启/禁（builtin 禁用拒绝）。
- `DELETE /api/agent/mcp/{id}` → 删除（builtin 拒删）。
- `POST /api/agent/mcp/{id}/test` →（可选）试连：streamable-http 发一次 MCP `initialize`/`tools/list` 探活，返回工具数或错误。
- 每次写操作成功后触发 §6 生效（cordisgen + ApplyMCPAll/EvictIdle）。

## 8. 前端 — 设置页新增「MCP 服务」子区（`web/index.html` 导航/面板 + `web/app.js`，仿 persona 区 `app.js:2812+`）

- 服务列表卡片：名称 / 传输 / 地址或命令 / 启用开关（`PATCH`）/ 删除（builtin 隐藏删除、开关禁用）。
- 新增表单：选 transport → streamable-http 填 url；stdio 填 command + args。提交 `POST`，刷新列表。
- （可选）「测试连接」按钮调 `/test`。
- 复用现有 `api()` 辅助 + `loadXxx/saveXxx` 模式。
- 注意：本区与「整体 prompt 预览」无直接耦合，但**若未来 MCP 工具影响注入 prompt，须遵守预览同步约定**（见备忘 `zhiwei-prompt-preview-sync`）。

## 9. 错误处理与安全

- 外部服务 `failOnStartupError:false`，坏服务不拖垮 agent（§5）。
- server_key 严格校验（cordis 强制 `^[A-Za-z0-9_]+$` 且跨服务唯一，否则 dsh 报错）。
- 内置 zhiwei 不可删/禁（repo + API 双重拦截）。
- `/internal/mcp` 的 loopback 保护不变；外部 MCP 是 dsh **出站**连接，不经该端点。
- stdio 服务 `command` 会在服务器上执行任意进程——一期是「开发者/全局后台」自用，暂不做命令白名单，但 UI/文档提示风险（二期若开放给终端用户须加沙箱/白名单）。

## 10. 测试

- `cordisgen` 单测：给定行集 → 断言输出含对应 `dsh-mcp-client` 块、外部块 `failOnStartupError:false`、内置块不变、`!!js` env 保留。
- `internal/repo/mcp_server` 集成测试（repotest 隔离库）：增删改启禁、唯一约束、builtin 拒删/拒禁。
- API handler 测试：各端点 + 校验失败分支 + builtin 拦截 + 触发生效被调用（用 fake 生效器计数）。
- `RuntimePool.EvictIdle` / `ApplyMCPAll` 单测：FakeRuntime 记录 Close/ApplyMCP 调用。
- （6a）边车补丁：一个 spike 脚本，运行时 `mcp/apply` 加一个 stdio echo 服务，断言下一轮模型可见 `mcp__echo_srv__*`（验证工具刷新，决定 6a 是否达标）。
- 端到端：dev（8081）起，加一个外部 stdio echo MCP → 发消息确认 agent 能调用；禁用 → 确认不可用。

## 11. 二期预告（skill + skills.sh）

- skills.sh 有可用 JSON API：`GET /api/search?q=<q>` → `{skills:[{id,skillId,name,installs,source}],count}`；skill 为 GitHub 仓库支撑的 `SKILL.md`。
- 需解决：SKILL.md ↔ dsh skills 机制（当前 `skills.enabled:false`）的格式兼容、安装（拉取 GitHub 内容落库/落盘）、启用（打开 dsh skills 并注入）。独立 spec。

## 12. 待确认/开放问题
- 6a 工具刷新 spike 结果（决定热插拔是否达标，或长期用 6b）。
- `/test` 试连是否一期就要（倾向要，能挡掉大量坏配置）。
- stdio 命令执行的安全边界（一期自用，先提示不做白名单）。
