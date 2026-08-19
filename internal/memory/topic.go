package memory

import (
	"strings"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TopicRef 是一条候选的 Topic 归属决策结果。
// ExistingID 与 NewName 互斥；两者皆空 = 未归类。
type TopicRef struct {
	ExistingID *ids.ID // 挂到已有 topic
	NewName    string  // 需新建的 topic 名（commit 时创建后回填 id）
}

// ResolveTopics 为每条候选决定 Topic 归属，并收集需要新建的主题名（去重）。
// 规则：
//   - topic_id 指向本 user 的 active/suggested topic → 直接挂
//   - 否则有 suggested_topic_name → 查同名 active/suggested topic，命中合并；未命中收集为新建
//   - 都没有 → 未归类
func ResolveTopics(cands []Candidate, existing []repo.Topic) (refs []TopicRef, newNames []string) {
	byID := map[ids.ID]bool{}
	byName := map[string]ids.ID{}
	for _, tp := range existing {
		if tp.Status == "dismissed" {
			continue
		}
		byID[tp.ID] = true
		byName[tp.Name] = tp.ID
	}
	seen := map[string]bool{}
	refs = make([]TopicRef, len(cands))
	for i, c := range cands {
		switch {
		case c.TopicID != nil && byID[*c.TopicID]:
			id := *c.TopicID
			refs[i] = TopicRef{ExistingID: &id}
		case c.SuggestedTopicName != "":
			name := strings.TrimSpace(c.SuggestedTopicName)
			if name == "" {
				break
			}
			if id, ok := byName[name]; ok {
				refs[i] = TopicRef{ExistingID: &id}
			} else {
				refs[i] = TopicRef{NewName: name}
				if !seen[name] {
					seen[name] = true
					newNames = append(newNames, name)
				}
			}
		}
	}
	return refs, newNames
}
