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

// TestParseWeeklyPatternsAsObjects：模型偶尔把 patterns 写成对象数组（2026-08-31 22:00
// 周报定时任务实测翻车：cannot unmarshal object into WeeklyContent.patterns of type string）。
// LooseStrings 须容错抽取文本字段，整份周报不再因这一个字段报废。
func TestParseWeeklyPatternsAsObjects(t *testing.T) {
	raw := `{"headline":"H","patterns":[{"text":"周二和周四深夜处理待办","confidence":0.8},"纯字符串条目"],"narrative":"n"}`
	c, err := ParseWeekly(raw)
	if err != nil {
		t.Fatalf("patterns 为对象数组时不应解析失败: %v", err)
	}
	if len(c.Patterns) != 2 || c.Patterns[0] != "周二和周四深夜处理待办" || c.Patterns[1] != "纯字符串条目" {
		t.Fatalf("patterns 容错抽取不符: %+v", c.Patterns)
	}
}

// TestParseDailyInsightsMixed：insights 混排字符串/对象/未知键对象时的容错。
func TestParseDailyInsightsMixed(t *testing.T) {
	raw := `{"headline":"H","insights":["纯字符串",{"insight":"带 insight 键的对象"},{"未知键":"v"}]}`
	c, err := ParseDaily(raw)
	if err != nil {
		t.Fatalf("insights 混排不应解析失败: %v", err)
	}
	if len(c.Insights) != 3 || c.Insights[0] != "纯字符串" || c.Insights[1] != "带 insight 键的对象" || c.Insights[2] == "" {
		t.Fatalf("insights 容错抽取不符: %+v", c.Insights)
	}
}
