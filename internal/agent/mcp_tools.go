package agent

import "github.com/modelcontextprotocol/go-sdk/mcp"

// registerReadTools 占位实现（Task 1）：Task 3 会替换为 4 个只读工具
// （search_memory / get_timeline / get_topics / get_todos）的注册。
// 当前为空，使 NewMCPServer 仅暴露 zhiwei_ping，用于验证 streamable-http 互通。
func registerReadTools(*mcp.Server, MCPDeps) {}
