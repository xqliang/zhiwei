# 个人智能体 / Chatbot 系统 · 设计规格（总纲）

- 日期：2026-08-24
- 状态：设计定稿待评审
- 分支 / worktree：`feat/agent-chatbot`（从 `main` 分出，不依赖 person-profile）
- 关联：
  - MVP 设计 `docs/superpowers/specs/2026-08-18-zhiwei-cloud-mvp-design.md` §6「检索与 Agent 问答」「Daily Review」（本设计是它的实现 + 升级）
  - 记忆抽取 `2026-08-19-zhiwei-sprint2-design.md`（`internal/pipeline/stage_extract.go` 范式，对话转记忆复用）
  - 人物系统 `.worktrees/person-profile/docs/superpowers/specs/2026-08-24-person-profile-system-design.md`（画像读写，本设计 P2 依赖）
  - 声纹边车 `2026-08-22-speaker-voiceprint-design.md`（Python sidecar + HTTP client 先例，本设计 Node 边车沿用其运维模式）
- 交付方式：**本总纲一次画清全量架构与数据模型，分 3 期（P1→P3）实现**，每期独立 plan、可单独上线

---

## 1. 目标与范围

围绕「知微」建立一个**会用工具、能读写、可视化**的个人智能体（chatbot），它能：

1. **对话问答**：基于我的 timeline（录音/转写）、memory、topic、todo（P2 起含 profile 画像）聊天，回答**带可点击证据引用**。
2. **读 + 分析**：按需检索我的信息与记忆，做归纳、对比、趋势分析。
3. **改我的信息**：按我的要求调整信息——但走**「提议 → 卡片确认 → 落库」两段式**，**绝不静默写入**。P1 覆盖 memory/topic/todo，P2 扩展到 profile。
4. **对话转记忆**：我与它的交谈历史，作为 memory（P2 起含 profile）的又一个抽取来源。
5. **报告**：对当天所有录音**归纳整理生成日报**；每周出**周报**；以 **topic/项目粒度**跟进整体状态（进展 / todo / 风险）。报告用**图文 / 列表 / 表格 / 曲线**卡片呈现。
6. **卡片交互**：数据展示、确认修改、报表（表格 / 曲线）都以结构化卡片在应用内呈现。

**技术主线**：用 **deepseek-harness（dsh）作为 agent 内核**（真实运行，非仿写），以 **headless Node 边车**形式跑，**不用它自带的 :3080 Web UI**——由我们自己的 Go + Vue 前端驱动并渲染输出。LLM 用 **DeepSeek 模型（经火山方舟 Ark 提供，复用现有 ARK key）**。

### 1.1 非目标

- 不做多租户 / 权限：沿用单用户 `user_id=1` MVP。
- 不做 dsh 自带 Web UI 的集成或改造。
- 不在 P1 依赖 person-profile；不在 P1 做飞书推送（报告先在应用内卡片呈现，飞书交付留后续）。
- 不做向量检索（见 §10：embedding 账号 403，且 DeepSeek 无 embedding API）——P1 用关键词 / 时间 / 重要度检索，向量留后续。
- 不做医疗 / 财务等专业建议；分析仅基于我自己的数据。

---

## 2. 关键决策（brainstorm 结论）

| # | 决策 | 理由 / 权衡 |
|---|---|---|
| D1 | **真实运行 dsh，headless 边车**（`dsh-jsonrpc-agent <cordis.yml>`，newline-delimited JSON-RPC 2.0 / stdio） | dsh 有专门为「被别的进程驱动」而设的 SDK server；不用其 Web UI，事件流由我们渲染。**不接** :3080 的 HTTP/WS BFF（内部、不稳定）。 |
| D2 | **Go 既是 dsh 的父进程，又是它的工具提供方**：Go `spawn` dsh 子进程走 stdio 驱动；同时 Go 暴露 **MCP-over-HTTP** 工具端点，dsh 在 cordis.yml 里连回来消费 | JSON-RPC 走 stdio 必须由父进程持有管道，故 Go 必须 spawn dsh（区别于声纹边车的「独立进程 + HTTP」）。工具走 MCP-HTTP 而非 stdio，因为 Go 本就是 HTTP server，dsh 连出即可，无需再起第二个 Go 进程。 |
| D3 | **写入 = 提议 → 确认两段式**：写工具只建 `agent_proposal`（pending）并返回确认卡，**不碰目标行**；用户点[确认]才经 Go 端点落库 | 「绝不静默覆盖」，对齐现有 memory/topic/todo 的 `suggested→confirmed` 文化与 person-profile 强约束；人审闸门在 dsh **之外**（Go 侧），因此即便转写/对话里含注入式「帮我改成 X」也无法自动生效——闸门兼作**提示注入防线**。 |
| D4 | **检索 = 混合**：prompt 注入轻量上下文头（日期 / owner 概要 / 最近轮次）+ agent 自主调 MCP 读工具按需深挖 | 更「agent」、可迭代检索；上下文头负责 grounding，工具负责深度。 |
| D5 | **报告引擎在 Go 侧**（`internal/review`），LLM 用 Ark 上的 DeepSeek 模型（复用现有 `ArkLLM` client，仅换 model id） | 报告是批量生成，不必绕 dsh；可被 cron / API / agent 工具三处调用。agent 在对话里说「生成今天报告」→ 调 `generate_report` 工具 → 落到同一引擎。 |
| D6 | **会话真相源双写**：dsh 在进程内持有事件日志（其内部 JSONL）；我们仍以 `agent_message` / `agent_conversation` 为**展示 + 抽取**的真相源 | dsh 的 wire 无 seed/resume（见 §4.4）；我们自己存历史，重启后用上下文头重播近期对话。 |
| D7 | **锁版本 + 接口隔离**：pin dsh 到确切 rc 版本；Go 侧用 `AgentRuntime` 接口封装 dsh，wire 细节不外泄 | dsh 是预发布 `0.1.1-rc.2`，明确「会有破坏性变更」、无协议版本协商。隔离后可整体替换 / 升级。 |
| D8 | **一次做全设计，分期实现**：P1 不含 profile，P2 接 profile，P3 报告进阶 | 对齐 person-profile「总纲 + 分期」。v1 报告范围（用户定）：**日报 + 周报 + topic/项目状态** 都进 P1。 |

---

## 3. 架构与数据流

### 3.1 组件拓扑（两条正交接缝）

```
┌──────────────┐   HTTP + WS     ┌───────────────────────────┐
│   Vue3 前端   │ ◀────────────▶ │        Go 后端 (zhiwei)     │
│ 问知微 / 报告  │   /api/agent/*  │                            │
│  卡片渲染     │   /api/reviews/* │  internal/agent            │
└──────────────┘                 │   ├─ Runtime(spawn dsh)     │
                                  │   ├─ MCP-HTTP 工具服务       │────┐
                                  │   └─ 编排 / 引用校验 / 落库   │    │ ① spawn + JSON-RPC/stdio
                                  │  internal/review (报告引擎)  │    │   initialize / session/prompt
                                  │  internal/repo (现有仓储)     │    │   ◀ 事件流 session.event/status
                                  └───────────────────────────┘    ▼
                                        ▲                    ┌──────────────────────┐
                                        │ ② MCP-over-HTTP     │  dsh 边车 (Node,      │
                                        │   mcp__zhiwei__*    │  headless)           │
                                        └─────────────────────│  agent 循环 + DeepSeek │
                                                              └──────────────────────┘
```

- **接缝①（驱动）**：Go `internal/agent.Runtime` spawn `dsh-jsonrpc-agent`，持有其 stdin/stdout，说 JSON-RPC；stderr → 日志。
- **接缝②（工具）**：dsh 的 `mcp-client` 通过 **streamable-http** 连 Go 的 `/internal/mcp` 端点（仅绑 127.0.0.1），拿到 `mcp__zhiwei__*` 工具。
- 两条缝正交：换掉①（未来直接嵌入 Node 或换 harness）不影响②的工具契约；换掉②（工具增减）不影响①。

### 3.2 一次对话轮次（chat turn）时序

```
用户在「问知微」输入 → 前端经 WS 发 {type:user_message, text}（连 /api/agent/conversations/{cid}/ws）
  → Go：① 建 user agent_message；② 组装上下文头（日期/owner概要/最近N轮摘要/检索种子）
        ③ 向 dsh 发 session/prompt {sessionId=cid, contentBlocks:[上下文头, 用户文本]}
  →（事件经同一条 WS 下推前端，无需另开连接）
  → dsh agent 循环：
        - assistant/chunk  ── Go 转发 → 前端流式渲染助手气泡（markdown）
        - tool/call {name, args}  ── agent 调 mcp__zhiwei__search_memory 等
              → 命中 Go MCP 端点 → 查 repo → 返回结构化结果
        - tool/result {message}   ── Go 转发 → 前端按 name 映射到卡片组件渲染
        - （写类工具 tool/result 携带 proposal → 前端渲染「确认卡」）
        - assistant/message       ── Go 落库 assistant agent_message + citations + tool_calls
        - turn/end {reason} / session.status: idle  ── Go 关闭本轮，WS 发 done 帧
  → 用户点确认卡[确认] → 前端 POST /api/agent/proposals/{pid}/confirm
        → Go 经现有 repo 落库（memory/topic/todo 更新）→ 返回新状态 → 卡片转「已确认」
```

**轮次完成判定**：监听 `session.status: idle`（wire 无「每 prompt 结果」，`messageId` 只是入队回执）。

### 3.3 边车生命周期与降级

- Go 启动时（若配置启用且 `node`/边车产物就绪）spawn dsh；崩溃 → 指数退避重启；保留 `sessionId ↔ conversation` 映射。
- **重启后**：dsh 不从 wire 恢复历史（§4.4）。策略：对活跃 conversation 起**新 sessionId**，下一轮 prompt 的上下文头里**重播最近 N 轮摘要**。P1 可接受；后续可上「thin Node wrapper + seed」根治。
- **边车不可用**：`/api/agent/*` 返回 503 + 前端「智能体暂不可用」卡片；**报告（§11 走 Go 直调 Ark 上的 DeepSeek）与其余功能不受影响**——对齐声纹「sidecar 未起不丢转写」。
- **stdout 即协议**：dsh cordis.yml **禁止加载任何 stdout logger**；Go 读 stdout 为协议、stderr 为日志。setup 脚本与 CI 校验这一点。

---

## 4. dsh 集成细节

### 4.1 边车打包与启动

- 落位 `services/agent-sidecar/`（与现有 `services/` 并列）：`package.json`（pin `@deepseek-ai/dsh` 及 `dsh-sdk-jsonrpc-server`/`dsh-mcp-client` 到**确切 rc 版本**）、`cordis.yml`、`README`。
- setup 脚本 `scripts/setup-agent-sidecar.sh`（仿 `scripts/setup-voiceprint.sh`）：校验 Node `^22.19 || >=24`、`pnpm install`、`pnpm build`（如需）。
- Makefile：`agent-sidecar-build`；Go 负责 spawn，无需 `*-start`（区别于声纹的独立 start）。
- 启动命令（Go spawn）：`node <bin>/dsh-jsonrpc-agent services/agent-sidecar/cordis.yml`，环境注入 `ARK_API_KEY` / `ZW_ARK_BASE_URL` / `ZW_AGENT_MODEL` / `DSH_SESSION_ROOT` / `DSH_SYSTEM_PROMPT`。

### 4.2 cordis.yml 组成（要点）

```yaml
# 仅示意关键插件行，具体版本/字段实现期定
plugins:
  - id: agent-loop            # 默认 react 循环
    name: '@deepseek-ai/dsh-agent-loop'
  - id: llm-ark               # OpenAI 兼容；指向火山方舟 Ark 上的 DeepSeek 模型
    name: '@deepseek-ai/dsh-llm-pi-ai'       # 通用 openai-completions，适配非 DeepSeek 原生端点(Ark)
    config:                                  # 备选：llm-deepseek + baseURL=Ark；确切插件/字段 spike 确认
      provider: openai-completions
      apiKeyEnv: ARK_API_KEY
      baseURL: ${ZW_ARK_BASE_URL:-https://ark.cn-beijing.volces.com/api/v3}
      models: [{ id: ${ZW_AGENT_MODEL}, contextWindow: 65536 }]   # Ark 的 DeepSeek 模型/endpoint id
  - id: sdk-jsonrpc-server    # 驱动缝①：暴露 JSON-RPC/stdio
    name: '@deepseek-ai/dsh-sdk-jsonrpc-server'
  - id: mcp-zhiwei            # 工具缝②：连回 Go 的 MCP-HTTP
    name: '@deepseek-ai/dsh-mcp-client'
    config:
      transport: streamable-http
      url: http://127.0.0.1:8080/internal/mcp
  # persona/系统提示：DSH_SYSTEM_PROMPT env（进程级，见 §4.4）
```

### 4.3 Wire 协议映射（dsh 事件 → 我们的处理）

| dsh `session.event` 类型 | Go / 前端处理 |
|---|---|
| `assistant/chunk {chunk}` | WS 推送 → 前端流式追加助手气泡（`text-delta`/`reasoning-delta`） |
| `assistant/message {message, usage}` | 落库 assistant `agent_message`；抽取 citations（§8.2）与 tool_calls |
| `tool/call {callId, name, arguments}` | WS 推送 → 前端起一张「工具进行中」卡（按 name 选组件） |
| `tool/result {message, error?}` | WS 推送 → 前端把结构化结果填进对应卡；写类工具结果含 `proposal` → 确认卡 |
| `turn/end {reason}` | reason∈{completed,aborted,blocked,max-tokens,error,interrupted}；异常 reason → 错误卡 |
| `session.status: idle` | 本轮结束，WS 发 `done` 帧 |
| `subagent.started/finished` | P1 忽略（如启用 dsh 子代理，后续展示） |

### 4.4 已知 wire 限制与对策

| 限制 | 对策（P1） |
|---|---|
| 无 `cancel`：不能中止进行中的一轮 | 「停止」= 前端断开 WS + Go kill 并重启边车（粗粒度；后续可上 ACP 的 cancel 或 wrapper） |
| 无 `seed`/`resume`（wire 层）：不能塞历史 | 我方存历史（§6）；重启用上下文头重播（§3.3） |
| system prompt 进程级、非每请求 | 单用户 MVP 一个 persona（`DSH_SYSTEM_PROMPT`）即可；多用户 / 多人设留后续（presets 或 wrapper） |
| 检索上下文无每请求字段 | 注入进 `session/prompt` 的 `contentBlocks`（作为用户消息前的上下文块，逐字送达） |
| 预发布、无版本协商 | pin 版本 + `AgentRuntime` 接口隔离（D7）；升级走 spike 验证 |

---

## 5. Go 侧组件（`internal/agent` + `internal/review`）

### 5.1 `internal/agent`

| 单元 | 职责 | 依赖 |
|---|---|---|
| `runtime.go` | spawn/监管 dsh 子进程；JSON-RPC 编解码；`initialize`/`session/prompt`/`shutdown`；事件分发 | `os/exec`、config |
| `runtime_fake.go`（test） | 脚本化事件的假运行时，单测编排不需真 dsh | — |
| `mcp_server.go` | `/internal/mcp` MCP-over-HTTP 端点；注册 `mcp__zhiwei__*` 工具（§7） | repo、review |
| `orchestrator.go` | 一轮对话编排：上下文头组装、发 prompt、消费事件、落库、WS 广播 | runtime、repo |
| `context.go` | 上下文头构造（日期 / owner 概要 / 最近 N 轮摘要 / 检索种子） | repo |
| `citations.go` | 从 assistant 消息 / 工具结果收集引用；剔除指向不存在 memory/session 的引用（防幻觉） | repo |
| `handlers.go` | HTTP：`/api/agent/*`（含 WS 流、proposal 确认） | orchestrator |

- `AgentRuntime` 接口：`Initialize(ctx, cfg) / Prompt(ctx, sessionID, blocks) (<-chan Event, error) / Shutdown()`；`runtime.go` 与 `runtime_fake.go` 两实现（D7）。

### 5.2 `internal/review`（报告引擎，§11）

`Generator{LLM, Model, Prompt}`：`Daily(date)` / `Weekly(range)` / `TopicStatus(topicID)` → 结构化报告对象 + 持久化。被 cron、`/api/reviews/*`、MCP `generate_report` 三处复用。LLM 用 Ark 上的 DeepSeek 模型（复用现有 `ArkLLM` client，仅换 model id）。

### 5.3 装配（`cmd/zhiwei-server/main.go`）

现有：`NewArkLLM` → pipeline → `api.RegisterMemory/Todo/Topic`。新增：
```go
dsRuntime := agent.NewRuntime(cfg.Agent)          // spawn dsh（可开关）
agentLLM  := provider.NewArkLLM(cfg.ARKBaseURL, cfg.ARKAPIKey)   // 复用现有 Ark client(报告/抽取用)
reviewer  := review.NewGenerator(agentLLM, cfg.AgentModel, ...)  // AgentModel = Ark 上的 DeepSeek 模型 id
mcpSrv    := agent.NewMCPServer(repos, reviewer)  // 挂 /internal/mcp
orch      := agent.NewOrchestrator(dsRuntime, repos, mcpSrv)
api.RegisterAgent(r, &api.AgentHandler{Orch: orch, ...})
api.RegisterReviews(r, &api.ReviewHandler{Reviewer: reviewer, ...})
```

---

## 6. 数据模型（迁移 `000005_agent`）

> 迁移号暂定 000005；person-profile / speaker-name-inference 并行分支亦占用 000005，**合并时统一重编号**（项目已知的并行迁移协调点）。沿用雪花 ID、`utf8mb4`、`DATETIME(3)`、JSON 列。

### 6.1 复用现有表

- `agent_message`（已存在：`role`/`content`/`citations JSON`）——**扩展**：加 `conversation_id BIGINT`、`kind VARCHAR(16)`（text|tool_call|tool_result|card）、`tool_payload JSON NULL`（工具名 + 参数 + 结果）、`dsh_seq INT NULL`（对齐 dsh 事件序，便于排序/去重）。
- `daily_review`（已存在：`review_date`/`content JSON`/`status`）——日报直接复用。

### 6.2 新表

```sql
-- 会话分组：一个「问知微」对话 = 一行；映射到 dsh sessionId
CREATE TABLE agent_conversation (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  title         VARCHAR(256) NOT NULL DEFAULT '',   -- 首条消息自动摘要
  dsh_session_id VARCHAR(64) NOT NULL,              -- 传给 dsh 的 sessionId（重启可换）
  status        VARCHAR(16) NOT NULL DEFAULT 'active', -- active|archived
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  last_active_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_user_active (user_id, last_active_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 写入闸门：agent 提议的每一处修改，人审前只落这里（D3）
CREATE TABLE agent_proposal (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  conversation_id BIGINT NULL,
  message_id    BIGINT NULL,                        -- 触发提议的 assistant 消息
  kind          VARCHAR(32) NOT NULL,               -- memory_update|memory_dismiss|topic_rename|topic_confirm|topic_dismiss|todo_create|todo_status|profile_*(P2)
  target_kind   VARCHAR(16) NOT NULL,               -- memory|topic|todo|profile
  target_id     BIGINT NULL,                        -- 目标行（新建类为空）
  payload       JSON NOT NULL,                      -- {old, new, args}
  rationale     TEXT NULL,                          -- agent 给的理由（展示用）
  status        VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|confirmed|dismissed|applied|expired
  applied_ref   BIGINT NULL,                        -- 落库后指向实际变更（如 memory.version 或新行 id）
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  resolved_at   DATETIME(3) NULL,
  KEY idx_user_status (user_id, status),
  KEY idx_conv (conversation_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 周报（与 daily_review 平行）
CREATE TABLE weekly_review (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  week_start    DATE NOT NULL,                       -- 周一
  week_end      DATE NOT NULL,
  content       JSON NULL,                           -- §11.2 结构
  status        VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|ready|failed
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_user_week (user_id, week_start)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 话题/项目状态快照（进展/todo/风险）
CREATE TABLE topic_status (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  topic_id      BIGINT NOT NULL,
  content       JSON NULL,                           -- §11.3 结构：summary/progress/milestones/open_todos/risks
  generated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_topic_time (topic_id, generated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 6.3 对话转记忆的溯源（§10）

`memory.session_id` 现为 `NOT NULL`。对话来源的记忆需要落位：**加 `memory.conversation_id BIGINT NULL` + 放宽 `session_id` 可空**（或用 `source_kind='conversation'` 标记，`session_id` 存触发对话的 conversation 的哨兵）。实现期二选一，倾向前者（清晰、可溯源到具体对话）。change 记入现有 memory 版本机制。

---

## 7. MCP 工具目录（`mcp__zhiwei__*`）

工具的**输出 schema 即卡片契约**（§9）。P1 工具：

### 7.1 读工具（agent 自主调用）

| 工具 | 入参 | 出参（→ 卡片） |
|---|---|---|
| `search_memory` | `query, type?, topic_id?, time_range?, limit?` | `[{id,type,title,content,event_at,topic,session_id,segment_ids,importance,confidence}]` → 记忆列表卡 |
| `get_timeline` | `date? / session_id? / time_range?` | `[{session_id,created_at,duration,speakers,segment_preview}]` → 时间线卡 |
| `get_topics` | `status?` | `[{id,name,status,memory_count,todo_count}]` → 话题列表卡 |
| `get_todos` | `status?, topic_id?` | `[{id,title,status,due_at,topic}]` → 待办列表卡 |
| `get_topic_status` | `topic_id` | §11.3 结构 → 话题状态卡（现算 + 落 `topic_status`） |

### 7.2 写工具（只建 proposal，不落库；D3）

| 工具 | 语义 | proposal.kind |
|---|---|---|
| `propose_memory_edit` | 改一条记忆内容/类型/topic | `memory_update` |
| `propose_memory_dismiss` | 忽略一条记忆 | `memory_dismiss` |
| `propose_topic_rename` / `propose_topic_confirm` / `propose_topic_dismiss` | 话题改名/确认/忽略 | `topic_*` |
| `propose_todo_create` | 新建待办 | `todo_create` |
| `propose_todo_status` | 待办状态流转（confirmed/done/dismissed） | `todo_status` |

- 每个写工具：查现值 → 组 `{old,new}` → 建 `agent_proposal(pending)` → **返回 proposal 供渲染确认卡**。工具**不**改目标行。
- P2 增 `propose_profile_*`（属性/关系/事件…），落 person-profile 的 pending 闸门。

### 7.3 报告工具

| 工具 | 语义 |
|---|---|
| `generate_report` | `type=daily|weekly|topic_status, target?` → 调 `internal/review` → 返回报告对象（→ 报告卡） |

### 7.4 工具安全

- 全部工具**限 `user_id=1`**；写工具永不直接 mutate（闸门在 Go/UI）。
- MCP 端点仅绑 `127.0.0.1`，不进公网路由；与 `/api/*` 分离于 `/internal/mcp`。

---

## 8. 写入闸门：提议 → 确认（D3 展开）

1. agent 调 `propose_*` → Go 建 `agent_proposal(status=pending, payload={old,new})` → `tool/result` 带回 proposal → 前端渲染**确认卡**（含 old vs new 并排、agent 理由、来源引用、[确认]/[放弃]）。
2. 用户[确认] → `POST /api/agent/proposals/{id}/confirm`：
   - Go 在**单事务**内经现有 repo 落库（如 `memory` PATCH：version+1 / 冲突置 superseded；`todo` 状态流转；`topic` 改名）→ proposal 转 `applied`、记 `applied_ref`。
   - 幂等：重复 confirm 已 applied 的 → no-op。
3. 用户[放弃] → proposal 转 `dismissed`。
4. 过期：pending 超期（如 24h）→ `expired`（可配）。
5. agent 侧感知：MVP 不强求 agent 实时得知确认结果；下一轮上下文头可带「上次提议已确认/放弃」摘要，让对话连贯。

> 该闸门同时是**提示注入防线**：转写/对话中的「帮我把 X 改成 Y」最多生成一个待确认提议，永远要人点确认才生效。

---

## 9. 卡片协议与前端

### 9.1 渲染两通道

- **助手文本** → markdown 气泡（流式）。
- **工具活动** → 卡片：前端维护 `toolName → Vue 组件` 映射表；`tool/call` 起「进行中」骨架卡，`tool/result` 填数据。

### 9.2 P1 卡片类型

| 卡片 | 来源工具 | 交互 |
|---|---|---|
| 记忆列表卡 | `search_memory` | 点条目跳 timeline 原文 |
| 时间线卡 | `get_timeline` | 点录音展开转写 |
| 话题/待办列表卡 | `get_topics`/`get_todos` | 点跳对应 tab |
| **确认卡** | `propose_*` | old/new 并排 + [确认]/[放弃] → 调 confirm 端点 |
| **报告卡** | `generate_report` / `/api/reviews/*` | 表格 + 曲线（SVG）+ 按 topic 折叠；日/周切换 |
| 话题状态卡 | `get_topic_status` | 进展条 / open todo / 风险标记 |
| 引用块 | assistant citations | 点展开对应 transcript 段 |

### 9.3 前端落点（`web/`，Vue3 CDN 无构建）

- 新 tab **「问知微」**：消息流（用户气泡 / 助手 markdown / 内联工具卡）+ 输入框 + WS 流式；复用现有设计系统（暖白纸 + 靛蓝、卡片 / 徽标）。
- 新 tab **「报告」**：日报 / 周报切换，报告卡；topic 状态卡在 topic 详情页可触发。
- 图表：无构建环境用**内联 SVG**（柱：topic 分布；折线：趋势）或 vendor 一个极小图表库；走 `dataviz` skill 定色板与规范。
- WS 客户端 + 卡片组件加进 `web/app.js`；`index.html` 视需要 vendor 图表库与 markdown 渲染器（皆放 `web/vendor/`，无构建）。

---

## 10. 检索（混合，D4）

- **上下文头**（每轮注入 prompt）：当天日期、owner 一句话概要、最近 N 轮对话摘要、（可选）与本轮 query 相关的 top-k 记忆种子。
- **agent 工具检索**：`search_memory` 等在 Go 侧做 **关键词(LIKE/ngram) + 时间范围 + 重要度** 打分召回（MVP §6.1 去掉向量项）。
- **向量检索留后续**：`memory.embedding` 列已在；但 Ark embedding 账号 403、DeepSeek 无 embedding API → P1 不做向量；接入独立 embedder 后再补「向量 + 关键词」混合。
- 评分（无向量版）：`0.5*关键词匹配 + 0.3*时间接近度 + 0.2*importance`，top-k 进上下文（阈值/权重可配，实调）。

---

## 11. 报告子系统（P1 全含：日报 + 周报 + 话题状态）

### 11.1 日报（复用 `daily_review`）
- 触发：每日 22:00（Go 侧定时器 / 系统 cron，`ZW_REVIEW_DAILY_CRON` 可配）+ `POST /api/reviews/daily/generate` + agent `generate_report`。
- 输入：当天 memory（按 topic）+ todo 变化 + 当天 timeline 统计 + 当天对话概况。
- 输出 JSON：`headline / highlights[] / decisions[] / todos{new,done,open} / insights[] / tomorrow[] / topic_distribution[{topic,count}]`。
- 约束：`tomorrow` 只引用当天 `confirmed` 未完成 todo，不凭空生成（沿用 MVP §6.3）。

### 11.2 周报（新表 `weekly_review`）
- 触发：每周一 cron + 手动 + agent 工具。
- 输入：本周 7 份日报 + 本周 memory/todo + topic 活动 + 与上周对比。
- 输出 JSON：`headline / by_topic[{topic, progress, key_events, open_todos, risks}] / trends[{metric, series[]}] / risks[] / next_week[]`。
- 曲线：`trends.series` 为图表就绪数据（如每日记忆数、todo 完成数）。

### 11.3 话题 / 项目状态（新表 `topic_status`）
- 触发：`get_topic_status` 工具 + topic 详情页 + 可选纳入周报。
- 输入：该 topic 的 memory 时间线 + todo（open/done）+ 最近活动。
- 输出 JSON：`summary / progress(0..1 或阶段) / milestones[] / decisions[] / open_todos[] / risks[{desc,severity}] / blockers[]`。
- 「项目粒度」：现模型以 `topic` 承载项目/主题；不新增项目实体（YAGNI）。topic 已与 memory/todo 关联（`topic_id`），聚合即得项目视图。

---

## 12. 对话转记忆（复用抽取范式）

- 引擎：仿 `internal/pipeline/stage_extract.go` 的 `memory.Extractor`，换**对话专用 prompt**（`prompts/conversation_extraction_v1.md`）。
- 输入：一段对话的近若干轮 user+assistant 文本（`agent_message`）。
- 输出：memory 候选（P2 起含 profile facts）+ todo 候选，走**同一质量闸门 + dedup**；`source_kind='conversation'`、`conversation_id` 溯源（§6.3）。
- 触发：对话结束/空闲后按需 + 每晚随日报批跑；幂等 dedup（自然键含 conversation_id）避免重复。
- 与 D3 关系：**抽取产出的是候选记忆**（走现有 memory suggested 流程），不是对我信息的「修改」；「修改」仍只经 §8 闸门。

---

## 13. API 一览

```text
# Agent 对话
POST   /api/agent/conversations                      新建对话
GET    /api/agent/conversations                       对话列表
GET    /api/agent/conversations/{cid}                 对话历史（agent_message）
POST   /api/agent/conversations/{cid}/messages        发用户消息（也可走 WS 上行）
GET    /api/agent/conversations/{cid}/ws              WebSocket：上行发消息 + 下行流式事件
POST   /api/agent/proposals/{pid}/confirm|dismiss     确认/放弃一处修改提议

# 报告
GET    /api/reviews/daily?date=                       取/触发日报
POST   /api/reviews/daily/generate                    手动生成日报
GET    /api/reviews/weekly?week_start=                取/触发周报
POST   /api/reviews/weekly/generate                   手动生成周报
GET    /api/topics/{id}/status                         取/生成话题状态

# 内部（仅 127.0.0.1，dsh 消费）
ALL    /internal/mcp                                  MCP-over-HTTP 工具端点
```

---

## 14. 配置项（`internal/config`）

| env | 默认 | 说明 |
|---|---|---|
| `ARK_API_KEY` | （现有必填） | 火山方舟 key，复用；dsh 与报告/抽取共用 |
| `ZW_ARK_BASE_URL` | `https://ark.cn-beijing.volces.com/api/v3` | 现有配置，Ark OpenAI 兼容前缀 |
| `ZW_AGENT_MODEL` | （填 Ark 的 DeepSeek 模型 id） | agent + 报告/抽取用的 DeepSeek 模型（如 `deepseek-v3` / `ep-xxx`），spike 确认 |
| `ZW_AGENT_ENABLED` | `true` | 关掉则不 spawn dsh（报告等仍可用） |
| `ZW_AGENT_SIDECAR_CMD` | `node .../dsh-jsonrpc-agent` | 边车启动命令 |
| `ZW_AGENT_CORDIS_CONFIG` | `services/agent-sidecar/cordis.yml` | cordis 配置路径 |
| `ZW_AGENT_MCP_URL` | `http://127.0.0.1:8080/internal/mcp` | 供 cordis.yml 引用 |
| `DSH_SESSION_ROOT` | `./data/dsh-sessions` | dsh 内部会话日志目录 |
| `DSH_SYSTEM_PROMPT` | 知微 persona 文件内容 | 进程级人设（§4.4） |
| `ZW_REVIEW_DAILY_CRON` | `0 22 * * *` | 日报 cron |
| `ZW_AGENT_RETRIEVE_TOPK` | `10` | 上下文头种子条数 |

persona / prompt 走版本化文件：`prompts/agent_persona_v1.md`、`prompts/conversation_extraction_v1.md`、`prompts/review_daily_v1.md`、`prompts/review_weekly_v1.md`、`prompts/topic_status_v1.md`；版本号进 trace。

---

## 15. 测试策略

沿用 `make test`（mock provider，无 MySQL）+ `make test-integration`（真 MySQL）+ `make spike-*`（真 LLM，手动不进 CI）：

- **纯逻辑单测**：上下文头组装、citation 校验、检索打分、proposal 闸门（pending→applied/dismissed/幂等）、对话抽取 dedup。
- **`runtime_fake` 编排单测**：脚本化 dsh 事件（chunk/tool_call/tool_result/idle），验证落库 + WS 广播 + 卡片映射数据，不需真 dsh。
- **MCP 工具单测**：工具即 Go 函数覆 repo，验证读结果 schema、写工具只建 proposal 不 mutate。
- **repo 单测**（integration）：agent_conversation/agent_proposal/weekly_review/topic_status CRUD、confirm 事务。
- **review 单测**：mock LLM 返回结构化报告，验证 daily/weekly/topic_status schema 与持久化。
- **spike（手动）**：`make spike-agent`（起真 dsh 边车 + Ark DeepSeek，跑一轮带工具调用的对话）、`make spike-review`（Ark DeepSeek 生成日报）。
- **前端**：卡片组件按 `toolName→component` 映射的渲染快照（轻量，人工/最小自动化）。

---

## 16. 分期实施

| 期 | 交付物 | 迁移 | 验收 |
|---|---|---|---|
| **P1 核心（不依赖 profile）** | dsh 边车集成（runtime/spawn/JSON-RPC）+ MCP 工具服务（读 + 写-提议：memory/topic/todo + generate_report）+ 「问知微」聊天页（流式/引用/卡片）+ 提议→确认闸门 + 对话转记忆 + **日报 + 周报 + 话题状态** + 报告页 | 000005 | 起边车→对话→agent 调工具→卡片渲染；提议→确认→落库；对话产出候选记忆；日/周报 + 话题状态卡可生成可视化；边车挂掉报告仍可用 |
| **P2 画像接入** | `propose_profile_*` 工具 + 画像读工具 + 画像确认卡 + 画像感知对话；「改我的信息」扩展到 profile | 依赖 person-profile 迁移 | 需 person-profile P1 落地/合并；对话可查/提议改画像，走其 pending 闸门 |
| **P3 报告进阶 & 交付** | 曲线可视化增强（接 person_metric/activity）+ 飞书推送日/周报（lark skill）+ 向量检索（接独立 embedder）+ dsh 升级/cancel（ACP 或 wrapper） | 视需要 | 趋势曲线；飞书交付；混合检索；可中止对话 |

全量模型在本总纲一次画清，plan 按期出。**先做 P1。**

---

## 17. 已知限制与后续

- **dsh 预发布**（0.1.1-rc.2）：会有破坏性变更、无版本协商 → pin 版本 + 接口隔离 + 升级走 spike。
- **wire 无 cancel/resume/seed、persona 进程级**：P1 用「重启 + 重播上下文头」「单 persona」绕过；根治需 thin Node wrapper 或 ACP。
- **向量检索缺位**：embedding 账号 403 + DeepSeek 无 embedding → P1 关键词检索，向量留 P3。
- **多用户**：persona 进程级限制下，P1 单用户；多用户需每会话 persona（presets/wrapper）。
- **对话转记忆的 `session_id` 可空改动**：需小迁移（§6.3），注意与现有查询兼容。
- **迁移号并行冲突**：000005 与 person-profile/speaker-name-inference 撞号，合并统一重编号。

---

## 18. 需求映射（确保用户列举项全部落位）

| 用户原始表述 | 落位 |
|---|---|
| 根据 timeline/memory/topic 聊天 | agent + `get_timeline`/`search_memory`/`get_topics`（§7.1） |
| 根据 profile 聊天 / 改画像 | P2：画像读工具 + `propose_profile_*`（§16） |
| 帮我修改我的信息 | 提议→确认闸门（§8），P1 覆 memory/topic/todo |
| 帮我分析数据 | agent 读工具 + 推理 + 报告（§7/§11） |
| 交谈历史成为 memory 来源 | 对话转记忆（§12） |
| 用 deepseek harness 实现 | dsh headless 边车 + DeepSeek 模型（§3/§4） |
| 总结当天录音生成报告 | 日报（§11.1） |
| 图文/列表/曲线报表 | 报告卡 + 表格 + SVG 曲线（§9.2/§11） |
| topic/项目粒度状态·进展·todo·风险 | 话题状态（§11.3，topic 承载项目） |
| 每周周报 | 周报（§11.2） |
| 检索我的信息和 memory | 混合检索 + 读工具（§10/§7.1） |
| 根据要求调整我的信息 | 提议→确认（§8） |
| 卡片交互展示/确认修改 | 卡片协议（§9），确认卡（§8） |
| 表格/曲线等报表 | 报告卡（§9.2/§11） |
