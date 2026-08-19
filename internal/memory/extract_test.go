package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
)

// fakeLLM 按调用次序弹出预置响应
type fakeLLM struct {
	responses []string
	calls     int
	lastUser  string
}

func (f *fakeLLM) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	if f.calls >= len(f.responses) {
		return provider.ChatResponse{}, fmt.Errorf("fakeLLM: 无预置响应（第 %d 次调用）", f.calls+1)
	}
	resp := f.responses[f.calls]
	f.calls++
	f.lastUser = req.User
	return provider.ChatResponse{Content: resp, TotalTokens: 100}, nil
}

func mkBlocks(n int) []Block {
	bs := make([]Block, n)
	for i := range bs {
		bs[i] = Block{SpeakerLabel: "1", Text: fmt.Sprintf("第%d块内容", i+1),
			StartMS: int64(i) * 1000, EndMS: int64(i)*1000 + 900, SegmentIDs: []ids.ID{ids.ID(int64(i + 1))}}
	}
	return bs
}

func TestExtractorSingleWindow(t *testing.T) {
	llm := &fakeLLM{responses: []string{`{"candidates":[
	  {"type":"event","title":"发邮件","content":"明天需要给 Tom 发邮件确认设计稿",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":true,"todo_due":null,"topic_id":null,"suggested_topic_name":null,"block_index":2}
	]}`}}
	ex := &Extractor{LLM: llm, Model: "fake-model", Prompt: "系统指令", Window: 10}
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)

	blocks := []Block{
		{SpeakerLabel: "1", Text: "块一", StartMS: 0, EndMS: 500, SegmentIDs: []ids.ID{11}},
		{SpeakerLabel: "2", Text: "块二", StartMS: 1000, EndMS: 1500, SegmentIDs: []ids.ID{12}},
	}
	cands, err := ex.Extract(ctx(t), blocks, nil, base)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("calls = %d, want 1", llm.calls)
	}
	if len(cands) != 1 {
		t.Fatalf("cands = %d", len(cands))
	}
	c := cands[0]
	if c.BlockIndex != 2 {
		t.Fatalf("block_index = %d", c.BlockIndex)
	}
	// provenance：block_index=2 → 第二块的 segment
	if len(c.SegmentIDs) != 1 || c.SegmentIDs[0] != 12 {
		t.Fatalf("SegmentIDs = %v", c.SegmentIDs)
	}
	// EventAt = base + 块二 start(1000ms)
	if !c.EventAt.Equal(base.Add(time.Second)) {
		t.Fatalf("EventAt = %v", c.EventAt)
	}
	// 用户消息包含块列表与主题占位
	if !strings.Contains(llm.lastUser, "块二") || !strings.Contains(llm.lastUser, "暂无") {
		t.Fatalf("user msg = %s", llm.lastUser)
	}
}

func TestExtractorMultiWindowAndDedup(t *testing.T) {
	// 12 块、窗口 5 → 3 次调用；两个窗口产出同 title+content 的候选 → 去重保留高置信
	same := `{"candidates":[{"type":"fact","title":"学 Rust","content":"用户正在学习 Rust 计划三个月读完一本书",
	  "epistemic_type":"observed","importance":0.7,"confidence":0.75,
	  "is_todo":false,"todo_due":null,"topic_id":null,"suggested_topic_name":null,"block_index":1}]}`
	high := `{"candidates":[{"type":"fact","title":"学 Rust","content":"用户正在学习 Rust 计划三个月读完一本书",
	  "epistemic_type":"observed","importance":0.7,"confidence":0.95,
	  "is_todo":false,"todo_due":null,"topic_id":null,"suggested_topic_name":null,"block_index":2}]}`
	empty := `{"candidates":[]}`
	llm := &fakeLLM{responses: []string{same, high, empty}}

	ex := &Extractor{LLM: llm, Model: "fake-model", Prompt: "sys", Window: 5}
	blocks := mkBlocks(12)
	cands, err := ex.Extract(ctx(t), blocks, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 3 {
		t.Fatalf("calls = %d, want 3", llm.calls)
	}
	if len(cands) != 1 {
		t.Fatalf("去重后 cands = %d, want 1", len(cands))
	}
	if cands[0].Confidence != 0.95 {
		t.Fatalf("应保留高置信版本，got %v", cands[0].Confidence)
	}
	// 高置信版本来自第 2 窗口的 block_index=2 → 全局第 7 块（下标 6）
	if want := blocks[6].SegmentIDs[0]; cands[0].SegmentIDs[0] != want {
		t.Fatalf("SegmentIDs[0] = %v, want %v", cands[0].SegmentIDs[0], want)
	}
}

func TestExtractorInvalidBlockIndex(t *testing.T) {
	// block_index 越界 → 用整个窗口的 segment 并集兜底
	llm := &fakeLLM{responses: []string{`{"candidates":[
	  {"type":"fact","title":"t","content":"足够长的一条内容描述内容",
	   "epistemic_type":"observed","importance":0.5,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topic_id":null,"suggested_topic_name":null,"block_index":99}
	]}`}}
	ex := &Extractor{LLM: llm, Model: "m", Prompt: "sys", Window: 10}
	blocks := []Block{
		{SpeakerLabel: "1", Text: "块一", StartMS: 0, EndMS: 500, SegmentIDs: []ids.ID{1}},
		{SpeakerLabel: "1", Text: "块二", StartMS: 1000, EndMS: 1500, SegmentIDs: []ids.ID{2}},
	}
	cands, err := ex.Extract(ctx(t), blocks, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands[0].SegmentIDs) != 2 {
		t.Fatalf("SegmentIDs = %v, want 2 个", cands[0].SegmentIDs)
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}
