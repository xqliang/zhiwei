package repo

import (
	"context"
	"testing"

	"zhiwei/internal/repotest"
)

// TestAgentConfigRepo 验证人设配置单例：未配置读到空；Upsert 后读回；再 Upsert 为更新（仍单行）。
func TestAgentConfigRepo(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &AgentConfigRepo{DB: db}
	// 隔离：清掉可能的历史单行，确保「未配置」初态。
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_config WHERE id = 1") })
	_, _ = db.Exec("DELETE FROM agent_config WHERE id = 1")

	c, err := r.Get(ctx)
	if err != nil {
		t.Fatalf("Get(空): %v", err)
	}
	if c.Identity != "" || c.Soul != "" {
		t.Fatalf("未配置应为空, got %+v", c)
	}

	if err := r.Upsert(ctx, "我是知微", "温柔简洁不废话"); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	c, _ = r.Get(ctx)
	if c.Identity != "我是知微" || c.Soul != "温柔简洁不废话" {
		t.Fatalf("读回不符: %+v", c)
	}

	// 再 Upsert = 更新（不新增行）。
	if err := r.Upsert(ctx, "我是知微v2", "毒舌"); err != nil {
		t.Fatalf("Upsert 更新: %v", err)
	}
	c, _ = r.Get(ctx)
	if c.Identity != "我是知微v2" || c.Soul != "毒舌" {
		t.Fatalf("更新后不符: %+v", c)
	}
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM agent_config"); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应恒为单行, got %d", n)
	}
}
