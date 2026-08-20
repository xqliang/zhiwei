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

// ResolveTopics 为每条候选决定 Topic 归属（多对多）：遍历 c.Topics，
// 对每个 ref 应用三规则（直挂合法 id / 同名合并 / 收集为新建建议），
// 返回每候选的 resolved TopicRef 列表（NewName 仍待 commit 建后回填 id）+ 去重的待建主题名。
func ResolveTopics(cands []Candidate, existing []repo.Topic) (refs [][]TopicRef, newNames []string) {
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
	refs = make([][]TopicRef, len(cands))
	for i, c := range cands {
		for _, tr := range c.Topics {
			switch {
			case tr.ExistingID != nil && byID[*tr.ExistingID]:
				id := *tr.ExistingID
				refs[i] = append(refs[i], TopicRef{ExistingID: &id})
			case tr.NewName != "":
				name := strings.TrimSpace(tr.NewName)
				if name == "" {
					continue
				}
				if id, ok := byName[name]; ok {
					refs[i] = append(refs[i], TopicRef{ExistingID: &id})
				} else {
					refs[i] = append(refs[i], TopicRef{NewName: name})
					if !seen[name] {
						seen[name] = true
						newNames = append(newNames, name)
					}
				}
			}
		}
	}
	return refs, newNames
}
