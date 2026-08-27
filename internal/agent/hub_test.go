package agent

import (
	"encoding/json"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// recvFrame 从通道读一帧，带超时（避免测试因 bug 永久阻塞）。
func recvFrame(t *testing.T, ch <-chan StreamFrame) (StreamFrame, bool) {
	t.Helper()
	select {
	case f, ok := <-ch:
		return f, ok
	case <-time.After(2 * time.Second):
		t.Fatal("等待帧超时")
		return StreamFrame{}, false
	}
}

// TestHubReplayAndFanout 锁定广播器核心：已订阅连接实时收帧；轮次进行中新订阅者(=刷新重连)
// 先拿到本轮已发生帧的重放；turn_end 后 running 归 false。
func TestHubReplayAndFanout(t *testing.T) {
	h := newTurnHub()
	cid := ids.New()

	_, ch1, running := h.subscribe(cid)
	if running {
		t.Fatal("空闲会话订阅不应 running")
	}
	if ch1 == nil {
		t.Fatal("subscribe 应返回持久通道（用于收后续轮次）")
	}
	if !h.startTurn(cid) {
		t.Fatal("空闲会话 startTurn 应成功")
	}
	h.broadcast(cid, StreamFrame{Type: "user", Content: "hi"})
	h.broadcast(cid, StreamFrame{Type: "assistant", Content: "hello"})

	if f, _ := recvFrame(t, ch1); f.Type != "user" {
		t.Fatalf("ch1 首帧应 user, got %q", f.Type)
	}
	if f, _ := recvFrame(t, ch1); f.Type != "assistant" {
		t.Fatalf("ch1 次帧应 assistant, got %q", f.Type)
	}

	// 新订阅者（刷新重连）：应 running 且重放已发生的 2 帧。
	replay, ch2, running2 := h.subscribe(cid)
	if !running2 {
		t.Fatal("轮次进行中，新订阅应 running")
	}
	if len(replay) != 2 || replay[0].Type != "user" || replay[1].Type != "assistant" {
		t.Fatalf("新订阅应重放本轮已发生的 2 帧, got %+v", replay)
	}

	// turn_end：两个订阅者都收到；running 归 false。
	h.broadcast(cid, StreamFrame{Type: "turn_end"})
	if f, _ := recvFrame(t, ch1); f.Type != "turn_end" {
		t.Fatalf("ch1 应收 turn_end, got %q", f.Type)
	}
	if f, _ := recvFrame(t, ch2); f.Type != "turn_end" {
		t.Fatalf("ch2 应收 turn_end, got %q", f.Type)
	}
	if _, _, r := h.subscribe(cid); r {
		t.Fatal("turn_end 后不应 running")
	}
}

// TestHubSingleActiveTurn 锁定单活轮：进行中拒绝第二轮；turn_end 后可再起一轮。
func TestHubSingleActiveTurn(t *testing.T) {
	h := newTurnHub()
	cid := ids.New()
	if !h.startTurn(cid) {
		t.Fatal("首轮应成功")
	}
	if h.startTurn(cid) {
		t.Fatal("已有活轮应拒绝第二轮")
	}
	h.broadcast(cid, StreamFrame{Type: "turn_end"})
	if !h.startTurn(cid) {
		t.Fatal("turn_end 后应可再起一轮")
	}
}

// TestHubUnsubscribeCloses 锁定退订关闭通道，且关闭后广播不 panic（慢/断连订阅者被安全丢弃）。
func TestHubUnsubscribeCloses(t *testing.T) {
	h := newTurnHub()
	cid := ids.New()
	_, ch, _ := h.subscribe(cid)
	h.unsubscribe(cid, ch)
	if _, ok := <-ch; ok {
		t.Fatal("unsubscribe 应关闭通道（读到 ok=false）")
	}
	// 退订后广播不应 panic（该 sub 已移除）。
	h.startTurn(cid)
	h.broadcast(cid, StreamFrame{Type: "user"})
}

// TestHubRunTurnBroadcasts 集成：runTurn 经 Orchestrator+FakeRuntime 跑一轮，订阅者应收到
// user→assistant→turn_end；轮次结束后 running 归 false（可再起新一轮）。
func TestHubRunTurnBroadcasts(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "hub 集成"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "你好"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)

	h := newTurnHub()
	_, ch, _ := h.subscribe(conv.ID)
	if !h.runTurn(orch, conv, "hi") {
		t.Fatal("runTurn 应成功启动")
	}

	// 收帧直到 turn_end。
	var types []string
	for {
		f, ok := recvFrame(t, ch)
		if !ok {
			break
		}
		types = append(types, f.Type)
		if f.Type == "turn_end" {
			break
		}
	}
	var sawUser, sawAssistant, sawEnd bool
	for _, ty := range types {
		switch ty {
		case "user":
			sawUser = true
		case "assistant":
			sawAssistant = true
		case "turn_end":
			sawEnd = true
		}
	}
	if !sawUser || !sawAssistant || !sawEnd {
		t.Fatalf("应收到 user/assistant/turn_end, got %v", types)
	}

	// 结束后可再起一轮（running 已复位）。等待 endTurn 生效。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if h.startTurn(conv.ID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn_end 后 running 未复位，无法再起新一轮")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
