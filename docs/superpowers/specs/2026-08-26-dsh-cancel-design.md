# dsh cancel（对话轮次中止）解阻设计

- 日期：2026-08-26
- 分支/worktree：`feat/agent-chatbot`
- 范围：让「问知微」聊天支持**中止进行中的一轮**（用户点「停止」）。当前 dsh JSON-RPC wire 无 cancel，轮次只能靠 `session.status:idle` 自然收尾或 5min 超时兜底。
- 关联规格：agent-chatbot spec §4.4 / §16 P3 / §17（「dsh 升级/cancel（ACP 或 wrapper）」）。

## 决定性事实（真 key 探针 + 依赖 d.ts 实证，2026-08-26）
- dsh 底层 **`Agent.cancel(cause, opts)` 已原生存在**（`dsh-agent/lib/types/runtime-types.d.ts:80`，文档「abort the active turn or between-turn task」）；取消产生 `turn/end reason={kind:'aborted', reason:{kind:'user'}}` + `agent/status→idle`（`dsh-session/.../types.d.ts:139-143`）。
- JSON-RPC server（`dsh-sdk-jsonrpc-server/lib/index.js`）**持有 agent 句柄**（`this.sessions` map + `ctx.agents` 注册表；`prompt()` 走 `rec.handle.agent.followup(msg)`）——`agent.cancel({kind:'user'})` 进程内可达。
- **唯独 wire 缺这一手**：`handleRequest` 的 `switch` 只认 `initialize|session/prompt|shutdown`（index.js:155-162），无 capabilities 协商。
- 结论：解阻 = 把已存在的能力接到 wire，**不是**造中止能力，更不是 kill/restart。

## 现状约束（Go 侧，file:line）
- `AgentRuntime` 接口仅 `Prompt`/`Close`（`internal/agent/runtime.go:18-24`），无 Cancel。`call` 的 ctx 取消只放弃 RPC 等待、不中止 dsh 轮次（runtime.go:282-284）；drain 契约注释明写「暂不引入中止」（runtime.go:296-297）。
- 轮次结束 = `session.status:idle` → `close(turns[sid])`（runtime.go:249-255）。`turn/end` 只判错（event.go:88-107）；`aborted` 的 reason.kind 非 `error` → 取消后轮次能干净收尾、不误报错。
- **ws.go 结构性阻塞点**：读循环 `raw.ReadJSON(&in)` 串行（ws.go:74-99），一轮 `RunTurnStream` 跑完才回到下次 Read → **轮次进行中读不到「停止」帧**。任何「客户端发停止」方案都必须把 WS reader 拆成并发 goroutine。这是本设计唯一有难度处。
- 单一共享 runtime 实例（main.go NewDSHRuntime→defer Close→NewOrchestrator）。`Close()` 是进程级、杀所有 session、且 `closed=true` 后不可再 spawn。

---

## 推荐方案：路径 1 — thin wrapper 暴露 `session/cancel`

### D1. 边车侧：新增 `session/cancel` method（不 patch node_modules）
wire 加第 4 个 client→server method `session/cancel {sessionId}` → 服务端 `ctx.agents.get(SessionId(sessionId)).cancel({kind:'user'})`。

**落地机制**（避免改 node_modules 易被 reinstall 冲掉，三选一，倾向 A）：
- **A（推荐）：自有 bin + 组合插件**。在 `services/agent-sidecar/` 写我们自己的入口（仿 demo `bin.js` 用 cordis 组合 `sdk-jsonrpc-server`+`llm-deepseek`+`agent-spine-demo`+`mcp-client`+`sessions`），再挂一个**自有小插件**：`inject:['agents']`，在 sdk-jsonrpc-server 的 request 分发之外注册 `session/cancel`（或 fork 该 server 插件为我们仓库内的一份薄封装，只在其 `handleRequest` switch 加一个 case）。`runtime.go` 的 `binPath()` 改指向自有 bin。→ 改动进我们的源码树、可控、不怕 reinstall。
- **B：patch-package**。保留 demo bin，用 `patch-package` 对 `dsh-sdk-jsonrpc-server/lib/index.js` 打补丁（`handleRequest` 加 `case "session/cancel"`），`postinstall` 自动重放。~30 行 diff，但绑死具体版本行号。
- **C：直接 fork 单文件进仓库**。把 `index.js` 复制进 `services/agent-sidecar/` 改一处，bin 指向它。最省事但要跟上游 rc 版本手动同步。

（三者都依赖 rc 版内部 API `ctx.agents`/`SessionId`；rc 破坏性变更风险已知，见风险节。）

### D2. Go runtime：`AgentRuntime.Cancel`
- 接口加 `Cancel(ctx context.Context, sessionID string) error`（runtime.go:18）。
- `dshRuntime.Cancel` = `r.call(ctx, "session/cancel", {sessionId})`，仿 `shutdown`（~15 行）。**无需新 channel 管道**——dsh abort→`session.status:idle`→现有 `close(turns[sid])` 自然关闭本轮 channel。
- `FakeRuntime` 加 Cancel（测试可记录调用 / 触发脚本提前收尾，runtime_fake.go）。
- 并发：`session/prompt` 是**异步返回**（followup 立即回 messageId，轮次异步跑），故流式期间 RPC 通道空闲，`session/cancel` 可与事件流并发下发、由 `call`/`readLoop` 按 id 正常配对——wire 可行不阻塞。

### D3. ws：并发 reader + `stop` 帧（本设计核心难点）
- 上行结构加 `Stop bool`：`{text?}` | `{stop:true}`。
- 把 ws 读循环拆成**独立 reader goroutine**：一个 goroutine 只管 `ReadJSON` 并投递到 `inCh`（区分 text / stop）；主循环 `select { case in := <-inCh: ...; case <-turnDone: ... }`。轮次进行中收到 `{stop:true}` → 调 `rt.Cancel(conv.DSHSessionID)`（不取消 turnCtx——那会违反 drain 契约 wedge readLoop；靠 dsh 优雅 abort）。
- 断连（reader EOF）语义保持：不打断落库，drain 到底。
- 并发正确性要点：单连接单活轮（现约束保留）；stop 只对「当前活轮」有效；reader 与 turn 生命周期的竞态用 channel + 明确 turnDone 信号收敛。

### D4. orchestrator：几乎不动
`for ev := range events`（orchestrator.go:99）靠 channel 关闭自然退出；`turn/end aborted` 不被判错 → 干净收尾。取消信号由 ws 直接调 `rt.Cancel`（编排器不持有 per-turn 句柄，无需参与）。前端把 `turn_end`（Error 空）当正常结束即可；可选：给 aborted 轮次一个「已停止」提示帧（orchestrator 认 `turn/end reason.kind==aborted` → 收尾帧带一个 stopped 标记，前端显示「已停止」）。

### D5. 前端
聊天输入区在 `agentTyping` 为真时把「发送」切成「停止」按钮 → WS 发 `{stop:true}`；收到 turn_end 恢复。

## 兜底（路径 3，wrapper 落地前临时）
断 WS + kill/restart 边车：**现状做不到优雅停**（WS 断连不停轮次；`Close()` 杀所有 session 且不可重 spawn）。要能用得新写 `Restart()`（重置 started/closed 允许重 spawn）+ ws 钩子，但单一共享 runtime → 杀一轮=掐所有并发用户 + 丢内存态会话（JSONL 在盘、内存态没了）。粗粒度、伤并发、UX 差，且是抛弃代码。**仅作 D1 落地前的应急，不建议正式做**。

## 演进（路径 2）
若后续某 dsh 版本把 cancel 原生上 wire，或迁 ACP（ACP 的 `session/cancel` 语义**需外部核实官方 spec**，本仓库无 ACP 材料）：则退化为**纯 Go 改动**（去掉自有 bin 的 shim，直接调原生 method），D2/D3/D4/D5 不变。故本设计的 Go 侧改动是面向未来兼容的。

## 风险
- **rc 版内部 API 依赖**：`ctx.agents`/`SessionId`/`this.sessions` 是 `0.1.1-rc.2` 内部结构，rc 升级可能破坏（spec §17:468 已警告）。缓解：shim 尽量薄 + spike 验证 + 版本 pin。
- **ws 并发正确性**：reader/turn 竞态是最易出 bug 处，需专门并发测试（httptest + 真 WS，模拟轮次中途发 stop）。
- **session→agent 映射**：cancel 必须精确命中该 conversation 的 dsh session，错发会中止别人的轮次。

## 需你定夺
1. **是否实现**（这条依赖 patch/fork dsh rc 版内部 API，非纯 Go；rc 升级有维护成本）。
2. **边车落地机制 A/B/C**（自有 bin 组合插件 / patch-package / fork 单文件）——倾向 A。
3. 中止后前端呈现：静默正常结束 vs 显式「已停止」提示帧（倾向后者，D4 可选项）。

## 验收（若实现）
- 聊天中途点「停止」→ 该轮 dsh 优雅 abort（`turn/end aborted`）→ WS 收 turn_end（无 error）→ 前端恢复；已落库的部分消息保留。
- 不影响同 runtime 其它并发会话。
- `rt.Cancel` 精确命中对应 session；ws 并发 reader 无竞态（并发测试锁定）。
- FakeRuntime 单测 + 真边车 spike（发 prompt→中途 session/cancel→观察 aborted+idle）。
