package memory

import (
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func TestResolveTopics(t *testing.T) {
	rustID := ids.ID(101)
	oldID := ids.ID(102) // dismissed，不可挂
	topics := []repo.Topic{
		{ID: rustID, Name: "Rust 学习", Status: "active"},
		{ID: oldID, Name: "旧主题", Status: "dismissed"},
	}

	cand := func(topicID *ids.ID, name string) Candidate {
		return Candidate{Type: "fact", Title: "t", Content: "足够长的一条内容描述",
			EpistemicType: "observed", Confidence: 0.9, TopicID: topicID, SuggestedTopicName: name}
	}
	rustStr := rustID
	badStr := ids.ID(999)

	cands := []Candidate{
		cand(&rustStr, ""),   // 0: 合法 topic_id → 挂 Rust
		cand(&badStr, ""),    // 1: 不存在的 topic_id → 未归类
		cand(nil, "Rust 学习"), // 2: 同名建议 → 合并到已有
		cand(nil, "爸妈健康"),    // 3: 新建议 → 需新建
		cand(nil, "爸妈健康"),    // 4: 同名新建议 → 与 3 共享一个新 topic
		cand(nil, ""),        // 5: 无归属
	}
	refs, newNames := ResolveTopics(cands, topics)

	if len(refs) != 6 {
		t.Fatalf("refs = %d", len(refs))
	}
	if refs[0].ExistingID == nil || *refs[0].ExistingID != rustID {
		t.Fatalf("refs[0] = %+v", refs[0])
	}
	if refs[1].ExistingID != nil || refs[1].NewName != "" {
		t.Fatalf("refs[1] 应未归类: %+v", refs[1])
	}
	if refs[2].ExistingID == nil || *refs[2].ExistingID != rustID {
		t.Fatalf("refs[2] 应合并同名: %+v", refs[2])
	}
	if refs[3].NewName != "爸妈健康" {
		t.Fatalf("refs[3] = %+v", refs[3])
	}
	if refs[5].ExistingID != nil || refs[5].NewName != "" {
		t.Fatalf("refs[5] 应未归类: %+v", refs[5])
	}
	// 新建列表去重
	if len(newNames) != 1 || newNames[0] != "爸妈健康" {
		t.Fatalf("newNames = %v", newNames)
	}
}

func TestResolveTopicsNoExisting(t *testing.T) {
	cands := []Candidate{{Type: "fact", Title: "t", Content: "足够长的一条内容描述",
		EpistemicType: "observed", Confidence: 0.9, SuggestedTopicName: "新主题"}}
	refs, newNames := ResolveTopics(cands, nil)
	if refs[0].NewName != "新主题" || len(newNames) != 1 {
		t.Fatalf("refs=%v newNames=%v", refs, newNames)
	}
}
