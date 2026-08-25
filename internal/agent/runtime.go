package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

func (r *dshRuntime) sidecarDir() string { return filepath.Dir(r.cfg.CordisConfig) }

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
		return err
	}
	r.started = true
	return nil
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
	// stdout 关闭（进程退出）：关闭所有未决轮次 channel，避免消费者永久阻塞。
	r.mu.Lock()
	for sid, ch := range r.turns {
		close(ch)
		delete(r.turns, sid)
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
	r.startMu.Lock()
	started := r.started
	cmd := r.cmd
	stdin := r.stdin
	r.startMu.Unlock()
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

// commonEnv 返回当前进程环境（透传 ARK_API_KEY 等）。
func commonEnv() []string { return os.Environ() }

// stderrLogWriter 把子进程 stderr 导到主进程 stderr（诊断，非协议）。
func stderrLogWriter() io.Writer { return os.Stderr }
