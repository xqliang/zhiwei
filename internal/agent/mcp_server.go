// Package agent 把「读/写我的数据」的能力暴露成 MCP 工具，供 dsh agent 调用。
// MCP server 进程内运行、复用主服务的 repo（一个 DB 池），通过 streamable-http
// 挂在 chi /internal/mcp；dsh 边车用 mcp-client(streamable-http) 连回来。
package agent

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/repo"
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
}

// pingArgs：无参工具的入参（空 struct → object schema 无属性）。
type pingArgs struct{}

// NewMCPServer 构造 MCP server 并注册全部工具。
func NewMCPServer(d MCPDeps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "zhiwei", Version: "0.1.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "zhiwei_ping",
		Description: "健康检查：无参，返回固定字符串，用于验证 MCP 连通性。",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ pingArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "pong-zhiwei"}},
		}, nil, nil
	})

	registerReadTools(s, d)  // 只读工具（search_memory / get_timeline / get_topics / get_todos）
	registerWriteTools(s, d) // 写-提议工具（propose_*）：只建 pending 提议，绝不直接 mutate（§8）
	return s
}

// MCPHandler 把 server 包成 streamable-http 的 http.Handler（挂 chi 用）。
func MCPHandler(s *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
}
