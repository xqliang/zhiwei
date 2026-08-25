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

	orch := NewOrchestrator(fake, convRepo, msgRepo)
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
	h := &AgentHandler{Orch: NewOrchestrator(fake, convRepo, msgRepo), Conversations: convRepo, Messages: msgRepo}
	r := chi.NewRouter()
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
