package profile

import "testing"

func TestParseFacts(t *testing.T) {
	// 正常输出（带 markdown 围栏，容错剥掉）
	raw := "```json\n{\"facts\":[\n" +
		"{\"plane\":\"attribute\",\"subject\":{\"kind\":\"self\"},\"attr_key\":\"occupation\"," +
		"\"value\":\"工程师\",\"confidence\":0.9,\"epistemic_type\":\" observed \",\"block_index\":1},\n" +
		"{\"plane\":\"attribute\",\"subject\":{\"kind\":\"mentioned\",\"name\":\" Alice \"}," +
		"\"attr_key\":\"occupation\",\"value\":\"医生\",\"confidence\":0.6,\"epistemic_type\":\"observed\",\"block_index\":2},\n" +
		"{\"plane\":\"relationship\",\"subject\":{\"kind\":\"self\"}," +
		"\"related\":{\"kind\":\"mentioned\",\"name\":\" Alice \"},\"relation_type\":\"配偶\"," +
		"\"label\":\"老婆\",\"confidence\":0.85,\"block_index\":2}\n]}\n```"
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Fatalf("应解析 3 条: %d", len(facts))
	}
	f0 := facts[0]
	if f0.Plane != "attribute" || f0.Subject.Kind != "self" || f0.AttrKey != "occupation" ||
		f0.Value != "工程师" || f0.Confidence != 0.9 || f0.BlockIndex != 1 ||
		f0.EpistemicType != "observed" { // 前后空格应被归一化为 "observed"（不被 validEpistemic 拒掉）
		t.Fatalf("fact0 错误: %+v", f0)
	}
	// subject/related 的名字应 TrimSpace（否则后续人物归属匹配不上）
	if facts[1].Subject.Name != "Alice" {
		t.Fatalf("subject.name 未 TrimSpace: %q", facts[1].Subject.Name)
	}
	f2 := facts[2]
	if f2.Plane != "relationship" || f2.RelationType != "配偶" || f2.Related.Name != "Alice" || f2.Label != "老婆" {
		t.Fatalf("fact2 错误: %+v", f2)
	}
}

func TestParseFactsDropsInvalid(t *testing.T) {
	raw := `{"facts":[
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"","value":"缺key","confidence":0.9},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"city","value":"","confidence":0.9},
		{"plane":"bogus","subject":{"kind":"self"},"attr_key":"city","value":"北京","confidence":0.9},
		{"plane":"relationship","subject":{"kind":"self"},"related":{"kind":"mentioned","name":"X"},"relation_type":"师徒","confidence":0.9},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"city","value":"北京","confidence":1.7},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"gender","value":"男","confidence":0.9,"epistemic_type":"神谕"}
	]}`
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 前 5 条非法被丢弃（空 key/空值/非法 plane/非法关系类型/非法 epistemic）；置信度越界被钳制保留
	if len(facts) != 1 {
		t.Fatalf("应保留 1 条: %+v", facts)
	}
	if facts[0].Confidence != 1.0 {
		t.Fatalf("confidence 未钳制: %v", facts[0].Confidence)
	}
}

func TestParseFactsEmpty(t *testing.T) {
	facts, err := ParseFacts(`{"facts":[]}`)
	if err != nil || len(facts) != 0 {
		t.Fatalf("空 facts 应成功: %v %v", facts, err)
	}
	if _, err := ParseFacts(`完全不是 JSON`); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}
