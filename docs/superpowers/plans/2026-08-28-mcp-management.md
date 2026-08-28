# MCP 服务管理（一期）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让开发者在设置页手动增删改查启禁全局 MCP 服务，并**进程内热插拔**真正影响 dsh 运行时（热插拔失败才降级 respawn）。

**Architecture:** 新表 `mcp_server`（全局）→ `/api/agent/mcp` REST → 每次写操作 (1) `cordisgen` 重写 `cordis.generated.yml`（给将来新 spawn 的 dsh）(2) 对在用 dsh 运行时下发 `mcp/apply`（给 `dsh-sdk-jsonrpc-server` 打补丁新增的 JSON-RPC 方法，用 `ctx.plugin(McpClient,cfg)`/`fork.dispose()` 热插拔）。内置 `zhiwei` 服务不可删禁；外部服务 `failOnStartupError:false`。

**Tech Stack:** Go + chi + sqlx + golang-migrate（MySQL/utf8mb4），Vue 3 CDN 前端，dsh 边车（cordis 插件框架，Node，patch-package）。

**规格来源：** `docs/superpowers/specs/2026-08-28-mcp-management-design.md`
**分支：** `feat/agent-mcp-manage`（已在 worktree）。
**测试库约定：** 集成测试用 `repotest.DSN`（按包隔离 `zhiwei_test_<pkg>`）；本 worktree 手动调试用临时库，勿动共享 `zhiwei`（见项目备忘 db-per-feature-convention）。运行集成测试：`make init-testdb` 后 `TEST_MYSQL_DSN=... go test ...`（DSN 见 Makefile `test-integration`）。

---

## File Structure

- Create `migrations/000021_mcp_server.up.sql` / `.down.sql` — 表 + 内置行。
- Create `internal/repo/mcp_server.go` — `MCPServer` 结构 + `MCPServerRepo`（List/Get/Create/Update/SetEnabled/Delete，builtin 保护）。
- Create `internal/repo/mcp_server_test.go` — repo 集成测试。
- Create `internal/agent/cordisgen.go` — 从 `[]repo.MCPServer` 生成 cordis 配置文本（基模板 + 外部块）。
- Create `internal/agent/cordisgen_test.go` — 纯函数单测。
- Create `internal/agent/mcp_handlers.go` — `/api/agent/mcp` 5+1 端点 + 触发生效。
- Create `internal/agent/mcp_handlers_test.go` — handler 测试。
- Modify `internal/agent/runtime.go` — 加 `ApplyMCP`；`AgentRuntime` 接口加方法；`FakeRuntime` 实现。
- Modify `internal/agent/pool.go` — 加 `ApplyMCPAll` / `EvictIdle`。
- Modify `internal/agent/handlers.go` — `RegisterAgent` 挂 MCP 路由（或在 mcp_handlers.go 内注册）。
- Modify `services/agent-sidecar/patches/` — 新增 `@deepseek-ai__dsh-sdk-jsonrpc-server@0.1.1-rc.2.patch` 追加 `mcp/apply`（现有 patch 若同名则合并编辑）。
- Modify `cmd/zhiwei-server/main.go` — 启动时首次 cordisgen + 注入生成路径；把 pool/repo 传给 MCP handler。
- Modify `internal/config/config.go` — 生成文件路径（默认 `services/agent-sidecar/cordis.generated.yml`）。
- Modify `.gitignore` — 忽略 `services/agent-sidecar/cordis.generated.yml`。
- Modify `web/index.html` + `web/app.js` — 设置页「MCP 服务」子区。

---

## Task 1: 迁移 `000021_mcp_server`

**Files:**
- Create: `migrations/000021_mcp_server.up.sql`
- Create: `migrations/000021_mcp_server.down.sql`

- [ ] **Step 1: 写 up 迁移**

`migrations/000021_mcp_server.up.sql`:
```sql
-- 全局 MCP 服务清单：dsh agent 连接的外部/内置 MCP 服务。启禁/增删经 /api/agent/mcp 管理，
-- 生效走 cordisgen 重写配置 + 运行时 mcp/apply 热插拔（见 spec 2026-08-28-mcp-management）。
-- id 用雪花 ID（应用层 ids.New 生成，与 agent_conversation 一致）；内置 zhiwei 行固定 id=1。
CREATE TABLE mcp_server (
  id           BIGINT UNSIGNED NOT NULL,
  server_key   VARCHAR(64)  NOT NULL,          -- cordis serverName；命名空间 mcp__<key>__*
  display_name VARCHAR(128) NOT NULL,
  transport    VARCHAR(32)  NOT NULL,          -- 'streamable-http' | 'stdio'
  url          TEXT NULL,
  command      VARCHAR(255) NULL,
  args         JSON NULL,
  env          JSON NULL,
  enabled      TINYINT(1) NOT NULL DEFAULT 1,
  builtin      TINYINT(1) NOT NULL DEFAULT 0,
  created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_mcp_server_key (server_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 内置知微工具：固定 id=1，不可删/禁；其地址是 per-user 的（ZW_AGENT_MCP_URL），故 url 留空，
-- cordisgen 对 builtin 不生成外部块（内置块由基模板 cordis.agent.yml 提供）。
INSERT INTO mcp_server (id, server_key, display_name, transport, url, enabled, builtin)
VALUES (1, 'zhiwei', '知微内置工具', 'streamable-http', '', 1, 1);
```

- [ ] **Step 2: 写 down 迁移**

`migrations/000021_mcp_server.down.sql`:
```sql
DROP TABLE IF EXISTS mcp_server;
```

- [ ] **Step 3: 迁移可用性验证（在隔离测试库）**

Run: `make init-testdb`
Expected: 迁移全部 up 成功，日志出现 `19/u agent_config`…`21/u mcp_server`（无 dirty）。

- [ ] **Step 4: Commit**

```bash
git add migrations/000021_mcp_server.up.sql migrations/000021_mcp_server.down.sql
git commit -m "feat(mcp): 000021 mcp_server 表 + 内置 zhiwei 行"
```

---

## Task 2: `MCPServerRepo`

**Files:**
- Create: `internal/repo/mcp_server.go`
- Test: `internal/repo/mcp_server_test.go`

- [ ] **Step 1: 写失败测试**

`internal/repo/mcp_server_test.go`:
```go
package repo_test

import (
	"context"
	"encoding/json"
	"testing"

	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

func mcpRepo(t *testing.T) *repo.MCPServerRepo {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &repo.MCPServerRepo{DB: db}
}

func TestMCPServerCRUD(t *testing.T) {
	r := mcpRepo(t)
	ctx := context.Background()

	// 初始应只有内置 zhiwei（迁移种入）
	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ServerKey != "zhiwei" || !list[0].Builtin {
		t.Fatalf("初始应只有内置 zhiwei: %+v", list)
	}

	// 新增一个 stdio 外部服务
	args := json.RawMessage(`["./echo.mjs"]`)
	m := &repo.MCPServer{
		ServerKey: "echo_srv", DisplayName: "回声", Transport: "stdio",
		Command: strptr("node"), Args: &args, Enabled: true,
	}
	if err := r.Create(ctx, m); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.ID.Int64() == 0 {
		t.Error("Create 应回填雪花 ID")
	}

	// SetEnabled false
	if err := r.SetEnabled(ctx, m.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, err := r.Get(ctx, m.ID)
	if err != nil || got.Enabled {
		t.Fatalf("SetEnabled(false) 未生效: %+v err=%v", got, err)
	}

	// 内置行不可删 / 不可禁
	builtin := list[0]
	if err := r.Delete(ctx, builtin.ID); err != repo.ErrBuiltinProtected {
		t.Errorf("内置行删除应被拒: %v", err)
	}
	if err := r.SetEnabled(ctx, builtin.ID, false); err != repo.ErrBuiltinProtected {
		t.Errorf("内置行禁用应被拒: %v", err)
	}

	// 删除外部服务
	if err := r.Delete(ctx, m.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list2, _ := r.List(ctx)
	if len(list2) != 1 {
		t.Errorf("删后应只剩内置: %+v", list2)
	}
}

func strptr(s string) *string { return &s }
```

- [ ] **Step 2: 运行验证失败**

Run: `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestMCPServerCRUD -v`
Expected: 编译失败 `undefined: repo.MCPServer`。

- [ ] **Step 3: 实现 repo**

`internal/repo/mcp_server.go`:
```go
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
```

- [ ] **Step 4: 运行验证通过**

Run: 同 Step 2 命令。
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/mcp_server.go internal/repo/mcp_server_test.go
git commit -m "feat(mcp): MCPServerRepo（CRUD + builtin 保护）"
```

---

## Task 3: `cordisgen`（DB → cordis 配置文本）

**Files:**
- Create: `internal/agent/cordisgen.go`
- Test: `internal/agent/cordisgen_test.go`

**说明：** 以 `cordis.agent.yml` 为基模板**原样保留**（含内置 mcp-zhiwei 与所有 `!!js` env 替换），把每个 `enabled 且非 builtin` 的服务追加成 `dsh-mcp-client` 列表块，外部块统一 `failOnStartupError: false`。

- [ ] **Step 1: 写失败测试**

`internal/agent/cordisgen_test.go`:
```go
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
		{ServerKey: "zhiwei", Builtin: true, Enabled: true},                 // builtin：不生成外部块
		{ServerKey: "echo_srv", Transport: "stdio", Command: strp("node"), Args: &args, Enabled: true},
		{ServerKey: "weather", Transport: "streamable-http", URL: &url, Enabled: true},
	}
	out, err := GenerateCordis(base, servers)
	if err != nil {
		t.Fatal(err)
	}
	// 基模板保留
	if !strings.Contains(out, "id: mcp-zhiwei") {
		t.Error("应保留基模板内置块")
	}
	// 外部块：stdio + http 各一，含 failOnStartupError: false
	if !strings.Contains(out, "id: mcp-echo_srv") || !strings.Contains(out, "transport: stdio") {
		t.Errorf("缺 stdio 外部块: %s", out)
	}
	if !strings.Contains(out, "id: mcp-weather") || !strings.Contains(out, "url: https://x.example/mcp") {
		t.Errorf("缺 http 外部块: %s", out)
	}
	if strings.Count(out, "failOnStartupError: false") != 2 {
		t.Errorf("两个外部块都应 failOnStartupError: false: %s", out)
	}
	// builtin zhiwei 不该被当外部块重复生成
	if strings.Contains(out, "id: mcp-zhiwei\n  name: '@deepseek-ai/dsh-mcp-client'\n  config:\n    transport") &&
		strings.Count(out, "serverName: zhiwei") > 1 {
		t.Errorf("builtin 不应被重复生成为外部块: %s", out)
	}
}

func strp(s string) *string { return &s }
```

- [ ] **Step 2: 运行验证失败**

Run: `go test ./internal/agent/ -run TestGenerateCordis -v`
Expected: 编译失败 `undefined: GenerateCordis`。

- [ ] **Step 3: 实现 cordisgen**

`internal/agent/cordisgen.go`:
```go
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
```

- [ ] **Step 4: 运行验证通过**

Run: `go test ./internal/agent/ -run TestGenerateCordis -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/agent/cordisgen.go internal/agent/cordisgen_test.go
git commit -m "feat(mcp): cordisgen — DB 服务列表生成 cordis 配置文本"
```

---

## Task 4: dsh 边车补丁 — `mcp/apply` 热插拔 RPC（含 spike 验证）

**Files:**
- Modify/Create: `services/agent-sidecar/patches/@deepseek-ai__dsh-sdk-jsonrpc-server@0.1.1-rc.2.patch`

**依据（已核实）：** `HarnessSdkJsonRpcServer` 构造存 `this.ctx`，已用 `await this.ctx.plugin(LlmDeepSeek,{})` 且返回 fork 用 `.dispose()`（`node_modules/@deepseek-ai/dsh-sdk-jsonrpc-server/lib/index.js:43,97,178`）。补丁与现有 `session/cancel` 补丁同构。

- [ ] **Step 1: 手改 lib（先直接改 node_modules 验证，再用 patch-package 固化）**

在 `node_modules/@deepseek-ai/dsh-sdk-jsonrpc-server/lib/index.js` 顶部 import 区加：
```js
import * as McpClient from "@deepseek-ai/dsh-mcp-client";
```
在 `HarnessSdkJsonRpcServer` 类内新增方法（紧邻 `cancel` 之后）：
```js
/**
 * Hot-swap the set of external MCP clients without restarting the host.
 * params: { servers: [{ serverName, transport, url?, command?, args?, env? }] }.
 * Adds newly-listed servers via ctx.plugin(McpClient,cfg), disposes removed
 * forks, and re-creates changed ones. Idempotent; returns per-server results.
 */
async applyMcp(params) {
	if (this._mcpForks === void 0) this._mcpForks = new Map(); // serverName -> { fork, sig }
	const servers = Array.isArray(params?.servers) ? params.servers : [];
	const desired = new Map(servers.map((s) => [s.serverName, s]));
	const results = [];
	// 移除：期望集里没有的
	for (const [name, entry] of [...this._mcpForks]) {
		if (!desired.has(name)) {
			try { entry.fork.dispose(); } catch {}
			this._mcpForks.delete(name);
		}
	}
	// 新增/变更
	for (const [name, cfg] of desired) {
		const sig = JSON.stringify(cfg);
		const existing = this._mcpForks.get(name);
		if (existing && existing.sig === sig) { results.push({ serverName: name, ok: true, unchanged: true }); continue; }
		if (existing) { try { existing.fork.dispose(); } catch {} this._mcpForks.delete(name); }
		try {
			const fork = await this.ctx.plugin(McpClient, cfg);
			this._mcpForks.set(name, { fork, sig });
			results.push({ serverName: name, ok: true });
		} catch (e) {
			results.push({ serverName: name, ok: false, error: String(e && e.message || e) });
		}
	}
	return { ok: results.every((r) => r.ok), results };
}
```
在 dispatch `switch (method)` 增加：
```js
case "mcp/apply": return this.applyMcp(params);
```

- [ ] **Step 2: Spike 验证「热加载后下一轮工具可见」**

手动脚本（在 `services/agent-sidecar/`）：起 dsh（`node node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/bin.js`，`DSH_CORDIS_CONFIG=$PWD/cordis.agent.yml`），依次发 JSON-RPC：`initialize` → `mcp/apply {servers:[{serverName:"echo_srv",transport:"stdio",command:"node",args:["spike/mcp-echo-server.mjs"]}]}` → `session/prompt` 一个会触发列出工具/调用 echo 的提问。
Run（人工观察）：断言该轮事件流出现 `mcp__echo_srv__*` 工具可用（tool_call 或工具清单）。
Expected: 可见 → 热插拔达标，继续 Step 3。**若不可见** → 记录到规格 §12，改用「ApplyMCP 后对该运行时开新 dsh session」或降级 respawn（Task 5 的 EvictIdle 已提供兜底路径），并在 Task 5 的 `ApplyMCP` 里标记需要 respawn。

- [ ] **Step 3: 用 patch-package 固化补丁**

Run: `cd services/agent-sidecar && npx patch-package @deepseek-ai/dsh-sdk-jsonrpc-server`
Expected: 生成/更新 `patches/@deepseek-ai__dsh-sdk-jsonrpc-server@0.1.1-rc.2.patch`，包含上面新增。确认 `package.json` 的 `postinstall`/patch-package 流程会应用它（现有 patch 已证明该流程存在）。

- [ ] **Step 4: Commit**

```bash
git add services/agent-sidecar/patches/
git commit -m "feat(mcp): dsh 补丁新增 mcp/apply 运行时热插拔 MCP 客户端"
```

---

## Task 5: 运行时下发 — `ApplyMCP` / `ApplyMCPAll` / `EvictIdle`

**Files:**
- Modify: `internal/agent/runtime.go`（`AgentRuntime` 接口 + `dshRuntime.ApplyMCP`）
- Modify: `internal/agent/pool.go`（`ApplyMCPAll` / `EvictIdle`）
- Modify: `internal/agent/<fake 运行时所在测试文件>`（`FakeRuntime.ApplyMCP`）
- Test: `internal/agent/pool_test.go`（新增或追加）

**MCPServerSpec：** 定义下发给 dsh 的最小结构（不含 builtin）。

- [ ] **Step 1: 写失败测试（pool 下发 + 计数）**

`internal/agent/pool_test.go`（追加）:
```go
func TestApplyMCPAllAndEvictIdle(t *testing.T) {
	var applied [][]MCPServerSpec
	mk := func(c RuntimeConfig) AgentRuntime {
		return &FakeRuntime{Script: [][]Event{{}}}
	}
	_ = mk
	fake := &FakeRuntime{}
	pool := NewRuntimePool(RuntimeConfig{}, "http://x/mcp", 4, func(RuntimeConfig) AgentRuntime { return fake })
	pool.Get(7) // 建一个运行时
	specs := []MCPServerSpec{{ServerName: "echo_srv", Transport: "stdio", Command: "node", Args: []string{"e.mjs"}}}
	pool.ApplyMCPAll(context.Background(), specs)
	applied = append(applied, fake.LastApplied)
	if len(fake.LastApplied) != 1 || fake.LastApplied[0].ServerName != "echo_srv" {
		t.Fatalf("ApplyMCPAll 未下发到运行时: %+v", fake.LastApplied)
	}
	// EvictIdle：无活跃轮次 → Close 掉
	pool.EvictIdle()
	if !fake.Closed {
		t.Error("EvictIdle 应 Close 空闲运行时")
	}
}
```
（`FakeRuntime` 需加字段 `LastApplied []MCPServerSpec`、`Closed bool` 与 `ApplyMCP`/`Close` 记录；`IsIdle()` 返回 true。）

- [ ] **Step 2: 运行验证失败**

Run: `go test ./internal/agent/ -run TestApplyMCPAllAndEvictIdle -v`
Expected: 编译失败 `undefined: MCPServerSpec` / `ApplyMCPAll`。

- [ ] **Step 3: 实现**

`internal/agent/runtime.go` — 加类型与接口方法，并在 `dshRuntime` 实现：
```go
// MCPServerSpec 是下发给 dsh mcp/apply 的单个外部 MCP 服务（不含 builtin）。
type MCPServerSpec struct {
	ServerName string            `json:"serverName"`
	Transport  string            `json:"transport"`
	URL        string            `json:"url,omitempty"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// ApplyMCP 向已启动的 dsh 下发期望的外部 MCP 服务集（热插拔）。未启动则跳过（下次 spawn 由
// cordis.generated.yml 生效）。返回 dsh 侧错误（调用方据此决定是否降级 respawn）。
func (r *dshRuntime) ApplyMCP(ctx context.Context, servers []MCPServerSpec) error {
	r.startMu.Lock()
	started := r.started
	r.startMu.Unlock()
	if !started {
		return nil // 惰性：还没起进程，新配置将在首次 spawn 时由生成文件带上
	}
	_, err := r.call(ctx, "mcp/apply", map[string]any{"servers": servers})
	return err
}
```
在 `AgentRuntime` 接口（同文件）增加：
```go
	ApplyMCP(ctx context.Context, servers []MCPServerSpec) error
	IsIdle() bool // 无进行中轮次（EvictIdle 用）
```
`dshRuntime.IsIdle`：
```go
func (r *dshRuntime) IsIdle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns) == 0
}
```

`internal/agent/pool.go` — 追加：
```go
// ApplyMCPAll 向所有在用运行时下发期望的外部 MCP 服务集（热插拔）。逐个下发；某运行时报错则
// 对其 EvictIdle 兜底（下一轮用 cordis.generated.yml respawn）。收集运行时引用在锁外调用，避免
// 持锁做 I/O（对齐 Get/evictLocked 的 I2 约束）。
func (p *RuntimePool) ApplyMCPAll(ctx context.Context, servers []MCPServerSpec) {
	p.mu.Lock()
	rts := make([]AgentRuntime, 0, len(p.runtimes))
	for _, e := range p.runtimes {
		rts = append(rts, e.rt)
	}
	p.mu.Unlock()
	for _, rt := range rts {
		if err := rt.ApplyMCP(ctx, servers); err != nil {
			// 热插拔失败 → 该运行时空闲时踢掉，下一轮 respawn 读新配置
			p.evictRuntimeIfIdle(rt)
		}
	}
}

// EvictIdle 关停所有空闲（无进行中轮次）运行时；下一轮 Prompt 惰性 respawn。锁内摘表、锁外 Close
//（对齐 I2）。有活跃轮次的运行时保留，避免打断。
func (p *RuntimePool) EvictIdle() {
	p.mu.Lock()
	var victims []AgentRuntime
	for uid, e := range p.runtimes {
		if e.rt.IsIdle() {
			victims = append(victims, e.rt)
			delete(p.runtimes, uid)
			delete(p.byToken, e.token)
			p.removeLRULocked(uid)
		}
	}
	p.mu.Unlock()
	closeAll(victims)
}

func (p *RuntimePool) evictRuntimeIfIdle(target AgentRuntime) {
	p.mu.Lock()
	var victim AgentRuntime
	for uid, e := range p.runtimes {
		if e.rt == target && e.rt.IsIdle() {
			victim = e.rt
			delete(p.runtimes, uid)
			delete(p.byToken, e.token)
			p.removeLRULocked(uid)
			break
		}
	}
	p.mu.Unlock()
	if victim != nil {
		_ = victim.Close()
	}
}

// removeLRULocked 从 lru 切片移除 uid（调用者持 mu）。
func (p *RuntimePool) removeLRULocked(uid int64) {
	for i, id := range p.lru {
		if id == uid {
			p.lru = append(p.lru[:i], p.lru[i+1:]...)
			return
		}
	}
}
```

`FakeRuntime`（测试双）新增字段与方法：`LastApplied []MCPServerSpec`、`Closed bool`；`ApplyMCP` 记录 `LastApplied`、返回 nil；`IsIdle()` 返回 `!f.busy`（默认 true）；`Close()` 置 `Closed=true`。

- [ ] **Step 4: 运行验证通过**

Run: `go test ./internal/agent/ -run 'TestApplyMCPAllAndEvictIdle|TestOrchestrator|TestDateTimeHead'`
Expected: PASS（含既有用例——接口新增方法后 FakeRuntime 已实现，编译通过）。

- [ ] **Step 5: Commit**

```bash
git add internal/agent/runtime.go internal/agent/pool.go internal/agent/pool_test.go
git commit -m "feat(mcp): 运行时 ApplyMCP + pool ApplyMCPAll/EvictIdle（热插拔+兜底）"
```

---

## Task 6: REST API `/api/agent/mcp`

**Files:**
- Create: `internal/agent/mcp_handlers.go`
- Modify: `internal/agent/handlers.go`（`AgentHandler` 加 `MCPServers *repo.MCPServerRepo` + 生效回调 `OnMCPChange func(context.Context)`；`RegisterAgent` 挂路由）
- Test: `internal/agent/mcp_handlers_test.go`

**server_key 校验：** `^[A-Za-z0-9_]+$`、长度 1..64、非保留字 `zhiwei`。

- [ ] **Step 1: 写失败测试**

`internal/agent/mcp_handlers_test.go`:
```go
package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/repo"
)

func TestMCPHandlersCRUD(t *testing.T) {
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM mcp_server WHERE builtin = 0") })
	var changes int
	h := &AgentHandler{
		MCPServers:  &repo.MCPServerRepo{DB: db},
		OnMCPChange: func(ctxx context.Context) { changes++ },
	}
	r := chi.NewRouter()
	r.Use(injectUser(1))
	RegisterAgent(r, h)

	// 新增合法
	rec := do(r, "POST", "/api/agent/mcp",
		`{"server_key":"echo_srv","display_name":"回声","transport":"stdio","command":"node","args":["e.mjs"],"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body.String())
	}
	if changes == 0 {
		t.Error("新增应触发 OnMCPChange")
	}

	// 非法 server_key 被拒
	rec = do(r, "POST", "/api/agent/mcp", `{"server_key":"bad key!","transport":"stdio","command":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 key 应 400, got %d", rec.Code)
	}

	// 列表含内置 + 新增
	rec = do(r, "GET", "/api/agent/mcp", "")
	if !strings.Contains(rec.Body.String(), "zhiwei") || !strings.Contains(rec.Body.String(), "echo_srv") {
		t.Errorf("列表应含内置与新增: %s", rec.Body.String())
	}
}

func do(r chi.Router, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}
```
（`injectUser` 测试中间件已存在于 handlers_test.go；`orchDSN` 同。补 `import "context"`。）

- [ ] **Step 2: 运行验证失败**

Run: `make init-testdb && TEST_MYSQL_DSN=... go test ./internal/agent/ -run TestMCPHandlersCRUD -v`
Expected: 编译失败 `h.MCPServers undefined` / `OnMCPChange undefined`。

- [ ] **Step 3: 实现 handler + 接线**

`internal/agent/handlers.go` — `AgentHandler` 加字段：
```go
	MCPServers  *repo.MCPServerRepo    // MCP 服务清单（全局）；nil 时 MCP 端点 503
	OnMCPChange func(ctx context.Context) // 写操作成功后触发（重生成 cordis + ApplyMCPAll）；nil 时不触发
```
`RegisterAgent` 内追加：
```go
	r.Get("/api/agent/mcp", h.listMCP)
	r.Post("/api/agent/mcp", h.createMCP)
	r.Put("/api/agent/mcp/{id}", h.updateMCP)
	r.Patch("/api/agent/mcp/{id}", h.patchMCP)
	r.Delete("/api/agent/mcp/{id}", h.deleteMCP)
```

`internal/agent/mcp_handlers.go`:
```go
package agent

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

var serverKeyRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

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

// validate 校验 server_key/transport/必填字段。
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

func (b *mcpBody) toModel() *repo.MCPServer {
	return &repo.MCPServer{
		ServerKey: b.ServerKey, DisplayName: b.DisplayName, Transport: b.Transport,
		URL: b.URL, Command: b.Command, Args: b.Args, Env: b.Env, Enabled: b.Enabled,
	}
}

func (h *AgentHandler) mcpAvailable(w http.ResponseWriter) bool {
	if h.MCPServers == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP 管理不可用"})
		return false
	}
	return true
}

func (h *AgentHandler) fireMCPChange(ctx context.Context) {
	if h.OnMCPChange != nil {
		h.OnMCPChange(ctx)
	}
}

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

func (h *AgentHandler) updateMCP(w http.ResponseWriter, r *http.Request) {
	if !h.mcpAvailable(w) {
		return
	}
	id, ok := parseID(w, r)
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
		mcpErr(w, err)
		return
	}
	h.fireMCPChange(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AgentHandler) patchMCP(w http.ResponseWriter, r *http.Request) {
	if !h.mcpAvailable(w) {
		return
	}
	id, ok := parseID(w, r)
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
		mcpErr(w, err)
		return
	}
	h.fireMCPChange(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AgentHandler) deleteMCP(w http.ResponseWriter, r *http.Request) {
	if !h.mcpAvailable(w) {
		return
	}
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.MCPServers.Delete(r.Context(), id); err != nil {
		mcpErr(w, err)
		return
	}
	h.fireMCPChange(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func parseID(w http.ResponseWriter, r *http.Request) (ids.ID, bool) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return ids.ID(0), false
	}
	return id, true
}

func mcpErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repo.ErrBuiltinProtected):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}
```
（`ids.ParseID(s string) (ids.ID, error)` 已确认；`ids.ID` 是 `type ID int64`，零值用 `ids.ID(0)`。参考 `internal/api/memory.go:99` 的 chi URL id 解析写法。）

- [ ] **Step 4: 运行验证通过**

Run: `make init-testdb && TEST_MYSQL_DSN=... go test ./internal/agent/ -run TestMCPHandlersCRUD -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/agent/mcp_handlers.go internal/agent/handlers.go internal/agent/mcp_handlers_test.go
git commit -m "feat(mcp): /api/agent/mcp 增删改查启禁 + 校验 + 生效回调"
```

---

## Task 7: 主装配接线（启动生成 + 变更生效）

**Files:**
- Modify: `internal/config/config.go`（生成文件路径字段，默认 sidecar 目录下 `cordis.generated.yml`）
- Modify: `cmd/zhiwei-server/main.go`（启动首生成 + 指向生成路径 + 组装 `OnMCPChange`）
- Modify: `.gitignore`

- [ ] **Step 1: 配置字段**

`internal/config/config.go`：加字段 `CordisGenerated`（env `ZW_AGENT_CORDIS_GENERATED`，默认与 `CordisConfig` 同目录的 `cordis.generated.yml`）。参照现有 `DSHSystemPrompt` 的 `getenv` 默认写法。

- [ ] **Step 2: 启动生成 + 接线（无独立测试，靠 e2e/构建）**

`cmd/zhiwei-server/main.go`（在 agentPool/handler 组装处，约 322-397）：
```go
mcpRepo := &repo.MCPServerRepo{DB: db}

// regenCordis：读基模板 + 启用服务 → 写生成文件（给将来新 spawn 的 dsh）。
baseCordis, err := os.ReadFile(cfg.CordisConfig)
if err != nil {
	log.Fatalf("读 cordis 基模板: %v", err)
}
regenCordis := func(ctx context.Context) error {
	servers, err := mcpRepo.Enabled(ctx)
	if err != nil {
		return err
	}
	out, err := agent.GenerateCordis(string(baseCordis), servers)
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.CordisGenerated, []byte(out), 0o644)
}
if err := regenCordis(context.Background()); err != nil {
	log.Fatalf("初次生成 cordis: %v", err)
}
// 让新 spawn 的 dsh 读生成文件
poolBase.CordisConfig = cfg.CordisGenerated

// 变更生效：重生成（新进程）+ 热插拔下发（在用进程）
onMCPChange := func(ctx context.Context) {
	if err := regenCordis(ctx); err != nil {
		log.Printf("重生成 cordis 失败: %v", err)
		return
	}
	specs, err := agent.SpecsFromServers(ctx, mcpRepo)
	if err != nil {
		log.Printf("读 MCP 服务失败: %v", err)
		return
	}
	agentPool.ApplyMCPAll(ctx, specs)
}
```
把 `MCPServers: mcpRepo, OnMCPChange: onMCPChange` 加入 `AgentHandler` 字面量。

新增辅助 `internal/agent/cordisgen.go` 里：
```go
// SpecsFromServers 把「启用且非 builtin」的 repo 行转成下发给 dsh 的 MCPServerSpec 列表。
func SpecsFromServers(ctx context.Context, repoMCP *repo.MCPServerRepo) ([]MCPServerSpec, error) {
	rows, err := repoMCP.Enabled(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MCPServerSpec, 0, len(rows))
	for _, s := range rows {
		if s.Builtin {
			continue
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
	return out, nil
}
```

- [ ] **Step 3: gitignore 生成文件**

`.gitignore` 追加：
```
services/agent-sidecar/cordis.generated.yml
```

- [ ] **Step 4: 构建 + 全量 agent 测试**

Run: `go build ./... && go vet ./... && make init-testdb && TEST_MYSQL_DSN=... go test ./internal/agent/... ./internal/repo/...`
Expected: BUILD/VET OK，测试 PASS。

- [ ] **Step 5: Commit**

```bash
git add cmd/zhiwei-server/main.go internal/config/config.go internal/agent/cordisgen.go .gitignore
git commit -m "feat(mcp): 启动生成 cordis + 变更时重生成并热插拔下发"
```

---

## Task 8: 前端「MCP 服务」设置子区

**Files:**
- Modify: `web/index.html`（设置面板内加区块 + 表单）
- Modify: `web/app.js`（refs/加载/增删改启禁）

- [ ] **Step 1: app.js 状态与方法**

在 persona 区（约 `app.js:2812+`）之后加：
```js
    // ---------- 设置：MCP 服务（全局，手动管理；启禁/增删经 /api/agent/mcp，热插拔生效） ----------
    const mcpServers = ref([]);
    const mcpForm = ref({ server_key: '', display_name: '', transport: 'streamable-http', url: '', command: '', args: '' });
    const mcpErr = ref('');
    async function loadMCP() {
      try { const d = await api('GET', '/api/agent/mcp'); mcpServers.value = (d && d.servers) || []; }
      catch (e) { showError(e); }
    }
    async function addMCP() {
      mcpErr.value = '';
      const f = mcpForm.value;
      const body = { server_key: f.server_key.trim(), display_name: f.display_name.trim() || f.server_key.trim(), transport: f.transport, enabled: true };
      if (f.transport === 'streamable-http') body.url = f.url.trim();
      else { body.command = f.command.trim(); body.args = f.args.trim() ? f.args.split(/\s+/) : []; }
      try {
        await api('POST', '/api/agent/mcp', body);
        mcpForm.value = { server_key: '', display_name: '', transport: 'streamable-http', url: '', command: '', args: '' };
        await loadMCP();
      } catch (e) { mcpErr.value = (e && e.message) || String(e); }
    }
    async function toggleMCP(m) {
      try { await api('PATCH', '/api/agent/mcp/' + m.id, { enabled: !m.enabled }); await loadMCP(); }
      catch (e) { showError(e); }
    }
    async function deleteMCP(m) {
      if (m.builtin) return;
      try { await api('DELETE', '/api/agent/mcp/' + m.id); await loadMCP(); }
      catch (e) { showError(e); }
    }
```
在 `switchTab` 的 `settings` 分支追加 `loadMCP();`（与 `loadAgentConfig()` 并列）。
在 `createApp` 返回对象（约 `app.js:3085`）追加：`mcpServers, mcpForm, mcpErr, loadMCP, addMCP, toggleMCP, deleteMCP,`。

- [ ] **Step 2: index.html 区块**

在设置面板（约 `index.html:929-960` 之后、面板内）加：
```html
        <h3 style="margin-top:24px">MCP 服务</h3>
        <p class="muted">管理知微 agent 连接的 MCP 服务；启用/禁用即时热插拔生效。内置「知微内置工具」不可删禁。</p>
        <ul class="mcp-list">
          <li v-for="m in mcpServers" :key="m.id" class="mcp-item">
            <label><input type="checkbox" :checked="m.enabled" :disabled="m.builtin" @change="toggleMCP(m)"></label>
            <span class="mcp-name">{{ m.display_name }} <code>{{ m.server_key }}</code></span>
            <span class="muted">{{ m.transport }} · {{ m.url || m.command }}</span>
            <button v-if="!m.builtin" class="btn-sm" @click="deleteMCP(m)">删除</button>
            <span v-else class="muted">内置</span>
          </li>
        </ul>
        <div class="mcp-form">
          <input v-model="mcpForm.server_key" placeholder="server_key（字母数字下划线）">
          <input v-model="mcpForm.display_name" placeholder="显示名（可选）">
          <select v-model="mcpForm.transport">
            <option value="streamable-http">streamable-http</option>
            <option value="stdio">stdio</option>
          </select>
          <input v-if="mcpForm.transport==='streamable-http'" v-model="mcpForm.url" placeholder="url">
          <template v-else>
            <input v-model="mcpForm.command" placeholder="command，如 node">
            <input v-model="mcpForm.args" placeholder="args（空格分隔）">
          </template>
          <button class="btn" @click="addMCP">添加</button>
          <span class="err" v-if="mcpErr">{{ mcpErr }}</span>
        </div>
```
（样式类复用现有设计系统；若无对应类，加少量 CSS 到 `index.html` 的 `<style>`。）

- [ ] **Step 3: 手动验证（dev）**

Run: 起 dev（端口 8081），登录 → 设置 → MCP 服务：看到内置 zhiwei（开关禁用、无删除）；添加一个 stdio echo → 列表出现；切换启用/删除正常。

- [ ] **Step 4: Commit**

```bash
git add web/index.html web/app.js
git commit -m "feat(mcp): 设置页 MCP 服务子区（列表/添加/启禁/删除）"
```

---

## Task 9: 端到端验证

- [ ] **Step 1: 起 dev + 加外部 MCP**

准备一个 stdio echo MCP（复用 `services/agent-sidecar/spike/mcp-echo-server.mjs`）。设置页添加：transport=stdio, command=node, args=`spike/mcp-echo-server.mjs`, 启用。

- [ ] **Step 2: 验证热插拔生效**

在「问知微」发一条会用到 echo 工具的消息 → 确认 agent 能调用 `mcp__echo_srv__*`（无需手动重启进程）。禁用该服务 → 再发消息 → 确认不再可用。
Expected: 启用即用、禁用即失效；若热插拔未即时生效则下一轮 respawn 后生效（记录实际表现到规格 §12）。

- [ ] **Step 3: 回归**

Run: `go build ./... && go vet ./... && make init-testdb && TEST_MYSQL_DSN=... go test ./...`
Expected: 全绿。

- [ ] **Step 4: 收尾**

用 `superpowers:finishing-a-development-branch` 决定合并/PR/清理。合并回 main 前确认迁移号仍为 000021（若期间 main 新增迁移则顺延重排）。

---

## Self-Review 结果（对照规格）

- **规格覆盖：** 数据模型(§4)→T1/T2；cordisgen(§5)→T3；热插拔+兜底(§6)→T4/T5；API(§7)→T6；前端(§8)→T8；安全 failOnStartupError/内置保护/key 校验(§9)→T3/T2/T6；测试(§10)→各任务 + T9；二期(§11) 不在本计划。`/test` 试连（规格 §7 可选）**本计划未纳入**——留作后续小任务（可加 `POST /api/agent/mcp/{id}/test`），已在此显式标注避免"静默漏做"。
- **占位符扫描：** 无 TODO/含糊步骤；每步含具体代码或命令。少数「按实际 API 调整」处（`ids.Parse`、`config.getenv`、FakeRuntime 既有字段）已指明参照对象。
- **类型一致：** `MCPServer`/`MCPServerSpec`/`GenerateCordis`/`SpecsFromServers`/`ApplyMCP`/`ApplyMCPAll`/`EvictIdle`/`ErrBuiltinProtected` 跨任务命名一致。
- **已知风险：** T4 热插拔工具刷新需 spike 实证，兜底=T5 的 EvictIdle→respawn，无死路。
