// Package agent 把「读/写我的数据」的能力暴露成 MCP 工具，供 dsh agent 调用。
// MCP server 进程内运行、复用主服务的 repo（一个 DB 池），通过 streamable-http
// 挂在 chi /internal/mcp；dsh 边车用 mcp-client(streamable-http) 连回来。
package agent

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/repo"
	"zhiwei/internal/retrieve"
	"zhiwei/internal/search"
)

// MCPDeps 是工具依赖的仓储集合（主服务装配时注入已开库的实例）。
type MCPDeps struct {
	Memory     *repo.MemoryRepo
	Session    *repo.SessionRepo
	Transcript *repo.TranscriptRepo
	Topic      *repo.TopicRepo
	Todo       *repo.TodoRepo
	// Proposals 是「写-提议闸门」的提议仓储（P2d）。写工具（propose_*）只用它 Create
	// 一条 pending 提议，绝不直接改领域行；确认端点再在单事务内落库（见 mcp_write_tools.go
	// 与 proposals.go）。这是提示注入的根防线（spec §8）。
	Proposals *repo.AgentProposalRepo
	// ---- 画像（人物系统）读工具 + propose 读现值用（P2）----
	// 读工具 get_profile/get_person 与 propose_profile_* 只读这些 repo；propose_* 绝不写画像，
	// 只 Create pending 提议（写在 confirm 单事务里经 profile.Service Ext 变体落库）。全部限 owner「我」。
	Persons          *repo.PersonRepo
	PersonAttributes *repo.PersonAttributeRepo
	PersonEvents     *repo.PersonEventRepo
	// PersonMetrics 是画像第 5 平面（person_metric，时序个人指标）读工具 get_metrics 的依赖；
	// propose_profile_metric 同样只 Create pending 提议、绝不写此表，落库在 confirm 单事务里经
	// profile.Service.ManualAddMetricExt 完成（见 proposals.go 的 profile_metric case）。
	PersonMetrics *repo.PersonMetricRepo
	// Retrieve 语义检索（可选）：非 nil 且 query 非空时 search_memory 走「向量+关键词」混合，
	// 否则退回 Memory.Search 关键词。装配见 main.go（ARK_AUDIO_API_KEY 未配则 nil，降级）。
	Retrieve *retrieve.Retriever
	// ---- 联网搜索（Phase 2：web_search / web_fetch 工具）----
	// Search 联网搜索器；nil 则 web_search 工具报「未启用」。每次调用读 Configs 最新配置
	//（设置页改引擎/API key 热生效，不重启）。装配见 main.go。
	Search *search.Searcher
	// Fetch 网页抓取器（SSRF 安全拨号）；nil 则 web_fetch 工具报「未启用」。
	Fetch *search.Fetcher
	// Configs 全局 agent 配置（web_search 读搜索引擎/API key；与 handlers 的 AgentHandler.Configs 同一实例）。
	Configs *repo.AgentConfigRepo
}

// pingArgs：无参工具的入参（空 struct → object schema 无属性）。
type pingArgs struct{}

// NewMCPServer 构造 MCP server 并注册全部工具。
//
// userID 决定该 server 的全部工具读/写「谁」的数据：注入后透传给每个工具注册函数与 handler
// 工厂，替代早先写死的 toolUserID 常量（2B-A）。2B-B 起由 MCPHandler 按每用户 dsh token 懒建
// 一个 NewMCPServer(deps, uid)（见 mcpRouter），不同用户天然读/写各自的数据、彼此隔离。
func NewMCPServer(d MCPDeps, userID int64) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "zhiwei", Version: "0.1.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "zhiwei_ping",
		Description: "健康检查：无参，返回固定字符串，用于验证 MCP 连通性。",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ pingArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "pong-zhiwei"}},
		}, nil, nil
	})

	registerReadTools(s, d, userID)    // 只读工具（search_memory / get_timeline / get_topics / get_todos）
	registerWriteTools(s, d, userID)   // 写-提议工具（propose_*）：只建 pending 提议，绝不直接 mutate（§8）
	registerProfileTools(s, d, userID) // 画像读工具（get_profile/get_person）+ propose_profile_*（同样只建提议）
	return s
}

// mcpRouter 按「请求路径末段的 token → userID」为每个用户懒建并缓存一个 MCP server，实现
// 2B-B 的多用户隔离：不同 dsh 子进程带各自的 token 回连 /internal/mcp/{token}，被路由到各自
// userID 的 server，读/写的数据天然按 userID 隔离。
//
// 并发：servers 缓存受 mu 保护（懒建时可能多请求并发命中同一新 userID，双检后只建一个）。
// tokenUserID 由 RuntimePool.TokenUserID 提供（其内部自带锁）。
type mcpRouter struct {
	deps        MCPDeps
	tokenUserID func(token string) (int64, bool)
	// customize 在每个 per-user server 建好后调用，用于注册额外工具（如报告工具 review.RegisterReportTools）。
	customize func(*mcp.Server)

	mu      sync.Mutex
	servers map[int64]*mcp.Server // userID → 缓存的 MCP server（保证同用户会话连续、工具只注册一次）
}

// newMCPRouter 构造路由器（导出的 MCPHandler 与单测都用它；单测直接调 serverFor 断言 token 解析）。
func newMCPRouter(deps MCPDeps, tokenUserID func(string) (int64, bool), customize func(*mcp.Server)) *mcpRouter {
	return &mcpRouter{deps: deps, tokenUserID: tokenUserID, customize: customize, servers: map[int64]*mcp.Server{}}
}

// serverFor 从请求路径末段取 token 并解析出 userID，返回（懒建/缓存的）该用户的 MCP server；
// token 缺失或无法解析 → nil（go-sdk 的 streamable handler 据此返回 4xx，拒绝未知/伪造 token）。
func (m *mcpRouter) serverFor(r *http.Request) *mcp.Server {
	token := mcpTokenFromPath(r.URL.Path)
	if token == "" {
		return nil
	}
	uid, ok := m.tokenUserID(token)
	if !ok {
		return nil // 未知 token：不放行（隔离的根防线——伪造 token 拿不到任何 server）
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.servers[uid]; ok {
		return s
	}
	s := NewMCPServer(m.deps, uid)
	if m.customize != nil {
		m.customize(s)
	}
	m.servers[uid] = s
	return s
}

// mcpTokenFromPath 取路径最后一段作为 token（端点为 /internal/mcp/{token}）。无末段或以 "/"
// 结尾返回 ""；裸 /internal/mcp（无 token）会取到 "mcp"，它不在 byToken 里 → 解析失败 → nil。
func mcpTokenFromPath(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 || i == len(p)-1 {
		return ""
	}
	return p[i+1:]
}

// MCPHandler 把「按 token 分用户」的 MCP 路由包成 streamable-http 的 http.Handler（挂 chi 用）。
// tokenUserID 传 RuntimePool.TokenUserID；customize 传注册报告工具等的闭包（每个 per-user server 建时调用）。
func MCPHandler(deps MCPDeps, tokenUserID func(string) (int64, bool), customize func(*mcp.Server)) http.Handler {
	router := newMCPRouter(deps, tokenUserID, customize)
	return mcp.NewStreamableHTTPHandler(router.serverFor, nil)
}
