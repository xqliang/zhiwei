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
