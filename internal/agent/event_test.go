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

// TestAssistantMessageExtractsReasoning 锁定：assistant/message 的 content 里 type=="reasoning" 的块
// 应被 Reasoning() 抽出（doubao thinking 模型在 cordis thinking:enabled 下会返回思考块），
// 且不污染 AssistantText()（后者只取 type=="text"）。多个 reasoning 块按序拼接。
func TestAssistantMessageExtractsReasoning(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "reasoning", "text": "先想一下，"},
		{"type": "reasoning", "text": "再想一下。"},
		{"type": "text", "text": "这是答复。"},
	}}})
	ev := Event{Type: EvAssistantMessage, Data: data}
	if got := ev.Reasoning(); got != "先想一下，再想一下。" {
		t.Errorf("Reasoning()=%q, want 先想一下，再想一下。", got)
	}
	if got := ev.AssistantText(); got != "这是答复。" {
		t.Errorf("AssistantText()=%q, want 这是答复。（reasoning 不应混入）", got)
	}
}

// TestReasoningEmptyWhenNoReasoningBlock 无 reasoning 块时 Reasoning() 返回空串（不误报）。
func TestReasoningEmptyWhenNoReasoningBlock(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "只有答复"},
	}}})
	ev := Event{Type: EvAssistantMessage, Data: data}
	if got := ev.Reasoning(); got != "" {
		t.Errorf("Reasoning()=%q, want 空串", got)
	}
}

// TestChunkDelta 锁定流式增量抽取：assistant/chunk 的 data.chunk.{blockType,text} 被 ChunkDelta()
// 取出——reasoning 块与 text 块各自路由；无 text 的边界 chunk（如块开始/结束）返回空 text 供调用方跳过。
func TestChunkDelta(t *testing.T) {
	// reasoning 增量
	r, _ := json.Marshal(map[string]any{"chunk": map[string]any{"type": "delta", "blockType": "reasoning", "index": 0, "text": "嗯，"}})
	if bt, txt := (Event{Type: EvAssistantChunk, Data: r}).ChunkDelta(); bt != "reasoning" || txt != "嗯，" {
		t.Errorf("reasoning chunk: bt=%q txt=%q, want reasoning/嗯，", bt, txt)
	}
	// text（答复）增量
	x, _ := json.Marshal(map[string]any{"chunk": map[string]any{"type": "delta", "blockType": "text", "index": 1, "text": "你好"}})
	if bt, txt := (Event{Type: EvAssistantChunk, Data: x}).ChunkDelta(); bt != "text" || txt != "你好" {
		t.Errorf("text chunk: bt=%q txt=%q, want text/你好", bt, txt)
	}
	// 无 text 的边界 chunk → 空 text
	b, _ := json.Marshal(map[string]any{"chunk": map[string]any{"type": "block-start", "blockType": "reasoning", "index": 0}})
	if _, txt := (Event{Type: EvAssistantChunk, Data: b}).ChunkDelta(); txt != "" {
		t.Errorf("边界 chunk 应无 text, got %q", txt)
	}
}
