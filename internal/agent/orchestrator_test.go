package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"zhiwei/internal/ids"
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
	// 未装配 Ctx/Persona，但日期+时区块无条件注入 → LastText = 日期头 + 原始问题（含原始，且被前置）。
	if !strings.Contains(fake.LastText, "我有哪些待办？") || fake.LastSessionID != conv.DSHSessionID {
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

// TestOrchestratorEmitsReasoning 锁定 Phase 2：assistant/message 带 reasoning 块时，应在答复文本
// 之前先推一帧 type=reasoning 并落库 kind=reasoning；最终答复仍取 text（reasoning 不当最终答复）。
func TestOrchestratorEmitsReasoning(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "思考内容"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}

	// 一条 assistant/message 同时含 reasoning + text 两种块。
	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "reasoning", "text": "用户问待办，我该查一下。"},
		{"type": "text", "text": "你有 1 条待办。"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)

	var frames []StreamFrame
	final, err := orch.RunTurnStream(ctx, conv, "我有哪些待办？", func(f StreamFrame) { frames = append(frames, f) })
	if err != nil {
		t.Fatalf("RunTurnStream: %v", err)
	}
	if final == nil || final.Content != "你有 1 条待办。" || final.Kind != "text" {
		t.Fatalf("最终答复应为 text 块内容, got %+v", final)
	}
	// 帧顺序：user → reasoning → assistant → turn_end；reasoning 必须在 assistant 之前。
	var ri, ai = -1, -1
	for i, f := range frames {
		if f.Type == "reasoning" && ri < 0 {
			ri = i
			if f.Content != "用户问待办，我该查一下。" {
				t.Errorf("reasoning 帧内容异常: %q", f.Content)
			}
		}
		if f.Type == "assistant" && ai < 0 {
			ai = i
		}
	}
	if ri < 0 {
		t.Fatalf("应有一帧 type=reasoning, got %+v", frames)
	}
	if ai < 0 || ri > ai {
		t.Errorf("reasoning 帧应在 assistant 帧之前: ri=%d ai=%d", ri, ai)
	}

	// 落库：user / reasoning / text 三条，且 reasoning 在 text 之前。
	msgs, err := msgRepo.ListByConversation(ctx, 1, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	var reasonIdx, textIdx = -1, -1
	for i, m := range msgs {
		if m.Kind == "reasoning" && reasonIdx < 0 {
			reasonIdx = i
			if m.Content != "用户问待办，我该查一下。" || m.Role != "assistant" {
				t.Errorf("reasoning 落库行异常: %+v", m)
			}
		}
		if m.Kind == "text" && m.Role == "assistant" && textIdx < 0 {
			textIdx = i
		}
	}
	if reasonIdx < 0 {
		t.Fatalf("应落一条 kind=reasoning 消息, got %+v", msgs)
	}
	if textIdx < 0 || reasonIdx > textIdx {
		t.Errorf("reasoning 行应在 assistant text 行之前: reasonIdx=%d textIdx=%d", reasonIdx, textIdx)
	}
}

// TestOrchestratorStreamsChunks 锁定流式：assistant/chunk 事件按 blockType 推 reasoning_delta /
// answer_delta 瞬时帧（不落库），最终 assistant/message 才落权威 reasoning + text 行。
func TestOrchestratorStreamsChunks(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "流式增量"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}

	mk := func(bt, txt string) Event {
		d, _ := json.Marshal(map[string]any{"chunk": map[string]any{"type": "delta", "blockType": bt, "text": txt}})
		return Event{Type: EvAssistantChunk, Data: d}
	}
	msg, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "reasoning", "text": "想想。"},
		{"type": "text", "text": "答复。"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{
		mk("reasoning", "想"), mk("reasoning", "想。"),
		mk("text", "答"), mk("text", "复。"),
		{Type: EvAssistantMessage, Data: msg},
	}}}
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)

	var frames []StreamFrame
	if _, err := orch.RunTurnStream(ctx, conv, "hi", func(f StreamFrame) { frames = append(frames, f) }); err != nil {
		t.Fatalf("RunTurnStream: %v", err)
	}
	// 应有 reasoning_delta / answer_delta 帧，且都在最终 reasoning/assistant 帧之前。
	var rd, ad, finalR, finalA int
	rd, ad, finalR, finalA = 0, 0, -1, -1
	for i, f := range frames {
		switch f.Type {
		case "reasoning_delta":
			rd++
		case "answer_delta":
			ad++
		case "reasoning":
			if finalR < 0 {
				finalR = i
			}
		case "assistant":
			if finalA < 0 {
				finalA = i
			}
		}
	}
	if rd != 2 || ad != 2 {
		t.Errorf("应有 2 reasoning_delta + 2 answer_delta, got rd=%d ad=%d", rd, ad)
	}
	if finalR < 0 || finalA < 0 {
		t.Fatalf("应有最终 reasoning + assistant 帧, got %+v", frames)
	}
	// 落库：只有 user + reasoning + text 三条（delta 不落库）。
	msgs, err := msgRepo.ListByConversation(ctx, 1, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("应落 3 条(user/reasoning/text)，delta 不落库，got %d: %+v", len(msgs), msgs)
	}
}

// TestAssemblePersona 纯函数：identity/soul 拼人设前言；空段跳过；全空返回 ""。
func TestAssemblePersona(t *testing.T) {
	if got := AssemblePersona("  ", "\n"); got != "" {
		t.Errorf("全空应返回空串, got %q", got)
	}
	got := AssemblePersona("我是知微", "温柔简洁")
	if !strings.Contains(got, "我是知微") || !strings.Contains(got, "温柔简洁") {
		t.Errorf("应含 identity+soul: %q", got)
	}
	// 只有 soul：不含身份段标题
	if g := AssemblePersona("", "毒舌"); !strings.Contains(g, "毒舌") || strings.Contains(g, "身份设定") {
		t.Errorf("只配 soul 时不应出现身份段: %q", g)
	}
}

// TestOrchestratorInjectsPersona 锁定人设注入：装配 o.Persona 后，发给 dsh 的文本前置人设前言 +
// 原始问题；落库的 user 消息仍是原始输入（不含人设，历史干净）。
func TestOrchestratorInjectsPersona(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "人设注入"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "好的"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
	orch.Persona = func(context.Context) string { return AssemblePersona("我是知微XP", "简洁XP") }

	const raw = "帮我看看今天安排"
	if _, err := orch.RunTurn(ctx, conv, raw); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !strings.Contains(fake.LastText, "我是知微XP") || !strings.Contains(fake.LastText, "简洁XP") {
		t.Errorf("发给 dsh 的文本应含人设前言: %q", fake.LastText)
	}
	if !strings.Contains(fake.LastText, raw) || fake.LastText == raw {
		t.Errorf("发给 dsh 应为「人设前言+原始问题」: %q", fake.LastText)
	}
	msgs, err := msgRepo.ListByConversation(ctx, 1, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 || msgs[0].Role != "user" || msgs[0].Content != raw {
		t.Errorf("落库 user 消息应为原始文本(不含人设), got %+v", msgs[0])
	}
}

// TestProfileContextHead 锁定上下文头组装（D2）：有 owner + 关键属性时，头含当天日期 + owner
// 前缀 + 关键属性值；无 Persons / nil 接收者返回 ""（调用方据此不注入）。
// TestOrchestratorCancel 锁定 Orchestrator.Cancel：透传到所选运行时的 Cancel，且 sessionID =
// conv.DSHSessionID。无需 DB——Cancel 不落库、不 drain，只下发取消信号。
func TestOrchestratorCancel(t *testing.T) {
	fake := &FakeRuntime{}
	orch := NewOrchestrator(rtFor(fake), nil, nil)
	conv := &repo.AgentConversation{UserID: 5, DSHSessionID: "sess-cancel-xyz"}
	if err := orch.Cancel(t.Context(), conv); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	calls, sid := fake.CancelInfo()
	if calls != 1 {
		t.Errorf("FakeRuntime.Cancel 应被调用 1 次, got %d", calls)
	}
	if sid != "sess-cancel-xyz" {
		t.Errorf("Cancel 的 sessionID 应为 conv.DSHSessionID, got %q", sid)
	}
}

// TestOrchestratorCancelRoutesByUser 锁定 2B-B：Cancel 按 conv.UserID 命中该用户自己的运行时，
// 绝不误伤别的用户的轮次。无需 DB。
func TestOrchestratorCancelRoutesByUser(t *testing.T) {
	fake7 := &FakeRuntime{}
	fake9 := &FakeRuntime{}
	runtimeFor := func(uid int64) AgentRuntime {
		switch uid {
		case 7:
			return fake7
		case 9:
			return fake9
		default:
			t.Errorf("非预期 userID 路由: %d", uid)
			return fake7
		}
	}
	orch := NewOrchestrator(runtimeFor, nil, nil)
	if err := orch.Cancel(t.Context(), &repo.AgentConversation{UserID: 9, DSHSessionID: "sess-9"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if c7, _ := fake7.CancelInfo(); c7 != 0 {
		t.Errorf("用户7 的运行时不应被取消, got %d", c7)
	}
	c9, sid9 := fake9.CancelInfo()
	if c9 != 1 || sid9 != "sess-9" {
		t.Errorf("用户9 的运行时应被取消 1 次且 sid=sess-9, got calls=%d sid=%q", c9, sid9)
	}
}

// TestOrchestratorAbortedTurnEndNotError 锁定 cancel 的收尾语义：turn/end reason.kind=aborted
// （dsh 被 session/cancel 优雅中止时产生）不被判为错误 → RunTurn 干净返回、turn_end 帧无 error。
// 这是整个「停止」功能能干净收尾的前提（区别于 kind=error，见 TestOrchestratorTurnError）。
func TestOrchestratorAbortedTurnEndNotError(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "中止收尾"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	abortedData, _ := json.Marshal(map[string]any{"reason": map[string]any{
		"kind": "aborted", "reason": map[string]any{"kind": "user"},
	}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvTurnEnd, Data: abortedData}}}}
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)
	var frames []StreamFrame
	if _, err := orch.RunTurnStream(ctx, conv, "停下", func(f StreamFrame) { frames = append(frames, f) }); err != nil {
		t.Fatalf("aborted 轮次不应返回错误: %v", err)
	}
	if n := len(frames); n == 0 || frames[n-1].Type != "turn_end" || frames[n-1].Error != "" {
		t.Errorf("末帧应为无 error 的 turn_end, got %+v", frames)
	}
}

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
	// 日期已移出 Head，改由 DateTimeHead 统一注入（见 TestDateTimeHead）；Head 只留 owner 画像。
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

// TestDateTimeHead 锁定「当前日期 + 时区」背景句的格式：日期 + 时区缩写 + UTC 偏移。
// 用 FixedZone 固定时刻，断言可确定（不受运行机器本地时区影响）。
func TestDateTimeHead(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600) // 东八区，缩写 CST，偏移 +08:00
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, loc)
	got := DateTimeHead(now)
	for _, want := range []string{"今天是 2026-03-15", "CST", "UTC+08:00"} {
		if !strings.Contains(got, want) {
			t.Errorf("DateTimeHead 应含 %q: %q", want, got)
		}
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
	// 无条件日期头会前置到 LastText，故用 Contains 校验「路由到对方 + 带上各自原始文本」这一被测点。
	if fakeA.LastSessionID != convA.DSHSessionID || !strings.Contains(fakeA.LastText, "来自7") {
		t.Errorf("fakeA 应收到 conv7 的会话/文本: sid=%q text=%q", fakeA.LastSessionID, fakeA.LastText)
	}
	if fakeB.LastSessionID != convB.DSHSessionID || !strings.Contains(fakeB.LastText, "来自9") {
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

// TestOrchestratorOnTurnComplete 锁定：每轮 runTurn 收尾会调用 OnTurnComplete（若装配）。
func TestOrchestratorOnTurnComplete(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	convRepo := &repo.AgentConversationRepo{DB: db}
	msgRepo := &repo.AgentMessageRepo{DB: db}
	ctx := t.Context()
	conv := &repo.AgentConversation{Title: "t"}
	if err := convRepo.Create(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if full, err := convRepo.Get(ctx, 1, conv.ID); err == nil {
		conv = full
	}

	msgData, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "答复"},
	}}})
	fake := &FakeRuntime{Script: [][]Event{{{Type: EvAssistantMessage, Data: msgData}}}}
	orch := NewOrchestrator(rtFor(fake), convRepo, msgRepo)

	called := 0
	var gotConvID ids.ID
	orch.OnTurnComplete = func(_ context.Context, c *repo.AgentConversation) {
		called++
		gotConvID = c.ID
	}
	if _, err := orch.RunTurn(ctx, conv, "你好"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if called != 1 {
		t.Errorf("OnTurnComplete 应被调用 1 次, got %d", called)
	}
	if gotConvID != conv.ID {
		t.Errorf("回调收到的 convID=%s want %s", gotConvID, conv.ID)
	}
}
