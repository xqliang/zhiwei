package repo_test

import (
	"context"
	"testing"

	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

func TestAgentSkillCRUD(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_skill WHERE name LIKE 'test-%'") })
	r := &repo.AgentSkillRepo{DB: db}
	ctx := context.Background()

	s := &repo.AgentSkill{
		Name: "test-git-commit", DisplayName: "Git Commit", Source: "github/awesome-copilot/git-commit",
		Description: "提交规范", Content: "---\nname: test-git-commit\n---\n正文",
	}
	if err := r.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID.Int64() == 0 {
		t.Error("Create 应回填 ID")
	}

	got, err := r.Get(ctx, s.ID)
	if err != nil || got.Name != "test-git-commit" || !got.Enabled {
		t.Fatalf("Get: %+v err=%v", got, err)
	}

	if err := r.SetEnabled(ctx, s.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, _ = r.Get(ctx, s.ID)
	if got.Enabled {
		t.Error("SetEnabled(false) 未生效")
	}

	if err := r.Delete(ctx, s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, s.ID); err == nil {
		t.Error("删除后 Get 应 ErrNoRows")
	}
}
