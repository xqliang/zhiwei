package agent

// MCP 服务管理端点（/api/agent/mcp）：全局清单的增删改查启禁。写操作成功后经 OnMCPChange
// 触发生效（重生成 cordis.generated.yml + 对在用运行时 mcp/apply 热插拔，见 main.go 装配）。
// 内置 zhiwei 服务受双重保护：repo 层 ErrBuiltinProtected + 这里的 403 映射。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// serverKeyRe 约束 server_key：cordis serverName 命名空间（mcp__<key>__*）要求合法标识符；
// 1..64 位字母数字下划线，与 mcp_server 表 VARCHAR(64) 对齐。
var serverKeyRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// mcpBody 是新增/编辑的请求体（可空字段用指针区分「未填」与「空串」）。
type mcpBody struct {
	ServerKey   string           `json:"server_key"`
	DisplayName string           `json:"display_name"`
	Transport   string           `json:"transport"`
	URL         *string          `json:"url"`
	Command     *string          `json:"command"`
	Args        *json.RawMessage `json:"args"`
	Env         *json.RawMessage `json:"env"`
	Enabled     bool             `json:"enabled"`
}

// validate 校验请求体：server_key 格式 + 非保留字；transport 合法且必填字段齐全。
func (b *mcpBody) validate() error {
	if !serverKeyRe.MatchString(b.ServerKey) {
		return errors.New("server_key 需匹配 ^[A-Za-z0-9_]{1,64}$")
	}
	if b.ServerKey == "zhiwei" {
		return errors.New("server_key 'zhiwei' 为内置保留")
	}
	switch b.Transport {
	case "streamable-http":
		if b.URL == nil || strings.TrimSpace(*b.URL) == "" {
			return errors.New("streamable-http 需 url")
		}
	case "stdio":
		if b.Command == nil || strings.TrimSpace(*b.Command) == "" {
			return errors.New("stdio 需 command")
		}
	default:
		return errors.New("transport 只支持 streamable-http|stdio")
	}
	return nil
}

// toModel 转成 repo 模型（ID 由调用方补充）。
func (b *mcpBody) toModel() *repo.MCPServer {
	return &repo.MCPServer{
		ServerKey: b.ServerKey, DisplayName: b.DisplayName, Transport: b.Transport,
		URL: b.URL, Command: b.Command, Args: b.Args, Env: b.Env, Enabled: b.Enabled,
	}
}

// mcpAvailable 检查管理面装配；未装配（MCPServers=nil）回 503。
func (h *AgentHandler) mcpAvailable(w http.ResponseWriter) bool {
	if h.MCPServers == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP 管理不可用"})
		return false
	}
	return true
}

// fireMCPChange 触发生效回调（nil 时只落库，等下次进程重启读新配置）。
func (h *AgentHandler) fireMCPChange(ctx context.Context) {
	if h.OnMCPChange != nil {
		h.OnMCPChange(ctx)
	}
}

// listMCP 返回全部服务（内置在前）。只读，不触发生效。
func (h *AgentHandler) listMCP(w http.ResponseWriter, r *http.Request) {
	if !h.mcpAvailable(w) {
		return
	}
	rows, err := h.MCPServers.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": rows})
}

// createMCP 新增服务。校验通过 → 落库 → 触发生效 → 回显新建行。
func (h *AgentHandler) createMCP(w http.ResponseWriter, r *http.Request) {
	if !h.mcpAvailable(w) {
		return
	}
	var b mcpBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := b.validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	m := b.toModel()
	if err := h.MCPServers.Create(r.Context(), m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.fireMCPChange(r.Context())
	writeJSON(w, http.StatusOK, m)
}

// updateMCP 编辑服务。内置行仅允许改 display_name（repo 层强制）；越权/不存在 → 404。
func (h *AgentHandler) updateMCP(w http.ResponseWriter, r *http.Request) {
	if !h.mcpAvailable(w) {
		return
	}
	id, ok := parseMCPID(w, r)
	if !ok {
		return
	}
	var b mcpBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := b.validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	m := b.toModel()
	m.ID = id
	if err := h.MCPServers.Update(r.Context(), m); err != nil {
		writeMCPRepoErr(w, err)
		return
	}
	h.fireMCPChange(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// patchMCP 启/禁服务（body: {"enabled": bool}）。内置禁用被拒；不存在的 id → 404。
func (h *AgentHandler) patchMCP(w http.ResponseWriter, r *http.Request) {
	if !h.mcpAvailable(w) {
		return
	}
	id, ok := parseMCPID(w, r)
	if !ok {
		return
	}
	var b struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := h.MCPServers.SetEnabled(r.Context(), id, b.Enabled); err != nil {
		writeMCPRepoErr(w, err)
		return
	}
	h.fireMCPChange(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// deleteMCP 删除服务。内置拒删；不存在的 id → 404。
func (h *AgentHandler) deleteMCP(w http.ResponseWriter, r *http.Request) {
	if !h.mcpAvailable(w) {
		return
	}
	id, ok := parseMCPID(w, r)
	if !ok {
		return
	}
	if err := h.MCPServers.Delete(r.Context(), id); err != nil {
		writeMCPRepoErr(w, err)
		return
	}
	h.fireMCPChange(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// parseMCPID 解析 URL 段 {id} 为 ids.ID；非法 → 400。
func parseMCPID(w http.ResponseWriter, r *http.Request) (ids.ID, bool) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return ids.ID(0), false
	}
	return id, true
}

// writeMCPRepoErr 把 repo 错误映射到 HTTP 状态码：内置保护→403、不存在→404、其余→500。
func writeMCPRepoErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repo.ErrBuiltinProtected):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}
