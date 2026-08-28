package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"zhiwei/internal/repo"
)

// GenerateCordis 生成完整 cordis 配置文本：base（cordis.agent.yml 原文，含内置 mcp-zhiwei 与所有
// !!js env 替换）后追加每个「启用且非 builtin」服务的 dsh-mcp-client 块。外部块统一
// failOnStartupError:false —— 一个坏服务被跳过、不拖垮 agent boot（内置块 fail:true 由 base 提供）。
func GenerateCordis(base string, servers []repo.MCPServer) (string, error) {
	var b strings.Builder
	b.WriteString(strings.TrimRight(base, "\n"))
	b.WriteString("\n")
	for _, s := range servers {
		if s.Builtin || !s.Enabled {
			continue
		}
		blk, err := mcpClientBlock(s)
		if err != nil {
			return "", fmt.Errorf("生成 %s 配置块: %w", s.ServerKey, err)
		}
		b.WriteString("\n")
		b.WriteString(blk)
	}
	return b.String(), nil
}

// mcpClientBlock 生成单个 dsh-mcp-client 列表块（YAML）。字面量写值（外部服务全局同构，
// 无需 !!js env 替换）。stdio 的 args/env 从 JSON 列还原为 YAML。
func mcpClientBlock(s repo.MCPServer) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "- id: mcp-%s\n", s.ServerKey)
	b.WriteString("  name: '@deepseek-ai/dsh-mcp-client'\n")
	b.WriteString("  config:\n")
	fmt.Fprintf(&b, "    transport: %s\n", s.Transport)
	fmt.Fprintf(&b, "    serverName: %s\n", s.ServerKey)
	switch s.Transport {
	case "streamable-http":
		if s.URL == nil || strings.TrimSpace(*s.URL) == "" {
			return "", fmt.Errorf("streamable-http 缺 url")
		}
		fmt.Fprintf(&b, "    url: %s\n", *s.URL)
	case "stdio":
		if s.Command == nil || strings.TrimSpace(*s.Command) == "" {
			return "", fmt.Errorf("stdio 缺 command")
		}
		fmt.Fprintf(&b, "    command: %s\n", *s.Command)
		if s.Args != nil {
			var args []string
			if err := json.Unmarshal(*s.Args, &args); err != nil {
				return "", fmt.Errorf("args 非字符串数组: %w", err)
			}
			if len(args) > 0 {
				b.WriteString("    args:\n")
				for _, a := range args {
					fmt.Fprintf(&b, "      - %s\n", yamlScalar(a))
				}
			}
		}
		if s.Env != nil {
			var env map[string]string
			if err := json.Unmarshal(*s.Env, &env); err != nil {
				return "", fmt.Errorf("env 非字符串对象: %w", err)
			}
			if len(env) > 0 {
				b.WriteString("    env:\n")
				for k, v := range env {
					fmt.Fprintf(&b, "      %s: %s\n", k, yamlScalar(v))
				}
			}
		}
	default:
		return "", fmt.Errorf("未知 transport: %s", s.Transport)
	}
	b.WriteString("    failOnStartupError: false\n")
	return b.String(), nil
}

// yamlScalar 用 JSON 引号安全转义标量（JSON 字符串是 YAML 双引号标量的合法子集），
// 避免特殊字符/空格破坏 YAML。
func yamlScalar(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

// SpecsFromServers 把「启用且非 builtin」的 repo 行转成下发给 dsh mcp/apply 的 MCPServerSpec
// 列表（cordisgen 生成文件与热插拔下发共用同一来源，保证两者一致）。args/env JSON 列解析失败
// 时跳过该字段（Create 端点已校验过形状，这里是防御性兜底，不让一个坏行打断整体生效）。
func SpecsFromServers(rows []repo.MCPServer) []MCPServerSpec {
	out := make([]MCPServerSpec, 0, len(rows))
	for _, s := range rows {
		if s.Builtin {
			continue // 内置 zhiwei 不下发：它的连接由基模板/env 提供
		}
		spec := MCPServerSpec{ServerName: s.ServerKey, Transport: s.Transport}
		if s.URL != nil {
			spec.URL = *s.URL
		}
		if s.Command != nil {
			spec.Command = *s.Command
		}
		if s.Args != nil {
			_ = json.Unmarshal(*s.Args, &spec.Args)
		}
		if s.Env != nil {
			_ = json.Unmarshal(*s.Env, &spec.Env)
		}
		out = append(out, spec)
	}
	return out
}
