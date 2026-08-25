package review

import (
	"context"
	"encoding/json"
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
