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
// 一轮对话按事件顺序推送：user → (reasoning | assistant | tool_call | tool_result)* → turn_end。
type StreamFrame struct {
	Type    string `json:"type"`               // user|reasoning|assistant|reasoning_delta|answer_delta|tool_call|tool_result|turn_active|turn_end
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

// handleWS 处理 GET /api/agent/conversations/{cid}/ws：
// 上行 {"text":"..."}（起一轮）或 {"stop":true}（中止当前活轮），下行按事件顺序推 StreamFrame。
//
// 并发模型（P3：轮次广播器）：轮次不再绑定在某个连接的 goroutine 上，而是归 turnHub 拥有并广播——
// 同一会话的多个连接（含刷新后的重连）都订阅同一路广播，故：
//   - 刷新断连后，hub 里在跑的一轮不受影响（继续落库 + 广播），重连的新连接经 subscribe 拿到
//     本轮已发生帧的重放 + 后续实时帧 → 「当前进度」得以恢复续流；
//   - 「停止」可从任意连接生效（reader 直接调 Orchestrator.Cancel，与连接解耦）。
//
// 每连接两段：
//   - writer goroutine：唯一经订阅通道写 WS 者；先补发 replay，再持续写实时帧。写失败不退出，
//     继续 drain 通道至其关闭，避免 broadcast 侧被慢连接阻塞（broadcast 本身也用非阻塞发送兜底）。
//   - reader（本 goroutine）：读上行。{text}→hub.runTurn（单活轮，已有活轮回私有错误帧）；
//     {stop}→Orchestrator.Cancel；断连→return（defer unsubscribe 关闭本连接订阅）。
// 私有反馈（text required / 已有活轮）经 c.writeJSON 回本连接——wsConn.mu 串行化，与 writer 并发写安全。
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
	// 路由到该用户的 dsh 运行时）。
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

	// 订阅本会话广播：replay=本轮已发生帧（重连补齐；空闲会话为空），ch=后续实时帧通道，
	// running=当前是否有进行中的一轮（重连到活轮时，先发一帧 turn_active 让前端恢复「思考中」态）。
	replay, ch, running := h.Hub.subscribe(cid)
	defer h.Hub.unsubscribe(cid, ch)
	go func() {
		if running {
			_ = c.writeJSON(StreamFrame{Type: "turn_active"}) // 瞬时控制帧：本会话有轮次进行中
		}
		for _, f := range replay {
			_ = c.writeJSON(f)
		}
		for f := range ch { // ch 被 unsubscribe / 慢订阅丢弃时关闭 → range 结束、writer 退出
			_ = c.writeJSON(f)
		}
	}()

	for {
		var in struct {
			Text string `json:"text"`
			Stop bool   `json:"stop"`
		}
		if err := raw.ReadJSON(&in); err != nil {
			return // 断连/协议错：defer unsubscribe 关闭订阅；hub 里在跑的轮次继续（落库+广播），不打断
		}
		if in.Stop {
			// 独立、短超时的 ctx 下发 session/cancel；绝不取消轮次自身的 ctx（那会违反 dsh drain 契约）。
			cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := h.Orch.Cancel(cctx, conv); err != nil {
				log.Printf("[ws] conv=%s 取消失败(轮次将靠 idle/超时收尾): %v", cid, err)
			}
			ccancel()
			continue
		}
		if in.Text == "" {
			_ = c.writeJSON(StreamFrame{Type: "turn_end", Error: "text required"})
			continue
		}
		if !h.Hub.runTurn(h.Orch, conv, in.Text) {
			// 单活轮：已有轮次在跑，回一帧私有错误 turn_end（仅本连接），前端据此恢复输入。
			_ = c.writeJSON(StreamFrame{Type: "turn_end", Error: "已有进行中的一轮，请稍候"})
		}
	}
}
