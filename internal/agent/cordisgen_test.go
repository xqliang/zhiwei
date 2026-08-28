package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"zhiwei/internal/repo"
)

func TestGenerateCordis(t *testing.T) {
	base := "- id: mcp-zhiwei\n  name: '@deepseek-ai/dsh-mcp-client'\n"
	args := json.RawMessage(`["a.mjs","--flag"]`)
	url := "https://x.example/mcp"
	servers := []repo.MCPServer{
		{ServerKey: "zhiwei", Builtin: true, Enabled: true},
		{ServerKey: "echo_srv", Transport: "stdio", Command: strp("node"), Args: &args, Enabled: true},
		{ServerKey: "weather", Transport: "streamable-http", URL: &url, Enabled: true},
	}
	out, err := GenerateCordis(base, servers)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "id: mcp-zhiwei") {
		t.Error("应保留基模板内置块")
	}
	if !strings.Contains(out, "id: mcp-echo_srv") || !strings.Contains(out, "transport: stdio") {
		t.Errorf("缺 stdio 外部块: %s", out)
	}
	if !strings.Contains(out, "id: mcp-weather") || !strings.Contains(out, "url: https://x.example/mcp") {
		t.Errorf("缺 http 外部块: %s", out)
	}
	if strings.Count(out, "failOnStartupError: false") != 2 {
		t.Errorf("两个外部块都应 failOnStartupError: false: %s", out)
	}
	if strings.Count(out, "serverName: zhiwei") > 1 {
		t.Errorf("builtin 不应被重复生成为外部块: %s", out)
	}
}

func strp(s string) *string { return &s }
