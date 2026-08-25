package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zhiwei/internal/provider"
)

// fakeLLM 是单测用 mock：Chat 返回预置 Reply（或 Err），并记录收到的 System/User。
// 实现 provider.LLMProvider，故无需 MySQL/网络即可测「渲染核」。
type fakeLLM struct {
	Reply  string
	Err    error
	GotReq provider.ChatRequest
}

func (f *fakeLLM) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	f.GotReq = req
	if f.Err != nil {
		return provider.ChatResponse{}, f.Err
	}
	return provider.ChatResponse{Content: f.Reply, TotalTokens: 42}, nil
}

func TestDailyContentRoundTrip(t *testing.T) {
	in := DailyContent{
		Headline: "今天完成了 X", Highlights: []string{"a", "b"},
		Todos:             DailyTodos{New: []string{"n1"}, Done: []string{}, Open: []string{"o1"}},
		TopicDistribution: []TopicCount{{Topic: "工作", Count: 3}},
	}
	b := mustJSON(in)
	var out DailyContent
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Headline != in.Headline || len(out.Highlights) != 2 || out.Todos.New[0] != "n1" {
		t.Errorf("round-trip 丢字段: %+v", out)
	}
}

// ---- M5：nil 切片兜底成 [] 而非 null ----

// TestNormalizeDailyNoNullSlices：日报所有切片留 nil，normalize 后序列化应全为 []。
func TestNormalizeDailyNoNullSlices(t *testing.T) {
	c := &DailyContent{Headline: "只有标题"} // 所有切片留 nil
	normalizeDaily(c)
	b := string(mustJSON(c))
	for _, want := range []string{
		`"highlights":[]`, `"decisions":[]`,
		`"new":[]`, `"done":[]`, `"open":[]`,
		`"insights":[]`, `"tomorrow":[]`, `"topic_distribution":[]`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("应含 %s，实际 = %s", want, b)
		}
	}
	if strings.Contains(b, "null") {
		t.Errorf("不应出现 null: %s", b)
	}
}

// TestNormalizeWeeklyNoNullSlices：验证顶层 + by_topic/trends 元素内部切片都被兜底。
func TestNormalizeWeeklyNoNullSlices(t *testing.T) {
	c := &WeeklyContent{
		Headline: "本周",
		ByTopic:  []WeeklyTopic{{Topic: "工作", Progress: 0.5}}, // KeyEvents/OpenTodos/Risks nil
		Trends:   []Trend{{Metric: "每日记忆数"}},                    // Series nil
	}
	normalizeWeekly(c)
	b := string(mustJSON(c))
	for _, want := range []string{
		`"key_events":[]`, `"open_todos":[]`, `"risks":[]`,
		`"series":[]`, `"next_week":[]`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("应含 %s，实际 = %s", want, b)
		}
	}
	if strings.Contains(b, "null") {
		t.Errorf("不应出现 null: %s", b)
	}
}

// TestNormalizeTopicStatusNoNullSlices：话题状态所有切片留 nil，normalize 后应全为 []。
func TestNormalizeTopicStatusNoNullSlices(t *testing.T) {
	c := &TopicStatusContent{Summary: "s"} // 切片留 nil
	normalizeTopicStatus(c)
	b := string(mustJSON(c))
	for _, want := range []string{
		`"milestones":[]`, `"decisions":[]`, `"open_todos":[]`, `"risks":[]`, `"blockers":[]`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("应含 %s，实际 = %s", want, b)
		}
	}
	if strings.Contains(b, "null") {
		t.Errorf("不应出现 null: %s", b)
	}
}
