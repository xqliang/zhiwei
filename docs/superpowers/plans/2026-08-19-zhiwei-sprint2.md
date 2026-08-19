# 知微云端 MVP · Sprint 2 实现计划（Memory 抽取 / Todo / Topic）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 打通产品核心价值链路——pipeline 新增 extract stage（LLM 抽取记忆候选 → 质量闸门 → Topic 归类 → 单事务落库 memory/todo/topic），配齐三组 API 与 Web 卡片 UI（时间线 memory/todo 卡片、Topics 页、独立待办页）。

**Architecture:** 合并式 extract stage（spec 修订决策）：handler 内部完成「对话块聚合 → 混合窗口 LLM 抽取 → 纯规则质量闸门 → Topic 归属解析 → 单事务提交（幂等）」，中间产物不落库。抽取纯逻辑放 `internal/memory`（无 DB/网络依赖，可完全单测）。Handler 签名扩展为接收 `*repo.Job` 以便写 trace。

**Tech Stack:** Go 1.26、chi v5、sqlx + MySQL 8（端口 3307，容器 `zhiwei-mvp-mysql`）、Ark LLM（`doubao-seed-1-6-flash-250828`，经现有 `provider.LLMProvider`）、Vue 3（CDN 无构建）。

**上游 spec:** `docs/superpowers/specs/2026-08-19-zhiwei-sprint2-design.md`（本计划实现该 spec；三处对总设计的修订记录在 spec §2）

**约定（全计划适用）:**
- 纯逻辑单测进 `make test`；集成测试以 `TEST_MYSQL_DSN` 是否存在为开关，经 `make test-integration`（自动重建 `zhiwei_test` 库）执行
- 所有命令在仓库根目录执行；本计划在 worktree `.claude/worktrees/sprint2-memory-todo-topic` 中进行
- 所有 ID 为雪花 ID，JSON 序列化为字符串
- 提交信息结尾带 `Co-Authored-By: Claude <noreply@anthropic.com>`（下文提交步骤中省略，执行时补上）

---

## 文件结构总览

```text
新增：
internal/ids/list.go                  # ID JSON 数组类型（memory.transcript_segment_ids 用）
internal/repo/topic.go                # topic DAO（含事务版方法与计数列表）
internal/repo/memory.go               # memory DAO（含事务版方法与 topic 名称联查）
internal/repo/todo.go                 # todo DAO + 状态机纯函数 CanTransition
internal/memory/block.go              # 对话块聚合 + 窗口切分
internal/memory/candidate.go          # 候选结构 + LLM 输出解析容错 + 质量闸门
internal/memory/topic.go              # Topic 归属决策（纯函数）
internal/memory/extract.go            # Extractor：LLM 编排（窗口循环/合并去重/provenance）
internal/pipeline/stage_extract.go    # extract stage（编排 + 单事务提交）
internal/pipeline/trace.go            # job.trace 追加辅助
internal/api/memory.go                # GET/PATCH /api/memories
internal/api/todo.go                  # GET/PATCH /api/todos
internal/api/topic.go                 # /api/topics 四个端点
prompts/extraction_v1.md              # 抽取 prompt（版本化，运行时读取）
web/app.js                            # Vue 应用（从 index.html 拆出）

修改：
internal/config/config.go             # 三个抽取参数 env
internal/pipeline/pool.go             # Handler 签名加 *repo.Job 参数
internal/pipeline/stage_asr.go        # StageDeps 扩展 + handler 适配新签名
cmd/zhiwei-server/main.go             # Flow 扩为三 stage、新依赖装配、新路由注册
internal/api/query.go                 # session 详情附带 memories/todos
scripts/e2e.sh                        # 追加 memory 产出断言
web/index.html                        # 四标签页 + 拆分引用 app.js
```

---

### Task 1: 配置扩展（抽取参数三个 env）

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/config/config_test.go` 的 `TestLoadDefaults` 中追加断言：

```go
	if c.ExtractWindow != 10 {
		t.Errorf("ExtractWindow = %d, want 10", c.ExtractWindow)
	}
	if c.QualityMinConf != 0.6 {
		t.Errorf("QualityMinConf = %v, want 0.6", c.QualityMinConf)
	}
	if c.QualityTodoConf != 0.85 {
		t.Errorf("QualityTodoConf = %v, want 0.85", c.QualityTodoConf)
	}
```

并新增一个用例（文件末尾追加）：

```go
func TestLoadExtractOverrides(t *testing.T) {
	t.Setenv("ARK_API_KEY", "test-key")
	t.Setenv("ZW_EXTRACT_WINDOW", "5")
	t.Setenv("ZW_QUALITY_MIN_CONF", "0.7")
	t.Setenv("ZW_QUALITY_TODO_CONF", "0.9")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ExtractWindow != 5 || c.QualityMinConf != 0.7 || c.QualityTodoConf != 0.9 {
		t.Fatalf("got window=%d minConf=%v todoConf=%v", c.ExtractWindow, c.QualityMinConf, c.QualityTodoConf)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/config/`
Expected: FAIL（字段未定义）

- [ ] **Step 3: 实现**

`internal/config/config.go` 的 `Config` 结构体追加字段：

```go
	// ---- Sprint 2：抽取参数（见 Sprint 2 设计文档 §3） ----
	ExtractWindow   int     // 抽取窗口切分大小（对话块数），超过则分多次 LLM 调用
	QualityMinConf  float64 // 质量闸门：候选最低置信度，低于丢弃
	QualityTodoConf float64 // todo 直接入库为 confirmed 的置信度阈值，低于降级 suggested
```

`Load()` 返回值中追加（新增两个辅助函数）：

```go
		ExtractWindow:   getenvInt("ZW_EXTRACT_WINDOW", 10),
		QualityMinConf:  getenvFloat("ZW_QUALITY_MIN_CONF", 0.6),
		QualityTodoConf: getenvFloat("ZW_QUALITY_TODO_CONF", 0.85),
```

文件末尾追加辅助函数：

```go
func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func getenvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return def
}
```

import 增加 `"strconv"`。

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/config/ -v`
Expected: PASS（3 个用例）

- [ ] **Step 5: 提交**

```bash
git add internal/config/
git commit -m "feat: 抽取参数配置（窗口大小与质量闸门阈值）"
```

---

### Task 2: ids.List 类型（JSON 数组 + DB JSON 列）

**Files:**
- Create: `internal/ids/list.go`
- Test: `internal/ids/list_test.go`

`memory.transcript_segment_ids` 是 MySQL JSON 列，业务上是 ID 数组。这个类型同时实现 `driver.Valuer` / `sql.Scanner`（DB 侧）与 `json.Marshaler`（API 侧输出字符串数组）。

- [ ] **Step 1: 写失败测试**

`internal/ids/list_test.go`：

```go
package ids

import (
	"encoding/json"
	"testing"
)

func TestListValueAndScan(t *testing.T) {
	l := List{1234567890123456789, 1234567890123456790}
	v, err := l.Value()
	if err != nil {
		t.Fatal(err)
	}
	// Valuer 输出 JSON 文本，可直接写入 MySQL JSON 列
	if s, ok := v.(string); !ok || s != `["1234567890123456789","1234567890123456790"]` {
		t.Fatalf("Value = %#v", v)
	}

	var out List
	if err := out.Scan([]byte(s0(t))); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0] != 1234567890123456789 {
		t.Fatalf("Scan out = %v", out)
	}
}

func TestListScanNilAndEmpty(t *testing.T) {
	var l List
	if err := l.Scan(nil); err != nil || l != nil {
		t.Fatalf("Scan(nil) -> %v %v", l, err)
	}
	if err := l.Scan([]byte("[]")); err != nil || len(l) != 0 {
		t.Fatalf("Scan([]) -> %v %v", l, err)
	}
}

func TestListJSONRoundTrip(t *testing.T) {
	b, err := json.Marshal(List{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `["1","2"]` {
		t.Fatalf("json = %s", b)
	}
	var l List
	if err := json.Unmarshal([]byte(`["1","2"]`), &l); err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 || l[1] != 2 {
		t.Fatalf("unmarshal = %v", l)
	}
	// nil 序列化为 null（对应 DB NULL 列）
	b, _ = json.Marshal(List(nil))
	if string(b) != "null" {
		t.Fatalf("nil json = %s", b)
	}
}

func s0(t *testing.T) string {
	t.Helper()
	l := List{1234567890123456789, 1234567890123456790}
	v, _ := l.Value()
	s, _ := v.(string)
	return s
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/ids/`
Expected: FAIL（`List` 未定义）

- [ ] **Step 3: 实现**

`internal/ids/list.go`：

```go
// list.go 提供 ID 的 JSON 数组类型，用于 memory.transcript_segment_ids 这类
// 「DB 存 JSON 文本、API 输出字符串数组」的列。
package ids

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// List 是 ID 数组。DB 侧序列化为 JSON 文本（写 MySQL JSON 列），
// API 侧输出 ["123","456"] 字符串数组（元素仍走 ID 的字符串序列化）。
type List []ID

// Value 实现 driver.Valuer：写库时序列化为 JSON 文本；nil 写 NULL。
func (l List) Value() (driver.Value, error) {
	if l == nil {
		return nil, nil
	}
	return l.toJSON()
}

// Scan 实现 sql.Scanner：从 JSON 文本（[]byte 或 string）还原。
func (l *List) Scan(src any) error {
	if src == nil {
		*l = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("ids.List.Scan: 不支持的源类型 %T", src)
	}
	return json.Unmarshal(raw, (*[]ID)(l))
}

// MarshalJSON 输出字符串数组；nil 输出 null。
func (l List) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("null"), nil
	}
	return l.toJSON()
}

// UnmarshalJSON 接受字符串数组。
func (l *List) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, (*[]ID)(l))
}

func (l List) toJSON() ([]byte, error) {
	return json.Marshal([]ID(l))
}

// 编译期断言接口实现完整。
var _ driver.Valuer = List(nil)
var _ sql.Scanner = (*List)(nil)
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/ids/ -v`
Expected: PASS（原有用例 + 3 个新用例）

- [ ] **Step 5: 提交**

```bash
git add internal/ids/
git commit -m "feat: ids.List 类型（JSON 数组双向映射）"
```

---

### Task 3: Topic DAO

**Files:**
- Create: `internal/repo/topic.go`
- Test: `internal/repo/topic_test.go`

- [ ] **Step 1: 写失败测试**

`internal/repo/topic_test.go`：

```go
package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestTopicCRUD(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &TopicRepo{DB: db}
	ctx := context.Background()

	// 创建（事务版 CreateExt 与普通 Create 走同一实现）
	tp := &Topic{Name: "Rust 学习", Status: "active", CreatedBy: "user"}
	if err := r.Create(ctx, tp); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tp.ID == 0 {
		t.Fatal("Create 未回填 ID")
	}

	// Get
	got, err := r.Get(ctx, tp.ID)
	if err != nil || got.Name != "Rust 学习" || got.CreatedBy != "user" {
		t.Fatalf("Get: %v %+v", err, got)
	}

	// 按名查找（active/suggested）
	found, err := r.FindActiveByName(ctx, 1, "Rust 学习")
	if err != nil {
		t.Fatalf("FindActiveByName: %v", err)
	}
	if found == nil || found.ID != tp.ID {
		t.Fatalf("found = %+v", found)
	}

	// dismissed 的同名 topic 不参与合并
	other := &Topic{Name: "旧主题", Status: "dismissed", CreatedBy: "ai"}
	_ = r.Create(ctx, other)
	if m, _ := r.FindActiveByName(ctx, 1, "旧主题"); m != nil {
		t.Fatal("dismissed topic 不应被 FindActiveByName 命中")
	}

	// 状态与改名
	if err := r.UpdateStatus(ctx, tp.ID, "dismissed"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if err := r.UpdateName(ctx, tp.ID, "Rust 进阶"); err != nil {
		t.Fatalf("UpdateName: %v", err)
	}
	got2, _ := r.Get(ctx, tp.ID)
	if got2.Name != "Rust 进阶" {
		t.Fatalf("name = %s", got2.Name)
	}
}

func TestTopicListActiveLimit(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	r := &TopicRepo{DB: db}
	ctx := context.Background()

	list, err := r.ListActive(ctx, 1, 30)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, tp := range list {
		if tp.Status == "dismissed" {
			t.Fatal("ListActive 不应包含 dismissed")
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（`TopicRepo` 未定义）

- [ ] **Step 3: 实现**

`internal/repo/topic.go`：

```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Topic 是记忆的组织层：AI 抽取时自动归类/建议，用户可确认、改名、忽略。
type Topic struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	Name        string    `db:"name" json:"name"`
	Description *string   `db:"description" json:"description,omitempty"`
	Status      string    `db:"status" json:"status"`         // suggested|active|dismissed
	CreatedBy   string    `db:"created_by" json:"created_by"` // ai|user
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// TopicWithCount 是列表接口的带计数视图。
type TopicWithCount struct {
	Topic
	MemoryCount   int `db:"memory_count" json:"memory_count"`       // active memory 数
	OpenTodoCount int `db:"open_todo_count" json:"open_todo_count"` // confirmed（未完成）todo 数
}

type TopicRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务，传 r.DB 即独立执行）。
func (r *TopicRepo) CreateExt(ctx context.Context, ext sqlx.ExtContext, tp *Topic) error {
	tp.ID = ids.New()
	if tp.UserID == 0 {
		tp.UserID = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO topic (id, user_id, name, description, status, created_by)
VALUES (:id, :user_id, :name, :description, :status, :created_by)`, tp)
	return err
}

func (r *TopicRepo) Create(ctx context.Context, tp *Topic) error {
	return r.CreateExt(ctx, r.DB, tp)
}

func (r *TopicRepo) Get(ctx context.Context, id ids.ID) (*Topic, error) {
	var tp Topic
	err := r.DB.GetContext(ctx, &tp, `SELECT * FROM topic WHERE id = ?`, id.Int64())
	return &tp, err
}

// ListActive 返回 active + suggested 的主题（抽取 prompt 输入 / 合并查重用），
// 按更新时间倒序，最多 limit 条。
func (r *TopicRepo) ListActive(ctx context.Context, userID int64, limit int) ([]Topic, error) {
	var list []Topic
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM topic
WHERE user_id = ? AND status IN ('active','suggested')
ORDER BY updated_at DESC LIMIT ?`, userID, limit)
	return list, err
}

// FindActiveByName 按名称精确查找 active/suggested 主题（同名合并用）；无命中返回 nil。
func (r *TopicRepo) FindActiveByName(ctx context.Context, userID int64, name string) (*Topic, error) {
	var tp Topic
	err := r.DB.GetContext(ctx, &tp, `
SELECT * FROM topic
WHERE user_id = ? AND name = ? AND status IN ('active','suggested')
ORDER BY id LIMIT 1`, userID, name)
	if err != nil {
		if err.Error() == "sql: no rows" {
			return nil, nil
		}
		return nil, err
	}
	return &tp, nil
}

// ListWithCounts 列出非 dismissed 主题及关联计数（Topics 页用）。
func (r *TopicRepo) ListWithCounts(ctx context.Context, userID int64) ([]TopicWithCount, error) {
	var list []TopicWithCount
	err := r.DB.SelectContext(ctx, &list, `
SELECT t.*,
  (SELECT COUNT(*) FROM memory m WHERE m.topic_id = t.id AND m.status = 'active') AS memory_count,
  (SELECT COUNT(*) FROM todo td WHERE td.topic_id = t.id AND td.status = 'confirmed') AS open_todo_count
FROM topic t
WHERE t.user_id = ? AND t.status != 'dismissed'
ORDER BY memory_count DESC, open_todo_count DESC, t.updated_at DESC`, userID)
	return list, err
}

func (r *TopicRepo) UpdateStatus(ctx context.Context, id ids.ID, status string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE topic SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *TopicRepo) UpdateName(ctx context.Context, id ids.ID, name string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE topic SET name = ? WHERE id = ?`, name, id.Int64())
	return err
}
```

- [ ] **Step 4: 运行测试通过**

Run: `make test-integration`
Expected: repo 包 PASS

- [ ] **Step 5: 提交**

```bash
git add internal/repo/topic.go internal/repo/topic_test.go
git commit -m "feat: topic DAO（事务创建/同名查找/计数列表）"
```

---

### Task 4: Memory DAO

**Files:**
- Create: `internal/repo/memory.go`
- Test: `internal/repo/memory_test.go`

- [ ] **Step 1: 写失败测试**

`internal/repo/memory_test.go`：

```go
package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
)

func newTestMemory(sessionID, topicID ids.ID) *Memory {
	eventAt := time.Now()
	return &Memory{
		Type: "event", Title: "给 Tom 发邮件", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Importance: 0.6, Confidence: 0.9,
		TopicID: &topicID, SessionID: sessionID, TranscriptSegmentIDs: ids.List{1, 2},
		EventAt: &eventAt, Status: "active",
	}
}

func TestMemoryInsertAndQuery(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	tr := &TopicRepo{DB: db}
	ctx := context.Background()

	topic := &Topic{Name: "工作", Status: "active", CreatedBy: "user"}
	_ = tr.Create(ctx, topic)

	sid := ids.New()
	m := newTestMemory(sid, topic.ID)
	if err := mr.InsertExt(ctx, db, []Memory{*m}); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("InsertExt 未回填 ID")
	}

	// 按 session 查询（联查 topic 名称）
	rows, err := mr.ListBySession(ctx, sid)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListBySession: %v len=%d", err, len(rows))
	}
	if rows[0].TopicName == nil || *rows[0].TopicName != "工作" {
		t.Fatalf("topic_name = %v", rows[0].TopicName)
	}
	if len(rows[0].TranscriptSegmentIDs) != 2 {
		t.Fatalf("segment_ids = %v", rows[0].TranscriptSegmentIDs)
	}

	// Get
	got, err := mr.Get(ctx, m.ID)
	if err != nil || got.Title != "给 Tom 发邮件" {
		t.Fatalf("Get: %v %+v", err, got)
	}

	// Save：改内容 version+1
	got.Content = "后天给 Tom 发邮件确认设计稿"
	got.Version++
	if err := mr.Save(ctx, got); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got2, _ := mr.Get(ctx, m.ID)
	if got2.Version != 2 || got2.Content != "后天给 Tom 发邮件确认设计稿" {
		t.Fatalf("after save: %+v", got2)
	}

	// 过滤列表：type 过滤命中，错误 type 无结果
	if rows, _ = mr.List(ctx, MemoryFilter{Type: "event", Limit: 10}); len(rows) < 1 {
		t.Fatal("type=event 应命中")
	}
	if rows, _ = mr.List(ctx, MemoryFilter{Type: "idea", Limit: 10}); len(rows) != 0 {
		t.Fatal("type=idea 不应命中")
	}
}

func TestMemoryDeleteBySession(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	mr := &MemoryRepo{DB: db}
	ctx := context.Background()

	sid := ids.New()
	m := newTestMemory(sid, 1)
	m.TopicID = nil
	_ = mr.InsertExt(ctx, db, []Memory{*m})
	if err := mr.DeleteBySessionExt(ctx, db, sid); err != nil {
		t.Fatalf("DeleteBySessionExt: %v", err)
	}
	if rows, _ := mr.ListBySession(ctx, sid); len(rows) != 0 {
		t.Fatalf("删除后仍有 %d 条", len(rows))
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（`MemoryRepo` 未定义）

- [ ] **Step 3: 实现**

`internal/repo/memory.go`：

```go
package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Memory 是从对话中抽取的一条记忆。embedding 列 Sprint 3 启用，本期留空。
type Memory struct {
	ID                   ids.ID     `db:"id" json:"id"`
	UserID               int64      `db:"user_id" json:"user_id"`
	Type                 string     `db:"type" json:"type"`
	Title                string     `db:"title" json:"title"`
	Content              string     `db:"content" json:"content"`
	EpistemicType        string     `db:"epistemic_type" json:"epistemic_type"`
	Importance           float64    `db:"importance" json:"importance"`
	Confidence           float64    `db:"confidence" json:"confidence"`
	TopicID              *ids.ID    `db:"topic_id" json:"topic_id,omitempty"`
	SessionID            ids.ID     `db:"session_id" json:"session_id"`
	TranscriptSegmentIDs ids.List   `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	EventAt              *time.Time `db:"event_at" json:"event_at,omitempty"`
	Status               string     `db:"status" json:"status"`
	Version              int        `db:"version" json:"version"`
	CreatedAt            time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time  `db:"updated_at" json:"updated_at"`
}

// MemoryRow 是带 topic 名称的列表视图（前端卡片直接展示归属）。
type MemoryRow struct {
	Memory
	TopicName *string `db:"topic_name" json:"topic_name,omitempty"`
}

// MemoryFilter 是列表查询条件，零值字段不参与过滤。
type MemoryFilter struct {
	Type    string
	TopicID *ids.ID
	Limit   int
	Offset  int
}

type MemoryRepo struct{ DB *sqlx.DB }

// InsertExt 批量插入（ext 传 *sqlx.Tx 即加入事务）。ID 在此生成并回填。
func (r *MemoryRepo) InsertExt(ctx context.Context, ext sqlx.ExtContext, ms []Memory) error {
	if len(ms) == 0 {
		return nil
	}
	for i := range ms {
		ms[i].ID = ids.New()
		if ms[i].UserID == 0 {
			ms[i].UserID = 1
		}
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO memory (id, user_id, type, title, content, epistemic_type,
  importance, confidence, topic_id, session_id, transcript_segment_ids, event_at, status)
VALUES (:id, :user_id, :type, :title, :content, :epistemic_type,
  :importance, :confidence, :topic_id, :session_id, :transcript_segment_ids, :event_at, :status)`, ms)
	return err
}

// DeleteBySessionExt 删除一个 session 的全部 memory（extract 重跑幂等用）。
func (r *MemoryRepo) DeleteBySessionExt(ctx context.Context, ext sqlx.ExtContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx, `DELETE FROM memory WHERE session_id = ?`, sessionID.Int64())
	return err
}

func (r *MemoryRepo) Get(ctx context.Context, id ids.ID) (*Memory, error) {
	var m Memory
	err := r.DB.GetContext(ctx, &m, `SELECT * FROM memory WHERE id = ?`, id.Int64())
	return &m, err
}

// Save 保存用户修正（version 由调用方 +1 后整体写回）。
func (r *MemoryRepo) Save(ctx context.Context, m *Memory) error {
	_, err := r.DB.ExecContext(ctx, `
UPDATE memory SET title = ?, content = ?, status = ?, version = ? WHERE id = ?`,
		m.Title, m.Content, m.Status, m.Version, m.ID.Int64())
	return err
}

func (r *MemoryRepo) List(ctx context.Context, f MemoryFilter) ([]MemoryRow, error) {
	where := map[string]any{}
	if f.Type != "" {
		where["m.type"] = f.Type
	}
	if f.TopicID != nil {
		where["m.topic_id"] = f.TopicID.Int64()
	}
	return r.listWhere(ctx, where, f.Limit, f.Offset)
}

func (r *MemoryRepo) ListBySession(ctx context.Context, sessionID ids.ID) ([]MemoryRow, error) {
	return r.listWhere(ctx, map[string]any{"m.session_id": sessionID.Int64()}, 200, 0)
}

func (r *MemoryRepo) ListByTopic(ctx context.Context, topicID ids.ID) ([]MemoryRow, error) {
	return r.listWhere(ctx, map[string]any{"m.topic_id": topicID.Int64()}, 200, 0)
}

// listWhere 组装 WHERE（列=值 AND 连接；map 迭代顺序不影响 AND 语义），
// 基础条件固定排除 dismissed。
func (r *MemoryRepo) listWhere(ctx context.Context, where map[string]any, limit, offset int) ([]MemoryRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var conds []string
	var args []any
	for col, val := range where {
		conds = append(conds, col+" = ?")
		args = append(args, val)
	}
	cond := "m.status != 'dismissed'"
	if len(conds) > 0 {
		cond += " AND " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, offset)
	var rows []MemoryRow
	err := r.DB.SelectContext(ctx, &rows, fmt.Sprintf(`
SELECT m.*, t.name AS topic_name
FROM memory m LEFT JOIN topic t ON m.topic_id = t.id
WHERE %s
ORDER BY m.event_at DESC, m.id DESC
LIMIT ? OFFSET ?`, cond), args...)
	return rows, err
}
```

- [ ] **Step 4: 运行测试通过**

Run: `make test-integration`
Expected: repo 包 PASS

- [ ] **Step 5: 提交**

```bash
git add internal/repo/memory.go internal/repo/memory_test.go
git commit -m "feat: memory DAO（事务批量插入/按 session 删除/topic 联查）"
```

---

### Task 5: Todo DAO（含状态机纯函数）

**Files:**
- Create: `internal/repo/todo.go`
- Test: `internal/repo/todo_test.go`

- [ ] **Step 1: 写失败测试（状态机是纯逻辑，无 DSN 也执行）**

`internal/repo/todo_test.go`：

```go
package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
)

// 纯逻辑：状态机（验证非法流转被拒绝）
func TestTodoCanTransition(t *testing.T) {
	ok := [][2]string{
		{"suggested", "confirmed"},
		{"suggested", "dismissed"},
		{"confirmed", "done"},
		{"confirmed", "dismissed"},
		{"done", "dismissed"},
	}
	for _, c := range ok {
		if !CanTransition(c[0], c[1]) {
			t.Errorf("%s -> %s 应允许", c[0], c[1])
		}
	}
	bad := [][2]string{
		{"suggested", "done"}, // 必须先确认
		{"done", "confirmed"}, // 完成不回退
		{"dismissed", "confirmed"},
		{"confirmed", "suggested"},
		{"", "done"},
	}
	for _, c := range bad {
		if CanTransition(c[0], c[1]) {
			t.Errorf("%s -> %s 应拒绝", c[0], c[1])
		}
	}
}

func TestTodoInsertAndList(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	tr := &TodoRepo{DB: db}
	ctx := context.Background()

	sid := ids.New()
	mem := &Memory{Type: "event", Title: "发邮件", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Importance: 0.6, Confidence: 0.9,
		SessionID: sid, Status: "active"}
	_ = mr.InsertExt(ctx, db, []Memory{*mem})

	due := time.Now().Add(24 * time.Hour)
	td := Todo{Title: "给 Tom 发邮件", SourceMemoryID: &mem.ID, Status: "confirmed",
		DueAt: &due, Confidence: 0.9}
	if err := tr.InsertExt(ctx, db, []Todo{td}); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}

	// 列表联查来源 session
	rows, err := tr.List(ctx, "", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.SourceMemoryID != nil && *row.SourceMemoryID == mem.ID {
			found = true
			if row.SourceSessionID == nil || *row.SourceSessionID != sid {
				t.Fatalf("source_session_id = %v", row.SourceSessionID)
			}
		}
	}
	if !found {
		t.Fatal("未找到刚插入的 todo")
	}

	// 状态更新 + status 过滤
	if err := tr.UpdateStatus(ctx, td.ID, "done"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	rows2, _ := tr.List(ctx, "done", nil)
	var seen bool
	for _, row := range rows2 {
		if row.ID == td.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatal("status=done 过滤未命中")
	}

	// 幂等清理：按来源 session 删除（先删 todo 再删 memory 的顺序由 stage 保证）
	if err := tr.DeleteBySessionExt(ctx, db, sid); err != nil {
		t.Fatalf("DeleteBySessionExt: %v", err)
	}
	_ = mr.DeleteBySessionExt(ctx, db, sid)
	rows3, _ := tr.ListBySession(ctx, sid)
	if len(rows3) != 0 {
		t.Fatalf("清理后仍有 %d 条", len(rows3))
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（`TodoRepo`、`CanTransition` 未定义）

- [ ] **Step 3: 实现**

`internal/repo/todo.go`：

```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Todo 是从对话中提取的待办。suggested 需用户确认后转 confirmed。
type Todo struct {
	ID             ids.ID     `db:"id" json:"id"`
	UserID         int64      `db:"user_id" json:"user_id"`
	Title          string     `db:"title" json:"title"`
	SourceMemoryID *ids.ID    `db:"source_memory_id" json:"source_memory_id,omitempty"`
	TopicID        *ids.ID    `db:"topic_id" json:"topic_id,omitempty"`
	Status         string     `db:"status" json:"status"` // suggested|confirmed|done|dismissed
	DueAt          *time.Time `db:"due_at" json:"due_at,omitempty"`
	Confidence     float64    `db:"confidence" json:"confidence"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
}

// TodoRow 是带来源会话的列表视图（待办页「跳转时间线」用）。
type TodoRow struct {
	Todo
	SourceSessionID *ids.ID `db:"source_session_id" json:"source_session_id,omitempty"`
}

// CanTransition 校验 todo 状态流转。
// 合法路径：suggested→confirmed、confirmed→done、任意非 dismissed→dismissed。
func CanTransition(from, to string) bool {
	switch {
	case from == "suggested" && to == "confirmed":
		return true
	case from == "confirmed" && to == "done":
		return true
	case (from == "suggested" || from == "confirmed" || from == "done") && to == "dismissed":
		return true
	}
	return false
}

type TodoRepo struct{ DB *sqlx.DB }

// InsertExt 批量插入（ext 传 *sqlx.Tx 即加入事务）。ID 在此生成并回填。
func (r *TodoRepo) InsertExt(ctx context.Context, ext sqlx.ExtContext, todos []Todo) error {
	if len(todos) == 0 {
		return nil
	}
	for i := range todos {
		todos[i].ID = ids.New()
		if todos[i].UserID == 0 {
			todos[i].UserID = 1
		}
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO todo (id, user_id, title, source_memory_id, topic_id, status, due_at, confidence)
VALUES (:id, :user_id, :title, :source_memory_id, :topic_id, :status, :due_at, :confidence)`, todos)
	return err
}

// DeleteBySessionExt 删除派生自某 session 全部 memory 的 todo（经 source_memory_id
// 子查询关联；extract 重跑幂等用，必须在删 memory 之前调用）。
func (r *TodoRepo) DeleteBySessionExt(ctx context.Context, ext sqlx.ExtContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx, `
DELETE FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?)`,
		sessionID.Int64())
	return err
}

func (r *TodoRepo) Get(ctx context.Context, id ids.ID) (*Todo, error) {
	var td Todo
	err := r.DB.GetContext(ctx, &td, `SELECT * FROM todo WHERE id = ?`, id.Int64())
	return &td, err
}

func (r *TodoRepo) UpdateStatus(ctx context.Context, id ids.ID, status string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE todo SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

const todoListBase = `
SELECT t.*, m.session_id AS source_session_id
FROM todo t LEFT JOIN memory m ON t.source_memory_id = m.id`

// List 列表。status / topicID 为空不过滤；dismissed 永不出现。
func (r *TodoRepo) List(ctx context.Context, status string, topicID *ids.ID) ([]TodoRow, error) {
	sql := todoListBase + " WHERE t.status != 'dismissed'"
	var args []any
	if status != "" {
		sql += " AND t.status = ?"
		args = append(args, status)
	}
	if topicID != nil {
		sql += " AND t.topic_id = ?"
		args = append(args, topicID.Int64())
	}
	sql += " ORDER BY t.id DESC LIMIT 200"
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, sql, args...)
	return rows, err
}

// ListByTopic 是 Topic 详情页的 todo 列表（含已完成）。
func (r *TodoRepo) ListByTopic(ctx context.Context, topicID ids.ID) ([]TodoRow, error) {
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, todoListBase+`
 WHERE t.topic_id = ? ORDER BY t.id DESC`, topicID.Int64())
	return rows, err
}

// ListBySession 是时间线详情页的 todo 列表。
func (r *TodoRepo) ListBySession(ctx context.Context, sessionID ids.ID) ([]TodoRow, error) {
	var rows []TodoRow
	err := r.DB.SelectContext(ctx, &rows, todoListBase+`
 WHERE m.session_id = ? ORDER BY t.id DESC`, sessionID.Int64())
	return rows, err
}
```

- [ ] **Step 4: 运行测试通过**

Run: `make test-integration`
Expected: repo 包全部 PASS（`go test ./internal/repo/` 无 DSN 时 CanTransition 用例仍 PASS，其余 SKIP）

- [ ] **Step 5: 提交**

```bash
git add internal/repo/todo.go internal/repo/todo_test.go
git commit -m "feat: todo DAO 与状态机校验"
```

---

### Task 6: memory 领域包——对话块聚合 + 窗口切分

**Files:**
- Create: `internal/memory/block.go`
- Test: `internal/memory/block_test.go`

- [ ] **Step 1: 写失败测试**

`internal/memory/block_test.go`：

```go
package memory

import (
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func seg(id int64, speaker string, text string, start, end int64) repo.TranscriptSegment {
	return repo.TranscriptSegment{ID: ids.ID(id), SpeakerLabel: speaker,
		Text: text, StartMS: start, EndMS: end}
}

func TestAggregateBlocks(t *testing.T) {
	segs := []repo.TranscriptSegment{
		seg(1, "1", "明天记得", 0, 1000),
		seg(2, "1", "给 Tom 发邮件", 1100, 2000), // 同说话人、间隔 100ms → 合并
		seg(3, "2", "好的", 2100, 2500),         // 换说话人 → 新块
		seg(4, "1", "另外一件事", 3000, 3500),     // 换回来 → 新块
		seg(5, "1", "隔了很久的话", 40000, 41000), // 同说话人但间隔 >30s → 强制切块
		seg(6, "1", "", 42000, 43000),           // 空文本 → 跳过
	}
	blocks := AggregateBlocks(segs, 30000)
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4: %+v", len(blocks), blocks)
	}
	b0 := blocks[0]
	if b0.Text != "明天记得给 Tom 发邮件" || b0.SpeakerLabel != "1" {
		t.Fatalf("b0 = %+v", b0)
	}
	if len(b0.SegmentIDs) != 2 || b0.SegmentIDs[0] != 1 || b0.SegmentIDs[1] != 2 {
		t.Fatalf("b0.SegmentIDs = %v", b0.SegmentIDs)
	}
	if b0.StartMS != 0 || b0.EndMS != 2000 {
		t.Fatalf("b0 时间 = %d-%d", b0.StartMS, b0.EndMS)
	}
	if blocks[3].Text != "隔了很久的话" || len(blocks[3].SegmentIDs) != 1 {
		t.Fatalf("b3 = %+v", blocks[3])
	}
}

func TestAggregateBlocksEmpty(t *testing.T) {
	if got := AggregateBlocks(nil, 30000); got != nil {
		t.Fatalf("nil in -> %v", got)
	}
	if got := AggregateBlocks([]repo.TranscriptSegment{seg(1, "1", "", 0, 100)}, 30000); len(got) != 0 {
		t.Fatalf("全空文本 -> %v", got)
	}
}

func TestSplitWindows(t *testing.T) {
	mk := func(n int) []Block {
		bs := make([]Block, n)
		for i := range bs {
			bs[i] = Block{Text: "b"}
		}
		return bs
	}
	// 不超过窗口大小：单窗口
	if w := SplitWindows(mk(10), 10); len(w) != 1 || len(w[0]) != 10 {
		t.Fatalf("10/10 -> %v", winLens(w))
	}
	// 超过：整窗 + 末尾残窗
	w := SplitWindows(mk(25), 10)
	if len(w) != 3 || len(w[0]) != 10 || len(w[2]) != 5 {
		t.Fatalf("25/10 -> %v", winLens(w))
	}
	// 空输入
	if w := SplitWindows(nil, 10); w != nil {
		t.Fatalf("nil -> %v", winLens(w))
	}
	// 窗口参数非法时回退默认 10
	if w := SplitWindows(mk(11), 0); len(w) != 2 {
		t.Fatalf("11/0 -> %v", winLens(w))
	}
}

func winLens(ws [][]Block) []int {
	out := make([]int, len(ws))
	for i, w := range ws {
		out[i] = len(w)
	}
	return out
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/memory/`
Expected: FAIL（`AggregateBlocks`、`SplitWindows` 未定义）

- [ ] **Step 3: 实现**

`internal/memory/block.go`：

```go
// Package memory 实现记忆抽取的纯逻辑：对话块聚合、窗口切分、候选解析、
// 质量闸门与 Topic 归属决策。全部不碰 DB 与网络，可完全单元测试。
// LLM 编排（Extractor）也在此包，但依赖以接口注入，测试用 fake。
package memory

import (
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// Block 是连续同说话人的段聚合成的对话块，抽取的最小输入单元。
type Block struct {
	SpeakerLabel string
	Text         string
	StartMS      int64
	EndMS        int64
	SegmentIDs   []ids.ID
}

// AggregateBlocks 把转写分段聚合为对话块：连续同说话人且间隔不超过 gapMS 的段
// 合并；换说话人或间隔超阈值则切块；空文本段跳过。
func AggregateBlocks(segs []repo.TranscriptSegment, gapMS int64) []Block {
	var blocks []Block
	for _, s := range segs {
		if s.Text == "" {
			continue
		}
		if n := len(blocks); n > 0 {
			last := &blocks[n-1]
			if last.SpeakerLabel == s.SpeakerLabel && s.StartMS-last.EndMS <= gapMS {
				last.Text += s.Text
				last.EndMS = s.EndMS
				last.SegmentIDs = append(last.SegmentIDs, s.ID)
				continue
			}
		}
		blocks = append(blocks, Block{
			SpeakerLabel: s.SpeakerLabel, Text: s.Text,
			StartMS: s.StartMS, EndMS: s.EndMS, SegmentIDs: []ids.ID{s.ID},
		})
	}
	return blocks
}

// SplitWindows 按窗口大小切分对话块（每窗口一次 LLM 调用）。
// window <= 0 时用默认 10；空输入返回 nil。
func SplitWindows(blocks []Block, window int) [][]Block {
	if window <= 0 {
		window = 10
	}
	if len(blocks) == 0 {
		return nil
	}
	if len(blocks) <= window {
		return [][]Block{blocks}
	}
	var out [][]Block
	for i := 0; i < len(blocks); i += window {
		end := i + window
		if end > len(blocks) {
			end = len(blocks)
		}
		out = append(out, blocks[i:end])
	}
	return out
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/memory/ -v`
Expected: PASS（3 个用例）

- [ ] **Step 5: 提交**

```bash
git add internal/memory/
git commit -m "feat: 对话块聚合与窗口切分（纯逻辑）"
```

---

### Task 7: memory 领域包——候选解析 + 质量闸门

**Files:**
- Create: `internal/memory/candidate.go`
- Test: `internal/memory/candidate_test.go`

- [ ] **Step 1: 写失败测试**

`internal/memory/candidate_test.go`：

```go
package memory

import (
	"testing"
	"time"

	"zhiwei/internal/ids"
)

func TestParseCandidatesHappyPath(t *testing.T) {
	raw := `{"candidates":[
	  {"type":"event","title":"发邮件","content":"明天需要给 Tom 发邮件确认设计稿",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":true,"todo_due":"2026-08-20T10:00:00Z","topic_id":null,
	   "suggested_topic_name":null,"block_index":1}
	]}`
	cands, err := ParseCandidates(raw)
	if err != nil {
		t.Fatalf("ParseCandidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("len = %d", len(cands))
	}
	c := cands[0]
	if c.Type != "event" || c.Title != "发邮件" || !c.IsTodo {
		t.Fatalf("cand = %+v", c)
	}
	if c.TodoDue == nil || c.TodoDue.UTC().Format(time.RFC3339) != "2026-08-20T10:00:00Z" {
		t.Fatalf("todo_due = %v", c.TodoDue)
	}
}

func TestParseCandidatesTolerance(t *testing.T) {
	// markdown 围栏（\x60 = 反引号，避免源码里嵌套代码围栏）+ 前后废话
	fence := "\x60\x60\x60json"
	raw := "好的，以下是结果：\n" + fence + "\n" +
		`{"candidates":[{"type":"fact","title":"学 Rust","content":"用户正在学习 Rust 计划三个月读完一本书",
	   "epistemic_type":"observed","importance":0.7,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topic_id":"123","suggested_topic_name":null,"block_index":2}]}` +
		"\n\x60\x60\x60\n以上。"
	cands, err := ParseCandidates(raw)
	if err != nil {
		t.Fatalf("ParseCandidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("len = %d", len(cands))
	}
	if cands[0].TopicID == nil || *cands[0].TopicID != 123 {
		t.Fatalf("topic_id = %v", cands[0].TopicID)
	}
	if cands[0].BlockIndex != 2 {
		t.Fatalf("block_index = %d", cands[0].BlockIndex)
	}
}

func TestParseCandidatesInvalid(t *testing.T) {
	if _, err := ParseCandidates("完全不是 JSON"); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	// 空候选合法（对话无值得记忆的内容）
	cands, err := ParseCandidates(`{"candidates":[]}`)
	if err != nil || len(cands) != 0 {
		t.Fatalf("空候选: %v %v", cands, err)
	}
}

func TestParseCandidatesBadDue(t *testing.T) {
	// todo_due 非法时间：保留候选，置空 due（不整体失败）
	raw := `{"candidates":[{"type":"event","title":"t","content":"八个字以上的内容描述",
	  "epistemic_type":"observed","importance":0.5,"confidence":0.9,
	  "is_todo":true,"todo_due":"not-a-date","topic_id":null,"suggested_topic_name":null,"block_index":1}]}`
	cands, err := ParseCandidates(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].TodoDue != nil {
		t.Fatalf("todo_due = %v, want nil", cands[0].TodoDue)
	}
}

func TestApplyGate(t *testing.T) {
	high := 0.9
	low := 0.5
	topicID := ids.ID(9)
	mk := func(conf float64, typ string) Candidate {
		return Candidate{Type: typ, Title: "t", Content: "这是一条足够长的内容描述",
			EpistemicType: "observed", Importance: 0.5, Confidence: conf,
			TopicID: &topicID}
	}
	cands := []Candidate{
		mk(high, "event"),        // 0: 通过
		mk(low, "event"),         // 1: 置信度不足，丢弃
		mk(high, "unknown-type"), // 2: 枚举外 type，丢弃
		{Type: "fact", Title: "t", Content: "太短", EpistemicType: "observed",
			Confidence: high}, // 3: 内容不足 8 字，丢弃
	}
	gated := ApplyGate(cands, GateConfig{MinConf: 0.6, TodoConf: 0.85})
	if len(gated) != 1 {
		t.Fatalf("gated = %d, want 1", len(gated))
	}
	if gated[0].Type != "event" {
		t.Fatalf("gated[0] = %+v", gated[0])
	}
}

func TestApplyGateTodoStatus(t *testing.T) {
	mkTodo := func(conf float64) Candidate {
		return Candidate{Type: "event", Title: "t", Content: "明天需要给 Tom 发邮件确认设计稿",
			EpistemicType: "observed", Importance: 0.5, Confidence: conf, IsTodo: true}
	}
	gated := ApplyGate([]Candidate{mkTodo(0.9), mkTodo(0.7)},
		GateConfig{MinConf: 0.6, TodoConf: 0.85})
	if len(gated) != 2 {
		t.Fatalf("len = %d", len(gated))
	}
	if gated[0].TodoStatus != "confirmed" {
		t.Fatalf("高置信 todo 应 confirmed，got %s", gated[0].TodoStatus)
	}
	if gated[1].TodoStatus != "suggested" {
		t.Fatalf("低置信 todo 应 suggested，got %s", gated[1].TodoStatus)
	}
	// 非 todo 不填状态
	gated2 := ApplyGate([]Candidate{{Type: "fact", Title: "t",
		Content: "这是一条足够长的内容描述", EpistemicType: "observed", Confidence: 0.9}},
		GateConfig{MinConf: 0.6, TodoConf: 0.85})
	if gated2[0].TodoStatus != "" {
		t.Fatalf("非 todo TodoStatus = %s", gated2[0].TodoStatus)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/memory/`
Expected: FAIL（`ParseCandidates`、`ApplyGate`、`Candidate` 未定义）

- [ ] **Step 3: 实现**

`internal/memory/candidate.go`：

```go
package memory

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"zhiwei/internal/ids"
)

// Candidate 是 LLM 输出的一条记忆候选（闸门前后的载体）。
type Candidate struct {
	Type               string // event|fact|decision|idea|problem|preference
	Title              string
	Content            string
	EpistemicType      string // observed|inferred|suggested
	Importance         float64
	Confidence         float64
	IsTodo             bool
	TodoDue            *time.Time
	TopicID            *ids.ID // LLM 直接给的已有 topic 归属
	SuggestedTopicName string  // LLM 建议的新主题名

	// 以下由编排层填充（LLM 不产出）
	BlockIndex int       // 候选来源块在窗口内的序号（1-based，0=未知）
	SegmentIDs []ids.ID  // provenance：来源块的 segment id 列表
	EventAt    time.Time // 近似事件时间 = 会话基准 + 块起点偏移
	TodoStatus string    // suggested|confirmed；非 todo 为空（闸门填充）
}

var validTypes = map[string]bool{
	"event": true, "fact": true, "decision": true,
	"idea": true, "problem": true, "preference": true,
}

var validEpistemic = map[string]bool{
	"observed": true, "inferred": true, "suggested": true,
}

type rawCandidate struct {
	Type               string  `json:"type"`
	Title              string  `json:"title"`
	Content            string  `json:"content"`
	EpistemicType      string  `json:"epistemic_type"`
	Importance         float64 `json:"importance"`
	Confidence         float64 `json:"confidence"`
	IsTodo             bool    `json:"is_todo"`
	TodoDue            string  `json:"todo_due"`
	TopicID            *string `json:"topic_id"`
	SuggestedTopicName string  `json:"suggested_topic_name"`
	BlockIndex         int     `json:"block_index"`
}

// ParseCandidates 解析 LLM 输出为候选列表。容错：截取首个 { 到末个 }，
// 天然剥掉模型可能输出的前后废话与 markdown 代码围栏。
// 彻底非法的 JSON 返回 error（由 stage 走重试）；
// 字段级问题（非法时间等）降级处理不整体失败。
func ParseCandidates(raw string) ([]Candidate, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out struct {
		Candidates []rawCandidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("抽取结果解析失败: %w", err)
	}
	cands := make([]Candidate, 0, len(out.Candidates))
	for _, rc := range out.Candidates {
		c := Candidate{
			Type:               rc.Type,
			Title:              rc.Title,
			Content:            rc.Content,
			EpistemicType:      rc.EpistemicType,
			Importance:         clamp01(rc.Importance),
			Confidence:         clamp01(rc.Confidence),
			IsTodo:             rc.IsTodo,
			SuggestedTopicName: strings.TrimSpace(rc.SuggestedTopicName),
			BlockIndex:         rc.BlockIndex,
		}
		if rc.TodoDue != "" {
			if du, err := time.Parse(time.RFC3339, rc.TodoDue); err == nil {
				c.TodoDue = &du
			} // 非法时间：置空保留候选
		}
		if rc.TopicID != nil {
			if id, err := ids.ParseID(*rc.TopicID); err == nil {
				c.TopicID = &id
			} // 非法 id：视为无归属
		}
		cands = append(cands, c)
	}
	return cands, nil
}

// GateConfig 是质量闸门阈值（来自配置）。
type GateConfig struct {
	MinConf  float64 // 候选最低置信度，低于丢弃
	TodoConf float64 // todo 直接入库为 confirmed 的阈值，低于降级 suggested
}

// ApplyGate 应用质量闸门：枚举外类型、置信度不足、内容过短的候选丢弃；
// todo 按置信度决定 suggested/confirmed。返回通过者。
func ApplyGate(cands []Candidate, cfg GateConfig) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if !validTypes[c.Type] || !validEpistemic[c.EpistemicType] {
			continue
		}
		if c.Confidence < cfg.MinConf {
			continue
		}
		if len([]rune(c.Content)) < 8 {
			continue
		}
		if c.IsTodo {
			if c.Confidence >= cfg.TodoConf {
				c.TodoStatus = "confirmed"
			} else {
				c.TodoStatus = "suggested"
			}
		}
		out = append(out, c)
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/memory/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/memory/
git commit -m "feat: LLM 候选解析容错与质量闸门（纯逻辑）"
```

---

### Task 8: memory 领域包——Topic 归属决策

**Files:**
- Create: `internal/memory/topic.go`
- Test: `internal/memory/topic_test.go`

- [ ] **Step 1: 写失败测试**

`internal/memory/topic_test.go`：

```go
package memory

import (
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func TestResolveTopics(t *testing.T) {
	rustID := ids.ID(101)
	oldID := ids.ID(102) // dismissed，不可挂
	topics := []repo.Topic{
		{ID: rustID, Name: "Rust 学习", Status: "active"},
		{ID: oldID, Name: "旧主题", Status: "dismissed"},
	}

	cand := func(topicID *ids.ID, name string) Candidate {
		return Candidate{Type: "fact", Title: "t", Content: "足够长的一条内容描述",
			EpistemicType: "observed", Confidence: 0.9, TopicID: topicID, SuggestedTopicName: name}
	}
	rustStr := rustID
	badStr := ids.ID(999)

	cands := []Candidate{
		cand(&rustStr, ""),    // 0: 合法 topic_id → 挂 Rust
		cand(&badStr, ""),     // 1: 不存在的 topic_id → 未归类
		cand(nil, "Rust 学习"), // 2: 同名建议 → 合并到已有
		cand(nil, "爸妈健康"),  // 3: 新建议 → 需新建
		cand(nil, "爸妈健康"),  // 4: 同名新建议 → 与 3 共享一个新 topic
		cand(nil, ""),         // 5: 无归属
	}
	refs, newNames := ResolveTopics(cands, topics)

	if len(refs) != 6 {
		t.Fatalf("refs = %d", len(refs))
	}
	if refs[0].ExistingID == nil || *refs[0].ExistingID != rustID {
		t.Fatalf("refs[0] = %+v", refs[0])
	}
	if refs[1].ExistingID != nil || refs[1].NewName != "" {
		t.Fatalf("refs[1] 应未归类: %+v", refs[1])
	}
	if refs[2].ExistingID == nil || *refs[2].ExistingID != rustID {
		t.Fatalf("refs[2] 应合并同名: %+v", refs[2])
	}
	if refs[3].NewName != "爸妈健康" {
		t.Fatalf("refs[3] = %+v", refs[3])
	}
	if refs[5].ExistingID != nil || refs[5].NewName != "" {
		t.Fatalf("refs[5] 应未归类: %+v", refs[5])
	}
	// 新建列表去重
	if len(newNames) != 1 || newNames[0] != "爸妈健康" {
		t.Fatalf("newNames = %v", newNames)
	}
}

func TestResolveTopicsNoExisting(t *testing.T) {
	cands := []Candidate{{Type: "fact", Title: "t", Content: "足够长的一条内容描述",
		EpistemicType: "observed", Confidence: 0.9, SuggestedTopicName: "新主题"}}
	refs, newNames := ResolveTopics(cands, nil)
	if refs[0].NewName != "新主题" || len(newNames) != 1 {
		t.Fatalf("refs=%v newNames=%v", refs, newNames)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/memory/`
Expected: FAIL（`ResolveTopics` 未定义）

- [ ] **Step 3: 实现**

`internal/memory/topic.go`：

```go
package memory

import (
	"strings"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TopicRef 是一条候选的 Topic 归属决策结果。
// ExistingID 与 NewName 互斥；两者皆空 = 未归类。
type TopicRef struct {
	ExistingID *ids.ID // 挂到已有 topic
	NewName    string  // 需新建的 topic 名（commit 时创建后回填 id）
}

// ResolveTopics 为每条候选决定 Topic 归属，并收集需要新建的主题名（去重）。
// 规则：
//   - topic_id 指向本 user 的 active/suggested topic → 直接挂
//   - 否则有 suggested_topic_name → 查同名 active/suggested topic，命中合并；未命中收集为新建
//   - 都没有 → 未归类
func ResolveTopics(cands []Candidate, existing []repo.Topic) (refs []TopicRef, newNames []string) {
	byID := map[ids.ID]bool{}
	byName := map[string]ids.ID{}
	for _, tp := range existing {
		if tp.Status == "dismissed" {
			continue
		}
		byID[tp.ID] = true
		byName[tp.Name] = tp.ID
	}
	seen := map[string]bool{}
	refs = make([]TopicRef, len(cands))
	for i, c := range cands {
		switch {
		case c.TopicID != nil && byID[*c.TopicID]:
			id := *c.TopicID
			refs[i] = TopicRef{ExistingID: &id}
		case c.SuggestedTopicName != "":
			name := strings.TrimSpace(c.SuggestedTopicName)
			if name == "" {
				break
			}
			if id, ok := byName[name]; ok {
				refs[i] = TopicRef{ExistingID: &id}
			} else {
				refs[i] = TopicRef{NewName: name}
				if !seen[name] {
					seen[name] = true
					newNames = append(newNames, name)
				}
			}
		}
	}
	return refs, newNames
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/memory/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/memory/
git commit -m "feat: Topic 归属决策（纯逻辑：直挂/同名合并/新建建议）"
```

---

### Task 9: memory 领域包——Extractor（LLM 编排）

**Files:**
- Create: `internal/memory/extract.go`
- Test: `internal/memory/extract_test.go`

- [ ] **Step 1: 写失败测试**

`internal/memory/extract_test.go`：

```go
package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// fakeLLM 按调用次序弹出预置响应
type fakeLLM struct {
	responses []string
	calls     int
	lastUser  string
}

func (f *fakeLLM) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	if f.calls >= len(f.responses) {
		return provider.ChatResponse{}, fmt.Errorf("fakeLLM: 无预置响应（第 %d 次调用）", f.calls+1)
	}
	resp := f.responses[f.calls]
	f.calls++
	f.lastUser = req.User
	return provider.ChatResponse{Content: resp, TotalTokens: 100}, nil
}

func mkBlocks(n int) []Block {
	bs := make([]Block, n)
	for i := range bs {
		bs[i] = Block{SpeakerLabel: "1", Text: fmt.Sprintf("第%d块内容", i+1),
			StartMS: int64(i) * 1000, EndMS: int64(i)*1000 + 900, SegmentIDs: []ids.ID{int64(i + 1)}}
	}
	return bs
}

func TestExtractorSingleWindow(t *testing.T) {
	llm := &fakeLLM{responses: []string{`{"candidates":[
	  {"type":"event","title":"发邮件","content":"明天需要给 Tom 发邮件确认设计稿",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":true,"todo_due":null,"topic_id":null,"suggested_topic_name":null,"block_index":2}
	]}`}}
	ex := &Extractor{LLM: llm, Model: "fake-model", Prompt: "系统指令", Window: 10}
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)

	blocks := []Block{
		{SpeakerLabel: "1", Text: "块一", StartMS: 0, EndMS: 500, SegmentIDs: []ids.ID{11}},
		{SpeakerLabel: "2", Text: "块二", StartMS: 1000, EndMS: 1500, SegmentIDs: []ids.ID{12}},
	}
	cands, err := ex.Extract(context.Background(), blocks, nil, base)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("calls = %d, want 1", llm.calls)
	}
	if len(cands) != 1 {
		t.Fatalf("cands = %d", len(cands))
	}
	c := cands[0]
	if c.BlockIndex != 2 {
		t.Fatalf("block_index = %d", c.BlockIndex)
	}
	// provenance：block_index=2 → 第二块的 segment
	if len(c.SegmentIDs) != 1 || c.SegmentIDs[0] != 12 {
		t.Fatalf("SegmentIDs = %v", c.SegmentIDs)
	}
	// EventAt = base + 块二 start(1000ms)
	if !c.EventAt.Equal(base.Add(time.Second)) {
		t.Fatalf("EventAt = %v", c.EventAt)
	}
	// 用户消息包含块列表与主题占位
	if !strings.Contains(llm.lastUser, "块二") || !strings.Contains(llm.lastUser, "暂无") {
		t.Fatalf("user msg = %s", llm.lastUser)
	}
	// System prompt 透传
	if ex.Prompt != "系统指令" {
		t.Fatalf("prompt = %s", ex.Prompt)
	}
}

func TestExtractorMultiWindowAndDedup(t *testing.T) {
	// 12 块、窗口 5 → 3 次调用；两个窗口产出同 title+content 的候选 → 去重保留高置信
	same := `{"candidates":[{"type":"fact","title":"学 Rust","content":"用户正在学习 Rust 计划三个月读完一本书",
	  "epistemic_type":"observed","importance":0.7,"confidence":0.75,
	  "is_todo":false,"todo_due":null,"topic_id":null,"suggested_topic_name":null,"block_index":1}]}`
	high := `{"candidates":[{"type":"fact","title":"学 Rust","content":"用户正在学习 Rust 计划三个月读完一本书",
	  "epistemic_type":"observed","importance":0.7,"confidence":0.95,
	  "is_todo":false,"todo_due":null,"topic_id":null,"suggested_topic_name":null,"block_index":2}]}`
	empty := `{"candidates":[]}`
	llm := &fakeLLM{responses: []string{same, high, empty}}

	ex := &Extractor{LLM: llm, Model: "fake-model", Prompt: "sys", Window: 5}
	blocks := mkBlocks(12)
	cands, err := ex.Extract(context.Background(), blocks, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 3 {
		t.Fatalf("calls = %d, want 3", llm.calls)
	}
	if len(cands) != 1 {
		t.Fatalf("去重后 cands = %d, want 1", len(cands))
	}
	if cands[0].Confidence != 0.95 {
		t.Fatalf("应保留高置信版本，got %v", cands[0].Confidence)
	}
	// 高置信版本来自第 2 窗口的 block_index=2 → 全局第 7 块（下标 6）
	if want := blocks[6].SegmentIDs[0]; cands[0].SegmentIDs[0] != want {
		t.Fatalf("SegmentIDs[0] = %v, want %v", cands[0].SegmentIDs[0], want)
	}
}

func TestExtractorInvalidBlockIndex(t *testing.T) {
	// block_index 越界 → 用整个窗口的 segment 并集兜底
	llm := &fakeLLM{responses: []string{`{"candidates":[
	  {"type":"fact","title":"t","content":"足够长的一条内容描述内容",
	   "epistemic_type":"observed","importance":0.5,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topic_id":null,"suggested_topic_name":null,"block_index":99}
	]}`}}
	ex := &Extractor{LLM: llm, Model: "m", Prompt: "sys", Window: 10}
	blocks := []Block{
		{SpeakerLabel: "1", Text: "块一", StartMS: 0, EndMS: 500, SegmentIDs: []ids.ID{1}},
		{SpeakerLabel: "1", Text: "块二", StartMS: 1000, EndMS: 1500, SegmentIDs: []ids.ID{2}},
	}
	cands, err := ex.Extract(context.Background(), blocks, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands[0].SegmentIDs) != 2 {
		t.Fatalf("SegmentIDs = %v, want 2 个", cands[0].SegmentIDs)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/memory/`
Expected: FAIL（`Extractor` 未定义）

- [ ] **Step 3: 实现**

`internal/memory/extract.go`：

```go
package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// Extractor 用 LLM 从对话块抽取记忆候选：按窗口逐次调用、合并去重、
// 填充 provenance（SegmentIDs）与近似事件时间（EventAt）。
type Extractor struct {
	LLM    provider.LLMProvider
	Model  string // 模型名（Tier 1 flash）
	Prompt string // 系统指令（prompts/extraction_v1.md 内容，含版本说明）
	Window int    // 窗口大小（块数），<=0 时 SplitWindows 内部回退默认
}

// Extract 抽取全部对话块。baseTime 是会话基准时间（session.created_at），
// EventAt = baseTime + 块 start_ms 偏移。
// 跨窗口同 title+content 的候选视为重复，保留置信度高者。
func (e *Extractor) Extract(ctx context.Context, blocks []Block, topics []repo.Topic, baseTime time.Time) ([]Candidate, error) {
	var all []Candidate
	seen := map[string]int{} // title\x00content -> 在 all 中的下标
	for _, win := range SplitWindows(blocks, e.Window) {
		resp, err := e.LLM.Chat(ctx, provider.ChatRequest{
			Model:  e.Model,
			System: e.Prompt,
			User:   buildUserMessage(win, topics),
		})
		if err != nil {
			return nil, fmt.Errorf("LLM 调用: %w", err)
		}
		cands, err := ParseCandidates(resp.Content)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			c.SegmentIDs, c.EventAt = blockProvenance(win, c.BlockIndex, baseTime)
			key := c.Title + "\x00" + c.Content
			if idx, ok := seen[key]; ok {
				if c.Confidence > all[idx].Confidence {
					all[idx] = c
				}
				continue
			}
			seen[key] = len(all)
			all = append(all, c)
		}
	}
	return all, nil
}

// blockProvenance 由 block_index 定位来源块；越界时用整个窗口的
// segment 并集与窗口起点兜底（宁粗勿丢）。
func blockProvenance(win []Block, idx int, base time.Time) ([]ids.ID, time.Time) {
	if idx >= 1 && idx <= len(win) {
		b := win[idx-1]
		return b.SegmentIDs, base.Add(time.Duration(b.StartMS) * time.Millisecond)
	}
	var segs []ids.ID
	start := int64(0)
	if len(win) > 0 {
		start = win[0].StartMS
		for _, b := range win {
			segs = append(segs, b.SegmentIDs...)
		}
	}
	return segs, base.Add(time.Duration(start) * time.Millisecond)
}

// buildUserMessage 组装用户消息：对话块列表 + 已有主题列表。
func buildUserMessage(win []Block, topics []repo.Topic) string {
	var sb strings.Builder
	sb.WriteString("对话块列表（格式：序号|说话人|时间偏移|文本）：\n")
	for i, b := range win {
		speaker := b.SpeakerLabel
		if speaker == "" {
			speaker = "未知"
		}
		fmt.Fprintf(&sb, "%d|%s|%s|%s\n", i+1, speaker, fmtOffset(b.StartMS), b.Text)
	}
	sb.WriteString("\n已有主题列表（格式：topic_id|名称），请优先归入已有主题：\n")
	if len(topics) == 0 {
		sb.WriteString("（暂无）\n")
	}
	for _, tp := range topics {
		fmt.Fprintf(&sb, "%s|%s\n", tp.ID.String(), tp.Name)
	}
	return sb.String()
}

// fmtOffset 毫秒 → HH:MM:SS
func fmtOffset(ms int64) string {
	s := ms / 1000
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/memory/ -v`
Expected: PASS（Task 7/8 用例 + 本任务 3 个用例）

- [ ] **Step 5: 提交**

```bash
git add internal/memory/
git commit -m "feat: Extractor LLM 编排（窗口循环/合并去重/provenance）"
```

---

### Task 10: 抽取 prompt 文件

**Files:**
- Create: `prompts/extraction_v1.md`

prompt 版本化：文件即 system prompt 全文，运行时由 main.go 读取（见 Task 15），版本号体现在文件名与正文首行，记入 job.trace 由 Task 11 实现。

- [ ] **Step 1: 写 prompt**

`prompts/extraction_v1.md`：

````markdown
# 知微记忆抽取 prompt（版本：extraction_v1）

你是个人 AI 记忆助手「知微」的记忆抽取器。输入是一段对话转写（已按说话人聚合为对话块，每块带序号）。你的任务：从对话中提取值得长期记住的记忆候选，并归入已有主题或建议新主题。

## 抽取规则

1. 只提取明确说出口的信息，不要推测对话双方没说的内容
2. 每条候选必须独立可读：content 用完整的一句话，包含必要的主语与时间
3. type 只能取：event（发生的事）、fact（事实/知识）、decision（决定）、idea（想法）、problem（问题/困扰）、preference（偏好/习惯）
4. epistemic_type：对话里明确说到 = observed；你从对话推断的 = inferred；你建议补充的 = suggested
5. importance 取 0~1：日常琐事 0.3 以下；对用户有意义 0.5~0.7；影响计划/关系/健康 0.8 以上
6. confidence 取 0~1：转写清晰明确 0.9 以上；有歧义或推断成分高则降低
7. 对话中出现的承诺/待办/约定置 is_todo=true，尽量给出 todo_due（ISO 8601 含时区，如 2026-08-20T10:00:00+08:00）；没有明确时间则 null
8. topic 归属：优先使用「已有主题列表」中的 topic_id（原样引用该 id）；都不合适才给 suggested_topic_name（简短名词短语，如「Rust 学习」「爸妈健康」）；确实无关则两者都为 null
9. 每个对话块最多产出 2 条候选，整批最多 10 条，宁缺毋滥
10. 每条候选输出 block_index（来源对话块的序号，对应输入列表中的序号）

## 输出格式

只输出 JSON，不要任何其他文字或代码围栏。无值得记忆的内容时输出 {"candidates":[]}。

{"candidates":[{"type":"event","title":"给 Tom 发邮件","content":"明天需要给 Tom 发邮件确认设计稿","epistemic_type":"observed","importance":0.6,"confidence":0.9,"is_todo":true,"todo_due":null,"topic_id":null,"suggested_topic_name":null,"block_index":1}]}
````

- [ ] **Step 2: 提交**

```bash
git add prompts/
git commit -m "feat: 记忆抽取 prompt v1（版本化）"
```

---

### Task 11: Handler 签名扩展 + trace 辅助 + extract stage

**Files:**
- Modify: `internal/pipeline/pool.go`（Handler 增加 `*repo.Job` 参数）
- Modify: `internal/pipeline/stage_asr.go`（StageDeps 扩展 + handler 适配签名）
- Modify: `internal/pipeline/pool_test.go`（fake handler 适配签名）
- Create: `internal/pipeline/trace.go`
- Create: `internal/pipeline/stage_extract.go`
- Test: `internal/pipeline/stage_extract_test.go`

说明：handler 要写 job.trace，而 trace 由 pool 在 handler 返回后统一持久化。把 job 传进 handler，handler 原地修改 `j.Trace`，pool 现有 `persist` 已经保存整个 job——无读写竞争，改动最小。

- [ ] **Step 1: 改 Handler 签名（pool.go）**

```go
// Handler 执行一个 stage。job 是当前任务（可向 j.Trace 追加执行记录，
// pool 会在 handler 返回后持久化）；sessionID 是流水线的处理对象。
// 返回 nil 即成功，状态机推进到下一 stage。
type Handler func(ctx context.Context, j *repo.Job, sessionID ids.ID) error
```

`claimAndRun` 中调用处改为：

```go
		runErr = safeRun(ctx, h, j, j.SessionID)
```

`safeRun` 改为：

```go
func safeRun(ctx context.Context, h Handler, j *repo.Job, sid ids.ID) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, j, sid)
}
```

- [ ] **Step 2: 适配现有 handler（stage_asr.go）**

两个 handler 的签名改为忽略 job 参数：

```go
func stageASR(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
```

```go
func stageSegment(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
```

`StageDeps` 扩展字段（保留原有四项，追加 Sprint 2 抽取依赖）：

```go
// StageDeps 是 stage 的依赖集合（接口化便于测试注入）。
type StageDeps struct {
	Sessions    *repo.SessionRepo
	Transcripts *repo.TranscriptRepo
	ASR         provider.ASRProvider
	DataDir     string // 转码输出目录

	// ---- Sprint 2：extract stage ----
	DB            *sqlx.DB            // 开启 commit 事务用
	Memories      *repo.MemoryRepo
	Todos         *repo.TodoRepo
	Topics        *repo.TopicRepo
	LLM           provider.LLMProvider
	LLMModel      string // Tier 1 flash 模型名
	Prompt        string // prompts/extraction_v1.md 内容（system prompt）
	ExtractWindow int    // 窗口切分大小（块数），0 = 用默认
	Gate          memory.GateConfig    // 质量闸门阈值
}
```

`BuildStages` 注册 extract：

```go
func BuildStages(d StageDeps) map[string]Handler {
	return map[string]Handler{
		"asr":     stageASR(d),
		"segment": stageSegment(d),
		"extract": stageExtract(d),
	}
}
```

import 增加 `"github.com/jmoiron/sqlx"` 与 `"zhiwei/internal/memory"`。

`pool_test.go` 中 fake handler 签名同步：

```go
		handlers := map[string]Handler{
			"asr":     func(ctx context.Context, _ *repo.Job, _ ids.ID) error { return nil },
			"segment": func(ctx context.Context, _ *repo.Job, _ ids.ID) error { return nil },
		}
```

- [ ] **Step 3: 写 trace 辅助（trace.go）**

`internal/pipeline/trace.go`：

```go
// trace.go 提供 job.trace 追加辅助。handler 原地修改 j.Trace，
// 由 pool 在 handler 返回后统一持久化（无读写竞争）。
package pipeline

import (
	"encoding/json"
	"time"

	"zhiwei/internal/repo"
)

// appendTrace 向 job.Trace 追加一条执行记录。
// 注意 Job.Trace 是 *json.RawMessage（可能为 nil）。
func appendTrace(j *repo.Job, e repo.TraceEntry) {
	var entries []repo.TraceEntry
	if j.Trace != nil && len(*j.Trace) > 0 {
		_ = json.Unmarshal(*j.Trace, &entries)
	}
	e.At = time.Now()
	entries = append(entries, e)
	b, err := json.Marshal(entries)
	if err == nil {
		raw := json.RawMessage(b)
		j.Trace = &raw
	}
}
```

- [ ] **Step 4: 写失败测试（extract stage 端到端，fake LLM）**

`internal/pipeline/stage_extract_test.go`：

```go
package pipeline

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// fakeExtractLLM 固定响应（含 1 条 todo 候选 + 1 条挂已有 topic 的候选）
type fakeExtractLLM struct{ calls int }

func (f *fakeExtractLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	f.calls++
	return provider.ChatResponse{Content: `{"candidates":[
	  {"type":"event","title":"给 Tom 发邮件","content":"明天需要给 Tom 发邮件确认设计稿",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":true,"todo_due":null,"topic_id":null,"suggested_topic_name":"工作沟通","block_index":1},
	  {"type":"fact","title":"学习 Rust","content":"用户正在学习 Rust 计划三个月内读完一本书",
	   "epistemic_type":"observed","importance":0.7,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topic_id":null,"suggested_topic_name":"Rust 学习","block_index":2}
	]}`, TotalTokens: 500}, nil
}

func setupExtractFixture(t *testing.T, deps *StageDeps) (sid ids.ID, rustTopic *repo.Topic) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 预置已有 topic：第二条候选的 suggested_topic_name 与之同名 → 验证合并
	rustTopic = &repo.Topic{Name: "Rust 学习", Status: "active", CreatedBy: "user"}
	if err := deps.Topics.Create(ctx, rustTopic); err != nil {
		t.Fatal(err)
	}

	sid = ids.New()
	if err := deps.Sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "x.wav",
		StoragePath: "/tmp/x.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := deps.Transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	conf := 0.95
	if err := deps.Transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1",
			Text: "明天记得给 Tom 发邮件确认设计稿", StartMS: 0, EndMS: 2000, Confidence: &conf},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "1",
			Text: "对了，我最近在学 Rust，打算三个月内读完那本书", StartMS: 2100, EndMS: 4000, Confidence: &conf},
	}); err != nil {
		t.Fatal(err)
	}
	return sid, rustTopic
}

func newExtractDeps(t *testing.T, llm *fakeExtractLLM) StageDeps {
	t.Helper()
	db, _ := repo.NewDB(repo.TestDSN(t))
	return StageDeps{
		Sessions:    &repo.SessionRepo{DB: db},
		Transcripts: &repo.TranscriptRepo{DB: db},
		DB:          db,
		Memories:    &repo.MemoryRepo{DB: db},
		Todos:       &repo.TodoRepo{DB: db},
		Topics:      &repo.TopicRepo{DB: db},
		LLM:         llm,
		LLMModel:    "fake-model",
		Prompt:      "测试 system prompt",
		ExtractWindow: 10,
		Gate:        memory.GateConfig{MinConf: 0.6, TodoConf: 0.85},
	}
}

func TestStageExtractCommit(t *testing.T) {
	llm := &fakeExtractLLM{}
	d := newExtractDeps(t, llm)
	sid, rustTopic := setupExtractFixture(t, &d)
	ctx := context.Background()

	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sid, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sid); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// memory：2 条落库
	mems, err := d.Memories.ListBySession(ctx, sid)
	if err != nil || len(mems) != 2 {
		t.Fatalf("memories = %d err=%v", len(mems), err)
	}
	// provenance：todo 候选来自块 1
	var todoMem *repo.MemoryRow
	for i := range mems {
		if mems[i].Title == "给 Tom 发邮件" {
			todoMem = &mems[i]
		}
	}
	if todoMem == nil {
		t.Fatal("未找到 todo 来源 memory")
	}
	if len(todoMem.TranscriptSegmentIDs) != 1 {
		t.Fatalf("segment_ids = %v", todoMem.TranscriptSegmentIDs)
	}

	// todo：confidence 0.9 >= 0.85 → confirmed
	todos, err := d.Todos.ListBySession(ctx, sid)
	if err != nil || len(todos) != 1 {
		t.Fatalf("todos = %d err=%v", len(todos), err)
	}
	if todos[0].Status != "confirmed" {
		t.Fatalf("todo status = %s, want confirmed", todos[0].Status)
	}
	if todos[0].SourceMemoryID == nil || *todos[0].SourceMemoryID != todoMem.ID {
		t.Fatalf("source_memory_id = %v", todos[0].SourceMemoryID)
	}

	// topic：「工作沟通」新建为 suggested；「Rust 学习」合并到已有 topic
	workTopic, err := d.Topics.FindActiveByName(ctx, 1, "工作沟通")
	if err != nil || workTopic == nil {
		t.Fatalf("工作沟通 topic 未创建: %v %v", workTopic, err)
	}
	if workTopic.Status != "suggested" || workTopic.CreatedBy != "ai" {
		t.Fatalf("工作沟通 = %+v", workTopic)
	}
	var rustMem *repo.MemoryRow
	for i := range mems {
		if mems[i].Title == "学习 Rust" {
			rustMem = &mems[i]
		}
	}
	if rustMem == nil || rustMem.TopicID == nil || *rustMem.TopicID != rustTopic.ID {
		t.Fatalf("Rust memory 未挂已有 topic: %+v", rustMem)
	}
	// todo 继承来源 memory 的 topic（工作沟通）
	if todos[0].TopicID == nil || *todos[0].TopicID != workTopic.ID {
		t.Fatalf("todo topic = %v, want 工作沟通", todos[0].TopicID)
	}

	// trace 已记录
	if j.Trace == nil || len(*j.Trace) == 0 {
		t.Fatal("job.trace 未写入")
	}
}

func TestStageExtractIdempotent(t *testing.T) {
	llm := &fakeExtractLLM{}
	d := newExtractDeps(t, llm)
	sid, _ := setupExtractFixture(t, &d)
	ctx := context.Background()

	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sid, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sid); err != nil {
		t.Fatal(err)
	}
	if err := handler(ctx, j, sid); err != nil { // 重跑
		t.Fatal(err)
	}
	mems, _ := d.Memories.ListBySession(ctx, sid)
	if len(mems) != 2 {
		t.Fatalf("重跑后 memories = %d, want 2（幂等）", len(mems))
	}
	todos, _ := d.Todos.ListBySession(ctx, sid)
	if len(todos) != 1 {
		t.Fatalf("重跑后 todos = %d, want 1（幂等）", len(todos))
	}
}

func TestStageExtractEmptyTranscript(t *testing.T) {
	llm := &fakeExtractLLM{}
	d := newExtractDeps(t, llm)
	// 新建一个全空文本的会话
	ctx := context.Background()
	sid2 := ids.New()
	_ = d.Sessions.Create(ctx, &repo.AudioSession{
		ID: sid2, Source: "web_upload", Filename: "e.wav",
		StoragePath: "/tmp/e.wav", Status: "processing",
	})
	tr2 := &repo.Transcript{SessionID: sid2, Language: "zh-CN"}
	_ = d.Transcripts.Create(ctx, tr2)
	_ = d.Transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr2.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "", StartMS: 0, EndMS: 100},
	})

	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sid2, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sid2); err != nil {
		t.Fatalf("空会话 extract 应成功: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("空会话不应调 LLM，calls = %d", llm.calls)
	}
	mems, _ := d.Memories.ListBySession(ctx, sid2)
	if len(mems) != 0 {
		t.Fatalf("空会话 memories = %d", len(mems))
	}
}
```

- [ ] **Step 5: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（`stageExtract` 未定义；注：Handler 签名改动后 `go build ./...` 应已通过）

- [ ] **Step 6: 实现 stage_extract.go**

`internal/pipeline/stage_extract.go`：

```go
// stage_extract 实现抽取 stage：对话块聚合 → LLM 抽取 → 质量闸门 →
// Topic 归属 → 单事务提交（memory + todo + 建议 topic）。
// 合并了上游 spec 的 extract/quality/commit 三步，理由见 Sprint 2 设计文档 §2：
// 中间产物无落库位置，质量闸门纯规则无独立重试价值，重跑整段代价可接受（幂等保证不重复）。
package pipeline

import (
	"context"
	"fmt"
	"log"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/repo"
)

// blockGapMS 是对话块聚合的间隔阈值：同说话人相邻段间隔超过此值强制切块。
const blockGapMS = 30000

// topicPromptLimit 是进入抽取 prompt 的已有主题数上限。
const topicPromptLimit = 30

func stageExtract(d StageDeps) Handler {
	return func(ctx context.Context, j *repo.Job, sessionID ids.ID) error {
		s, err := d.Sessions.Get(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读取 session: %w", err)
		}
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读取 transcript: %w", err)
		}
		segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
		if err != nil {
			return fmt.Errorf("读取 segments: %w", err)
		}

		// ① 对话块聚合；无有效文字的会话直接完成（低价值不进抽取）
		blocks := memory.AggregateBlocks(segs, blockGapMS)
		if len(blocks) == 0 {
			appendTrace(j, repo.TraceEntry{Stage: "extract", MS: 0, Error: "无有效文字，跳过抽取"})
			return nil
		}

		// ② + ③ LLM 抽取（窗口切分在 Extractor 内部）
		topics, err := d.Topics.ListActive(ctx, s.UserID, topicPromptLimit)
		if err != nil {
			return fmt.Errorf("读取 topics: %w", err)
		}
		ex := &memory.Extractor{LLM: d.LLM, Model: d.LLMModel, Prompt: d.Prompt, Window: d.ExtractWindow}
		llmBegin := time.Now()
		cands, err := ex.Extract(ctx, blocks, topics, s.CreatedAt)
		if err != nil {
			return fmt.Errorf("抽取: %w", err)
		}
		appendTrace(j, repo.TraceEntry{Stage: "extract:llm", Model: d.LLMModel, MS: msSince(llmBegin)})

		// ④ 质量闸门
		gated := memory.ApplyGate(cands, d.Gate)

		// ⑤ Topic 归属决策（纯逻辑）+ 单事务提交
		refs, newNames := memory.ResolveTopics(gated, topics)
		commitBegin := time.Now()
		err = commitExtract(ctx, d, sessionID, gated, refs, newNames)
		if err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		appendTrace(j, repo.TraceEntry{
			Stage: "extract:commit", MS: msSince(commitBegin),
			Error: fmt.Sprintf("候选=%d 通过=%d 新topic=%d", len(cands), len(gated), len(newNames)),
		})
		log.Printf("[extract] session=%s blocks=%d 候选=%d 通过闸门=%d 新topic=%d",
			sessionID, len(blocks), len(cands), len(gated), len(newNames))
		return nil
	}
}

// commitExtract 在单事务内完成幂等清理与落库。
// 顺序：先删派生 todo（经 source_memory_id 关联）→ 删 memory →
// 建新建议 topic → 插 memory → 插 todo。任一步失败整体回滚。
func commitExtract(ctx context.Context, d StageDeps, sessionID ids.ID,
	gated []memory.Candidate, refs []memory.TopicRef, newNames []string) error {

	tx, err := d.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后 Rollback 是 no-op

	// 1. 幂等清理
	if err := d.Todos.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 todo: %w", err)
	}
	if err := d.Memories.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 memory: %w", err)
	}

	// 2. 新建建议 topic（ResolveTopics 已保证与现有 active/suggested 不同名）
	nameToID := make(map[string]ids.ID, len(newNames))
	for _, name := range newNames {
		tp := &repo.Topic{Name: name, Status: "suggested", CreatedBy: "ai"}
		if err := d.Topics.CreateExt(ctx, tx, tp); err != nil {
			return fmt.Errorf("创建建议 topic %q: %w", name, err)
		}
		nameToID[name] = tp.ID
	}

	// 3. memory 入库
	memories := make([]repo.Memory, len(gated))
	for i, c := range gated {
		memories[i] = repo.Memory{
			Type: c.Type, Title: c.Title, Content: c.Content,
			EpistemicType: c.EpistemicType,
			Importance:    c.Importance, Confidence: c.Confidence,
			SessionID: sessionID, TranscriptSegmentIDs: c.SegmentIDs,
			EventAt: &c.EventAt, Status: "active",
		}
		if ref := refs[i]; ref.ExistingID != nil {
			memories[i].TopicID = ref.ExistingID
		} else if ref.NewName != "" {
			id := nameToID[ref.NewName]
			memories[i].TopicID = &id
		}
	}
	if err := d.Memories.InsertExt(ctx, tx, memories); err != nil {
		return fmt.Errorf("写 memory: %w", err)
	}

	// 4. todo 入库（继承来源 memory 的 topic 归属）
	var todos []repo.Todo
	for i, c := range gated {
		if !c.IsTodo || c.TodoStatus == "" {
			continue
		}
		todos = append(todos, repo.Todo{
			Title: c.Title, SourceMemoryID: &memories[i].ID,
			TopicID: memories[i].TopicID, Status: c.TodoStatus,
			DueAt: c.TodoDue, Confidence: c.Confidence,
		})
	}
	if err := d.Todos.InsertExt(ctx, tx, todos); err != nil {
		return fmt.Errorf("写 todo: %w", err)
	}

	return tx.Commit()
}

func msSince(begin time.Time) int64 {
	return time.Since(begin).Milliseconds()
}
```

- [ ] **Step 7: 运行测试通过**

Run: `make test-integration`
Expected: pipeline 包全部 PASS（含原有 pool/stage_asr 用例——签名改动已适配）

Run: `go test ./... `
Expected: 无集成环境时全 SKIP，编译通过

- [ ] **Step 8: 提交**

```bash
git add internal/pipeline/
git commit -m "feat: extract stage（聚合→抽取→闸门→Topic→单事务提交，幂等）"
```

---

### Task 12: memories API

**Files:**
- Create: `internal/api/memory.go`
- Test: `internal/api/memory_test.go`

- [ ] **Step 1: 写失败测试**

`internal/api/memory_test.go`：

```go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func setupMemoryAPI(t *testing.T) (http.Handler, *repo.MemoryRepo, *repo.TopicRepo) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &repo.MemoryRepo{DB: db}
	tr := &repo.TopicRepo{DB: db}
	r := chi.NewRouter()
	RegisterMemory(r, &MemoryHandler{Memories: mr, Topics: tr})
	return r, mr, tr
}

func TestMemoryListAndFilter(t *testing.T) {
	r, mr, tr := setupMemoryAPI(t)
	ctx := context.Background()

	topic := &repo.Topic{Name: "工作", Status: "active", CreatedBy: "user"}
	_ = tr.Create(ctx, topic)
	eventAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	_ = mr.InsertExt(ctx, mr.DB, []repo.Memory{
		{Type: "event", Title: "A", Content: "事件 A 的完整描述", EpistemicType: "observed",
			Confidence: 0.9, TopicID: &topic.ID, SessionID: ids.New(),
			EventAt: &eventAt, Status: "active"},
		{Type: "fact", Title: "B", Content: "事实 B 的完整描述", EpistemicType: "observed",
			Confidence: 0.9, SessionID: ids.New(), EventAt: &eventAt, Status: "active"},
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/memories?type=event", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Memories []repo.MemoryRow `json:"memories"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Memories) != 1 {
		t.Fatalf("type=event 过滤后 = %d", len(resp.Memories))
	}
	if resp.Memories[0].TopicName == nil || *resp.Memories[0].TopicName != "工作" {
		t.Fatalf("topic_name = %v", resp.Memories[0].TopicName)
	}

	// topic_id 过滤
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet,
		"/api/memories?topic_id="+topic.ID.String(), nil))
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), `"title":"A"`) {
		t.Fatalf("topic filter: %d %s", rec2.Code, rec2.Body.String())
	}
	// 非法 topic_id → 400
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/memories?topic_id=abc", nil))
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("非法 topic_id 应 400, got %d", rec3.Code)
	}
}

func TestMemoryPatch(t *testing.T) {
	r, mr, _ := setupMemoryAPI(t)
	ctx := context.Background()

	m := &repo.Memory{Type: "fact", Title: "原标题", Content: "原始内容的完整描述",
		EpistemicType: "observed", Confidence: 0.9, SessionID: ids.New(), Status: "active"}
	_ = mr.InsertExt(ctx, mr.DB, []repo.Memory{*m})

	// 修正内容 → version+1
	body := `{"content":"修正后的内容描述"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/memories/"+m.ID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := mr.Get(ctx, m.ID)
	if got.Content != "修正后的内容描述" || got.Version != 2 {
		t.Fatalf("after patch: %+v", got)
	}

	// dismiss
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPatch, "/api/memories/"+m.ID.String(),
		strings.NewReader(`{"status":"dismissed"}`))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("dismiss: %d", rec2.Code)
	}
	rows, _ := mr.ListBySession(ctx, m.SessionID)
	if len(rows) != 0 {
		t.Fatalf("dismissed 后列表不应出现, got %d", len(rows))
	}

	// 不存在 → 404
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodPatch,
		"/api/memories/123", strings.NewReader(`{"status":"dismissed"}`)))
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("404: got %d", rec3.Code)
	}
	// 非法 status → 400
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodPatch, "/api/memories/"+m.ID.String(),
		strings.NewReader(`{"status":"bogus"}`))
	req4.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusBadRequest {
		t.Fatalf("非法 status 应 400, got %d", rec4.Code)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（`RegisterMemory`、`MemoryHandler` 未定义）

- [ ] **Step 3: 实现**

`internal/api/memory.go`：

```go
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// MemoryHandler 处理 memory 查询与修正。
type MemoryHandler struct {
	Memories *repo.MemoryRepo
	Topics   *repo.TopicRepo
}

// RegisterMemory 挂载 memory 路由。
func RegisterMemory(r chi.Router, h *MemoryHandler) {
	r.Get("/api/memories", h.List)
	r.Patch("/api/memories/{id}", h.Patch)
}

func (h *MemoryHandler) List(w http.ResponseWriter, r *http.Request) {
	f := repo.MemoryFilter{
		Type:   r.URL.Query().Get("type"),
		Limit:  intQuery(r, "limit", 50),
		Offset: intOffset(r),
	}
	if v := r.URL.Query().Get("topic_id"); v != "" {
		tid, err := ids.ParseID(v)
		if err != nil {
			http.Error(w, "topic_id 非法", http.StatusBadRequest)
			return
		}
		f.TopicID = &tid
	}
	if f.Type != "" && !validMemoryType(f.Type) {
		http.Error(w, "type 取值非法", http.StatusBadRequest)
		return
	}
	rows, err := h.Memories.List(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"memories": rows})
}

func (h *MemoryHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Title   *string `json:"title"`
		Content *string `json:"content"`
		Status  *string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if req.Status != nil && *req.Status != "active" && *req.Status != "dismissed" && *req.Status != "superseded" {
		http.Error(w, "status 取值非法", http.StatusBadRequest)
		return
	}
	m, err := h.Memories.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "memory 不存在", http.StatusNotFound)
		return
	}
	// 改内容（title/content）则 version+1；status 单独变更不加版本
	contentChanged := false
	if req.Title != nil && strings.TrimSpace(*req.Title) != "" {
		m.Title = *req.Title
		contentChanged = true
	}
	if req.Content != nil && strings.TrimSpace(*req.Content) != "" {
		m.Content = *req.Content
		contentChanged = true
	}
	if contentChanged {
		m.Version++
	}
	if req.Status != nil {
		m.Status = *req.Status
	}
	if err := h.Memories.Save(r.Context(), m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"memory": m})
}

func validMemoryType(t string) bool {
	switch t {
	case "event", "fact", "decision", "idea", "problem", "preference":
		return true
	}
	return false
}
```

`intOffset` 辅助加到 `internal/api/query.go` 末尾（`intQuery` 旁边）：

```go
// intOffset 解析 offset 查询参数，非法或负数归零。
func intOffset(r *http.Request) int {
	v := r.URL.Query().Get("offset")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
```

- [ ] **Step 4: 运行测试通过**

Run: `make test-integration`
Expected: api 包 PASS

- [ ] **Step 5: 提交**

```bash
git add internal/api/
git commit -m "feat: memories API（过滤列表/修正/dismiss）"
```

---

### Task 13: todos API

**Files:**
- Create: `internal/api/todo.go`
- Test: `internal/api/todo_test.go`

- [ ] **Step 1: 写失败测试**

`internal/api/todo_test.go`：

```go
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func setupTodoAPI(t *testing.T) (http.Handler, *repo.TodoRepo, *repo.Todo) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := &repo.TodoRepo{DB: db}
	mr := &repo.MemoryRepo{DB: db}
	ctx := context.Background()

	mem := &repo.Memory{Type: "event", Title: "发邮件", Content: "明天需要给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Confidence: 0.9, SessionID: ids.New(), Status: "active"}
	_ = mr.InsertExt(ctx, db, []repo.Memory{*mem})
	td := &repo.Todo{Title: "给 Tom 发邮件", SourceMemoryID: &mem.ID,
		Status: "suggested", Confidence: 0.8}
	if err := tr.InsertExt(ctx, db, []repo.Todo{*td}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	RegisterTodo(r, &TodoHandler{Todos: tr})
	return r, tr, td
}

func TestTodoList(t *testing.T) {
	r, _, _ := setupTodoAPI(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/todos?status=suggested", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"source_session_id"`) {
		t.Fatalf("响应应含 source_session_id: %s", rec.Body.String())
	}
}

func TestTodoPatchTransitions(t *testing.T) {
	r, _, td := setupTodoAPI(t)
	patch := func(body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/api/todos/"+td.ID.String(),
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	// suggested → done 非法（必须先确认）
	if code := patch(`{"status":"done"}`); code != http.StatusConflict {
		t.Fatalf("suggested→done 应 409, got %d", code)
	}
	// suggested → confirmed
	if code := patch(`{"status":"confirmed"}`); code != http.StatusOK {
		t.Fatalf("confirm: %d", code)
	}
	// confirmed → done
	if code := patch(`{"status":"done"}`); code != http.StatusOK {
		t.Fatalf("done: %d", code)
	}
	// done → confirmed 非法
	if code := patch(`{"status":"confirmed"}`); code != http.StatusConflict {
		t.Fatalf("done→confirmed 应 409, got %d", code)
	}
	// 任意 → dismissed
	if code := patch(`{"status":"dismissed"}`); code != http.StatusOK {
		t.Fatalf("dismiss: %d", code)
	}
	// 不存在 → 404
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch,
		"/api/todos/123", strings.NewReader(`{"status":"done"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("404: got %d", rec.Code)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（`RegisterTodo`、`TodoHandler` 未定义）

- [ ] **Step 3: 实现**

`internal/api/todo.go`：

```go
package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TodoHandler 处理待办查询与状态流转。
type TodoHandler struct {
	Todos *repo.TodoRepo
}

// RegisterTodo 挂载 todo 路由。
func RegisterTodo(r chi.Router, h *TodoHandler) {
	r.Get("/api/todos", h.List)
	r.Patch("/api/todos/{id}", h.Patch)
}

func (h *TodoHandler) List(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && !validTodoStatus(status) {
		http.Error(w, "status 取值非法", http.StatusBadRequest)
		return
	}
	var topicID *ids.ID
	if v := r.URL.Query().Get("topic_id"); v != "" {
		tid, err := ids.ParseID(v)
		if err != nil {
			http.Error(w, "topic_id 非法", http.StatusBadRequest)
			return
		}
		topicID = &tid
	}
	rows, err := h.Todos.List(r.Context(), status, topicID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"todos": rows})
}

func (h *TodoHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validTodoStatus(req.Status) {
		http.Error(w, "status 取值非法", http.StatusBadRequest)
		return
	}
	td, err := h.Todos.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "todo 不存在", http.StatusNotFound)
		return
	}
	if !repo.CanTransition(td.Status, req.Status) {
		http.Error(w, "不允许的状态流转: "+td.Status+" → "+req.Status, http.StatusConflict)
		return
	}
	if err := h.Todos.UpdateStatus(r.Context(), id, req.Status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	td.Status = req.Status
	writeJSON(w, map[string]any{"todo": td})
}

func validTodoStatus(s string) bool {
	switch s {
	case "suggested", "confirmed", "done", "dismissed":
		return true
	}
	return false
}
```

- [ ] **Step 4: 运行测试通过**

Run: `make test-integration`
Expected: api 包 PASS

- [ ] **Step 5: 提交**

```bash
git add internal/api/
git commit -m "feat: todos API（状态机校验的流转）"
```

---

### Task 14: topics API

**Files:**
- Create: `internal/api/topic.go`
- Test: `internal/api/topic_test.go`

- [ ] **Step 1: 写失败测试**

`internal/api/topic_test.go`：

```go
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func setupTopicAPI(t *testing.T) (http.Handler, *repo.TopicRepo, *repo.MemoryRepo, *repo.TodoRepo, *repo.Topic) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := &repo.TopicRepo{DB: db}
	mr := &repo.MemoryRepo{DB: db}
	tdr := &repo.TodoRepo{DB: db}
	ctx := context.Background()

	tp := &repo.Topic{Name: "Rust 学习", Status: "suggested", CreatedBy: "ai"}
	if err := tr.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	// 一条 memory + 一条 confirmed todo 挂上去（验证计数）
	eventAt := time.Now()
	_ = mr.InsertExt(ctx, db, []repo.Memory{{
		Type: "fact", Title: "学 Rust", Content: "用户正在学习 Rust 计划三个月读完一本书",
		EpistemicType: "observed", Confidence: 0.9, TopicID: &tp.ID,
		SessionID: ids.New(), EventAt: &eventAt, Status: "active",
	}})
	mem, _ := mr.ListByTopic(ctx, tp.ID)
	_ = tdr.InsertExt(ctx, db, []repo.Todo{{
		Title: "读完 Rust 书", TopicID: &tp.ID, Status: "confirmed", Confidence: 0.9,
		SourceMemoryID: &mem[0].ID,
	}})

	r := chi.NewRouter()
	RegisterTopic(r, &TopicHandler{Topics: tr, Memories: mr, Todos: tdr})
	return r, tr, mr, tdr, tp
}

func TestTopicListWithCounts(t *testing.T) {
	r, _, _, _, _ := setupTopicAPI(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"memory_count":1`) || !strings.Contains(body, `"open_todo_count":1`) {
		t.Fatalf("counts missing: %s", body)
	}
}

func TestTopicCreateAndDuplicate(t *testing.T) {
	r, _, _, _, tp := setupTopicAPI(t)
	// 创建
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/topics",
		strings.NewReader(`{"name":"健身计划","description":"每周三次"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	// 重名 → 409
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/topics",
		strings.NewReader(`{"name":"`+tp.Name+`"}`))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("重名应 409, got %d", rec2.Code)
	}
	// 空名 → 400
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/api/topics", strings.NewReader(`{"name":"  "}`))
	req3.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("空名应 400, got %d", rec3.Code)
	}
}

func TestTopicDetailAndPatch(t *testing.T) {
	r, tr, _, _, tp := setupTopicAPI(t)

	// 详情：topic + memories + todos
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topics/"+tp.ID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"memories"`, `"todos"`, "学 Rust"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail 缺 %s: %s", want, body)
		}
	}

	// 确认 suggested→active
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPatch, "/api/topics/"+tp.ID.String(),
		strings.NewReader(`{"status":"active"}`))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("confirm: %d", rec2.Code)
	}
	got, _ := tr.Get(context.Background(), tp.ID)
	if got.Status != "active" {
		t.Fatalf("status = %s", got.Status)
	}

	// 改名
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPatch, "/api/topics/"+tp.ID.String(),
		strings.NewReader(`{"name":"Rust 进阶"}`))
	req3.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("rename: %d", rec3.Code)
	}
	got2, _ := tr.Get(context.Background(), tp.ID)
	if got2.Name != "Rust 进阶" {
		t.Fatalf("name = %s", got2.Name)
	}

	// 忽略
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodPatch, "/api/topics/"+tp.ID.String(),
		strings.NewReader(`{"status":"dismissed"}`))
	req4.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Fatalf("dismiss: %d", rec4.Code)
	}

	// 不存在 → 404
	rec5 := httptest.NewRecorder()
	r.ServeHTTP(rec5, httptest.NewRequest(http.MethodGet, "/api/topics/123", nil))
	if rec5.Code != http.StatusNotFound {
		t.Fatalf("404: got %d", rec5.Code)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（`RegisterTopic`、`TopicHandler` 未定义）

- [ ] **Step 3: 实现**

`internal/api/topic.go`：

```go
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TopicHandler 处理主题的增查改。
type TopicHandler struct {
	Topics   *repo.TopicRepo
	Memories *repo.MemoryRepo
	Todos    *repo.TodoRepo
}

// RegisterTopic 挂载 topic 路由。
func RegisterTopic(r chi.Router, h *TopicHandler) {
	r.Get("/api/topics", h.List)
	r.Post("/api/topics", h.Create)
	r.Get("/api/topics/{id}", h.Get)
	r.Patch("/api/topics/{id}", h.Patch)
}

func (h *TopicHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.Topics.ListWithCounts(r.Context(), 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"topics": list})
}

func (h *TopicHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, "name 不能为空", http.StatusBadRequest)
		return
	}
	// 与现有 active/suggested 重名 → 409
	if dup, err := h.Topics.FindActiveByName(r.Context(), 1, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if dup != nil {
		http.Error(w, "同名主题已存在", http.StatusConflict)
		return
	}
	tp := &repo.Topic{Name: name, Status: "active", CreatedBy: "user"}
	if req.Description != "" {
		tp.Description = &req.Description
	}
	if err := h.Topics.Create(r.Context(), tp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"topic": tp})
}

func (h *TopicHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	tp, err := h.Topics.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "topic 不存在", http.StatusNotFound)
		return
	}
	memories, err := h.Memories.ListByTopic(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	todos, err := h.Todos.ListByTopic(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"topic": tp, "memories": memories, "todos": todos})
}

func (h *TopicHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Status *string `json:"status"`
		Name   *string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if (req.Status == nil && req.Name == nil) ||
		(req.Status != nil && *req.Status != "active" && *req.Status != "dismissed") {
		http.Error(w, "status 取值非法（active|dismissed）", http.StatusBadRequest)
		return
	}
	tp, err := h.Topics.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "topic 不存在", http.StatusNotFound)
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			http.Error(w, "name 不能为空", http.StatusBadRequest)
			return
		}
		if name != tp.Name {
			if dup, err := h.Topics.FindActiveByName(r.Context(), 1, name); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else if dup != nil {
				http.Error(w, "同名主题已存在", http.StatusConflict)
				return
			}
		}
		if err := h.Topics.UpdateName(r.Context(), id, name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.Status != nil {
		if err := h.Topics.UpdateStatus(r.Context(), id, *req.Status); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	got, err := h.Topics.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"topic": got})
}
```

- [ ] **Step 4: 运行测试通过**

Run: `make test-integration`
Expected: api 包 PASS

- [ ] **Step 5: 提交**

```bash
git add internal/api/
git commit -m "feat: topics API（计数列表/创建/详情/确认改名忽略）"
```

---

### Task 15: session 详情扩展 + 服务完整装配

**Files:**
- Modify: `internal/api/query.go`（详情附带 memories/todos）
- Modify: `cmd/zhiwei-server/main.go`
- Test: `internal/api/query_test.go`（扩展既有用例）

- [ ] **Step 1: 扩展测试**

在 `internal/api/query_test.go` 的 `TestSessionsAndDetail` 中：

（1）`transcripts.InsertSegments(...)` 调用之后、`handler := setupQueryAPI(...)` 之前追加构造数据：

```go
	memories := &repo.MemoryRepo{DB: db}
	todos := &repo.TodoRepo{DB: db}
	eventAt := time.Now()
	_ = memories.InsertExt(ctx, db, []repo.Memory{{
		Type: "event", Title: "发邮件", Content: "明天记得给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Confidence: 0.9, SessionID: sid,
		EventAt: &eventAt, Status: "active",
	}})
	memRows, _ := memories.ListBySession(ctx, sid)
	_ = todos.InsertExt(ctx, db, []repo.Todo{{
		Title: "给 Tom 发邮件", SourceMemoryID: &memRows[0].ID, Status: "confirmed",
		Confidence: 0.9,
	}})
```

（2）调用处改为 `handler := setupQueryAPI(t, sessions, jobs, transcripts, memories, todos)`。

（3）详情断言的 `want` 列表追加三项：

```go
	for _, want := range []string{`"segments"`, "明天记得发邮件", "说话人 1",
		`"memories"`, `"todos"`, "发邮件"} {
```

（`time` import 补上；`setupQueryAPI` 签名改为接收五个 repo 并注册新 handler 依赖，见下。）

`setupQueryAPI` 改为：

```go
func setupQueryAPI(t *testing.T, s *repo.SessionRepo, j *repo.JobRepo,
	tr *repo.TranscriptRepo, m *repo.MemoryRepo, td *repo.TodoRepo) http.Handler {
	t.Helper()
	_ = ids.Init(1)
	r := chi.NewRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: s, Jobs: j, Transcripts: tr, Memories: m, Todos: td,
	})
	return r
}
```

- [ ] **Step 2: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（QueryHandler 无 Memories/Todos 字段）

- [ ] **Step 3: 实现**

`internal/api/query.go` 的 `QueryHandler` 扩展：

```go
// QueryHandler 会话/任务查询。
type QueryHandler struct {
	Sessions    *repo.SessionRepo
	Jobs        *repo.JobRepo
	Transcripts *repo.TranscriptRepo
	Memories    *repo.MemoryRepo // Sprint 2：详情附带 memory 卡片
	Todos       *repo.TodoRepo   // Sprint 2：详情附带 todo 卡片
}
```

`GetSession` 中，在 `resp["job"]` 块之前追加：

```go
	if h.Memories != nil {
		if mems, err := h.Memories.ListBySession(r.Context(), sid); err == nil {
			resp["memories"] = mems
		}
	}
	if h.Todos != nil {
		if todos, err := h.Todos.ListBySession(r.Context(), sid); err == nil {
			resp["todos"] = todos
		}
	}
```

`cmd/zhiwei-server/main.go` 完整替换为：

```go
// zhiwei-server 是知微云端 MVP 的唯一入口：HTTP API + 异步 pipeline worker。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"zhiwei/internal/api"
	"zhiwei/internal/config"
	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/pipeline"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := ids.Init(1); err != nil {
		log.Fatal(err)
	}
	if cfg.StepFunAPIKey == "" {
		log.Fatal("STEPFUN_API_KEY 未设置：ASR 不可用。请先 source .env（set -a; source .env; set +a）再启动")
	}
	db, err := repo.NewDB(cfg.MySQLDSN)
	if err != nil {
		log.Fatal(err)
	}

	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	memories := &repo.MemoryRepo{DB: db}
	todos := &repo.TodoRepo{DB: db}
	topics := &repo.TopicRepo{DB: db}

	// 抽取 prompt（版本化文件，运行时读取；版本号见文件名）
	promptBytes, err := os.ReadFile("prompts/extraction_v1.md")
	if err != nil {
		log.Fatal("读取抽取 prompt 失败: ", err)
	}

	// pipeline 装配：ASR 用 StepFun realtime（见 asr-protocol-notes.md），
	// LLM 走 Ark OpenAI 兼容接口（Tier 1 flash）
	asr := provider.NewStepFunASR(cfg.StepFunASREndpoint, cfg.StepFunAPIKey)
	llm := provider.NewArkLLM(cfg.ARKBaseURL, cfg.ARKAPIKey)
	stages := pipeline.BuildStages(pipeline.StageDeps{
		Sessions: sessions, Transcripts: transcripts, ASR: asr, DataDir: cfg.DataDir,
		DB: db, Memories: memories, Todos: todos, Topics: topics,
		LLM: llm, LLMModel: cfg.LLMFastModel,
		Prompt:        string(promptBytes),
		ExtractWindow: cfg.ExtractWindow,
		Gate:          memory.GateConfig{MinConf: cfg.QualityMinConf, TodoConf: cfg.QualityTodoConf},
	})
	flow := pipeline.Flow{Stages: []string{"asr", "segment", "extract"}}
	pool := pipeline.NewPool(jobs, flow, stages)
	pool.OnDone(func(ctx context.Context, sid ids.ID) {
		_ = sessions.UpdateStatus(ctx, sid, "completed")
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool.Start(ctx)

	r := api.NewRouter()
	api.RegisterAudio(r, sessions, jobs, cfg.DataDir)
	api.RegisterQuery(r, &api.QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos,
	})
	api.RegisterMemory(r, &api.MemoryHandler{Memories: memories, Topics: topics})
	api.RegisterTodo(r, &api.TodoHandler{Todos: todos})
	api.RegisterTopic(r, &api.TopicHandler{Topics: topics, Memories: memories, Todos: todos})

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r}
	go func() {
		log.Println("zhiwei-server listening on :" + cfg.Port)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	_ = srv.Close()
}
```

- [ ] **Step 4: 运行全部测试 + 编译**

Run: `make test-integration`
Expected: 全部 PASS

Run: `make build`
Expected: 编译通过

- [ ] **Step 5: 手动冒烟（真实 LLM，可选但推荐）**

```bash
make compose-up && make migrate-up
make dev-start
# 浏览器录音上传一段包含待办的话（如「明天记得给 Tom 发邮件」）
# 时间线详情应出现 memory 卡片；GET /api/memories 非空
make dev-stop
```

- [ ] **Step 6: 提交**

```bash
git add internal/api/ cmd/zhiwei-server/
git commit -m "feat: 三 stage 流水线装配与 session 详情扩展"
```

---

### Task 16: Web UI（四标签页 + 时间线卡片 + Topics + 待办）

**Files:**
- Create: `web/app.js`
- Modify: `web/index.html`（拆分引用 app.js，新增两个标签页视图）

无构建、无新依赖。样式沿用现有卡片/徽标体系。

- [ ] **Step 1: 写 app.js（Vue 应用，从 index.html 内联脚本迁移并扩展）**

`web/app.js`：

```js
// 知微 Web 前端（Vue 3 CDN，无构建）。
// 标签页：时间线 / 录音 / Topics / 待办（问知微、今日留待后续 Sprint）。
const { createApp, ref, computed, onUnmounted } = Vue;

// memory 类型 → 中文标签与颜色（卡片徽标用）
const TYPE_META = {
  event:      { label: '事件', color: '#6366f1' },
  fact:       { label: '事实', color: '#0891b2' },
  decision:   { label: '决定', color: '#7c3aed' },
  idea:       { label: '想法', color: '#d97706' },
  problem:    { label: '问题', color: '#dc2626' },
  preference: { label: '偏好', color: '#059669' },
};

createApp({
  setup() {
    const tab = ref('timeline');
    const toast = ref('');

    // ---------- 通用 ----------
    function fmtTime(iso) { return iso ? new Date(iso).toLocaleString('zh-CN') : ''; }
    function fmtDate(iso) { return iso ? new Date(iso).toLocaleDateString('zh-CN') : ''; }
    function fmtDue(iso) {
      if (!iso) return '';
      const d = new Date(iso);
      const s = d.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' });
      return d < new Date() ? s + ' · 已过期' : s;
    }
    async function api(method, url, body) {
      const opt = { method };
      if (body !== undefined) {
        opt.headers = { 'Content-Type': 'application/json' };
        opt.body = JSON.stringify(body);
      }
      const r = await fetch(url, opt);
      if (!r.ok) {
        let msg = '请求失败';
        try { msg = (await r.json()).error || msg; } catch (e) {}
        throw new Error(msg);
      }
      return r.json();
    }
    function showError(e) {
      toast.value = (e && e.message) || String(e);
      setTimeout(() => { toast.value = ''; }, 3000);
    }
    function typeMeta(t) { return TYPE_META[t] || { label: t, color: '#6b7280' }; }
    function statusText(status, stage) {
      if (status === 'done' || status === 'completed') return '已完成';
      if (status === 'failed') return '失败';
      if (status === 'running') return '处理中 · ' + (stage || '');
      return '排队中';
    }
    function spClass(speaker) {
      const n = (speaker || '').replace(/\D/g, '') || '1';
      return 'sp' + Math.min(Number(n), 3);
    }

    // ---------- 时间线 ----------
    const sessions = ref([]);
    const detail = ref(null);

    async function loadSessions() {
      try {
        const d = await api('GET', '/api/sessions');
        sessions.value = d.sessions || [];
      } catch (e) { showError(e); }
    }
    async function openSession(id) {
      try {
        detail.value = await api('GET', '/api/sessions/' + id);
      } catch (e) { showError(e); }
    }
    async function dismissMemory(m) {
      try {
        await api('PATCH', '/api/memories/' + m.id, { status: 'dismissed' });
        detail.value.memories = (detail.value.memories || []).filter(x => x.id !== m.id);
      } catch (e) { showError(e); }
    }
    async function retryJob(id) {
      try {
        await api('POST', '/api/jobs/' + id + '/retry');
        await openSession(detail.value.session.id);
      } catch (e) { showError(e); }
    }

    // ---------- 录音 ----------
    const recording = ref(false);
    const recSeconds = ref(0);
    const uploadInfo = ref(null);
    let recorder = null, recTimer = null, pollTimer = null;

    async function startRec() {
      try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        recorder = new MediaRecorder(stream, { mimeType: 'audio/webm;codecs=opus' });
        const chunks = [];
        recorder.ondataavailable = e => chunks.push(e.data);
        recorder.onstop = () => {
          stream.getTracks().forEach(t => t.stop());
          upload(new File(chunks, 'record-' + Date.now() + '.webm', { type: 'audio/webm' }), 'web_record');
        };
        recorder.start();
        recording.value = true; recSeconds.value = 0;
        recTimer = setInterval(() => recSeconds.value++, 1000);
      } catch (e) { showError(e); }
    }
    function stopRec() {
      recorder.stop(); recording.value = false;
      clearInterval(recTimer);
    }
    function onDrop(e) {
      const f = e.dataTransfer.files[0];
      if (f) upload(f, 'web_upload');
    }
    async function upload(file, source) {
      const fd = new FormData();
      fd.append('file', file); fd.append('source', source);
      uploadInfo.value = { filename: file.name, status: 'pending', text: '上传中…' };
      try {
        const r = await fetch('/api/audio', { method: 'POST', body: fd });
        const d = await r.json();
        if (!r.ok) throw new Error(d.error || '上传失败');
        uploadInfo.value = { filename: file.name, status: 'running', text: '已上传，处理中…' };
        pollTimer = setInterval(async () => {
          try {
            const rr = await fetch('/api/sessions/' + d.session_id);
            const dd = await rr.json();
            const st = dd.job ? dd.job.status : dd.session.status;
            if (st === 'done' || st === 'completed') {
              clearInterval(pollTimer);
              uploadInfo.value = { filename: file.name, status: 'done', text: '处理完成 ✓' };
              loadSessions();
            } else if (st === 'failed') {
              clearInterval(pollTimer);
              uploadInfo.value = { filename: file.name, status: 'failed', text: '处理失败，可在时间线重跑' };
            }
          } catch (e) { /* 轮询失败静默重试 */ }
        }, 2000);
      } catch (e) {
        uploadInfo.value = { filename: file.name, status: 'failed', text: e.message };
      }
    }

    // ---------- Topics ----------
    const topics = ref([]);
    const topicDetail = ref(null);
    const showNewTopic = ref(false);
    const newTopic = ref({ name: '', description: '' });
    const renaming = ref(null); // { id, name }

    async function loadTopics() {
      try {
        const d = await api('GET', '/api/topics');
        topics.value = d.topics || [];
      } catch (e) { showError(e); }
    }
    async function openTopic(id) {
      try {
        topicDetail.value = await api('GET', '/api/topics/' + id);
      } catch (e) { showError(e); }
    }
    async function confirmTopic(t) {
      try {
        await api('PATCH', '/api/topics/' + t.id, { status: 'active' });
        await loadTopics();
      } catch (e) { showError(e); }
    }
    async function dismissTopic(t) {
      try {
        await api('PATCH', '/api/topics/' + t.id, { status: 'dismissed' });
        await loadTopics();
      } catch (e) { showError(e); }
    }
    function startRename(t) { renaming.value = { id: t.id, name: t.name }; }
    async function commitRename() {
      const rn = renaming.value;
      renaming.value = null;
      if (!rn || !rn.name.trim()) return;
      try {
        await api('PATCH', '/api/topics/' + rn.id, { name: rn.name.trim() });
        await loadTopics();
        if (topicDetail.value && topicDetail.value.topic.id === rn.id) {
          await openTopic(rn.id);
        }
      } catch (e) { showError(e); }
    }
    async function createTopic() {
      if (!newTopic.value.name.trim()) return;
      try {
        await api('POST', '/api/topics', newTopic.value);
        newTopic.value = { name: '', description: '' };
        showNewTopic.value = false;
        await loadTopics();
      } catch (e) { showError(e); }
    }

    // ---------- 待办 ----------
    const todos = ref([]);
    const doneCollapsed = ref(true);
    const suggestedTodos = computed(() => todos.value.filter(t => t.status === 'suggested'));
    const activeTodos = computed(() => todos.value.filter(t => t.status === 'confirmed'));
    const doneTodos = computed(() => todos.value.filter(t => t.status === 'done'));

    async function loadTodos() {
      try {
        const d = await api('GET', '/api/todos');
        todos.value = d.todos || [];
      } catch (e) { showError(e); }
    }
    async function setTodoStatus(t, status) {
      try {
        await api('PATCH', '/api/todos/' + t.id, { status });
        await loadTodos();
      } catch (e) { showError(e); }
    }
    async function jumpToSession(sessionId) {
      switchTab('timeline');
      await openSession(sessionId);
    }

    // ---------- 标签页切换 ----------
    function switchTab(name) {
      tab.value = name;
      if (name === 'timeline') loadSessions();
      if (name === 'topics') { topicDetail.value = null; renaming.value = null; loadTopics(); }
      if (name === 'todos') loadTodos();
    }
    loadSessions();

    onUnmounted(() => { clearInterval(recTimer); clearInterval(pollTimer); });

    return {
      tab, toast, switchTab,
      fmtTime, fmtDate, fmtDue, typeMeta, statusText, spClass,
      sessions, detail, loadSessions, openSession, dismissMemory, retryJob,
      recording, recSeconds, uploadInfo, startRec, stopRec, onDrop,
      topics, topicDetail, showNewTopic, newTopic, renaming,
      loadTopics, openTopic, confirmTopic, dismissTopic, startRename, commitRename, createTopic,
      todos, doneCollapsed, suggestedTodos, activeTodos, doneTodos,
      loadTodos, setTodoStatus, jumpToSession,
    };
  }
}).mount('#app');
```

- [ ] **Step 2: 重写 index.html（四标签页 + 引用 app.js）**

`web/index.html` 整体替换为：

```html
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>知微</title>
<script src="/app/vendor/vue.global.prod.js"></script>
<style>
  * { box-sizing: border-box; margin: 0; }
  body { font-family: -apple-system, "PingFang SC", sans-serif; background: #f6f7f9; color: #222; }
  .tabs { display: flex; background: #fff; border-bottom: 1px solid #e5e7eb; padding: 0 16px; }
  .tabs button { padding: 14px 18px; border: none; background: none; font-size: 15px; cursor: pointer; color: #6b7280; }
  .tabs button.active { color: #111; border-bottom: 2px solid #111; font-weight: 600; }
  .wrap { max-width: 760px; margin: 0 auto; padding: 16px; }
  .card { background: #fff; border-radius: 12px; padding: 14px 16px; margin-bottom: 12px; box-shadow: 0 1px 2px rgba(0,0,0,.04); }
  .muted { color: #9ca3af; font-size: 13px; }
  .badge { display: inline-block; padding: 2px 8px; border-radius: 99px; font-size: 12px; }
  .badge.pending, .badge.running, .badge.suggested { background: #fef3c7; color: #92400e; }
  .badge.completed, .badge.done, .badge.active, .badge.confirmed { background: #d1fae5; color: #065f46; }
  .badge.failed { background: #fee2e2; color: #991b1b; }
  .seg { padding: 8px 0; border-bottom: 1px dashed #f0f0f0; }
  .seg:last-child { border: none; }
  .sp { font-size: 12px; color: #fff; border-radius: 6px; padding: 1px 6px; margin-right: 8px; }
  .sp1 { background: #6366f1; } .sp2 { background: #10b981; } .sp3 { background: #f59e0b; }
  button.primary { background: #111; color: #fff; border: none; border-radius: 10px; padding: 10px 22px; font-size: 15px; cursor: pointer; }
  button.primary:disabled { background: #9ca3af; }
  button.mini { background: #f3f4f6; color: #374151; border: none; border-radius: 8px; padding: 5px 12px; font-size: 13px; cursor: pointer; }
  button.mini:hover { background: #e5e7eb; }
  #drop { border: 2px dashed #d1d5db; border-radius: 12px; padding: 36px; text-align: center; color: #6b7280; margin-bottom: 12px; }
  #drop.rec { border-color: #ef4444; color: #ef4444; }
  .toast { position: fixed; bottom: 24px; left: 50%; transform: translateX(-50%); background: #111; color: #fff; padding: 10px 20px; border-radius: 10px; font-size: 14px; z-index: 99; }
  .type-tag { display: inline-block; color: #fff; border-radius: 6px; padding: 1px 8px; font-size: 12px; margin-right: 8px; }
  .kv { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
  .overdue { color: #dc2626; }
  .todo-group-title { font-size: 14px; font-weight: 600; margin: 18px 0 8px; }
  input.txt { border: 1px solid #d1d5db; border-radius: 8px; padding: 8px 10px; font-size: 14px; width: 100%; }
</style>
</head>
<body>
<div id="app">
  <div class="tabs">
    <button :class="{active: tab==='timeline'}" @click="switchTab('timeline')">时间线</button>
    <button :class="{active: tab==='record'}" @click="switchTab('record')">录音</button>
    <button :class="{active: tab==='topics'}" @click="switchTab('topics')">Topics</button>
    <button :class="{active: tab==='todos'}" @click="switchTab('todos')">待办</button>
  </div>
  <div v-if="toast" class="toast">{{ toast }}</div>

  <!-- ============ 时间线 ============ -->
  <div class="wrap" v-if="tab==='timeline'">
    <div v-if="!sessions.length" class="card muted">还没有记录。去「录音」页上传或录一段吧。</div>
    <div class="card" v-for="s in sessions" :key="s.id" style="cursor:pointer" @click="openSession(s.id)">
      <div class="kv">
        <div>
          <b>{{ s.filename }}</b>
          <div class="muted">{{ fmtTime(s.created_at) }} · {{ s.source === 'web_record' ? '录音' : '上传' }}</div>
        </div>
        <span class="badge" :class="s.job_status || s.status">{{ statusText(s.job_status, s.job_stage) }}</span>
      </div>
    </div>

    <!-- 详情弹层 -->
    <div v-if="detail" class="card">
      <div class="kv">
        <b>转写详情</b>
        <button class="muted" style="border:none;background:none;cursor:pointer" @click="detail=null">✕ 关闭</button>
      </div>
      <div class="muted" style="margin:6px 0">{{ detail.session.filename }}</div>
      <div v-if="detail.job && detail.job.status==='failed'" style="margin:8px 0">
        <span class="badge failed">处理失败</span>
        <span class="muted">{{ detail.job.last_error }}</span>
        <button class="primary" style="padding:4px 12px" @click="retryJob(detail.job.id)">重跑</button>
      </div>
      <div v-for="(sg, i) in detail.segments" :key="i" class="seg">
        <span class="sp" :class="spClass(sg.speaker)">{{ sg.speaker }}</span>{{ sg.text }}
      </div>

      <!-- memory 卡片 -->
      <template v-if="detail.memories && detail.memories.length">
        <div class="todo-group-title">提取的记忆</div>
        <div class="card" v-for="m in detail.memories" :key="m.id" style="margin:6px 0; position:relative">
          <div class="kv">
            <div>
              <span class="type-tag" :style="{background: typeMeta(m.type).color}">{{ typeMeta(m.type).label }}</span>
              <b>{{ m.title }}</b>
            </div>
            <button class="mini" @click="dismissMemory(m)" title="忽略此记忆">✕</button>
          </div>
          <div style="margin:6px 0">{{ m.content }}</div>
          <div class="muted">
            重要度 {{ (m.importance ?? 0).toFixed(1) }} · 置信 {{ (m.confidence ?? 0).toFixed(2) }}
            <template v-if="m.topic_name"> · {{ m.topic_name }}</template>
            <template v-if="m.event_at"> · {{ fmtTime(m.event_at) }}</template>
          </div>
        </div>
      </template>

      <!-- todo 卡片（只读，操作去待办页） -->
      <template v-if="detail.todos && detail.todos.length">
        <div class="todo-group-title">提取的待办</div>
        <div class="card" v-for="td in detail.todos" :key="td.id" style="margin:6px 0">
          <div class="kv">
            <div>☑️ {{ td.title }}</div>
            <span class="badge" :class="td.status">
              {{ td.status === 'suggested' ? '待确认' : td.status === 'confirmed' ? '已确认' : '已完成' }}
            </span>
          </div>
          <div class="muted" v-if="td.due_at">截止 {{ fmtDue(td.due_at) }} · 到「待办」页处理</div>
        </div>
      </template>
    </div>
  </div>

  <!-- ============ 录音 ============ -->
  <div class="wrap" v-if="tab==='record'">
    <div id="drop" :class="{rec: recording}" @dragover.prevent @drop.prevent="onDrop">
      <template v-if="!recording">拖拽音频文件到此处，或点击下方按钮录音</template>
      <template v-else>● 录音中…（{{ recSeconds }}s）</template>
    </div>
    <div style="display:flex; gap:12px">
      <button class="primary" v-if="!recording" @click="startRec">开始录音</button>
      <button class="primary" v-else @click="stopRec">停止并上传</button>
    </div>
    <div v-if="uploadInfo" class="card" style="margin-top:12px">
      <div class="muted">{{ uploadInfo.filename }}</div>
      <span class="badge" :class="uploadInfo.status">{{ uploadInfo.text }}</span>
    </div>
  </div>

  <!-- ============ Topics ============ -->
  <div class="wrap" v-if="tab==='topics'">
    <!-- 列表视图 -->
    <template v-if="!topicDetail">
      <div class="kv" style="margin-bottom:12px">
        <b>主题</b>
        <button class="primary" style="padding:6px 16px; font-size:14px" @click="showNewTopic = !showNewTopic">＋ 新建</button>
      </div>
      <div class="card" v-if="showNewTopic">
        <input class="txt" v-model="newTopic.name" placeholder="主题名称（必填）" style="margin-bottom:8px">
        <input class="txt" v-model="newTopic.description" placeholder="描述（可选）" style="margin-bottom:8px">
        <button class="primary" style="padding:6px 16px; font-size:14px" @click="createTopic">创建</button>
      </div>
      <div v-if="!topics.length" class="card muted">还没有主题。录音抽取后会自动归类，也可以手动新建。</div>
      <div class="card" v-for="t in topics" :key="t.id">
        <div class="kv">
          <div style="cursor:pointer" @click="openTopic(t.id)">
            <b>{{ t.name }}</b>
            <span v-if="t.status==='suggested'" class="badge suggested">待确认</span>
            <div class="muted">{{ t.memory_count }} 条记忆 · {{ t.open_todo_count }} 个进行中待办</div>
          </div>
          <div style="display:flex; gap:6px">
            <button class="mini" v-if="t.status==='suggested'" @click="confirmTopic(t)">确认</button>
            <button class="mini" @click="dismissTopic(t)">忽略</button>
          </div>
        </div>
      </div>
    </template>

    <!-- 详情视图 -->
    <template v-else>
      <div class="kv" style="margin-bottom:12px">
        <div>
          <b>{{ topicDetail.topic.name }}</b>
          <span v-if="topicDetail.topic.status==='suggested'" class="badge suggested">待确认</span>
          <div class="muted" v-if="topicDetail.topic.description">{{ topicDetail.topic.description }}</div>
        </div>
        <button class="mini" @click="topicDetail=null">← 返回列表</button>
      </div>
      <div class="card" style="margin-bottom:12px">
        <button class="mini" v-if="renaming===null" @click="startRename(topicDetail.topic)">改名</button>
        <template v-else>
          <input class="txt" v-model="renaming.name" style="margin-bottom:8px">
          <button class="mini" @click="commitRename">保存</button>
        </template>
      </div>
      <div class="todo-group-title">记忆时间线</div>
      <div v-if="!topicDetail.memories.length" class="card muted">暂无记忆</div>
      <div class="card" v-for="m in topicDetail.memories" :key="m.id">
        <div>
          <span class="type-tag" :style="{background: typeMeta(m.type).color}">{{ typeMeta(m.type).label }}</span>
          <b>{{ m.title }}</b>
        </div>
        <div style="margin:6px 0">{{ m.content }}</div>
        <div class="muted">{{ fmtTime(m.event_at) }}</div>
      </div>
      <div class="todo-group-title">关联待办</div>
      <div v-if="!topicDetail.todos.length" class="card muted">暂无待办</div>
      <div class="card" v-for="td in topicDetail.todos" :key="td.id">
        <div class="kv">
          <div>☑️ {{ td.title }}</div>
          <span class="badge" :class="td.status">
            {{ td.status === 'suggested' ? '待确认' : td.status === 'confirmed' ? '已确认' : '已完成' }}
          </span>
        </div>
      </div>
    </template>
  </div>

  <!-- ============ 待办 ============ -->
  <div class="wrap" v-if="tab==='todos'">
    <div class="todo-group-title">待确认（{{ suggestedTodos.length }}）</div>
    <div v-if="!suggestedTodos.length" class="card muted">没有待确认的待办</div>
    <div class="card" v-for="td in suggestedTodos" :key="td.id" style="background:#fffbeb">
      <div class="kv">
        <b>☑️ {{ td.title }}</b>
        <div style="display:flex; gap:6px">
          <button class="mini" @click="setTodoStatus(td, 'confirmed')">加入</button>
          <button class="mini" @click="setTodoStatus(td, 'dismissed')">忽略</button>
        </div>
      </div>
      <div class="muted" v-if="td.due_at" :class="{overdue: new Date(td.due_at) < new Date()}">截止 {{ fmtDue(td.due_at) }}</div>
      <div class="muted" v-if="td.source_session_id" style="cursor:pointer"
           @click="jumpToSession(td.source_session_id)">↗ 查看来源对话</div>
    </div>

    <div class="todo-group-title">进行中（{{ activeTodos.length }}）</div>
    <div v-if="!activeTodos.length" class="card muted">没有进行中的待办</div>
    <div class="card" v-for="td in activeTodos" :key="td.id">
      <div class="kv">
        <b>☑️ {{ td.title }}</b>
        <div style="display:flex; gap:6px">
          <button class="mini" @click="setTodoStatus(td, 'done')">完成</button>
          <button class="mini" @click="setTodoStatus(td, 'dismissed')">忽略</button>
        </div>
      </div>
      <div class="muted" v-if="td.due_at" :class="{overdue: new Date(td.due_at) < new Date()}">截止 {{ fmtDue(td.due_at) }}</div>
    </div>

    <div class="todo-group-title" style="cursor:pointer" @click="doneCollapsed = !doneCollapsed">
      已完成（{{ doneTodos.length }}）{{ doneCollapsed ? ' ▸' : ' ▾' }}
    </div>
    <template v-if="!doneCollapsed">
      <div v-if="!doneTodos.length" class="card muted">没有已完成的待办</div>
      <div class="card" v-for="td in doneTodos" :key="td.id" style="opacity:.6">
        <div class="kv">
          <div>✅ <s>{{ td.title }}</s></div>
        </div>
      </div>
    </template>
  </div>
</div>

<script src="/app/app.js"></script>
</body>
</html>
```

- [ ] **Step 3: 手动验证**

```bash
make dev-start
# 浏览器打开 http://localhost:8080
# 1. 时间线：点开已完成会话 → 转写下方出现 memory 卡片（可 ✕ 忽略）与 todo 卡片（只读）
# 2. Topics：列表含计数与「待确认」徽标；点进详情看到记忆时间线与待办；可确认/改名/忽略/新建
# 3. 待办：待确认组可「加入/忽略」；进行中可「完成」；已完成默认折叠；「查看来源对话」跳时间线
make dev-stop
```

Expected: 四个标签页全部可用，操作即时生效无整页刷新。

- [ ] **Step 4: 提交**

```bash
git add web/
git commit -m "feat: Web 四标签页（时间线记忆卡片/Topics/待办）"
```

---

### Task 17: e2e 扩展与真实验收

**Files:**
- Modify: `scripts/e2e.sh`

- [ ] **Step 1: 扩展 e2e 脚本**

`scripts/e2e.sh` 中 `if [ "$STATUS" = "done" ] ... fi` 的整个 done 分支（现为「segments 非空即 PASS」）替换为：

```bash
  if [ "$STATUS" = "done" ] || [ "$STATUS" = "completed" ]; then
    if ! echo "$DETAIL" | python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get('segments') else 1)"; then
      echo "FAIL: segments 为空"; exit 1
    fi
    # Sprint 2：断言 memory 抽取产出（真实语音才有内容）
    MEMS=$(curl -fsS "localhost:8080/api/memories?limit=5")
    echo "memories: $(echo "$MEMS" | head -c 500)"
    if ! echo "$MEMS" | python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get('memories') else 1)"; then
      echo "FAIL: memories 为空（真实语音不应为空）"; exit 1
    fi
    echo "PASS: pipeline 跑通，转写与记忆抽取产出正常"
    echo "$DETAIL" | python3 -m json.tool | head -30
    exit 0
  fi
```

（脚本其余部分不变；仍以 `bash scripts/e2e.sh [音频文件]` 方式运行，默认 `testdata/speech.wav` 真实语音。）

- [ ] **Step 2: 真人语音验收（Sprint 2 Done 标准）**

用手机录一段两人对话（30 秒+，内容包含明确待办如「明天记得给 Tom 发邮件」和至少一个可归类主题如「我在学 Rust」），替换 `testdata` 中的验收音频后跑：

```bash
make e2e
# 或手动：make dev-start 后浏览器录音上传
```

验收清单：
- [ ] 时间线详情出现 memory 卡片（类型/标题/内容/归属 Topic 正确）
- [ ] 对话中的待办被提取，高置信的直接 confirmed，低置信的进「待确认」组
- [ ] Topic 页出现归类（挂已有主题）或 AI 建议主题（黄色「待确认」徽标）
- [ ] 待办页可走完 确认 → 完成 闭环，「查看来源对话」正确跳转
- [ ] `make test` 与 `make test-integration` 全绿

- [ ] **Step 3: 更新 README 的 API 说明**

`README.md` 的 API 列表追加：

```markdown
GET/PATCH /api/memories      记忆列表（type/topic_id 过滤）/ 修正与忽略
GET/PATCH /api/todos         待办列表 / 状态流转（确认/完成/忽略）
GET/POST/PATCH /api/topics   主题计数列表 / 新建 / 确认/改名/忽略
```

- [ ] **Step 4: 提交**

```bash
git add scripts/e2e.sh README.md
git commit -m "test: e2e 追加 memory 产出断言与 Sprint 2 验收标准"
```

---

## Sprint 2 完成后

系统状态：录音/上传 → ASR → **LLM 记忆抽取 → 质量闸门 → Topic 归类 → memory/todo/topic 落库** → 四页 Web UI 全链路可用。用户第一次体验「它居然记得」。

下一步：基于本计划的实现情况编写 Sprint 3 计划（Embedding 回填 + 混合检索 + Agent 问答），届时为存量 memory 批量生成 embedding（新增回填任务），并把 `memory.embedding` 列用起来。
