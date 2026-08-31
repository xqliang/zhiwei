package agent

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// fakeTitleLLM 用于测试（占位，验证接口签名可实现）。
type fakeTitleLLM struct {
	out string
	err error
}

func (f *fakeTitleLLM) Chat(_ context.Context, _ titleChatReq) (string, error) {
	return f.out, f.err
}

// TestShouldCascadePerson 校验「是否级联删除人物」的判定。
func TestShouldCascadePerson(t *testing.T) {
	cases := []struct {
		name       string
		source     string // person source: 'llm' or 'manual'
		changeLogs []string
		want       bool
	}{
		{"未编辑过的 llm 人物", "llm", []string{"create"}, true},
		{"手动创建的人物", "manual", []string{"create"}, false},
		{"被编辑过的 llm 人物", "llm", []string{"create", "update"}, false},
		{"无变更记录的 llm 人物", "llm", []string{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			person := &repo.Person{Source: c.source, IsOwner: false}
			logs := make([]*repo.PersonChangeLog, len(c.changeLogs))
			for i, ct := range c.changeLogs {
				logs[i] = &repo.PersonChangeLog{ChangeType: ct, ChangedBy: "llm"}
			}
			got := shouldCascadePerson(person, logs)
			if got != c.want {
				t.Errorf("shouldCascadePerson() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestIsPersonEdited 校验「人物是否被编辑过」的判定。
func TestIsPersonEdited(t *testing.T) {
	cases := []struct {
		name string
		logs []*repo.PersonChangeLog
		want bool
	}{
		{"无变更记录", []*repo.PersonChangeLog{}, false},
		{"仅有 create", []*repo.PersonChangeLog{{ChangeType: "create", ChangedBy: "llm"}}, false},
		{"有 update", []*repo.PersonChangeLog{{ChangeType: "create", ChangedBy: "llm"}, {ChangeType: "update", ChangedBy: "llm"}}, true},
		{"有 user 的 update", []*repo.PersonChangeLog{{ChangeType: "update", ChangedBy: "user"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPersonEdited(c.logs); got != c.want {
				t.Errorf("isPersonEdited() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestBuildCascadePrompt 校验确认提示信息的构建。
func TestBuildCascadePrompt(t *testing.T) {
	person := &repo.Person{ID: ids.New(), DisplayName: "张三"}
	prompt := buildCascadePrompt(person, "该人物被手工编辑过")
	if prompt.PersonID != person.ID.String() {
		t.Errorf("PersonID = %q, want %q", prompt.PersonID, person.ID.String())
	}
	if prompt.Name != "张三" {
		t.Errorf("Name = %q, want 张三", prompt.Name)
	}
	if prompt.Reason != "该人物被手工编辑过" {
		t.Errorf("Reason = %q", prompt.Reason)
	}
}

// TestCascadeSpeaker 校验声纹→人物的级联处理主流程。
func TestCascadeSpeaker(t *testing.T) {
	// 未编辑过的 llm 人物 → 自动 dismiss
	conv := &repo.Speaker{ID: ids.New(), Name: "李四"}
	person := &repo.Person{ID: ids.New(), DisplayName: "李四", Source: "llm", IsOwner: false}
	logs := []*repo.PersonChangeLog{{ChangeType: "create", ChangedBy: "llm"}}

	prompts, err := CascadeSpeaker(context.Background(), conv, person, logs, nil)
	if err != nil {
		t.Fatalf("CascadeSpeaker: %v", err)
	}
	if len(prompts) != 0 {
		t.Errorf("未编辑人物应无 prompt, got %v", prompts)
	}
	if !personDismissed(person) {
		t.Error("未编辑人物应被 dismiss")
	}

	// 编辑过的人物 → 不 dismiss，返回 prompt
	person2 := &repo.Person{ID: ids.New(), DisplayName: "王五", Source: "llm", IsOwner: false}
	logs2 := []*repo.PersonChangeLog{{ChangeType: "create", ChangedBy: "llm"}, {ChangeType: "update", ChangedBy: "user"}}
	prompts2, err := CascadeSpeaker(context.Background(), conv, person2, logs2, nil)
	if err != nil {
		t.Fatalf("CascadeSpeaker: %v", err)
	}
	if len(prompts2) != 1 {
		t.Fatalf("编辑过人物应返回 1 个 prompt, got %d", len(prompts2))
	}
	if personDismissed(person2) {
		t.Error("编辑过人物不应被 dismiss")
	}

	// owner 人物 → 永不处理
	owner := &repo.Person{ID: ids.New(), DisplayName: "Owner", Source: "llm", IsOwner: true}
	prompts3, err := CascadeSpeaker(context.Background(), conv, owner, logs, nil)
	if err != nil {
		t.Fatalf("CascadeSpeaker: %v", err)
	}
	if len(prompts3) != 0 {
		t.Errorf("owner 人物应无 prompt, got %v", prompts3)
	}
	if personDismissed(owner) {
		t.Error("owner 人物不应被 dismiss")
	}
}
