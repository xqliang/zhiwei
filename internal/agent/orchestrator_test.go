package agent

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"zhiwei/internal/repo"
)

func orchDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	return dsn
}

func TestOrchestratorRunTurn(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()

	conv := &repo.AgentConversation{Title: "编排测试"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}

	toolResData, _ := json.Marshal(map[string]any{
		"message": map[string]any{"content": []map[string]any{
			{"type": "tool-result", "toolCallId": "c1", "isError": false,
				"content": []map[string]any{{"type": "text", "text": "[{\"title\":\"待办A\"}]"}}},
		}},
	})
	callData, _ := json.Marshal(map[string]any{"callId": "c1", "name": "mcp__zhiwei__get_todos", "arguments": "{}"})
	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "你有 1 条待办：待办A。"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{
		{Type: EvToolCall, Data: callData},
		{Type: EvToolResult, Data: toolResData},
		{Type: EvAssistantMessage, Data: msgData},
	}}}

	orch := NewOrchestrator(fake, convRepo, msgRepo)
	final, err := orch.RunTurn(ctx, conv, "我有哪些待办？")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if final.Content != "你有 1 条待办：待办A。" {
		t.Errorf("最终助手文本异常: %q", final.Content)
	}
	if fake.LastText != "我有哪些待办？" || fake.LastSessionID != conv.DSHSessionID {
		t.Errorf("Prompt 入参异常: text=%q sid=%q", fake.LastText, fake.LastSessionID)
	}

	msgs, err := msgRepo.ListByConversation(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("应落 4 条消息, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[3].Kind != "text" || msgs[3].Content == "" {
		t.Errorf("消息序列异常: %+v", msgs)
	}
	var sawToolCall bool
	for _, m := range msgs {
		if m.Kind == "tool_call" && m.Content == "mcp__zhiwei__get_todos" {
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Error("未落 tool_call 消息")
	}
}

func TestOrchestratorTurnError(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "错误轮次"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	errData, _ := json.Marshal(map[string]any{"reason": map[string]any{
		"kind": "error", "error": map[string]any{"message": "model 404"},
	}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvTurnEnd, Data: errData}}}}
	orch := NewOrchestrator(fake, convRepo, msgRepo)
	_, err = orch.RunTurn(ctx, conv, "hi")
	if err == nil {
		t.Error("turn/end error 应使 RunTurn 返回错误")
	}
}

// TestOrchestratorInterleavedOrder 锁定 I1：一轮里「先说一句 → 调工具 → 再答复」时，
// 两条 assistant 文本应各自成行、并与工具行保持事件顺序落库，最终返回最后一条文本。
// 回归此前的缺陷：把整轮文本拼成一条、循环后才写 → 前言被并进末尾且排到工具行之后。
func TestOrchestratorInterleavedOrder(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "交错顺序"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}

	// 事件脚本：前言文本 → 工具调用 → 工具结果 → 最终答复。
	pre, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "我查一下你的待办。"},
	}}})
	callData, _ := json.Marshal(map[string]any{"callId": "c1", "name": "mcp__zhiwei__get_todos", "arguments": "{}"})
	toolResData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "tool-result", "toolCallId": "c1", "isError": false,
			"content": []map[string]any{{"type": "text", "text": "[{\"title\":\"待办A\"}]"}}},
	}}})
	ans, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "你有 1 条待办：待办A。"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{
		{Type: EvAssistantMessage, Data: pre},
		{Type: EvToolCall, Data: callData},
		{Type: EvToolResult, Data: toolResData},
		{Type: EvAssistantMessage, Data: ans},
	}}}

	orch := NewOrchestrator(fake, convRepo, msgRepo)
	final, err := orch.RunTurn(ctx, conv, "我有哪些待办？")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if final == nil || final.Content != "你有 1 条待办：待办A。" {
		t.Fatalf("最终答复应为最后一条文本, got %+v", final)
	}

	msgs, err := msgRepo.ListByConversation(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 期望顺序：user / text(前言) / tool_call / tool_result / text(答复)
	wantRoles := []string{"user", "assistant", "assistant", "assistant", "assistant"}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("应落 %d 条, got %d: %+v", len(wantRoles), len(msgs), msgs)
	}
	for i := range msgs {
		if msgs[i].Role != wantRoles[i] {
			t.Errorf("msg[%d] role=%q, want %q", i, msgs[i].Role, wantRoles[i])
		}
	}
	// 只校验 assistant 行的 kind（user 行 kind 由 Append 默认为 text，不在本用例关注范围）。
	if msgs[1].Kind != "text" || msgs[2].Kind != "tool_call" || msgs[3].Kind != "tool_result" || msgs[4].Kind != "text" {
		t.Errorf("assistant 行 kind 顺序异常: %q/%q/%q/%q", msgs[1].Kind, msgs[2].Kind, msgs[3].Kind, msgs[4].Kind)
	}
	if msgs[1].Content != "我查一下你的待办。" {
		t.Errorf("前言文本错位/丢失: %q", msgs[1].Content)
	}
	// tool_result 落库 payload 须含 call_id（与 tool_call 的 call_id 一致），供历史重载时按 id 配对。
	var trp struct {
		CallID string `json:"call_id"`
	}
	if msgs[3].ToolPayload != nil {
		_ = json.Unmarshal(*msgs[3].ToolPayload, &trp)
	}
	if trp.CallID != "c1" {
		t.Errorf("tool_result 落库 payload 应含 call_id=c1, got %q", trp.CallID)
	}
}

// TestProfileContextHead 锁定上下文头组装（D2）：有 owner + 关键属性时，头含当天日期 + owner
// 前缀 + 关键属性值；无 Persons / nil 接收者返回 ""（调用方据此不注入）。
func TestProfileContextHead(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	persons := &repo.PersonRepo{DB: db}
	attrs := &repo.PersonAttributeRepo{DB: db}
	ctx := t.Context()
	owner := ensureOwner(t, persons)

	// seed 一条独占 active 属性（occupation 是优先键，必进头）
	a := &repo.PersonAttribute{PersonID: owner.ID, AttrKey: "occupation", ValueText: "上下文头职业CH",
		ValueType: "text", Status: "active", Source: "manual", Confidence: 1}
	if err := attrs.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM person_attribute WHERE id = ?", a.ID.Int64()) })

	pc := &ProfileContext{Persons: persons, Attributes: attrs}
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	head := pc.Head(ctx, now)
	if !strings.Contains(head, "今天是 2026-03-15") {
		t.Errorf("头应含当天日期: %q", head)
	}
	if !strings.Contains(head, "关于用户本人") {
		t.Errorf("头应含 owner 前缀: %q", head)
	}
	if !strings.Contains(head, "上下文头职业CH") {
		t.Errorf("头应含关键属性值: %q", head)
	}

	// 无 Persons / nil 接收者 → 空头（不注入）
	if (&ProfileContext{}).Head(ctx, now) != "" {
		t.Error("无 Persons 应返回空头")
	}
	var np *ProfileContext
	if np.Head(ctx, now) != "" {
		t.Error("nil 接收者应返回空头")
	}
}

// TestOrchestratorContextInjection 锁定 D2：装配 Ctx 后，发给 dsh 的文本被前置 owner 上下文头
// （含关键属性值 + 原始问题），但落库的 user 消息仍是「原始 userText」（不含头）。
func TestOrchestratorContextInjection(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	attrs := &repo.PersonAttributeRepo{DB: db}
	ctx := t.Context()
	owner := ensureOwner(t, persons)

	a := &repo.PersonAttribute{PersonID: owner.ID, AttrKey: "city", ValueText: "注入城市CI",
		ValueType: "text", Status: "active", Source: "manual", Confidence: 1}
	if err := attrs.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM person_attribute WHERE id = ?", a.ID.Int64()) })

	conv := &repo.AgentConversation{Title: "上下文注入"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}

	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "好的"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}

	orch := NewOrchestrator(fake, convRepo, msgRepo)
	orch.Ctx = &ProfileContext{Persons: persons, Attributes: attrs} // 装配上下文头

	const raw = "帮我看看今天的安排"
	if _, err := orch.RunTurn(ctx, conv, raw); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	// 发给 dsh 的文本：带上下文头（含关键属性值）+ 原始问题；且确实被前置（!= 原始）
	if !strings.Contains(fake.LastText, "关于用户本人") || !strings.Contains(fake.LastText, "注入城市CI") {
		t.Errorf("发给 dsh 的文本应含上下文头: %q", fake.LastText)
	}
	if !strings.Contains(fake.LastText, raw) {
		t.Errorf("发给 dsh 的文本应含原始问题: %q", fake.LastText)
	}
	if fake.LastText == raw {
		t.Errorf("发给 dsh 的文本应被前置上下文头(应 != 原始): %q", fake.LastText)
	}

	// 关键：落库的 user 消息 = 原始 userText（不含头），历史保持干净
	msgs, err := msgRepo.ListByConversation(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 || msgs[0].Role != "user" {
		t.Fatalf("首条应为 user 消息: %+v", msgs)
	}
	if msgs[0].Content != raw {
		t.Errorf("落库 user 消息应为原始文本(不含头), got %q", msgs[0].Content)
	}
}
