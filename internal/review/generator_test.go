package review

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGenerateDailyOK(t *testing.T) {
	f := &fakeLLM{Reply: `{"headline":"今天很好","highlights":["a"]}`}
	g := &Generator{LLM: f, Model: "m", DailyPrompt: "SYS"}
	c, raw, err := g.generateDaily(context.Background(), DailyInput{Date: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if c.Headline != "今天很好" || len(raw) == 0 {
		t.Errorf("内容异常: %+v", c)
	}
	if f.GotReq.System != "SYS" || f.GotReq.Model != "m" {
		t.Errorf("Chat 请求未带 prompt/model: %+v", f.GotReq)
	}
}

func TestGenerateDailyLLMErr(t *testing.T) {
	g := &Generator{LLM: &fakeLLM{Err: errors.New("boom")}, Model: "m"}
	if _, _, err := g.generateDaily(context.Background(), DailyInput{}); err == nil {
		t.Error("LLM 错误应上抛")
	}
}

func TestGenerateDailyParseErr(t *testing.T) {
	g := &Generator{LLM: &fakeLLM{Reply: "模型跑偏没给 JSON"}, Model: "m"}
	if _, _, err := g.generateDaily(context.Background(), DailyInput{}); err == nil {
		t.Error("解析失败应上抛")
	}
}
