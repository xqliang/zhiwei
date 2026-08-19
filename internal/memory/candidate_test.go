package memory

import (
	"testing"
	"time"

	"zhiwei/internal/ids"
)

func TestParseCandidatesHappyPath(t *testing.T) {
	raw := `{"candidates":[
  {"type":"event","title":"发邮件","content":"明天需要给 Tom 发邮件确认设计稿",
   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
   "is_todo":true,"todo_due":"2026-08-20T10:00:00Z","topic_id":null,
   "suggested_topic_name":null,"block_index":1}
]}`
	cands, err := ParseCandidates(raw)
	if err != nil {
		t.Fatalf("ParseCandidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("len = %d", len(cands))
	}
	c := cands[0]
	if c.Type != "event" || c.Title != "发邮件" || !c.IsTodo {
		t.Fatalf("cand = %+v", c)
	}
	if c.TodoDue == nil || c.TodoDue.UTC().Format(time.RFC3339) != "2026-08-20T10:00:00Z" {
		t.Fatalf("todo_due = %v", c.TodoDue)
	}
}

func TestParseCandidatesTolerance(t *testing.T) {
	// markdown 围栏（\x60 = 反引号，避免源码里嵌套代码围栏）+ 前后废话
	fence := "\x60\x60\x60json"
	raw := "好的，以下是结果：\n" + fence + "\n" +
		`{"candidates":[{"type":"fact","title":"学 Rust","content":"用户正在学习 Rust 计划三个月读完一本书",
   "epistemic_type":"observed","importance":0.7,"confidence":0.9,
   "is_todo":false,"todo_due":null,"topic_id":"123","suggested_topic_name":null,"block_index":2}]}` +
		"\n\x60\x60\x60\n以上。"
	cands, err := ParseCandidates(raw)
	if err != nil {
		t.Fatalf("ParseCandidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("len = %d", len(cands))
	}
	if cands[0].TopicID == nil || *cands[0].TopicID != 123 {
		t.Fatalf("topic_id = %v", cands[0].TopicID)
	}
	if cands[0].BlockIndex != 2 {
		t.Fatalf("block_index = %d", cands[0].BlockIndex)
	}
}

func TestParseCandidatesInvalid(t *testing.T) {
	if _, err := ParseCandidates("完全不是 JSON"); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	// 空候选合法（对话无值得记忆的内容）
	cands, err := ParseCandidates(`{"candidates":[]}`)
	if err != nil || len(cands) != 0 {
		t.Fatalf("空候选: %v %v", cands, err)
	}
}

func TestParseCandidatesBadDue(t *testing.T) {
	// todo_due 非法时间：保留候选，置空 due（不整体失败）
	raw := `{"candidates":[{"type":"event","title":"t","content":"八个字以上的内容描述",
  "epistemic_type":"observed","importance":0.5,"confidence":0.9,
  "is_todo":true,"todo_due":"not-a-date","topic_id":null,"suggested_topic_name":null,"block_index":1}]}`
	cands, err := ParseCandidates(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].TodoDue != nil {
		t.Fatalf("todo_due = %v, want nil", cands[0].TodoDue)
	}
}

func TestParseCandidatesNumericTopicID(t *testing.T) {
	// 模型常见偏差：topic_id 输出成 JSON 数字而非字符串。
	// 必须容错解析成功，而不是让整个 payload 反序列化失败。
	raw := `{"candidates":[{"type":"fact","title":"t","content":"八个字以上的内容描述",
  "epistemic_type":"observed","importance":0.5,"confidence":0.9,
  "is_todo":false,"todo_due":null,"topic_id":123,"suggested_topic_name":null,"block_index":1}]}`
	cands, err := ParseCandidates(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("len = %d", len(cands))
	}
	if cands[0].TopicID == nil || *cands[0].TopicID != 123 {
		t.Fatalf("topic_id = %v, want 123", cands[0].TopicID)
	}
}

func TestApplyGate(t *testing.T) {
	high := 0.9
	low := 0.5
	topicID := ids.ID(9)
	mk := func(conf float64, typ string) Candidate {
		return Candidate{Type: typ, Title: "t", Content: "这是一条足够长的内容描述",
			EpistemicType: "observed", Importance: 0.5, Confidence: conf,
			TopicID: &topicID}
	}
	cands := []Candidate{
		mk(high, "event"),        // 0: 通过
		mk(low, "event"),         // 1: 置信度不足，丢弃
		mk(high, "unknown-type"), // 2: 枚举外 type，丢弃
		{Type: "fact", Title: "t", Content: "太短", EpistemicType: "observed",
			Confidence: high}, // 3: 内容不足 8 字，丢弃
	}
	gated := ApplyGate(cands, GateConfig{MinConf: 0.6, TodoConf: 0.85})
	if len(gated) != 1 {
		t.Fatalf("gated = %d, want 1", len(gated))
	}
	if gated[0].Type != "event" {
		t.Fatalf("gated[0] = %+v", gated[0])
	}
}

func TestApplyGateTodoStatus(t *testing.T) {
	mkTodo := func(conf float64) Candidate {
		return Candidate{Type: "event", Title: "t", Content: "明天需要给 Tom 发邮件确认设计稿",
			EpistemicType: "observed", Importance: 0.5, Confidence: conf, IsTodo: true}
	}
	gated := ApplyGate([]Candidate{mkTodo(0.9), mkTodo(0.7)},
		GateConfig{MinConf: 0.6, TodoConf: 0.85})
	if len(gated) != 2 {
		t.Fatalf("len = %d", len(gated))
	}
	if gated[0].TodoStatus != "confirmed" {
		t.Fatalf("高置信 todo 应 confirmed，got %s", gated[0].TodoStatus)
	}
	if gated[1].TodoStatus != "suggested" {
		t.Fatalf("低置信 todo 应 suggested，got %s", gated[1].TodoStatus)
	}
	// 非 todo 不填状态
	gated2 := ApplyGate([]Candidate{{Type: "fact", Title: "t",
		Content: "这是一条足够长的内容描述", EpistemicType: "observed", Confidence: 0.9}},
		GateConfig{MinConf: 0.6, TodoConf: 0.85})
	if gated2[0].TodoStatus != "" {
		t.Fatalf("非 todo TodoStatus = %s", gated2[0].TodoStatus)
	}
}
