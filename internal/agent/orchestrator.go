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

// RunTurn 处理一条用户消息（非流式，REST 用）。见 runTurn 的落库与 drain 语义。
func (o *Orchestrator) RunTurn(ctx context.Context, conv *repo.AgentConversation, userText string) (*repo.AgentMessage, error) {
	return o.runTurn(ctx, conv, userText, nil)
}

// RunTurnStream 与 RunTurn 落库语义完全相同，但每成功落库一条消息就通过 emit 推一帧（WS 流式用）。
// emit 必须快速返回、不得 panic、不得阻塞——它跑在消费 runtime 事件的同一 goroutine 上，
// 阻塞会拖住 runtime 的唯一 readLoop（拖垮所有会话）；WS 侧写失败应吞掉并继续（见 ws.go）。
func (o *Orchestrator) RunTurnStream(ctx context.Context, conv *repo.AgentConversation, userText string, emit func(StreamFrame)) (*repo.AgentMessage, error) {
	return o.runTurn(ctx, conv, userText, emit)
}

// runTurn 是一轮对话的核心：落用户消息 → 驱动 runtime → 完整消费（drain）事件 channel
// 直到关闭（满足 AgentRuntime.Prompt 的 drain 契约）→ 每个事件按序各自成行落库（I1：
// dsh 一轮里「先说一句 → 调工具 → 再答复」必须保序，不能拼成一条排到工具行后）→
// 刷新会话活跃时间。emit 非 nil 时，每落一条消息推一帧（含落库后的 msg_id）。
// 返回本轮最后一条落库的 assistant 文本（纯错误轮次可能为 nil）；turn/end 模型错误或
// 落库错误则整轮返回 error（已落库的消息仍保留）。
func (o *Orchestrator) runTurn(ctx context.Context, conv *repo.AgentConversation, userText string, emit func(StreamFrame)) (*repo.AgentMessage, error) {
	send := func(f StreamFrame) {
		if emit != nil {
			emit(f)
		}
	}
	um := &repo.AgentMessage{ConversationID: &conv.ID, Role: "user", Content: userText}
	if err := o.Messages.Append(ctx, um); err != nil {
		return nil, err
	}
	send(StreamFrame{Type: "user", MsgID: um.ID.String(), Content: userText})

	events, err := o.Runtime.Prompt(ctx, conv.DSHSessionID, userText)
	if err != nil {
		return nil, err
	}
	var lastText *repo.AgentMessage // 本轮最后一条 assistant 文本，作为「最终答复」返回
	var turnErr string              // turn/end 携带的模型侧错误（非进程崩溃）
	var appendErr error             // 首个落库错误；记录后仍继续 drain 事件流，轮次结束再上报
	// appendMsg 落一条消息：只记录首个错误；成功后（文本消息更新 lastText 并）推流式帧。
	appendMsg := func(m *repo.AgentMessage, f StreamFrame) {
		if err := o.Messages.Append(ctx, m); err != nil {
			if appendErr == nil {
				appendErr = err
			}
			return
		}
		if m.Kind == "text" {
			lastText = m
		}
		f.MsgID = m.ID.String()
		send(f)
	}
	for ev := range events {
		switch ev.Type {
		case EvAssistantMessage:
			text := ev.AssistantText()
			if text == "" {
				continue // M2：空文本不落库（错误轮次/空消息），避免污染历史
			}
			appendMsg(
				&repo.AgentMessage{ConversationID: &conv.ID, Role: "assistant", Kind: "text", Content: text},
				StreamFrame{Type: "assistant", Content: text},
			)
		case EvToolCall:
			callID, name, args := ev.ToolCall()
			payload, _ := json.Marshal(map[string]any{"call_id": callID, "name": name, "arguments": args})
			raw := json.RawMessage(payload)
			appendMsg(
				&repo.AgentMessage{ConversationID: &conv.ID, Role: "assistant", Kind: "tool_call", Content: name, ToolPayload: &raw},
				StreamFrame{Type: "tool_call", CallID: callID, Name: name, Args: args},
			)
		case EvToolResult:
			text, isErr := ev.ToolResultText()
			payload, _ := json.Marshal(map[string]any{"text": text, "is_error": isErr})
			raw := json.RawMessage(payload)
			appendMsg(
				&repo.AgentMessage{ConversationID: &conv.ID, Role: "assistant", Kind: "tool_result", Content: text, ToolPayload: &raw},
				StreamFrame{Type: "tool_result", Content: text, IsError: isErr},
			)
		case EvTurnEnd:
			if msg := ev.TurnEndErr(); msg != "" {
				turnErr = msg
			}
		}
	}
	_ = o.Conversations.Touch(ctx, conv.ID)
	// 轮次收尾帧：Error 为空表示正常结束（前端据此关闭「正在输入」态）。
	send(StreamFrame{Type: "turn_end", Error: turnErr})
	// 上报优先级：模型轮次错误 > 落库错误；两种情况都带回 lastText（可能为 nil）。
	if turnErr != "" {
		return lastText, fmt.Errorf("agent 轮次错误: %s", turnErr)
	}
	if appendErr != nil {
		return lastText, fmt.Errorf("落库助手消息失败: %w", appendErr)
	}
	return lastText, nil
}
