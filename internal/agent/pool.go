package agent

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"zhiwei/internal/auth"
)

// RuntimePool 管理「每登录用户一个 dsh 运行时 + 一个 MCP token」，是 2B-B 多用户隔离的核心：
//
//   - 每个 userID 惰性获得一个独立的 AgentRuntime（dsh 子进程）与一个随机 MCP token；
//     token 派生出该用户专属的 MCP 回连地址 mcpBaseURL+"/"+token，dsh 经它读/写「自己」的数据
//     （MCPHandler 按 token→userID 解析出该用户的 MCP server，见 mcp_server.go）。
//   - SessionRoot 也按 userID 派生（base/u<uid>），避免不同用户的 dsh 会话日志互相踩踏。
//   - 进程数上限 cap（ZW_AGENT_MAX_USERS）：超出时按 LRU 关停并移除最久未用的用户运行时
//     （连同其 token），防止用户数增长把机器进程/句柄耗尽。
//
// 并发模型：所有导出方法都在 mu 保护下操作三张表（runtimes/byToken/lru）。makeRT 只构造结构体
// （NewDSHRuntime 惰性 spawn，真正起进程在首次 Prompt），故 Get 持锁期间不做 I/O、很快返回。
// 唯一的例外是 LRU 回收时对被淘汰运行时的 Close()——dsh 的 Close 会做 shutdown RPC + kill（有界
// 5s），此处仍在锁内完成（保证「关停 + 从表中移除」原子，杜绝与并发 Get 的竞态）；回收是罕见路径
// （仅当在线用户数超过 cap 时才触发），可接受这一短暂持锁。测试用的 FakeRuntime.Close 是瞬时的。
type RuntimePool struct {
	mu sync.Mutex

	// base 是运行时配置模板：CordisConfig/Model/SystemPrompt 全用户共享；MCPURL 与 SessionRoot
	// 每用户从它派生（base.MCPURL 被忽略——改用 mcpBaseURL+token；base.SessionRoot 作为父目录）。
	base RuntimeConfig
	// mcpBaseURL 是 MCP 端点基址（如 http://127.0.0.1:8080/internal/mcp）；每用户地址 = 它 + "/" + token。
	mcpBaseURL string
	// cap 是同时在线的用户运行时上限；<=0 时构造函数会纠正为 1。
	cap int
	// makeRT 构造一个运行时（生产 = NewDSHRuntime；测试注入 fake 工厂以计数 Close/隔离进程）。
	makeRT func(RuntimeConfig) AgentRuntime

	runtimes map[int64]*poolEntry // userID → 运行时槽位
	byToken  map[string]int64     // MCP token → userID（供 MCPHandler 反查）
	lru      []int64              // 使用顺序：队首最久未用、队尾最近使用；回收从队首取
	// fallbackSeq 仅在 crypto/rand（auth.NewToken）极端失败时用于兜底生成进程内唯一 token。
	fallbackSeq int64
}

// poolEntry 是池内一个用户的运行时槽位（运行时 + 其 MCP token）。
type poolEntry struct {
	rt    AgentRuntime
	token string
}

// NewRuntimePool 构造运行时池。capN<=0 会被纠正为 1（至少容纳一个用户，避免除零/空池）。
// makeRT 生产传 func(c RuntimeConfig) AgentRuntime { return NewDSHRuntime(c) }。
func NewRuntimePool(base RuntimeConfig, mcpBaseURL string, capN int, makeRT func(RuntimeConfig) AgentRuntime) *RuntimePool {
	if capN <= 0 {
		capN = 1
	}
	return &RuntimePool{
		base:       base,
		mcpBaseURL: mcpBaseURL,
		cap:        capN,
		makeRT:     makeRT,
		runtimes:   map[int64]*poolEntry{},
		byToken:    map[string]int64{},
	}
}

// Get 返回 userID 的运行时：已存在则更新 LRU 后返回；不存在则铸 token、派生每用户配置、makeRT 建
// 运行时并登记，随后若超 cap 触发 LRU 回收（关停 + 移除最久未用者，连同其 token）。全程持 mu。
func (p *RuntimePool) Get(userID int64) AgentRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.runtimes[userID]; ok {
		p.touchLocked(userID)
		return e.rt
	}
	token := p.mintTokenLocked()
	cfg := p.base
	cfg.MCPURL = p.mcpBaseURL + "/" + token
	cfg.SessionRoot = p.deriveSessionRoot(userID)
	rt := p.makeRT(cfg)
	p.runtimes[userID] = &poolEntry{rt: rt, token: token}
	p.byToken[token] = userID
	p.lru = append(p.lru, userID) // 新建即最近使用（队尾）
	p.evictLocked()               // 超 cap 才回收；新建者在队尾，绝不会被本次回收误杀
	return rt
}

// TokenUserID 把 MCP token 反解析成 userID（供 MCPHandler 按请求路径末段的 token 选 per-user server）。
func (p *RuntimePool) TokenUserID(token string) (int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	uid, ok := p.byToken[token]
	return uid, ok
}

// Close 关停全部运行时并清空表（进程退出时 defer 调用）。返回首个 Close 错误（其余仍尽力关停）。
func (p *RuntimePool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var firstErr error
	for uid, e := range p.runtimes {
		if err := e.rt.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(p.runtimes, uid)
	}
	p.byToken = map[string]int64{}
	p.lru = nil
	return firstErr
}

// touchLocked 把 userID 移到 LRU 队尾（标记为最近使用）。调用者须持 mu。
func (p *RuntimePool) touchLocked(userID int64) {
	for i, id := range p.lru {
		if id == userID {
			p.lru = append(p.lru[:i], p.lru[i+1:]...)
			break
		}
	}
	p.lru = append(p.lru, userID)
}

// evictLocked 在超 cap 时从 LRU 队首起关停并移除最久未用的运行时（连同其 token），直到不超 cap。
// 调用者须持 mu。Close 在锁内完成，保证「关停 + 移除」对并发 Get 原子（见类型注释的权衡）。
func (p *RuntimePool) evictLocked() {
	for len(p.runtimes) > p.cap && len(p.lru) > 0 {
		oldest := p.lru[0]
		p.lru = p.lru[1:]
		e, ok := p.runtimes[oldest]
		if !ok {
			continue // lru 与 runtimes 理论恒一致；防御性跳过悬挂项
		}
		_ = e.rt.Close()
		delete(p.runtimes, oldest)
		delete(p.byToken, e.token)
	}
}

// deriveSessionRoot 为 userID 派生独立会话根目录（base/u<uid>）；base 为空则返回空（dsh 用默认）。
func (p *RuntimePool) deriveSessionRoot(userID int64) string {
	if p.base.SessionRoot == "" {
		return ""
	}
	return filepath.Join(p.base.SessionRoot, fmt.Sprintf("u%d", userID))
}

// mintTokenLocked 铸一个未使用过的 MCP token。用 auth.NewToken（crypto/rand，256-bit，碰撞不可能）；
// 极端下 crypto/rand 失败时回退到「进程内单调序号 + 纳秒」保证唯一——token 只作 loopback 端点的
// 路由键（非跨进程秘密的唯一来源），兜底可接受。调用者须持 mu（读写 byToken/fallbackSeq）。
func (p *RuntimePool) mintTokenLocked() string {
	if tok, err := auth.NewToken(); err == nil && tok != "" {
		if _, exists := p.byToken[tok]; !exists {
			return tok
		}
	}
	p.fallbackSeq++
	return fmt.Sprintf("fallback-%d-%d", p.fallbackSeq, time.Now().UnixNano())
}
