# Agent Chatbot · P1-A 数据与配置底座 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为个人智能体/chatbot（P1）建好数据持久层与配置：迁移 `000005_agent`（新增 4 表 + 扩展 `agent_message`）、对应仓储（conversation/message/proposal/review/topic_status）、以及 `internal/config` 的 agent/DeepSeek 配置项——全部 TDD、无外部依赖，作为 Plan 2/3 的地基。

**Architecture:** 纯 Go + MySQL 数据层，沿用现有 `internal/repo`（`XxxRepo{DB *sqlx.DB}` + sqlx + 雪花 `ids.ID`）与 `internal/config`（env + 默认值）范式。复用现有 `agent_message`/`daily_review` 两表；本计划新增 `agent_conversation`/`agent_proposal`/`weekly_review`/`topic_status` 并扩展 `agent_message`。**不触碰** dsh、MCP、前端（Plan 2/3）；**不改 memory 表**（其 `session_id` 可空 + `conversation_id` 留到 Plan 3 对话转记忆时再加，避免本期引入 NULL 扫描风险）。

**Tech Stack:** Go 1.25、`github.com/jmoiron/sqlx`、MySQL 8（`DATETIME(3)`/`JSON`/`utf8mb4`）、`golang-migrate`、雪花 ID（`internal/ids`）。

**设计依据：** `docs/superpowers/specs/2026-08-24-agent-chatbot-system-design.md` §6（数据模型）、§14（配置）。

**约定（贯穿全计划，务必一致）：**
- 可空外键/引用列 → Go `*ids.ID`；可空时间 → `*time.Time`；可空整型 → `*int`。
- 可空「文本」列一律 `NOT NULL DEFAULT ''` 配 Go `string`（MySQL `NULL`→`string` 扫描会报错）。
- JSON 列 → Go `json.RawMessage`（`NULL`→`nil` 安全；非空写 JSON 文本）。
- 写方法凡需入事务的，签名收 `ExecerContext`（事务外传 `r.DB`，事务内传 `*sqlx.Tx`），对齐 `TodoRepo.InsertExt`。
- 插入前 `ids.New()` 生成主键，`UserID==0` 则置 `1`（对齐 `TodoRepo.InsertExt`）。
- 集成测试用 `NewDB(TestDSN(t))` + `t.Context()`；未设 `TEST_MYSQL_DSN` 自动 skip。红灯以「`go test` 编译失败：undefined」体现，绿灯跑 `make test-integration`（自动重建 `zhiwei_test` + 迁移 + 真连 MySQL）。

---

## File Structure

| 文件 | 责任 | 动作 |
|---|---|---|
| `migrations/000005_agent.up.sql` | 建 4 新表 + 扩展 `agent_message` | Create |
| `migrations/000005_agent.down.sql` | 反向回滚 | Create |
| `internal/config/config.go` | 新增 agent/DeepSeek 配置字段 + `Load()` 接线 | Modify |
| `internal/config/config_test.go` | 新增配置项默认值/覆盖测试 | Modify |
| `internal/repo/agent_conversation.go` | `AgentConversation` + `AgentConversationRepo` | Create |
| `internal/repo/agent_conversation_test.go` | 会话 CRUD 集成测试 | Create |
| `internal/repo/agent_message.go` | `AgentMessage` + `AgentMessageRepo` | Create |
| `internal/repo/agent_message_test.go` | 消息 append/list 集成测试 | Create |
| `internal/repo/agent_proposal.go` | `AgentProposal` + `AgentProposalRepo` + 状态校验 | Create |
| `internal/repo/agent_proposal_test.go` | 提议 CRUD + Resolve 集成测试 + 纯逻辑状态测试 | Create |
| `internal/repo/review.go` | `DailyReview`/`WeeklyReview` + 仓储（Upsert/Get） | Create |
| `internal/repo/review_test.go` | 日/周报 upsert 幂等集成测试 | Create |
| `internal/repo/topic_status.go` | `TopicStatus` + 仓储（Insert/GetLatest） | Create |
| `internal/repo/topic_status_test.go` | 话题状态快照集成测试 | Create |

**类型契约（跨任务一致，后续 Plan 2/3 依赖）：** 见各任务定义；关键方法名——`AgentConversationRepo.{Create,Get,List,Touch,SetDSHSession}`、`AgentMessageRepo.{Append,ListByConversation}`、`AgentProposalRepo.{Create,Get,ListPending,Resolve}`、`ReviewRepo.{UpsertDaily,GetDaily,UpsertWeekly,GetWeekly}`、`TopicStatusRepo.{Insert,GetLatest}`。

---

## Task 1: 迁移 000005_agent（建表 + 扩展 agent_message）

**Files:**
- Create: `migrations/000005_agent.up.sql`
- Create: `migrations/000005_agent.down.sql`

> 迁移号 000005 与并行分支（person-profile、speaker-name-inference）撞号，合并时统一重编号（见 spec §17）。本分支从 `main`（迁移止于 000004）分出，故本地用 000005。

- [ ] **Step 1: 写 up 迁移**

Create `migrations/000005_agent.up.sql`:

```sql
-- 个人智能体/chatbot P1 数据层（设计见 docs/superpowers/specs/2026-08-24-agent-chatbot-system-design.md §6）。
-- 复用现有 agent_message / daily_review；本迁移新增 4 表 + 扩展 agent_message。
-- 注意：memory 的 session_id 可空化 + conversation_id 列留到 Plan 3（对话转记忆）再加。

-- 会话分组：一个「问知微」对话 = 一行；映射到 dsh sessionId。
CREATE TABLE agent_conversation (
  id             BIGINT PRIMARY KEY,
  user_id        BIGINT NOT NULL DEFAULT 1,
  title          VARCHAR(256) NOT NULL DEFAULT '',   -- 首条消息自动摘要
  dsh_session_id VARCHAR(64) NOT NULL,               -- 传给 dsh 的 sessionId（重启可换）
  status         VARCHAR(16) NOT NULL DEFAULT 'active', -- active|archived
  created_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  last_active_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_user_active (user_id, last_active_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 写入闸门：agent 提议的每处修改，人审前只落这里（spec §8，绝不静默写）。
CREATE TABLE agent_proposal (
  id              BIGINT PRIMARY KEY,
  user_id         BIGINT NOT NULL DEFAULT 1,
  conversation_id BIGINT NULL,
  message_id      BIGINT NULL,                       -- 触发提议的 assistant 消息
  kind            VARCHAR(32) NOT NULL,              -- memory_update|memory_dismiss|topic_rename|topic_confirm|topic_dismiss|todo_create|todo_status
  target_kind     VARCHAR(16) NOT NULL,              -- memory|topic|todo
  target_id       BIGINT NULL,                       -- 目标行（新建类为空）
  payload         JSON NOT NULL,                     -- {old, new, args}
  rationale       TEXT NOT NULL,                     -- agent 理由（展示用；无则空串）
  status          VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|applied|dismissed|expired
  applied_ref     BIGINT NULL,                       -- 落库后指向实际变更行/版本
  created_at      DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  resolved_at     DATETIME(3) NULL,
  KEY idx_user_status (user_id, status),
  KEY idx_conv (conversation_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 周报（与现有 daily_review 平行）。
CREATE TABLE weekly_review (
  id         BIGINT PRIMARY KEY,
  user_id    BIGINT NOT NULL DEFAULT 1,
  week_start DATE NOT NULL,                           -- 周一
  week_end   DATE NOT NULL,
  content    JSON NULL,
  status     VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|ready|failed
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_user_week (user_id, week_start)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 话题/项目状态快照（进展/todo/风险）。
CREATE TABLE topic_status (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  topic_id     BIGINT NOT NULL,
  content      JSON NULL,
  generated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_topic_time (topic_id, generated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 扩展现有 agent_message（原列：id,user_id,role,content,citations,created_at）。
ALTER TABLE agent_message ADD COLUMN conversation_id BIGINT NULL AFTER user_id;
ALTER TABLE agent_message ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT 'text' AFTER role; -- text|tool_call|tool_result|card
ALTER TABLE agent_message ADD COLUMN tool_payload JSON NULL AFTER citations;              -- 工具名+参数+结果
ALTER TABLE agent_message ADD COLUMN dsh_seq INT NULL AFTER tool_payload;                 -- 对齐 dsh 事件序
ALTER TABLE agent_message ADD KEY idx_am_conversation (conversation_id, id);
```

- [ ] **Step 2: 写 down 迁移**

Create `migrations/000005_agent.down.sql`:

```sql
-- 反向回滚。MySQL 不支持 DROP COLUMN IF EXISTS，down 仅用于开发环境。
ALTER TABLE agent_message DROP KEY idx_am_conversation;
ALTER TABLE agent_message DROP COLUMN dsh_seq;
ALTER TABLE agent_message DROP COLUMN tool_payload;
ALTER TABLE agent_message DROP COLUMN kind;
ALTER TABLE agent_message DROP COLUMN conversation_id;

DROP TABLE IF EXISTS topic_status;
DROP TABLE IF EXISTS weekly_review;
DROP TABLE IF EXISTS agent_proposal;
DROP TABLE IF EXISTS agent_conversation;
```

- [ ] **Step 3: 应用迁移验证 SQL 合法**

Run: `make migrate-up`
Expected: 无报错，输出迁移到 version 5（或 `5/u agent`）。若失败，读报错修 SQL。

补充验证（可选）：`make migrate-down` 回滚一版再 `make migrate-up`，确认 down/up 均可执行；随后保持在最新版。

- [ ] **Step 4: Commit**

```bash
git add migrations/000005_agent.up.sql migrations/000005_agent.down.sql
git commit -m "feat(agent-migration): 000005 建 conversation/proposal/weekly_review/topic_status + 扩展 agent_message"
```

---

## Task 2: 配置项（agent + DeepSeek-on-Ark）

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/config/config_test.go` 末尾追加（该文件已 `package config`、已 import `testing`/`os`）：

```go
func TestAgentConfigDefaults(t *testing.T) {
	t.Setenv("ARK_API_KEY", "test-key") // Load 要求 ARK_API_KEY
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AgentEnabled {
		t.Error("AgentEnabled 默认应为 true")
	}
	if cfg.AgentCordisConfig != "services/agent-sidecar/cordis.yml" {
		t.Errorf("AgentCordisConfig 默认错误: %q", cfg.AgentCordisConfig)
	}
	if cfg.AgentMCPURL != "http://127.0.0.1:8080/internal/mcp" {
		t.Errorf("AgentMCPURL 默认错误: %q", cfg.AgentMCPURL)
	}
	if cfg.DSHSessionRoot != "./data/dsh-sessions" {
		t.Errorf("DSHSessionRoot 默认错误: %q", cfg.DSHSessionRoot)
	}
	if cfg.AgentRetrieveTopK != 10 {
		t.Errorf("AgentRetrieveTopK 默认应为 10, got %d", cfg.AgentRetrieveTopK)
	}
	if cfg.ReviewDailyCron != "0 22 * * *" {
		t.Errorf("ReviewDailyCron 默认错误: %q", cfg.ReviewDailyCron)
	}
}

func TestAgentConfigOverride(t *testing.T) {
	t.Setenv("ARK_API_KEY", "test-key")
	t.Setenv("ZW_AGENT_ENABLED", "false")
	t.Setenv("ZW_AGENT_MODEL", "deepseek-v3-250324")
	t.Setenv("ZW_AGENT_RETRIEVE_TOPK", "20")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentEnabled {
		t.Error("ZW_AGENT_ENABLED=false 应关闭")
	}
	if cfg.AgentModel != "deepseek-v3-250324" {
		t.Errorf("AgentModel 覆盖失败: %q", cfg.AgentModel)
	}
	if cfg.AgentRetrieveTopK != 20 {
		t.Errorf("AgentRetrieveTopK 覆盖失败: %d", cfg.AgentRetrieveTopK)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config/ -run TestAgentConfig -v`
Expected: 编译失败 `cfg.AgentEnabled undefined`（字段尚未定义）。

- [ ] **Step 3: 实现配置字段与接线**

在 `internal/config/config.go` 的 `Config` 结构体末尾（`EnrollMinDurationMS` 字段后、结构体闭合 `}` 前）加：

```go

	// ---- Agent / Chatbot（P1；设计见 agent-chatbot spec §14）----
	AgentEnabled      bool   // ZW_AGENT_ENABLED，关掉则不 spawn dsh（报告等仍可用）
	AgentModel        string // ZW_AGENT_MODEL：Ark 上的 DeepSeek 模型/endpoint id（agent 与报告/抽取共用）
	AgentSidecarCmd   string // ZW_AGENT_SIDECAR_CMD：dsh 边车启动命令
	AgentCordisConfig string // ZW_AGENT_CORDIS_CONFIG：cordis.yml 路径
	AgentMCPURL       string // ZW_AGENT_MCP_URL：供 cordis.yml 连回的 MCP-HTTP 地址
	DSHSessionRoot    string // DSH_SESSION_ROOT：dsh 内部会话日志目录
	AgentRetrieveTopK int    // ZW_AGENT_RETRIEVE_TOPK：上下文头检索种子条数
	ReviewDailyCron   string // ZW_REVIEW_DAILY_CRON：日报定时
```

在 `Load()` 的返回 `&Config{...}` 字面量里（`EnrollMinDurationMS` 那行之后、`}` 之前）加：

```go

		// ---- Agent / Chatbot ----
		AgentEnabled:      getenvBool("ZW_AGENT_ENABLED", true),
		AgentModel:        getenv("ZW_AGENT_MODEL", ""),
		AgentSidecarCmd:   getenv("ZW_AGENT_SIDECAR_CMD", "node services/agent-sidecar/node_modules/.bin/dsh-jsonrpc-agent"),
		AgentCordisConfig: getenv("ZW_AGENT_CORDIS_CONFIG", "services/agent-sidecar/cordis.yml"),
		AgentMCPURL:       getenv("ZW_AGENT_MCP_URL", "http://127.0.0.1:8080/internal/mcp"),
		DSHSessionRoot:    getenv("DSH_SESSION_ROOT", "./data/dsh-sessions"),
		AgentRetrieveTopK: getenvInt("ZW_AGENT_RETRIEVE_TOPK", 10),
		ReviewDailyCron:   getenv("ZW_REVIEW_DAILY_CRON", "0 22 * * *"),
```

在文件末尾（`getenvFloat` 之后）加布尔读取助手：

```go

// getenvBool 读取布尔环境变量；"false"/"0" 视为 false，其余非空视为 true，未设置返回默认值。
func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v != "false" && v != "0"
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/config/ -run TestAgentConfig -v`
Expected: PASS（两个测试均通过）。

再跑全包回归：`go test ./internal/config/`
Expected: ok（不破坏现有配置测试）。

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(agent-config): 新增 agent/DeepSeek-on-Ark 配置项 + getenvBool"
```

---

## Task 3: agent_conversation 仓储

**Files:**
- Create: `internal/repo/agent_conversation.go`
- Test: `internal/repo/agent_conversation_test.go`

- [ ] **Step 1: 写失败测试**

Create `internal/repo/agent_conversation_test.go`:

```go
package repo

import (
	"testing"

	"zhiwei/internal/ids"
)

func TestAgentConversationCRUD(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &AgentConversationRepo{DB: db}
	ctx := t.Context()

	c := &AgentConversation{Title: "关于项目A的对话"}
	if err := r.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == 0 {
		t.Fatal("Create 未回填 ID")
	}
	if c.UserID != 1 {
		t.Errorf("UserID 应默认 1, got %d", c.UserID)
	}
	if c.DSHSessionID == "" {
		t.Error("DSHSessionID 应默认非空（回退用会话 ID 字符串）")
	}

	got, err := r.Get(ctx, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "关于项目A的对话" || got.Status != "active" {
		t.Errorf("Get 结果异常: %+v", got)
	}

	if err := r.SetDSHSession(ctx, c.ID, "sess-xyz"); err != nil {
		t.Fatalf("SetDSHSession: %v", err)
	}
	if err := r.Touch(ctx, c.ID); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	list, err := r.List(ctx, 1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, x := range list {
		if x.ID == c.ID {
			found = true
			if x.DSHSessionID != "sess-xyz" {
				t.Errorf("SetDSHSession 未生效: %q", x.DSHSessionID)
			}
		}
	}
	if !found {
		t.Error("List 未包含新建会话")
	}
	_ = ids.ID(0) // 保持 ids 引用
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run TestAgentConversationCRUD`
Expected: 编译失败 `undefined: AgentConversationRepo` / `undefined: AgentConversation`。

- [ ] **Step 3: 实现仓储**

Create `internal/repo/agent_conversation.go`:

```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentConversation 是一次「问知微」对话；映射到 dsh 的 sessionId（重启可换新）。
type AgentConversation struct {
	ID           ids.ID    `db:"id" json:"id"`
	UserID       int64     `db:"user_id" json:"user_id"`
	Title        string    `db:"title" json:"title"`
	DSHSessionID string    `db:"dsh_session_id" json:"dsh_session_id"`
	Status       string    `db:"status" json:"status"` // active|archived
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	LastActiveAt time.Time `db:"last_active_at" json:"last_active_at"`
}

type AgentConversationRepo struct{ DB *sqlx.DB }

// Create 新建会话：生成雪花 ID，UserID 默认 1，DSHSessionID 为空时回退成会话 ID 字符串。
func (r *AgentConversationRepo) Create(ctx context.Context, c *AgentConversation) error {
	c.ID = ids.New()
	if c.UserID == 0 {
		c.UserID = 1
	}
	if c.DSHSessionID == "" {
		c.DSHSessionID = c.ID.String()
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO agent_conversation (id, user_id, title, dsh_session_id)
VALUES (:id, :user_id, :title, :dsh_session_id)`, c)
	return err
}

func (r *AgentConversationRepo) Get(ctx context.Context, id ids.ID) (*AgentConversation, error) {
	var c AgentConversation
	err := r.DB.GetContext(ctx, &c, `SELECT * FROM agent_conversation WHERE id = ?`, id.Int64())
	return &c, err
}

// List 返回某用户的活跃会话，最近活跃优先。
func (r *AgentConversationRepo) List(ctx context.Context, userID int64) ([]AgentConversation, error) {
	var rows []AgentConversation
	err := r.DB.SelectContext(ctx, &rows, `
SELECT * FROM agent_conversation
WHERE user_id = ? AND status = 'active'
ORDER BY last_active_at DESC LIMIT 200`, userID)
	return rows, err
}

// Touch 刷新 last_active_at（每轮对话后调用）。不存在返回 nil（UPDATE 0 行）。
func (r *AgentConversationRepo) Touch(ctx context.Context, id ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE agent_conversation SET last_active_at = CURRENT_TIMESTAMP(3) WHERE id = ?`, id.Int64())
	return err
}

// SetDSHSession 更新映射的 dsh sessionId（边车重启后换新 session 时用）。
func (r *AgentConversationRepo) SetDSHSession(ctx context.Context, id ids.ID, dshSessionID string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE agent_conversation SET dsh_session_id = ? WHERE id = ?`, dshSessionID, id.Int64())
	return err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `make test-integration`（首跑会重建 `zhiwei_test` 并应用含 000005 的迁移）
Expected: `internal/repo` 下 `TestAgentConversationCRUD` PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/agent_conversation.go internal/repo/agent_conversation_test.go
git commit -m "feat(agent-repo): agent_conversation 仓储（Create/Get/List/Touch/SetDSHSession）"
```

---

## Task 4: agent_message 仓储

**Files:**
- Create: `internal/repo/agent_message.go`
- Test: `internal/repo/agent_message_test.go`

- [ ] **Step 1: 写失败测试**

Create `internal/repo/agent_message_test.go`:

```go
package repo

import (
	"encoding/json"
	"testing"
)

func TestAgentMessageAppendAndList(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cr := &AgentConversationRepo{DB: db}
	mr := &AgentMessageRepo{DB: db}
	ctx := t.Context()

	conv := &AgentConversation{Title: "t"}
	if err := cr.Create(ctx, conv); err != nil {
		t.Fatalf("conv Create: %v", err)
	}

	// 用户消息（纯文本）
	um := &AgentMessage{ConversationID: &conv.ID, Role: "user", Content: "帮我查上周的记忆"}
	if err := mr.Append(ctx, um); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	if um.ID == 0 || um.Kind != "text" {
		t.Errorf("默认字段异常: id=%v kind=%q", um.ID, um.Kind)
	}

	// 助手消息（带 citations JSON）
	cites := json.RawMessage(`[{"memory_id":"123","reason":"相关"}]`)
	am := &AgentMessage{ConversationID: &conv.ID, Role: "assistant", Content: "找到 1 条", Citations: cites}
	if err := mr.Append(ctx, am); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}

	list, err := mr.ListByConversation(ctx, conv.ID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 条消息, got %d", len(list))
	}
	if list[0].Role != "user" || list[1].Role != "assistant" {
		t.Errorf("顺序应按 id 升序（user 先）: %q,%q", list[0].Role, list[1].Role)
	}
	if len(list[1].Citations) == 0 {
		t.Error("assistant 的 citations 未持久化")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run TestAgentMessageAppendAndList`
Expected: 编译失败 `undefined: AgentMessageRepo` / `undefined: AgentMessage`。

- [ ] **Step 3: 实现仓储**

Create `internal/repo/agent_message.go`:

```go
package repo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentMessage 是对话流里的一条消息（用户/助手），可携带引用与工具载荷。
// Citations/ToolPayload 为 JSON 列（json.RawMessage：NULL→nil 安全）。
type AgentMessage struct {
	ID             ids.ID          `db:"id" json:"id"`
	UserID         int64           `db:"user_id" json:"user_id"`
	ConversationID *ids.ID         `db:"conversation_id" json:"conversation_id,omitempty"`
	Role           string          `db:"role" json:"role"`           // user|assistant
	Kind           string          `db:"kind" json:"kind"`           // text|tool_call|tool_result|card
	Content        string          `db:"content" json:"content"`
	Citations      json.RawMessage `db:"citations" json:"citations,omitempty"`
	ToolPayload    json.RawMessage `db:"tool_payload" json:"tool_payload,omitempty"`
	DSHSeq         *int            `db:"dsh_seq" json:"dsh_seq,omitempty"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
}

type AgentMessageRepo struct{ DB *sqlx.DB }

// Append 追加一条消息：生成 ID，UserID 默认 1，Kind 空时默认 text。
func (r *AgentMessageRepo) Append(ctx context.Context, m *AgentMessage) error {
	m.ID = ids.New()
	if m.UserID == 0 {
		m.UserID = 1
	}
	if m.Kind == "" {
		m.Kind = "text"
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO agent_message (id, user_id, conversation_id, role, kind, content, citations, tool_payload, dsh_seq)
VALUES (:id, :user_id, :conversation_id, :role, :kind, :content, :citations, :tool_payload, :dsh_seq)`, m)
	return err
}

// ListByConversation 按 id 升序（= 时间顺序）返回一段对话的全部消息。
func (r *AgentMessageRepo) ListByConversation(ctx context.Context, convID ids.ID) ([]AgentMessage, error) {
	var rows []AgentMessage
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM agent_message WHERE conversation_id = ? ORDER BY id ASC`, convID.Int64())
	return rows, err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `make test-integration`
Expected: `TestAgentMessageAppendAndList` PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/agent_message.go internal/repo/agent_message_test.go
git commit -m "feat(agent-repo): agent_message 仓储（Append/ListByConversation）"
```

---

## Task 5: agent_proposal 仓储（写入闸门数据层）

**Files:**
- Create: `internal/repo/agent_proposal.go`
- Test: `internal/repo/agent_proposal_test.go`

> 本任务只做数据层 CRUD + 状态校验；「确认时在事务内落到 memory/topic/todo」的应用逻辑属 Plan 2。`Resolve` 收 `ExecerContext` 以便 Plan 2 在同一事务内调用。

- [ ] **Step 1: 写失败测试（含纯逻辑状态校验）**

Create `internal/repo/agent_proposal_test.go`:

```go
package repo

import (
	"encoding/json"
	"testing"

	"zhiwei/internal/ids"
)

func TestValidProposalStatus(t *testing.T) {
	for _, s := range []string{"pending", "applied", "dismissed", "expired"} {
		if !ValidProposalStatus(s) {
			t.Errorf("%q 应合法", s)
		}
	}
	for _, s := range []string{"", "confirmed", "foo"} {
		if ValidProposalStatus(s) {
			t.Errorf("%q 应非法", s)
		}
	}
}

func TestAgentProposalCRUDAndResolve(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	pr := &AgentProposalRepo{DB: db}
	ctx := t.Context()

	target := ids.New()
	p := &AgentProposal{
		Kind:       "memory_update",
		TargetKind: "memory",
		TargetID:   &target,
		Payload:    json.RawMessage(`{"old":{"content":"旧"},"new":{"content":"新"}}`),
		Rationale:  "用户口述订正",
	}
	if err := pr.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.ID == 0 || p.Status != "pending" {
		t.Errorf("默认字段异常: id=%v status=%q", p.ID, p.Status)
	}

	pend, err := pr.ListPending(ctx, 1)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pend) == 0 {
		t.Fatal("ListPending 应含新建提议")
	}

	// 非法状态被拒
	if err := pr.Resolve(ctx, db, p.ID, "confirmed", nil); err == nil {
		t.Error("Resolve 非法状态应报错")
	}

	// 合法：applied + 回填 applied_ref
	ref := ids.New()
	if err := pr.Resolve(ctx, db, p.ID, "applied", &ref); err != nil {
		t.Fatalf("Resolve applied: %v", err)
	}
	got, err := pr.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "applied" {
		t.Errorf("status 应 applied, got %q", got.Status)
	}
	if got.AppliedRef == nil || *got.AppliedRef != ref {
		t.Error("applied_ref 未回填")
	}
	if got.ResolvedAt == nil {
		t.Error("resolved_at 未设置")
	}

	// applied 后不再出现在 pending
	pend2, _ := pr.ListPending(ctx, 1)
	for _, x := range pend2 {
		if x.ID == p.ID {
			t.Error("applied 提议不应再在 pending 列表")
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run 'TestValidProposalStatus|TestAgentProposalCRUDAndResolve'`
Expected: 编译失败 `undefined: ValidProposalStatus` / `undefined: AgentProposalRepo`。

- [ ] **Step 3: 实现仓储**

Create `internal/repo/agent_proposal.go`:

```go
package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentProposal 是 agent 提议的一处修改，人审前只落此表（绝不静默写，spec §8）。
// 状态流：pending →(用户确认并落库) applied ｜ →(放弃) dismissed ｜ →(超期) expired。
// spec §8 的 "confirmed" 语义由 applied 承载（确认与落库在同一事务原子完成）。
type AgentProposal struct {
	ID             ids.ID          `db:"id" json:"id"`
	UserID         int64           `db:"user_id" json:"user_id"`
	ConversationID *ids.ID         `db:"conversation_id" json:"conversation_id,omitempty"`
	MessageID      *ids.ID         `db:"message_id" json:"message_id,omitempty"`
	Kind           string          `db:"kind" json:"kind"`
	TargetKind     string          `db:"target_kind" json:"target_kind"`
	TargetID       *ids.ID         `db:"target_id" json:"target_id,omitempty"`
	Payload        json.RawMessage `db:"payload" json:"payload"`
	Rationale      string          `db:"rationale" json:"rationale"`
	Status         string          `db:"status" json:"status"`
	AppliedRef     *ids.ID         `db:"applied_ref" json:"applied_ref,omitempty"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
	ResolvedAt     *time.Time      `db:"resolved_at" json:"resolved_at,omitempty"`
}

// ValidProposalStatus 校验提议状态枚举。
func ValidProposalStatus(s string) bool {
	switch s {
	case "pending", "applied", "dismissed", "expired":
		return true
	}
	return false
}

type AgentProposalRepo struct{ DB *sqlx.DB }

// Create 新建提议：生成 ID，UserID 默认 1，Status 强制 pending，Payload 空时置 "{}"。
func (r *AgentProposalRepo) Create(ctx context.Context, p *AgentProposal) error {
	p.ID = ids.New()
	if p.UserID == 0 {
		p.UserID = 1
	}
	p.Status = "pending"
	if len(p.Payload) == 0 {
		p.Payload = json.RawMessage("{}")
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO agent_proposal
  (id, user_id, conversation_id, message_id, kind, target_kind, target_id, payload, rationale, status)
VALUES
  (:id, :user_id, :conversation_id, :message_id, :kind, :target_kind, :target_id, :payload, :rationale, :status)`, p)
	return err
}

func (r *AgentProposalRepo) Get(ctx context.Context, id ids.ID) (*AgentProposal, error) {
	var p AgentProposal
	err := r.DB.GetContext(ctx, &p, `SELECT * FROM agent_proposal WHERE id = ?`, id.Int64())
	return &p, err
}

// ListPending 返回某用户全部待确认提议（最新优先）。
func (r *AgentProposalRepo) ListPending(ctx context.Context, userID int64) ([]AgentProposal, error) {
	var rows []AgentProposal
	err := r.DB.SelectContext(ctx, &rows, `
SELECT * FROM agent_proposal
WHERE user_id = ? AND status = 'pending'
ORDER BY id DESC LIMIT 200`, userID)
	return rows, err
}

// Resolve 把 pending 提议置为终态（applied/dismissed/expired），设 resolved_at；
// applied 时回填 appliedRef。收 ExecerContext：Plan 2 确认端点会在「落库到
// memory/topic/todo」的同一事务内调用（事务外调用传 r.DB）。
// 仅对仍处 pending 的行生效（幂等：重复 resolve 已终态行不改动）。
func (r *AgentProposalRepo) Resolve(ctx context.Context, ext ExecerContext, id ids.ID, status string, appliedRef *ids.ID) error {
	if status == "pending" || !ValidProposalStatus(status) {
		return fmt.Errorf("非法提议终态: %q", status)
	}
	var ref any
	if appliedRef != nil {
		ref = appliedRef.Int64()
	}
	_, err := ext.ExecContext(ctx, `
UPDATE agent_proposal
SET status = ?, applied_ref = ?, resolved_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND status = 'pending'`, status, ref, id.Int64())
	return err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `make test-integration`（纯逻辑 `TestValidProposalStatus` 无需 DB 也会跑）
Expected: 两个测试 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/agent_proposal.go internal/repo/agent_proposal_test.go
git commit -m "feat(agent-repo): agent_proposal 仓储 + 状态校验（Create/Get/ListPending/Resolve）"
```

---

## Task 6: daily_review / weekly_review 仓储

**Files:**
- Create: `internal/repo/review.go`
- Test: `internal/repo/review_test.go`

> `daily_review` 表 000001 已存在（无仓储），`weekly_review` 由 Task 1 新建。二者都用「按自然键 upsert」（日报按 `(user_id, review_date)`、周报按 `(user_id, week_start)`），重跑覆盖 content/status。

- [ ] **Step 1: 写失败测试**

Create `internal/repo/review_test.go`:

```go
package repo

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReviewUpsertDailyIdempotent(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	rr := &ReviewRepo{DB: db}
	ctx := t.Context()
	day := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	if err := rr.UpsertDaily(ctx, 1, day, json.RawMessage(`{"headline":"v1"}`), "ready"); err != nil {
		t.Fatalf("UpsertDaily v1: %v", err)
	}
	// 同一天再写 → 覆盖，不重复行
	if err := rr.UpsertDaily(ctx, 1, day, json.RawMessage(`{"headline":"v2"}`), "ready"); err != nil {
		t.Fatalf("UpsertDaily v2: %v", err)
	}
	got, err := rr.GetDaily(ctx, 1, day)
	if err != nil {
		t.Fatalf("GetDaily: %v", err)
	}
	if got == nil {
		t.Fatal("GetDaily 返回 nil")
	}
	var body struct{ Headline string }
	_ = json.Unmarshal(got.Content, &body)
	if body.Headline != "v2" {
		t.Errorf("应覆盖为 v2, got %q", body.Headline)
	}
}

func TestReviewUpsertWeekly(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	rr := &ReviewRepo{DB: db}
	ctx := t.Context()
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 6)

	if err := rr.UpsertWeekly(ctx, 1, start, end, json.RawMessage(`{"headline":"周"}`), "ready"); err != nil {
		t.Fatalf("UpsertWeekly: %v", err)
	}
	got, err := rr.GetWeekly(ctx, 1, start)
	if err != nil {
		t.Fatalf("GetWeekly: %v", err)
	}
	if got == nil || got.Status != "ready" {
		t.Errorf("GetWeekly 异常: %+v", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run 'TestReviewUpsert'`
Expected: 编译失败 `undefined: ReviewRepo`。

- [ ] **Step 3: 实现仓储**

Create `internal/repo/review.go`:

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

// DailyReview / WeeklyReview 是结构化报告的持久化行（content 为 JSON）。
type DailyReview struct {
	ID         ids.ID          `db:"id" json:"id"`
	UserID     int64           `db:"user_id" json:"user_id"`
	ReviewDate time.Time       `db:"review_date" json:"review_date"`
	Content    json.RawMessage `db:"content" json:"content,omitempty"`
	Status     string          `db:"status" json:"status"`
	CreatedAt  time.Time       `db:"created_at" json:"created_at"`
}

type WeeklyReview struct {
	ID        ids.ID          `db:"id" json:"id"`
	UserID    int64           `db:"user_id" json:"user_id"`
	WeekStart time.Time       `db:"week_start" json:"week_start"`
	WeekEnd   time.Time       `db:"week_end" json:"week_end"`
	Content   json.RawMessage `db:"content" json:"content,omitempty"`
	Status    string          `db:"status" json:"status"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`
}

type ReviewRepo struct{ DB *sqlx.DB }

// UpsertDaily 按 (user_id, review_date) upsert：存在则覆盖 content/status。
func (r *ReviewRepo) UpsertDaily(ctx context.Context, userID int64, date time.Time, content json.RawMessage, status string) error {
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO daily_review (id, user_id, review_date, content, status)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE content = VALUES(content), status = VALUES(status)`,
		ids.New().Int64(), userID, date, []byte(content), status)
	return err
}

// GetDaily 取某天日报；不存在返回 (nil, nil)。
func (r *ReviewRepo) GetDaily(ctx context.Context, userID int64, date time.Time) (*DailyReview, error) {
	var d DailyReview
	err := r.DB.GetContext(ctx, &d,
		`SELECT * FROM daily_review WHERE user_id = ? AND review_date = ?`, userID, date)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// UpsertWeekly 按 (user_id, week_start) upsert。
func (r *ReviewRepo) UpsertWeekly(ctx context.Context, userID int64, start, end time.Time, content json.RawMessage, status string) error {
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO weekly_review (id, user_id, week_start, week_end, content, status)
VALUES (?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE week_end = VALUES(week_end), content = VALUES(content), status = VALUES(status)`,
		ids.New().Int64(), userID, start, end, []byte(content), status)
	return err
}

// GetWeekly 取某周周报；不存在返回 (nil, nil)。
func (r *ReviewRepo) GetWeekly(ctx context.Context, userID int64, start time.Time) (*WeeklyReview, error) {
	var w WeeklyReview
	err := r.DB.GetContext(ctx, &w,
		`SELECT * FROM weekly_review WHERE user_id = ? AND week_start = ?`, userID, start)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `make test-integration`
Expected: `TestReviewUpsertDailyIdempotent`、`TestReviewUpsertWeekly` PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/review.go internal/repo/review_test.go
git commit -m "feat(agent-repo): daily/weekly review 仓储（自然键 upsert + Get）"
```

---

## Task 7: topic_status 仓储

**Files:**
- Create: `internal/repo/topic_status.go`
- Test: `internal/repo/topic_status_test.go`

> 话题状态为「追加快照」（同一 topic 多行历史，取最新），非 upsert——便于看状态演进。

- [ ] **Step 1: 写失败测试**

Create `internal/repo/topic_status_test.go`:

```go
package repo

import (
	"encoding/json"
	"testing"

	"zhiwei/internal/ids"
)

func TestTopicStatusInsertAndGetLatest(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	sr := &TopicStatusRepo{DB: db}
	ctx := t.Context()
	topicID := ids.New()

	if err := sr.Insert(ctx, 1, topicID, json.RawMessage(`{"summary":"第一次"}`)); err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	if err := sr.Insert(ctx, 1, topicID, json.RawMessage(`{"summary":"第二次"}`)); err != nil {
		t.Fatalf("Insert 2: %v", err)
	}

	got, err := sr.GetLatest(ctx, topicID)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if got == nil {
		t.Fatal("GetLatest 返回 nil")
	}
	var body struct{ Summary string }
	_ = json.Unmarshal(got.Content, &body)
	if body.Summary != "第二次" {
		t.Errorf("应取最新快照, got %q", body.Summary)
	}

	// 无快照的 topic 返回 (nil, nil)
	none, err := sr.GetLatest(ctx, ids.New())
	if err != nil {
		t.Fatalf("GetLatest none: %v", err)
	}
	if none != nil {
		t.Error("无快照应返回 nil")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run TestTopicStatusInsertAndGetLatest`
Expected: 编译失败 `undefined: TopicStatusRepo`。

- [ ] **Step 3: 实现仓储**

Create `internal/repo/topic_status.go`:

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

// TopicStatus 是某 topic 的状态快照（进展/todo/风险），追加式历史。
type TopicStatus struct {
	ID          ids.ID          `db:"id" json:"id"`
	UserID      int64           `db:"user_id" json:"user_id"`
	TopicID     ids.ID          `db:"topic_id" json:"topic_id"`
	Content     json.RawMessage `db:"content" json:"content,omitempty"`
	GeneratedAt time.Time       `db:"generated_at" json:"generated_at"`
}

type TopicStatusRepo struct{ DB *sqlx.DB }

// Insert 追加一条快照（不 upsert，保留历史）。UserID 传 0 视为 1。
func (r *TopicStatusRepo) Insert(ctx context.Context, userID int64, topicID ids.ID, content json.RawMessage) error {
	if userID == 0 {
		userID = 1
	}
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO topic_status (id, user_id, topic_id, content)
VALUES (?, ?, ?, ?)`, ids.New().Int64(), userID, topicID.Int64(), []byte(content))
	return err
}

// GetLatest 取某 topic 最新快照；无则返回 (nil, nil)。
func (r *TopicStatusRepo) GetLatest(ctx context.Context, topicID ids.ID) (*TopicStatus, error) {
	var s TopicStatus
	err := r.DB.GetContext(ctx, &s, `
SELECT * FROM topic_status WHERE topic_id = ?
ORDER BY generated_at DESC, id DESC LIMIT 1`, topicID.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `make test-integration`
Expected: `TestTopicStatusInsertAndGetLatest` PASS。

- [ ] **Step 5: 全量回归 + Commit**

Run: `make test`（纯逻辑，无 MySQL）与 `make test-integration`（真连）各跑一遍。
Expected: 全绿。

```bash
git add internal/repo/topic_status.go internal/repo/topic_status_test.go
git commit -m "feat(agent-repo): topic_status 快照仓储（Insert/GetLatest）"
```

---

## 收尾验收（Plan 1 完成标志）

- [ ] `make migrate-up` / `make migrate-down` 均可执行；000005 up/down 无误。
- [ ] `make test` 全绿（新增纯逻辑测试：`TestValidProposalStatus`、config 两测）。
- [ ] `make test-integration` 全绿（5 个新仓储的集成测试）。
- [ ] `internal/config` 暴露 8 个新 agent 配置项，默认值符合 spec §14。
- [ ] 4 张新表 + `agent_message` 扩展列就位；memory 表未改动（留 Plan 3）。
- [ ] 5 个仓储的方法签名与「类型契约」一致，供 Plan 2/3 直接依赖。

**下一步（不在本计划内）：** Plan 2 = dsh 边车 spike + AgentRuntime + MCP 工具 + 对话编排 + WS + 聊天前端。
