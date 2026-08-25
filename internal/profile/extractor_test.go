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

// TestExtractorEventNoCollapse 是 event 平面 factKey 判别的回归测试（对齐
// TestExtractorRelationSubjectNoCollapse 的先例）：同一窗口内产出多条 self event
// 事实——主体全是 self、attr/relation 字段全空，仅靠 event_type/title 区分。
// 修复前去重键漏 event 字段会把所有 self event 塌缩成 1 条；修复后判别键不同，
// 应逐条保留（对齐下游 DB：每条 event 是独立一行）。
func TestExtractorEventNoCollapse(t *testing.T) {
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "上个月去云南旅游，还参加了同学会，之前也去过西藏", SegmentIDs: []ids.ID{501}},
	}
	// 三条 self event，主体/时间等其余字段相同，仅 event_type/title 不同：
	//   旅行·去云南 vs 聚会·同学会（类型+标题都不同）
	//   旅行·去云南 vs 旅行·去西藏（同类型、不同标题——单独盯住 title 判别位）
	resp := `{"facts":[
		{"plane":"event","subject":{"kind":"self"},"event_type":"旅行","title":"去云南",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"event","subject":{"kind":"self"},"event_type":"聚会","title":"同学会",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"event","subject":{"kind":"self"},"event_type":"旅行","title":"去西藏",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1}
	]}`
	ex := &Extractor{LLM: &fakeLLM{resps: []string{resp}}, Model: "m", Prompt: "s", Window: 10}
	facts, err := ex.Extract(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Fatalf("event 判别(event_type/title)不同不应塌缩，应 3 条: %+v", facts)
	}
	// 顺序保持输入序，逐条核对判别字段
	if facts[0].EventType != "旅行" || facts[0].EventTitle != "去云南" {
		t.Fatalf("event0 错误: %+v", facts[0])
	}
	if facts[1].EventType != "聚会" || facts[1].EventTitle != "同学会" {
		t.Fatalf("event1 错误: %+v", facts[1])
	}
	// 同类型(旅行)不同 title 也保留——证明 title 参与判别键
	if facts[2].EventType != "旅行" || facts[2].EventTitle != "去西藏" {
		t.Fatalf("event2(同类型不同标题)错误: %+v", facts[2])
	}
}

// TestExtractorMetricNoCollapse 是 metric 平面 factKey 判别的回归测试（对齐
// TestExtractorEventNoCollapse 先例）：同一窗口内多条 self metric——主体全是 self、
// attr/relation/event 字段全空，仅靠 metric_key/metric_value/measured_at 区分。
// metric 是测点流（同键多采样各自成行），三段判别缺一都会塌缩掉独立测点。
func TestExtractorMetricNoCollapse(t *testing.T) {
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "最近有点焦虑，今天称了 72.5 公斤，昨天 73", SegmentIDs: []ids.ID{601}},
	}
	// 四条 self metric，逐位盯住 metric_key / metric_value / measured_at 三个判别位：
	//   emotion·焦虑 vs weight·72.5      —— metric_key 不同
	//   weight·72.5  vs weight·73.0      —— 同 key 不同 value（钉 metric_value 判别位）
	//   weight·72.5·"" vs weight·72.5·8-20 —— 同 key 同 value 不同 measured_at（钉时间判别位：同值两次采样不塌缩）
	resp := `{"facts":[
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"emotion","metric_value":"焦虑",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"weight","metric_value":"72.5",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"weight","metric_value":"73.0",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"metric","subject":{"kind":"self"},"metric_key":"weight","metric_value":"72.5",
		 "measured_at":"2026-08-20","confidence":0.9,"epistemic_type":"observed","block_index":1}
	]}`
	ex := &Extractor{LLM: &fakeLLM{resps: []string{resp}}, Model: "m", Prompt: "s", Window: 10}
	facts, err := ex.Extract(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 4 {
		t.Fatalf("metric 判别(key/value/measured_at)不同不应塌缩，应 4 条: %+v", facts)
	}
	if facts[0].MetricKey != "emotion" || facts[0].MetricValue != "焦虑" {
		t.Fatalf("metric0 错误: %+v", facts[0])
	}
	// 同 key 不同 value 两条并存——证明 metric_value 参与判别键
	if facts[1].MetricValue != "72.5" || facts[2].MetricValue != "73.0" {
		t.Fatalf("同 key 不同 value 应各留: %q / %q", facts[1].MetricValue, facts[2].MetricValue)
	}
	// 同 key 同 value 不同 measured_at 两条并存——证明 measured_at 参与判别键（测点流：同值两次采样不塌缩）
	if facts[3].MetricKey != "weight" || facts[3].MetricValue != "72.5" || facts[3].MeasuredAt != "2026-08-20" {
		t.Fatalf("metric3(同值不同时刻)错误: %+v", facts[3])
	}
}

// TestExtractorCycleNoCollapse 是 cycle 平面 factKey 判别的回归测试，兼作「AnchorDate
// 不入键」修复的护栏。cycle 键须**精确镜像 DB 自然键 (session,person,cycle_type,label)**——
// 只含 CycleType+CycleLabel、**不含 AnchorDate**：否则同 (type,label) 不同 anchor 的两条
// 在此不塌缩，却在 Service 单事务里被 DB 自然键 dedup 成「先到的赢」而非「高置信赢」。
func TestExtractorCycleNoCollapse(t *testing.T) {
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "我在吃降压药和打胰岛素，还有生理期", SegmentIDs: []ids.ID{701}},
	}
	// 四条 self cycle：
	//   medication·降压药 vs medication·胰岛素     —— 同 type 不同 label（钉 CycleLabel 判别位）
	//   medication·降压药(anchor 8-01,conf .9) vs medication·降压药(anchor 9-01,conf .6)
	//                                              —— 同 type+label 仅 anchor 不同：**应塌缩**，高置信(.9,8-01)胜出（钉 anchor 不入键）
	//   menstrual(空 label)                        —— 空 label 合法且自成一键（NULL label 视作独立键）
	resp := `{"facts":[
		{"plane":"cycle","subject":{"kind":"self"},"cycle_type":"medication","cycle_label":"降压药",
		 "anchor_date":"2026-08-01","confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"cycle","subject":{"kind":"self"},"cycle_type":"medication","cycle_label":"胰岛素",
		 "anchor_date":"2026-07-01","confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"cycle","subject":{"kind":"self"},"cycle_type":"medication","cycle_label":"降压药",
		 "anchor_date":"2026-09-01","confidence":0.6,"epistemic_type":"observed","block_index":1},
		{"plane":"cycle","subject":{"kind":"self"},"cycle_type":"menstrual",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1}
	]}`
	ex := &Extractor{LLM: &fakeLLM{resps: []string{resp}}, Model: "m", Prompt: "s", Window: 10}
	facts, err := ex.Extract(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 降压药两条(仅 anchor 不同)塌缩成 1，加胰岛素、menstrual → 共 3 条
	if len(facts) != 3 {
		t.Fatalf("cycle 应 3 条(降压药同 type+label 塌缩、胰岛素/menstrual 各留): %+v", facts)
	}
	// 存活的降压药须是高置信那条(conf .9, anchor 8-01)——证明 anchor 不入键且高置信胜出，
	// 而非「先到的赢」的相反错误。若 anchor 误入键，这里会是 4 条而非 3 条。
	if facts[0].CycleLabel != "降压药" || facts[0].Confidence != 0.9 || facts[0].AnchorDate != "2026-08-01" {
		t.Fatalf("降压药应留高置信那条(conf .9/anchor 8-01): %+v", facts[0])
	}
	// 同 type(medication)不同 label 并存——证明 CycleLabel 参与判别键
	if facts[1].CycleType != "medication" || facts[1].CycleLabel != "胰岛素" {
		t.Fatalf("胰岛素应独立保留: %+v", facts[1])
	}
	// 空 label 的 menstrual 自成一键并保留
	if facts[2].CycleType != "menstrual" || facts[2].CycleLabel != "" {
		t.Fatalf("menstrual(空 label)应独立保留: %+v", facts[2])
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
