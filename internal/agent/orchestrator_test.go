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

// rtFor 把单个运行时包成 Orchestrator.RuntimeFor 工厂（测试便捷：任何 userID 都返回同一 rt）。
// 2B-B 起 NewOrchestrator 第一参收 func(userID int64) AgentRuntime（生产 = pool.Get）。
func rtFor(rt AgentRuntime) func(int64) AgentRuntime {
	return func(int64) AgentRuntime { return rt }
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

	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
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

	msgs, err := msgRepo.ListByConversation(ctx, 1, conv.ID)
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
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
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

	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
	final, err := orch.RunTurn(ctx, conv, "我有哪些待办？")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if final == nil || final.Content != "你有 1 条待办：待办A。" {
		t.Fatalf("最终答复应为最后一条文本, got %+v", final)
	}

	msgs, err := msgRepo.ListByConversation(ctx, 1, conv.ID)
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
	head := pc.Head(ctx, toolUserID, now)
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
	if (&ProfileContext{}).Head(ctx, toolUserID, now) != "" {
		t.Error("无 Persons 应返回空头")
	}
	var np *ProfileContext
	if np.Head(ctx, toolUserID, now) != "" {
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

	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
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
	msgs, err := msgRepo.ListByConversation(ctx, 1, conv.ID)
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

// TestOrchestratorRuntimeForByUser 锁定 2B-B：runTurn 按 conv.UserID 经 RuntimeFor 选运行时。
// 两个 conv 属不同用户 → 应被路由到各自的 fake（会话/文本各归其主，绝不串到对方运行时）。
func TestOrchestratorRuntimeForByUser(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()

	convA := &repo.AgentConversation{UserID: 7, Title: "用户7"}
	convB := &repo.AgentConversation{UserID: 9, Title: "用户9"}
	if err := convRepo.Create(ctx, convA); err != nil {
		t.Fatal(err)
	}
	if err := convRepo.Create(ctx, convB); err != nil {
		t.Fatal(err)
	}

	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "好的"},
	}}})
	fakeA := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	fakeB := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	// RuntimeFor：按 userID 分流（7→fakeA，9→fakeB，其余 nil 触发 panic 以暴露路由错误）。
	runtimeFor := func(uid int64) AgentRuntime {
		switch uid {
		case 7:
			return fakeA
		case 9:
			return fakeB
		default:
			t.Errorf("非预期 userID 路由: %d", uid)
			return fakeA
		}
	}
	orch := NewOrchestrator(runtimeFor, convRepo, msgRepo)

	if _, err := orch.RunTurn(ctx, convA, "来自7"); err != nil {
		t.Fatalf("RunTurn A: %v", err)
	}
	if _, err := orch.RunTurn(ctx, convB, "来自9"); err != nil {
		t.Fatalf("RunTurn B: %v", err)
	}

	// 各自的运行时各被调一次，且收到的是「自己那条会话的 sessionID + 文本」。
	if fakeA.calls != 1 || fakeB.calls != 1 {
		t.Fatalf("每个运行时应恰被调一次: A=%d B=%d", fakeA.calls, fakeB.calls)
	}
	if fakeA.LastSessionID != convA.DSHSessionID || fakeA.LastText != "来自7" {
		t.Errorf("fakeA 应收到 conv7 的会话/文本: sid=%q text=%q", fakeA.LastSessionID, fakeA.LastText)
	}
	if fakeB.LastSessionID != convB.DSHSessionID || fakeB.LastText != "来自9" {
		t.Errorf("fakeB 应收到 conv9 的会话/文本: sid=%q text=%q", fakeB.LastSessionID, fakeB.LastText)
	}
	// 交叉核对：绝不能把某会话串到对方运行时。
	if fakeA.LastSessionID == convB.DSHSessionID || fakeB.LastSessionID == convA.DSHSessionID {
		t.Error("会话被错误路由到了对方用户的运行时")
	}
}

// TestProfileContextHeadByUser 锁定 2B-B：ProfileContext.Head 按传入 userID 取 owner。
// owner 用户看到自己的画像头；另一个没有 owner 的用户得到空头（不串用、不泄露他人画像）。
func TestProfileContextHeadByUser(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	persons := &repo.PersonRepo{DB: db}
	attrs := &repo.PersonAttributeRepo{DB: db}
	ctx := t.Context()
	owner := ensureOwner(t, persons) // owner 属 toolUserID(=1)

	a := &repo.PersonAttribute{PersonID: owner.ID, AttrKey: "occupation", ValueText: "隔离头职业IU",
		ValueType: "text", Status: "active", Source: "manual", Confidence: 1}
	if err := attrs.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM person_attribute WHERE id = ?", a.ID.Int64()) })

	pc := &ProfileContext{Persons: persons, Attributes: attrs}
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)

	// owner 用户（toolUserID）：头含其属性值。
	if h := pc.Head(ctx, toolUserID, now); !strings.Contains(h, "隔离头职业IU") {
		t.Errorf("owner 用户的头应含其属性值: %q", h)
	}
	// 另一个无 owner 的用户：空头（不注入、不泄露 owner 画像）。
	const otherUser = int64(424242)
	if h := pc.Head(ctx, otherUser, now); h != "" {
		t.Errorf("无 owner 的用户应得空头(不串用他人画像), got %q", h)
	}
}
