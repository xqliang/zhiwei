package pipeline

import (
	"testing"
)

// TestParseCorrectionEdits 解析 LLM 输出：正常/带围栏废话/空 edits/清洗/非法 JSON。
func TestParseCorrectionEdits(t *testing.T) {
	// 1) 正常输出。
	got, err := ParseCorrectionEdits(`{"edits":[{"orig":"常梦瑜","corrected":"张梦瑜","entity_id":"9527","confidence":0.9,"reason":"读音相近"}]}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 1 || got[0].Orig != "常梦瑜" || got[0].Corrected != "张梦瑜" || got[0].EntityID != "9527" {
		t.Fatalf("解析不符: %+v", got)
	}
	if got[0].Confidence != 0.9 {
		t.Fatalf("置信度不符: %v", got[0].Confidence)
	}

	// 2) 带代码围栏与前后废话（容错：截首{尾}，同 ParseNameCandidates）。
	got, err = ParseCorrectionEdits("好的，以下是纠错结果：\n```json\n{\"edits\":[]}\n```")
	if err != nil || len(got) != 0 {
		t.Fatalf("围栏容错失败: %v %+v", err, got)
	}

	// 3) 清洗：置信度越界 clamp、空 orig/corrected 丢弃。
	got, err = ParseCorrectionEdits(`{"edits":[{"orig":"","corrected":"张梦瑜","entity_id":"9527","confidence":1.5},{"orig":"阿黄","corrected":"阿皇","entity_id":"9528","confidence":0.8}]}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("空 orig 应丢弃: %+v", got)
	}
	if got[0].Confidence != 0.8 {
		t.Fatalf("置信度应保留 0.8: %+v", got)
	}
	// clamp 验证：单独一条越界的。
	got, err = ParseCorrectionEdits(`{"edits":[{"orig":"阿黄","corrected":"阿皇","entity_id":"9528","confidence":1.5}]}`)
	if err != nil || len(got) != 1 || got[0].Confidence != 1 {
		t.Fatalf("置信度应 clamp 到 1: %v %+v", err, got)
	}

	// 4) 彻底非法 JSON → error（调用方吞错跳过该段）。
	if _, err := ParseCorrectionEdits(`这不是JSON`); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}
