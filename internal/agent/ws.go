package agent

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"zhiwei/internal/ids"
)

// StreamFrame 是 WS 下行的一帧，也是 Orchestrator 流式回调(emit)的载荷。
// 一轮对话按事件顺序推送：user → (assistant | tool_call | tool_result)* → turn_end。
type StreamFrame struct {
	Type    string `json:"type"`               // user|assistant|tool_call|tool_result|turn_end
	MsgID   string `json:"msg_id,omitempty"`   // 落库后的 agent_message id（前端去重/引用用）
	Content string `json:"content,omitempty"`  // user 文本 / assistant 文本 / tool_result 文本
	CallID  string `json:"call_id,omitempty"`  // tool_call / tool_result 关联的调用 id
	Name    string `json:"name,omitempty"`     // tool_call 的工具名
	Args    string `json:"args,omitempty"`     // tool_call 的参数（JSON 字符串）
	IsError bool   `json:"is_error,omitempty"` // tool_result 是否为错误结果
	Error   string `json:"error,omitempty"`    // turn_end 的模型侧错误（空 = 正常结束）
}

// wsUpgrader：单用户本机 MVP，放行任意 Origin（生产部署需按域名校验 Origin 防 CSWSH）。
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// wsConn 给 gorilla 连接包一层写锁：gorilla 不允许并发写。当前所有写都在同一 goroutine，
// 锁是防御性的（便于日后加 ping/pong 等旁路写）。
type wsConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *wsConn) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(v)
}

// inMsg 是 reader goroutine 投递给主循环的一条上行消息：普通文本（起一轮）或停止（中止当前活轮）。
type inMsg struct {
	Text string
	Stop bool
}

// handleWS 处理 GET /api/agent/conversations/{cid}/ws：
// 上行 {"text":"..."}（起一轮）或 {"stop":true}（中止当前活轮），下行按事件顺序推 StreamFrame。
//
// 并发模型（本次为支持「轮次进行中中止」而从串行读循环改造）：
//   - reader goroutine：唯一的 raw.ReadJSON 读者，把上行帧投递到 inCh；断连/协议错→close(inCh) 退出。
//     拆出独立 goroutine 是关键——旧的串行循环要等 RunTurnStream 跑完才回到 ReadJSON，轮次进行中
//     根本读不到 {stop:true}。
//   - turn goroutine：一轮 RunTurnStream 单独跑在一个 goroutine（跑完发 turnDone），使主循环在轮次
//     进行中仍能继续从 inCh 收 stop。
//   - 主循环：select { inCh | turnDone }，用 turnRunning 维持「单连接单活轮」；收到 stop 且有活轮时
//     经 h.Orch.Cancel 发 session/cancel（独立 background+短超时 ctx，【绝不】取消 turnCtx——那会
//     违反 drain 契约、wedge readLoop）。取消后 dsh 优雅 abort→事件流关闭→RunTurnStream 自然收尾。
//
// 并发正确性：raw.ReadJSON 只在 reader、写只经 wsConn.writeJSON（c.mu 串行化，满足 gorilla 单读单写）；
// turnRunning/turnDone 只被主循环触碰；conv 只读。断连时若轮次在跑，不打断落库（drain 契约）——直接
// 返回让 turn goroutine 自然把事件 drain 完（emit 写失败被吞掉），turnDone 是 buffered(1) 故其收尾发送
// 即使无人接收也不阻塞、不泄漏。
func (h *AgentHandler) handleWS(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	cid, err := ids.ParseID(chi.URLParam(r, "cid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cid"})
		return
	}
	// Upgrade 前按当前登录用户取会话（2B-B：越权访问他人会话直接 404；conv.UserID 供 orchestrator
	// 路由到该用户的 dsh 运行时，每轮闭包捕获 conv 即可，无需再单独捕获 uid）。
	conv, err := h.Conversations.Get(r.Context(), uid, cid)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "conversation not found"})
		return
	}
	raw, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade 失败时 gorilla 已写好 HTTP 响应
	}
	defer raw.Close()
	c := &wsConn{conn: raw}

	// reader goroutine：只管读 + 投递；读错（断连/协议错）→ close(inCh) 通知主循环后退出。
	inCh := make(chan inMsg, 8)
	go func() {
		defer close(inCh)
		for {
			var in struct {
				Text string `json:"text"`
				Stop bool   `json:"stop"`
			}
			if err := raw.ReadJSON(&in); err != nil {
				return
			}
			inCh <- inMsg{Text: in.Text, Stop: in.Stop}
		}
	}()

	// 主循环：协调「单连接单活轮」。turnRunning 只被本 goroutine 读写。
	var turnRunning bool
	turnDone := make(chan struct{}, 1) // buffered(1)：turn 收尾发送即使主循环已返回也不阻塞
	// drainDone 机会性回收一个已完成的轮次（把 turnDone 里可能待处理的完成信号先吃掉），避免
	// 「轮次刚结束、turnDone 尚未被 select 处理」时把紧接着的新一轮误判成并发第二轮而拒绝。
	drainDone := func() {
		if turnRunning {
			select {
			case <-turnDone:
				turnRunning = false
			default:
			}
		}
	}
	for {
		select {
		case in, ok := <-inCh:
			if !ok {
				return // 断连：reader 已退出。若有轮次在跑，不打断它（drain 契约），让其自然收尾。
			}
			if in.Stop {
				drainDone()
				if turnRunning {
					// 独立、短超时的 ctx 下发 session/cancel；绝不取消 turnCtx。
					cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := h.Orch.Cancel(cctx, conv); err != nil {
						log.Printf("[ws] conv=%s 取消失败(轮次将靠 idle/超时收尾): %v", cid, err)
					}
					ccancel()
				}
				continue
			}
			if in.Text == "" {
				_ = c.writeJSON(StreamFrame{Type: "turn_end", Error: "text required"})
				continue
			}
			drainDone()
			if turnRunning {
				// 单连接单活轮：已有轮次在跑，拒绝并发第二轮（回一帧错误的 turn_end，前端恢复输入）。
				_ = c.writeJSON(StreamFrame{Type: "turn_end", Error: "已有进行中的一轮，请稍候"})
				continue
			}
			turnRunning = true
			text := in.Text
			// 每轮用独立、脱离请求取消的 context：客户端中途断开不应打断落库；中止靠 dsh 的
			// session/cancel（上面），不靠 turnCtx；5 分钟超时兜底防极端卡死。
			turnCtx, turnCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			go func() {
				defer func() { turnDone <- struct{}{} }()
				defer turnCancel()
				emit := func(f StreamFrame) {
					if err := c.writeJSON(f); err != nil {
						log.Printf("[ws] 写帧失败(继续 drain): %v", err)
					}
				}
				if _, err := h.Orch.RunTurnStream(turnCtx, conv, text, emit); err != nil {
					log.Printf("[ws] conv=%s 轮次错误: %v", cid, err)
					// turn_end 帧（含 Error）已在 runTurn 内推出，这里不重复推送。
				}
			}()
		case <-turnDone:
			turnRunning = false
		}
	}
}
