# Agent Chatbot · P2b Go AgentRuntime + 对话编排 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Go 后端**自己 spawn 并驱动 dsh**（newline-JSON-RPC/stdio），取代 P2a 用的 Node `drive.mjs`：实现 `AgentRuntime`（进程管理 + JSON-RPC 编解码 + 事件流）、对话编排（组上下文 → 跑一轮 → 收集助手回答/工具活动/引用 → 落 `agent_conversation`/`agent_message`）、以及**非流式**聊天 API（`POST /api/agent/conversations/{id}/messages` → 返回最终助手消息）。

**Architecture:** 一个长驻 dsh 子进程（惰性启动，首次用时 spawn，此时 HTTP `/internal/mcp` 已监听，dsh 的 mcp-client 能连回来）。单读 goroutine 读 dsh stdout、按 `id` 分发响应、按 `sessionId` 把 `session.event` 路由到该轮的事件 channel、`session.status: idle` 关闭该轮 channel。`Prompt` 返回事件 channel——**编排器**（本期）消费它拼最终消息并落库；**WS**（P2c）将消费同一 channel 推流。**不含** WS/前端（P2c）、写-提议工具（P2d）。

**Tech Stack:** Go 1.26（`os/exec`、`bufio`、`sync`、`encoding/json`、goroutine/channel）、chi。dsh 边车 `services/agent-sidecar/`（已装、cordis 就绪）。模型 `doubao-seed-1-6-250615`（`ZW_AGENT_MODEL`→`DSH_MODEL`）。

**依据：**
- wire 编排范本：`services/agent-sidecar/spike/drive.mjs`（已验证）——initialize `{cwd, provider:"deepseek-official", model}` → session/prompt `{sessionId, contentBlocks:[{type:"text",text}]}`（立即返 `{messageId}`）→ 消费 `session.event`/`session.status` 到 `idle` → shutdown。响应有 `id` 无 `method`；通知有 `method` 无 `id`；每帧一行 `JSON+"\n"`。
- 事件形状（第一个 spike 抓取）：`session.event.event = {type, seq, time, data}`；`assistant/message.data.message.content=[{type:"reasoning",...},{type:"text",text}]`；`tool/call.data={callId,name,arguments}`；`tool/result.data.message.content=[{type:"tool-result",toolCallId,content:[{type:"text",text}],isError}]`；`turn/end.data.reason.kind ∈ completed|error|...`。
- 边车客户端风格：`internal/voiceprint/client.go`（interface + impl + `NewClient`，测试可 mock）。
- 装配：`cmd/zhiwei-server/main.go`（MCP 挂在 156-165；服务 `signal.NotifyContext` + `srv` on `cfg.Port`）。
- Plan 1 仓储：`repo.AgentConversationRepo`（Create/Get/List/Touch/SetDSHSession）、`repo.AgentMessageRepo`（Append/ListByConversation），`AgentMessage{ConversationID *ids.ID, Role, Kind, Content, Citations *json.RawMessage, ToolPayload *json.RawMessage, DSHSeq *int}`。config：`AgentEnabled/AgentModel/AgentCordisConfig/DSHSessionRoot/AgentMCPURL`（Plan 1 已加）。

**贯穿约定：**
- dsh bin 路径：`<sidecarDir>/node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/bin.js`，用 `node` 起（`sidecarDir` = `AgentCordisConfig` 所在目录）。env 注入同 drive.mjs：`ARK_API_KEY`(透传)、`DSH_CORDIS_CONFIG`、`DSH_SESSION_ROOT`、`DSH_MODEL`、`DSH_CWD`、`DSH_HOME`(=sidecarDir/.dsh)、`DSH_SYSTEM_PROMPT`(persona)、`ZW_AGENT_MCP_URL`(=`http://127.0.0.1:<cfg.Port>/internal/mcp`，让子进程 mcp-client 连回本服务)。
- stdout 是协议：用 `bufio.Scanner` 但**必须调大 buffer**（一轮数百帧、单帧可能较大，设 `Buffer(make([]byte,0,64*1024), 4*1024*1024)`）。非 JSON 行忽略。
- 轮次完成 = 收到该 session 的 `session.status:"idle"`（`Prompt` 之后）。错误经 `turn/end.reason.kind=="error"` 到达、不崩进程——编排器需检查并回传错误。
- 单用户 MVP：`user_id=1`；本期无鉴权（同 `/api/*`）。
- 测试：runtime/编排用 **fake**（无需真 dsh）跑单测；真 dsh 的验证走独立 e2e 任务（手动/spike 风格，不进常规单测）。集成库用 `zhiwei_agentchat_test`（DSN 见下），新测试插共享表数据**必须 `t.Cleanup` 自清理**。起真服务需仓库根 `.env`（STEPFUN/TOS keys）+ 避开被占的 8080（用 `ZW_PORT`）。DSN：`zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_agentchat_test?parseTime=true&charset=utf8mb4&multiStatements=true`。

---

## File Structure

| 文件 | 责任 | 动作 |
|---|---|---|
| `internal/agent/event.go` | `Event` 类型 + 事件常量 + 从 `session.event` 解码的 typed 访问器 | Create |
| `internal/agent/runtime.go` | `AgentRuntime` 接口 + `dshRuntime`（spawn/JSON-RPC/事件分发/Prompt/Close） | Create |
| `internal/agent/runtime_fake.go` | `FakeRuntime`：脚本化事件，供编排单测 | Create |
| `internal/agent/runtime_test.go` | fake 驱动的编排单测 + 帧解码单测 | Create |
| `internal/agent/orchestrator.go` | `Orchestrator`：组上下文 → 跑一轮 → 收集/落库 → 返回最终助手消息 | Create |
| `internal/agent/orchestrator_test.go` | 编排单测（fake runtime + repo，集成库） | Create |
| `internal/agent/handlers.go` | `AgentHandler` + `RegisterAgent`：`/api/agent/conversations*`、`POST .../messages`（非流式） | Create |
| `internal/agent/handlers_test.go` | handler 单测（httptest + fake runtime + repo） | Create |
| `cmd/zhiwei-server/main.go` | 构造 runtime + orchestrator（惰性启动）+ `RegisterAgent` + 关停时 `runtime.Close()` | Modify |
| `internal/config/config.go` | 加 `DSHSystemPrompt`（persona，env `DSH_SYSTEM_PROMPT`，有默认） | Modify |

**类型契约：** `agent.AgentRuntime` 接口 `{ Prompt(ctx, sessionID string, text string) (<-chan Event, error); Close() error }`；`agent.NewDSHRuntime(RuntimeConfig) *dshRuntime`（实现接口）；`agent.FakeRuntime`（实现接口，测试用）；`agent.Orchestrator{Runtime, Conversations, Messages, ...}`；`agent.NewOrchestrator(...)`；`orchestrator.RunTurn(ctx, convID ids.ID, userText string) (*repo.AgentMessage, error)`（返回落库后的最终 assistant 消息）。

---

## Task 1: AgentRuntime — spawn + 驱动 dsh（核心）

**Files:** `internal/agent/event.go`、`internal/agent/runtime.go`（create）

- [ ] **Step 1: 写事件类型 `internal/agent/event.go`**

```go
package agent

import "encoding/json"

// 事件类型常量（对齐 dsh SessionEventMap，spike 抓取）。
const (
	EvAssistantChunk   = "assistant/chunk"
	EvAssistantMessage = "assistant/message"
	EvToolCall         = "tool/call"
	EvToolResult       = "tool/result"
	EvTurnEnd          = "turn/end"
)

// Event 是 dsh session.event 里的一条事件（Data 为其 data 字段原文，按 Type 解码）。
type Event struct {
	Type string          `json:"type"`
	Seq  int             `json:"seq"`
	Data json.RawMessage `json:"data"`
}

// AssistantText 从 assistant/message 事件提取纯文本（拼接 content 里的 text 块）。
func (e Event) AssistantText() string {
	var d struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return ""
	}
	var s string
	for _, b := range d.Message.Content {
		if b.Type == "text" {
			s += b.Text
		}
	}
	return s
}

// ToolCall 从 tool/call 事件提取 {callId, name, arguments(JSON 字符串)}。
func (e Event) ToolCall() (callID, name, arguments string) {
	var d struct {
		CallID    string `json:"callId"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	_ = json.Unmarshal(e.Data, &d)
	return d.CallID, d.Name, d.Arguments
}

// ToolResultText 从 tool/result 事件提取首个 tool-result 的文本与 isError。
func (e Event) ToolResultText() (text string, isError bool) {
	var d struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				IsError   bool   `json:"isError"`
				Content   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return "", false
	}
	for _, c := range d.Message.Content {
		if c.Type == "tool-result" {
			for _, t := range c.Content {
				if t.Type == "text" {
					text += t.Text
				}
			}
			return text, c.IsError
		}
	}
	return "", false
}

// TurnEndErr 若 turn/end.reason.kind=="error" 返回错误信息，否则空串。
func (e Event) TurnEndErr() string {
	var d struct {
		Reason struct {
			Kind  string `json:"kind"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"reason"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return ""
	}
	if d.Reason.Kind == "error" {
		if d.Reason.Error.Message != "" {
			return d.Reason.Error.Message
		}
		return "turn ended with error"
	}
	return ""
}
```

- [ ] **Step 2: 写 `internal/agent/runtime.go`**

```go
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
)

// AgentRuntime 抽象「驱动一个 agent 跑一轮对话」。dsh 实现 + fake 实现（测试）。
type AgentRuntime interface {
	// Prompt 向某会话发一条用户消息，返回该轮的事件流 channel（轮次结束时关闭）。
	// 同一 sessionID 复用会话记忆；channel 关闭表示该轮 idle。
	Prompt(ctx context.Context, sessionID, text string) (<-chan Event, error)
	// Close 关停底层 dsh 进程。
	Close() error
}

// RuntimeConfig 是 dshRuntime 的启动参数（来自 config）。
type RuntimeConfig struct {
	CordisConfig string // DSH_CORDIS_CONFIG（cordis.yml 绝对/相对路径）
	Model        string // DSH_MODEL
	SessionRoot  string // DSH_SESSION_ROOT
	SystemPrompt string // DSH_SYSTEM_PROMPT
	MCPURL       string // ZW_AGENT_MCP_URL（子进程 mcp-client 连回本服务）
}

type rpcResp struct {
	result json.RawMessage
	err    error
}

// dshRuntime spawn 一个长驻 dsh 子进程并用 JSON-RPC/stdio 驱动它。
type dshRuntime struct {
	cfg RuntimeConfig

	mu      sync.Mutex
	started bool
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	nextID  int
	pending map[int]chan rpcResp   // 请求 id -> 响应
	turns   map[string]chan Event  // sessionId -> 当前轮事件流
}

// NewDSHRuntime 构造（尚未 spawn；首次 Prompt 时惰性启动）。
func NewDSHRuntime(cfg RuntimeConfig) *dshRuntime {
	return &dshRuntime{cfg: cfg, pending: map[int]chan rpcResp{}, turns: map[string]chan Event{}}
}

func (r *dshRuntime) sidecarDir() string { return filepath.Dir(r.cfg.CordisConfig) }

func (r *dshRuntime) binPath() string {
	return filepath.Join(r.sidecarDir(), "node_modules", "@deepseek-ai", "dsh-sdk-jsonrpc-demo", "lib", "bin.js")
}

// ensureStarted 惰性 spawn dsh + 起读 goroutine + initialize 握手（只做一次）。
func (r *dshRuntime) ensureStarted(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return nil
	}
	dir := r.sidecarDir()
	cmd := exec.Command("node", r.binPath())
	cmd.Dir = dir
	cmd.Env = append(commonEnv(), r.dshEnv()...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		r.mu.Unlock()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.mu.Unlock()
		return err
	}
	cmd.Stderr = stderrLogWriter() // 见下：子进程 stderr → 日志（非协议）
	if err := cmd.Start(); err != nil {
		r.mu.Unlock()
		return fmt.Errorf("spawn dsh: %w", err)
	}
	r.cmd = cmd
	r.stdin = stdin
	r.started = true
	r.mu.Unlock()

	go r.readLoop(stdout)

	// initialize 握手（一次）。provider 固定 deepseek-official（llm-deepseek 注册的 route）。
	_, err = r.call(ctx, "initialize", map[string]any{
		"cwd": dir, "provider": "deepseek-official", "model": r.cfg.Model,
	})
	return err
}

// dshEnv 组装 DSH_* 环境变量。
func (r *dshRuntime) dshEnv() []string {
	return []string{
		"DSH_CORDIS_CONFIG=" + r.cfg.CordisConfig,
		"DSH_SESSION_ROOT=" + r.cfg.SessionRoot,
		"DSH_MODEL=" + r.cfg.Model,
		"DSH_CWD=" + r.sidecarDir(),
		"DSH_HOME=" + filepath.Join(r.sidecarDir(), ".dsh"),
		"DSH_SYSTEM_PROMPT=" + r.cfg.SystemPrompt,
		"ZW_AGENT_MCP_URL=" + r.cfg.MCPURL,
	}
}

// readLoop 是唯一的 stdout 读者：解析每行帧，分发响应/通知。
func (r *dshRuntime) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 大 buffer：单帧可能较大
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var frame struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			} `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &frame) != nil {
			continue // 非 JSON 行忽略
		}
		if frame.ID != nil && frame.Method == "" {
			r.deliverResp(*frame.ID, frame.Result, frame.Error)
			continue
		}
		if frame.Method != "" {
			r.onNotification(frame.Method, frame.Params)
		}
	}
	// stdout 关闭（进程退出）：关闭所有未决轮次 channel，避免消费者永久阻塞。
	r.mu.Lock()
	for sid, ch := range r.turns {
		close(ch)
		delete(r.turns, sid)
	}
	r.mu.Unlock()
}

func (r *dshRuntime) deliverResp(id int, result json.RawMessage, rpcErr *struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}) {
	r.mu.Lock()
	ch := r.pending[id]
	delete(r.pending, id)
	r.mu.Unlock()
	if ch == nil {
		return
	}
	if rpcErr != nil {
		ch <- rpcResp{err: fmt.Errorf("dsh rpc error: %s (code %d)", rpcErr.Message, rpcErr.Code)}
	} else {
		ch <- rpcResp{result: result}
	}
}

func (r *dshRuntime) onNotification(method string, params json.RawMessage) {
	switch method {
	case "session.event":
		var p struct {
			SessionID string `json:"sessionId"`
			Event     Event  `json:"event"`
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		r.mu.Lock()
		ch := r.turns[p.SessionID]
		r.mu.Unlock()
		if ch != nil {
			ch <- p.Event // 消费者需及时 drain（编排器/WS）
		}
	case "session.status":
		var p struct {
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		if p.Status == "idle" {
			r.mu.Lock()
			if ch := r.turns[p.SessionID]; ch != nil {
				close(ch)
				delete(r.turns, p.SessionID)
			}
			r.mu.Unlock()
		}
	}
}

// call 发一个请求并等响应（用于 initialize/prompt/shutdown）。
func (r *dshRuntime) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	respCh := make(chan rpcResp, 1)
	r.pending[id] = respCh
	frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	b, _ := json.Marshal(frame)
	b = append(b, '\n')
	_, werr := r.stdin.Write(b)
	r.mu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("write %s: %w", method, werr)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-respCh:
		return resp.result, resp.err
	}
}

func (r *dshRuntime) Prompt(ctx context.Context, sessionID, text string) (<-chan Event, error) {
	if err := r.ensureStarted(ctx); err != nil {
		return nil, err
	}
	ch := make(chan Event, 64)
	r.mu.Lock()
	r.turns[sessionID] = ch
	r.mu.Unlock()
	_, err := r.call(ctx, "session/prompt", map[string]any{
		"sessionId":     sessionID,
		"contentBlocks": []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		r.mu.Lock()
		delete(r.turns, sessionID)
		r.mu.Unlock()
		close(ch)
		return nil, err
	}
	return ch, nil
}

func (r *dshRuntime) Close() error {
	r.mu.Lock()
	started := r.started
	stdin := r.stdin
	cmd := r.cmd
	r.mu.Unlock()
	if !started {
		return nil
	}
	// best-effort shutdown（忽略错误），关 stdin 让 bin 退出。
	_, _ = r.call(context.Background(), "shutdown", nil)
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return nil
}
```

> `commonEnv()`、`stderrLogWriter()` 见 Step 3。`provider:"deepseek-official"` 是 `llm-deepseek` 注册的 route（drive.mjs 同）。
>
> **并发实现要点（务必）**：`ensureStarted` 必须在 **initialize 完成后**才允许任何 `Prompt` 发 `session/prompt`——用一个专用互斥（如 `startMu`）把「spawn + 起 readLoop + initialize + 置 `started=true`」整段串行化持有，避免并发首次 `Prompt` 抢跑（上面代码把这段分散在 `mu` 下、存在竞态，实现时改为 `startMu` 整段守护）。`cmd/stdin/started` 等生命周期字段在 `startMu` 下写、`Close` 也用 `startMu` 读；`pending/nextID/turns` 与 stdin 写用 `r.mu`。另把 `readLoop` 里 `frame.Error` 与 `deliverResp` 的匿名 struct 抽成具名类型 `type rpcError struct { Message string \`json:"message"\`; Code int \`json:"code"\` }`，更清晰。

- [ ] **Step 3: env 与 stderr 辅助（追加到 `runtime.go`）**

```go
import "os" // 加到 runtime.go 的 import 块

// commonEnv 返回当前进程环境（透传 ARK_API_KEY 等）。
func commonEnv() []string { return os.Environ() }

// stderrLogWriter 把子进程 stderr 导到主进程 stderr（诊断，非协议）。
func stderrLogWriter() io.Writer { return os.Stderr }
```
（若要文件日志可后续换；本期直接透到 stderr 简单可查。）

- [ ] **Step 4: 编译检查**

Run: `go build ./internal/agent/`
Expected: 通过（暂无使用者）。`gofmt -l internal/agent`、`go vet ./internal/agent/` 干净。

- [ ] **Step 5: Commit**

```bash
git add internal/agent/event.go internal/agent/runtime.go
git commit -m "feat(agent-runtime): AgentRuntime 接口 + dshRuntime(spawn dsh + JSON-RPC/stdio 驱动 + 事件分发)"
```

---

## Task 2: FakeRuntime + 编排器 Orchestrator

**Files:** `internal/agent/runtime_fake.go`、`internal/agent/orchestrator.go`（create），`internal/config/config.go`（modify：加 DSHSystemPrompt）

- [ ] **Step 1: config 加 persona**

在 `internal/config/config.go` 的 `Config` 结构体 Agent 段加字段：
```go
	DSHSystemPrompt   string // DSH_SYSTEM_PROMPT：dsh 进程级人设
```
在 `Load()` 的 Agent 段加：
```go
		DSHSystemPrompt:   getenv("DSH_SYSTEM_PROMPT", "你是知微(zhiwei)个人智能体，基于用户的记忆/时间线/话题/待办用简体中文亲切、简洁地回答；需要时调用工具读取用户数据，不要编造。"),
```

- [ ] **Step 2: FakeRuntime `internal/agent/runtime_fake.go`**

```go
package agent

import "context"

// FakeRuntime 是测试用运行时：每次 Prompt 回放一组预设事件后关闭 channel。
type FakeRuntime struct {
	// Script 按调用顺序返回事件序列；用尽后返回最后一组（或空）。
	Script [][]Event
	// LastPrompt 记录最近一次 Prompt 的入参，供断言。
	LastSessionID string
	LastText      string
	Err           error // 非 nil 时 Prompt 返回该错误
	calls         int
}

func (f *FakeRuntime) Prompt(_ context.Context, sessionID, text string) (<-chan Event, error) {
	f.LastSessionID, f.LastText = sessionID, text
	if f.Err != nil {
		return nil, f.Err
	}
	var evs []Event
	if f.calls < len(f.Script) {
		evs = f.Script[f.calls]
	} else if len(f.Script) > 0 {
		evs = f.Script[len(f.Script)-1]
	}
	f.calls++
	ch := make(chan Event, len(evs)+1)
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (f *FakeRuntime) Close() error { return nil }
```

- [ ] **Step 3: Orchestrator `internal/agent/orchestrator.go`**

```go
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"zhiwei/internal/repo"
)

// Orchestrator 跑一轮对话：落用户消息 → 驱动 runtime → 消费事件流（拼助手文本、
// 记工具活动）→ 落助手消息 → 刷新会话活跃时间 → 返回落库后的助手消息。
type Orchestrator struct {
	Runtime       AgentRuntime
	Conversations *repo.AgentConversationRepo
	Messages      *repo.AgentMessageRepo
}

func NewOrchestrator(rt AgentRuntime, conv *repo.AgentConversationRepo, msg *repo.AgentMessageRepo) *Orchestrator {
	return &Orchestrator{Runtime: rt, Conversations: conv, Messages: msg}
}

// RunTurn 处理一条用户消息：需要 conv 已存在（handler 负责创建/校验）。
// 返回落库后的最终 assistant 文本消息。工具调用/结果作为 kind=tool_call/tool_result
// 的消息落库（tool_payload 存原始）。turn/end 错误则整轮返回 error（助手消息仍尽量落）。
func (o *Orchestrator) RunTurn(ctx context.Context, conv *repo.AgentConversation, userText string) (*repo.AgentMessage, error) {
	// 1) 落用户消息
	um := &repo.AgentMessage{ConversationID: &conv.ID, Role: "user", Content: userText}
	if err := o.Messages.Append(ctx, um); err != nil {
		return nil, err
	}
	// 2) 驱动 runtime（sessionID 用会话的 dsh_session_id）
	events, err := o.Runtime.Prompt(ctx, conv.DSHSessionID, userText)
	if err != nil {
		return nil, err
	}
	// 3) 消费事件流
	var finalText strings.Builder
	var turnErr string
	for ev := range events {
		switch ev.Type {
		case EvAssistantMessage:
			finalText.WriteString(ev.AssistantText())
		case EvToolCall:
			callID, name, args := ev.ToolCall()
			payload, _ := json.Marshal(map[string]any{"call_id": callID, "name": name, "arguments": args})
			raw := json.RawMessage(payload)
			_ = o.Messages.Append(ctx, &repo.AgentMessage{
				ConversationID: &conv.ID, Role: "assistant", Kind: "tool_call",
				Content: name, ToolPayload: &raw,
			})
		case EvToolResult:
			text, isErr := ev.ToolResultText()
			payload, _ := json.Marshal(map[string]any{"text": text, "is_error": isErr})
			raw := json.RawMessage(payload)
			_ = o.Messages.Append(ctx, &repo.AgentMessage{
				ConversationID: &conv.ID, Role: "assistant", Kind: "tool_result",
				Content: text, ToolPayload: &raw,
			})
		case EvTurnEnd:
			if msg := ev.TurnEndErr(); msg != "" {
				turnErr = msg
			}
		}
	}
	// 4) 落最终 assistant 文本消息
	am := &repo.AgentMessage{ConversationID: &conv.ID, Role: "assistant", Kind: "text", Content: finalText.String()}
	if err := o.Messages.Append(ctx, am); err != nil {
		return nil, err
	}
	// 5) 刷新会话活跃时间
	_ = o.Conversations.Touch(ctx, conv.ID)
	if turnErr != "" {
		return am, fmt.Errorf("agent 轮次错误: %s", turnErr)
	}
	return am, nil
}
```

> 说明：本期上下文头（日期/owner 概要/检索种子）先不注入——`session/prompt` 直接送用户文本，dsh persona 已在进程级设好；上下文头留 P2c/P3 增强（避免本期 YAGNI）。

- [ ] **Step 4: Commit**

```bash
git add internal/agent/runtime_fake.go internal/agent/orchestrator.go internal/config/config.go
git commit -m "feat(agent-runtime): FakeRuntime + Orchestrator(跑一轮→落 agent_message)+ persona 配置"
```

---

## Task 3: 编排单测（fake + 集成库）

**Files:** `internal/agent/orchestrator_test.go`（create）

- [ ] **Step 1: 写测试**

```go
package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"zhiwei/internal/repo"
)

func orchDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	return dsn
}

func TestOrchestratorRunTurn(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()

	conv := &repo.AgentConversation{Title: "编排测试"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { /* 无级联删；会话/消息残留无害，membership 断言抗污染 */ })

	// 脚本：一次工具调用 + 结果 + 最终 assistant 文本
	toolResData, _ := json.Marshal(map[string]any{
		"message": map[string]any{"content": []map[string]any{
			{"type": "tool-result", "toolCallId": "c1", "isError": false,
				"content": []map[string]any{{"type": "text", "text": "[{\"title\":\"待办A\"}]"}}},
		}},
	})
	callData, _ := json.Marshal(map[string]any{"callId": "c1", "name": "mcp__zhiwei__get_todos", "arguments": "{}"})
	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "你有 1 条待办：待办A。"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{
		{Type: EvToolCall, Data: callData},
		{Type: EvToolResult, Data: toolResData},
		{Type: EvAssistantMessage, Data: msgData},
	}}}

	orch := NewOrchestrator(fake, convRepo, msgRepo)
	final, err := orch.RunTurn(ctx, conv, "我有哪些待办？")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if final.Content != "你有 1 条待办：待办A。" {
		t.Errorf("最终助手文本异常: %q", final.Content)
	}
	if fake.LastText != "我有哪些待办？" || fake.LastSessionID != conv.DSHSessionID {
		t.Errorf("Prompt 入参异常: text=%q sid=%q", fake.LastText, fake.LastSessionID)
	}

	// 落库校验：user + tool_call + tool_result + assistant text = 4 条
	msgs, err := msgRepo.ListByConversation(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("应落 4 条消息, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[3].Kind != "text" || msgs[3].Content == "" {
		t.Errorf("消息序列异常: %+v", msgs)
	}
	var sawToolCall bool
	for _, m := range msgs {
		if m.Kind == "tool_call" && m.Content == "mcp__zhiwei__get_todos" {
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Error("未落 tool_call 消息")
	}
}

func TestOrchestratorTurnError(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "错误轮次"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	errData, _ := json.Marshal(map[string]any{"reason": map[string]any{
		"kind": "error", "error": map[string]any{"message": "model 404"},
	}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvTurnEnd, Data: errData}}}}
	orch := NewOrchestrator(fake, convRepo, msgRepo)
	_, err = orch.RunTurn(ctx, conv, "hi")
	if err == nil {
		t.Error("turn/end error 应使 RunTurn 返回错误")
	}
}
```

- [ ] **Step 2: 跑测试**

Run: `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_agentchat_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/agent/ -run TestOrchestrator -v` → PASS。
再 `go build ./...`、`gofmt -l internal/agent`、`go vet ./internal/agent/`。

- [ ] **Step 3: Commit**

```bash
git add internal/agent/orchestrator_test.go
git commit -m "test(agent-runtime): Orchestrator 单测(fake runtime, 落库序列 + 错误轮次)"
```

---

## Task 4: 聊天 API（非流式）+ 主服务装配

**Files:** `internal/agent/handlers.go`、`internal/agent/handlers_test.go`（create），`cmd/zhiwei-server/main.go`（modify）

- [ ] **Step 1: handlers `internal/agent/handlers.go`**

```go
package agent

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// AgentHandler 提供对话 REST（本期非流式；WS 流式见 P2c）。
type AgentHandler struct {
	Orch          *Orchestrator
	Conversations *repo.AgentConversationRepo
	Messages      *repo.AgentMessageRepo
}

// RegisterAgent 挂载 /api/agent 路由。
func RegisterAgent(r chi.Router, h *AgentHandler) {
	r.Post("/api/agent/conversations", h.createConversation)
	r.Get("/api/agent/conversations", h.listConversations)
	r.Get("/api/agent/conversations/{cid}", h.getConversation)
	r.Post("/api/agent/conversations/{cid}/messages", h.postMessage)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *AgentHandler) createConversation(w http.ResponseWriter, r *http.Request) {
	var body struct{ Title string `json:"title"` }
	_ = json.NewDecoder(r.Body).Decode(&body)
	c := &repo.AgentConversation{Title: body.Title}
	if err := h.Conversations.Create(r.Context(), c); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, c)
}

func (h *AgentHandler) listConversations(w http.ResponseWriter, r *http.Request) {
	list, err := h.Conversations.List(r.Context(), 1)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, list)
}

func (h *AgentHandler) getConversation(w http.ResponseWriter, r *http.Request) {
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid cid"})
		return
	}
	msgs, err := h.Messages.ListByConversation(r.Context(), cid)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"conversation_id": cid, "messages": msgs})
}

func (h *AgentHandler) postMessage(w http.ResponseWriter, r *http.Request) {
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid cid"})
		return
	}
	var body struct{ Text string `json:"text"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "text required"})
		return
	}
	conv, err := h.Conversations.Get(r.Context(), cid)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "conversation not found"})
		return
	}
	final, err := h.Orch.RunTurn(r.Context(), conv, body.Text)
	if err != nil {
		// 即使轮次报错，final 可能已落库；回传错误 + 已知内容
		writeJSON(w, 502, map[string]any{"error": err.Error(), "assistant": final})
		return
	}
	writeJSON(w, 200, final)
}
```

- [ ] **Step 2: handler 单测 `internal/agent/handlers_test.go`**

```go
package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/repo"
)

func TestPostMessageEndToEndFake(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "API 测试"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}

	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "答复内容"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	h := &AgentHandler{Orch: NewOrchestrator(fake, convRepo, msgRepo), Conversations: convRepo, Messages: msgRepo}
	r := chi.NewRouter()
	RegisterAgent(r, h)

	req := httptest.NewRequest("POST", "/api/agent/conversations/"+conv.ID.String()+"/messages",
		strings.NewReader(`{"text":"你好"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got repo.AgentMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("resp 非 AgentMessage: %v", err)
	}
	if got.Content != "答复内容" || got.Role != "assistant" {
		t.Errorf("响应异常: %+v", got)
	}
}
```

- [ ] **Step 3: 跑测试**

Run: `TEST_MYSQL_DSN="...zhiwei_agentchat_test..." go test ./internal/agent/ -run TestPostMessage -v` → PASS。

- [ ] **Step 4: 主服务装配 `cmd/zhiwei-server/main.go`**

在 MCP 挂载（`r.Handle("/internal/mcp/*", ...)`）之后、`srv := &http.Server{...}` 之前加：
```go
	// Agent 运行时（惰性 spawn dsh；首次对话时启动，此时 /internal/mcp 已监听）。
	if cfg.AgentEnabled {
		rt := agent.NewDSHRuntime(agent.RuntimeConfig{
			CordisConfig: cfg.AgentCordisConfig,
			Model:        cfg.AgentModel,
			SessionRoot:  cfg.DSHSessionRoot,
			SystemPrompt: cfg.DSHSystemPrompt,
			MCPURL:       "http://127.0.0.1:" + cfg.Port + "/internal/mcp",
		})
		defer rt.Close()
		agentConvs := &repo.AgentConversationRepo{DB: db}
		agentMsgs := &repo.AgentMessageRepo{DB: db}
		agent.RegisterAgent(r, &agent.AgentHandler{
			Orch:          agent.NewOrchestrator(rt, agentConvs, agentMsgs),
			Conversations: agentConvs,
			Messages:      agentMsgs,
		})
	}
```
（`defer rt.Close()` 在 main 返回时关停 dsh；main 末尾已有 `<-ctx.Done()` 阻塞到信号。确保 `agent` 已 import——已在。）

- [ ] **Step 5: 构建 + Commit**

Run: `go build ./cmd/zhiwei-server` → 通过。
```bash
git add internal/agent/handlers.go internal/agent/handlers_test.go cmd/zhiwei-server/main.go
git commit -m "feat(agent-api): /api/agent/conversations* + 非流式 postMessage(RunTurn) + 主服务装配(惰性 dsh)"
```

---

## Task 5: 端到端验证（Go 后端驱动 dsh + 真工具 + 真数据）

**Files:** 无（验证 + 记录）

> 复用 P2a 的运维套路：8080 被占 → `ZW_PORT=18080` + 主服务 MCPURL 自动跟随 `cfg.Port`；起服务 source 仓库根 `.env`；用 `zhiwei_agentchat_test` 或有数据的库。**关键**：本期不用 Node drive.mjs——由 Go 后端自己 spawn dsh。

- [ ] **Step 1: 起服务（含 agent runtime）**

写 `/tmp` 启动脚本：source 仓库根 `.env`，设 `ZW_MYSQL_DSN`（有数据的库，如 seed 过的 `zhiwei_agentchat_test`）、`ZW_PORT=18080`、`ZW_AGENT_MODEL=doubao-seed-1-6-250615`、`ZW_AGENT_ENABLED=true`，`go run ./cmd/zhiwei-server`。后台启动，Read 日志确认监听 18080。

- [ ] **Step 2: seed 一条数据 + POST 一条消息**

seed 一条带唯一关键词的 memory 到该库（同 P2a Task 5 的一次性 Go 程序）。然后 `curl`（或写个 Node/脚本文件避开 shell guard）：
- 新建会话：`POST http://127.0.0.1:18080/api/agent/conversations {"title":"e2e"}` → 拿 `id`。
- 发消息：`POST .../conversations/<id>/messages {"text":"用 search_memory 搜索关键词 <kw>，把标题告诉我"}`。
Expected: 响应是一条 assistant `AgentMessage`，`content` 含 seed 的标题；且 `GET .../conversations/<id>` 显示落库了 user/tool_call/tool_result/assistant 序列。**这证明 Go 后端 spawn 的 dsh 经 /internal/mcp 真调了工具、读了真数据、Go 落了库。**

- [ ] **Step 3: 记录 + 收尾**

记录验收结论（Go 驱动、真工具调用、落库序列）。停服务、清 `/tmp` 脚本、确认 `git status` 无意外新增。若 dsh 首轮启动慢/超时，调 handler 或 runtime 的超时（本期 `call` 用 `r.Context()`；HTTP handler 的 request context 默认无超时，够用；如需可加）。

---

## 收尾验收（P2b 完成标志）

- [ ] `go build ./...`、`go vet ./internal/agent`、gofmt 干净。
- [ ] fake 驱动的 Orchestrator/handler 单测在独立库全绿（RunTurn 落库序列、错误轮次、postMessage）。
- [ ] Go 后端能 spawn 并驱动 dsh 跑完一轮（Task 5 e2e）：真工具调用 + 真数据 + 落 `agent_message`，**全程无 Node drive.mjs**。
- [ ] `defer rt.Close()` 在关停时干净退出 dsh。

**下一步（不在本计划内）：** P2c = WS 端点（消费 `Prompt` 的事件 channel 推流 assistant/chunk + tool 卡片）+ 「问知微」前端（Vue tab）。P2d = 写-提议工具 + 确认卡。
