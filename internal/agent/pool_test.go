package agent

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestApplyMCPAllAndEvictIdle：ApplyMCPAll 把期望的外部 MCP 服务集下发到所有在用运行时；
// EvictIdle 关停空闲（无进行中轮次）运行时。用 FakeRuntime 断言，无需真 dsh。
// 注：FakeRuntime.Closed 是既有的 int 计数器（Close 调用次数），故用 == 0 断言未关停。
func TestApplyMCPAllAndEvictIdle(t *testing.T) {
	fake := &FakeRuntime{}
	pool := NewRuntimePool(RuntimeConfig{}, "http://x/mcp", 4, func(RuntimeConfig) AgentRuntime { return fake })
	pool.Get(7) // 建一个运行时
	specs := []MCPServerSpec{{ServerName: "echo_srv", Transport: "stdio", Command: "node", Args: []string{"e.mjs"}}}
	pool.ApplyMCPAll(context.Background(), specs)
	if len(fake.LastApplied) != 1 || fake.LastApplied[0].ServerName != "echo_srv" {
		t.Fatalf("ApplyMCPAll 未下发到运行时: %+v", fake.LastApplied)
	}
	pool.EvictIdle()
	if fake.Closed == 0 {
		t.Error("EvictIdle 应 Close 空闲运行时")
	}
}

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

// blockingCloseRuntime 是 Close 会阻塞（直到 release 被关闭）的运行时：进入 Close 时先关 started
// 发信号，再阻塞在 <-release。用于 I2 用例断言「回收时的 Close 不持池锁」。
type blockingCloseRuntime struct {
	FakeRuntime
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingCloseRuntime) Close() error {
	b.once.Do(func() { close(b.started) }) // 通知：已进入 Close
	<-b.release                            // 阻塞，模拟 dsh Close 的有界 5s shutdown
	return nil
}

// TestRuntimePoolEvictClosesOutsideLock 锁定 I2：LRU 回收被淘汰运行时时，Close 必须在【锁外】执行。
// 此前 evictLocked 在持 p.mu 期间调 e.rt.Close()（dsh 有界 5s），会阻塞所有并发 Get/TokenUserID。
// 本用例让被淘汰者的 Close 阻塞，断言阻塞期间并发 TokenUserID/Get 仍能立即返回（证明锁已释放）。
func TestRuntimePoolEvictClosesOutsideLock(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var relOnce sync.Once
	releaseClose := func() { relOnce.Do(func() { close(release) }) }
	defer releaseClose() // 兜底：即便断言提前失败也解除阻塞，避免泄漏 goroutine

	// cap=1：每新增一个用户都会淘汰旧用户。第一个运行时是阻塞 Close 的，其余用普通 Fake。
	made := 0 // makeRT 恒在 p.mu 下调用（Get 锁内），故普通计数无需额外同步
	p := NewRuntimePool(RuntimeConfig{}, "http://x/internal/mcp", 1, func(c RuntimeConfig) AgentRuntime {
		made++
		if made == 1 {
			return &blockingCloseRuntime{started: started, release: release}
		}
		return &FakeRuntime{Cfg: c}
	})

	_ = p.Get(1) // 建 user1（阻塞 Close 运行时）

	// 后台 Get(2)：超 cap → 淘汰 user1；新代码应「锁内摘表、锁外 Close」，故此调用会阻塞在锁外的 Close。
	getDone := make(chan struct{})
	go func() {
		_ = p.Get(2)
		close(getDone)
	}()

	// 等 user1 的 Close 真正开始
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("回收未触发被淘汰运行时的 Close")
	}

	// 关键断言：Close 阻塞期间，并发 TokenUserID + Get(命中缓存) 必须立即返回（锁未被 Close 占用）。
	done := make(chan struct{})
	go func() {
		_, _ = p.TokenUserID("no-such")
		_ = p.Get(2) // user2 已在表中，命中缓存路径
		close(done)
	}()
	select {
	case <-done: // 好：锁是空闲的
	case <-time.After(3 * time.Second):
		t.Fatal("Close 阻塞期间并发 TokenUserID/Get 被阻塞——Close 仍在持锁（I2 未修复）")
	}

	// 释放 Close，等待 Get(2) 收尾
	releaseClose()
	select {
	case <-getDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 释放后 Get(2) 未返回")
	}

	// user1 已被淘汰、池维持在 cap=1
	if _, ok := p.runtimes[1]; ok {
		t.Error("user1 应已被淘汰移除")
	}
	if len(p.runtimes) != 1 {
		t.Errorf("池应维持 cap=1, got %d", len(p.runtimes))
	}
}
