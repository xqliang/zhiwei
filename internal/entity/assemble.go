package entity

import (
	"context"
	"strings"

	"zhiwei/internal/repo"
)

// assemble.go 实现实时实体聚合(去拷贝化):纠错/设置页不再读 entity_kb 的 auto 拷贝,
// 而是每次从 person/pet/project/topic/speaker 源表实时聚合成白名单。
// 与 seed.go 的 RefreshAuto(全删全落拷贝,过渡期保留)共用 collectXxx 收集函数。

// placeholderNames 明显占位名集合(需求④):与 isAutoSpeakerName(说话人+5位)一起,
// 对 person/speaker 等所有 kind 生效——修此前只过滤 speaker、person 线漏网把占位名收进实体表。
var placeholderNames = map[string]bool{
	"未知": true, "未知同事": true, "未命名": true,
}

// isPlaceholderName 判定是否应排除的占位名:自动说话人名(说话人+5位)或明显占位名。
func isPlaceholderName(name string) bool {
	if isAutoSpeakerName(name) {
		return true
	}
	return placeholderNames[strings.TrimSpace(name)]
}

// kindDedupPriority 跨 kind 去重优先级:person 最低(=最高优先),其余相同。
// 需求③:人物与说话人同名只留一条,person 是真身档案优先保留。
func kindDedupPriority(kind string) int {
	if kind == repo.EntityKindPerson {
		return 0
	}
	return 1
}

// dedupeAcrossKinds 按 canonical(大小写不敏感,与 entity_kb ai_ci 唯一键一致)跨 kind 去重,
// 同名取优先级最高者(person 优先);保持首次出现顺序。
func dedupeAcrossKinds(list []repo.Entity) []repo.Entity {
	best := map[string]repo.Entity{}
	order := make([]string, 0, len(list))
	for _, e := range list {
		key := strings.ToLower(strings.TrimSpace(e.Canonical))
		if key == "" {
			continue
		}
		if cur, ok := best[key]; ok {
			if kindDedupPriority(e.Kind) < kindDedupPriority(cur.Kind) {
				best[key] = e
			}
			continue
		}
		best[key] = e
		order = append(order, key)
	}
	out := make([]repo.Entity, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

// MergeWhitelist 纠错白名单合并(纯函数):auto 先入(剔除 disabled,大小写不敏感),
// 再叠加 manual;同 canonical 由 manual 覆盖(auto 无稳定 id,manual 保留真实 id+note)。
func MergeWhitelist(auto, manual []repo.Entity, disabled map[string]bool) []repo.Entity {
	out := make([]repo.Entity, 0, len(auto)+len(manual))
	idx := map[string]int{} // lower(canonical) → out 下标
	for _, e := range auto {
		key := strings.ToLower(strings.TrimSpace(e.Canonical))
		if key == "" {
			continue
		}
		if disabled != nil && disabled[key] {
			continue
		}
		if _, ok := idx[key]; ok {
			continue
		}
		idx[key] = len(out)
		out = append(out, e)
	}
	for _, e := range manual {
		key := strings.ToLower(strings.TrimSpace(e.Canonical))
		if key == "" {
			continue
		}
		if i, ok := idx[key]; ok {
			out[i] = e // manual 同名覆盖 auto
			continue
		}
		idx[key] = len(out)
		out = append(out, e)
	}
	return out
}

// AssembleEntities 实时聚合用户实体白名单(**不落库**):来源 = person(显示名+别名+称呼)
// /project/pet/speaker/topic;**不含 task**(需求②)。收集后过滤占位名(需求④)、
// 跨 kind 去重 person 优先(需求③)。读源表失败返回 error(调用方降级)。
func AssembleEntities(ctx context.Context, d SeedDeps, userID int64, kinds []string) ([]repo.Entity, error) {
	enabled := map[string]bool{}
	for _, k := range kinds {
		enabled[k] = true
	}
	var all []repo.Entity
	// person(捎带 project):与原 RefreshAuto 一致,一次遍历同时收人物与项目。
	if enabled[repo.EntityKindPerson] && d.Persons != nil {
		persons, projects, err := collectPersonAndProject(ctx, d, userID)
		if err != nil {
			return nil, err
		}
		all = append(all, persons...)
		if enabled[repo.EntityKindProject] {
			all = append(all, projects...)
		}
	}
	if enabled[repo.EntityKindPet] && d.Pets != nil && d.Persons != nil {
		pets, err := collectPet(ctx, d, userID)
		if err != nil {
			return nil, err
		}
		all = append(all, pets...)
	}
	if enabled[repo.EntityKindSpeaker] && d.Speakers != nil {
		sps, err := collectSpeaker(ctx, d)
		if err != nil {
			return nil, err
		}
		all = append(all, sps...)
	}
	if enabled[repo.EntityKindTopic] && d.Topics != nil {
		tps, err := collectTopic(ctx, d, userID)
		if err != nil {
			return nil, err
		}
		all = append(all, tps...)
	}
	// task 不再收集(需求②):待办不进实体词。

	// 过滤占位名(对所有 kind 生效)+ 跨 kind 去重(person 优先)。
	filtered := all[:0]
	for _, e := range all {
		if isPlaceholderName(e.Canonical) {
			continue
		}
		filtered = append(filtered, e)
	}
	return dedupeAcrossKinds(filtered), nil
}
