package entity

import (
	"context"
	"fmt"
	"strings"

	"zhiwei/internal/repo"
)

// seed.go 实现 auto 实体的来源收集。收集逻辑抽成 collectXxx 纯收集函数(返回
// []repo.Entity、不落库),供 assemble.go 的 AssembleEntities(实时聚合、不落库)复用。

// SeedDeps 实体来源依赖。各 repo 为 nil 时对应 kind 跳过（测试/降级装配）。
// 注意：speaker 表当前无 user_id 作用域（历史设计，List 返回全量名册），kind=speaker
// 暂按全量名册非随机名入库（随机名「说话人xxxxx」无纠错价值，跳过）——人名重复入库无害。
type SeedDeps struct {
	KB            *repo.EntityKBRepo
	Persons       *repo.PersonRepo
	Attributes    *repo.PersonAttributeRepo
	Relationships *repo.PersonRelationshipRepo
	Pets          *repo.PersonPetRepo
	Speakers      *repo.SpeakerRepo
	Todos         *repo.TodoRepo
	Topics        *repo.TopicRepo
}

// collectPersonAndProject 收集人物实体(显示名 + 别名 aliases + 关系称呼 label)与项目实体
// (current_projects)。一次遍历人物表同时产出两者(原 RefreshAuto 的捎带收集)。
func collectPersonAndProject(ctx context.Context, d SeedDeps, userID int64) (persons, projects []repo.Entity, err error) {
	ps, err := d.Persons.List(ctx, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("读 person 名册: %w", err)
	}
	for _, p := range ps {
		// person.List 返回非 dismissed（active|pending|merged）；只收 active/pending，
		// merged 是已并入他人的旧行，其名不再单独作为纠错目标。
		if p.Status != "active" && p.Status != "pending" {
			continue
		}
		addSeedEntity(&persons, p.DisplayName, repo.EntityKindPerson, "person:"+p.ID.String())
		if d.Attributes != nil {
			attrs, err := d.Attributes.ListByPerson(ctx, p.ID)
			if err != nil {
				return nil, nil, fmt.Errorf("读 person 属性(person=%s): %w", p.ID, err)
			}
			for _, a := range attrs {
				if a.Status != "active" || a.ValueText == "" {
					continue
				}
				switch a.AttrKey {
				case "aliases":
					addSeedEntity(&persons, a.ValueText, repo.EntityKindPerson, "person_attr:"+a.ID.String())
				case "current_projects":
					addSeedEntity(&projects, a.ValueText, repo.EntityKindProject, "person_attr:"+a.ID.String())
				}
			}
		}
		if d.Relationships != nil {
			rels, err := d.Relationships.ListByPerson(ctx, p.ID)
			if err != nil {
				return nil, nil, fmt.Errorf("读 person 关系(person=%s): %w", p.ID, err)
			}
			for _, rel := range rels {
				// ListByPerson 返回全状态，只收 active 的自由称呼（张总等）。
				if rel.Status == "active" && rel.Label != nil && *rel.Label != "" {
					addSeedEntity(&persons, *rel.Label, repo.EntityKindPerson, "person_rel:"+rel.ID.String())
				}
			}
		}
	}
	return persons, projects, nil
}

// collectPet 收集宠物实体(name + nickname)，按用户的人物遍历(pet 表挂在 person 下)。
func collectPet(ctx context.Context, d SeedDeps, userID int64) ([]repo.Entity, error) {
	ps, err := d.Persons.List(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("读 person 名册: %w", err)
	}
	var list []repo.Entity
	for _, p := range ps {
		petList, err := d.Pets.ListByPerson(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("读 person 宠物(person=%s): %w", p.ID, err)
		}
		for _, pet := range petList {
			if pet.Status != "active" {
				continue
			}
			addSeedEntity(&list, pet.Name, repo.EntityKindPet, "pet:"+pet.ID.String())
			if pet.Nickname != nil && *pet.Nickname != "" {
				addSeedEntity(&list, *pet.Nickname, repo.EntityKindPet, "pet:"+pet.ID.String())
			}
		}
	}
	return list, nil
}

// collectSpeaker 收集说话人实体(全量名册非随机名)。
func collectSpeaker(ctx context.Context, d SeedDeps) ([]repo.Entity, error) {
	sps, err := d.Speakers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("读说话人名册: %w", err)
	}
	var list []repo.Entity
	for _, sp := range sps {
		if isAutoSpeakerName(sp.Name) {
			continue
		}
		addSeedEntity(&list, sp.Name, repo.EntityKindSpeaker, "speaker:"+sp.ID.String())
	}
	return list, nil
}

// collectTopic 收集 active 话题名(topic kind)。
func collectTopic(ctx context.Context, d SeedDeps, userID int64) ([]repo.Entity, error) {
	ts, err := d.Topics.ListActive(ctx, userID, 500)
	if err != nil {
		return nil, fmt.Errorf("读话题: %w", err)
	}
	var list []repo.Entity
	for _, tp := range ts {
		addSeedEntity(&list, tp.Name, repo.EntityKindTopic, "topic:"+tp.ID.String())
	}
	return list, nil
}

// addSeedEntity 追加一个待落库实体（算好拼音/音素键；canonical 去首尾空白）。
// 空/超长（>128 rune，DB VARCHAR(128) 上限）丢弃——宁缺勿错，不让单条脏数据
// 炸掉整批 ReplaceAuto 事务。
func addSeedEntity(list *[]repo.Entity, canonical, kind, sourceRef string) {
	canonical = strings.TrimSpace(canonical)
	if canonical == "" || len([]rune(canonical)) > 128 {
		return
	}
	e := repo.Entity{Canonical: canonical, Kind: kind, Source: repo.EntitySourceAuto, Enabled: true}
	py := NormalizePinyin(canonical)
	if py != "" {
		e.Pinyin = &py
	}
	// 含拉丁成分才落 metaphone 键（纯中文实体该列为 NULL，召回只走拼音）。
	if lt := NormalizeLatin(canonical); lt != "" && lt != py {
		e.Metaphone = &lt
	}
	if sourceRef != "" {
		e.SourceRef = &sourceRef
	}
	*list = append(*list, e)
}

// isAutoSpeakerName 自动登记的随机说话人名（stage_speaker.go 的 rand5 产物形态：
// 「说话人」+5位[a-z0-9]，见 stage_speaker_name.go autoNamePattern）。不 import
// pipeline（避免 entity→pipeline 反向依赖），形态自持一份；形态变更时两处同步。
func isAutoSpeakerName(name string) bool {
	const prefix = "说话人"
	if len(name) != len(prefix)+5 || !strings.HasPrefix(name, prefix) {
		return false
	}
	for _, r := range name[len(prefix):] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
