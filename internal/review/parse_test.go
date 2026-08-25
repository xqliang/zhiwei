package review

import "testing"

func TestParseDailyStripsFence(t *testing.T) {
	raw := "好的，这是日报：\n```json\n{\"headline\":\"H\",\"highlights\":[\"x\"],\"todos\":{\"new\":[],\"done\":[],\"open\":[\"o\"]},\"topic_distribution\":[{\"topic\":\"工作\",\"count\":2}]}\n```\n"
	c, err := ParseDaily(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Headline != "H" || len(c.Highlights) != 1 || c.Todos.Open[0] != "o" || c.TopicDistribution[0].Count != 2 {
		t.Errorf("解析结果异常: %+v", c)
	}
}

func TestParseDailyInvalid(t *testing.T) {
	if _, err := ParseDaily("这不是 JSON，模型跑偏了"); err == nil {
		t.Error("非法 JSON 应返回 error")
	}
}

func TestParseTopicStatusRisks(t *testing.T) {
	raw := `{"summary":"s","progress":0.5,"risks":[{"desc":"缺人","severity":"high"}],"blockers":[]}`
	c, err := ParseTopicStatus(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Progress != 0.5 || len(c.Risks) != 1 || c.Risks[0].Severity != "high" {
		t.Errorf("解析结果异常: %+v", c)
	}
}

func TestParseWeeklyMinimal(t *testing.T) {
	c, err := ParseWeekly(`{}`)
	if err != nil || c == nil {
		t.Fatalf("空对象应解析为零值结构: %v", err)
	}
}
