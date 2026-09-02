package profile

import "testing"

func TestParseFacts(t *testing.T) {
	// 正常输出（带 markdown 围栏，容错剥掉）
	raw := "```json\n{\"facts\":[\n" +
		"{\"plane\":\"attribute\",\"subject\":{\"kind\":\"self\"},\"attr_key\":\"occupation\"," +
		"\"value\":\"工程师\",\"value_type\":\" text \",\"confidence\":0.9,\"epistemic_type\":\" observed \",\"block_index\":1},\n" +
		"{\"plane\":\"attribute\",\"subject\":{\"kind\":\"mentioned\",\"name\":\" Alice \"}," +
		"\"attr_key\":\"occupation\",\"value\":\"医生\",\"confidence\":0.6,\"epistemic_type\":\"observed\",\"block_index\":2},\n" +
		"{\"plane\":\"relationship\",\"subject\":{\"kind\":\"self\"}," +
		"\"related\":{\"kind\":\"mentioned\",\"name\":\" Alice \"},\"relation_type\":\"配偶\"," +
		"\"label\":\"老婆\",\"confidence\":0.85,\"block_index\":2},\n" +
		"{\"plane\":\"event\",\"subject\":{\"kind\":\"self\"},\"event_type\":\"旅行\"," +
		"\"title\":\" 去云南旅游 \",\"occurred_at\":\" 2026-07-20 \",\"confidence\":0.8,\"block_index\":3}\n]}\n```"
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 4 {
		t.Fatalf("应解析 4 条: %d", len(facts))
	}
	f0 := facts[0]
	if f0.Plane != "attribute" || f0.Subject.Kind != "self" || f0.AttrKey != "occupation" ||
		f0.Value != "工程师" || f0.Confidence != 0.9 || f0.BlockIndex != 1 ||
		f0.EpistemicType != "observed" || f0.ValueType != "text" { // 前后空格应被归一化（epistemic/value_type 均 trim）
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
	// event 平面：title/occurred_at 的前后空格应被 TrimSpace 归一化
	f3 := facts[3]
	if f3.Plane != "event" || f3.EventType != "旅行" || f3.EventTitle != "去云南旅游" || f3.OccurredAt != "2026-07-20" {
		t.Fatalf("event fact3 错误（trim 应生效）: %+v", f3)
	}
}

func TestParseFactsDropsInvalid(t *testing.T) {
	raw := `{"facts":[
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"","value":"缺key","confidence":0.9},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"city","value":"","confidence":0.9},
		{"plane":"bogus","subject":{"kind":"self"},"attr_key":"city","value":"北京","confidence":0.9},
		{"plane":"relationship","subject":{"kind":"self"},"related":{"kind":"mentioned","name":"X"},"relation_type":"师徒","confidence":0.9},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"city","value":"北京","confidence":1.7},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"gender","value":"男","confidence":0.9,"epistemic_type":"神谕"},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"city","value":"北京","direction":"diagonal","confidence":0.9},
		{"plane":"relationship","subject":{"kind":"self"},"related":{},"relation_type":"配偶","confidence":0.9},
		{"plane":"attribute","subject":{"kind":"bogus"},"attr_key":"city","value":"北京","confidence":0.9},
		{"plane":"event","subject":{"kind":"self"},"event_type":"神秘事件","title":"某事","confidence":0.9},
		{"plane":"event","subject":{"kind":"self"},"event_type":"旅行","title":"","confidence":0.9}
	]}`
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 除「置信度越界」那条被钳制保留外，其余全部因条目级非法被丢弃：空 key / 空值 /
	// 非法 plane / 非法关系类型 / 非法 epistemic / 非法 direction / related.kind 空 /
	// subject.kind 非法 / 非法事件类型 / event 空标题。共保留 1 条（confidence 钳制到 1.0）。
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

func TestParseFactsEvent(t *testing.T) {
	raw := `{"facts":[
		{"plane":"event","subject":{"kind":"self"},"event_type":"旅行","title":"去云南旅游一周",
		 "description":"和朋友自驾","occurred_at":"2026-07-20","end_at":"2026-07-27","location":"云南",
		 "importance":0.8,"confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"event","subject":{"kind":"self"},"event_type":"聚会","title":"同学十年聚会",
		 "importance":1.5,"confidence":0.7,"epistemic_type":"observed","block_index":2}
	]}`
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("应 2 条: %d", len(facts))
	}
	f0 := facts[0]
	if f0.EventType != "旅行" || f0.EventTitle != "去云南旅游一周" || f0.OccurredAt != "2026-07-20" ||
		f0.EndAt != "2026-07-27" || f0.EventLocation != "云南" || f0.EventDescription != "和朋友自驾" {
		t.Fatalf("event fact0 错误: %+v", f0)
	}
	// P2a①：importance 解析落 EventImportance
	if f0.EventImportance != 0.8 {
		t.Fatalf("event fact0 importance 应解析为 0.8: %v", f0.EventImportance)
	}
	// P2a①：超范围 importance（1.5）应 clamp 到 1.0
	if facts[1].EventImportance != 1.0 {
		t.Fatalf("event fact1 importance 应 clamp 到 1.0: %v", facts[1].EventImportance)
	}
	// occurred_at 缺省允许（事件仍创建，时间列 NULL 由 service 处理）
	if facts[1].OccurredAt != "" {
		t.Fatalf("occurred_at 应允许为空: %q", facts[1].OccurredAt)
	}
}

func TestParseFactsCycle(t *testing.T) {
	// 注意 frequency 字段的 json 标签是 "frequency"（Go 字段 FrequencyText），对齐 prompt v3 契约
	raw := `{"facts":[
		{"plane":"cycle","subject":{"kind":"self"},"cycle_type":"medication","cycle_label":" 降压药 ",
		 "anchor_date":" 2026-08-01 ","period_days":30,"duration_days":1,"dosage":" 1片 ",
		 "frequency":" 每日一次 ","confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"cycle","subject":{"kind":"self"},"cycle_type":"menstrual","confidence":0.8,"block_index":2},
		{"plane":"cycle","subject":{"kind":"self"},"cycle_type":"bogus","confidence":0.9}
	]}`
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 非法 type（bogus）丢；medication 全字段 + menstrual 空 label 各保留 → 2 条
	if len(facts) != 2 {
		t.Fatalf("应保留 2 条: %+v", facts)
	}
	// 全字段：label/anchor/dosage/frequency 前后空格应 TrimSpace；period/duration 为 int 原样透传
	f0 := facts[0]
	if f0.Plane != "cycle" || f0.CycleType != "medication" || f0.CycleLabel != "降压药" ||
		f0.AnchorDate != "2026-08-01" || f0.PeriodDays != 30 || f0.DurationDays != 1 ||
		f0.Dosage != "1片" || f0.FrequencyText != "每日一次" {
		t.Fatalf("cycle fact0 错误（trim 应生效）: %+v", f0)
	}
	// menstrual 空 label/anchor 合法（纯周期记录，label/anchor 可空——不因缺字段被丢）
	f1 := facts[1]
	if f1.CycleType != "menstrual" || f1.CycleLabel != "" || f1.AnchorDate != "" {
		t.Fatalf("cycle fact1（空 label 应合法）错误: %+v", f1)
	}
}

func TestParseFactsActivity(t *testing.T) {
	// activity 字段的 json 标签是 "activity"（Go 字段 ActivityText）；location 复用 event 的
	// json:"location"（Go 字段 Location），一条 fact 非 event 即 activity，不冲突（见 rawFact 注释）。
	raw := `{"facts":[
		{"plane":"activity","subject":{"kind":"self"},"activity":" 写代码 ","tool":" 电脑 ",
		 "location":" 公司 ","started_at":" 2026-08-20 ","duration_min":120,
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"activity","subject":{"kind":"self"},"activity":"通勤","commute_mode":"地铁",
		 "started_at":"2026-08-20","duration_min":40,"confidence":0.95,"block_index":1},
		{"plane":"activity","subject":{"kind":"self"},"activity":"","tool":"手机","confidence":0.9}
	]}`
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 空 activity 那条被丢（仅强制 activity 非空）；保留 2 条
	if len(facts) != 2 {
		t.Fatalf("应保留 2 条: %+v", facts)
	}
	// 全字段：activity/tool/location/started_at 前后空格应 TrimSpace；duration_min int 原样透传
	f0 := facts[0]
	if f0.Plane != "activity" || f0.ActivityText != "写代码" || f0.Tool != "电脑" ||
		f0.Location != "公司" || f0.StartedAt != "2026-08-20" || f0.DurationMin != 120 {
		t.Fatalf("activity fact0 错误（trim 应生效）: %+v", f0)
	}
	// 通勤：commute_mode 解析；tool/location 缺省允许为空；duration_min 透传
	f1 := facts[1]
	if f1.ActivityText != "通勤" || f1.CommuteMode != "地铁" || f1.Tool != "" || f1.Location != "" || f1.DurationMin != 40 {
		t.Fatalf("activity fact1 错误（可空字段缺省合法）: %+v", f1)
	}
}

// TestParsePetFacts 覆盖 pet 平面解析：合法行保留、name 缺失丢弃、species 原样保留
// （解析层不再归一——非法值/缺省的收敛「其他」推迟到写入期 petRow/mergePetRow）。
func TestParsePetFacts(t *testing.T) {
	raw := `{"facts":[
	  {"plane":"pet","subject":{"kind":"self"},"pet_name":"小花","pet_nickname":"花花",
	   "species":"猫","breed":"布偶","gender":"母","age_text":"3岁","birthday":"2023-04-01",
	   "likes":"不吃鱼","confidence":0.9,"epistemic_type":"observed","block_index":1},
	  {"plane":"pet","subject":{"kind":"self"},"species":"狗","confidence":0.9,"epistemic_type":"observed"},
	  {"plane":"pet","subject":{"kind":"self"},"pet_name":"豆豆","species":"龙猫","confidence":0.9,"epistemic_type":"observed"},
	  {"plane":"pet","subject":{"kind":"self"},"pet_name":"旺财","confidence":0.6,"epistemic_type":"observed"}
	]}`
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Fatalf("应保留 3 条（缺 pet_name 的丢弃）: %d", len(facts))
	}
	f0 := facts[0]
	if f0.PetName != "小花" || f0.PetNickname != "花花" || f0.Species != "猫" ||
		f0.Breed != "布偶" || f0.Gender != "母" || f0.AgeText != "3岁" ||
		f0.Birthday != "2023-04-01" || f0.Likes != "不吃鱼" {
		t.Fatalf("① 字段解析异常: %+v", f0)
	}
	if facts[1].Species != "龙猫" {
		t.Fatalf("② 非法 species 应原样保留（收敛推迟到写入期）: %q", facts[1].Species)
	}
	if facts[2].Species != "" {
		t.Fatalf("③ 缺省 species 应保留空串（未提到，收敛推迟到写入期）: %q", facts[2].Species)
	}
	if facts[2].PetName != "旺财" {
		t.Fatalf("③ pet_name 应保留: %q", facts[2].PetName)
	}
}

// TestParseMentionedNames：顶层 mentioned_names 解析（容围栏/前后缀、trim 空白、缺字段→nil）。
func TestParseMentionedNames(t *testing.T) {
	raw := "好的，这是结果：\n```json\n{\"facts\":[],\"mentioned_names\":[\" 振州 \",\"王工\"]}\n```\n"
	got := ParseMentionedNames(raw)
	if len(got) != 2 || got[0] != "振州" || got[1] != "王工" {
		t.Fatalf("解析/trim 不符: %v", got)
	}
	if ParseMentionedNames(`{"facts":[]}`) != nil {
		t.Fatal("缺 mentioned_names 字段应返回 nil")
	}
	if ParseMentionedNames("不是 JSON") != nil {
		t.Fatal("非法 JSON 应返回 nil（best-effort）")
	}
}
