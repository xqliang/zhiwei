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

// ToolResultText 从 tool/result 事件提取首个 tool-result 的文本与 isError。
func (e Event) ToolResultText() (text string, isError bool) {
	var d struct {
		Message struct {
			Content []struct {
				Type    string `json:"type"`
				IsError bool   `json:"isError"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(e.Data, &d) != nil {
		return "", false
	}
	for _, c := range d.Message.Content {
		if c.Type == "tool-result" {
			for _, t := range c.Content {
				if t.Type == "text" {
					text += t.Text
				}
			}
			return text, c.IsError
		}
	}
	return "", false
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
