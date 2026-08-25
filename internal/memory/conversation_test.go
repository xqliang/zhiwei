package memory

// conversation_test.go 覆盖对话转记忆的纯逻辑与「块组装 + Extractor 复用」链路
//（不依赖 DB，mock LLM）：commitConversation 落库放集成测试（conversation_e2e_test.go）。
// 复用 extract_test.go 里已有的 fakeLLM 与 ctx(t) 助手（同包）。

import (
	"strings"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestBuildConversationBlocks 验证对话块组装：只保留 user/assistant 文本发言，
// 跳过工具消息与空白；说话人标签用户/知微；baseTime=首条文本时间；StartMS 为相对偏移；
// SegmentIDs 为空（对话无 transcript 段）。
func TestBuildConversationBlocks(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	cid := ids.New()
	msgs := []repo.AgentMessage{
		{ConversationID: &cid, Role: "user", Kind: "text", Content: "我最近在学 Rust", CreatedAt: t0},
		{ConversationID: &cid, Role: "assistant", Kind: "tool_call", Content: `{"name":"search_memory"}`, CreatedAt: t0.Add(1 * time.Second)},
		{ConversationID: &cid, Role: "assistant", Kind: "text", Content: "了解，学多久了？", CreatedAt: t0.Add(2 * time.Second)},
		{ConversationID: &cid, Role: "user", Kind: "text", Content: "  ", CreatedAt: t0.Add(3 * time.Second)}, // 空白跳过
	}
	blocks, base := buildConversationBlocks(msgs)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2（跳过 tool_call 与空白）", len(blocks))
	}
	if !base.Equal(t0) {
		t.Fatalf("base = %v, want %v", base, t0)
	}
	if blocks[0].SpeakerLabel != "用户" || blocks[1].SpeakerLabel != "知微" {
		t.Fatalf("speaker = %q,%q", blocks[0].SpeakerLabel, blocks[1].SpeakerLabel)
	}
	if blocks[0].StartMS != 0 || blocks[1].StartMS != 2000 {
		t.Fatalf("StartMS = %d,%d, want 0,2000", blocks[0].StartMS, blocks[1].StartMS)
	}
	if len(blocks[0].SegmentIDs) != 0 {
		t.Fatalf("对话块不应有 segment 溯源: %v", blocks[0].SegmentIDs)
	}
	// countTextMessages 与块筛选口径一致：2 条文本消息
	if n := countTextMessages(msgs); n != 2 {
		t.Fatalf("countTextMessages = %d, want 2", n)
	}
}

// TestConversationExtractorReuse 证明源无关的 Extractor 对对话块开箱即用：
// LLM 收到的 User 消息含 用户/知微 标签、候选被解析、EventAt = base + 块 StartMS。
func TestConversationExtractorReuse(t *testing.T) {
	t0 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	cid := ids.New()
	msgs := []repo.AgentMessage{
		{ConversationID: &cid, Role: "user", Kind: "text", Content: "我最近在学 Rust", CreatedAt: t0},
		{ConversationID: &cid, Role: "assistant", Kind: "text", Content: "了解，学多久了？", CreatedAt: t0.Add(2 * time.Second)},
	}
	blocks, base := buildConversationBlocks(msgs)

	// 候选指向第 2 块（知微块，StartMS=2000）→ 验证 EventAt 偏移经对话块流通
	llm := &fakeLLM{responses: []string{`{"candidates":[
	  {"type":"fact","title":"学 Rust","content":"用户最近在学习 Rust 编程语言",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topics":[],"block_index":2}
	]}`}}
	ex := &Extractor{LLM: llm, Model: "fake-model", Prompt: "对话抽取系统指令", Window: 10}
	cands, err := ex.Extract(ctx(t), blocks, nil, base)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("cands = %d, want 1", len(cands))
	}
	// 用户消息含双方说话人标签（证明对话块被正确渲染进 prompt）
	if !strings.Contains(llm.lastUser, "用户") || !strings.Contains(llm.lastUser, "知微") {
		t.Fatalf("user msg 缺说话人标签: %s", llm.lastUser)
	}
	// EventAt = base + 第 2 块 StartMS(2000ms)
	if !cands[0].EventAt.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("EventAt = %v, want %v", cands[0].EventAt, base.Add(2*time.Second))
	}
}

// TestConversationCandidateParse 用任务 3 prompt 的样例结构（topic_id 用真实数字串）喂 ParseCandidates，
// 锁定 prompt ↔ 解析器契约：type/is_todo/todo_due/topics（已有 id / 建议名）解析正确。
func TestConversationCandidateParse(t *testing.T) {
	sample := `{"candidates":[
	  {"type":"preference","title":"偏好晨间深度工作","content":"用户说自己习惯早上做需要专注的工作",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topics":[{"topic_id":"7200000000000001"}],"block_index":2},
	  {"type":"event","title":"周五提交季度报告","content":"用户承诺本周五前提交季度报告",
	   "epistemic_type":"observed","importance":0.7,"confidence":0.88,
	   "is_todo":true,"todo_due":"2026-08-28T18:00:00+08:00","topics":[{"suggested_name":"季度报告"}],"block_index":3}
	]}`
	cands, err := ParseCandidates(sample)
	if err != nil {
		t.Fatalf("ParseCandidates: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("cands = %d, want 2", len(cands))
	}
	// 第一条：preference，非 todo，归入已有 topic id
	if cands[0].Type != "preference" || cands[0].IsTodo {
		t.Fatalf("cand[0] type=%q is_todo=%v", cands[0].Type, cands[0].IsTodo)
	}
	if len(cands[0].Topics) != 1 || cands[0].Topics[0].ExistingID == nil ||
		*cands[0].Topics[0].ExistingID != ids.ID(7200000000000001) {
		t.Fatalf("cand[0] topics 未解析出已有 id: %+v", cands[0].Topics)
	}
	// 第二条：todo，todo_due 解析成 RFC3339 时间，归入建议新主题名
	if !cands[1].IsTodo || cands[1].TodoDue == nil {
		t.Fatalf("cand[1] is_todo=%v todo_due=%v", cands[1].IsTodo, cands[1].TodoDue)
	}
	if len(cands[1].Topics) != 1 || cands[1].Topics[0].NewName != "季度报告" {
		t.Fatalf("cand[1] topics 未解析出建议名: %+v", cands[1].Topics)
	}
}

// TestConversationGate 复用同一质量闸门：低置信被丢、短内容被丢、
// todo 按阈值定 suggested/confirmed。
func TestConversationGate(t *testing.T) {
	cfg := GateConfig{MinConf: 0.6, TodoConf: 0.85}
	cands := []Candidate{
		{Type: "fact", EpistemicType: "observed", Confidence: 0.5, Content: "足够长的内容描述"},                   // 丢：conf<0.6
		{Type: "fact", EpistemicType: "observed", Confidence: 0.9, Content: "短"},                          // 丢：内容<8 rune
		{Type: "fact", EpistemicType: "observed", Confidence: 0.9, Content: "足够长的内容描述啊"},                  // 留
		{Type: "event", EpistemicType: "observed", Confidence: 0.9, IsTodo: true, Content: "足够长的待办内容描述"},  // 留：todo confirmed
		{Type: "event", EpistemicType: "observed", Confidence: 0.7, IsTodo: true, Content: "足够长的待办内容描述二"}, // 留：todo suggested
		{Type: "bogus", EpistemicType: "observed", Confidence: 0.9, Content: "足够长的内容描述"},                  // 丢：非法 type
	}
	out := ApplyGate(cands, cfg)
	if len(out) != 3 {
		t.Fatalf("过闸门 = %d, want 3", len(out))
	}
	var confirmed, suggested int
	for _, c := range out {
		switch c.TodoStatus {
		case "confirmed":
			confirmed++
		case "suggested":
			suggested++
		}
	}
	if confirmed != 1 || suggested != 1 {
		t.Fatalf("todo 状态 confirmed=%d suggested=%d, want 1/1", confirmed, suggested)
	}
}
