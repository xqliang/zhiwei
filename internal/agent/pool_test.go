package agent

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestPool 构造一个用 FakeRuntime 作工厂的池（不 spawn 真进程）。makeRT 把 pool 派生的每用户
// 配置塞进 FakeRuntime.Cfg，便于断言 MCPURL/SessionRoot 派生。
func newTestPool(base RuntimeConfig, mcpBaseURL string, capN int) *RuntimePool {
	return NewRuntimePool(base, mcpBaseURL, capN, func(c RuntimeConfig) AgentRuntime {
		return &FakeRuntime{Cfg: c}
	})
}

// TestRuntimePoolPerUserIsolation：不同用户得到不同运行时 + 各自独立 token；同用户复用同一实例；
// token→userID 反查正确、未知 token 不命中；每用户配置从模板派生（共享字段透传，MCPURL/SessionRoot 各自派生）。
func TestRuntimePoolPerUserIsolation(t *testing.T) {
	base := RuntimeConfig{CordisConfig: "cordis.yml", Model: "m1", SessionRoot: "/sroot", SystemPrompt: "sp"}
	p := newTestPool(base, "http://127.0.0.1:8080/internal/mcp", 8)
	defer p.Close()

	rt1 := p.Get(1)
	rt2 := p.Get(2)
	if rt1 == rt2 {
		t.Fatal("不同用户应得到不同运行时")
	}
	if p.Get(1) != rt1 {
		t.Fatal("同用户再取应复用同一运行时实例")
	}

	tok1, tok2 := p.runtimes[1].token, p.runtimes[2].token
	if tok1 == "" || tok2 == "" || tok1 == tok2 {
		t.Fatalf("每用户 token 应非空且互异: %q %q", tok1, tok2)
	}
	if uid, ok := p.TokenUserID(tok1); !ok || uid != 1 {
		t.Errorf("token1 应反查到 user1: uid=%d ok=%v", uid, ok)
	}
	if uid, ok := p.TokenUserID(tok2); !ok || uid != 2 {
		t.Errorf("token2 应反查到 user2: uid=%d ok=%v", uid, ok)
	}
	if _, ok := p.TokenUserID("no-such-token"); ok {
		t.Error("未知 token 不应命中")
	}

	// 每用户派生配置：MCPURL = base + "/" + token；SessionRoot = base/u<uid>；模板字段透传。
	c1 := rt1.(*FakeRuntime).Cfg
	if c1.MCPURL != "http://127.0.0.1:8080/internal/mcp/"+tok1 {
		t.Errorf("user1 MCPURL 派生错误: %q", c1.MCPURL)
	}
	if !strings.HasSuffix(c1.SessionRoot, "u1") {
		t.Errorf("user1 SessionRoot 应派生为 .../u1: %q", c1.SessionRoot)
	}
	if c1.Model != "m1" || c1.CordisConfig != "cordis.yml" || c1.SystemPrompt != "sp" {
		t.Errorf("共享模板字段应透传: %+v", c1)
	}
	// 两用户的 MCPURL 必须不同（token 不同），这是 MCP 侧隔离的基础。
	c2 := rt2.(*FakeRuntime).Cfg
	if c1.MCPURL == c2.MCPURL {
		t.Errorf("两用户 MCPURL 不应相同: %q", c1.MCPURL)
	}
}

// TestRuntimePoolLRUEvict：超 cap 时按 LRU 关停并移除最久未用者（连同 token）；最近使用者不被淘汰。
func TestRuntimePoolLRUEvict(t *testing.T) {
	p := newTestPool(RuntimeConfig{SessionRoot: "/r"}, "http://x/internal/mcp", 2)
	defer p.Close()

	rt1 := p.Get(1).(*FakeRuntime)
	tok1 := p.runtimes[1].token
	_ = p.Get(2)
	_ = p.Get(3) // 超 cap(2)：最久未用的 user1 应被淘汰

	if rt1.Closed != 1 {
		t.Errorf("被淘汰运行时应恰 Close 一次, got %d", rt1.Closed)
	}
	if _, ok := p.runtimes[1]; ok {
		t.Error("user1 应已从池中移除")
	}
	if _, ok := p.TokenUserID(tok1); ok {
		t.Error("被淘汰用户的 token 应一并从反查表清除")
	}
	if len(p.runtimes) != 2 {
		t.Errorf("池应维持在 cap=2, got %d", len(p.runtimes))
	}

	// LRU 语义：先 Get(2) 把它标记为最近使用，再新增 user4 → 应淘汰 user3（而非刚用过的 user2）。
	rt3 := p.runtimes[3].rt.(*FakeRuntime)
	_ = p.Get(2) // touch user2（移到队尾）
	_ = p.Get(4) // 超 cap：淘汰队首（user3）
	if rt3.Closed != 1 {
		t.Errorf("user3 应被淘汰并 Close 一次, got %d", rt3.Closed)
	}
	if _, ok := p.runtimes[2]; !ok {
		t.Error("最近使用的 user2 不应被淘汰")
	}
	if _, ok := p.runtimes[3]; ok {
		t.Error("user3 应已被淘汰")
	}
}

// TestRuntimePoolClose：Close 关停全部运行时（各一次）并清空表。
func TestRuntimePoolClose(t *testing.T) {
	p := newTestPool(RuntimeConfig{}, "http://x/internal/mcp", 8)
	a := p.Get(1).(*FakeRuntime)
	b := p.Get(2).(*FakeRuntime)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if a.Closed != 1 || b.Closed != 1 {
		t.Errorf("每个运行时应恰 Close 一次: a=%d b=%d", a.Closed, b.Closed)
	}
	if len(p.runtimes) != 0 || len(p.byToken) != 0 {
		t.Errorf("Close 后表应清空: runtimes=%d byToken=%d", len(p.runtimes), len(p.byToken))
	}
}

// TestMCPRouterTokenRouting：MCPHandler 的路由器按「路径末段 token → userID」懒建/缓存 per-user
// server；未知 token 或裸路径返回 nil（不放行）；customize 钩子对每个 server 恰调一次。
func TestMCPRouterTokenRouting(t *testing.T) {
	tokens := map[string]int64{"tok-alice": 11, "tok-bob": 22}
	resolver := func(tok string) (int64, bool) { uid, ok := tokens[tok]; return uid, ok }
	router := newMCPRouter(MCPDeps{}, resolver, nil)

	sa := router.serverFor(httptest.NewRequest("POST", "/internal/mcp/tok-alice", nil))
	if sa == nil {
		t.Fatal("已知 token 应返回非 nil server")
	}
	if router.serverFor(httptest.NewRequest("POST", "/internal/mcp/tok-alice", nil)) != sa {
		t.Error("同一用户应复用缓存的 server 实例（会话连续 + 工具只注册一次）")
	}
	sb := router.serverFor(httptest.NewRequest("POST", "/internal/mcp/tok-bob", nil))
	if sb == nil || sb == sa {
		t.Error("不同用户应得到不同 server")
	}
	// 未知 token → nil（伪造 token 拿不到任何 server，隔离根防线）。
	if router.serverFor(httptest.NewRequest("POST", "/internal/mcp/forged", nil)) != nil {
		t.Error("未知 token 应返回 nil")
	}
	// 裸路径（无 token 段）：末段 "mcp" 不是已知 token → nil。
	if router.serverFor(httptest.NewRequest("POST", "/internal/mcp", nil)) != nil {
		t.Error("裸路径应返回 nil")
	}

	// customize：每个新建 server 恰调一次；命中缓存不重复调。
	calls := 0
	r2 := newMCPRouter(MCPDeps{}, resolver, func(*mcp.Server) { calls++ })
	_ = r2.serverFor(httptest.NewRequest("POST", "/internal/mcp/tok-alice", nil))
	_ = r2.serverFor(httptest.NewRequest("POST", "/internal/mcp/tok-alice", nil)) // 缓存命中，不再 customize
	if calls != 1 {
		t.Errorf("customize 应对每个 server 恰调一次, got %d", calls)
	}
}
