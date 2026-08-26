package agent

import (
	"encoding/json"
	"testing"
)

// TestToolResultExtractsCallID 锁定：tool/result 事件应能提取 dsh 的 toolCallId（供前端按
// call_id 精确配对工具卡，而非 FIFO 顺序配对——顺序在批量/乱序返回时会错配）。
// 纯单测（不依赖 DB），刻意用带连字符的 callId 与 isError=true，兼验文本拼接与错误标志。
func TestToolResultExtractsCallID(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "tool-result", "toolCallId": "call-42", "isError": true,
			"content": []map[string]any{
				{"type": "text", "text": "出错"},
				{"type": "text", "text": "了"},
			}},
	}}})
	ev := Event{Type: EvToolResult, Data: data}
	callID, text, isErr := ev.ToolResult()
	if callID != "call-42" {
		t.Errorf("callID=%q, want call-42", callID)
	}
	if text != "出错了" {
		t.Errorf("text=%q, want 出错了", text)
	}
	if !isErr {
		t.Error("isErr 应为 true")
	}
}
