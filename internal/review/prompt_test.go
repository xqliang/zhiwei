package review

import (
	"strings"
	"testing"
	"time"
)

func TestBuildDailyUserDeterministic(t *testing.T) {
	in := DailyInput{
		Date:            time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
		MemoriesByTopic: []TopicLines{{Topic: "工作", Lines: []string{"完成设计稿"}}},
		TodosNew:        []string{"发邮件"},
		SessionCount:    2, TotalDurationMS: 65000, SegmentCount: 10, Speakers: []string{"我", "张三"},
	}
	out := BuildDailyUser(in)
	for _, want := range []string{"日期：2026-08-24", "【工作】", "完成设计稿", "发邮件", "录音 2 条", "总时长 65 秒", "张三"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺片段 %q，实际：\n%s", want, out)
		}
	}
	// 空分组占位
	if !strings.Contains(BuildDailyUser(DailyInput{Date: in.Date}), "（无）") {
		t.Error("空输入应含「（无）」占位")
	}
}

func TestBuildTopicStatusUser(t *testing.T) {
	out := BuildTopicStatusUser(TopicStatusInput{TopicName: "Rust 学习", OpenTodos: []string{"读完第 5 章"}})
	if !strings.Contains(out, "话题名称：Rust 学习") || !strings.Contains(out, "读完第 5 章") {
		t.Errorf("话题状态 user message 异常：\n%s", out)
	}
}
