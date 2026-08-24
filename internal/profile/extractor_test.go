package profile

import (
	"context"
	"strings"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/provider"
)

// fakeLLM 按序返回预置响应（每次 Chat 弹出一条），并记录最近一次请求的
// User 内容（lastUser）供 prompt 组装断言，参照 internal/memory/extract_test.go。
type fakeLLM struct {
	resps    []string
	lastUser string
}

func (f *fakeLLM) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	f.lastUser = req.User
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

// TestExtractorRelationSubjectNoCollapse 是 factKey 修复的回归测试：同一窗口内产出
// 「我老婆是老师 / 我妈是老师」两条 subject=relation 事实——身份仅靠 Subject.Relation
// 区分（配偶 vs 父母），Name 均为空。修复前去重键漏 Relation 会塌缩成 1 条；
// 修复后两条判别键不同，应保留 2 条（对齐下游 DB 自然键：解析后是两个不同 person）。
func TestExtractorRelationSubjectNoCollapse(t *testing.T) {
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "我老婆是老师，我妈也是老师", SegmentIDs: []ids.ID{301}},
	}
	resp := `{"facts":[
		{"plane":"attribute","subject":{"kind":"relation","relation":"配偶"},"attr_key":"occupation","value":"老师",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"attribute","subject":{"kind":"relation","relation":"父母"},"attr_key":"occupation","value":"老师",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1}
	]}`
	ex := &Extractor{LLM: &fakeLLM{resps: []string{resp}}, Model: "m", Prompt: "s", Window: 10}
	facts, err := ex.Extract(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("relation 主体不同(配偶/父母)不应塌缩，应 2 条: %+v", facts)
	}
	// 断言两条主体 relation 确实分别是 配偶 / 父母（顺序保持输入序）
	if facts[0].Subject.Relation != "配偶" || facts[1].Subject.Relation != "父母" {
		t.Fatalf("主体 relation 错误: %q / %q", facts[0].Subject.Relation, facts[1].Subject.Relation)
	}
}

// TestExtractorInvalidBlockIndex 覆盖 factProvenance 越界兜底：block_index=0 或 >len
// 时用整个窗口的 segment 并集回填（对照 memory 包同名用例）。
func TestExtractorInvalidBlockIndex(t *testing.T) {
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "块一", SegmentIDs: []ids.ID{1}},
		{SpeakerLabel: "我", Text: "块二", SegmentIDs: []ids.ID{2}},
	}
	// 两条不同内容（attr_key 不同）避免被去重；分别测 0 与超范围两个越界分支。
	resp := `{"facts":[
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"occupation","value":"a",
		 "confidence":0.9,"epistemic_type":"observed","block_index":0},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"hobbies","value":"b",
		 "confidence":0.9,"epistemic_type":"observed","block_index":99}
	]}`
	ex := &Extractor{LLM: &fakeLLM{resps: []string{resp}}, Model: "m", Prompt: "s", Window: 10}
	facts, err := ex.Extract(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("应 2 条: %+v", facts)
	}
	for i, f := range facts {
		if len(f.SegmentIDs) != 2 || f.SegmentIDs[0] != 1 || f.SegmentIDs[1] != 2 {
			t.Fatalf("fact%d 越界应回填整窗并集 {1,2}: %v", i, f.SegmentIDs)
		}
	}
}

// TestExtractorUserMessage 断言 prompt 组装：捕获 ChatRequest.User，校验含对话块行
// 与已知人物名单行（person_id|名字|（用户本人…）标注）。参照 memory 的 lastUser 模式。
func TestExtractorUserMessage(t *testing.T) {
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "我在互联网公司做后端开发", SegmentIDs: []ids.ID{401}},
	}
	llm := &fakeLLM{resps: []string{`{"facts":[]}`}}
	ex := &Extractor{LLM: llm, Model: "m", Prompt: "s", Window: 10}
	if _, err := ex.Extract(context.Background(), blocks, []PersonRef{{ID: 7, Name: "张三", IsOwner: true}}); err != nil {
		t.Fatal(err)
	}
	// 对话块行：表头 + 块文本
	if !strings.Contains(llm.lastUser, "对话块列表") || !strings.Contains(llm.lastUser, "我在互联网公司做后端开发") {
		t.Fatalf("user msg 缺对话块: %s", llm.lastUser)
	}
	// 人物名单行：表头 + person_id|名字 + 本人标注
	if !strings.Contains(llm.lastUser, "已知人物列表") ||
		!strings.Contains(llm.lastUser, "7|张三|") ||
		!strings.Contains(llm.lastUser, "用户本人") {
		t.Fatalf("user msg 缺人物名单/本人标注: %s", llm.lastUser)
	}
}

// TestExtractorStatsResetPerCall 断言 Stats 反映「最近一次」调用：连跑两次 Extract，
// 第二次的窗口数/token 不应累加第一次的量（对照 memory 包同名用例）。
func TestExtractorStatsResetPerCall(t *testing.T) {
	// Window=1 → 每块一窗；三条空响应够两次调用（2 窗 + 1 窗）。
	llm := &fakeLLM{resps: []string{`{"facts":[]}`, `{"facts":[]}`, `{"facts":[]}`}}
	ex := &Extractor{LLM: llm, Model: "m", Prompt: "s", Window: 1}
	twoBlocks := []memory.Block{
		{SpeakerLabel: "我", Text: "块一", SegmentIDs: []ids.ID{1}},
		{SpeakerLabel: "我", Text: "块二", SegmentIDs: []ids.ID{2}},
	}
	if _, err := ex.Extract(context.Background(), twoBlocks, nil); err != nil {
		t.Fatal(err)
	}
	if st := ex.Stats(); st.Windows != 2 || st.Tokens != 84 {
		t.Fatalf("第一次 Stats = %+v, want {Windows:2 Tokens:84}", st)
	}
	oneBlock := []memory.Block{{SpeakerLabel: "我", Text: "块三", SegmentIDs: []ids.ID{3}}}
	if _, err := ex.Extract(context.Background(), oneBlock, nil); err != nil {
		t.Fatal(err)
	}
	if st := ex.Stats(); st.Windows != 1 || st.Tokens != 42 {
		t.Fatalf("第二次 Stats = %+v, want 重置后 {Windows:1 Tokens:42}", st)
	}
}
