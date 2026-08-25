package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
// 返回落库后的最终 assistant 文本消息；turn/end 错误则整轮返回 error（助手消息仍尽量落）。
func (o *Orchestrator) RunTurn(ctx context.Context, conv *repo.AgentConversation, userText string) (*repo.AgentMessage, error) {
	um := &repo.AgentMessage{ConversationID: &conv.ID, Role: "user", Content: userText}
	if err := o.Messages.Append(ctx, um); err != nil {
		return nil, err
	}
	events, err := o.Runtime.Prompt(ctx, conv.DSHSessionID, userText)
	if err != nil {
		return nil, err
	}
	var finalText strings.Builder
	var turnErr string
	for ev := range events {
		switch ev.Type {
		case EvAssistantMessage:
			finalText.WriteString(ev.AssistantText())
		case EvToolCall:
			callID, name, args := ev.ToolCall()
			payload, _ := json.Marshal(map[string]any{"call_id": callID, "name": name, "arguments": args})
			raw := json.RawMessage(payload)
			_ = o.Messages.Append(ctx, &repo.AgentMessage{
				ConversationID: &conv.ID, Role: "assistant", Kind: "tool_call",
				Content: name, ToolPayload: &raw,
			})
		case EvToolResult:
			text, isErr := ev.ToolResultText()
			payload, _ := json.Marshal(map[string]any{"text": text, "is_error": isErr})
			raw := json.RawMessage(payload)
			_ = o.Messages.Append(ctx, &repo.AgentMessage{
				ConversationID: &conv.ID, Role: "assistant", Kind: "tool_result",
				Content: text, ToolPayload: &raw,
			})
		case EvTurnEnd:
			if msg := ev.TurnEndErr(); msg != "" {
				turnErr = msg
			}
		}
	}
	am := &repo.AgentMessage{ConversationID: &conv.ID, Role: "assistant", Kind: "text", Content: finalText.String()}
	if err := o.Messages.Append(ctx, am); err != nil {
		return nil, err
	}
	_ = o.Conversations.Touch(ctx, conv.ID)
	if turnErr != "" {
		return am, fmt.Errorf("agent 轮次错误: %s", turnErr)
	}
	return am, nil
}
