package agent

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"zhiwei/internal/repo"
)

// TestRunTurnStreamEmitsInOrder：流式回调按事件顺序推帧，且与落库同源（复用 I1 逐条保序）。
// 期望帧序：user → assistant(前言) → tool_call → tool_result → assistant(答复) → turn_end。
func TestRunTurnStreamEmitsInOrder(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "流式顺序"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}

	pre, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "我查一下。"},
	}}})
	callData, _ := json.Marshal(map[string]any{"callId": "c1", "name": "mcp__zhiwei__get_todos", "arguments": "{}"})
	toolRes, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "tool-result", "toolCallId": "c1", "isError": false,
			"content": []map[string]any{{"type": "text", "text": "[]"}}},
	}}})
	ans, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "你没有待办。"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{
		{Type: EvAssistantMessage, Data: pre},
		{Type: EvToolCall, Data: callData},
		{Type: EvToolResult, Data: toolRes},
		{Type: EvAssistantMessage, Data: ans},
	}}}

	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
	var frames []StreamFrame
	final, err := orch.RunTurnStream(ctx, conv, "有待办吗？", func(f StreamFrame) { frames = append(frames, f) })
	if err != nil {
		t.Fatalf("RunTurnStream: %v", err)
	}
	if final == nil || final.Content != "你没有待办。" {
		t.Fatalf("final 应为最后一条 assistant 文本, got %+v", final)
	}
	wantTypes := []string{"user", "assistant", "tool_call", "tool_result", "assistant", "turn_end"}
	if len(frames) != len(wantTypes) {
		t.Fatalf("帧数=%d, want %d: %+v", len(frames), len(wantTypes), frames)
	}
	for i, wt := range wantTypes {
		if frames[i].Type != wt {
			t.Errorf("frame[%d].Type=%q, want %q", i, frames[i].Type, wt)
		}
	}
	if frames[0].Content != "有待办吗？" || frames[0].MsgID == "" {
		t.Errorf("user 帧异常: %+v", frames[0])
	}
	if frames[1].Content != "我查一下。" || frames[1].MsgID == "" {
		t.Errorf("前言帧异常（错位/丢失/无 msg_id）: %+v", frames[1])
	}
	if frames[2].Name != "mcp__zhiwei__get_todos" || frames[2].CallID != "c1" || frames[2].MsgID == "" {
		t.Errorf("tool_call 帧异常: %+v", frames[2])
	}
	// tool_result 帧须回携同一 call_id（供前端按 id 精确配对工具卡，而非 FIFO）。
	if frames[3].CallID != "c1" || frames[3].Content != "[]" {
		t.Errorf("tool_result 帧异常（应带 call_id=c1）: %+v", frames[3])
	}
	if frames[4].Content != "你没有待办。" {
		t.Errorf("答复帧异常: %+v", frames[4])
	}
	if frames[5].Error != "" {
		t.Errorf("turn_end 应无错误: %+v", frames[5])
	}
}

// TestWSEndToEnd：起 httptest 服务 + FakeRuntime，走真 WS 往返，验证上行发消息、下行按序收帧。
func TestWSEndToEnd(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "WS 测试"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	ans, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "答复内容"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: ans}}}}
	h := &AgentHandler{Orch: NewOrchestrator(rtFor(fake), convRepo, msgRepo), Conversations: convRepo, Messages: msgRepo}
	r := chi.NewRouter()
	r.Use(injectUser(1)) // 模拟 authGate 注入 uid=1
	RegisterAgent(r, h)
	srv := httptest.NewServer(r)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/agent/conversations/" + conv.ID.String() + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]string{"text": "你好"}); err != nil {
		t.Fatal(err)
	}

	var got []StreamFrame
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var f StreamFrame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatalf("读帧: %v (已收 %+v)", err, got)
		}
		got = append(got, f)
		if f.Type == "turn_end" {
			break
		}
	}
	// 期望：user → assistant → turn_end
	if len(got) != 3 || got[0].Type != "user" || got[1].Type != "assistant" || got[2].Type != "turn_end" {
		t.Fatalf("帧序列异常: %+v", got)
	}
	if got[1].Content != "答复内容" {
		t.Errorf("assistant 内容: %q", got[1].Content)
	}
	if got[2].Error != "" {
		t.Errorf("turn_end 错误: %q", got[2].Error)
	}
}

// TestWSStopCancelsRunningTurn 锁定并发 reader（本设计核心回归）：一轮【进行中】发 {stop:true}
// → 服务端并发读到停止帧、调 Orchestrator.Cancel(→FakeRuntime.Cancel) → FakeRuntime 关闭挂起的
// turn channel（模拟 dsh 优雅 abort→idle）→ RunTurnStream 干净收尾 → 客户端收到无 error 的 turn_end。
//
// 为什么这是关键回归：旧的串行读循环要等 RunTurnStream 跑完才回到 ReadJSON。本轮用 Block 模式
// 「挂起」到收到 cancel 才结束——若 reader 仍串行，停止帧永远读不到、轮次永不结束 → 测试超时挂死。
// 故本用例专门证明「轮次进行中也能读到并处理 stop」。
//
// 同步点用 assistant 帧（而非 user 帧）：assistant 帧由编排器 drain 事件时推出，其发生【晚于】
// Prompt 返回（此时 FakeRuntime 已登记挂起 channel），从而保证随后 Cancel 一定能命中该 channel、
// 关闭它使轮次收尾（若用更早的 user 帧同步，Cancel 可能早于 Prompt 登记 → 关不掉 → 挂死）。
func TestWSStopCancelsRunningTurn(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "WS 停止"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}

	// Block=true：Prompt 回放脚本（含一条 assistant 前言）后【不关闭】channel，模拟轮次挂起，
	// 直到 Cancel 才关闭。
	prelude, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "正在处理…"},
	}}})
	fake := &FakeRuntime{Block: true, Script: [][]Event{{{Type: EvAssistantMessage, Data: prelude}}}}
	h := &AgentHandler{Orch: NewOrchestrator(rtFor(fake), convRepo, msgRepo), Conversations: convRepo, Messages: msgRepo}
	r := chi.NewRouter()
	r.Use(injectUser(1)) // 模拟 authGate 注入 uid=1
	RegisterAgent(r, h)
	srv := httptest.NewServer(r)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/agent/conversations/" + conv.ID.String() + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 起一轮。
	if err := conn.WriteJSON(map[string]any{"text": "开始一轮"}); err != nil {
		t.Fatal(err)
	}

	// 读帧：见到 assistant（证明 Prompt 已返回、挂起 channel 已登记）后发 stop；随后读到 turn_end 收尾。
	var end StreamFrame
	sentStop := false
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var f StreamFrame
		if err := conn.ReadJSON(&f); err != nil {
			t.Fatalf("读帧: %v", err)
		}
		if f.Type == "assistant" && !sentStop {
			sentStop = true
			if err := conn.WriteJSON(map[string]any{"stop": true}); err != nil {
				t.Fatalf("发 stop: %v", err)
			}
		}
		if f.Type == "turn_end" {
			end = f
			break
		}
	}
	if !sentStop {
		t.Fatal("未见到 assistant 帧（轮次未按预期进行中挂起）")
	}
	if end.Error != "" {
		t.Errorf("中止后 turn_end 应无 error（aborted 干净收尾）, got %q", end.Error)
	}
	// Cancel 必须精确命中本会话的 dsh session（错发会中止别人的轮次）。
	calls, sid := fake.CancelInfo()
	if calls != 1 {
		t.Errorf("FakeRuntime.Cancel 应被调用 1 次, got %d", calls)
	}
	if sid != conv.DSHSessionID {
		t.Errorf("Cancel 的 sessionID 应为 conv.DSHSessionID=%q, got %q", conv.DSHSessionID, sid)
	}
}
