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
	cand := func(ts []TopicRef) Candidate {
		return Candidate{Type: "fact", Title: "t", Content: "足够长的一条内容描述",
			EpistemicType: "observed", Confidence: 0.9, Topics: ts}
	}
	rustStr := rustID
	bad := ids.ID(999)

	cands := []Candidate{
		cand([]TopicRef{{ExistingID: &rustStr}}),                      // 0: 合法 → Rust
		cand([]TopicRef{{ExistingID: &bad}}),                          // 1: 不存在 → 空
		cand([]TopicRef{{NewName: "Rust 学习"}}),                        // 2: 同名 → 合并
		cand([]TopicRef{{NewName: "爸妈健康"}}),                         // 3: 新建议
		cand([]TopicRef{{NewName: "爸妈健康"}}),                         // 4: 同名新建议
		cand(nil),                                                     // 5: 无归属
		cand([]TopicRef{{ExistingID: &rustStr}, {NewName: "爸妈健康"}}), // 6: 多 ref
	}
	refs, newNames := ResolveTopics(cands, topics)

	if len(refs) != 7 {
		t.Fatalf("refs = %d", len(refs))
	}
	if len(refs[0]) != 1 || *refs[0][0].ExistingID != rustID {
		t.Fatalf("refs[0] = %+v", refs[0])
	}
	if len(refs[1]) != 0 {
		t.Fatalf("refs[1] 应空: %+v", refs[1])
	}
	if len(refs[2]) != 1 || *refs[2][0].ExistingID != rustID {
		t.Fatalf("refs[2] 应合并同名: %+v", refs[2])
	}
	if len(refs[3]) != 1 || refs[3][0].NewName != "爸妈健康" {
		t.Fatalf("refs[3] = %+v", refs[3])
	}
	if len(refs[5]) != 0 {
		t.Fatalf("refs[5] 应空: %+v", refs[5])
	}
	if len(refs[6]) != 2 {
		t.Fatalf("refs[6] 应 2 项: %+v", refs[6])
	}
	if len(newNames) != 1 || newNames[0] != "爸妈健康" {
		t.Fatalf("newNames = %v", newNames)
	}
	_ = bad
}

func TestResolveTopicsNoExisting(t *testing.T) {
	cands := []Candidate{{Type: "fact", Title: "t", Content: "足够长的一条内容描述",
		EpistemicType: "observed", Confidence: 0.9, Topics: []TopicRef{{NewName: "新主题"}}}}
	refs, newNames := ResolveTopics(cands, nil)
	if len(refs[0]) != 1 || refs[0][0].NewName != "新主题" || len(newNames) != 1 {
		t.Fatalf("refs=%v newNames=%v", refs, newNames)
	}
}
