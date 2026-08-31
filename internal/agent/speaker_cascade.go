package agent

import (
	"context"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// CascadePrompt 是需要用户确认删除的人物信息。
// 声纹被删/合并时，若关联人物被编辑过（或手动创建），不自动删除，
// 而是把该人物打包成一条 prompt 返回，交由用户确认后再决定去留。
type CascadePrompt struct {
	PersonID string `json:"person_id"`
	Name     string `json:"name"`
	Reason   string `json:"reason"`
}

// isPersonEdited 判断人物是否被编辑过（有非 create 的变更记录）。
// 「编辑过」= person_change_log 中存在任意 change_type != 'create' 的记录。
func isPersonEdited(logs []*repo.PersonChangeLog) bool {
	for _, l := range logs {
		if l.ChangeType != "create" {
			return true
		}
	}
	return false
}

// shouldCascadePerson 判断是否应该级联 dismiss 人物。
// 条件：非 owner + llm 创建 + 未被编辑过。
// owner 永不删；手动创建（source=manual）的人物需用户确认，不自动删。
func shouldCascadePerson(person *repo.Person, logs []*repo.PersonChangeLog) bool {
	if person.IsOwner {
		return false
	}
	if person.Source != "llm" {
		return false
	}
	return !isPersonEdited(logs)
}

// buildCascadePrompt 构建确认提示。
func buildCascadePrompt(person *repo.Person, reason string) CascadePrompt {
	return CascadePrompt{
		PersonID: person.ID.String(),
		Name:     person.DisplayName,
		Reason:   reason,
	}
}

// personDismissed 判断人物是否已被 dismiss（测试辅助）。
func personDismissed(person *repo.Person) bool {
	return person.Status == "dismissed"
}

// CascadeSpeaker 执行声纹→人物的级联处理。
// 返回需要用户确认的人物列表（编辑过 / 手动创建的人物不自动删，需确认）。
//
// dismissFn 可选：传入则执行实际的 dismiss（软删）落库操作；
// 为 nil 时仅在内存中设置 person.Status 字段（测试用，不触库）。
func CascadeSpeaker(ctx context.Context, speaker *repo.Speaker, person *repo.Person,
	logs []*repo.PersonChangeLog, dismissFn func(context.Context, ids.ID) error) ([]CascadePrompt, error) {

	if person == nil {
		return nil, nil
	}

	// owner 人物永不处理（既不自动删，也不提示）
	if person.IsOwner {
		return nil, nil
	}

	// 未编辑过的 llm 人物 → 自动级联 dismiss
	if shouldCascadePerson(person, logs) {
		if dismissFn != nil {
			if err := dismissFn(ctx, person.ID); err != nil {
				return nil, err
			}
		}
		person.Status = "dismissed" // 生产由 dismissFn 落库；此处同步内存态供调用方/测试判定
		return nil, nil
	}

	// 编辑过 / 手动创建的人物 → 不自动删，返回确认提示
	reason := "该人物被手工编辑过"
	if person.Source == "manual" {
		reason = "该人物为手动创建"
	}
	return []CascadePrompt{buildCascadePrompt(person, reason)}, nil
}
