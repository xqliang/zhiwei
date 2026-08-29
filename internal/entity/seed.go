package entity

import (
	"context"
	"strings"

	"zhiwei/internal/repo"
)

// SeedDeps 种子刷新依赖（correct stage 每次运行前刷新实体库）。
// 各 repo 为 nil 时对应 kind 跳过（测试/降级装配）；KB 为 nil 时整个刷新 no-op。
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

// RefreshAuto 重建用户 auto 实体：对 kinds 里每个 kind，收集当前来源行的名字
// → ReplaceAuto（事务内删旧 auto 该 kind + 落新，带拼音/音素键；manual 条目与
// 禁用态由 ReplaceAuto 内部保留）。
// 任一 kind 刷新失败即返回错误——调用方（correct stage）吞错降级用库内旧实体继续。
func RefreshAuto(ctx context.Context, d SeedDeps, userID int64, kinds []string) error {
	if d.KB == nil {
		return nil
	}
	enabled := map[string]bool{}
	for _, k := range kinds {
		enabled[k] = true
	}
	// person 聚合：display_name + 别名(aliases) + 称呼(relationship.label)；同一轮遍历
	// 顺带收集 current_projects（归 project kind，少查一遍属性表）。
	if enabled[repo.EntityKindPerson] && d.Persons != nil {
		var persons, projects []repo.Entity
		if ps, err := d.Persons.List(ctx, userID); err == nil {
			for _, p := range ps {
				// person.List 返回非 dismissed（active|pending|merged）；只收 active/pending，
				// merged 是已并入他人的旧行，其名不再单独作为纠错目标。
				if p.Status != "active" && p.Status != "pending" {
					continue
				}
				addSeedEntity(&persons, p.DisplayName, repo.EntityKindPerson, "person:"+p.ID.String())
				if d.Attributes != nil {
					if attrs, err := d.Attributes.ListByPerson(ctx, p.ID); err == nil {
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
				}
				if d.Relationships != nil {
					if rels, err := d.Relationships.ListByPerson(ctx, p.ID); err == nil {
						for _, rel := range rels {
							// ListByPerson 返回全状态，只收 active 的自由称呼（张总等）。
							if rel.Status == "active" && rel.Label != nil && *rel.Label != "" {
								addSeedEntity(&persons, *rel.Label, repo.EntityKindPerson, "person_rel:"+rel.ID.String())
							}
						}
					}
				}
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindPerson, dedupeSeed(persons)); err != nil {
			return err
		}
		if enabled[repo.EntityKindProject] {
			if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindProject, dedupeSeed(projects)); err != nil {
				return err
			}
		}
	} else if enabled[repo.EntityKindProject] && d.Persons != nil && d.Attributes != nil {
		// person 关了但 project 开着：单独跑一遍属性收集（少见配置，简单实现）。
		var projects []repo.Entity
		if ps, err := d.Persons.List(ctx, userID); err == nil {
			for _, p := range ps {
				if p.Status != "active" && p.Status != "pending" {
					continue
				}
				if attrs, err := d.Attributes.ListByPerson(ctx, p.ID); err == nil {
					for _, a := range attrs {
						if a.Status == "active" && a.AttrKey == "current_projects" && a.ValueText != "" {
							addSeedEntity(&projects, a.ValueText, repo.EntityKindProject, "person_attr:"+a.ID.String())
						}
					}
				}
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindProject, dedupeSeed(projects)); err != nil {
			return err
		}
	}
	// pet：name + nickname（按用户的人物遍历；pet 表挂在 person 下）。
	if enabled[repo.EntityKindPet] && d.Pets != nil && d.Persons != nil {
		var list []repo.Entity
		if ps, err := d.Persons.List(ctx, userID); err == nil {
			for _, p := range ps {
				if petList, err := d.Pets.ListByPerson(ctx, p.ID); err == nil {
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
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindPet, dedupeSeed(list)); err != nil {
			return err
		}
	}
	// speaker：全量名册非随机名（「说话人xxxxx」是自动登记占位名，无纠错价值）。
	if enabled[repo.EntityKindSpeaker] && d.Speakers != nil {
		var list []repo.Entity
		if sps, err := d.Speakers.List(ctx); err == nil {
			for _, sp := range sps {
				if isAutoSpeakerName(sp.Name) {
					continue
				}
				addSeedEntity(&list, sp.Name, repo.EntityKindSpeaker, "speaker:"+sp.ID.String())
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindSpeaker, dedupeSeed(list)); err != nil {
			return err
		}
	}
	// task：未关闭 todo 标题。
	if enabled[repo.EntityKindTask] && d.Todos != nil {
		var list []repo.Entity
		if titles, err := d.Todos.ListOpenTitles(ctx, userID); err == nil {
			for _, t := range titles {
				addSeedEntity(&list, t, repo.EntityKindTask, "")
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindTask, dedupeSeed(list)); err != nil {
			return err
		}
	}
	// topic：active 话题名。
	if enabled[repo.EntityKindTopic] && d.Topics != nil {
		var list []repo.Entity
		if ts, err := d.Topics.ListActive(ctx, userID, 500); err == nil {
			for _, tp := range ts {
				addSeedEntity(&list, tp.Name, repo.EntityKindTopic, "topic:"+tp.ID.String())
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindTopic, dedupeSeed(list)); err != nil {
			return err
		}
	}
	return nil
}

// addSeedEntity 追加一个待落库实体（算好拼音/音素键；canonical 去首尾空白）。
// 空/超长（>128 rune，DB VARCHAR(128) 上限）丢弃——宁缺勿错，不让单条脏数据
// 炸掉整批 ReplaceAuto 事务。
func addSeedEntity(list *[]repo.Entity, canonical, kind, sourceRef string) {
	canonical = strings.TrimSpace(canonical)
	if canonical == "" || len([]rune(canonical)) > 128 {
		return
	}
	e := repo.Entity{Canonical: canonical, Kind: kind, Enabled: true}
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

// dedupeSeed 同 canonical 去重（不同来源行可能产出同名，如 person 别名=pet 名；
// 唯一键 (user_id, canonical, kind) 下重复插入会被 INSERT IGNORE 静默跳过，去重
// 让 source_ref 尽量指向首个来源，行为更可预期）。
func dedupeSeed(list []repo.Entity) []repo.Entity {
	seen := map[string]bool{}
	out := list[:0]
	for _, e := range list {
		if seen[e.Canonical] {
			continue
		}
		seen[e.Canonical] = true
		out = append(out, e)
	}
	return out
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
