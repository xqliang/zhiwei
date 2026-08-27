package agent

import (
	"context"
	"log"
	"sync"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// turnHub 是「每会话轮次广播器」：让同一会话的多个 WS 连接（含刷新后的重连）都能看到进行中一轮
// 的实时流，并支持断点重放——新订阅者先拿到本轮已发生的帧快照，再持续收后续实时帧。
//
// 为什么需要它（P3）：旧实现把一轮跑在某个 WS 连接自己的 goroutine 里、emit 直写该连接。刷新页面
// 断连后，服务端那一轮仍在跑（每步照常落库），但事件不会推给刷新后的新连接——「当前进度」丢失。
// hub 把轮次与具体连接解耦：轮次归 hub 拥有并广播，连接只是订阅者，来去自如。
//
// 并发模型：所有状态都在 h.mu 下访问。broadcast 对订阅通道用**非阻塞发送**（select default）——
// 慢/断连订阅者会被丢弃并关闭其通道，让其靠重连重放追上，绝不阻塞轮次或其他订阅者。订阅通道
// 有较大缓冲；关闭只在持锁时发生一次（unsubscribe 或 broadcast 丢弃），故与发送不会竞态、无双关。
type turnHub struct {
	mu    sync.Mutex
	convs map[ids.ID]*hubConv
}

// hubConv 一个会话的广播状态：持久订阅者 + 当前/最近一轮的帧缓冲（重放用）+ 是否有活轮。
type hubConv struct {
	subs    map[chan StreamFrame]struct{}
	buffer  []StreamFrame // 当前轮的帧（startTurn 时清空）；轮次结束后保留至下一轮开始，供刚重连者重放
	running bool
}

func newTurnHub() *turnHub { return &turnHub{convs: map[ids.ID]*hubConv{}} }

// getOrInit 取（或建）某会话的广播状态。调用方须持有 h.mu。
func (h *turnHub) getOrInit(cid ids.ID) *hubConv {
	c := h.convs[cid]
	if c == nil {
		c = &hubConv{subs: map[chan StreamFrame]struct{}{}}
		h.convs[cid] = c
	}
	return c
}

// subscribe 订阅某会话：返回 (replay, ch, running)。replay=订阅时刻本轮已发生的帧快照（重连补齐用）；
// ch=持久接收通道（收后续所有轮次的帧，直到 unsubscribe 关闭它）；running=当前是否有进行中的一轮。
func (h *turnHub) subscribe(cid ids.ID) (replay []StreamFrame, ch chan StreamFrame, running bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.getOrInit(cid)
	if c.running {
		// 仅在有活轮时回放本轮已发生的帧（重连补齐）；空闲会话的历史已在 DB，无需回放、免去重噪音。
		replay = append([]StreamFrame(nil), c.buffer...)
	}
	ch = make(chan StreamFrame, 256)
	c.subs[ch] = struct{}{}
	return replay, ch, c.running
}

// unsubscribe 退订：从订阅集移除并关闭通道（幂等——已移除则 no-op，不会双关）。
func (h *turnHub) unsubscribe(cid ids.ID, ch chan StreamFrame) {
	if ch == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.convs[cid]; c != nil {
		if _, ok := c.subs[ch]; ok {
			delete(c.subs, ch)
			close(ch)
		}
	}
}

// startTurn 尝试起一轮：已有活轮返回 false（单活轮）；否则置 running、清空重放缓冲（历史在 DB）并返回 true。
func (h *turnHub) startTurn(cid ids.ID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.getOrInit(cid)
	if c.running {
		return false
	}
	c.running = true
	c.buffer = c.buffer[:0]
	return true
}

// broadcast 记录并广播一帧：追加到重放缓冲，非阻塞扇出给所有订阅者（满/断连者丢弃并关闭）。
// turn_end 帧把 running 归 false（轮次结束）。
func (h *turnHub) broadcast(cid ids.ID, f StreamFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.convs[cid]
	if c == nil {
		return
	}
	c.buffer = append(c.buffer, f)
	if f.Type == "turn_end" {
		c.running = false
	}
	for ch := range c.subs {
		select {
		case ch <- f:
		default:
			// 慢/断连订阅者：丢弃并关闭，让其靠重连（subscribe 的 replay）追上，绝不阻塞广播。
			delete(c.subs, ch)
			close(ch)
		}
	}
}

// endTurn 收尾兜底：若轮次仍标记 running 且最后一帧不是 turn_end（如 Prompt 建流失败提前返回、
// 从未 emit turn_end），补广播一帧 turn_end，避免会话「卡在进行中」永远无法再起新一轮。
func (h *turnHub) endTurn(cid ids.ID, errMsg string) {
	h.mu.Lock()
	c := h.convs[cid]
	need := c != nil && c.running && !(len(c.buffer) > 0 && c.buffer[len(c.buffer)-1].Type == "turn_end")
	h.mu.Unlock()
	if need {
		h.broadcast(cid, StreamFrame{Type: "turn_end", Error: errMsg})
	}
}

// runTurn 起一轮并在独立 goroutine 跑 orch.RunTurnStream，emit 走 broadcast。已有活轮返回 false。
// 轮次用脱离请求的独立 ctx（客户端断连不打断落库；5 分钟兜底防卡死），与旧 ws 实现一致；
// 中止仍靠 Orchestrator.Cancel（dsh 优雅 abort→turn/end→broadcast 收尾）。
func (h *turnHub) runTurn(orch *Orchestrator, conv *repo.AgentConversation, text string) bool {
	if !h.startTurn(conv.ID) {
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var errMsg string
		if _, err := orch.RunTurnStream(ctx, conv, text, func(f StreamFrame) { h.broadcast(conv.ID, f) }); err != nil {
			log.Printf("[hub] conv=%s 轮次错误: %v", conv.ID, err)
			errMsg = err.Error()
		}
		h.endTurn(conv.ID, errMsg) // turn/end 通常已由 RunTurnStream 广播；此处仅兜底未收尾的情况
	}()
	return true
}
