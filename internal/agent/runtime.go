package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// AgentRuntime 抽象「驱动一个 agent 跑一轮对话」。dsh 实现 + fake 实现（测试）。
type AgentRuntime interface {
	// Prompt 向某会话发一条用户消息，返回该轮的事件流 channel（轮次结束时关闭）。
	// 同一 sessionID 复用会话记忆；channel 关闭表示该轮 idle。
	Prompt(ctx context.Context, sessionID, text string) (<-chan Event, error)
	// Warm 预热运行时：提前 spawn 子进程 + 完成握手，使首个 Prompt 不必现等启动。幂等（已启动则 no-op）。
	Warm(ctx context.Context) error
	// Cancel 请求 dsh 优雅中止 sessionID 进行中的一轮（用户点「停止」）。
	// 语义：发 session/cancel RPC 让 dsh 内部 agent.cancel({kind:'user'}) 优雅 abort
	// → 产生 turn/end reason.kind=aborted + session.status:idle → readLoop 收到 idle 后
	// close(turns[sid]) → RunTurnStream 的 drain 循环因 channel 关闭而自然返回。
	// 关键：Cancel【不】取消该轮的事件流 channel、也不碰 turnCtx——那会违反 drain 契约、
	// wedge 唯一的 readLoop；取消完全靠 dsh 优雅 abort 后 channel 自然关闭来收尾。
	// dsh 若不支持该 method → 返回 rpc 错误；调用方应吞掉并记日志，轮次照旧靠 idle/超时收尾。
	Cancel(ctx context.Context, sessionID string) error
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

// rpcResp 是一次 JSON-RPC 请求的结果（result 与 err 二选一）。
type rpcResp struct {
	result json.RawMessage
	err    error
}

// rpcError 是 JSON-RPC 响应帧里的 error 对象（具名类型，供 readLoop/deliverResp 复用）。
type rpcError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// errDSHExited 在 dsh 子进程退出（stdout EOF）时返回给所有阻塞中的 call，
// 让它们以错误返回而非永久挂起。
var errDSHExited = errors.New("dsh 进程已退出")

// dshRuntime spawn 一个长驻 dsh 子进程并用 JSON-RPC/stdio 驱动它。
//
// 并发模型（两把锁，职责严格分开，避免数据竞争）：
//   - startMu 只守护「生命周期字段」cmd/stdin/started，以及 ensureStarted / Close
//     这两段生命周期流程。ensureStarted 把「spawn + 起 readLoop + initialize 握手 +
//     置 started=true」整段串行化持有 startMu，确保任何 Prompt 发 session/prompt 之前
//     initialize 一定已完成（否则并发首次 Prompt 会抢跑）。
//   - mu 守护「运行期共享状态」pending/nextID/turns，以及对 stdin 的并发写（序列化，
//     避免两条帧交错写坏协议流）。
//
// stdin/started/cmd 只在 ensureStarted 里（持 startMu）写一次；之后只读。所有读都发生在
// 一次 ensureStarted（acquire+release startMu）之后，因此与那次写存在 happens-before，
// 无数据竞争。
type dshRuntime struct {
	cfg RuntimeConfig

	// 生命周期字段：仅在 startMu 保护下访问。
	startMu sync.Mutex
	started bool
	closed  bool // Close 是否已调用（幂等守卫）
	cmd     *exec.Cmd
	stdin   io.WriteCloser

	// 运行期共享状态 + stdin 写：在 mu 保护下访问。
	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResp  // 请求 id -> 响应
	turns   map[string]chan Event // sessionId -> 当前轮事件流
}

// NewDSHRuntime 构造（尚未 spawn；首次 Prompt 时惰性启动）。
func NewDSHRuntime(cfg RuntimeConfig) *dshRuntime {
	return &dshRuntime{cfg: cfg, pending: map[int]chan rpcResp{}, turns: map[string]chan Event{}}
}

// sidecarDir 返回边车目录的【绝对路径】。必须绝对：spawn 时 cmd.Dir 设为本目录，
// 若 binPath/DSH_CORDIS_CONFIG 用相对路径会相对 cmd.Dir 再解析一次→路径翻倍（
// 如 services/agent-sidecar/services/agent-sidecar/...）导致 MODULE_NOT_FOUND。
// 绝对化后 node 的 bin.js 实参与 cordis 配置均与 cwd 无关。
func (r *dshRuntime) sidecarDir() string {
	dir := filepath.Dir(r.cfg.CordisConfig)
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func (r *dshRuntime) binPath() string {
	return filepath.Join(r.sidecarDir(), "node_modules", "@deepseek-ai", "dsh-sdk-jsonrpc-demo", "lib", "bin.js")
}

// ensureStarted 惰性 spawn dsh + 起读 goroutine + initialize 握手（只做一次）。
//
// 整段持有 startMu：串行化「spawn + 起 readLoop + initialize + 置 started=true」，并且
// 只有在 initialize 成功后才置 started=true——这样并发首次 Prompt 不会在握手完成前抢发
// session/prompt。
func (r *dshRuntime) ensureStarted(ctx context.Context) error {
	r.startMu.Lock()
	defer r.startMu.Unlock()
	if r.started {
		return nil
	}
	dir := r.sidecarDir()
	cmd := exec.Command("node", r.binPath())
	cmd.Dir = dir
	cmd.Env = append(commonEnv(), r.dshEnv()...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = stderrLogWriter() // 见下：子进程 stderr → 日志（非协议）
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn dsh: %w", err)
	}
	r.cmd = cmd
	r.stdin = stdin
	go r.readLoop(stdout)
	// initialize 握手（一次）。provider 固定 deepseek-official（llm-deepseek 注册的 route）。
	if _, err := r.call(ctx, "initialize", map[string]any{
		"cwd": dir, "provider": "deepseek-official", "model": r.cfg.Model,
	}); err != nil {
		// 握手失败：杀掉子进程并清理句柄，避免进程泄漏。started 保持 false，
		// 后续 Prompt 可干净重启；cmd/stdin 置 nil，Close 也不会碰这个已死的 cmd。
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		r.cmd = nil
		r.stdin = nil
		return err
	}
	r.started = true
	return nil
}

// dshEnv 组装 DSH_* 环境变量。CordisConfig 绝对化：dsh 子进程 cwd=sidecarDir，
// 相对配置路径会相对 cwd 再解析一次而翻倍/找不到。
func (r *dshRuntime) dshEnv() []string {
	cordis := r.cfg.CordisConfig
	if abs, err := filepath.Abs(cordis); err == nil {
		cordis = abs
	}
	return []string{
		"DSH_CORDIS_CONFIG=" + cordis,
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
			Error  *rpcError       `json:"error"`
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
	// stdout 关闭（进程退出）：关闭所有未决轮次 channel，避免消费者永久阻塞；
	// 同时让所有阻塞中的 call 以 errDSHExited 返回（respCh 是 buffered(1)，send 不会阻塞）。
	r.mu.Lock()
	for sid, ch := range r.turns {
		close(ch)
		delete(r.turns, sid)
	}
	for id, ch := range r.pending {
		ch <- rpcResp{err: errDSHExited}
		delete(r.pending, id)
	}
	r.mu.Unlock()
}

func (r *dshRuntime) deliverResp(id int, result json.RawMessage, rpcErr *rpcError) {
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
	frame := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		frame["params"] = params // 省略 nil params，避免协议流里出现 "params":null
	}
	b, _ := json.Marshal(frame)
	b = append(b, '\n')
	_, werr := r.stdin.Write(b)
	r.mu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("write %s: %w", method, werr)
	}
	select {
	case <-ctx.Done():
		// ctx 取消/超时：清理 pending，避免响应永远不来时的永久泄漏。
		// respCh 是 buffered(1)，即便 deliverResp 与此处竞态也不会阻塞。
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return nil, ctx.Err()
	case resp := <-respCh:
		return resp.result, resp.err
	}
}

// Warm 预热：提前 spawn dsh 子进程 + initialize 握手（幂等，已启动则直接返回）。
// 供服务启动后后台预热 owner 运行时，把 node 启动 + 握手的一次性延迟从「首条消息」挪到启动阶段。
func (r *dshRuntime) Warm(ctx context.Context) error { return r.ensureStarted(ctx) }

// Prompt 向某会话发一条用户消息，返回该轮的事件流 channel（轮次结束时由 readLoop 关闭）。
//
// 消费契约（调用方必须遵守）：调用方必须及时且无条件地把返回的 channel drain 到关闭为止。
// 返回的 channel 是 buffered 的；一旦消费者 stall 或提前放弃（不再接收），buffer 填满后就会
// 阻塞唯一的读 goroutine（readLoop），进而 wedge 整个 runtime——任何 session 都不会再收到
// 后续的 RPC 响应/事件。P2c 的 WS 消费者必须遵守本契约（即便自身 ctx 已取消，也要把 channel
// drain 完）。中止一轮请用 Cancel（发 session/cancel 让 dsh 优雅 abort→idle→本 channel 自然
// 关闭→drain 循环结束）；绝不能用 ctx.cancel/提前弃读来「中止」——那正是会 wedge readLoop 的做法。
//
// 单轮次契约：同一 sessionID 同时只能有一个进行中的轮次；若已存在则直接返回错误。
func (r *dshRuntime) Prompt(ctx context.Context, sessionID, text string) (<-chan Event, error) {
	if err := r.ensureStarted(ctx); err != nil {
		return nil, err
	}
	ch := make(chan Event, 64)
	r.mu.Lock()
	if _, exists := r.turns[sessionID]; exists {
		// 已有进行中的轮次：拒绝第二轮，避免孤儿/永不关闭的 channel 与 double-close。
		r.mu.Unlock()
		return nil, fmt.Errorf("会话 %s 已有进行中的轮次", sessionID)
	}
	r.turns[sessionID] = ch
	r.mu.Unlock()
	_, err := r.call(ctx, "session/prompt", map[string]any{
		"sessionId":     sessionID,
		"contentBlocks": []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		// 仅从 turns 摘除；不 close(ch)——readLoop 是 turn channel 的唯一关闭者。
		// 此处的 ch 从未返回给调用方，无人接收，交给 GC 即可；再 close 会与 readLoop
		// 造成 double-close / send-on-closed panic。
		r.mu.Lock()
		delete(r.turns, sessionID)
		r.mu.Unlock()
		return nil, err
	}
	return ch, nil
}

// Cancel 请求 dsh 优雅中止 sessionID 进行中的一轮：发 session/cancel RPC。
//
// 机制（见接口注释）：dsh 收到后走进程内 agent.cancel({kind:'user'}) → turn/end aborted +
// session.status:idle；readLoop 收到 idle 后 close(turns[sid])，进行中那轮的 drain 循环
// 因 channel 关闭而自然返回。故本方法【只】发一条 RPC，绝不触碰 turns/pending/turnCtx，
// 与事件流并发进行（session/prompt 异步返回，流式期间 RPC 通道空闲，call/readLoop 按 id 配对）。
//
// 未启动即无任何进行中的轮次可中止（stdin 也尚未就绪）→ 直接返回 nil（no-op）。
// dsh 若不认识该 method → call 返回 rpc 错误，原样上抛，由调用方吞掉记日志。
func (r *dshRuntime) Cancel(ctx context.Context, sessionID string) error {
	r.startMu.Lock()
	started := r.started
	r.startMu.Unlock()
	if !started {
		return nil
	}
	_, err := r.call(ctx, "session/cancel", map[string]any{"sessionId": sessionID})
	return err
}

func (r *dshRuntime) Close() error {
	r.startMu.Lock()
	// 幂等：已关闭或从未启动，都置 closed 后直接返回（第二次 Close / Close-before-start 是 no-op）。
	if r.closed || !r.started {
		r.closed = true
		r.startMu.Unlock()
		return nil
	}
	r.closed = true
	cmd := r.cmd
	stdin := r.stdin
	r.startMu.Unlock()
	// best-effort shutdown（忽略错误），有界超时避免子进程「活着但无响应」时 Close 永久挂起。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = r.call(ctx, "shutdown", nil)
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return nil
}

// commonEnv 返回当前进程环境（透传 ARK_API_KEY 等）。
func commonEnv() []string { return os.Environ() }

// stderrLogWriter 把子进程 stderr 导到主进程 stderr（诊断，非协议）。
func stderrLogWriter() io.Writer { return os.Stderr }
