package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"zhiwei/internal/repo"
)

// fakeLLM 实现 titleLLM，断言 Chat 入参并返回预设内容/错误。
type fakeLLM struct {
	out string
	err error
}

func (f *fakeLLM) Chat(_ context.Context, _ titleChatReq) (string, error) {
	return f.out, f.err
}

func TestSanitizeTitle(t *testing.T) {
	cases := map[string]string{
		`"项目排期讨论"`: "项目排期讨论",
		"《周报整理》":    "周报整理",
		"关于待办。":      "关于待办",
		"  带空格的标题 ": "带空格的标题",
		"第一行\n第二行":  "第一行",
	}
	for in, want := range cases {
		if got := sanitizeTitle(in); got != want {
			t.Errorf("sanitizeTitle(%q)=%q want %q", in, got, want)
		}
	}
}

func TestShouldGenerate(t *testing.T) {
	cases := []struct {
		source string
		count  int
		title  string
		want   bool
	}{
		{"", 2, "", true},
		{"", 2, "新对话", true},
		{"auto", 3, "旧自动标题", true},
		{"manual", 2, "", false},
		{"", 1, "", false},
		{"", 2, "真实标题", false},
	}
	for _, c := range cases {
		if got := shouldGenerate(c.title, c.source, c.count); got != c.want {
			t.Errorf("shouldGenerate(%q,%q,%d)=%v want %v", c.title, c.source, c.count, got, c.want)
		}
	}
}

func TestGenerateTitleDeps(t *testing.T) {
	msgs := []repo.AgentMessage{
		{Role: "user", Content: "帮我看下本周待办"},
		{Role: "assistant", Content: "好的，以下是待办"},
	}
	deps := &titleDeps{
		state: titleState{title: "", source: ""},
		count: 2,
		msgs:  msgs,
		llm:   &fakeLLM{out: `"本周待办梳理"`},
	}
	got, err := deps.generate(context.Background())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got != "本周待办梳理" {
		t.Errorf("title=%q", got)
	}
	if deps.updatedTo != "本周待办梳理" || deps.updatedSrc != "auto" {
		t.Errorf("未写入 auto: %+v", deps)
	}
}

func TestGenerateTitleManualSkip(t *testing.T) {
	deps := &titleDeps{state: titleState{title: "x", source: "manual"}, count: 5}
	if _, err := deps.generate(context.Background()); !errors.Is(err, errTitleSkip) {
		t.Errorf("manual 应跳过(errTitleSkip), got %v", err)
	}
}

func TestGenerateTitleLLMFailSilent(t *testing.T) {
	deps := &titleDeps{
		state: titleState{title: "", source: ""},
		count: 2,
		llm:   &fakeLLM{err: errors.New("boom")},
	}
	if _, err := deps.generate(context.Background()); !errors.Is(err, errTitleSkip) {
		t.Errorf("LLM 失败应静默跳过(errTitleSkip), got %v", err)
	}
}

// 保证生成标题不含换行/引号（对 LLM 输出鲁棒性回归）
func TestGenerateTitleNoGarbage(t *testing.T) {
	deps := &titleDeps{
		state: titleState{title: "", source: ""},
		count: 2,
		msgs:  []repo.AgentMessage{{Role: "user", Content: "hi"}},
		llm:   &fakeLLM{out: "标题\n还有解释文字"},
	}
	got, err := deps.generate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, "\n") {
		t.Errorf("标题不应含换行: %q", got)
	}
}
