package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// ErrBuiltinProtected 内置服务（zhiwei）不可删除/禁用时返回。
var ErrBuiltinProtected = errors.New("内置 MCP 服务不可删除或禁用")

// MCPServer 是一条全局 MCP 服务配置。args/env 为可空 JSON 列（stdio 用），用 *json.RawMessage
// 对齐 agent_message 的 ToolPayload（值类型扫描 NULL 会报错）。
type MCPServer struct {
	ID          ids.ID           `db:"id" json:"id"`
	ServerKey   string           `db:"server_key" json:"server_key"`
	DisplayName string           `db:"display_name" json:"display_name"`
	Transport   string           `db:"transport" json:"transport"`
	URL         *string          `db:"url" json:"url,omitempty"`
	Command     *string          `db:"command" json:"command,omitempty"`
	Args        *json.RawMessage `db:"args" json:"args,omitempty"`
	Env         *json.RawMessage `db:"env" json:"env,omitempty"`
	Enabled     bool             `db:"enabled" json:"enabled"`
	Builtin     bool             `db:"builtin" json:"builtin"`
	CreatedAt   time.Time        `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time        `db:"updated_at" json:"updated_at"`
}

type MCPServerRepo struct{ DB *sqlx.DB }

// List 返回全部服务，内置在前、其余按创建时间。
func (r *MCPServerRepo) List(ctx context.Context) ([]MCPServer, error) {
	var rows []MCPServer
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM mcp_server ORDER BY builtin DESC, created_at ASC`)
	return rows, err
}

// Enabled 返回启用中的服务（cordisgen / ApplyMCP 用）。
func (r *MCPServerRepo) Enabled(ctx context.Context) ([]MCPServer, error) {
	var rows []MCPServer
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM mcp_server WHERE enabled = 1 ORDER BY builtin DESC, created_at ASC`)
	return rows, err
}

// Get 按 id 查。
func (r *MCPServerRepo) Get(ctx context.Context, id ids.ID) (*MCPServer, error) {
	var m MCPServer
	err := r.DB.GetContext(ctx, &m, `SELECT * FROM mcp_server WHERE id = ?`, id.Int64())
	return &m, err
}

// Create 新增（雪花 ID）。ServerKey 唯一由 DB 约束保证；调用方须先做格式校验。
func (r *MCPServerRepo) Create(ctx context.Context, m *MCPServer) error {
	m.ID = ids.New()
	m.Builtin = false // 只有迁移能种内置行
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO mcp_server (id, server_key, display_name, transport, url, command, args, env, enabled, builtin)
VALUES (:id, :server_key, :display_name, :transport, :url, :command, :args, :env, :enabled, 0)`, m)
	return err
}

// Update 改可编辑字段（内置行只允许改 display_name，其余忽略）。不存在 → ErrNoRows。
func (r *MCPServerRepo) Update(ctx context.Context, m *MCPServer) error {
	cur, err := r.Get(ctx, m.ID)
	if err != nil {
		return err
	}
	if cur.Builtin {
		_, err := r.DB.ExecContext(ctx,
			`UPDATE mcp_server SET display_name = ? WHERE id = ?`, m.DisplayName, m.ID.Int64())
		return err
	}
	res, err := r.DB.NamedExecContext(ctx, `
UPDATE mcp_server SET server_key=:server_key, display_name=:display_name, transport=:transport,
  url=:url, command=:command, args=:args, env=:env, enabled=:enabled WHERE id=:id`, m)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetEnabled 启/禁。内置行禁用被拒（ErrBuiltinProtected）；不存在 → ErrNoRows。
func (r *MCPServerRepo) SetEnabled(ctx context.Context, id ids.ID, enabled bool) error {
	cur, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	if cur.Builtin && !enabled {
		return ErrBuiltinProtected
	}
	_, err = r.DB.ExecContext(ctx, `UPDATE mcp_server SET enabled = ? WHERE id = ?`, enabled, id.Int64())
	return err
}

// Delete 删除。内置行被拒（ErrBuiltinProtected）；不存在 → ErrNoRows。
func (r *MCPServerRepo) Delete(ctx context.Context, id ids.ID) error {
	cur, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	if cur.Builtin {
		return ErrBuiltinProtected
	}
	res, err := r.DB.ExecContext(ctx, `DELETE FROM mcp_server WHERE id = ?`, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
