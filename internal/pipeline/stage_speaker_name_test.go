// stage_speaker_name_test 验证 speakername stage：纯函数（isAutoName /
// ParseNameCandidates）单测无需 DB；runSpeakerNameStage 集成测试见下（需 TEST_MYSQL_DSN）。
package pipeline

import (
	"testing"
)

func TestIsAutoName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"说话人ab3x9", true},    // 自动登记随机名（rand5 产物）
		{"说话人zzzzz", true},    // 全字母也命中
		{"张三", false},         // 已确认真名
		{"说话人 1", false},      // 显示回退（带空格），从不落库，不该命中
		{"说话人ab3x", false},    // 4 位，非 rand5 形态
		{"说话人AB3X9", false},   // 大写，rand5 只产小写
		{"说话人ab3x9额外", false}, // 后缀多余
	}
	for _, c := range cases {
		if got := isAutoName(c.name); got != c.want {
			t.Errorf("isAutoName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseNameCandidates(t *testing.T) {
	// 正常：含围栏废话、多候选乱序 → 剥壳解析 + 按置信度倒排
	raw := "好的，以下是结果：\n```json\n{\"speakers\":[{\"ref\":\"待识别人物A\",\"candidates\":[{\"name\":\"张明\",\"confidence\":0.4,\"evidence\":\"自称我姓张\"},{\"name\":\"张总\",\"confidence\":0.82,\"evidence\":\"对方称呼张总\"}]},{\"ref\":\"待识别人物B\",\"candidates\":[]}]}\n```"
	got, err := ParseNameCandidates(raw)
	if err != nil {
		t.Fatal(err)
	}
	cands := got["待识别人物A"]
	if len(cands) != 2 {
		t.Fatalf("A 应 2 候选，实际 %d", len(cands))
	}
	if cands[0].Name != "张总" || cands[0].Confidence != 0.82 {
		t.Fatalf("倒序首位应为 张总/0.82，实际 %s/%.2f", cands[0].Name, cands[0].Confidence)
	}
	if cands[0].Evidence != "对方称呼张总" {
		t.Fatalf("evidence 应透传『对方称呼张总』，实际 %q", cands[0].Evidence)
	}
	if len(got["待识别人物B"]) != 0 {
		t.Fatalf("B 应 0 候选")
	}

	// 置信度越界 clamp 到 [0,1]；空名候选丢弃
	got2, err := ParseNameCandidates(`{"speakers":[{"ref":"X","candidates":[{"name":"a","confidence":1.5},{"name":"","confidence":0.9},{"name":"b","confidence":-0.1}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	c2 := got2["X"]
	if len(c2) != 2 {
		t.Fatalf("空名丢弃后应 2 条，实际 %d", len(c2))
	}
	if c2[0].Confidence != 1 || c2[1].Confidence != 0 {
		t.Fatalf("clamp 失败: %.2f %.2f", c2[0].Confidence, c2[1].Confidence)
	}

	// 纯空白名（trim 后为空）也应丢弃
	got3, err := ParseNameCandidates(`{"speakers":[{"ref":"Y","candidates":[{"name":"  ","confidence":0.9},{"name":"c","confidence":0.5}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got3["Y"]) != 1 || got3["Y"][0].Name != "c" {
		t.Fatalf("纯空白名应丢弃，仅留 c，实际 %+v", got3["Y"])
	}

	// 空输出契约：prompt 承诺无待识别人物时输出 {"speakers":[]} → 非 nil 空 map、无 error
	got4, err := ParseNameCandidates(`{"speakers":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got4 == nil {
		t.Fatal("空 speakers 应返回非 nil 空 map")
	}
	if len(got4) != 0 {
		t.Fatalf("空 speakers 应返回空 map，实际 %d 项", len(got4))
	}

	// 彻底非法 JSON → error（stage 走重试）
	if _, err := ParseNameCandidates(`完全不是 JSON`); err == nil {
		t.Fatal("非法 JSON 应返回 error")
	}
}
