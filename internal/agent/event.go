package agent

import "encoding/json"

// 事件类型常量（对齐 dsh SessionEventMap，spike 抓取）。
const (
	EvAssistantChunk   = "assistant/chunk"
	EvAssistantMessage = "assistant/message"
	EvToolCall         = "tool/call"
	EvToolResult       = "tool/result"
	EvTurnEnd          = "turn/end"
)

// Event 是 dsh session.event 里的一条事件（Data 为其 data 字段原文，按 Type 解码）。
type Event struct {
	Type string          `json:"type"`
	Seq  int             `json:"seq"`
	Data json.RawMessage `json:"data"`
}

// AssistantText 从 assistant/message 事件提取纯文本（拼接 content 里的 text 块）。
func (e Event) AssistantText() string {
	var d struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return ""
	}
	var s string
	for _, b := range d.Message.Content {
		if b.Type == "text" {
			s += b.Text
		}
	}
	return s
}

// Reasoning 从 assistant/message 事件提取模型思考文本（拼接 content 里 type=="reasoning" 的块）。
// doubao thinking 模型在 cordis thinking:enabled + reasoningEffort 下会随答复一并返回 reasoning 块；
// 与 AssistantText 分开抽取，二者互不污染（前者只取 text、本方法只取 reasoning）。空则返回空串。
func (e Event) Reasoning() string {
	var d struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return ""
	}
	var s string
	for _, b := range d.Message.Content {
		if b.Type == "reasoning" {
			s += b.Text
		}
	}
	return s
}

// ChunkDelta 从 assistant/chunk 事件提取流式增量：blockType（reasoning|text 等）+ text 增量。
// dsh 在最终 assistant/message 之前会先流式发一串 chunk（逐 token 增量），供前端「边想边现」。
// 块开始/结束等无文本的边界 chunk 返回空 text，调用方据此跳过。
func (e Event) ChunkDelta() (blockType, text string) {
	var d struct {
		Chunk struct {
			BlockType string `json:"blockType"`
			Text      string `json:"text"`
		} `json:"chunk"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return "", ""
	}
	return d.Chunk.BlockType, d.Chunk.Text
}

// ToolCall 从 tool/call 事件提取 {callId, name, arguments(JSON 字符串)}。
func (e Event) ToolCall() (callID, name, arguments string) {
	var d struct {
		CallID    string `json:"callId"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	_ = json.Unmarshal(e.Data, &d)
	return d.CallID, d.Name, d.Arguments
}

// ToolResult 从 tool/result 事件提取首个 tool-result 的 {toolCallId, 文本, isError}。
// callID 用于把结果精确配对回对应的 tool/call（前端据此定位工具卡，避免 FIFO 顺序在
// 批量/乱序返回时错配）；dsh 的 tool-result 块自带 toolCallId（与 tool/call 的 callId 同值）。
func (e Event) ToolResult() (callID, text string, isError bool) {
	var d struct {
		Message struct {
			Content []struct {
				Type       string `json:"type"`
				ToolCallID string `json:"toolCallId"`
				IsError    bool   `json:"isError"`
				Content    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return "", "", false
	}
	for _, c := range d.Message.Content {
		if c.Type == "tool-result" {
			for _, t := range c.Content {
				if t.Type == "text" {
					text += t.Text
				}
			}
			return c.ToolCallID, text, c.IsError
		}
	}
	return "", "", false
}

// TurnEndErr 若 turn/end.reason.kind=="error" 返回错误信息，否则空串。
func (e Event) TurnEndErr() string {
	var d struct {
		Reason struct {
			Kind  string `json:"kind"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"reason"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return ""
	}
	if d.Reason.Kind == "error" {
		if d.Reason.Error.Message != "" {
			return d.Reason.Error.Message
		}
		return "turn ended with error"
	}
	return ""
}
