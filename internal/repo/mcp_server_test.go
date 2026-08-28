package repo_test

import (
	"context"
	"encoding/json"
	"testing"

	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

func mcpRepo(t *testing.T) *repo.MCPServerRepo {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &repo.MCPServerRepo{DB: db}
}

func TestMCPServerCRUD(t *testing.T) {
	r := mcpRepo(t)
	ctx := context.Background()

	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ServerKey != "zhiwei" || !list[0].Builtin {
		t.Fatalf("初始应只有内置 zhiwei: %+v", list)
	}

	args := json.RawMessage(`["./echo.mjs"]`)
	m := &repo.MCPServer{
		ServerKey: "echo_srv", DisplayName: "回声", Transport: "stdio",
		Command: strptr("node"), Args: &args, Enabled: true,
	}
	if err := r.Create(ctx, m); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.ID.Int64() == 0 {
		t.Error("Create 应回填雪花 ID")
	}

	if err := r.SetEnabled(ctx, m.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, err := r.Get(ctx, m.ID)
	if err != nil || got.Enabled {
		t.Fatalf("SetEnabled(false) 未生效: %+v err=%v", got, err)
	}

	builtin := list[0]
	if err := r.Delete(ctx, builtin.ID); err != repo.ErrBuiltinProtected {
		t.Errorf("内置行删除应被拒: %v", err)
	}
	if err := r.SetEnabled(ctx, builtin.ID, false); err != repo.ErrBuiltinProtected {
		t.Errorf("内置行禁用应被拒: %v", err)
	}

	if err := r.Delete(ctx, m.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list2, _ := r.List(ctx)
	if len(list2) != 1 {
		t.Errorf("删后应只剩内置: %+v", list2)
	}
}

func strptr(s string) *string { return &s }
