package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"zhiwei/internal/repo"
)

// Orchestrator 跑一轮对话：落用户消息 → 驱动 runtime → 消费事件流（拼助手文本、
// 记工具活动）→ 落助手消息 → 刷新会话活跃时间 → 返回落库后的助手消息。
type Orchestrator struct {
	Runtime       AgentRuntime
	Conversations *repo.AgentConversationRepo
	Messages      *repo.AgentMessageRepo
}

func NewOrchestrator(rt AgentRuntime, conv *repo.AgentConversationRepo, msg *repo.AgentMessageRepo) *Orchestrator {
	return &Orchestrator{Runtime: rt, Conversations: conv, Messages: msg}
}

// RunTurn 处理一条用户消息（conv 已存在，由 handler 创建/校验）。完整消费（drain）
// runtime 返回的事件 channel 直到关闭——满足 AgentRuntime.Prompt 的 drain 契约。
// 返回本轮最后一条落库的 assistant 文本消息（纯错误轮次可能为 nil）；turn/end 模型错误
// 或落库错误则整轮返回 error（已成功落库的消息仍保留）。
func (o *Orchestrator) RunTurn(ctx context.Context, conv *repo.AgentConversation, userText string) (*repo.AgentMessage, error) {
	um := &repo.AgentMessage{ConversationID: &conv.ID, Role: "user", Content: userText}
	if err := o.Messages.Append(ctx, um); err != nil {
		return nil, err
	}
	events, err := o.Runtime.Prompt(ctx, conv.DSHSessionID, userText)
	if err != nil {
		return nil, err
	}
	// 逐条落库，保留 assistant 文本与工具活动的交错顺序（I1）：dsh 一轮里可能出现
	// 「先说一句 → 调工具 → 再答复」，若把所有文本拼成一条、循环后才落库，会被并进
	// 末尾且排到工具行之后——重载历史时顺序就错了。故每个事件各自成行、按序落库。
	var lastText *repo.AgentMessage // 本轮最后一条 assistant 文本，作为「最终答复」返回给 HTTP
	var turnErr string              // turn/end 携带的模型侧错误（非进程崩溃）
	var appendErr error             // 首个落库错误；记录后仍继续 drain 事件流（满足 Prompt 契约），轮次结束再上报
	// appendMsg 落一条消息：只记录首个错误；成功落库的文本消息更新 lastText。
	appendMsg := func(m *repo.AgentMessage) {
		if err := o.Messages.Append(ctx, m); err != nil {
			if appendErr == nil {
				appendErr = err
			}
			return
		}
		if m.Kind == "text" {
			lastText = m
		}
	}
	for ev := range events {
		switch ev.Type {
		case EvAssistantMessage:
			text := ev.AssistantText()
			if text == "" {
				continue // M2：空文本不落库（错误轮次/空消息），避免污染历史
			}
			appendMsg(&repo.AgentMessage{
				ConversationID: &conv.ID, Role: "assistant", Kind: "text", Content: text,
			})
		case EvToolCall:
			callID, name, args := ev.ToolCall()
			payload, _ := json.Marshal(map[string]any{"call_id": callID, "name": name, "arguments": args})
			raw := json.RawMessage(payload)
			appendMsg(&repo.AgentMessage{
				ConversationID: &conv.ID, Role: "assistant", Kind: "tool_call",
				Content: name, ToolPayload: &raw,
			})
		case EvToolResult:
			text, isErr := ev.ToolResultText()
			payload, _ := json.Marshal(map[string]any{"text": text, "is_error": isErr})
			raw := json.RawMessage(payload)
			appendMsg(&repo.AgentMessage{
				ConversationID: &conv.ID, Role: "assistant", Kind: "tool_result",
				Content: text, ToolPayload: &raw,
			})
		case EvTurnEnd:
			if msg := ev.TurnEndErr(); msg != "" {
				turnErr = msg
			}
		}
	}
	_ = o.Conversations.Touch(ctx, conv.ID)
	// 上报优先级：模型轮次错误 > 落库错误；两种情况都带回 lastText（可能为 nil）。
	if turnErr != "" {
		return lastText, fmt.Errorf("agent 轮次错误: %s", turnErr)
	}
	if appendErr != nil {
		return lastText, fmt.Errorf("落库助手消息失败: %w", appendErr)
	}
	return lastText, nil
}
