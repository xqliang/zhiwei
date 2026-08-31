package entity

import (
	"testing"

	"zhiwei/internal/repo"
)

func testEntity(id int64, canonical, kind, py, mt string) repo.Entity {
	e := repo.Entity{Canonical: canonical, Kind: kind}
	if py != "" {
		e.Pinyin = &py
	}
	if mt != "" {
		e.Metaphone = &mt
	}
	return e
}

// TestRecallCandidates 中文召回：ASR 听错的「长梦鱼」应召回实体「张梦瑜」（拼音相似），
// minSim 过滤生效。
func TestRecallCandidates(t *testing.T) {
	ents := []repo.Entity{
		testEntity(1, "张梦瑜", "person", "zhang meng yu", ""),
		testEntity(2, "王芳", "person", "wang fang", ""),
	}
	cands := RecallCandidates("明天长梦鱼要来开会", ents, 5, 0.7)
	if len(cands) == 0 {
		t.Fatal("应召回候选")
	}
	if cands[0].Canonical != "张梦瑜" {
		t.Fatalf("Top-1 应为张梦瑜, got %s", cands[0].Canonical)
	}
	if cands[0].Similarity < 0.7 {
		t.Fatalf("相似度应≥0.7, got %v", cands[0].Similarity)
	}
}

// TestRecallCandidatesLatin 拉丁召回：实体 metaphone 键与段内英文词匹配。
func TestRecallCandidatesLatin(t *testing.T) {
	ents := []repo.Entity{
		testEntity(3, "Skynet", "custom", "skynet", "skynet"),
	}
	// ASR 常见错：大小写/连写差异。
	cands := RecallCandidates("我们在做 Sky-net 的二期", ents, 5, 0.85)
	if len(cands) == 0 || cands[0].Canonical != "Skynet" {
		t.Fatalf("应召回 Skynet, got %+v", cands)
	}
}

// TestRecallCandidatesEmpty 无匹配（段内没有相近发音）→ 空白名单（stage 据此跳过 LLM）。
func TestRecallCandidatesEmpty(t *testing.T) {
	ents := []repo.Entity{testEntity(5, "张梦瑜", "person", "zhang meng yu", "")}
	if cands := RecallCandidates("今天天气不错", ents, 5, 0.7); len(cands) != 0 {
		t.Fatalf("无关文本不应召回, got %+v", cands)
	}
}

// TestRecallCandidatesTopK Top-K 截断：多个候选时只保留相似度最高的 K 个，降序排列。
func TestRecallCandidatesTopK(t *testing.T) {
	ents := []repo.Entity{
		testEntity(1, "张梦瑜", "person", "zhang meng yu", ""),
		testEntity(2, "王芳", "person", "wang fang", ""),
		testEntity(3, "李工", "speaker", "li gong", ""),
	}
	// 文本同时含三个名字的近似发音，K=2 只留 top-2。
	cands := RecallCandidates("长梦鱼和王房还有厉工一起吃饭", ents, 2, 0.6)
	if len(cands) != 2 {
		t.Fatalf("应截断为 2 个, got %d: %+v", len(cands), cands)
	}
	if cands[0].Similarity < cands[1].Similarity {
		t.Fatalf("应按相似度降序: %+v", cands)
	}
}
