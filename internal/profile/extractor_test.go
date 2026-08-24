package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/provider"
)

// fakeLLM 按序返回预置响应（每次 Chat 弹出一条）。
type fakeLLM struct{ resps []string }

func (f *fakeLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	if len(f.resps) == 0 {
		return provider.ChatResponse{}, nil
	}
	r := f.resps[0]
	f.resps = f.resps[1:]
	return provider.ChatResponse{Content: r, TotalTokens: 42}, nil
}

var _ provider.LLMProvider = (*fakeLLM)(nil)

func TestExtractorExtract(t *testing.T) {
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "我在互联网公司做后端开发", StartMS: 0, EndMS: 3000,
			SegmentIDs: []ids.ID{101, 102}},
		{SpeakerLabel: "我", Text: "我老婆 Alice 是医生", StartMS: 4000, EndMS: 7000,
			SegmentIDs: []ids.ID{103}},
	}
	resp := `{"facts":[
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"occupation","value":"后端开发工程师",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"relationship","subject":{"kind":"self"},"related":{"kind":"mentioned","name":"Alice"},
		 "relation_type":"配偶","label":"老婆","confidence":0.85,"epistemic_type":"observed","block_index":2}
	]}`
	ex := &Extractor{LLM: &fakeLLM{resps: []string{resp}}, Model: "test-model", Prompt: "sys", Window: 10}
	facts, err := ex.Extract(context.Background(), blocks, []PersonRef{{ID: 1, Name: "我", IsOwner: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("应 2 条: %+v", facts)
	}
	// SegmentIDs 由 block_index 回填（block 1 → segs 101,102）
	if len(facts[0].SegmentIDs) != 2 || facts[0].SegmentIDs[0] != 101 {
		t.Fatalf("fact0 溯源错误: %v", facts[0].SegmentIDs)
	}
	if len(facts[1].SegmentIDs) != 1 || facts[1].SegmentIDs[0] != 103 {
		t.Fatalf("fact1 溯源错误: %v", facts[1].SegmentIDs)
	}
	if ex.Stats().Windows != 1 || ex.Stats().Tokens != 42 {
		t.Fatalf("stats 错误: %+v", ex.Stats())
	}
}

func TestExtractorDedupAcrossWindows(t *testing.T) {
	// 两个窗口（Window=1 强制切两窗），两边输出同一条事实但置信度不同 → 保留高者
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "我喜欢游泳", SegmentIDs: []ids.ID{201}},
		{SpeakerLabel: "我", Text: "我说过我喜欢游泳", SegmentIDs: []ids.ID{202}},
	}
	resp := `{"facts":[{"plane":"attribute","subject":{"kind":"self"},"attr_key":"hobbies","value":"游泳",
		"confidence":0.6,"epistemic_type":"observed","block_index":1}]}`
	resp2 := `{"facts":[{"plane":"attribute","subject":{"kind":"self"},"attr_key":"hobbies","value":"游泳",
		"confidence":0.9,"epistemic_type":"observed","block_index":1}]}`
	ex := &Extractor{LLM: &fakeLLM{resps: []string{resp, resp2}}, Model: "m", Prompt: "s", Window: 1}
	facts, err := ex.Extract(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("跨窗口去重应剩 1 条: %+v", facts)
	}
	if facts[0].Confidence != 0.9 {
		t.Fatalf("应保留高置信: %v", facts[0].Confidence)
	}
	if ex.Stats().Windows != 2 {
		t.Fatalf("应 2 窗口: %d", ex.Stats().Windows)
	}
}
