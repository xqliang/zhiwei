package agent

import (
	"context"
	"sync"
)

// FakeRuntime 是测试用运行时：每次 Prompt 回放一组预设事件后关闭 channel。
type FakeRuntime struct {
	// Script 按调用顺序返回事件序列；用尽后返回最后一组（或空）。
	Script [][]Event
	// LastPrompt 记录最近一次 Prompt 的入参，供断言。
	LastSessionID string
	LastText      string
	Err           error // 非 nil 时 Prompt 返回该错误
	// Cfg 记录构造本运行时的配置（RuntimePool 的 makeRT 注入）；pool_test 据此断言每用户
	// MCPURL/SessionRoot 的派生。普通编排测试用 &FakeRuntime{...} 直接构造，Cfg 留零值即可。
	Cfg RuntimeConfig
	// Closed 记录 Close 被调用的次数；pool_test 的 LRU 回收断言用（被淘汰运行时应恰 Close 一次）。
	Closed int
	calls  int

	// Block 为 true 时 Prompt 回放脚本事件后【不关闭】channel，模拟一轮「挂起」（dsh 轮次进行中），
	// 直到 Cancel 被调用才关闭该 channel（模拟 dsh abort→session.status:idle→readLoop close）。
	// 供 ws 并发 reader 回归测试：轮次进行中发 {stop:true} → 断言 Cancel 被调用、且轮次干净收尾。
	Block bool

	// mu 保护下列在多 goroutine 间并发访问的字段（-race 安全）：Block 模式下 Prompt（turn goroutine）
	// 登记 openTurns，而 Cancel（ws 主循环 goroutine）读取并关闭之；cancels/lastCancelSID 也在
	// Cancel 里写、测试经 CancelInfo() 读。既有 calls/LastSessionID 等字段仅在无并发路径使用，保持原样。
	mu            sync.Mutex
	cancels       int                   // Cancel 被调用的次数
	lastCancelSID string                // 最近一次 Cancel 的 sessionID
	openTurns     map[string]chan Event // Block 模式下未关闭的 turn channel：sessionID -> ch
}

// Warm 测试实现：no-op（无子进程可 spawn）。
func (f *FakeRuntime) Warm(_ context.Context) error { return nil }

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
	if f.Block {
		// 挂起模式：不关闭 channel，登记待 Cancel 关闭（模拟一轮真正「进行中」）。
		f.mu.Lock()
		if f.openTurns == nil {
			f.openTurns = map[string]chan Event{}
		}
		f.openTurns[sessionID] = ch
		f.mu.Unlock()
		return ch, nil
	}
	close(ch)
	return ch, nil
}

// Cancel 记录被取消的 sessionID；Block 模式下关闭对应的挂起 turn channel（模拟 dsh 优雅 abort
// → readLoop close(turns[sid])），使消费方的 drain 循环自然结束、轮次干净收尾（不注入 error）。
// 对没有挂起轮次的 sessionID 只记录、不做别的（幂等，模拟「dsh 无活轮时的无害取消」）。
func (f *FakeRuntime) Cancel(_ context.Context, sessionID string) error {
	f.mu.Lock()
	f.cancels++
	f.lastCancelSID = sessionID
	ch := f.openTurns[sessionID]
	delete(f.openTurns, sessionID)
	f.mu.Unlock()
	if ch != nil {
		close(ch)
	}
	return nil
}

// CancelInfo 原子读取 Cancel 的调用次数与最近一次的 sessionID（-race 安全，供并发测试断言）。
func (f *FakeRuntime) CancelInfo() (calls int, lastSessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels, f.lastCancelSID
}

func (f *FakeRuntime) Close() error { f.Closed++; return nil }
