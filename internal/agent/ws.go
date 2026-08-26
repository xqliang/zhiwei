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

// handleWS 处理 GET /api/agent/conversations/{cid}/ws：
// 上行读用户消息 {"text": "..."}，每条跑一轮 RunTurnStream，下行按事件顺序推 StreamFrame。
//
// 断连处理（关键）：客户端断开时下一次 ReadJSON 报错→退出循环。若断在轮次中途，emit 的
// 写失败被吞掉并记录，但 runTurn 仍会把 runtime 事件 channel drain 到关闭（满足单 readLoop
// 契约）后才返回——绝不因断连提前 return 而拖死 runtime。
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

	for {
		var in struct {
			Text string `json:"text"`
		}
		if err := raw.ReadJSON(&in); err != nil {
			return // 断开 / 协议错误：结束会话循环
		}
		if in.Text == "" {
			_ = c.writeJSON(StreamFrame{Type: "turn_end", Error: "text required"})
			continue
		}
		// 每轮用独立、脱离请求取消的 context：客户端中途断开不应打断落库；runtime 无 turn 级
		// cancel，轮次靠 dsh 的 idle 自然收尾，5 分钟超时兜底防极端卡死。
		turnCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		emit := func(f StreamFrame) {
			if err := c.writeJSON(f); err != nil {
				log.Printf("[ws] 写帧失败(继续 drain): %v", err)
			}
		}
		_, err := h.Orch.RunTurnStream(turnCtx, conv, in.Text, emit)
		cancel()
		if err != nil {
			log.Printf("[ws] conv=%s 轮次错误: %v", cid, err)
			// turn_end 帧（含 Error）已在 runTurn 内推出，这里不重复推送。
		}
	}
}
