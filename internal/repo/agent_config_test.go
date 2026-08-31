package repo

import (
	"context"
	"testing"

	"zhiwei/internal/repotest"
)

// TestAgentConfigRepo 验证人设配置单例：未配置读到空；Upsert 后读回；再 Upsert 为更新（仍单行）。
// Phase 2 起含搜索列（search_engine/search_api_key）roundtrip：空引擎归一 auto、空 key 存 NULL。
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
	if c.Identity != "" || c.Soul != "" || c.SearchEngine != "" || c.SearchAPIKey != nil {
		t.Fatalf("未配置应为空, got %+v", c)
	}

	if err := r.Upsert(ctx, AgentConfig{Identity: "我是知微", Soul: "温柔简洁不废话"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	c, _ = r.Get(ctx)
	if c.Identity != "我是知微" || c.Soul != "温柔简洁不废话" {
		t.Fatalf("读回不符: %+v", c)
	}
	if c.SearchEngine != "auto" { // 空 engine 归一为 auto（默认免 key 链）
		t.Fatalf("未给引擎应归一 auto, got %q", c.SearchEngine)
	}
	// 空 key 存 NULL，持久化行的 NULL → *string 扫描路径读回 nil（SearchKey 免判空取空串）。
	if c.SearchAPIKey != nil || c.SearchKey() != "" {
		t.Fatalf("空 key 应存 NULL 读回 nil, got %v", c.SearchAPIKey)
	}

	// 再 Upsert = 更新（不新增行）；搜索列一并写入读回。
	if err := r.Upsert(ctx, AgentConfig{
		Identity: "我是知微v2", Soul: "毒舌",
		SearchEngine: "tavily", SearchAPIKey: strPtr("tvly-test"),
	}); err != nil {
		t.Fatalf("Upsert 更新: %v", err)
	}
	c, _ = r.Get(ctx)
	if c.Identity != "我是知微v2" || c.Soul != "毒舌" {
		t.Fatalf("更新后不符: %+v", c)
	}
	if c.SearchEngine != "tavily" || c.SearchKey() != "tvly-test" {
		t.Fatalf("搜索列读回不符: engine=%q key=%v", c.SearchEngine, c.SearchAPIKey)
	}
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM agent_config"); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应恒为单行, got %d", n)
	}
}

// strPtr 测试辅助：取字符串指针。
func strPtr(s string) *string { return &s }
