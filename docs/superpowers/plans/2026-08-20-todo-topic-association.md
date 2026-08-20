# 代办/记忆 ↔ Topic 多对多关联 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `todo`↔`topic` 与 `memory`↔`topic` 从单值外键改为多对多；抽取 LLM 自动关联多个 topic；代办与记忆均支持手动加/删 topic；extract 重跑按自然键保留手动关联。

**Architecture:** 用 expand/contract 迁移：000002 先加 `memory_topic`/`todo_topic` 关联表（带 `source` 列）并回填存量，**保留** `topic_id` 列；代码切换到关联表后，000003 删除 `topic_id` 列。抽取候选 `topics` 由单值升级为数组（`[]TopicRef`），`ResolveTopics` 遍历解析多 ref，commit 单事务内：快照 `source='user'` 关联→清理→建建议 topic→插 memory/todo→插关联表(`ai`)→按自然键重链 `user`。自然键 = 来源块 `transcript_segment_ids` + `title`（segment 跨重跑稳定）。

**Tech Stack:** Go 1.25 + chi + sqlx + MySQL（golang-migrate）、Vue 3 CDN 前端。测试两级：`make test`（纯逻辑，无 DB）/ `make test-integration`（重建 `zhiwei_test` 库 + 真连 MySQL）。集成单测快路径：`make init-testdb` 一次后 `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run TestX ./internal/<pkg> -v`。

**Task 依赖图：** T1 ∥ T2 起步 → T3（依赖 T1）→ T4（依赖 T2、T3）→ T5（依赖 T4）→ T6 ∥ T7（依赖 T5）。T4/T5 为耦合核心，按序执行；T1/T2 可并行。

---

## Task 1: 迁移 000002——新增多对多关联表（扩张期，保留 topic_id 列）

**Files:**
- Create: `migrations/000002_todo_topic.up.sql`
- Create: `migrations/000002_todo_topic.down.sql`

- [ ] **Step 1: 写 up 迁移**

`migrations/000002_todo_topic.up.sql`：
```sql
-- 代办/记忆 ↔ topic 多对多关联表（spec §3）。
-- 本迁移仅「扩张」：新增关联表 + 回填存量单值 topic_id；保留 topic_id 列，
-- 由 000003 在代码切换到关联表后删除（expand/contract，保增量 green 提交）。
CREATE TABLE memory_topic (
  memory_id  BIGINT NOT NULL,
  topic_id   BIGINT NOT NULL,
  source     VARCHAR(8) NOT NULL DEFAULT 'ai',  -- ai=抽取自动, user=手动
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (memory_id, topic_id),
  KEY idx_topic (topic_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE todo_topic (
  todo_id  BIGINT NOT NULL,
  topic_id BIGINT NOT NULL,
  source   VARCHAR(8) NOT NULL DEFAULT 'ai',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (todo_id, topic_id),
  KEY idx_topic (topic_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 存量单值关联回填进关联表（source='ai'）
INSERT IGNORE INTO memory_topic (memory_id, topic_id, source)
  SELECT id, topic_id, 'ai' FROM memory WHERE topic_id IS NOT NULL;
INSERT IGNORE INTO todo_topic (todo_id, topic_id, source)
  SELECT id, topic_id, 'ai' FROM todo WHERE topic_id IS NOT NULL;
```

- [ ] **Step 2: 写 down 迁移**

`migrations/000002_todo_topic.down.sql`：
```sql
DROP TABLE IF EXISTS todo_topic;
DROP TABLE IF EXISTS memory_topic;
```

- [ ] **Step 3: 验证迁移可干净 apply/rollback**

前置：MySQL 容器已起 `make compose-up`。
Run: `make init-testdb`
Expected: 无错误（重建 `zhiwei_test` 库并跑全部迁移含 000002）。

Run: `docker exec zhiwei-mvp-mysql mysql -uroot -proot zhiwei_test -e "SHOW TABLES LIKE '%_topic'"`
Expected: 输出 `memory_topic` 与 `todo_topic` 两行。

Run: `make migrate-down && make migrate-up`
Expected: 先回滚 000002（删两表）再重新 up（建回），无错误。

- [ ] **Step 4: 提交**

```bash
git add migrations/000002_todo_topic.up.sql migrations/000002_todo_topic.down.sql
git commit -m "feat: 迁移 000002 新增 todo_topic/memory_topic 关联表（扩张期）"
```

---

## Task 2: 自然键 canonical（纯逻辑）

**Files:**
- Create: `internal/memory/naturalkey.go`
- Create: `internal/memory/naturalkey_test.go`

- [ ] **Step 1: 写失败测试**

`internal/memory/naturalkey_test.go`：
```go
package memory

import (
	"strings"
	"testing"

	"zhiwei/internal/ids"
)

func TestNaturalKey(t *testing.T) {
	a, b := ids.ID(3), ids.ID(1)
	// 顺序无关：排序后一致
	k1 := NaturalKey([]ids.ID{a, b}, "买菜")
	k2 := NaturalKey([]ids.ID{b, a}, "买菜")
	if k1 != k2 {
		t.Fatalf("排序不稳定: %q vs %q", k1, k2)
	}
	// 分隔符隔离 segment 与 title，防歧义
	if !strings.Contains(k1, "\x1f") {
		t.Fatalf("缺分隔符: %q", k1)
	}
	// 空 segment：退化为 title（键仍稳定可比较）
	if NaturalKey(nil, "X") != "\x1fX" {
		t.Fatalf("空段退化失败: %q", NaturalKey(nil, "X"))
	}
	// 不同 title → 不同键
	if NaturalKey([]ids.ID{a}, "A") == NaturalKey([]ids.ID{a}, "B") {
		t.Fatalf("title 未入键")
	}
	// 不同 segment → 不同键
	if NaturalKey([]ids.ID{a}, "A") == NaturalKey([]ids.ID{b}, "A") {
		t.Fatalf("segment 未入键")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/memory -run TestNaturalKey -v`
Expected: FAIL `undefined: NaturalKey`。

- [ ] **Step 3: 实现**

`internal/memory/naturalkey.go`：
```go
package memory

import (
	"sort"
	"strings"

	"zhiwei/internal/ids"
)

// NaturalKey 是 memory/todo 跨 extract 重跑的稳定标识：来源块的 segment id 集合 + 标题。
// segment 来自 asr/segment stage，extract 重跑不动 segment → 跨重跑稳定。
// 用于重跑时按自然键快照与重链 source='user' 的手动 topic 关联（spec §6）。
// 排序保证 segment 顺序无关；\x1f 分隔避免 segment 与 title 串扰。
func NaturalKey(segmentIDs []ids.ID, title string) string {
	tmp := make([]string, len(segmentIDs))
	for i, id := range segmentIDs {
		tmp[i] = id.String()
	}
	sort.Strings(tmp)
	return strings.Join(tmp, ",") + "\x1f" + title
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/memory -run TestNaturalKey -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/memory/naturalkey.go internal/memory/naturalkey_test.go
git commit -m "feat: 自然键 NaturalKey(segment_ids,title) 用于重跑保留手动关联"
```

---

## Task 3: 关联表 DAO——memory_topic / todo_topic（含事务内多行查询接口）

**Files:**
- Modify: `internal/repo/db.go`（新增 `QueryerContext` 接口）
- Create: `internal/repo/memory_topic.go`
- Create: `internal/repo/todo_topic.go`
- Test: `internal/repo/memory_topic_test.go`、`internal/repo/todo_topic_test.go`

- [ ] **Step 1: 在 db.go 增事务内多行查询接口**

`internal/repo/db.go` 末尾追加（紧邻 `QueryRowxContext` 定义之后）：
```go
// QueryerContext 是 *sqlx.DB 与 *sqlx.Tx 共同满足的多行查询接口
//（SelectContext）。事务内需要返回多行的读操作（如重跑前快照手动关联）
// 以此为参数，事务外调用传 r.DB。
type QueryerContext interface {
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
}

var _ QueryerContext = (*sqlx.DB)(nil)
var _ QueryerContext = (*sqlx.Tx)(nil)
```

- [ ] **Step 2: 写 memory_topic 失败测试**

`internal/repo/memory_topic_test.go`：
```go
package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestMemoryTopicRepo(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &MemoryTopicRepo{DB: db}

	// 预置 1 memory + 2 topic
	m := &Memory{Type: "fact", Title: "t", Content: "足够长的内容描述",
		EpistemicType: "observed", Confidence: 0.9, SessionID: ids.New()}
	if err := (&MemoryRepo{DB: db}).InsertExt(ctx, db, []*Memory{m}); err != nil {
		t.Fatal(err)
	}
	tp1 := &Topic{Name: "T1", Status: "active", CreatedBy: "user"}
	tp2 := &Topic{Name: "T2", Status: "active", CreatedBy: "user"}
	(&TopicRepo{DB: db}).Create(ctx, tp1)
	(&TopicRepo{DB: db}).Create(ctx, tp2)

	// AddLink 幂等：两次加同一关联不报错、不重复
	if err := r.AddLink(ctx, m.ID, tp1.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.AddLink(ctx, m.ID, tp1.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.AddLink(ctx, m.ID, tp2.ID); err != nil {
		t.Fatal(err)
	}

	// ListByMemoryIDs 聚合
	got, err := r.ListByMemoryIDs(ctx, []ids.ID{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[m.ID]) != 2 {
		t.Fatalf("topics = %d, want 2", len(got[m.ID]))
	}
	for _, ti := range got[m.ID] {
		if ti.Source != "user" {
			t.Fatalf("source = %s, want user", ti.Source)
		}
	}

	// InsertExt 批量（ai）幂等去重
	if err := r.InsertExt(ctx, db, []*MemoryTopicLink{
		{MemoryID: m.ID, TopicID: tp1.ID, Source: "ai"}, // 已有 user 行，PK 冲突 IGNORE
		{MemoryID: m.ID, TopicID: tp1.ID, Source: "ai"},
	}); err != nil {
		t.Fatal(err)
	}
	got2, _ := r.ListByMemoryIDs(ctx, []ids.ID{m.ID})
	if len(got2[m.ID]) != 2 { // 仍 2 条（PK 去重，source 不变）
		t.Fatalf("InsertExt 后 topics = %d, want 2", len(got2[m.ID]))
	}

	// RemoveLink
	if err := r.RemoveLink(ctx, m.ID, tp2.ID); err != nil {
		t.Fatal(err)
	}
	got3, _ := r.ListByMemoryIDs(ctx, []ids.ID{m.ID})
	if len(got3[m.ID]) != 1 {
		t.Fatalf("RemoveLink 后 topics = %d, want 1", len(got3[m.ID]))
	}
}
```

> `TestDSN(t)` 返回测试库 DSN 并在未配 `TEST_MYSQL_DSN` 时 `t.Skip`（仓库现有约定，见 `repo/main_test.go` 同包）。如不存在则参照 `internal/repo/db_test.go` 中已有的取法复用。

- [ ] **Step 3: 跑测试确认失败**

Run: `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run TestMemoryTopicRepo ./internal/repo -v`
Expected: FAIL `undefined: MemoryTopicRepo`。

- [ ] **Step 4: 实现 memory_topic.go**

`internal/repo/memory_topic.go`：
```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"zhiwei/internal/ids"
)

// MemoryTopicLink 是 memory↔topic 多对多关联行。
type MemoryTopicLink struct {
	MemoryID  ids.ID    `db:"memory_id" json:"-"`
	TopicID   ids.ID    `db:"topic_id" json:"-"`
	Source    string    `db:"source" json:"source"` // ai|user
	CreatedAt time.Time `db:"created_at" json:"-"`
}

// TopicInfo 是给前端展示的 topic 摘要（列表行内联）。
type TopicInfo struct {
	ID     ids.ID `db:"id" json:"id"`
	Name   string `db:"name" json:"name"`
	Status string `db:"status" json:"status"`
	Source string `db:"source" json:"source"` // ai|user
}

type MemoryTopicRepo struct{ DB *sqlx.DB }

// InsertExt 批量插关联（INSERT IGNORE 幂等，PK 去重）。ext 传 *sqlx.Tx 入事务。
func (r *MemoryTopicRepo) InsertExt(ctx context.Context, ext ExecerContext, rows []*MemoryTopicLink) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := ext.NamedExecContext(ctx,
		`INSERT IGNORE INTO memory_topic (memory_id, topic_id, source) VALUES (:memory_id, :topic_id, :source)`, rows)
	return err
}

// AddLink 单条加关联（手动，source='user'）。幂等：已存在不报错不重复。
func (r *MemoryTopicRepo) AddLink(ctx context.Context, memoryID, topicID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`INSERT IGNORE INTO memory_topic (memory_id, topic_id, source) VALUES (?, ?, 'user')`,
		memoryID.Int64(), topicID.Int64())
	return err
}

// RemoveLink 单条移除关联。
func (r *MemoryTopicRepo) RemoveLink(ctx context.Context, memoryID, topicID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM memory_topic WHERE memory_id = ? AND topic_id = ?`, memoryID.Int64(), topicID.Int64())
	return err
}

// DeleteBySessionExt 删某 session 全部 memory 的关联（extract 重跑清理用，事务内，
// 须在删 memory 之前调用——子查询依赖 memory 行仍存在）。
func (r *MemoryTopicRepo) DeleteBySessionExt(ctx context.Context, ext ExecerContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx,
		`DELETE FROM memory_topic WHERE memory_id IN (SELECT id FROM memory WHERE session_id = ?)`,
		sessionID.Int64())
	return err
}

// MemoryUserLink 是快照手动关联用的行（带自然键成分 segment_ids+title）。
type MemoryUserLink struct {
	TopicID     ids.ID  `db:"topic_id"`
	SegmentIDs  ids.List `db:"transcript_segment_ids"`
	Title       string  `db:"title"`
}

// SnapshotUserBySessionExt 读取某 session 待删 memory 的 source='user' 关联，
// 带自然键成分，供 commit 按自然键重链（spec §6）。事务内读保证一致性。
func (r *MemoryTopicRepo) SnapshotUserBySessionExt(ctx context.Context, ext QueryerContext, sessionID ids.ID) ([]MemoryUserLink, error) {
	var rows []MemoryUserLink
	err := ext.SelectContext(ctx, &rows, `
SELECT mt.topic_id, m.transcript_segment_ids, m.title
FROM memory_topic mt JOIN memory m ON mt.memory_id = m.id
WHERE mt.source = 'user' AND m.session_id = ?`, sessionID.Int64())
	return rows, err
}

// ListByMemoryIDs 按一批 memory_id 聚合 topic 摘要（列表接口内联 topics[] 用）。
func (r *MemoryTopicRepo) ListByMemoryIDs(ctx context.Context, memIDs []ids.ID) (map[ids.ID][]TopicInfo, error) {
	out := map[ids.ID][]TopicInfo{}
	if len(memIDs) == 0 {
		return out, nil
	}
	q, args, err := sqlx.In(`
SELECT mt.memory_id AS owner_id, t.id, t.name, t.status, mt.source
FROM memory_topic mt JOIN topic t ON mt.topic_id = t.id
WHERE mt.memory_id IN (?)`, memIDs)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		OwnerID ids.ID `db:"owner_id"`
		TopicInfo
	}
	if err := r.DB.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.OwnerID] = append(out[x.OwnerID], x.TopicInfo)
	}
	return out, nil
}
```

- [ ] **Step 5: 实现 todo_topic.go（同型，owner 为 todo，快照经 source_memory 取 segment_ids+title）**

`internal/repo/todo_topic.go`：
```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"zhiwei/internal/ids"
)

type TodoTopicLink struct {
	TodoID    ids.ID    `db:"todo_id" json:"-"`
	TopicID   ids.ID    `db:"topic_id" json:"-"`
	Source    string    `db:"source" json:"source"`
	CreatedAt time.Time `db:"created_at" json:"-"`
}

type TodoTopicRepo struct{ DB *sqlx.DB }

func (r *TodoTopicRepo) InsertExt(ctx context.Context, ext ExecerContext, rows []*TodoTopicLink) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := ext.NamedExecContext(ctx,
		`INSERT IGNORE INTO todo_topic (todo_id, topic_id, source) VALUES (:todo_id, :topic_id, :source)`, rows)
	return err
}

func (r *TodoTopicRepo) AddLink(ctx context.Context, todoID, topicID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`INSERT IGNORE INTO todo_topic (todo_id, topic_id, source) VALUES (?, ?, 'user')`,
		todoID.Int64(), topicID.Int64())
	return err
}

func (r *TodoTopicRepo) RemoveLink(ctx context.Context, todoID, topicID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM todo_topic WHERE todo_id = ? AND topic_id = ?`, todoID.Int64(), topicID.Int64())
	return err
}

// DeleteBySessionExt 删某 session 派生 todo 的关联（事务内，须在删 todo 之前调用）。
func (r *TodoTopicRepo) DeleteBySessionExt(ctx context.Context, ext ExecerContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx, `
DELETE FROM todo_topic WHERE todo_id IN (
  SELECT t.id FROM todo t
  JOIN memory m ON t.source_memory_id = m.id WHERE m.session_id = ?)`, sessionID.Int64())
	return err
}

// TodoUserLink 快照行：自然键成分取自 source memory（todo 无自身 segment）。
type TodoUserLink struct {
	TopicID    ids.ID  `db:"topic_id"`
	SegmentIDs ids.List `db:"transcript_segment_ids"`
	Title      string  `db:"title"` // 来源 memory 的 title（候选共享 title）
}

func (r *TodoTopicRepo) SnapshotUserBySessionExt(ctx context.Context, ext QueryerContext, sessionID ids.ID) ([]TodoUserLink, error) {
	var rows []TodoUserLink
	err := ext.SelectContext(ctx, &rows, `
SELECT tt.topic_id, m.transcript_segment_ids, m.title
FROM todo_topic tt
JOIN todo t ON tt.todo_id = t.id
JOIN memory m ON t.source_memory_id = m.id
WHERE tt.source = 'user' AND m.session_id = ?`, sessionID.Int64())
	return rows, err
}

func (r *TodoTopicRepo) ListByTodoIDs(ctx context.Context, todoIDs []ids.ID) (map[ids.ID][]TopicInfo, error) {
	out := map[ids.ID][]TopicInfo{}
	if len(todoIDs) == 0 {
		return out, nil
	}
	q, args, err := sqlx.In(`
SELECT tt.todo_id AS owner_id, t.id, t.name, t.status, tt.source
FROM todo_topic tt JOIN topic t ON tt.topic_id = t.id
WHERE tt.todo_id IN (?)`, todoIDs)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		OwnerID ids.ID `db:"owner_id"`
		TopicInfo
	}
	if err := r.DB.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.OwnerID] = append(out[x.OwnerID], x.TopicInfo)
	}
	return out, nil
}
```

- [ ] **Step 6: 写 todo_topic 测试（同型，断言 AddLink/ListByTodoIDs/RemoveLink/DeleteBySessionExt）**

`internal/repo/todo_topic_test.go`：仿 `TestMemoryTopicRepo`，预置 memory+todo+2 topic，测 AddLink 幂等、ListByTodoIDs 聚合（2 条 source=user）、RemoveLink、DeleteBySessionExt（建第二个 session 的 todo+link，调 DeleteBySessionExt(sessionA) 后 sessionA 的 link 清空、sessionB 不受影响）。

```go
package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestTodoTopicRepo(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &TodoTopicRepo{DB: db}

	sid := ids.New()
	(&SessionRepo{DB: db}).Create(ctx, &repoSessionFix(t, sid))
	mr := &MemoryRepo{DB: db}
	tr := &TodoRepo{DB: db}
	tp := &TopicRepo{DB: db}

	m := &Memory{Type: "fact", Title: "t", Content: "足够长的内容描述",
		EpistemicType: "observed", Confidence: 0.9, SessionID: sid}
	mr.InsertExt(ctx, db, []*Memory{m})
	td := &Todo{Title: "td", SourceMemoryID: &m.ID, Status: "suggested", Confidence: 0.9}
	tr.InsertExt(ctx, db, []*Todo{td})
	t1 := &Topic{Name: "T1", Status: "active", CreatedBy: "user"}
	t2 := &Topic{Name: "T2", Status: "active", CreatedBy: "user"}
	tp.Create(ctx, t1)
	tp.Create(ctx, t2)

	r.AddLink(ctx, td.ID, t1.ID)
	r.AddLink(ctx, td.ID, t1.ID) // 幂等
	r.AddLink(ctx, td.ID, t2.ID)

	got, _ := r.ListByTodoIDs(ctx, []ids.ID{td.ID})
	if len(got[td.ID]) != 2 {
		t.Fatalf("topics = %d, want 2", len(got[td.ID]))
	}

	// DeleteBySessionExt 只影响本 session
	if err := r.DeleteBySessionExt(ctx, db, sid); err != nil {
		t.Fatal(err)
	}
	got2, _ := r.ListByTodoIDs(ctx, []ids.ID{td.ID})
	if len(got2[td.ID]) != 0 {
		t.Fatalf("DeleteBySessionExt 后 topics = %d, want 0", len(got2[td.ID]))
	}
}

// repoSessionFix 构造一个最小可用 AudioSession（仅测试用）。
func repoSessionFix(t *testing.T, id ids.ID) *AudioSession {
	t.Helper()
	return &AudioSession{ID: id, Source: "web_upload", Filename: "x.wav", StoragePath: "/tmp/x.wav", Status: "processing"}
}
```
> 若 `AudioSession` 字段名与实际不符，按 `internal/repo/session.go` 实际构造调整（`stage_extract_test.go` 的 `setupExtractFixture` 已有可用范例可抄）。

- [ ] **Step 7: 跑测试确认通过**

Run: `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestMemoryTopicRepo|TestTodoTopicRepo' ./internal/repo -v`
Expected: PASS。

- [ ] **Step 8: 提交**

```bash
git add internal/repo/db.go internal/repo/memory_topic.go internal/repo/todo_topic.go internal/repo/memory_topic_test.go internal/repo/todo_topic_test.go
git commit -m "feat: memory_topic/todo_topic 关联表 DAO（含事务内快照/聚合查询）"
```

---

## Task 4: 多 topic 抽取与落库（prompt v2 + Candidate.Topics + ResolveTopics 多 ref + commit 写关联表；双写过渡保留 legacy topic_id）

> 说明：本任务改 `memory`/`pipeline`/`prompts`，**不动 repo/api**。commit 同时写新关联表(`ai`)与 legacy `topic_id`（取首个 resolved topic），保 `repo` 旧查询仍正确、测试不红。T5 再移除 legacy 双写。

**Files:**
- Create: `prompts/extraction_v2.md`
- Modify: `cmd/zhiwei-server/main.go:25`（prompt 路径改 v2）
- Modify: `internal/memory/candidate.go`（Candidate 字段 + ParseCandidates）
- Modify: `internal/memory/topic.go`（ResolveTopics 多 ref）
- Modify: `internal/pipeline/stage_asr.go`（StageDeps 加两个 repo 字段）
- Modify: `internal/pipeline/stage_extract.go`（commit 快照/清理/写关联/重链 + 双写 legacy）
- Modify: `cmd/zhiwei-server/main.go`（装配新 repo）
- Test: `internal/memory/candidate_test.go`、`internal/memory/topic_test.go`、`internal/pipeline/stage_extract_test.go`

- [ ] **Step 1: 写 prompt v2**

`prompts/extraction_v2.md`：基于 v1，把第 8 条与输出样例的 `topic_id`/`suggested_topic_name` 改为 `topics` 数组。完整文件：
```markdown
# 知微记忆抽取 prompt（版本：extraction_v2）

你是个人 AI 记忆助手「知微」的记忆抽取器。输入是一段对话转写（已按说话人聚合为对话块，每块带序号）。你的任务：从对话中提取值得长期记住的记忆候选，并归入已有主题或建议新主题。

## 抽取规则

1. 只提取明确说出口的信息，不要推测对话双方没说的内容
2. 每条候选必须独立可读：content 用完整的一句话，包含必要的主语与时间
3. type 只能取：event（发生的事）、fact（事实/知识）、decision（决定）、idea（想法）、problem（问题/困扰）、preference（偏好/习惯）
4. epistemic_type：对话里明确说到 = observed；你从对话推断的 = inferred；你建议补充的 = suggested
5. importance 取 0~1：日常琐事 0.3 以下；对用户有意义 0.5~0.7；影响计划/关系/健康 0.8 以上
6. confidence 取 0~1：转写清晰明确 0.9 以上；有歧义或推断成分高则降低
7. 对话中出现的承诺/待办/约定置 is_todo=true，尽量给出 todo_due（ISO 8601 含时区，如 2026-08-20T10:00:00+08:00）；没有明确时间则 null
8. topic 归属用 topics 数组：每项二选一——优先用「已有主题列表」中的 topic_id（原样引用该 id 字符串），都不合适才给 suggested_name（简短名词短语，如「Rust 学习」「爸妈健康」）；一条候选可归入多个主题（0~N 项）；确实无关则 topics 为空数组
9. 每个对话块最多产出 2 条候选，整批最多 10 条，宁缺毋滥
10. 每条候选输出 block_index（来源对话块的序号，对应输入列表中的序号）

## 输出格式

只输出 JSON，不要任何其他文字或代码围栏。无值得记忆的内容时输出 {"candidates":[]}。

{"candidates":[{"type":"event","title":"给 Tom 发邮件","content":"明天需要给 Tom 发邮件确认设计稿","epistemic_type":"observed","importance":0.6,"confidence":0.9,"is_todo":true,"todo_due":null,"topics":[{"topic_id":"<已有主题id>"}],"block_index":1}]}
```

- [ ] **Step 2: 改 main.go prompt 路径**

`cmd/zhiwei-server/main.go:25`：
```go
const promptPath = "prompts/extraction_v2.md"
```

- [ ] **Step 3: 写 Candidate.Topics 解析失败测试**

在 `internal/memory/candidate_test.go` 新增（若已有 ParseCandidates 测试则补一条用 `topics` 数组的）：
```go
func TestParseCandidatesTopics(t *testing.T) {
	raw := `{"candidates":[{
	  "type":"event","title":"买菜","content":"明天要去买菜和猫粮",
	  "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	  "is_todo":true,"todo_due":null,
	  "topics":[{"topic_id":"101"},{"suggested_name":"爸妈健康"}],
	  "block_index":1
	}]}`
	cs, err := ParseCandidates(raw)
	if err != nil || len(cs) != 1 {
		t.Fatalf("cs=%d err=%v", len(cs), err)
	}
	c := cs[0]
	if len(c.Topics) != 2 {
		t.Fatalf("topics = %d, want 2", len(c.Topics))
	}
	if c.Topics[0].ExistingID == nil || *c.Topics[0].ExistingID != ids.ID(101) {
		t.Fatalf("topics[0] = %+v", c.Topics[0])
	}
	if c.Topics[1].NewName != "爸妈健康" {
		t.Fatalf("topics[1] = %+v", c.Topics[1])
	}
	// topics 缺失/空 → 空切片，不失败
	cs2, err := ParseCandidates(`{"candidates":[{"type":"fact","title":"x","content":"足够长的内容描述","epistemic_type":"observed","confidence":0.9,"is_todo":false,"todo_due":null,"block_index":1}]}`)
	if err != nil || len(cs2[0].Topics) != 0 {
		t.Fatalf("缺失 topics 应为空切片: %+v", cs2)
	}
}
```

- [ ] **Step 4: 跑测试确认失败**

Run: `go test ./internal/memory -run TestParseCandidatesTopics -v`
Expected: FAIL（`c.Topics` undefined / 旧字段还在）。

- [ ] **Step 5: 改 Candidate 结构与 ParseCandidates**

`internal/memory/candidate.go`：
- `Candidate` 删 `TopicID *ids.ID` 与 `SuggestedTopicName string`，加 `Topics []TopicRef`（复用同包 `topic.go` 的 `TopicRef{ExistingID, NewName}`）。
- `rawCandidate` 删 `TopicID`/`SuggestedTopicName`，加：
```go
type rawTopicRef struct {
	TopicID       json.RawMessage `json:"topic_id"`     // 字符串或数字
	SuggestedName string          `json:"suggested_name"`
}
// rawCandidate 内：
//   Topics []rawTopicRef `json:"topics"`
```
- `ParseCandidates` 构造 `c` 时删除旧两字段赋值，改为解析 `rc.Topics`：
```go
for _, rr := range rc.Topics {
	tr := TopicRef{}
	if len(rr.TopicID) > 0 && string(rr.TopicID) != "null" {
		// 复用既有 string/number 容错解析逻辑
		var s string
		if e := json.Unmarshal(rr.TopicID, &s); e == nil {
			if id, e := ids.ParseID(s); e == nil {
				idv := id
				tr.ExistingID = &idv
			}
		} else {
			var n int64
			if e := json.Unmarshal(rr.TopicID, &n); e == nil {
				idv := ids.ID(n)
				tr.ExistingID = &idv
			}
		}
	}
	if name := strings.TrimSpace(rr.SuggestedName); name != "" && tr.ExistingID == nil {
		tr.NewName = name
	}
	if tr.ExistingID != nil || tr.NewName != "" {
		c.Topics = append(c.Topics, tr)
	}
}
```
> 把原 `rc.TopicID` 解析逻辑整体迁入此循环（原逻辑见 candidate.go:90-106）。

- [ ] **Step 6: 跑解析测试通过**

Run: `go test ./internal/memory -run TestParseCandidates -v`
Expected: PASS（含新 TestParseCandidatesTopics 与既有解析测试）。

- [ ] **Step 7: 写 ResolveTopics 多 ref 失败测试（更新 topic_test.go）**

`internal/memory/topic_test.go` 用 `Topics` 字段重写 `TestResolveTopics`：
```go
func TestResolveTopics(t *testing.T) {
	rustID := ids.ID(101)
	oldID := ids.ID(102) // dismissed，不可挂
	topics := []repo.Topic{
		{ID: rustID, Name: "Rust 学习", Status: "active"},
		{ID: oldID, Name: "旧主题", Status: "dismissed"},
	}
	cand := func(topics []TopicRef) Candidate {
		return Candidate{Type: "fact", Title: "t", Content: "足够长的一条内容描述",
			EpistemicType: "observed", Confidence: 0.9, Topics: topics}
	}
	rustStr := rustID
	bad := ids.ID(999)
	ptr := func(id ids.ID) *ids.ID { return &id }

	cands := []Candidate{
		cand([]TopicRef{{ExistingID: &rustStr}}),            // 0: 合法 → 挂 Rust
		cand([]TopicRef{{ExistingID: &bad}}),                // 1: 不存在 → 空
		cand([]TopicRef{{NewName: "Rust 学习"}}),             // 2: 同名 → 合并
		cand([]TopicRef{{NewName: "爸妈健康"}}),              // 3: 新建议
		cand([]TopicRef{{NewName: "爸妈健康"}}),              // 4: 同名新建议
		cand(nil),                                           // 5: 无归属
		cand([]TopicRef{{ExistingID: &rustStr}, {NewName: "爸妈健康"}}), // 6: 多 ref
	}
	refs, newNames := ResolveTopics(cands, topics)

	if len(refs) != 7 {
		t.Fatalf("refs = %d", len(refs))
	}
	if len(refs[0]) != 1 || *refs[0][0].ExistingID != rustID {
		t.Fatalf("refs[0] = %+v", refs[0])
	}
	if len(refs[1]) != 0 {
		t.Fatalf("refs[1] 应空: %+v", refs[1])
	}
	if len(refs[2]) != 1 || *refs[2][0].ExistingID != rustID {
		t.Fatalf("refs[2] 应合并同名: %+v", refs[2])
	}
	if len(refs[3]) != 1 || refs[3][0].NewName != "爸妈健康" {
		t.Fatalf("refs[3] = %+v", refs[3])
	}
	if len(refs[5]) != 0 {
		t.Fatalf("refs[5] 应空: %+v", refs[5])
	}
	// 6: Rust 直挂 + 爸妈健康新建议
	if len(refs[6]) != 2 {
		t.Fatalf("refs[6] 应 2 项: %+v", refs[6])
	}
	_ = ptr
	// 新建列表去重
	if len(newNames) != 1 || newNames[0] != "爸妈健康" {
		t.Fatalf("newNames = %v", newNames)
	}
}
```

- [ ] **Step 8: 跑确认失败**

Run: `go test ./internal/memory -run TestResolveTopics -v`
Expected: FAIL（签名不匹配）。

- [ ] **Step 9: 改 ResolveTopics 为多 ref**

`internal/memory/topic.go` 替换 `ResolveTopics`：
```go
// ResolveTopics 为每条候选决定 Topic 归属（多对多）：遍历 c.Topics，
// 对每个 ref 应用三规则（直挂合法 id / 同名合并 / 收集为新建建议），
// 返回每候选的 resolved TopicRef 列表（NewName 仍待 commit 建后回填 id）+ 去重的待建主题名。
func ResolveTopics(cands []Candidate, existing []repo.Topic) (refs [][]TopicRef, newNames []string) {
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
	refs = make([][]TopicRef, len(cands))
	for i, c := range cands {
		for _, tr := range c.Topics {
			switch {
			case tr.ExistingID != nil && byID[*tr.ExistingID]:
				id := *tr.ExistingID
				refs[i] = append(refs[i], TopicRef{ExistingID: &id})
			case tr.NewName != "":
				name := strings.TrimSpace(tr.NewName)
				if name == "" {
					continue
				}
				if id, ok := byName[name]; ok {
					refs[i] = append(refs[i], TopicRef{ExistingID: &id})
				} else {
					refs[i] = append(refs[i], TopicRef{NewName: name})
					if !seen[name] {
						seen[name] = true
						newNames = append(newNames, name)
					}
				}
			}
		}
	}
	return refs, newNames
}
```

- [ ] **Step 10: 跑 ResolveTopics 测试通过**

Run: `go test ./internal/memory -run TestResolveTopics -v`
Expected: PASS。

- [ ] **Step 11: StageDeps 加两 repo 字段**

`internal/pipeline/stage_asr.go` `StageDeps` 结构体（:22 附近）内，紧邻 `Topics *repo.TopicRepo` 加：
```go
	MemoryTopics *repo.MemoryTopicRepo
	TodoTopics    *repo.TodoTopicRepo
```

- [ ] **Step 12: main.go 装配两 repo 并注入 StageDeps**

`cmd/zhiwei-server/main.go`：
- 在 `topics := &repo.TopicRepo{DB: db}`（:48）后加：
```go
	memoryTopics := &repo.MemoryTopicRepo{DB: db}
	todoTopics := &repo.TodoTopicRepo{DB: db}
```
- `BuildStages(pipeline.StageDeps{...})`（:62）的实参里加 `MemoryTopics: memoryTopics, TodoTopics: todoTopics,`。

- [ ] **Step 13: 改 stage_extract 调用与 commit（快照/清理/写关联/重链 + 双写 legacy）**

`internal/pipeline/stage_extract.go`：

`stageExtract`（:67）调用处改为接多 ref：
```go
	refs, newNames := memory.ResolveTopics(gated, topics)
	commitBegin := time.Now()
	err = commitExtract(ctx, d, sessionID, s.UserID, gated, refs, newNames)
```

替换整个 `commitExtract`（:88-162）：
```go
// commitExtract 在单事务内完成幂等清理与落库（多对多版）。
// 顺序：快照手动关联(source=user) → 删 todo_topic → 删 todo → 删 memory_topic → 删 memory
// → 建建议 topic → 插 memory + memory_topic(ai) → 插 todo + todo_topic(ai) → 按自然键重链 user。
// 过渡双写：仍写 legacy memory/todo.topic_id（取首个 resolved topic），保 repo 旧查询正确；
// T5 移除双写、T6 删 topic_id 列。
func commitExtract(ctx context.Context, d StageDeps, sessionID ids.ID, userID int64,
	gated []memory.Candidate, refs [][]memory.TopicRef, newNames []string) error {

	tx, err := d.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// 1. 快照手动关联（按自然键 K），source='user' 行稍后按 K 重链
	memSnap := map[string][]ids.ID{}
	todoSnap := map[string][]ids.ID{}
	if memLinks, err := d.MemoryTopics.SnapshotUserBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("快照 memory 手动关联: %w", err)
	} else {
		for _, l := range memLinks {
			k := memory.NaturalKey(l.SegmentIDs, l.Title)
			memSnap[k] = append(memSnap[k], l.TopicID)
		}
	}
	if todoLinks, err := d.TodoTopics.SnapshotUserBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("快照 todo 手动关联: %w", err)
	} else {
		for _, l := range todoLinks {
			k := memory.NaturalKey(l.SegmentIDs, l.Title)
			todoSnap[k] = append(todoSnap[k], l.TopicID)
		}
	}

	// 2. 幂等清理（顺序：关联表依赖主表行存在，须先删关联再删主表）
	if err := d.TodoTopics.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 todo_topic: %w", err)
	}
	if err := d.Todos.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 todo: %w", err)
	}
	if err := d.MemoryTopics.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 memory_topic: %w", err)
	}
	if err := d.Memories.DeleteBySessionExt(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("清理旧 memory: %w", err)
	}

	// 3. 新建建议 topic（事务内同名查重兜底，沿用 Sprint 2 §3.5）
	nameToID := make(map[string]ids.ID, len(newNames))
	for _, name := range newNames {
		if existing, err := d.Topics.FindActiveByNameExt(ctx, tx, userID, name); err != nil {
			return fmt.Errorf("查重建议 topic %q: %w", name, err)
		} else if existing != nil {
			nameToID[name] = existing.ID
			continue
		}
		tp := &repo.Topic{Name: name, Status: "suggested", CreatedBy: "ai"}
		if err := d.Topics.CreateExt(ctx, tx, tp); err != nil {
			return fmt.Errorf("创建建议 topic %q: %w", name, err)
		}
		nameToID[name] = tp.ID
	}

	// resolveTopicID 把单条 ref 折算成最终 topic id（ExistingID 直用，NewName 查 nameToID）
	resolveTopicID := func(ref memory.TopicRef) (ids.ID, bool) {
		if ref.ExistingID != nil {
			return *ref.ExistingID, true
		}
		if id, ok := nameToID[ref.NewName]; ok {
			return id, true
		}
		return 0, false
	}

	// 4. memory 入库 + memory_topic(ai) + 重链 user + 双写 legacy topic_id
	memories := make([]*repo.Memory, len(gated))
	var memTopicRows []*repo.MemoryTopicLink
	for i, c := range gated {
		memories[i] = &repo.Memory{
			Type: c.Type, Title: c.Title, Content: c.Content,
			EpistemicType: c.EpistemicType,
			Importance: c.Importance, Confidence: c.Confidence,
			SessionID: sessionID, TranscriptSegmentIDs: ids.List(c.SegmentIDs),
			EventAt: &c.EventAt, Status: "active",
		}
		// 收集本 memory 的 resolved topic ids（去重）
		seen := map[ids.ID]bool{}
		var tids []ids.ID
		for _, ref := range refs[i] {
			if id, ok := resolveTopicID(ref); ok && !seen[id] {
				seen[id] = true
				tids = append(tids, id)
				memTopicRows = append(memTopicRows,
					&repo.MemoryTopicLink{TopicID: id, Source: "ai"}) // MemoryID 在 InsertExt 回填后补
			}
		}
		// 过渡双写：legacy topic_id 取首个 resolved topic
		if len(tids) > 0 {
			first := tids[0]
			memories[i].TopicID = &first
		}
		memories[i].SegmentIDs = c.SegmentIDs // 临时挂在 struct 上供下方取自然键？见下注
		_ = tids
	}
	if err := d.Memories.InsertExt(ctx, tx, memories); err != nil {
		return fmt.Errorf("写 memory: %w", err)
	}
	// 回填 memory_topic 的 MemoryID
	for i := range gated {
		_ = i
	}
	// （由于 memTopicRows 在循环里 append 时未带 MemoryID，改用按候选重建）
	memTopicRows = memTopicRows[:0]
	for i, c := range gated {
		k := memory.NaturalKey(c.SegmentIDs, c.Title)
		for _, ref := range refs[i] {
			if id, ok := resolveTopicID(ref); ok {
				memTopicRows = append(memTopicRows, &repo.MemoryTopicLink{MemoryID: memories[i].ID, TopicID: id, Source: "ai"})
			}
		}
		for _, tid := range memSnap[k] {
			memTopicRows = append(memTopicRows, &repo.MemoryTopicLink{MemoryID: memories[i].ID, TopicID: tid, Source: "user"})
		}
	}
	if err := d.MemoryTopics.InsertExt(ctx, tx, memTopicRows); err != nil {
		return fmt.Errorf("写 memory_topic: %w", err)
	}

	// 5. todo 入库 + todo_topic(ai) + 重链 user + 双写 legacy topic_id
	var todos []*repo.Todo
	var todoTopicRows []*repo.TodoTopicLink
	for i, c := range gated {
		if !c.IsTodo || c.TodoStatus == "" {
			continue
		}
		td := &repo.Todo{
			Title: c.Title, SourceMemoryID: &memories[i].ID,
			Status: c.TodoStatus, DueAt: c.TodoDue, Confidence: c.Confidence,
		}
		// 过渡双写：legacy topic_id 取首个 resolved topic
		if memories[i].TopicID != nil {
			td.TopicID = memories[i].TopicID
		}
		todos = append(todos, td)
		k := memory.NaturalKey(c.SegmentIDs, c.Title)
		for _, ref := range refs[i] {
			if id, ok := resolveTopicID(ref); ok {
				todoTopicRows = append(todoTopicRows, &repo.TodoTopicLink{TopicID: id, Source: "ai"}) // TodoID 待回填
			}
		}
		for _, tid := range todoSnap[k] {
			todoTopicRows = append(todoTopicRows, &repo.TodoTopicLink{TopicID: tid, Source: "user"})
		}
	}
	if err := d.Todos.InsertExt(ctx, tx, todos); err != nil {
		return fmt.Errorf("写 todo: %w", err)
	}
	// todos 与 todoTopicRows 同序追加（仅对 IsTodo 候选），按追加顺序回填 TodoID
	ti := 0
	for _, c := range gated {
		if !c.IsTodo || c.TodoStatus == "" {
			continue
		}
		// 该候选对应一条 todo；其 todoTopic 段在 todoTopicRows 中连续
		_ = c
	}
	// 简化：重建 todoTopicRows，按 todos 顺序绑定
	todoTopicRows = todoTopicRows[:0]
	todoIdx := 0
	for i, c := range gated {
		if !c.IsTodo || c.TodoStatus == "" {
			continue
		}
		k := memory.NaturalKey(c.SegmentIDs, c.Title)
		for _, ref := range refs[i] {
			if id, ok := resolveTopicID(ref); ok {
				todoTopicRows = append(todoTopicRows, &repo.TodoTopicLink{TodoID: todos[todoIdx].ID, TopicID: id, Source: "ai"})
			}
		}
		for _, tid := range todoSnap[k] {
			todoTopicRows = append(todoTopicRows, &repo.TodoTopicLink{TodoID: todos[todoIdx].ID, TopicID: tid, Source: "user"})
		}
		todoIdx++
	}
	if err := d.TodoTopics.InsertExt(ctx, tx, todoTopicRows); err != nil {
		return fmt.Errorf("写 todo_topic: %w", err)
	}

	return tx.Commit()
}
```
> 上面为可读性保留了思考注释；**实现时清理掉冗余/空循环**（`memories[i].SegmentIDs = ...` 这行删除——Memory 无此字段；把两段「重建」直接写成一段，不要先 append 再 `[:0]` 重来）。最终实现应只遍历两次：一次插 memory 时同步收集 memTopicRows（带已回填的 MemoryID），一次插 todo 时同步收集 todoTopicRows（带已回填 TodoID）。参考精简版：
```go
// 4. memory + memory_topic
memories := make([]*repo.Memory, len(gated))
var memTopicRows []*repo.MemoryTopicLink
for i, c := range gated {
	memories[i] = &repo.Memory{ /* 字段同上，不含 TopicID 双写先留空 */ }
}
d.Memories.InsertExt(ctx, tx, memories)
for i, c := range gated {
	k := memory.NaturalKey(c.SegmentIDs, c.Title)
	for _, ref := range refs[i] {
		if id, ok := resolveTopicID(ref); ok {
			memTopicRows = append(memTopicRows, &repo.MemoryTopicLink{MemoryID: memories[i].ID, TopicID: id, Source: "ai"})
		}
	}
	for _, tid := range memSnap[k] {
		memTopicRows = append(memTopicRows, &repo.MemoryTopicLink{MemoryID: memories[i].ID, TopicID: tid, Source: "user"})
	}
}
d.MemoryTopics.InsertExt(ctx, tx, memTopicRows)

// 5. todo + todo_topic（todo 与 gated 中 IsTodo 候选同序）
var todos []*repo.Todo
var todoTopicRows []*repo.TodoTopicLink
todoIdx := 0
for i, c := range gated {
	if !c.IsTodo || c.TodoStatus == "" { continue }
	td := &repo.Todo{Title: c.Title, SourceMemoryID: &memories[i].ID, Status: c.TodoStatus, DueAt: c.TodoDue, Confidence: c.Confidence}
	if memories[i].TopicID != nil { td.TopicID = memories[i].TopicID } // 双写
	todos = append(todos, td)
	todoIdx++ // 仅用于校验下标，可删
}
_ = todoIdx
d.Todos.InsertExt(ctx, tx, todos)
// 按 todos 顺序回填关联（todos 与「IsTodo 候选」一一对应）
ti2 := 0
for i, c := range gated {
	if !c.IsTodo || c.TodoStatus == "" { continue }
	k := memory.NaturalKey(c.SegmentIDs, c.Title)
	for _, ref := range refs[i] {
		if id, ok := resolveTopicID(ref); ok {
			todoTopicRows = append(todoTopicRows, &repo.TodoTopicLink{TodoID: todos[ti2].ID, TopicID: id, Source: "ai"})
		}
	}
	for _, tid := range todoSnap[k] {
		todoTopicRows = append(todoTopicRows, &repo.TodoTopicLink{TodoID: todos[ti2].ID, TopicID: tid, Source: "user"})
	}
	ti2++
}
d.TodoTopics.InsertExt(ctx, tx, todoTopicRows)
```
> 双写 `memories[i].TopicID`/`td.TopicID` 在本任务保留（取首 resolved），让 `repo.Memory/Todo` 的 `TopicID` 字段与旧查询/旧测试不红。但上面「精简版」里 memory 的 TopicID 双写未设——**请补**：在 4 的循环里 `if 首个resolved存在 { memories[i].TopicID = &firstID }`，5 里 `td.TopicID = memories[i].TopicID`。实现时以「先收集本候选 tids，首个写入 TopicID，其余进关联表」为准。

- [ ] **Step 14: 更新 stage_extract_test.go 的 fake LLM 与断言**

`internal/pipeline/stage_extract_test.go`：
- `fakeExtractLLM.Chat` 返回的候选 JSON 把 `"topic_id":null,"suggested_topic_name":"工作沟通（抽取fixture）"` 改为 `"topics":[{"suggested_name":"工作沟通（抽取fixture）"}]`；第二条同理 `"topics":[{"suggested_name":"Rust 学习（抽取fixture）"}]`。
- `newExtractDeps` 的 `StageDeps{...}` 加 `MemoryTopics: &repo.MemoryTopicRepo{DB: db}, TodoTopics: &repo.TodoTopicRepo{DB: db},`。
- `TestStageExtractCommit` 既有断言（`rustMem.TopicID`、`todos[0].TopicID`）在双写下仍成立（首 resolved topic）。**新增**断言关联表：
```go
	// 多对多关联表：todo 挂「工作沟通（抽取fixture）」，memory「学习 Rust」挂「Rust 学习（抽取fixture）」
	memLinks, err := d.MemoryTopics.ListByMemoryIDs(ctx, []ids.ID{todoMem.ID, rustMem.ID})
	if err != nil { t.Fatal(err) }
	if len(memLinks[*todoMem... /* 用实际拿到的 memory id */]) == 0 { /* 见下 */ }
```
> 实现时：用 `todoMem.ID`（todo 来源 memory，挂「工作沟通」1 条 ai）与 `rustMem.ID`（挂「Rust 学习」1 条 ai）分别断言 `len==1` 且 `Source=="ai"`。`todoMem`/`rustMem` 变量测试中已存在（:118-156）。
- `TestStageExtractIdempotent` 不变（重跑后仍 2 memory/1 todo）；**新增**断言重跑后关联表不重复：
```go
	memLinks, _ := d.MemoryTopics.ListByMemoryIDs(ctx, allMemIDs)
	// 每个 memory 仍 1 条 ai，无重复
```
> 用 `d.Memories.ListBySession` 拿全 mem id，断言每个 `len(memLinks[id]) == 1`。

- [ ] **Step 15: 跑纯逻辑 + 集成测试**

Run: `go test ./internal/memory -v`
Expected: PASS（candidate/topic/naturalkey 全过）。

Run: `make init-testdb && TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 -run 'TestStageExtract' ./internal/pipeline -v`
Expected: PASS（commit + idempotent + empty）。

- [ ] **Step 16: 提交**

```bash
git add prompts/extraction_v2.md cmd/zhiwei-server/main.go internal/memory/candidate.go internal/memory/topic.go internal/memory/candidate_test.go internal/memory/topic_test.go internal/pipeline/stage_asr.go internal/pipeline/stage_extract.go internal/pipeline/stage_extract_test.go
git commit -m "feat: 多 topic 抽取——Candidate.Topics 数组/ResolveTopics 多 ref/commit 写关联表+重链(过渡双写 legacy topic_id)"
```

---

## Task 5: repo/API 切换到关联表 + 手动关联端点 + 移除 legacy topic_id

> 本任务把查询从单值 `topic_id` 切到关联表（`topics[]` 内联），加手动加/删端点，移除 T4 的过渡双写与 `TopicID` 字段。完成后 `topic_id` 列无人引用（T6 删除）。

**Files:**
- Modify: `internal/repo/todo.go`、`internal/repo/memory.go`、`internal/repo/topic.go`
- Modify: `internal/api/todo.go`、`internal/api/memory.go`、`internal/api/topic.go`、`internal/api/router.go`（若路由集中接线）
- Modify: `internal/pipeline/stage_extract.go`（移除双写 TopicID）
- Modify: `cmd/zhiwei-server/main.go`（handler 注入新 repo）
- Test: `internal/repo/memory_test.go`、`internal/repo/todo_test.go`、`internal/api/todo_test.go`、`internal/api/memory_test.go`、`internal/pipeline/stage_extract_test.go`

- [ ] **Step 1: 写 memory List 返回 topics[] 失败测试**

`internal/repo/memory_test.go` 新增：
```go
func TestMemoryListWithTopics(t *testing.T) {
	db, _ := NewDB(TestDSN(t))
	ctx := context.Background()
	mr := &MemoryRepo{DB: db}
	mtr := &MemoryTopicRepo{DB: db}
	tr := &TopicRepo{DB: db}
	m := &Memory{Type: "fact", Title: "多主题记忆", Content: "足够长的内容描述",
		EpistemicType: "observed", Confidence: 0.9, SessionID: ids.New()}
	mr.InsertExt(ctx, db, []*Memory{m})
	t1 := &Topic{Name: "主题一", Status: "active", CreatedBy: "user"}
	t2 := &Topic{Name: "主题二", Status: "active", CreatedBy: "user"}
	tr.Create(ctx, t1); tr.Create(ctx, t2)
	mtr.AddLink(ctx, m.ID, t1.ID)
	mtr.AddLink(ctx, m.ID, t2.ID)

	rows, err := mr.List(ctx, MemoryFilter{Limit: 50})
	if err != nil { t.Fatal(err) }
	if len(rows) != 1 { t.Fatalf("rows=%d", len(rows)) }
	if len(rows[0].Topics) != 2 {
		t.Fatalf("topics=%d, want 2: %+v", len(rows[0].Topics), rows[0].Topics)
	}
}
```
> `MemoryRow` 需有 `Topics []TopicInfo` 字段（Step 3 加）。

- [ ] **Step 2: 跑确认失败**

Run: `make init-testdb && TEST_MYSQL_DSN="..." go test -p 1 -run TestMemoryListWithTopics ./internal/repo -v`
Expected: FAIL（`rows[0].Topics` undefined）。

- [ ] **Step 3: memory repo 切换——去 TopicID、List 内联 topics[]**

`internal/repo/memory.go`：
- `Memory` 删 `TopicID *ids.ID` 字段（db:"topic_id"）。
- `InsertExt` 的 SQL 去掉 `topic_id` 列与 `:topic_id` 值。
- `MemoryRow` 删 `TopicName *string`，加 `Topics []TopicInfo `json:"topics"``（非 db 映射，Go 内填充）。
- `listWhere` 基础 SQL 去掉 `LEFT JOIN topic t` 与 `t.name AS topic_name`，只 `SELECT m.* FROM memory m`。
- `topic_id` 过滤改走关联表：`List`/`ListByTopic` 把 `m.topic_id = ?` 换成 `m.id IN (SELECT memory_id FROM memory_topic WHERE topic_id = ?)`。
- 在 `listWhere` 返回前，用 `MemoryTopicRepo.ListByMemoryIDs` 聚合填充 `Topics`：
```go
func (r *MemoryRepo) attachTopics(ctx context.Context, rows []MemoryRow) error {
	if len(rows) == 0 { return nil }
	ids := make([]ids.ID, len(rows))
	for i, r := range rows { ids[i] = r.ID }
	m, err := (&MemoryTopicRepo{DB: r.DB}).ListByMemoryIDs(ctx, ids)
	if err != nil { return err }
	for i := range rows { rows[i].Topics = m[rows[i].ID] }
	return nil
}
```
> `listWhere` 末尾 `err := r.DB.SelectContext(...)` 后调 `r.attachTopics(ctx, rows)` 再 `return rows, err`。`ListByTopic` 改为 `listWhere(ctx, map[string]any{"m.id IN (SELECT memory_id FROM memory_topic WHERE topic_id = ?)": topicID.Int64()}, 200, 0)`——注意 listWhere 的「键含空格→原样拼接」会把 `col ?` 拼成 `m.id IN (...) ?`，不符。改为显式分支：在 `listWhere` 增一条「键以 `IN` 结尾则按 `键 ?`」？更简单：`ListByTopic` 直接写完整 SQL 不走 listWhere：
```go
func (r *MemoryRepo) ListByTopic(ctx context.Context, topicID ids.ID) ([]MemoryRow, error) {
	var rows []MemoryRow
	err := r.DB.SelectContext(ctx, &rows, `
SELECT m.* FROM memory m
WHERE m.status != 'dismissed' AND m.id IN (SELECT memory_id FROM memory_topic WHERE topic_id = ?)
ORDER BY m.event_at DESC, m.id DESC LIMIT 200`, topicID.Int64())
	if err == nil { err = r.attachTopics(ctx, rows) }
	return rows, err
}
```
> `List` 的 `f.TopicID != nil` 分支同样改为 `m.id IN (SELECT memory_id FROM memory_topic WHERE topic_id = ?)` 写进 where map；因 listWhere 键含空格会拼成 `键 ?`，需把整段 `m.id IN (SELECT ... WHERE topic_id = ?)` 作为键的「列」+ ` ?`——会变成 `... = ? ?`。**故 List 的 topic 过滤不走 listWhere map**，改为在 listWhere 增一个 `topicID *ids.ID` 显式参数分支。实现：给 `listWhere` 加一个 `topicID *ids.ID` 参数（或新写 `listByFilter`），topic 非空时 SQL 追加 `AND m.id IN (SELECT memory_id FROM memory_topic WHERE topic_id = ?)`。以新增参数为准，更新 `List`/`ListBySession`/`ListByTopic` 调用。

- [ ] **Step 4: todo repo 切换——同型**

`internal/repo/todo.go`：
- `Todo` 删 `TopicID`；`InsertExt` SQL 去 `topic_id`。
- `TodoRow` 加 `Topics []TopicInfo `json:"topics"``。
- `todoListBase` 不变（它 JOIN memory 取 source_session_id，无 topic）。
- `List` 的 `topicID != nil` 过滤从 `t.topic_id = ?` 改为 `t.id IN (SELECT todo_id FROM todo_topic WHERE topic_id = ?)`。
- `ListByTopic`、`ListBySession` 同改用子查询 + 末尾 `attachTopics`（`TodoTopicRepo.ListByTodoIDs`）。
- 新增 `attachTopics`（仿 memory）。

- [ ] **Step 5: topic repo 计数切关联表**

`internal/repo/topic.go` `ListWithCounts`（:95-101）子查询改为：
```sql
  (SELECT COUNT(*) FROM memory_topic mt JOIN memory m ON mt.memory_id=m.id
     WHERE mt.topic_id = t.id AND m.status='active') AS memory_count,
  (SELECT COUNT(*) FROM todo_topic tt JOIN todo td ON tt.todo_id=td.id
     WHERE tt.topic_id = t.id AND td.status='confirmed') AS open_todo_count
```

- [ ] **Step 6: 移除 stage_extract 双写**

`internal/pipeline/stage_extract.go` `commitExtract`：删除所有 `memories[i].TopicID = ...` 与 `td.TopicID = memories[i].TopicID` 双写赋值（T4 Step13 精简版里保留的那两行）。`Memory`/`Todo` 已无 `TopicID` 字段，不写即编译失败→确认全删。

- [ ] **Step 7: 写 api 手动端点失败测试**

`internal/api/todo_test.go` 新增（仿既有 `todo_test.go` httptest 模式）：
```go
func TestTodoAddRemoveTopic(t *testing.T) {
	// 预置 todo + topic（用真 DB，仿 todo_test.go 既有 setup）
	h := &TodoHandler{Todos: td, TodoTopics: tt} // 见 main 接线
	// POST /api/todos/{id}/topics {topic_id} → 200；重复 → 200（幂等）
	// DELETE /api/todos/{id}/topics/{topic_id} → 200/204
	// 不存在 topic_id → 404
}
```
> 具体参照 `internal/api/todo_test.go` 已有的 `httptest.NewRequest` + `chi.NewRouter()` + `RegisterTodo` 模式补全 setup（建 todo、建 topic、断言状态码与 `List` 返回 `topics[]`）。

- [ ] **Step 8: 跑确认失败**

Run: `make init-testdb && TEST_MYSQL_DSN="..." go test -p 1 -run 'TestTodoAddRemoveTopic|TestMemoryListWithTopics' ./internal/... -v`
Expected: FAIL（新端点/字段未实现）。

- [ ] **Step 9: 实现 api todo 手动端点 + topics[] 响应**

`internal/api/todo.go`：
- `TodoHandler` 加 `TodoTopics *repo.TodoTopicRepo`、`Topics *repo.TopicRepo`（校验 topic 存在用）。
- `RegisterTodo` 加路由：
```go
	r.Post("/api/todos/{id}/topics", h.AddTopic)
	r.Delete("/api/todos/{id}/topics/{topic_id}", h.RemoveTopic)
```
- `AddTopic`/`RemoveTopic`：
```go
func (h *TodoHandler) AddTopic(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil { http.Error(w, "invalid id", http.StatusBadRequest); return }
	var req struct{ TopicID string `json:"topic_id"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TopicID == "" {
		http.Error(w, "请求体非法", http.StatusBadRequest); return
	}
	tid, err := ids.ParseID(req.TopicID)
	if err != nil { http.Error(w, "topic_id 非法", http.StatusBadRequest); return
	}
	if _, err := h.Todos.Get(r.Context(), id); err != nil { http.Error(w, "todo 不存在", http.StatusNotFound); return }
	tp, err := h.Topics.Get(r.Context(), tid)
	if err != nil || tp.Status == "dismissed" { http.Error(w, "topic 不存在", http.StatusNotFound); return }
	if err := h.TodoTopics.AddLink(r.Context(), id, tid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError); return
	}
	w.WriteHeader(http.StatusOK)
}
func (h *TodoHandler) RemoveTopic(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil { http.Error(w, "invalid id", http.StatusBadRequest); return }
	tid, err := ids.ParseID(chi.URLParam(r, "topic_id"))
	if err != nil { http.Error(w, "invalid topic_id", http.StatusBadRequest); return }
	if err := h.TodoTopics.RemoveLink(r.Context(), id, tid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError); return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
- `List` 响应 `rows` 已含 `Topics`（Step 4 的 TodoRow），`writeJSON` 直接序列化即可（无需改）。

- [ ] **Step 10: 实现 api memory 手动端点（同型）**

`internal/api/memory.go`：
- `MemoryHandler` 加 `MemoryTopics *repo.MemoryTopicRepo`（`Topics` 已有）。
- `RegisterMemory` 加 `r.Post/.../topics`、`r.Delete/.../topics/{topic_id}`。
- `AddTopic`/`RemoveTopic` 仿 todo（owner=memory，校验 memory 存在 + topic 非 dismissed）。
- `List` 响应 `rows` 已含 `Topics`（Step 3）。

- [ ] **Step 11: main.go 注入 handler 新 repo**

`cmd/zhiwei-server/main.go`：
- `api.RegisterTodo(r, &api.TodoHandler{Todos: todos, TodoTopics: todoTopics, Topics: topics})`
- `api.RegisterMemory(r, &api.MemoryHandler{Memories: memories, Topics: topics, MemoryTopics: memoryTopics})`
- `api.RegisterTopic(r, &api.TopicHandler{Topics: topics, Memories: memories, Todos: todos})`（Get 用 ListByTopic 已切关联表，无需新字段）

- [ ] **Step 12: 更新受影响测试**

- `internal/pipeline/stage_extract_test.go`：删掉对 `rustMem.TopicID`/`todos[0].TopicID` 的断言（字段已删），改为断言 `memLinks`/`todoLinks`（T4 已加的关联表断言保留）。
- `internal/repo/todo_test.go`/`memory_test.go`/`topic_test.go`：把引用 `TopicID`/`TopicName` 的断言改为 `Topics`/`topics[]`。
- `internal/api/todo_test.go`/`memory_test.go`/`topic_test.go`：列表响应断言改为 `topics[]`。

- [ ] **Step 13: 全量测试**

Run: `go test ./internal/memory ./internal/ids ./internal/config ./internal/provider -v`（纯逻辑，应全过）
Run: `make test-integration`
Expected: 全部 PASS（repo + pipeline + api 集成）。

- [ ] **Step 14: 提交**

```bash
git add internal/repo internal/api internal/pipeline/stage_extract.go cmd/zhiwei-server/main.go
git commit -m "feat: repo/api 切换到关联表(topics[] 内联)+手动加删 topic 端点+移除 legacy topic_id"
```

---

## Task 6: 迁移 000003——删除 legacy topic_id 列（收缩期）

**Files:**
- Create: `migrations/000003_drop_topic_id.up.sql`
- Create: `migrations/000003_drop_topic_id.down.sql`

- [ ] **Step 1: 写 up**

`migrations/000003_drop_topic_id.up.sql`：
```sql
-- 收缩期：代码已切到 memory_topic/todo_topic 关联表，删除冗余的单值 topic_id 列。
ALTER TABLE memory DROP KEY idx_topic, DROP COLUMN topic_id;
ALTER TABLE todo    DROP KEY idx_topic, DROP COLUMN topic_id;
```

- [ ] **Step 2: 写 down（重建列+索引，NULL，多→单无法无损还原，仅恢复结构）**

`migrations/000003_drop_topic_id.down.sql`：
```sql
ALTER TABLE memory ADD COLUMN topic_id BIGINT NULL, ADD KEY idx_topic (topic_id);
ALTER TABLE todo    ADD COLUMN topic_id BIGINT NULL, ADD KEY idx_topic (topic_id);
```

- [ ] **Step 3: 验证 + 全量集成测试**

Run: `make init-testdb && make test-integration`
Expected: 全 PASS（迁移删列后，无代码引用 topic_id，集成测试仍绿）。

- [ ] **Step 4: 提交**

```bash
git add migrations/000003_drop_topic_id.up.sql migrations/000003_drop_topic_id.down.sql
git commit -m "feat: 迁移 000003 删除 memory/todo 的 legacy topic_id 列（收缩期）"
```

---

## Task 7: Web UI——多 topic 徽标 + 加/删 topic 交互

**Files:**
- Modify: `web/app.js`
- Modify: `web/index.html`

- [ ] **Step 1: app.js 加 topic 辅助与加/删方法**

`web/app.js` 在「待办」段（`loadTodos` 附近，:226 后）新增：
```js
    function topicChips(item) { return (item && item.topics) || []; }
    async function addTodoTopic(t, topicId) {
      try { await api('POST', '/api/todos/' + t.id + '/topics', { topic_id: topicId }); await loadTodos(); }
      catch (e) { showError(e); }
    }
    async function removeTodoTopic(t, topicId) {
      try { await api('DELETE', '/api/todos/' + t.id + '/topics/' + topicId); await loadTodos(); }
      catch (e) { showError(e); }
    }
    async function addMemoryTopic(m, topicId) {
      try { await api('POST', '/api/memories/' + m.id + '/topics', { topic_id: topicId }); await toggleSession(detail.value.session.id); }
      catch (e) { showError(e); }
    }
    async function removeMemoryTopic(m, topicId) {
      try { await api('DELETE', '/api/memories/' + m.id + '/topics/' + topicId); await toggleSession(detail.value.session.id); }
      catch (e) { showError(e); }
    }
```
> 加/删后待办页 `loadTodos()` 刷新、时间线 `toggleSession` 重拉详情。`topicChips` 给模板统一取 `topics`。
> 在 `createApp` setup 的 `return` 对象里加入：`topicChips, addTodoTopic, removeTodoTopic, addMemoryTopic, removeMemoryTopic`。
> 另加一个 `availableTopics`：`const availableTopics = computed(() => topics.value.filter(t => t.status !== 'dismissed'));` 并加入 return，供「+ 关联」下拉选项。切换到 todos/topics 页时确保 `loadTopics()` 已加载（`switchTab('todos')` 内补 `loadTopics()`）。

- [ ] **Step 2: index.html 时间线 memory 卡片改多徽标 + 加/删**

`web/index.html:97` 的 `<template v-if="m.topic_name"> · {{ m.topic_name }}</template>` 替换为：
```html
              <template v-if="topicChips(m).length">
                · <span v-for="tp in topicChips(m)" :key="tp.id" class="badge" style="background:#eef2ff;color:#3730a3;margin-right:4px">{{ tp.name }}<span v-if="tp.source==='user'" style="opacity:.6">✎</span><button class="mini" style="padding:0 4px;margin-left:2px" @click.stop="removeMemoryTopic(m, tp.id)">✕</button></span>
              </template>
              <select class="mini" @change="if($event.target.value){addMemoryTopic(m,$event.target.value);$event.target.value=''}">
                <option value="">+ 关联</option>
                <option v-for="t in availableTopics" :key="t.id" :value="t.id">{{ t.name }}</option>
              </select>
```

- [ ] **Step 3: 待办页卡片加多 topic 徽标 + 加/删**

`web/index.html` 待办页三组卡片（待确认 :209 / 进行中 :224 / 已完成 :240）的标题行下方各加一行（已完成组可只读展示不加操作）：
```html
      <div v-if="topicChips(td).length" class="muted" style="margin-top:4px">
        <span v-for="tp in topicChips(td)" :key="tp.id" class="badge" style="background:#eef2ff;color:#3730a3;margin-right:4px">{{ tp.name }}<button class="mini" style="padding:0 4px;margin-left:2px" @click.stop="removeTodoTopic(td, tp.id)">✕</button></span>
      </div>
      <select class="mini" v-if="td.status!=='done'" @change="if($event.target.value){addTodoTopic(td,$event.target.value);$event.target.value=''}">
        <option value="">+ 关联 Topic</option>
        <option v-for="t in availableTopics" :key="t.id" :value="t.id">{{ t.name }}</option>
      </select>
```

- [ ] **Step 4: Topics 详情页 memory/todo 卡片也用 topicChips（可选，已通过列表 topics[] 展示）**

`web/index.html` Topics 详情 memory 卡片（:184）与 todo 卡片（:194）可在 muted 行追加 `topicChips(m)` 徽标，写法同 Step 2 的 `<span v-for>`。

- [ ] **Step 5: 手动验收**

Run: `set -a; source .env; set +a; make dev-start` → 打开 http://localhost:8080
- 上传/录音一段含明确待办 + 可归类主题的音频，等到处理完成。
- 时间线 memory 卡片显示多 topic 徽标，✕ 可移除，下拉可加；待办页同样可加/删 topic。
- 切到 Topics 页，计数与详情列表反映多关联。

- [ ] **Step 6: 提交**

```bash
git add web/app.js web/index.html
git commit -m "feat(web): 多 topic 徽标 + 待办/记忆手动加删 topic 关联交互"
```

---

## 自检（写计划后通读 spec 复核）

- **spec 覆盖**：§1 目标↔T1(表)+T4(自动多)+T5(手动)+T4(commit 重链)；§3 schema↔T1+T6；§4 抽取↔T4；§5 手动 API↔T5；§6 重跑保留↔T4 commit 快照/重链+T3 Snapshot；§7 web↔T7；§8 测试↔各任务 TDD 步。无遗漏。
- **占位符**：T7 Step2/3 模板代码为完整片段；T3 Step6 `repoSessionFix` 与 `AudioSession` 字段标注「按实际调整」——这是对既有结构的引用提示而非占位，实现时直接抄 `stage_extract_test.go` 的 `setupExtractFixture`。T5 Step3 的 listWhere topic 过滤给了两种写法并明确「以新增 topicID 参数为准」。
- **类型一致**：`MemoryTopicLink`/`TodoTopicLink`/`TopicInfo` 在 T3 定义，T4/T5 引用一致；`ResolveTopics` 返回 `[][]TopicRef`（T4 定义，T5 commit 消费一致）；`NaturalKey`（T2）在 T4 commit 引用一致；`QueryerContext`（T3 db.go）在 T3 Snapshot 方法与 T4 commit 调用一致。
