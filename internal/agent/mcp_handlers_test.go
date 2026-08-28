package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/repo"
)

// doMCP 便捷发请求：注册路由 → 发 → 返回 recorder。
func doMCP(h *AgentHandler, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Use(injectUser(1))
	RegisterAgent(r, h)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestMCPHandlersCRUD 锁定 /api/agent/mcp 端点：列表/新增（校验）/启禁/删除 + 内置保护 + 生效回调。
func TestMCPHandlersCRUD(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM mcp_server WHERE builtin = 0") })
	var changes int
	h := &AgentHandler{
		MCPServers:  &repo.MCPServerRepo{DB: db},
		OnMCPChange: func(context.Context) { changes++ },
	}

	// 初始列表：只有内置 zhiwei
	rec := doMCP(h, "GET", "/api/agent/mcp", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"server_key":"zhiwei"`) {
		t.Errorf("列表应含内置 zhiwei: %s", rec.Body.String())
	}
	if changes != 0 {
		t.Error("GET 不应触发 OnMCPChange")
	}

	// 新增合法 stdio 服务
	rec = doMCP(h, "POST", "/api/agent/mcp",
		`{"server_key":"echo_srv","display_name":"回声","transport":"stdio","command":"node","args":["e.mjs"],"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body.String())
	}
	if changes != 1 {
		t.Errorf("新增应触发 OnMCPChange, got %d", changes)
	}
	var created repo.MCPServer
	if err := jsonDecode(rec.Body.String(), &created); err != nil {
		t.Fatalf("resp 解析: %v", err)
	}
	if created.ServerKey != "echo_srv" || created.Command == nil || *created.Command != "node" {
		t.Errorf("新增回显异常: %+v", created)
	}

	// 非法 server_key 被拒（400，且不触发生效）
	rec = doMCP(h, "POST", "/api/agent/mcp", `{"server_key":"bad key!","transport":"stdio","command":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 key 应 400, got %d", rec.Code)
	}
	// 保留字 zhiwei 被拒
	rec = doMCP(h, "POST", "/api/agent/mcp", `{"server_key":"zhiwei","transport":"stdio","command":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("保留字 zhiwei 应 400, got %d", rec.Code)
	}
	// streamable-http 缺 url 被拒
	rec = doMCP(h, "POST", "/api/agent/mcp", `{"server_key":"nohost","transport":"streamable-http"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("http 缺 url 应 400, got %d", rec.Code)
	}
	// 未知 transport 被拒
	rec = doMCP(h, "POST", "/api/agent/mcp", `{"server_key":"badtr","transport":"grpc","command":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知 transport 应 400, got %d", rec.Code)
	}

	// 启禁外部服务（PATCH）
	rec = doMCP(h, "PATCH", "/api/agent/mcp/"+created.ID.String(), `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 内置禁用被拒（403）
	rec = doMCP(h, "PATCH", "/api/agent/mcp/1", `{"enabled":false}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("内置禁用应 403, got %d", rec.Code)
	}

	// 删除外部服务
	rec = doMCP(h, "DELETE", "/api/agent/mcp/"+created.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 内置删除被拒（403）
	rec = doMCP(h, "DELETE", "/api/agent/mcp/1", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("内置删除应 403, got %d", rec.Code)
	}

	// 不存在的 id → 404
	rec = doMCP(h, "DELETE", "/api/agent/mcp/999999", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在应 404, got %d", rec.Code)
	}
}

// TestMCPHandlersUnavailable：MCPServers 为 nil 时端点 503（管理面未装配的降级）。
func TestMCPHandlersUnavailable(t *testing.T) {
	h := &AgentHandler{} // 无 MCPServers
	rec := doMCP(h, "GET", "/api/agent/mcp", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("无 MCPServers 应 503, got %d", rec.Code)
	}
}

func jsonDecode(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}
