# 用户画像 P1a（后端地基）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地用户画像系统 P1a 后端：4 张新表（person / person_attribute / person_relationship / person_change_log）+ repo 层 + `internal/profile` 领域包（属性目录 / LLM 抽取 / 置信度闸门 / 人物归属 / 落库编排）+ pipeline `profile` 阶段 + 全量 REST API + 主程序装配。

**Architecture:** 围绕 `person` 中心的两个数据平面（属性/关系）+ 统一审计 `person_change_log`。所有变更（手动/LLM/确认）都经 `profile.Service` 单事务写入并记审计；`Service.ExtractSession` 被 pipeline stage 与 API 回填端点共用；幂等靠自然键去重（区别于 extract 阶段的删重插，画像跨 session 累积且带用户确认状态，不能删）。设计规格：`docs/superpowers/specs/2026-08-24-person-profile-system-design.md`。

**Tech Stack:** Go 1.25 / chi v5 / sqlx / MySQL（雪花 ID、无 AUTO_INCREMENT）/ 现有 `provider.LLMProvider`（火山 Ark）。

**与规格的两处小偏差（实现层面确认更优）：**
1. `value_type` 不含 `list_item`——列表与否由目录 `Cardinality(single|list)` 表达，`value_type` 只描述值本身类型（text/enum/bool/date/number）。
2. 属性自然键去重在 SQL 里按原始 `value_text` 相等比较（闸门比较用归一化）——LLM 重复输出同值字符串已覆盖幂等场景。

**工作目录：** 全程在 worktree `.worktrees/person-profile`（分支 `feat/person-profile`）内操作：
`cd /Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/.worktrees/person-profile`

**测试命令约定：**
- 单元测试（无 MySQL）：`go test ./internal/profile/ -run TestXxx -v`
- 集成测试（单包，需 docker MySQL 已起 + 已 init-testdb）：
  `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestXxx -v`
- 全量：`make test`（单元）/ `make test-integration`（集成，自动重建测试库+迁移）

---

## 文件结构总览

| 文件 | 职责 |
|---|---|
| `migrations/000005_person.{up,down}.sql` | 4 张新表 |
| `internal/repo/person.go` | Person + PersonRepo + EnsurePersonBootstrap + RecentSessionIDs + ListWithPending |
| `internal/repo/person_attribute.go` | PersonAttribute + PersonAttributeRepo |
| `internal/repo/person_relationship.go` | PersonRelationship + PersonRelationshipRepo |
| `internal/repo/person_change_log.go` | PersonChangeLog + PersonChangeLogRepo |
| `internal/repo/session.go`（改） | 新增 ListCompletedIDs（回填端点用） |
| `internal/profile/catalog.go` | 属性目录（key→中文/分组/类型/枚举/单值列表） |
| `internal/profile/fact.go` | Fact 类型 + ParseFacts（LLM 输出解析） |
| `internal/profile/gate.go` | 置信度闸门决策 |
| `internal/profile/extractor.go` | LLM 抽取器（窗口切分/去重/溯源） |
| `prompts/profile_extraction_v1.md` | 版本化抽取 prompt |
| `internal/profile/service.go` | Service + 人物归属解析 + ApplyFacts（事务编排） |
| `internal/profile/service_manual.go` | 手动 CRUD（person/attribute/relationship，带审计） |
| `internal/profile/confirm.go` | ConfirmPending / DismissPending |
| `internal/profile/extract_session.go` | ExtractSession（stage 与回填共用入口） |
| `internal/pipeline/stage_asr.go`（改） | StageDeps 增 Profile 字段；BuildStages 注册 profile |
| `internal/pipeline/stage_profile.go` | profile stage 薄包装 |
| `internal/config/config.go`（改） | ZW_PROFILE_* 三个配置 |
| `internal/api/person.go` | 人物/属性/关系/历史/确认队列/回填全部 handler |
| `cmd/zhiwei-server/main.go`（改） | 装配：repo + bootstrap + Service + stage + API |

任务顺序即依赖顺序（迁移 → repo → 领域包 → stage → config → API → 装配）。

---

### Task 1: 迁移 000005_person（4 张表）

**Files:**
- Create: `migrations/000005_person.up.sql`
- Create: `migrations/000005_person.down.sql`

- [ ] **Step 1: 写 up 迁移**

`migrations/000005_person.up.sql`：

```sql
-- 用户画像/人物系统 P1（spec 2026-08-24-person-profile-system-design §4）。
-- 4 张表：person 主体 / person_attribute 属性平面 / person_relationship 关系平面 /
-- person_change_log 统一审计（只追加，永不 update/delete）。
-- 回填（owner person + speaker→person）不在此做：雪花 ID 由 Go 侧
-- repo.EnsurePersonBootstrap 启动时幂等生成，迁移只建表。
-- 横切字段（source/confidence/epistemic_type/status/溯源/supersedes_id/version）
-- 在 attribute 与 relationship 两表结构一致，见 spec §3。

CREATE TABLE person (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  display_name VARCHAR(128) NOT NULL,
  speaker_id   BIGINT NULL,                        -- 可选关联声纹；一个声纹至多绑一个人
  is_owner     TINYINT(1) NOT NULL DEFAULT 0,      -- 「我」本人，全局至多一个
  summary      TEXT NULL,
  source       VARCHAR(8) NOT NULL DEFAULT 'manual',  -- manual|llm（llm=抽取自动新建）
  status       VARCHAR(16) NOT NULL DEFAULT 'active', -- active|pending|merged|dismissed
  created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_speaker (speaker_id),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE person_attribute (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  person_id     BIGINT NOT NULL,
  attr_key      VARCHAR(64) NOT NULL,              -- 目录 key 或自由 key（落「其他」组）
  value_text    TEXT NOT NULL,
  value_type    VARCHAR(16) NOT NULL DEFAULT 'text', -- text|enum|bool|date|number
  confidence    DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed', -- observed|inferred|predicted|suggested
  source        VARCHAR(8) NOT NULL DEFAULT 'manual',    -- manual|llm
  status        VARCHAR(16) NOT NULL DEFAULT 'active',   -- active|pending|superseded|dismissed
  session_id    BIGINT NULL,
  memory_id     BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id BIGINT NULL,                       -- 冲突 pending 指向当前 active 行
  version       INT NOT NULL DEFAULT 1,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person_key_status (person_id, attr_key, status),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE person_relationship (
  id                BIGINT PRIMARY KEY,
  user_id           BIGINT NOT NULL DEFAULT 1,
  person_id         BIGINT NOT NULL,               -- 主体
  related_person_id BIGINT NULL,                   -- 对端人物（组织关系可空）
  relation_type     VARCHAR(24) NOT NULL,          -- 配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他
  direction         VARCHAR(8) NULL,               -- upstream|downstream|peer（上下游）
  org_name          VARCHAR(128) NULL,
  label             VARCHAR(128) NULL,             -- 自由称呼（「大儿子」「张总」）
  confidence        DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  epistemic_type    VARCHAR(16) NOT NULL DEFAULT 'observed',
  source            VARCHAR(8) NOT NULL DEFAULT 'manual',
  status            VARCHAR(16) NOT NULL DEFAULT 'active',
  session_id        BIGINT NULL,
  memory_id         BIGINT NULL,
  transcript_segment_ids JSON NULL,
  supersedes_id     BIGINT NULL,
  version           INT NOT NULL DEFAULT 1,
  created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_person (person_id, status),
  KEY idx_related (related_person_id),
  KEY idx_user_status (user_id, status),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE person_change_log (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  person_id     BIGINT NOT NULL,
  entity_kind   VARCHAR(16) NOT NULL,              -- person|attribute|relationship（P2+ 扩 event/metric/…）
  entity_id     BIGINT NULL,                       -- 目标行 id（删除后仍留历史）
  attr_key      VARCHAR(64) NULL,                  -- attribute 平面冗余，便于按字段查历史
  change_type   VARCHAR(16) NOT NULL,              -- create|update|confirm|dismiss|supersede|delete|reaffirm
  changed_by    VARCHAR(8) NOT NULL,               -- user|llm
  old_value     JSON NULL,                         -- 变更前快照（JSON 文本）
  new_value     JSON NULL,
  confidence    DECIMAL(5,4) NULL,
  epistemic_type VARCHAR(16) NULL,
  session_id    BIGINT NULL,
  memory_id     BIGINT NULL,
  transcript_segment_ids JSON NULL,
  note          TEXT NULL,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_person_kind_time (person_id, entity_kind, created_at),
  KEY idx_entity (entity_kind, entity_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- [ ] **Step 2: 写 down 迁移**

`migrations/000005_person.down.sql`：

```sql
DROP TABLE IF EXISTS person_change_log;
DROP TABLE IF EXISTS person_relationship;
DROP TABLE IF EXISTS person_attribute;
DROP TABLE IF EXISTS person;
```

- [ ] **Step 3: 验证迁移可执行**

```bash
make compose-up && make init-testdb
```
Expected: `migrate` 无报错退出（建表成功；测试库重建含 000005）。

- [ ] **Step 4: Commit**

```bash
git add migrations/000005_person.up.sql migrations/000005_person.down.sql
git commit -m "feat(profile): 迁移 000005_person——person/attribute/relationship/change_log 四表"
```

---

### Task 2: repo 层——Person + PersonRepo + bootstrap

**Files:**
- Create: `internal/repo/person.go`
- Create: `internal/repo/person_test.go`
- Modify: `internal/repo/session.go`（末尾追加 ListCompletedIDs）

- [ ] **Step 1: 写失败的集成测试**

`internal/repo/person_test.go`（本任务**只含 `TestPersonLifecycle`**；`TestPersonListWithPendingAndRecentSessions` 依赖 Task 3 的 `PersonAttributeRepo`，其完整代码在 Task 3 Step 1 追加）：

```go
package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestPersonLifecycle(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	speakers := &SpeakerRepo{DB: db}

	// bootstrap：owner 回填 + speaker→person 回填，且幂等
	sp := &Speaker{Name: "回填测试说话人"}
	if err := speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePersonBootstrap(ctx, persons, speakers); err != nil {
		t.Fatal(err)
	}
	owner, err := persons.GetOwner(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || !owner.IsOwner || owner.DisplayName != "我" {
		t.Fatalf("owner 未回填: %+v", owner)
	}
	if err := EnsurePersonBootstrap(ctx, persons, speakers); err != nil {
		t.Fatal(err)
	}
	if o2, _ := persons.GetOwner(ctx, 1); o2 == nil || o2.ID != owner.ID {
		t.Fatal("bootstrap 不幂等：owner 被重复创建")
	}
	linked, err := persons.GetBySpeaker(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linked == nil || linked.DisplayName != sp.Name {
		t.Fatalf("speaker 未回填为 person: %+v", linked)
	}

	// 新建 + 按名查找 + 更新 + 状态
	p := &Person{DisplayName: "张三"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 || p.Status != "active" || p.Source != "manual" {
		t.Fatalf("Create 默认值未兜底: %+v", p)
	}
	found, err := persons.FindByName(ctx, 1, "张三")
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || found.ID != p.ID {
		t.Fatalf("FindByName 未命中: %+v", found)
	}
	sid := sp.ID
	if err := persons.Update(ctx, p.ID, "张三丰", &sid, nil); err != nil {
		t.Fatal(err)
	}
	got, err := persons.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "张三丰" || got.SpeakerID == nil || *got.SpeakerID != sp.ID {
		t.Fatalf("Update 未生效: %+v", got)
	}
	if err := persons.SetStatus(ctx, p.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if g2, _ := persons.Get(ctx, p.ID); g2.Status != "dismissed" {
		t.Fatalf("SetStatus 未生效: %+v", g2)
	}
}

func TestPersonListWithPendingAndRecentSessions(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	attrs := &PersonAttributeRepo{DB: db}

	p := &Person{DisplayName: "计数测试人物"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	sess := ids.New()
	// 一条 active、一条 pending：pending 计数应为 1
	if err := attrs.Create(ctx, &PersonAttribute{PersonID: p.ID, AttrKey: "city", ValueText: "上海", Status: "active", SessionID: &sess}); err != nil {
		t.Fatal(err)
	}
	if err := attrs.Create(ctx, &PersonAttribute{PersonID: p.ID, AttrKey: "occupation", ValueText: "医生", Status: "pending", SessionID: &sess}); err != nil {
		t.Fatal(err)
	}

	list, err := persons.ListWithPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var hit *PersonWithPending
	for i := range list {
		if list[i].ID == p.ID {
			hit = &list[i]
		}
	}
	if hit == nil || hit.PendingCount != 1 {
		t.Fatalf("ListWithPending 计数错误: %+v", hit)
	}

	sids, err := persons.RecentSessionIDs(ctx, p.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sids) != 1 || sids[0] != sess {
		t.Fatalf("RecentSessionIDs 错误: %v", sids)
	}
}
```

- [ ] **Step 2: 跑测试确认失败（编译错误）**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestPersonLifecycle -v
```
Expected: FAIL，`undefined: PersonRepo` / `undefined: Person`（编译错误）。

- [ ] **Step 3: 实现 person.go**

`internal/repo/person.go`：

```go
package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Person 是画像主体（人物系统）：owner「我」+ 他人，可选关联 0/1 个声纹。
// 只被提到、从未录音的人（配偶/孩子）也能建档（speaker_id 为 NULL）。
// 状态机：active（正常）| pending（LLM 抽取自动新建，待确认）| merged（已并入他人，P1 不用）| dismissed（归档）。
type Person struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	DisplayName string    `db:"display_name" json:"display_name"`
	SpeakerID   *ids.ID   `db:"speaker_id" json:"speaker_id,omitempty"`
	IsOwner     bool      `db:"is_owner" json:"is_owner"`
	Summary     *string   `db:"summary" json:"summary,omitempty"`
	Source      string    `db:"source" json:"source"` // manual|llm
	Status      string    `db:"status" json:"status"` // active|pending|merged|dismissed
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// PersonWithPending 是名册列表的带计数视图（人物卡角标用）。
type PersonWithPending struct {
	Person
	PendingCount int `db:"pending_count" json:"pending_count"` // 该人物待确认的属性+关系数
}

type PersonRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。零值兜底同其他 repo。
func (r *PersonRepo) CreateExt(ctx context.Context, ext ExecerContext, p *Person) error {
	p.ID = ids.New()
	if p.UserID == 0 {
		p.UserID = 1
	}
	if p.Source == "" {
		p.Source = "manual"
	}
	if p.Status == "" {
		p.Status = "active"
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person (id, user_id, display_name, speaker_id, is_owner, summary, source, status)
VALUES (:id, :user_id, :display_name, :speaker_id, :is_owner, :summary, :source, :status)`, p)
	return err
}

func (r *PersonRepo) Create(ctx context.Context, p *Person) error {
	return r.CreateExt(ctx, r.DB, p)
}

// Get 按 id 查；不存在返回 (nil, nil)（与 FindActiveByNameExt 风格一致，调用方判 nil）。
func (r *PersonRepo) Get(ctx context.Context, id ids.ID) (*Person, error) {
	var p Person
	err := r.DB.GetContext(ctx, &p, `SELECT * FROM person WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// List 返回非 dismissed 人物（含 pending，名册要展示），is_owner 优先 + 更新时间倒序。
func (r *PersonRepo) List(ctx context.Context, userID int64) ([]Person, error) {
	var list []Person
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person WHERE user_id = ? AND status != 'dismissed'
ORDER BY is_owner DESC, updated_at DESC`, userID)
	return list, err
}

// ListWithPending 名册 + 每人 pending 计数（属性+关系），供名册角标。
func (r *PersonRepo) ListWithPending(ctx context.Context, userID int64) ([]PersonWithPending, error) {
	var list []PersonWithPending
	err := r.DB.SelectContext(ctx, &list, `
SELECT p.*,
  (SELECT COUNT(*) FROM person_attribute a WHERE a.person_id = p.id AND a.status = 'pending')
+ (SELECT COUNT(*) FROM person_relationship rel WHERE rel.person_id = p.id AND rel.status = 'pending') AS pending_count
FROM person p
WHERE p.user_id = ? AND p.status != 'dismissed'
ORDER BY p.is_owner DESC, p.updated_at DESC`, userID)
	return list, err
}

// GetOwnerExt 返回 is_owner=1 的「我」；不存在返回 (nil, nil)。可在事务连接上执行。
func (r *PersonRepo) GetOwnerExt(ctx context.Context, ext QueryRowxContext, userID int64) (*Person, error) {
	var p Person
	err := ext.QueryRowxContext(ctx,
		`SELECT * FROM person WHERE user_id = ? AND is_owner = 1 LIMIT 1`, userID).StructScan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PersonRepo) GetOwner(ctx context.Context, userID int64) (*Person, error) {
	return r.GetOwnerExt(ctx, r.DB, userID)
}

// GetBySpeakerExt 按声纹找绑定人物；未绑定返回 (nil, nil)。
func (r *PersonRepo) GetBySpeakerExt(ctx context.Context, ext QueryRowxContext, speakerID ids.ID) (*Person, error) {
	var p Person
	err := ext.QueryRowxContext(ctx,
		`SELECT * FROM person WHERE speaker_id = ? LIMIT 1`, speakerID.Int64()).StructScan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PersonRepo) GetBySpeaker(ctx context.Context, speakerID ids.ID) (*Person, error) {
	return r.GetBySpeakerExt(ctx, r.DB, speakerID)
}

// FindByNameExt 按显示名精确匹配 active/pending 人物（画像归属解析用）；无命中返回 nil。
// 只查 display_name；别名匹配（aliases 属性）由上层 profile.Service 扩展，P1 先名精确。
func (r *PersonRepo) FindByNameExt(ctx context.Context, ext QueryRowxContext, userID int64, name string) (*Person, error) {
	var p Person
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person
WHERE user_id = ? AND display_name = ? AND status IN ('active','pending')
ORDER BY is_owner DESC, id LIMIT 1`, userID, name).StructScan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PersonRepo) FindByName(ctx context.Context, userID int64, name string) (*Person, error) {
	return r.FindByNameExt(ctx, r.DB, userID, name)
}

// Update 手动编辑：改名/换绑声纹/改备注。speakerID/summary 传 nil 即清空。
func (r *PersonRepo) Update(ctx context.Context, id ids.ID, name string, speakerID *ids.ID, summary *string) error {
	var sp any
	if speakerID != nil {
		sp = speakerID.Int64()
	}
	var sm any
	if summary != nil {
		sm = *summary
	}
	_, err := r.DB.ExecContext(ctx,
		`UPDATE person SET display_name = ?, speaker_id = ?, summary = ? WHERE id = ?`,
		name, sp, sm, id.Int64())
	return err
}

func (r *PersonRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE person SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

// RecentSessionIDs 该人物画像信息溯源涉及的 session（人物页「最近互动」入口），
// 属性+关系两平面 UNION 去重。雪花 ID 时间有序，DESC 即最近优先。
func (r *PersonRepo) RecentSessionIDs(ctx context.Context, personID ids.ID, limit int) ([]ids.ID, error) {
	var out []ids.ID
	err := r.DB.SelectContext(ctx, &out, `
SELECT session_id FROM (
  SELECT session_id FROM person_attribute
   WHERE person_id = ? AND session_id IS NOT NULL AND status != 'dismissed'
  UNION
  SELECT session_id FROM person_relationship
   WHERE person_id = ? AND session_id IS NOT NULL AND status != 'dismissed'
) t ORDER BY session_id DESC LIMIT ?`, personID.Int64(), personID.Int64(), limit)
	return out, err
}

// EnsurePersonBootstrap 幂等回填（main.go 启动时调用，迁移 000005 之后）：
// ① 无 is_owner=1 人物则建「我」；② 为每个未绑定的 active speaker 建人物
// （display_name=声纹名）。查后再建，重跑无副作用。
func EnsurePersonBootstrap(ctx context.Context, persons *PersonRepo, speakers *SpeakerRepo) error {
	owner, err := persons.GetOwner(ctx, 1)
	if err != nil {
		return err
	}
	if owner == nil {
		if err := persons.Create(ctx, &Person{DisplayName: "我", IsOwner: true}); err != nil {
			return err
		}
	}
	list, err := speakers.List(ctx)
	if err != nil {
		return err
	}
	for _, sp := range list {
		if sp.Status != "active" {
			continue
		}
		p, err := persons.GetBySpeaker(ctx, sp.ID)
		if err != nil {
			return err
		}
		if p != nil {
			continue
		}
		sid := sp.ID
		if err := persons.Create(ctx, &Person{DisplayName: sp.Name, SpeakerID: &sid}); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: session.go 追加 ListCompletedIDs（回填端点用）**

在 `internal/repo/session.go` 末尾追加：

```go
// ListCompletedIDs 返回已完成 session 的 id（雪花 ID 升序=时间从旧到新），
// 画像回填端点按历史顺序重放用。limit 上限由调用方控制。
func (r *SessionRepo) ListCompletedIDs(ctx context.Context, limit int) ([]ids.ID, error) {
	var out []ids.ID
	err := r.DB.SelectContext(ctx, &out,
		`SELECT id FROM audio_session WHERE status = 'completed' ORDER BY id LIMIT ?`, limit)
	return out, err
}
```

- [ ] **Step 5: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestPersonLifecycle -v
```
Expected: PASS（bootstrap 幂等 + CRUD 全过）。

- [ ] **Step 6: Commit**

```bash
git add internal/repo/person.go internal/repo/person_test.go internal/repo/session.go
git commit -m "feat(profile): PersonRepo + EnsurePersonBootstrap + ListCompletedIDs"
```

---

### Task 3: repo 层——PersonAttribute + PersonAttributeRepo

**Files:**
- Create: `internal/repo/person_attribute.go`
- Modify: `internal/repo/person_test.go`（追加第二个测试函数）

- [ ] **Step 1: person_test.go 追加失败的测试**

在 `internal/repo/person_test.go` 末尾追加（Task 2 Step 1 注释里预留的函数）：

```go
func TestPersonListWithPendingAndRecentSessions(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	attrs := &PersonAttributeRepo{DB: db}

	p := &Person{DisplayName: "计数测试人物"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	sess := ids.New()
	// 一条 active、一条 pending：pending 计数应为 1
	if err := attrs.Create(ctx, &PersonAttribute{PersonID: p.ID, AttrKey: "city", ValueText: "上海", Status: "active", SessionID: &sess}); err != nil {
		t.Fatal(err)
	}
	if err := attrs.Create(ctx, &PersonAttribute{PersonID: p.ID, AttrKey: "occupation", ValueText: "医生", Status: "pending", SessionID: &sess}); err != nil {
		t.Fatal(err)
	}

	list, err := persons.ListWithPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var hit *PersonWithPending
	for i := range list {
		if list[i].ID == p.ID {
			hit = &list[i]
		}
	}
	if hit == nil || hit.PendingCount != 1 {
		t.Fatalf("ListWithPending 计数错误: %+v", hit)
	}

	sids, err := persons.RecentSessionIDs(ctx, p.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sids) != 1 || sids[0] != sess {
		t.Fatalf("RecentSessionIDs 错误: %v", sids)
	}
}
```

在 `internal/repo/person_attribute_test.go` 写主测试：

```go
package repo

import (
	"context"
	"testing"
)

func TestPersonAttributeQueries(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	attrs := &PersonAttributeRepo{DB: db}

	p := &Person{DisplayName: "属性测试人物"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	sess := ids.New()

	// 单值 key：两条不同值（模拟 active + 冲突 pending）
	a1 := &PersonAttribute{PersonID: p.ID, AttrKey: "city", ValueText: "北京", Status: "active", SessionID: &sess}
	if err := attrs.Create(ctx, a1); err != nil {
		t.Fatal(err)
	}
	a2 := &PersonAttribute{PersonID: p.ID, AttrKey: "city", ValueText: "上海", Status: "pending", SessionID: &sess, SupersedesID: &a1.ID}
	if err := attrs.Create(ctx, a2); err != nil {
		t.Fatal(err)
	}
	// 列表 key：两个元素
	a3 := &PersonAttribute{PersonID: p.ID, AttrKey: "hobbies", ValueText: "游泳", Status: "active", SessionID: &sess}
	if err := attrs.Create(ctx, a3); err != nil {
		t.Fatal(err)
	}

	// FindActiveByKey：单值当前值 = a1
	got, err := attrs.FindActiveByKey(ctx, p.ID, "city")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != a1.ID {
		t.Fatalf("FindActiveByKey 错误: %+v", got)
	}
	// FindActiveByKeyValue：列表同值命中 / 未命中
	g2, err := attrs.FindActiveByKeyValue(ctx, p.ID, "hobbies", "游泳")
	if err != nil {
		t.Fatal(err)
	}
	if g2 == nil || g2.ID != a3.ID {
		t.Fatalf("FindActiveByKeyValue 未命中: %+v", g2)
	}
	g3, err := attrs.FindActiveByKeyValue(ctx, p.ID, "hobbies", "篮球")
	if err != nil {
		t.Fatal(err)
	}
	if g3 != nil {
		t.Fatalf("FindActiveByKeyValue 不应命中: %+v", g3)
	}
	// FindByNaturalKey：同 session 同 key 同值（任意 status）命中
	g4, err := attrs.FindByNaturalKey(ctx, sess, p.ID, "city", "上海")
	if err != nil {
		t.Fatal(err)
	}
	if g4 == nil || g4.ID != a2.ID {
		t.Fatalf("FindByNaturalKey 未命中: %+v", g4)
	}
	// ListByPerson：3 行全在
	rows, err := attrs.ListByPerson(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("ListByPerson 应 3 行: %d", len(rows))
	}
	// ListPending：仅 a2
	pend, err := attrs.ListPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range pend {
		if r.ID == a2.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("ListPending 未包含 pending 行")
	}
	// SetStatus + BumpConfidence（封顶 0.99）
	if err := attrs.SetStatus(ctx, a1.ID, "superseded"); err != nil {
		t.Fatal(err)
	}
	if err := attrs.BumpConfidence(ctx, a1.ID, 0.05); err != nil {
		t.Fatal(err)
	}
	// Get 校验
	g5, err := attrs.Get(ctx, a1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g5.Status != "superseded" || g5.Confidence <= 0.8 {
		t.Fatalf("SetStatus/BumpConfidence 未生效: %+v", g5)
	}
}
```

注：`TestPersonAttributeQueries` 需要 `ids` 包导入——文件头 import 块补 `"zhiwei/internal/ids"`（上面已含）。

- [ ] **Step 2: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run 'TestPersonAttribute|TestPersonListWithPending' -v
```
Expected: FAIL，`undefined: PersonAttributeRepo`（编译错误）。

- [ ] **Step 3: 实现 person_attribute.go**

`internal/repo/person_attribute.go`：

```go
package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonAttribute 是画像的类型化属性行（spec §4.2）。
// 列表型属性（爱好/书单…）= 同 attr_key 多行 active，每元素独立
// 置信度/来源/溯源/确认；单值型 = 同 key 至多一行 active。
// 单值 vs 列表由 internal/profile 目录的 Cardinality 声明，表结构不区分。
type PersonAttribute struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	PersonID    ids.ID    `db:"person_id" json:"person_id"`
	AttrKey     string    `db:"attr_key" json:"attr_key"`
	ValueText   string    `db:"value_text" json:"value_text"`
	ValueType   string    `db:"value_type" json:"value_type"` // text|enum|bool|date|number
	Confidence  float64   `db:"confidence" json:"confidence"`
	EpistemicType string  `db:"epistemic_type" json:"epistemic_type"` // observed|inferred|predicted|suggested
	Source      string    `db:"source" json:"source"`                 // manual|llm
	Status      string    `db:"status" json:"status"`                 // active|pending|superseded|dismissed
	SessionID   *ids.ID   `db:"session_id" json:"session_id,omitempty"`
	MemoryID    *ids.ID   `db:"memory_id" json:"memory_id,omitempty"`
	TranscriptSegmentIDs ids.List `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	SupersedesID *ids.ID  `db:"supersedes_id" json:"supersedes_id,omitempty"` // 冲突 pending 指向当前 active 行
	Version     int       `db:"version" json:"version"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

type PersonAttributeRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。零值兜底。
// 注：Confidence==0 也兜底为 0.8——闸门已把低置信候选拦在门外，到这里的 0 视为漏填。
func (r *PersonAttributeRepo) CreateExt(ctx context.Context, ext ExecerContext, a *PersonAttribute) error {
	a.ID = ids.New()
	if a.UserID == 0 {
		a.UserID = 1
	}
	if a.ValueType == "" {
		a.ValueType = "text"
	}
	if a.Confidence == 0 {
		a.Confidence = 0.8
	}
	if a.EpistemicType == "" {
		a.EpistemicType = "observed"
	}
	if a.Source == "" {
		a.Source = "manual"
	}
	if a.Status == "" {
		a.Status = "active"
	}
	if a.Version == 0 {
		a.Version = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_attribute
  (id, user_id, person_id, attr_key, value_text, value_type, confidence, epistemic_type,
   source, status, session_id, memory_id, transcript_segment_ids, supersedes_id, version)
VALUES
  (:id, :user_id, :person_id, :attr_key, :value_text, :value_type, :confidence, :epistemic_type,
   :source, :status, :session_id, :memory_id, :transcript_segment_ids, :supersedes_id, :version)`, a)
	return err
}

func (r *PersonAttributeRepo) Create(ctx context.Context, a *PersonAttribute) error {
	return r.CreateExt(ctx, r.DB, a)
}

func (r *PersonAttributeRepo) Get(ctx context.Context, id ids.ID) (*PersonAttribute, error) {
	var a PersonAttribute
	err := r.DB.GetContext(ctx, &a, `SELECT * FROM person_attribute WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListByPerson 全状态列表（详情页要展示 active+pending，历史走 change_log），按 key、id 排序。
func (r *PersonAttributeRepo) ListByPerson(ctx context.Context, personID ids.ID) ([]PersonAttribute, error) {
	var list []PersonAttribute
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_attribute WHERE person_id = ? ORDER BY attr_key, id`, personID.Int64())
	return list, err
}

// FindActiveByKeyExt 单值型 key 的当前 active 行；无返回 nil。可在事务连接上执行。
func (r *PersonAttributeRepo) FindActiveByKeyExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, attrKey string) (*PersonAttribute, error) {
	var a PersonAttribute
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_attribute
WHERE person_id = ? AND attr_key = ? AND status = 'active'
ORDER BY id DESC LIMIT 1`, personID.Int64(), attrKey).StructScan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *PersonAttributeRepo) FindActiveByKey(ctx context.Context, personID ids.ID, attrKey string) (*PersonAttribute, error) {
	return r.FindActiveByKeyExt(ctx, r.DB, personID, attrKey)
}

// FindActiveByKeyValueExt 列表型 key 的同值 active 行（无则 nil）；闸门「同值→佐证」判定用。
func (r *PersonAttributeRepo) FindActiveByKeyValueExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, attrKey, value string) (*PersonAttribute, error) {
	var a PersonAttribute
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_attribute
WHERE person_id = ? AND attr_key = ? AND value_text = ? AND status = 'active'
ORDER BY id DESC LIMIT 1`, personID.Int64(), attrKey, value).StructScan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *PersonAttributeRepo) FindActiveByKeyValue(ctx context.Context, personID ids.ID, attrKey, value string) (*PersonAttribute, error) {
	return r.FindActiveByKeyValueExt(ctx, r.DB, personID, attrKey, value)
}

// FindByNaturalKeyExt 幂等去重查询：同 session 同 person 同 key 同原始值（任意 status）
// 已有行则返回该行——重跑同一 session 不重复建 pending / 不重复 bump（spec §6.3）。
func (r *PersonAttributeRepo) FindByNaturalKeyExt(ctx context.Context, ext QueryRowxContext, sessionID, personID ids.ID, attrKey, value string) (*PersonAttribute, error) {
	var a PersonAttribute
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_attribute
WHERE session_id = ? AND person_id = ? AND attr_key = ? AND value_text = ?
ORDER BY id LIMIT 1`, sessionID.Int64(), personID.Int64(), attrKey, value).StructScan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *PersonAttributeRepo) FindByNaturalKey(ctx context.Context, sessionID, personID ids.ID, attrKey, value string) (*PersonAttribute, error) {
	return r.FindByNaturalKeyExt(ctx, r.DB, sessionID, personID, attrKey, value)
}

func (r *PersonAttributeRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_attribute SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonAttributeRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// BumpConfidenceExt 佐证上调置信度，封顶 0.99（同 memory 的佐证模式）。
func (r *PersonAttributeRepo) BumpConfidenceExt(ctx context.Context, ext ExecerContext, id ids.ID, delta float64) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_attribute SET confidence = LEAST(confidence + ?, 0.99) WHERE id = ?`,
		delta, id.Int64())
	return err
}

func (r *PersonAttributeRepo) BumpConfidence(ctx context.Context, id ids.ID, delta float64) error {
	return r.BumpConfidenceExt(ctx, r.DB, id, delta)
}

// ListPending 全局确认队列（属性平面部分），按 id 升序（先产生的先确认）。
func (r *PersonAttributeRepo) ListPending(ctx context.Context, userID int64) ([]PersonAttribute, error) {
	var list []PersonAttribute
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_attribute WHERE user_id = ? AND status = 'pending' ORDER BY id`, userID)
	return list, err
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run 'TestPerson' -v
```
Expected: PASS（person + attribute 两组测试全过）。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/person_attribute.go internal/repo/person_attribute_test.go internal/repo/person_test.go
git commit -m "feat(profile): PersonAttributeRepo（单值/列表/自然键去重/pending 队列）"
```

---

### Task 4: repo 层——PersonRelationship + PersonRelationshipRepo

**Files:**
- Create: `internal/repo/person_relationship.go`
- Create: `internal/repo/person_relationship_test.go`

- [ ] **Step 1: 写失败的集成测试**

`internal/repo/person_relationship_test.go`：

```go
package repo

import (
	"context"
	"testing"
)

func TestPersonRelationshipQueries(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	rels := &PersonRelationshipRepo{DB: db}

	a := &Person{DisplayName: "关系测试-甲"}
	b := &Person{DisplayName: "关系测试-乙"}
	if err := persons.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := persons.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	sess := ids.New()

	// owner(甲) → 乙 的配偶关系（active）
	r1 := &PersonRelationship{PersonID: a.ID, RelatedPersonID: &b.ID, RelationType: "配偶", Status: "active", SessionID: &sess}
	if err := rels.Create(ctx, r1); err != nil {
		t.Fatal(err)
	}
	// 组织关系：无对端人物，只有 org_name
	r2 := &PersonRelationship{PersonID: a.ID, RelationType: "组织", OrgName: strp("校友会"), Status: "active", SessionID: &sess}
	if err := rels.Create(ctx, r2); err != nil {
		t.Fatal(err)
	}

	// FindActiveByTypeExt：按类型+对端命中
	got, err := rels.FindActiveByTypeExt(ctx, db, a.ID, "配偶", &b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != r1.ID {
		t.Fatalf("FindActiveByTypeExt 未命中: %+v", got)
	}
	// 组织类型、对端为 nil 的命中
	got2, err := rels.FindActiveByTypeExt(ctx, db, a.ID, "组织", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got2 == nil || got2.ID != r2.ID {
		t.Fatalf("FindActiveByTypeExt(组织,nil) 未命中: %+v", got2)
	}
	// 自然键去重：同 session 同三元组（任意 status）命中
	got3, err := rels.FindByNaturalKeyExt(ctx, db, sess, a.ID, "配偶", &b.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got3 == nil || got3.ID != r1.ID {
		t.Fatalf("FindByNaturalKeyExt 未命中: %+v", got3)
	}
	// ListByPerson / ListPending / SetStatus
	rows, err := rels.ListByPerson(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListByPerson 应 2 行: %d", len(rows))
	}
	r3 := &PersonRelationship{PersonID: a.ID, RelatedPersonID: &b.ID, RelationType: "同事", Status: "pending", SessionID: &sess}
	if err := rels.Create(ctx, r3); err != nil {
		t.Fatal(err)
	}
	pend, err := rels.ListPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range pend {
		if r.ID == r3.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("ListPending 未包含 pending 关系")
	}
	if err := rels.SetStatus(ctx, r3.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if g4, _ := rels.Get(ctx, r3.ID); g4.Status != "dismissed" {
		t.Fatalf("SetStatus 未生效: %+v", g4)
	}
}

// strp 字符串取址小工具（测试专用）。
func strp(s string) *string { return &s }
```

注：`strp` 若与包内已有工具重名，改名 `strPtrRel`。集成测试包 `internal/repo` 已有 `ids` import（person_test 用过）——新文件 import 块需含 `"zhiwei/internal/ids"`。

- [ ] **Step 2: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestPersonRelationship -v
```
Expected: FAIL，`undefined: PersonRelationshipRepo`（编译错误）。

- [ ] **Step 3: 实现 person_relationship.go**

`internal/repo/person_relationship.go`：

```go
package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonRelationship 是关系平面（spec §4.3）：person 与对端人物/组织的边。
// 「老婆做什么的」= 配偶关系边 + 对端 person 的 occupation 属性；
// 「几个孩子/几岁/生日」= N 条子女边 + 各子女 person 的 age/birthday 属性。
type PersonRelationship struct {
	ID                ids.ID    `db:"id" json:"id"`
	UserID            int64     `db:"user_id" json:"user_id"`
	PersonID          ids.ID    `db:"person_id" json:"person_id"` // 主体
	RelatedPersonID   *ids.ID   `db:"related_person_id" json:"related_person_id,omitempty"`
	RelationType      string    `db:"relation_type" json:"relation_type"` // 配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他
	Direction         *string   `db:"direction" json:"direction,omitempty"` // upstream|downstream|peer
	OrgName           *string   `db:"org_name" json:"org_name,omitempty"`
	Label             *string   `db:"label" json:"label,omitempty"` // 自由称呼（「大儿子」「张总」）
	Confidence        float64   `db:"confidence" json:"confidence"`
	EpistemicType     string    `db:"epistemic_type" json:"epistemic_type"`
	Source            string    `db:"source" json:"source"`     // manual|llm
	Status            string    `db:"status" json:"status"`     // active|pending|superseded|dismissed
	SessionID         *ids.ID   `db:"session_id" json:"session_id,omitempty"`
	MemoryID          *ids.ID   `db:"memory_id" json:"memory_id,omitempty"`
	TranscriptSegmentIDs ids.List `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	SupersedesID      *ids.ID   `db:"supersedes_id" json:"supersedes_id,omitempty"`
	Version           int       `db:"version" json:"version"`
	CreatedAt         time.Time `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time `db:"updated_at" json:"updated_at"`
}

type PersonRelationshipRepo struct{ DB *sqlx.DB }

func (r *PersonRelationshipRepo) CreateExt(ctx context.Context, ext ExecerContext, rel *PersonRelationship) error {
	rel.ID = ids.New()
	if rel.UserID == 0 {
		rel.UserID = 1
	}
	if rel.Confidence == 0 {
		rel.Confidence = 0.8
	}
	if rel.EpistemicType == "" {
		rel.EpistemicType = "observed"
	}
	if rel.Source == "" {
		rel.Source = "manual"
	}
	if rel.Status == "" {
		rel.Status = "active"
	}
	if rel.Version == 0 {
		rel.Version = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_relationship
  (id, user_id, person_id, related_person_id, relation_type, direction, org_name, label,
   confidence, epistemic_type, source, status, session_id, memory_id, transcript_segment_ids, supersedes_id, version)
VALUES
  (:id, :user_id, :person_id, :related_person_id, :relation_type, :direction, :org_name, :label,
   :confidence, :epistemic_type, :source, :status, :session_id, :memory_id, :transcript_segment_ids, :supersedes_id, :version)`, rel)
	return err
}

func (r *PersonRelationshipRepo) Create(ctx context.Context, rel *PersonRelationship) error {
	return r.CreateExt(ctx, r.DB, rel)
}

func (r *PersonRelationshipRepo) Get(ctx context.Context, id ids.ID) (*PersonRelationship, error) {
	var rel PersonRelationship
	err := r.DB.GetContext(ctx, &rel, `SELECT * FROM person_relationship WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rel, nil
}

// ListByPerson 主体维度的全状态关系列表（详情页展示 active+pending）。
func (r *PersonRelationshipRepo) ListByPerson(ctx context.Context, personID ids.ID) ([]PersonRelationship, error) {
	var list []PersonRelationship
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_relationship WHERE person_id = ? ORDER BY id`, personID.Int64())
	return list, err
}

// FindActiveByTypeExt 按（主体, 类型, 对端）找 active 行；对端与 label 用 NULL 安全比较
// （<=>）。组织关系（对端 nil）与人物关系都能命中。无返回 nil。
func (r *PersonRelationshipRepo) FindActiveByTypeExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, relationType string, relatedPersonID *ids.ID) (*PersonRelationship, error) {
	var rel PersonRelationship
	var rid any
	if relatedPersonID != nil {
		rid = relatedPersonID.Int64()
	}
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_relationship
WHERE person_id = ? AND relation_type = ? AND related_person_id <=> ? AND status = 'active'
ORDER BY id DESC LIMIT 1`, personID.Int64(), relationType, rid).StructScan(&rel)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rel, nil
}

// FindByNaturalKeyExt 幂等去重：同 session 同（主体,类型,对端）任意 status 的行。
// label 不进自然键（同一关系不同称呼视为同一条）。
func (r *PersonRelationshipRepo) FindByNaturalKeyExt(ctx context.Context, ext QueryRowxContext, sessionID, personID ids.ID, relationType string, relatedPersonID *ids.ID) (*PersonRelationship, error) {
	var rel PersonRelationship
	var rid any
	if relatedPersonID != nil {
		rid = relatedPersonID.Int64()
	}
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_relationship
WHERE session_id = ? AND person_id = ? AND relation_type = ? AND related_person_id <=> ?
ORDER BY id LIMIT 1`, sessionID.Int64(), personID.Int64(), relationType, rid).StructScan(&rel)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rel, nil
}

func (r *PersonRelationshipRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_relationship SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonRelationshipRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// ListPending 全局确认队列（关系平面部分）。
func (r *PersonRelationshipRepo) ListPending(ctx context.Context, userID int64) ([]PersonRelationship, error) {
	var list []PersonRelationship
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_relationship WHERE user_id = ? AND status = 'pending' ORDER BY id`, userID)
	return list, err
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run 'TestPerson' -v
```
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/person_relationship.go internal/repo/person_relationship_test.go
git commit -m "feat(profile): PersonRelationshipRepo（NULL 安全匹配/自然键去重/pending 队列）"
```

---

### Task 5: repo 层——PersonChangeLog + PersonChangeLogRepo

**Files:**
- Create: `internal/repo/person_change_log.go`
- Create: `internal/repo/person_change_log_test.go`

- [ ] **Step 1: 写失败的集成测试**

`internal/repo/person_change_log_test.go`：

```go
package repo

import (
	"context"
	"testing"
)

func TestPersonChangeLogAppendOnly(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	logs := &PersonChangeLogRepo{DB: db}

	p := &Person{DisplayName: "审计测试人物"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	attrID := ids.New()
	sess := ids.New()
	oldV := `"教师"`
	newV := `"医生"`

	// create + update 两条审计
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "attribute", EntityID: &attrID, AttrKey: strp("occupation"),
		ChangeType: "create", ChangedBy: "llm", NewValue: strp(oldV), SessionID: &sess,
		Confidence: fp(0.9),
	}); err != nil {
		t.Fatal(err)
	}
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "attribute", EntityID: &attrID, AttrKey: strp("occupation"),
		ChangeType: "update", ChangedBy: "user", OldValue: strp(oldV), NewValue: strp(newV),
	}); err != nil {
		t.Fatal(err)
	}

	// ListByPerson：2 条，按时间正序
	rows, err := logs.ListByPerson(ctx, p.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应 2 条审计: %d", len(rows))
	}
	if rows[0].ChangeType != "create" || rows[0].ChangedBy != "llm" {
		t.Fatalf("第一条审计错误: %+v", rows[0])
	}
	// entity_kind 过滤
	only, err := logs.ListByPerson(ctx, p.ID, "attribute", "occupation")
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 2 {
		t.Fatalf("attr_key 过滤应 2 条: %d", len(only))
	}
	none, err := logs.ListByPerson(ctx, p.ID, "attribute", "city")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("city 过滤应 0 条: %d", len(none))
	}
}

// fp float64 取址小工具（测试专用；与 strp 同思路）。
func fp(f float64) *float64 { return &f }
```

- [ ] **Step 2: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestPersonChangeLog -v
```
Expected: FAIL，`undefined: PersonChangeLogRepo`（编译错误）。

- [ ] **Step 3: 实现 person_change_log.go**

`internal/repo/person_change_log.go`：

```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonChangeLog 是跨平面统一审计（spec §4.8）：谁（changed_by）、何时（created_at）、
// 从什么（old_value）改成什么（new_value）、关联哪条 timeline/事件（session_id/memory_id/
// transcript_segment_ids）。只追加，永不 update/delete——repo 不提供修改方法。
type PersonChangeLog struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	PersonID    ids.ID    `db:"person_id" json:"person_id"`
	EntityKind  string    `db:"entity_kind" json:"entity_kind"` // person|attribute|relationship（P2+ 扩 event/metric/…）
	EntityID    *ids.ID   `db:"entity_id" json:"entity_id,omitempty"`
	AttrKey     *string   `db:"attr_key" json:"attr_key,omitempty"`
	ChangeType  string    `db:"change_type" json:"change_type"` // create|update|confirm|dismiss|supersede|delete|reaffirm
	ChangedBy   string    `db:"changed_by" json:"changed_by"`   // user|llm
	OldValue    *string   `db:"old_value" json:"old_value,omitempty"` // JSON 快照文本（如 "医生"）
	NewValue    *string   `db:"new_value" json:"new_value,omitempty"`
	Confidence  *float64  `db:"confidence" json:"confidence,omitempty"`
	EpistemicType *string `db:"epistemic_type" json:"epistemic_type,omitempty"`
	SessionID   *ids.ID   `db:"session_id" json:"session_id,omitempty"`
	MemoryID    *ids.ID   `db:"memory_id" json:"memory_id,omitempty"`
	TranscriptSegmentIDs ids.List `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	Note        *string   `db:"note" json:"note,omitempty"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

type PersonChangeLogRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上追加一条审计（只追加语义，无 Update/Delete 方法）。
func (r *PersonChangeLogRepo) CreateExt(ctx context.Context, ext ExecerContext, l *PersonChangeLog) error {
	l.ID = ids.New()
	if l.UserID == 0 {
		l.UserID = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_change_log
  (id, user_id, person_id, entity_kind, entity_id, attr_key, change_type, changed_by,
   old_value, new_value, confidence, epistemic_type, session_id, memory_id, transcript_segment_ids, note)
VALUES
  (:id, :user_id, :person_id, :entity_kind, :entity_id, :attr_key, :change_type, :changed_by,
   :old_value, :new_value, :confidence, :epistemic_type, :session_id, :memory_id, :transcript_segment_ids, :note)`, l)
	return err
}

func (r *PersonChangeLogRepo) Create(ctx context.Context, l *PersonChangeLog) error {
	return r.CreateExt(ctx, r.DB, l)
}

// ListByPerson 人物的全平面审计历史，时间正序；entityKind/attrKey 为空不过滤。
func (r *PersonChangeLogRepo) ListByPerson(ctx context.Context, personID ids.ID, entityKind, attrKey string) ([]PersonChangeLog, error) {
	q := `SELECT * FROM person_change_log WHERE person_id = ?`
	args := []any{personID.Int64()}
	if entityKind != "" {
		q += ` AND entity_kind = ?`
		args = append(args, entityKind)
	}
	if attrKey != "" {
		q += ` AND attr_key = ?`
		args = append(args, attrKey)
	}
	q += ` ORDER BY id`
	var list []PersonChangeLog
	err := r.DB.SelectContext(ctx, &list, q, args...)
	return list, err
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run 'TestPerson' -v
```
Expected: PASS。

- [ ] **Step 5: 全 repo 包回归 + Commit**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -v 2>&1 | tail -5
```
Expected: 既有 repo 测试 + 新增测试全部 PASS（无回归）。

```bash
git add internal/repo/person_change_log.go internal/repo/person_change_log_test.go
git commit -m "feat(profile): PersonChangeLogRepo（跨平面只追加审计）"
```

---

### Task 6: profile 领域包——属性目录 catalog.go

**Files:**
- Create: `internal/profile/catalog.go`
- Create: `internal/profile/catalog_test.go`

- [ ] **Step 1: 写失败的单元测试**

`internal/profile/catalog_test.go`：

```go
package profile

import "testing"

func TestCatalogDefs(t *testing.T) {
	// 已知 key：完整定义
	d := Def("occupation")
	if d.Label != "职业" || d.Group != "工作" || d.Cardinality != CardinalitySingle || d.ValueType != "text" {
		t.Fatalf("occupation 定义错误: %+v", d)
	}
	// 枚举型
	g := Def("gender")
	if g.ValueType != "enum" || len(g.EnumOptions) != 3 {
		t.Fatalf("gender 定义错误: %+v", g)
	}
	// 列表型
	h := Def("hobbies")
	if !IsList("hobbies") || h.Group != "兴趣" {
		t.Fatalf("hobbies 应为列表型: %+v", h)
	}
	// 目录外 key：默认「其他」组、text、single
	u := Def("custom_key_xyz")
	if u.Group != "其他" || u.ValueType != "text" || u.Cardinality != CardinalitySingle {
		t.Fatalf("未知 key 默认定义错误: %+v", u)
	}
	// 分组顺序：GroupOrder 覆盖所有目录里出现过的分组
	seen := map[string]bool{}
	for _, d := range All() {
		seen[d.Group] = true
	}
	for _, g := range GroupOrder {
		if g != "其他" {
			delete(seen, g)
		}
	}
	if len(seen) > 1 { // 只允许剩「其他」（目录内不显式用它）
		t.Fatalf("有分组未列入 GroupOrder: %v", seen)
	}
	// key 不重复
	keys := map[string]bool{}
	for _, d := range All() {
		if keys[d.Key] {
			t.Fatalf("目录 key 重复: %s", d.Key)
		}
		keys[d.Key] = true
	}
	// 需求字段抽查（用户原始清单的关键字段必须都在）
	for _, k := range []string{"aliases", "birthday", "gender", "zodiac", "mbti", "education",
		"school", "city", "address", "phone", "occupation", "industry", "office_location",
		"work_start_time", "work_end_time", "commute_mode", "often_travel", "current_projects",
		"meal_time", "cuisine", "eats_spicy", "eats_numbing", "smokes", "drinks",
		"wears_makeup", "perfume", "hobbies", "skills", "reading_now", "books_read",
		"movies_watched", "music_listened", "games_played", "fav_celebrities", "fav_anime",
		"fav_movie_genres", "catchphrases", "invests_stocks", "cities_visited", "places_traveled",
		"has_car", "car_brand", "phone_brand", "recent_concerns", "attention_topics",
		"personality", "chronic_diseases"} {
		if _, ok := catalogMap[k]; !ok {
			t.Errorf("需求字段缺失于目录: %s", k)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/profile/ -run TestCatalog -v
```
Expected: FAIL——目录不存在（包/符号未定义，编译错误）。

- [ ] **Step 3: 实现 catalog.go**

`internal/profile/catalog.go`：

```go
// Package profile 实现用户画像（人物系统）的领域逻辑：属性目录、
// LLM 抽取、置信度闸门、人物归属解析与落库编排。
// 设计规格：docs/superpowers/specs/2026-08-24-person-profile-system-design.md。
package profile

// AttrDef 是属性目录里一个已知字段的定义。目录外的 key 仍可用
// （Def 返回默认定义，归「其他」组），保证「所有个人信息」可自由扩展。
type AttrDef struct {
	Key         string
	Label       string   // 中文名（表单/展示用）
	Group       string   // 分组名（GroupOrder 之一）
	ValueType   string   // text|enum|bool|date|number（值的类型；列表与否看 Cardinality）
	EnumOptions []string // ValueType=enum 时的取值集
	Cardinality string   // single=同 key 至多一行 active | list=同 key 多行 active（每元素一行）
}

// GroupOrder 属性分组展示顺序（人物详情页分区用）。
var GroupOrder = []string{"基本", "工作", "生活习惯", "兴趣", "出行物品", "关注性格", "健康", "其他"}

// Cardinality 取值。
const (
	CardinalitySingle = "single"
	CardinalityList   = "list"
)

func def(key, label, group, vt, card string, enum ...string) AttrDef {
	return AttrDef{Key: key, Label: label, Group: group, ValueType: vt, EnumOptions: enum, Cardinality: card}
}

// catalog 是已知属性全集（spec §4.9 的字段映射）。
var catalog = []AttrDef{
	// ---- 基本 ----
	def("aliases", "别名", "基本", "text", CardinalityList),
	def("birthday", "生日", "基本", "date", CardinalitySingle),
	def("gender", "性别", "基本", "enum", CardinalitySingle, "男", "女", "其他"),
	def("zodiac", "星座", "基本", "enum", CardinalitySingle,
		"白羊座", "金牛座", "双子座", "巨蟹座", "狮子座", "处女座",
		"天秤座", "天蝎座", "射手座", "摩羯座", "水瓶座", "双鱼座"),
	def("mbti", "MBTI", "基本", "enum", CardinalitySingle,
		"INTJ", "INTP", "ENTJ", "ENTP", "INFJ", "INFP", "ENFJ", "ENFP",
		"ISTJ", "ISFJ", "ESTJ", "ESFJ", "ISTP", "ISFP", "ESTP", "ESFP"),
	def("education", "学历", "基本", "enum", CardinalitySingle, "高中及以下", "大专", "本科", "硕士", "博士"),
	def("school", "学校", "基本", "text", CardinalityList),
	def("city", "城市", "基本", "text", CardinalitySingle),
	def("address", "住址", "基本", "text", CardinalitySingle),
	def("phone", "手机号", "基本", "text", CardinalitySingle),

	// ---- 工作 ----
	def("occupation", "职业", "工作", "text", CardinalitySingle),
	def("industry", "所属行业", "工作", "text", CardinalitySingle),
	def("office_location", "办公地点", "工作", "text", CardinalitySingle),
	def("work_start_time", "上班时间", "工作", "text", CardinalitySingle),
	def("work_end_time", "下班时间", "工作", "text", CardinalitySingle),
	def("commute_mode", "通勤方式", "工作", "enum", CardinalitySingle,
		"步行", "自行车", "电动车", "地铁", "公交", "开车", "打车", "班车", "火车", "高铁", "飞机"),
	def("often_travel", "是否经常出差", "工作", "bool", CardinalitySingle),
	def("current_projects", "正在进行的项目", "工作", "text", CardinalityList),

	// ---- 生活习惯 ----
	def("meal_time", "吃饭时间", "生活习惯", "text", CardinalitySingle),
	def("cuisine", "喜欢的菜系", "生活习惯", "enum", CardinalityList,
		"川菜", "粤菜", "湘菜", "鲁菜", "苏菜", "浙菜", "闽菜", "徽菜",
		"火锅", "烧烤", "西餐", "日料", "韩餐", "家常菜"),
	def("eats_spicy", "是否吃辣", "生活习惯", "bool", CardinalitySingle),
	def("eats_numbing", "是否吃麻", "生活习惯", "bool", CardinalitySingle),
	def("smokes", "是否吸烟", "生活习惯", "bool", CardinalitySingle),
	def("drinks", "是否喝酒", "生活习惯", "bool", CardinalitySingle),
	def("wears_makeup", "是否化妆", "生活习惯", "bool", CardinalitySingle),
	def("perfume", "香水", "生活习惯", "text", CardinalitySingle),

	// ---- 兴趣 ----
	def("hobbies", "爱好", "兴趣", "text", CardinalityList),
	def("skills", "学的技能", "兴趣", "text", CardinalityList),
	def("reading_now", "正在看的书", "兴趣", "text", CardinalityList),
	def("books_read", "看过的书", "兴趣", "text", CardinalityList),
	def("movies_watched", "看过的影视", "兴趣", "text", CardinalityList),
	def("music_listened", "听过的音乐", "兴趣", "text", CardinalityList),
	def("games_played", "玩过的游戏", "兴趣", "text", CardinalityList),
	def("fav_celebrities", "喜欢的明星", "兴趣", "text", CardinalityList),
	def("fav_anime", "喜欢的动漫", "兴趣", "text", CardinalityList),
	def("fav_movie_genres", "喜欢的电影类型", "兴趣", "text", CardinalityList),
	def("catchphrases", "口头禅", "兴趣", "text", CardinalityList),
	def("invests_stocks", "是否炒股", "兴趣", "bool", CardinalitySingle),

	// ---- 出行物品 ----
	def("cities_visited", "去过的城市", "出行物品", "text", CardinalityList),
	def("places_traveled", "旅游过的地方", "出行物品", "text", CardinalityList),
	def("has_car", "是否有车", "出行物品", "bool", CardinalitySingle),
	def("car_brand", "车品牌", "出行物品", "text", CardinalitySingle),
	def("phone_brand", "手机品牌", "出行物品", "text", CardinalitySingle),

	// ---- 关注性格 ----
	def("recent_concerns", "最近关心的事情", "关注性格", "text", CardinalityList),
	def("attention_topics", "关注领域", "关注性格", "enum", CardinalityList,
		"政治", "军事", "体育", "三农", "科技", "财经", "娱乐", "教育", "健康"),
	def("personality", "性格", "关注性格", "text", CardinalitySingle),

	// ---- 健康（P3 深化，先占位属性） ----
	def("chronic_diseases", "慢性病", "健康", "text", CardinalityList),
}

var catalogMap = func() map[string]AttrDef {
	m := make(map[string]AttrDef, len(catalog))
	for _, d := range catalog {
		m[d.Key] = d
	}
	return m
}()

// Def 返回 key 的目录定义；目录外 key 返回「其他」组的默认定义（text/single），可自由扩展。
func Def(key string) AttrDef {
	if d, ok := catalogMap[key]; ok {
		return d
	}
	return AttrDef{Key: key, Label: key, Group: "其他", ValueType: "text", Cardinality: CardinalitySingle}
}

// IsList 判断 key 是否列表型属性。
func IsList(key string) bool { return Def(key).Cardinality == CardinalityList }

// All 返回全部目录定义（目录顺序即分组顺序）。
func All() []AttrDef { return catalog }
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/profile/ -run TestCatalog -v
```
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/profile/catalog.go internal/profile/catalog_test.go
git commit -m "feat(profile): 属性目录 catalog（47 个已知字段+目录外自由扩展）"
```

---

### Task 7: profile——Fact 类型与 LLM 输出解析 fact.go

**Files:**
- Create: `internal/profile/fact.go`
- Create: `internal/profile/fact_test.go`

- [ ] **Step 1: 写失败的单元测试**

`internal/profile/fact_test.go`：

```go
package profile

import "testing"

func TestParseFacts(t *testing.T) {
	// 正常输出（带 markdown 围栏，容错剥掉）
	raw := "```json\n{\"facts\":[\n" +
		"{\"plane\":\"attribute\",\"subject\":{\"kind\":\"self\"},\"attr_key\":\"occupation\"," +
		"\"value\":\"工程师\",\"confidence\":0.9,\"epistemic_type\":\"observed\",\"block_index\":1},\n" +
		"{\"plane\":\"attribute\",\"subject\":{\"kind\":\"mentioned\",\"name\":\"Alice\"}," +
		"\"attr_key\":\"occupation\",\"value\":\"医生\",\"confidence\":0.6,\"epistemic_type\":\"observed\",\"block_index\":2},\n" +
		"{\"plane\":\"relationship\",\"subject\":{\"kind\":\"self\"}," +
		"\"related\":{\"kind\":\"mentioned\",\"name\":\"Alice\"},\"relation_type\":\"配偶\"," +
		"\"label\":\"老婆\",\"confidence\":0.85,\"block_index\":2}\n]}\n```"
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Fatalf("应解析 3 条: %d", len(facts))
	}
	f0 := facts[0]
	if f0.Plane != "attribute" || f0.Subject.Kind != "self" || f0.AttrKey != "occupation" ||
		f0.Value != "工程师" || f0.Confidence != 0.9 || f0.BlockIndex != 1 {
		t.Fatalf("fact0 错误: %+v", f0)
	}
	f2 := facts[2]
	if f2.Plane != "relationship" || f2.RelationType != "配偶" || f2.Related.Name != "Alice" || f2.Label != "老婆" {
		t.Fatalf("fact2 错误: %+v", f2)
	}
}

func TestParseFactsDropsInvalid(t *testing.T) {
	raw := `{"facts":[
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"","value":"缺key","confidence":0.9},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"city","value":"","confidence":0.9},
		{"plane":"bogus","subject":{"kind":"self"},"attr_key":"city","value":"北京","confidence":0.9},
		{"plane":"relationship","subject":{"kind":"self"},"related":{"kind":"mentioned","name":"X"},"relation_type":"师徒","confidence":0.9},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"city","value":"北京","confidence":1.7},
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"gender","value":"男","confidence":0.9,"epistemic_type":"神谕"}
	]}`
	facts, err := ParseFacts(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 前 5 条非法被丢弃（空 key/空值/非法 plane/非法关系类型/非法 epistemic）；置信度越界被钳制保留
	if len(facts) != 1 {
		t.Fatalf("应保留 1 条: %+v", facts)
	}
	if facts[0].Confidence != 1.0 {
		t.Fatalf("confidence 未钳制: %v", facts[0].Confidence)
	}
}

func TestParseFactsEmpty(t *testing.T) {
	facts, err := ParseFacts(`{"facts":[]}`)
	if err != nil || len(facts) != 0 {
		t.Fatalf("空 facts 应成功: %v %v", facts, err)
	}
	if _, err := ParseFacts(`完全不是 JSON`); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/profile/ -run TestParseFacts -v
```
Expected: FAIL，`undefined: ParseFacts`（编译错误）。

- [ ] **Step 3: 实现 fact.go**

`internal/profile/fact.go`：

```go
package profile

import (
	"encoding/json"
	"fmt"
	"strings"

	"zhiwei/internal/ids"
)

// Subject 是 LLM 对「这条信息属于谁」的指代（Go 侧做确定性解析，见 service.go resolveSubject）：
//   self=第一人称「我」 | speaker:Name=说话人 | mentioned:Name=对话里提到的具名他人
//   relation:TYPE=关系指代（如「我老婆」→ TYPE=配偶）
type Subject struct {
	Kind     string `json:"kind"`     // self|speaker|mentioned|relation
	Name     string `json:"name"`     // speaker/mentioned 时的名字
	Relation string `json:"relation"` // kind=relation 时的关系类型（如 配偶）
}

// Fact 是 LLM 输出的一条画像事实（闸门前后通用载体）。P1 两个平面：
// attribute（属性）/ relationship（关系）。P2+ 扩 event/metric/cycle/activity。
type Fact struct {
	Plane   string  // attribute|relationship
	Subject Subject // 信息归属的人物指代

	// ---- attribute 平面 ----
	AttrKey string // 目录 key（落库以目录校验，未知 key 仍可用归「其他」）
	Value   string
	ValueType string // LLM 给的类型提示（仅供参考，落库以目录为准）

	// ---- relationship 平面 ----
	RelationType string  // 关系类型枚举
	Related      Subject // 关系对端人物指代
	Direction    string  // upstream|downstream|peer（上下游）
	OrgName      string  // 组织名（组织关系）
	Label        string  // 自由称呼（「大儿子」）

	// ---- 通用 ----
	Confidence    float64
	EpistemicType string // observed|inferred|predicted|suggested
	BlockIndex    int    // 来源对话块序号（1-based，0=未知）

	// 编排层填充（LLM 不产出）
	SegmentIDs []ids.ID // provenance：来源块的 segment id
}

var validPlanes = map[string]bool{"attribute": true, "relationship": true}

var validEpistemic = map[string]bool{
	"observed": true, "inferred": true, "predicted": true, "suggested": true,
}

// ValidRelations 关系类型枚举（与迁移注释一致）。
var ValidRelations = map[string]bool{
	"配偶": true, "子女": true, "父母": true, "兄弟姐妹": true, "亲戚": true,
	"朋友": true, "同事": true, "领导": true, "下属": true,
	"客户": true, "供应商": true, "合作方": true, "组织": true, "其他": true,
}

var validDirections = map[string]bool{"upstream": true, "downstream": true, "peer": true, "": true}

type rawSubject struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Relation string `json:"relation"`
}

type rawFact struct {
	Plane         string     `json:"plane"`
	Subject       rawSubject `json:"subject"`
	AttrKey       string     `json:"attr_key"`
	Value         string     `json:"value"`
	ValueType     string     `json:"value_type"`
	RelationType  string     `json:"relation_type"`
	Related       rawSubject `json:"related"`
	Direction     string     `json:"direction"`
	OrgName       string     `json:"org_name"`
	Label         string     `json:"label"`
	Confidence    float64    `json:"confidence"`
	EpistemicType string     `json:"epistemic_type"`
	BlockIndex    int        `json:"block_index"`
}

// ParseFacts 解析 LLM 输出。容错风格同 memory.ParseCandidates：截取首个 { 到末个 }，
// 天然剥掉前后废话与 markdown 围栏；彻底非法 JSON 返回 error（stage 走重试）。
// 条目级问题（非法 plane/枚举/空字段）直接丢弃该条——宁少勿错，不整体失败。
// epistemic_type 缺省视为 observed（画像事实多为对话直陈）。
func ParseFacts(raw string) ([]Fact, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out struct {
		Facts []rawFact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("画像抽取结果解析失败: %w", err)
	}
	facts := make([]Fact, 0, len(out.Facts))
	for _, rf := range out.Facts {
		f := Fact{
			Plane:         rf.Plane,
			Subject:       Subject(rf.Subject),
			AttrKey:       strings.TrimSpace(rf.AttrKey),
			Value:         strings.TrimSpace(rf.Value),
			ValueType:     rf.ValueType,
			RelationType:  strings.TrimSpace(rf.RelationType),
			Related:       Subject(rf.Related),
			Direction:     strings.TrimSpace(rf.Direction),
			OrgName:       strings.TrimSpace(rf.OrgName),
			Label:         strings.TrimSpace(rf.Label),
			Confidence:    clamp01(rf.Confidence),
			EpistemicType: rf.EpistemicType,
			BlockIndex:    rf.BlockIndex,
		}
		if !validPlanes[f.Plane] || !validDirections[f.Direction] {
			continue
		}
		if f.EpistemicType == "" {
			f.EpistemicType = "observed"
		}
		if !validEpistemic[f.EpistemicType] {
			continue
		}
		switch f.Plane {
		case "attribute":
			if f.AttrKey == "" || f.Value == "" {
				continue
			}
		case "relationship":
			if !ValidRelations[f.RelationType] || f.Related.Kind == "" {
				continue
			}
		}
		facts = append(facts, f)
	}
	return facts, nil
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

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/profile/ -run TestParseFacts -v
```
Expected: PASS（3 个测试全过）。

- [ ] **Step 5: Commit**

```bash
git add internal/profile/fact.go internal/profile/fact_test.go
git commit -m "feat(profile): Fact 类型 + ParseFacts（条目级容错解析）"
```

---

### Task 8: profile——置信度闸门 gate.go

**Files:**
- Create: `internal/profile/gate.go`
- Create: `internal/profile/gate_test.go`

- [ ] **Step 1: 写失败的单元测试**

`internal/profile/gate_test.go`：

```go
package profile

import (
	"testing"

	"zhiwei/internal/repo"
)

func TestDecideAttribute(t *testing.T) {
	cfg := GateConfig{AutoConf: 0.75}
	base := Fact{Plane: "attribute", AttrKey: "occupation", Value: "工程师",
		Confidence: 0.9, EpistemicType: "observed"}

	// 无现值、高置信 observed → 直接 active
	if d := DecideAttribute(base, nil, false, false, cfg); d != DecisionCreateActive {
		t.Fatalf("高置信无现值应为 create_active: %v", d)
	}
	// 无现值、低置信 → pending
	low := base
	low.Confidence = 0.6
	if d := DecideAttribute(low, nil, false, false, cfg); d != DecisionCreatePending {
		t.Fatalf("低置信应为 create_pending: %v", d)
	}
	// 高置信但 suggested（推测）→ pending（只有 observed/inferred 可自动写入）
	sugg := base
	sugg.EpistemicType = "suggested"
	if d := DecideAttribute(sugg, nil, false, false, cfg); d != DecisionCreatePending {
		t.Fatalf("suggested 应为 create_pending: %v", d)
	}
	// 同 session 同值已处理 → skip（幂等）
	if d := DecideAttribute(base, nil, false, true, cfg); d != DecisionSkip {
		t.Fatalf("dedupHit 应 skip: %v", d)
	}
	// 有现值同值 → reaffirm（佐证）
	same := &repo.PersonAttribute{ValueText: "工程师"}
	if d := DecideAttribute(base, same, false, false, cfg); d != DecisionReaffirm {
		t.Fatalf("同值应 reaffirm: %v", d)
	}
	// 有现值不同值（单值型）→ 冲突 pending，绝不静默覆盖
	diff := &repo.PersonAttribute{ValueText: "教师"}
	if d := DecideAttribute(base, diff, false, false, cfg); d != DecisionConflictPending {
		t.Fatalf("单值冲突应 conflict_pending: %v", d)
	}
	// 列表型：existing 只会是同值行，无值 → 按置信度 create
	lowList := Fact{Plane: "attribute", AttrKey: "hobbies", Value: "游泳",
		Confidence: 0.6, EpistemicType: "observed"}
	if d := DecideAttribute(lowList, nil, true, false, cfg); d != DecisionCreatePending {
		t.Fatalf("列表低置信应 create_pending: %v", d)
	}
	highList := lowList
	highList.Confidence = 0.9
	if d := DecideAttribute(highList, nil, true, false, cfg); d != DecisionCreateActive {
		t.Fatalf("列表高置信应 create_active: %v", d)
	}
	// 阈值兜底：AutoConf<=0 时用默认 0.75
	if d := DecideAttribute(base, nil, false, false, GateConfig{}); d != DecisionCreateActive {
		t.Fatalf("默认阈值 0.75，0.9 应 active: %v", d)
	}
	// 值归一化比较：现值「 工程师 」与新值「工程师」视为同值
	spaced := &repo.PersonAttribute{ValueText: " 工程师 "}
	if d := DecideAttribute(base, spaced, false, false, cfg); d != DecisionReaffirm {
		t.Fatalf("归一化后同值应 reaffirm: %v", d)
	}
}

func TestDecideRelationship(t *testing.T) {
	cfg := GateConfig{AutoConf: 0.75}
	f := Fact{Plane: "relationship", RelationType: "配偶",
		Related: Subject{Kind: "mentioned", Name: "Alice"},
		Confidence: 0.9, EpistemicType: "observed"}

	if d := DecideRelationship(f, nil, false, cfg); d != DecisionCreateActive {
		t.Fatalf("高置信新关系应 create_active: %v", d)
	}
	if d := DecideRelationship(f, nil, true, cfg); d != DecisionSkip {
		t.Fatalf("dedupHit 应 skip: %v", d)
	}
	if d := DecideRelationship(f, &repo.PersonRelationship{}, false, cfg); d != DecisionReaffirm {
		t.Fatalf("同键关系应 reaffirm: %v", d)
	}
	low := f
	low.Confidence = 0.6
	if d := DecideRelationship(low, nil, false, cfg); d != DecisionCreatePending {
		t.Fatalf("低置信应 create_pending: %v", d)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/profile/ -run TestDecide -v
```
Expected: FAIL，`undefined: GateConfig`（编译错误）。

- [ ] **Step 3: 实现 gate.go**

`internal/profile/gate.go`：

```go
package profile

import (
	"zhiwei/internal/repo"
)

// GateConfig 是画像闸门阈值（来自 ZW_PROFILE_AUTO_CONFIDENCE 配置）。
type GateConfig struct {
	AutoConf float64 // 自动写入 active 的置信阈值；<=0 用默认 0.75
}

func (g GateConfig) autoConf() float64 {
	if g.AutoConf <= 0 {
		return 0.75
	}
	return g.AutoConf
}

// Decision 是一条事实的落库决策（spec §5 闸门规则）。
type Decision string

const (
	DecisionCreateActive    Decision = "create_active"    // 无现值且高置信 observed/inferred → 直接 active
	DecisionCreatePending   Decision = "create_pending"   // 无现值低置信 → pending 待人工确认
	DecisionReaffirm        Decision = "reaffirm"          // 同值已存在 → 佐证：上调置信度 +0.05 封顶 0.99
	DecisionConflictPending Decision = "conflict_pending" // 单值冲突 → pending（supersedes 指向现值），绝不静默覆盖
	DecisionSkip            Decision = "skip"              // 自然键已处理过（同 session 同值）→ 幂等跳过
)

// DecideAttribute 属性闸门（spec §5.2-5.4）。
// existing 的语义按 cardinality 区分：
//   单值型 = 该 key 当前 active 行（值可能不同 → 冲突路径）；
//   列表型 = 该 key 同值 active 行（无则 nil；列表元素的 existing 必同值 → 只有 reaffirm）。
// dedupHit = 自然键 (session,person,key,value) 已存在任意 status 行。
func DecideAttribute(f Fact, existing *repo.PersonAttribute, isList bool, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit {
		return DecisionSkip
	}
	if existing != nil {
		if repo.NormalizeTitle(existing.ValueText) == repo.NormalizeTitle(f.Value) {
			return DecisionReaffirm
		}
		return DecisionConflictPending // 只有单值型会走到这里（列表型 existing 即同值行）
	}
	if f.Confidence >= cfg.autoConf() && (f.EpistemicType == "observed" || f.EpistemicType == "inferred") {
		return DecisionCreateActive
	}
	return DecisionCreatePending
}

// DecideRelationship 关系闸门：关系天然多条（多个子女/朋友/同事并存），无冲突路径——
// 同键（主体,类型,对端）已 active → 佐证；新键按置信度 create。
// 注：同类型不同对端（如两位「配偶」）在 P1 会并存两行 active，用户可在队列里放弃其一；
// 更精细的唯一性约束（配偶唯一）留给后续按 relation_type 配置。
func DecideRelationship(f Fact, existing *repo.PersonRelationship, dedupHit bool, cfg GateConfig) Decision {
	if dedupHit {
		return DecisionSkip
	}
	if existing != nil {
		return DecisionReaffirm
	}
	if f.Confidence >= cfg.autoConf() && (f.EpistemicType == "observed" || f.EpistemicType == "inferred") {
		return DecisionCreateActive
	}
	return DecisionCreatePending
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/profile/ -run TestDecide -v
```
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/profile/gate.go internal/profile/gate_test.go
git commit -m "feat(profile): 置信度闸门 DecideAttribute/DecideRelationship"
```

---

### Task 9: profile——LLM 抽取器 extractor.go

**Files:**
- Create: `internal/profile/extractor.go`
- Create: `internal/profile/extractor_test.go`

- [ ] **Step 1: 写失败的单元测试（fake LLM）**

`internal/profile/extractor_test.go`：

```go
package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/provider"
)

// fakeLLM 按序返回预置响应（每次 Chat 弹出一条）。
type fakeLLM struct{ resps []string }

func (f *fakeLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	if len(f.resps) == 0 {
		return provider.ChatResponse{}, nil
	}
	r := f.resps[0]
	f.resps = f.resps[1:]
	return provider.ChatResponse{Content: r, TotalTokens: 42}, nil
}

var _ provider.LLMProvider = (*fakeLLM)(nil)

func TestExtractorExtract(t *testing.T) {
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "我在互联网公司做后端开发", StartMS: 0, EndMS: 3000,
			SegmentIDs: []ids.ID{101, 102}},
		{SpeakerLabel: "我", Text: "我老婆 Alice 是医生", StartMS: 4000, EndMS: 7000,
			SegmentIDs: []ids.ID{103}},
	}
	resp := `{"facts":[
		{"plane":"attribute","subject":{"kind":"self"},"attr_key":"occupation","value":"后端开发工程师",
		 "confidence":0.9,"epistemic_type":"observed","block_index":1},
		{"plane":"relationship","subject":{"kind":"self"},"related":{"kind":"mentioned","name":"Alice"},
		 "relation_type":"配偶","label":"老婆","confidence":0.85,"epistemic_type":"observed","block_index":2}
	]}`
	ex := &Extractor{LLM: &fakeLLM{resps: []string{resp}}, Model: "test-model", Prompt: "sys", Window: 10}
	facts, err := ex.Extract(context.Background(), blocks, []PersonRef{{ID: 1, Name: "我", IsOwner: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("应 2 条: %+v", facts)
	}
	// SegmentIDs 由 block_index 回填（block 1 → segs 101,102）
	if len(facts[0].SegmentIDs) != 2 || facts[0].SegmentIDs[0] != 101 {
		t.Fatalf("fact0 溯源错误: %v", facts[0].SegmentIDs)
	}
	if len(facts[1].SegmentIDs) != 1 || facts[1].SegmentIDs[0] != 103 {
		t.Fatalf("fact1 溯源错误: %v", facts[1].SegmentIDs)
	}
	if ex.Stats().Windows != 1 || ex.Stats().Tokens != 42 {
		t.Fatalf("stats 错误: %+v", ex.Stats())
	}
}

func TestExtractorDedupAcrossWindows(t *testing.T) {
	// 两个窗口（Window=1 强制切两窗），两边输出同一条事实但置信度不同 → 保留高者
	blocks := []memory.Block{
		{SpeakerLabel: "我", Text: "我喜欢游泳", SegmentIDs: []ids.ID{201}},
		{SpeakerLabel: "我", Text: "我说过我喜欢游泳", SegmentIDs: []ids.ID{202}},
	}
	resp := `{"facts":[{"plane":"attribute","subject":{"kind":"self"},"attr_key":"hobbies","value":"游泳",
		"confidence":0.6,"epistemic_type":"observed","block_index":1}]}`
	resp2 := `{"facts":[{"plane":"attribute","subject":{"kind":"self"},"attr_key":"hobbies","value":"游泳",
		"confidence":0.9,"epistemic_type":"observed","block_index":1}]}`
	ex := &Extractor{LLM: &fakeLLM{resps: []string{resp, resp2}}, Model: "m", Prompt: "s", Window: 1}
	facts, err := ex.Extract(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("跨窗口去重应剩 1 条: %+v", facts)
	}
	if facts[0].Confidence != 0.9 {
		t.Fatalf("应保留高置信: %v", facts[0].Confidence)
	}
	if ex.Stats().Windows != 2 {
		t.Fatalf("应 2 窗口: %d", ex.Stats().Windows)
	}
}
```

注：若 `internal/profile` 包内其他测试文件已定义 `fakeLLM`（后续任务会有），本文件复用之，勿重复定义。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/profile/ -run TestExtractor -v
```
Expected: FAIL，`undefined: Extractor`（编译错误）。

- [ ] **Step 3: 实现 extractor.go**

`internal/profile/extractor.go`：

```go
package profile

import (
	"context"
	"fmt"
	"strings"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/provider"
)

// ExtractStats 是一次 Extract 的调用统计（写 job.trace 用）。
type ExtractStats struct {
	Windows int // LLM 调用次数（窗口数）
	Tokens  int // 累计 token 用量
}

// PersonRef 是进入抽取 prompt 的已知人物名单（稳定引用，避免 LLM 每次发明新指代）。
type PersonRef struct {
	ID      ids.ID
	Name    string
	Aliases string // 逗号分隔别名（P1 空；aliases 属性扩展后续接）
	IsOwner bool
}

// Extractor 用 LLM 从对话块抽取画像事实：按窗口逐次调用、合并去重、回填溯源。
// 结构与 memory.Extractor 对齐（同一套窗口切分与 provenance 思路）。
type Extractor struct {
	LLM    provider.LLMProvider
	Model  string // 模型名（Tier 1 flash）
	Prompt string // prompts/profile_extraction_v1.md 内容
	Window int    // 窗口大小（块数），<=0 时 memory.SplitWindows 内部回退默认 10

	// stats 记录最近一次 Extract 的统计（每个 stage 各自 new 一个，无并发共享）。
	stats ExtractStats
}

func (e *Extractor) Stats() ExtractStats { return e.stats }

// Extract 抽取全部对话块。跨窗口同自然键（plane+subject+key+value）的重复
// 视为同一事实，保留置信度高者。
func (e *Extractor) Extract(ctx context.Context, blocks []memory.Block, persons []PersonRef) ([]Fact, error) {
	e.stats = ExtractStats{}
	var all []Fact
	seen := map[string]int{} // 自然键 -> 在 all 中的下标
	for winIdx, win := range memory.SplitWindows(blocks, e.Window) {
		resp, err := e.LLM.Chat(ctx, provider.ChatRequest{
			Model:  e.Model,
			System: e.Prompt,
			User:   buildProfileUserMessage(win, persons),
		})
		if err != nil {
			return nil, fmt.Errorf("LLM 调用: %w", err)
		}
		e.stats.Windows++
		e.stats.Tokens += resp.TotalTokens
		facts, err := ParseFacts(resp.Content)
		if err != nil {
			return nil, fmt.Errorf("第 %d 窗口解析失败: %w", winIdx+1, err)
		}
		for _, f := range facts {
			f.SegmentIDs = factProvenance(win, f.BlockIndex)
			key := factKey(f)
			if idx, ok := seen[key]; ok {
				if f.Confidence > all[idx].Confidence {
					all[idx] = f
				}
				continue
			}
			seen[key] = len(all)
			all = append(all, f)
		}
	}
	return all, nil
}

// factProvenance 由 block_index 定位来源块；越界（0 或超范围）用整个窗口的
// segment 并集兜底（宁粗勿丢，同 memory.blockProvenance 思路）。
func factProvenance(win []memory.Block, idx int) []ids.ID {
	if idx >= 1 && idx <= len(win) {
		return win[idx-1].SegmentIDs
	}
	var segs []ids.ID
	for _, b := range win {
		segs = append(segs, b.SegmentIDs...)
	}
	return segs
}

// factKey 批内去重自然键：平面+主体+内容拼串。
func factKey(f Fact) string {
	return f.Plane + "\x00" + f.Subject.Kind + "\x00" + f.Subject.Name + "\x00" +
		f.AttrKey + "\x00" + f.Value + "\x00" + f.RelationType + "\x00" + f.Related.Name
}

// buildProfileUserMessage 组装用户消息：对话块 + 已知人物名单。
func buildProfileUserMessage(win []memory.Block, persons []PersonRef) string {
	var sb strings.Builder
	sb.WriteString("对话块列表（格式：序号|说话人|文本）：\n")
	for i, b := range win {
		speaker := b.SpeakerLabel
		if speaker == "" {
			speaker = "未知"
		}
		fmt.Fprintf(&sb, "%d|%s|%s\n", i+1, speaker, b.Text)
	}
	sb.WriteString("\n已知人物列表（格式：person_id|名字|备注），subject 请优先引用已知人物：\n")
	if len(persons) == 0 {
		sb.WriteString("（暂无）\n")
	}
	for _, p := range persons {
		note := ""
		if p.IsOwner {
			note = "（用户本人，subject 用 {\"kind\":\"self\"}）"
		}
		fmt.Fprintf(&sb, "%s|%s|%s\n", p.ID.String(), p.Name, note)
	}
	return sb.String()
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/profile/ -run TestExtractor -v
```
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/profile/extractor.go internal/profile/extractor_test.go
git commit -m "feat(profile): Extractor——窗口切分/跨窗口去重/溯源回填"
```

---

### Task 10: 画像抽取 prompt（版本化文件）

**Files:**
- Create: `prompts/profile_extraction_v1.md`

- [ ] **Step 1: 写 prompt 文件**

`prompts/profile_extraction_v1.md`：

```markdown
# 画像抽取 prompt v1

你是「知微」的用户画像抽取器。从对话转写中抽取**关于人物的结构化画像事实**：
身份属性（职业/生日/习惯…）与人物关系（配偶/子女/同事…）。

只抽「稳定或有价值的人物信息」，不要抽：
- 一次性事件/待办/话题（另有记忆系统负责）
- 情绪、健康等时序状态（P3 由专门的平面处理，本版忽略）
- 你不确定归属主体的信息（宁少勿错）

## 输出格式

只输出 JSON，不要任何解释或 markdown 围栏：

{"facts": [
  {
    "plane": "attribute",
    "subject": {"kind": "self"},
    "attr_key": "occupation",
    "value": "后端开发工程师",
    "confidence": 0.9,
    "epistemic_type": "observed",
    "block_index": 1
  }
]}

没有可抽取的信息时输出 {"facts": []}。

## 字段说明

- plane：`attribute`（属性）或 `relationship`（关系）。
- subject（信息属于谁）：
  - `{"kind":"self"}` —— 第一人称「我」说的关于自己的信息
  - `{"kind":"speaker","name":"张三"}` —— 说话人说的关于自己的信息（name 填说话人名）
  - `{"kind":"mentioned","name":"Alice"}` —— 对话里提到的具名他人
  - `{"kind":"relation","relation":"配偶"}` —— 关系指代（「我老婆」→ 配偶；只对「我」的关系用）
- attr_key：必须从下方属性目录里选；对话表达了目录外的人物信息时可用简短英文 snake_case 自造 key。
- value：属性值，中文短语；bool 型用 "true"/"false"；日期用 YYYY-MM-DD。
- confidence：0~1，你对「这条信息真实且归属正确」的把握。转写含混、反讽、假设语气时降低。
- epistemic_type：`observed`（直接陈述）/ `inferred`（合理推断，如提到工牌推出职业）/ `suggested`（建议或猜测）。
- block_index：信息来源对话块的序号（用户消息里每行开头的数字）。
- relationship 平面额外字段：
  - related：关系对端的 subject（同 subject 结构）
  - relation_type：配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他
  - label：自由称呼（如「大儿子」「张总」），可选
  - direction：上下游关系填 upstream|downstream|peer，可选
  - org_name：组织关系（校友会/协会）填组织名，可选

## 属性目录（attr_key | 中文说明 | 类型）

基本：aliases 别名(列表) | birthday 生日(日期) | gender 性别(男/女/其他) | zodiac 星座 |
mbti MBTI | education 学历(高中及以下/大专/本科/硕士/博士) | school 学校(列表) |
city 城市 | address 住址 | phone 手机号

工作：occupation 职业 | industry 所属行业 | office_location 办公地点 |
work_start_time 上班时间 | work_end_time 下班时间 | commute_mode 通勤方式(步行/自行车/电动车/地铁/公交/开车/打车/班车/火车/高铁/飞机) |
often_travel 是否经常出差(bool) | current_projects 正在进行的项目(列表)

生活习惯：meal_time 吃饭时间 | cuisine 喜欢的菜系(列表:川菜/粤菜/湘菜/火锅/烧烤/西餐/日料/韩餐/家常菜等) |
eats_spicy 是否吃辣(bool) | eats_numbing 是否吃麻(bool) | smokes 是否吸烟(bool) |
drinks 是否喝酒(bool) | wears_makeup 是否化妆(bool) | perfume 香水

兴趣：hobbies 爱好(列表:游泳/读书/羽毛球/篮球/足球/乒乓球/钓鱼等) | skills 学的技能(列表:唱歌/弹琴/书法等) |
reading_now 正在看的书(列表) | books_read 看过的书(列表) | movies_watched 看过的影视(列表) |
music_listened 听过的音乐(列表) | games_played 玩过的游戏(列表) | fav_celebrities 喜欢的明星(列表) |
fav_anime 喜欢的动漫(列表) | fav_movie_genres 喜欢的电影类型(列表) | catchphrases 口头禅(列表) |
invests_stocks 是否炒股(bool)

出行物品：cities_visited 去过的城市(列表) | places_traveled 旅游过的地方(列表) |
has_car 是否有车(bool) | car_brand 车品牌 | phone_brand 手机品牌

关注性格：recent_concerns 最近关心的事情(列表) | attention_topics 关注领域(列表:政治/军事/体育/三农/科技/财经/娱乐/教育/健康) |
personality 性格

健康：chronic_diseases 慢性病(列表)

## 示例

对话：
1|我|我老婆 Alice 是儿科医生，我们家老大今年上小学了
2|我|最近太忙，每天九点才下班

输出：
{"facts": [
  {"plane":"relationship","subject":{"kind":"self"},"related":{"kind":"mentioned","name":"Alice"},
   "relation_type":"配偶","label":"老婆","confidence":0.95,"epistemic_type":"observed","block_index":1},
  {"plane":"attribute","subject":{"kind":"relation","relation":"配偶"},
   "attr_key":"occupation","value":"儿科医生","confidence":0.9,"epistemic_type":"observed","block_index":1},
  {"plane":"relationship","subject":{"kind":"self"},"related":{"kind":"relation","relation":"子女"},
   "relation_type":"子女","label":"老大","confidence":0.8,"epistemic_type":"inferred","block_index":1},
  {"plane":"attribute","subject":{"kind":"self"},"attr_key":"work_end_time","value":"21:00",
   "confidence":0.85,"epistemic_type":"observed","block_index":2}
]}
```

- [ ] **Step 2: 确认文件被运行时读取的路径正确**

文件放 `prompts/profile_extraction_v1.md`（与 extraction_v3.md 同目录）；main.go 装配在 Task 17。

- [ ] **Step 3: Commit**

```bash
git add prompts/profile_extraction_v1.md
git commit -m "feat(profile): 画像抽取 prompt v1（属性目录+关系+subject 指代协议）"
```

---

### Task 11: profile——Service + 人物归属解析 + ApplyFacts（事务编排核心）

**Files:**
- Create: `internal/profile/service.go`
- Create: `internal/profile/service_test.go`

- [ ] **Step 1: 写失败的集成测试**

`internal/profile/service_test.go`：

```go
package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// fakeLLM 已在 extractor_test.go 定义（同包共享），此处不重复定义。

// newTestService 建好 Service 并跑 bootstrap（owner「我」必备）。
// Memories/Speakers 必须给：ApplyFacts 读 session memories，speaker 归属解析查名册。
func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		DB: db, Persons: &repo.PersonRepo{DB: db},
		Memories:      &repo.MemoryRepo{DB: db},
		Speakers:      &repo.SpeakerRepo{DB: db},
		Attributes:    &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db},
		ChangeLogs:    &repo.PersonChangeLogRepo{DB: db},
		Gate:          GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(context.Background(), svc.Persons, &repo.SpeakerRepo{DB: db}); err != nil {
		t.Fatal(err)
	}
	return svc
}

func ownerID(t *testing.T, svc *Service) ids.ID {
	t.Helper()
	o, err := svc.Persons.GetOwner(context.Background(), 1)
	if err != nil || o == nil {
		t.Fatalf("owner 缺失: %v %v", o, err)
	}
	return o.ID
}

func TestApplyFactsGatePaths(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)
	sess := ids.New()

	facts := []Fact{
		// ① 无现值高置信 observed → active
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "工程师", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ② 无现值低置信 → pending
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "personality",
			Value: "内向", Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ③ 列表低置信 → pending
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "hobbies",
			Value: "游泳", Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ④ 关系：mentioned:Alice 高置信 → active + 自动建 pending 人物 Alice
		{Plane: "relationship", Subject: Subject{Kind: "self"},
			Related: Subject{Kind: "mentioned", Name: "Alice"}, RelationType: "配偶",
			Label: "老婆", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ⑤ 关系指代 subject：属性挂到 owner 的配偶（=上一步新建的 Alice）身上
		{Plane: "attribute", Subject: Subject{Kind: "relation", Relation: "配偶"},
			AttrKey: "occupation", Value: "医生", Confidence: 0.9, EpistemicType: "observed",
			SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	// ①④⑤ active；②③ pending
	if st.Active != 3 || st.Pending != 2 || st.Skipped != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	// 校验：occupation=工程师 active；personality/hobbies pending
	oa, _ := svc.Attributes.FindActiveByKey(ctx, oid, "occupation")
	if oa == nil || oa.ValueText != "工程师" || oa.Source != "llm" || oa.SessionID == nil || *oa.SessionID != sess {
		t.Fatalf("occupation active 行错误: %+v", oa)
	}
	pa, _ := svc.Attributes.FindActiveByKey(ctx, oid, "personality")
	if pa != nil {
		t.Fatalf("低置信不应 active: %+v", pa)
	}
	// Alice：pending 人物 + 配偶关系 active + occupation active
	alice, _ := svc.Persons.FindByName(ctx, 1, "Alice")
	if alice == nil || alice.Status != "pending" || alice.Source != "llm" {
		t.Fatalf("Alice 人物错误: %+v", alice)
	}
	rel, err := svc.Relationships.FindActiveByTypeExt(ctx, svc.DB, oid, "配偶", &alice.ID)
	if err != nil || rel == nil {
		t.Fatalf("配偶关系未建立: %v %v", rel, err)
	}
	ao, _ := svc.Attributes.FindActiveByKey(ctx, alice.ID, "occupation")
	if ao == nil || ao.ValueText != "医生" {
		t.Fatalf("Alice 职业错误: %+v", ao)
	}
	// 审计：owner 侧至少 create(attribute×3) 条目 + person create(Alice) + relationship create
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "attribute", "")
	if len(logs) < 3 {
		t.Fatalf("owner 属性审计不足: %d", len(logs))
	}

	// 幂等：同 session 重跑全部 skip
	st2, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != len(facts) || st2.Active != 0 || st2.Pending != 0 || st2.Reaffirmed != 0 {
		t.Fatalf("重跑应全部 skip: %+v", st2)
	}
	// Alice 不被重复创建
	if a2, _ := svc.Persons.FindByName(ctx, 1, "Alice"); a2.ID != alice.ID {
		t.Fatal("Alice 被重复创建")
	}

	// 冲突：另一 session 说 occupation=教师（高置信）→ pending + supersedes 指向现值
	sess2 := ids.New()
	st3, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "教师", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3.Pending != 1 || st3.Conflicts != 1 {
		t.Fatalf("冲突统计错误: %+v", st3)
	}
	// 佐证：sess2 再说 hobbies=游泳（sess 里已有 pending 行？不对——自然键含 session，
	// sess2 无此行 → 走闸门：列表 existing（active）无、pending 行不算 existing → create_pending）
	// 改测佐证路径：sess 里 active 的 occupation 在 sess2 重申 → reaffirm
	st4, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "工程师", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st4.Reaffirmed != 1 {
		t.Fatalf("同值重申应 reaffirm: %+v", st4)
	}
}
```

注：最后一段 st4 与 st3 都用 sess2 且 key=occupation 但值不同（教师/工程师）——自然键不同不冲突；st4 命中 active 同值行 → reaffirm。语义正确。

- [ ] **Step 2: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/profile/ -run TestApplyFacts -v
```
Expected: FAIL，`undefined: Service`（编译错误）。

- [ ] **Step 3: 实现 service.go**

`internal/profile/service.go`：

```go
package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// ErrNotFound 目标行不存在（API 层映射 404）。
var ErrNotFound = errors.New("记录不存在")

// Service 是画像域的编排服务：pipeline profile stage 与 API（回填/确认/手动 CRUD）
// 共用同一入口，保证「写必带审计 + 单事务 + 闸门」三件事只实现一次。
type Service struct {
	DB            *sqlx.DB
	Sessions      *repo.SessionRepo // ExtractSession 用（Task 13）
	Transcripts   *repo.TranscriptRepo
	Memories      *repo.MemoryRepo
	Speakers      *repo.SpeakerRepo
	Persons       *repo.PersonRepo
	Attributes    *repo.PersonAttributeRepo
	Relationships *repo.PersonRelationshipRepo
	ChangeLogs    *repo.PersonChangeLogRepo

	LLM           provider.LLMProvider // ExtractSession 用（Task 13）；手动 CRUD 不需要
	Model         string
	Prompt        string
	PromptVersion string
	Window        int
	Gate          GateConfig
}

// Provenance 一条事实的溯源信息。
type Provenance struct {
	SessionID  ids.ID
	SegmentIDs []ids.ID
}

// ApplyStats 一次 ApplyFacts 的决策统计（trace 与日志用）。
type ApplyStats struct {
	Total      int
	Active     int // 直接写入 active
	Pending    int // 低置信/冲突待确认
	Reaffirmed int // 同值佐证（置信度上调）
	Conflicts  int // Pending 中的冲突条数
	Skipped    int // 幂等跳过 / 主体解析不到
}

// ApplyFacts 把一批 LLM 事实应用到库：人物归属解析 → 闸门 → 单事务写入
// （含 change_log）。幂等靠自然键去重（spec §6.3）——同 session 重跑不重复
// 建 pending、不重复 bump；用户此前的 confirm/dismiss 决定保留。
func (s *Service) ApplyFacts(ctx context.Context, sessionID ids.ID, userID int64, facts []Fact) (ApplyStats, error) {
	var st ApplyStats
	st.Total = len(facts)
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后 Rollback 是 no-op

	// 本 session 的 memories：供 memory_id 溯源（按 segment 交集最大匹配）。
	// 事务外读即可（只读，不依赖事务内一致性）。
	memRows, err := s.Memories.ListBySession(ctx, sessionID)
	if err != nil {
		return st, fmt.Errorf("读 session memories: %w", err)
	}

	for _, f := range facts {
		prov := Provenance{SessionID: sessionID, SegmentIDs: f.SegmentIDs}
		if err := s.applyFact(ctx, tx, userID, f, prov, memRows, &st); err != nil {
			return st, fmt.Errorf("应用事实(plane=%s key=%s): %w", f.Plane, f.AttrKey, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

func (s *Service) applyFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	prov Provenance, memRows []repo.MemoryRow, st *ApplyStats) error {

	personID, err := s.resolveSubject(ctx, tx, f.Subject, prov)
	if err != nil {
		return err
	}
	if personID == 0 {
		st.Skipped++ // 主体解析不到（如无名 relation 指代且查不到对端）
		return nil
	}
	memID := matchMemory(memRows, f.SegmentIDs)

	if f.Plane == "relationship" {
		return s.applyRelationshipFact(ctx, tx, userID, f, personID, memID, prov, st)
	}
	return s.applyAttributeFact(ctx, tx, userID, f, personID, memID, prov, st)
}

// ---- 属性平面 ----

func (s *Service) applyAttributeFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	d := Def(f.AttrKey)
	isList := d.Cardinality == CardinalityList

	var existing *repo.PersonAttribute
	var err error
	if isList {
		existing, err = s.Attributes.FindActiveByKeyValueExt(ctx, tx, personID, f.AttrKey, f.Value)
	} else {
		existing, err = s.Attributes.FindActiveByKeyExt(ctx, tx, personID, f.AttrKey)
	}
	if err != nil {
		return err
	}
	dedup, err := s.Attributes.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID, f.AttrKey, f.Value)
	if err != nil {
		return err
	}

	switch DecideAttribute(f, existing, isList, dedup != nil, s.Gate) {
	case DecisionSkip:
		st.Skipped++
	case DecisionReaffirm:
		if err := s.Attributes.BumpConfidenceExt(ctx, tx, existing.ID, 0.05); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, reaffirmAttrLog(personID, existing, memID, prov)); err != nil {
			return err
		}
		st.Reaffirmed++
	case DecisionCreateActive:
		row := attrRow(userID, personID, f, d, "active", nil, memID, prov)
		if err := s.Attributes.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createAttrLog(personID, row, memID, prov, "")); err != nil {
			return err
		}
		st.Active++
	case DecisionCreatePending, DecisionConflictPending:
		var sup *ids.ID
		note := ""
		if existing != nil {
			idv := existing.ID
			sup = &idv
			note = "conflict: 与现值「" + existing.ValueText + "」冲突，待人工确认"
			st.Conflicts++
		}
		row := attrRow(userID, personID, f, d, "pending", sup, memID, prov)
		if err := s.Attributes.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createAttrLog(personID, row, memID, prov, note)); err != nil {
			return err
		}
		st.Pending++
	}
	return nil
}

// ---- 关系平面 ----

func (s *Service) applyRelationshipFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	relatedID, err := s.resolveSubject(ctx, tx, f.Related, prov)
	if err != nil {
		return err
	}
	if relatedID == 0 && f.OrgName == "" && f.Related.Name == "" {
		st.Skipped++ // 对端完全解析不到且无组织名
		return nil
	}

	existing, err := s.Relationships.FindActiveByTypeExt(ctx, tx, personID, f.RelationType, idPtr(relatedID))
	if err != nil {
		return err
	}
	dedup, err := s.Relationships.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID, f.RelationType, idPtr(relatedID))
	if err != nil {
		return err
	}

	dec := DecideRelationship(f, existing, dedup != nil, s.Gate)
	switch dec {
	case DecisionSkip:
		st.Skipped++
	case DecisionReaffirm:
		if err := s.Relationships.SetStatusExt(ctx, tx, existing.ID, "active"); err != nil { // no-op touch（updated_at 刷新，确认可见）
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, reaffirmRelLog(personID, existing, memID, prov)); err != nil {
			return err
		}
		st.Reaffirmed++
	default: // DecisionCreateActive / DecisionCreatePending
		status := "pending"
		if dec == DecisionCreateActive {
			status = "active"
		}
		row := relRow(userID, personID, f, relatedID, status, memID, prov)
		if err := s.Relationships.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createRelLog(personID, row, memID, prov)); err != nil {
			return err
		}
		if status == "active" {
			st.Active++
		} else {
			st.Pending++
		}
	}
	return nil
}

// ---- 人物归属解析（spec §6.2）----

// resolveSubject 把 LLM 的 subject 指代解析为 person id（事务内执行）：
//   self → owner；speaker:名 → 声纹名册按名找 speaker 再找绑定 person，找不到回落按名解析；
//   relation:类型 → owner 的该类型 active 关系对端；查不到且有名字则按名解析；
//   mentioned:名 → 按名找，找不到新建 source=llm status=pending 人物（走确认防噪声）。
// 返回 0 表示解析不到（调用方跳过该事实）。
func (s *Service) resolveSubject(ctx context.Context, tx *sqlx.Tx, subj Subject, prov Provenance) (ids.ID, error) {
	switch subj.Kind {
	case "self":
		return s.ownerID(ctx, tx)
	case "speaker":
		if pid, err := s.personBySpeakerName(ctx, tx, subj.Name); err != nil {
			return 0, err
		} else if pid != 0 {
			return pid, nil
		}
		return s.resolveOrCreateByName(ctx, tx, subj.Name, prov)
	case "relation":
		if pid, err := s.personByOwnerRelation(ctx, tx, subj.Relation); err != nil {
			return 0, err
		} else if pid != 0 {
			return pid, nil
		}
		if subj.Name != "" {
			return s.resolveOrCreateByName(ctx, tx, subj.Name, prov)
		}
		return 0, nil
	case "mentioned":
		return s.resolveOrCreateByName(ctx, tx, subj.Name, prov)
	}
	return 0, nil
}

func (s *Service) ownerID(ctx context.Context, tx *sqlx.Tx) (ids.ID, error) {
	owner, err := s.Persons.GetOwnerExt(ctx, tx, 1)
	if err != nil {
		return 0, err
	}
	if owner == nil {
		return 0, fmt.Errorf("owner person 缺失（EnsurePersonBootstrap 未跑）")
	}
	return owner.ID, nil
}

// personBySpeakerName 声纹名册按名找 active speaker → 绑定的 person。
// 名册规模小（MVP），直接全量遍历；speaker 名通常就是 person 名。
func (s *Service) personBySpeakerName(ctx context.Context, tx *sqlx.Tx, name string) (ids.ID, error) {
	if name == "" {
		return 0, nil
	}
	list, err := s.Speakers.List(ctx)
	if err != nil {
		return 0, err
	}
	for _, sp := range list {
		if sp.Status != "active" || sp.Name != name {
			continue
		}
		p, err := s.Persons.GetBySpeakerExt(ctx, tx, sp.ID)
		if err != nil {
			return 0, err
		}
		if p != nil {
			return p.ID, nil
		}
	}
	return 0, nil
}

// personByOwnerRelation owner 的指定类型 active 关系对端（「我老婆」→ 配偶 person）。
func (s *Service) personByOwnerRelation(ctx context.Context, tx *sqlx.Tx, relationType string) (ids.ID, error) {
	owner, err := s.Persons.GetOwnerExt(ctx, tx, 1)
	if err != nil || owner == nil {
		return 0, err
	}
	rel, err := s.Relationships.FindActiveByTypeExt(ctx, tx, owner.ID, relationType, nil)
	if err != nil {
		return 0, err
	}
	if rel == nil {
		// 对端可能非 nil 的同类型关系：列表找第一条有对端的（如多个子女取最老的一条）
		list, err := s.Relationships.ListByPerson(ctx, owner.ID)
		if err != nil {
			return 0, err
		}
		for i := range list {
			r := list[i]
			if r.RelationType == relationType && r.Status == "active" && r.RelatedPersonID != nil {
				return *r.RelatedPersonID, nil
			}
		}
		return 0, nil
	}
	if rel.RelatedPersonID == nil {
		return 0, nil
	}
	return *rel.RelatedPersonID, nil
}

// resolveOrCreateByName 按显示名找 active/pending 人物；找不到新建
// source=llm status=pending 的人物并记审计（spec §2 决策 2：自动建档走确认）。
func (s *Service) resolveOrCreateByName(ctx context.Context, tx *sqlx.Tx, name string, prov Provenance) (ids.ID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	p, err := s.Persons.FindByNameExt(ctx, tx, 1, name)
	if err != nil {
		return 0, err
	}
	if p != nil {
		return p.ID, nil
	}
	p = &repo.Person{DisplayName: name, Source: "llm", Status: "pending"}
	if err := s.Persons.CreateExt(ctx, tx, p); err != nil {
		return 0, err
	}
	sid := prov.SessionID
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: p.ID, EntityKind: "person", EntityID: &p.ID,
		ChangeType: "create", ChangedBy: "llm", NewValue: snap(p.DisplayName),
		SessionID: &sid, Note: strPtr("LLM 抽取自动新建人物，待确认"),
	}); err != nil {
		return 0, err
	}
	return p.ID, nil
}

// ---- 行构造与审计构造小工具 ----

func attrRow(userID int64, personID ids.ID, f Fact, d AttrDef, status string,
	supersedes, memID *ids.ID, prov Provenance) *repo.PersonAttribute {
	return &repo.PersonAttribute{
		UserID: userID, PersonID: personID, AttrKey: f.AttrKey, ValueText: f.Value,
		ValueType: d.ValueType, Confidence: f.Confidence, EpistemicType: f.EpistemicType,
		Source: "llm", Status: status, SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs), SupersedesID: supersedes,
	}
}

func relRow(userID int64, personID ids.ID, f Fact, relatedID ids.ID, status string,
	memID *ids.ID, prov Provenance) *repo.PersonRelationship {
	row := &repo.PersonRelationship{
		UserID: userID, PersonID: personID, RelationType: f.RelationType,
		Confidence: f.Confidence, EpistemicType: f.EpistemicType,
		Source: "llm", Status: status, SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
	if relatedID != 0 {
		row.RelatedPersonID = &relatedID
	}
	if f.Direction != "" {
		row.Direction = strPtr(f.Direction)
	}
	if f.OrgName != "" {
		row.OrgName = strPtr(f.OrgName)
	}
	if f.Label != "" {
		row.Label = strPtr(f.Label)
	}
	return row
}

func createAttrLog(personID ids.ID, row *repo.PersonAttribute, memID *ids.ID, prov Provenance, note string) *repo.PersonChangeLog {
	l := &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "attribute", EntityID: &row.ID,
		AttrKey: strPtr(row.AttrKey), ChangeType: "create", ChangedBy: "llm",
		NewValue: snap(row.ValueText), Confidence: fp(row.Confidence),
		EpistemicType: strPtr(row.EpistemicType),
		SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
	if note != "" {
		l.Note = strPtr(note)
	}
	return l
}

func reaffirmAttrLog(personID ids.ID, row *repo.PersonAttribute, memID *ids.ID, prov Provenance) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "attribute", EntityID: &row.ID,
		AttrKey: strPtr(row.AttrKey), ChangeType: "reaffirm", ChangedBy: "llm",
		NewValue: snap(row.ValueText), SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
		Note: strPtr("同值佐证：置信度 +0.05（封顶 0.99）"),
	}
}

func createRelLog(personID ids.ID, row *repo.PersonRelationship, memID *ids.ID, prov Provenance) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "relationship", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "llm", NewValue: snap(row.RelationType),
		Confidence: fp(row.Confidence), SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
}

func reaffirmRelLog(personID ids.ID, row *repo.PersonRelationship, memID *ids.ID, prov Provenance) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "relationship", EntityID: &row.ID,
		ChangeType: "reaffirm", ChangedBy: "llm", NewValue: snap(row.RelationType),
		SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
}

// matchMemory 按 segment 交集给事实找最相关的 memory（溯源 memory_id）；无交集返回 nil。
func matchMemory(rows []repo.MemoryRow, segIDs []ids.ID) *ids.ID {
	var best *ids.ID
	bestN := 0
	for i := range rows {
		n := 0
		for _, sid := range rows[i].TranscriptSegmentIDs {
			for _, f := range segIDs {
				if sid == f {
					n++
				}
			}
		}
		if n > bestN {
			idv := rows[i].ID
			best = &idv
			bestN = n
		}
	}
	return best
}

// ---- 小工具 ----

// snap 把任意值序列化为 JSON 文本快照（change_log old/new_value 用）。
func snap(v any) *string {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func strPtr(s string) *string { return &s }

func fp(f float64) *float64 { return &f }

// idPtr 0 → nil（SQL NULL 安全传参）。
func idPtr(id ids.ID) *ids.ID {
	if id == 0 {
		return nil
	}
	return &id
}
```

注：`fp` 若与 `internal/repo` 包内测试工具重名没关系（不同包）；但 `internal/profile` 包内若与既有测试文件冲突，保留一份即可。

- [ ] **Step 4: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/profile/ -run TestApplyFacts -v
```
Expected: PASS（闸门全路径 + 归属解析 + 幂等 + 冲突 + 佐证）。

- [ ] **Step 5: Commit**

```bash
git add internal/profile/service.go internal/profile/service_test.go
git commit -m "feat(profile): Service.ApplyFacts——归属解析+闸门+单事务落库+审计"
```

---

### Task 12: profile——手动 CRUD（service_manual.go）+ 确认/放弃（confirm.go）

**Files:**
- Create: `internal/profile/service_manual.go`
- Create: `internal/profile/confirm.go`
- Create: `internal/profile/confirm_test.go`

- [ ] **Step 1: 写失败的集成测试**

`internal/profile/confirm_test.go`：

```go
package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestManualAndConfirmFlows(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	// ---- 手动建人物 + 手动加属性 ----
	p, err := svc.ManualCreatePerson(ctx, "Bob", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "active" || p.Source != "manual" {
		t.Fatalf("手动人物应 active/manual: %+v", p)
	}
	// 手动加属性：单值 key 无现值 → active conf=1.0 source=manual
	a1, err := svc.ManualAddAttribute(ctx, oid, "city", "北京")
	if err != nil {
		t.Fatal(err)
	}
	if a1.Status != "active" || a1.Confidence != 1.0 || a1.Source != "manual" {
		t.Fatalf("手动属性错误: %+v", a1)
	}
	// 手动改值：旧行 superseded、新行 active 且 supersedes_id 指向旧行
	a2, err := svc.ManualAddAttribute(ctx, oid, "city", "上海")
	if err != nil {
		t.Fatal(err)
	}
	if a2.Status != "active" || a2.SupersedesID == nil || *a2.SupersedesID != a1.ID {
		t.Fatalf("手动改值应 supersede: %+v", a2)
	}
	old, _ := svc.Attributes.Get(ctx, a1.ID)
	if old.Status != "superseded" {
		t.Fatalf("旧值应 superseded: %+v", old)
	}
	// 手动加关系
	rel, err := svc.ManualAddRelationship(ctx, oid, "朋友", &p.ID, "", "", "老朋友")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Status != "active" || rel.Source != "manual" {
		t.Fatalf("手动关系错误: %+v", rel)
	}

	// ---- 确认队列：冲突 pending 确认 → 旧 superseded 新 active ----
	// 此刻 city 的 active 行是 a2（上海）
	sess := ids.New()
	_, err = svc.ApplyFacts(ctx, sess, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "city",
			Value: "深圳", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pend, _ := svc.Attributes.ListPending(ctx, 1)
	var cityPend *ids.ID
	for i := range pend {
		if pend[i].AttrKey == "city" && pend[i].ValueText == "深圳" {
			idv := pend[i].ID
			cityPend = &idv
		}
	}
	if cityPend == nil {
		t.Fatal("city 深圳 pending 未生成")
	}
	if err := svc.ConfirmPending(ctx, "attribute", *cityPend); err != nil {
		t.Fatal(err)
	}
	confirmed, _ := svc.Attributes.Get(ctx, *cityPend)
	if confirmed.Status != "active" {
		t.Fatalf("确认后应 active: %+v", confirmed)
	}
	if confirmed.SupersedesID == nil || *confirmed.SupersedesID != a2.ID {
		t.Fatalf("冲突确认行应 supersedes a2: %+v", confirmed.SupersedesID)
	}
	replaced, _ := svc.Attributes.Get(ctx, a2.ID)
	if replaced.Status != "superseded" {
		t.Fatalf("被替换的上海行应 superseded: %+v", replaced)
	}

	// ---- 手动删属性 → dismissed（放最后：前面冲突流依赖 city 的 active 行）----
	if err := svc.ManualDeleteAttribute(ctx, a2.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Attributes.Get(ctx, a2.ID); d.Status != "dismissed" {
		t.Fatalf("删除应 dismissed: %+v", d)
	}

	// ---- 放弃：pending → dismissed ----
	sess2 := ids.New()
	_, _ = svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "personality",
			Value: "外向", Confidence: 0.5, EpistemicType: "observed"},
	})
	pend2, _ := svc.Attributes.ListPending(ctx, 1)
	if len(pend2) == 0 {
		t.Fatal("应有 pending")
	}
	if err := svc.DismissPending(ctx, "attribute", pend2[0].ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Attributes.Get(ctx, pend2[0].ID); d.Status != "dismissed" {
		t.Fatalf("放弃后应 dismissed: %+v", d)
	}

	// ---- 确认 pending 人物 ----
	sess3 := ids.New()
	_, _ = svc.ApplyFacts(ctx, sess3, 1, []Fact{
		{Plane: "relationship", Subject: Subject{Kind: "self"},
			Related: Subject{Kind: "mentioned", Name: "确认人物测试"}, RelationType: "朋友",
			Confidence: 0.9, EpistemicType: "observed"},
	})
	cand, _ := svc.Persons.FindByName(ctx, 1, "确认人物测试")
	if cand == nil || cand.Status != "pending" {
		t.Fatalf("应为 pending 人物: %+v", cand)
	}
	if err := svc.ConfirmPending(ctx, "person", cand.ID); err != nil {
		t.Fatal(err)
	}
	if c2, _ := svc.Persons.Get(ctx, cand.ID); c2.Status != "active" {
		t.Fatalf("人物确认后应 active: %+v", c2)
	}

	// ---- 不存在/状态非法 → ErrNotFound / 业务错误 ----
	if err := svc.ConfirmPending(ctx, "attribute", ids.New()); err == nil {
		t.Fatal("不存在的 id 应报错")
	}
	if err := svc.ConfirmPending(ctx, "bogus", a1.ID); err == nil {
		t.Fatal("非法 kind 应报错")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/profile/ -run TestManualAndConfirm -v
```
Expected: FAIL，`undefined: (*Service).ManualCreatePerson`（编译错误）。

- [ ] **Step 3: 实现 service_manual.go**

`internal/profile/service_manual.go`：

```go
package profile

import (
	"context"
	"fmt"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// 手动操作（spec §5.1）：立即 active、source=manual、confidence=1.0、记审计
// （changed_by=user）。手动改值 = 旧行 superseded + 新行（supersedes_id 指向旧行）。

// ManualCreatePerson 手动新建人物（active/manual + create 审计）。
func (s *Service) ManualCreatePerson(ctx context.Context, name string, speakerID *ids.ID, summary *string) (*repo.Person, error) {
	p := &repo.Person{DisplayName: name, SpeakerID: speakerID, Summary: summary, Source: "manual", Status: "active"}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Persons.CreateExt(ctx, tx, p); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: p.ID, EntityKind: "person", EntityID: &p.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(p.DisplayName),
	}); err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// ManualUpdatePerson 手动编辑人物（改名/换绑声纹/改备注）。
func (s *Service) ManualUpdatePerson(ctx context.Context, id ids.ID, name string, speakerID *ids.ID, summary *string) error {
	p, err := s.Persons.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Persons.Update(ctx, id, name, speakerID, summary); err != nil {
		return err
	}
	old := snap(p.DisplayName)
	newV := snap(name)
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: id, EntityKind: "person", EntityID: &id,
		ChangeType: "update", ChangedBy: "user", OldValue: old, NewValue: newV,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ManualSetPersonStatus 人物状态流转（归档=dismissed 等）。
func (s *Service) ManualSetPersonStatus(ctx context.Context, id ids.ID, status string) error {
	p, err := s.Persons.Get(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Persons.SetStatus(ctx, id, status); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: id, EntityKind: "person", EntityID: &id,
		ChangeType: "update", ChangedBy: "user", OldValue: snap(p.Status), NewValue: snap(status),
		Note: strPtr("人物状态流转"),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ManualAddAttribute 手动加/改属性：单值型已有 active 时旧行 superseded、
// 新行 supersedes_id 指向旧行（即手动改值）；列表型纯叠加新行。
func (s *Service) ManualAddAttribute(ctx context.Context, personID ids.ID, attrKey, value string) (*repo.PersonAttribute, error) {
	d := Def(attrKey)
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var existing *repo.PersonAttribute
	if d.Cardinality == CardinalityList {
		existing, err = s.Attributes.FindActiveByKeyValueExt(ctx, tx, personID, attrKey, value)
	} else {
		existing, err = s.Attributes.FindActiveByKeyExt(ctx, tx, personID, attrKey)
	}
	if err != nil {
		return nil, err
	}
	// 同值已存在：幂等返回旧行（不重复叠加）
	if existing != nil && repo.NormalizeTitle(existing.ValueText) == repo.NormalizeTitle(value) {
		return existing, tx.Rollback() // no-op
	}

	var sup *ids.ID
	changeType := "create"
	if existing != nil {
		idv := existing.ID
		sup = &idv
		changeType = "update"
		if err := s.Attributes.SetStatusExt(ctx, tx, existing.ID, "superseded"); err != nil {
			return nil, err
		}
	}
	row := &repo.PersonAttribute{
		PersonID: personID, AttrKey: attrKey, ValueText: value, ValueType: d.ValueType,
		Confidence: 1.0, EpistemicType: "observed", Source: "manual",
		Status: "active", SupersedesID: sup,
	}
	if err := s.Attributes.CreateExt(ctx, tx, row); err != nil {
		return nil, err
	}
	l := &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "attribute", EntityID: &row.ID,
		AttrKey: strPtr(attrKey), ChangeType: changeType, ChangedBy: "user",
		NewValue: snap(value), Confidence: fp(1.0), EpistemicType: strPtr("observed"),
	}
	if existing != nil {
		l.OldValue = snap(existing.ValueText)
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, l); err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualDeleteAttribute 手动删属性 → dismissed + delete 审计。
func (s *Service) ManualDeleteAttribute(ctx context.Context, id ids.ID) error {
	a, err := s.Attributes.Get(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Attributes.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: a.PersonID, EntityKind: "attribute", EntityID: &id,
		AttrKey: strPtr(a.AttrKey), ChangeType: "delete", ChangedBy: "user",
		OldValue: snap(a.ValueText),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ManualAddRelationship 手动加关系边（active/manual + create 审计）。
// relatedPersonID 可空（组织关系）；direction/orgName/label 可选。
func (s *Service) ManualAddRelationship(ctx context.Context, personID ids.ID, relationType string,
	relatedPersonID *ids.ID, direction, orgName, label string) (*repo.PersonRelationship, error) {

	if !ValidRelations[relationType] {
		return nil, fmt.Errorf("非法关系类型: %s", relationType)
	}
	row := &repo.PersonRelationship{
		PersonID: personID, RelatedPersonID: relatedPersonID, RelationType: relationType,
		Confidence: 1.0, EpistemicType: "observed", Source: "manual", Status: "active",
	}
	if direction != "" {
		row.Direction = strPtr(direction)
	}
	if orgName != "" {
		row.OrgName = strPtr(orgName)
	}
	if label != "" {
		row.Label = strPtr(label)
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Relationships.CreateExt(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "relationship", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "user", NewValue: snap(relationType),
		Confidence: fp(1.0),
	}); err != nil {
		return nil, err
	}
	return row, tx.Commit()
}

// ManualDeleteRelationship 手动删关系 → dismissed + delete 审计。
func (s *Service) ManualDeleteRelationship(ctx context.Context, id ids.ID) error {
	rel, err := s.Relationships.Get(ctx, id)
	if err != nil {
		return err
	}
	if rel == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.Relationships.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: rel.PersonID, EntityKind: "relationship", EntityID: &id,
		ChangeType: "delete", ChangedBy: "user", OldValue: snap(rel.RelationType),
	}); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: 实现 confirm.go**

`internal/profile/confirm.go`：

```go
package profile

import (
	"context"
	"fmt"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// ConfirmPending 确认一条 pending（kind ∈ person|attribute|relationship）：
// pending → active；attribute/relationship 若带 supersedes_id，被指向的旧行 → superseded。
// 每步变更记审计（changed_by=user）。非 pending 行确认报错（幂等由前端/状态保证）。
func (s *Service) ConfirmPending(ctx context.Context, kind string, id ids.ID) error {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	switch kind {
	case "person":
		p, err := s.Persons.Get(ctx, id)
		if err != nil {
			return err
		}
		if p == nil {
			return ErrNotFound
		}
		if p.Status != "pending" {
			return fmt.Errorf("仅 pending 状态可确认（当前 %s）", p.Status)
		}
		if err := s.Persons.SetStatus(ctx, id, "active"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: id, EntityKind: "person", EntityID: &id,
			ChangeType: "confirm", ChangedBy: "user", NewValue: snap(p.DisplayName),
			Note: strPtr("确认 LLM 自动新建的人物"),
		}); err != nil {
			return err
		}
	case "attribute":
		a, err := s.Attributes.Get(ctx, id)
		if err != nil {
			return err
		}
		if a == nil {
			return ErrNotFound
		}
		if a.Status != "pending" {
			return fmt.Errorf("仅 pending 状态可确认（当前 %s）", a.Status)
		}
		if a.SupersedesID != nil {
			if err := s.Attributes.SetStatusExt(ctx, tx, *a.SupersedesID, "superseded"); err != nil {
				return err
			}
			if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
				PersonID: a.PersonID, EntityKind: "attribute", EntityID: a.SupersedesID,
				AttrKey: strPtr(a.AttrKey), ChangeType: "supersede", ChangedBy: "user",
				Note: strPtr("冲突确认：旧值被新值替换"),
			}); err != nil {
				return err
			}
		}
		if err := s.Attributes.SetStatusExt(ctx, tx, id, "active"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: a.PersonID, EntityKind: "attribute", EntityID: &id,
			AttrKey: strPtr(a.AttrKey), ChangeType: "confirm", ChangedBy: "user",
			NewValue: snap(a.ValueText), OldValue: snap(""),
			Confidence: fp(a.Confidence), EpistemicType: strPtr(a.EpistemicType),
		}); err != nil {
			return err
		}
	case "relationship":
		rel, err := s.Relationships.Get(ctx, id)
		if err != nil {
			return err
		}
		if rel == nil {
			return ErrNotFound
		}
		if rel.Status != "pending" {
			return fmt.Errorf("仅 pending 状态可确认（当前 %s）", rel.Status)
		}
		if rel.SupersedesID != nil {
			if err := s.Relationships.SetStatusExt(ctx, tx, *rel.SupersedesID, "superseded"); err != nil {
				return err
			}
		}
		if err := s.Relationships.SetStatusExt(ctx, tx, id, "active"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: rel.PersonID, EntityKind: "relationship", EntityID: &id,
			ChangeType: "confirm", ChangedBy: "user", NewValue: snap(rel.RelationType),
			Confidence: fp(rel.Confidence),
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("未知 kind: %s（可选 person|attribute|relationship）", kind)
	}
	return tx.Commit()
}

// DismissPending 放弃一条 pending（或手动 dismiss 任意行）→ dismissed + 审计。
func (s *Service) DismissPending(ctx context.Context, kind string, id ids.ID) error {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	switch kind {
	case "person":
		p, err := s.Persons.Get(ctx, id)
		if err != nil {
			return err
		}
		if p == nil {
			return ErrNotFound
		}
		if err := s.Persons.SetStatus(ctx, id, "dismissed"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: id, EntityKind: "person", EntityID: &id,
			ChangeType: "dismiss", ChangedBy: "user", OldValue: snap(p.DisplayName),
			Note: strPtr("放弃 LLM 自动新建的人物"),
		}); err != nil {
			return err
		}
	case "attribute":
		a, err := s.Attributes.Get(ctx, id)
		if err != nil {
			return err
		}
		if a == nil {
			return ErrNotFound
		}
		if err := s.Attributes.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: a.PersonID, EntityKind: "attribute", EntityID: &id,
			AttrKey: strPtr(a.AttrKey), ChangeType: "dismiss", ChangedBy: "user",
			OldValue: snap(a.ValueText), Confidence: fp(a.Confidence),
		}); err != nil {
			return err
		}
	case "relationship":
		rel, err := s.Relationships.Get(ctx, id)
		if err != nil {
			return err
		}
		if rel == nil {
			return ErrNotFound
		}
		if err := s.Relationships.SetStatusExt(ctx, tx, id, "dismissed"); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: rel.PersonID, EntityKind: "relationship", EntityID: &id,
			ChangeType: "dismiss", ChangedBy: "user", OldValue: snap(rel.RelationType),
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("未知 kind: %s（可选 person|attribute|relationship）", kind)
	}
	return tx.Commit()
}
```

- [ ] **Step 5: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/profile/ -run 'TestManualAndConfirm|TestApplyFacts' -v
```
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/profile/service_manual.go internal/profile/confirm.go internal/profile/confirm_test.go
git commit -m "feat(profile): 手动 CRUD（写必带审计）+ ConfirmPending/DismissPending"
```

---

### Task 13: profile——ExtractSession（stage 与回填共用入口）

**Files:**
- Create: `internal/profile/extract_session.go`
- Create: `internal/profile/extract_session_test.go`

- [ ] **Step 1: 写失败的集成测试**

`internal/profile/extract_session_test.go`：

```go
package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// mkSession 给 ExtractSession 造最小 session+transcript+segments 夹具。
func mkSession(t *testing.T, svc *Service, texts []string) ids.ID {
	t.Helper()
	ctx := context.Background()
	sess := &repo.AudioSession{Source: "web_upload", Filename: "t.wav", StoragePath: "/tmp/t.wav", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sess.ID, Language: "zh-CN"}
	if err := svc.Transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	segs := make([]repo.TranscriptSegment, len(texts))
	for i, txt := range texts {
		segs[i] = repo.TranscriptSegment{
			TranscriptID: tr.ID, SequenceNo: i + 1, SpeakerLabel: "我", Text: txt,
			StartMS: int64(i * 4000), EndMS: int64(i*4000 + 3000),
		}
	}
	if err := svc.Transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

func TestExtractSession(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	svc := &Service{
		DB: db,
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Speakers: &repo.SpeakerRepo{DB: db},
		Persons: &repo.PersonRepo{DB: db}, Attributes: &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db}, ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
		LLM: &fakeLLM{resps: []string{`{"facts":[
			{"plane":"attribute","subject":{"kind":"self"},"attr_key":"occupation","value":"后端开发工程师",
			 "confidence":0.9,"epistemic_type":"observed","block_index":1},
			{"plane":"relationship","subject":{"kind":"self"},"related":{"kind":"mentioned","name":"Alice"},
			 "relation_type":"配偶","label":"老婆","confidence":0.85,"epistemic_type":"observed","block_index":1}
		]}`}},
		Model: "test", Prompt: "sys", Window: 10, Gate: GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(ctx, svc.Persons, svc.Speakers); err != nil {
		t.Fatal(err)
	}

	sid := mkSession(t, svc, []string{"我在互联网公司做后端开发，我老婆 Alice 是医生"})
	res, err := svc.ExtractSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if res.Windows != 1 || res.Tokens != 42 {
		t.Fatalf("stats 错误: %+v", res)
	}
	if res.Apply.Active != 2 {
		t.Fatalf("应 2 条 active(职业+配偶关系): %+v", res.Apply)
	}
	oid := ownerID(t, svc)
	oa, _ := svc.Attributes.FindActiveByKey(ctx, oid, "occupation")
	if oa == nil || oa.ValueText != "后端开发工程师" {
		t.Fatalf("owner 职业未落库: %+v", oa)
	}
	alice, _ := svc.Persons.FindByName(ctx, 1, "Alice")
	if alice == nil || alice.Status != "pending" {
		t.Fatalf("Alice 应为 pending 人物: %+v", alice)
	}

	// 幂等：重跑同 session 全部 skip
	res2, err := svc.ExtractSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Apply.Skipped != 2 || res2.Apply.Active != 0 {
		t.Fatalf("重跑应全部 skip: %+v", res2.Apply)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/profile/ -run TestExtractSession -v
```
Expected: FAIL，`undefined: (*Service).ExtractSession`（编译错误）。

- [ ] **Step 3: 实现 extract_session.go**

`internal/profile/extract_session.go`：

```go
package profile

import (
	"context"
	"fmt"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/repo"
)

// ExtractResult 是一次 ExtractSession 的统计（trace/日志用）。
type ExtractResult struct {
	Apply   ApplyStats
	Windows int
	Tokens  int
}

// ExtractSession 对一个 session 跑完整画像抽取：读转写段（说话人名替换）→
// 聚合块 → LLM 抽取 → ApplyFacts 落库。pipeline profile stage 与 API 回填
// 端点共用此入口，逻辑只实现一次。
// 无有效文字的 session 直接返回零值（低价值不进抽取，同 extract stage）。
func (s *Service) ExtractSession(ctx context.Context, sessionID ids.ID) (ExtractResult, error) {
	var res ExtractResult
	ss, err := s.Sessions.Get(ctx, sessionID)
	if err != nil {
		return res, fmt.Errorf("读取 session: %w", err)
	}
	if ss == nil {
		return res, ErrNotFound
	}
	tr, err := s.Transcripts.GetBySession(ctx, sessionID)
	if err != nil {
		return res, fmt.Errorf("读取 transcript: %w", err)
	}
	segs, err := s.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return res, fmt.Errorf("读取 segments: %w", err)
	}
	// 说话人名替换（同 extract stage）：speaker_id → 已登记声纹名，LLM 才能区分谁说的
	if speakerNames, err := speakerNameMap(ctx, s.Speakers); err == nil {
		for i := range segs {
			if segs[i].SpeakerID != nil {
				if name, ok := speakerNames[*segs[i].SpeakerID]; ok {
					segs[i].SpeakerLabel = name
				}
			}
		}
	}
	blocks := memory.AggregateBlocks(segs, 30000)
	if len(blocks) == 0 {
		return res, nil
	}
	// 已知人物名单（稳定引用）
	persons, err := s.Persons.List(ctx, ss.UserID)
	if err != nil {
		return res, err
	}
	refs := make([]PersonRef, 0, len(persons))
	for _, p := range persons {
		refs = append(refs, PersonRef{ID: p.ID, Name: p.DisplayName, IsOwner: p.IsOwner})
	}
	ex := &Extractor{LLM: s.LLM, Model: s.Model, Prompt: s.Prompt, Window: s.Window}
	facts, err := ex.Extract(ctx, blocks, refs)
	if err != nil {
		return res, err
	}
	res.Windows, res.Tokens = ex.Stats().Windows, ex.Stats().Tokens
	st, err := s.ApplyFacts(ctx, sessionID, ss.UserID, facts)
	if err != nil {
		return res, err
	}
	res.Apply = st
	return res, nil
}

func speakerNameMap(ctx context.Context, speakers *repo.SpeakerRepo) (map[ids.ID]string, error) {
	list, err := speakers.List(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[ids.ID]string, len(list))
	for _, sp := range list {
		m[sp.ID] = sp.Name
	}
	return m, nil
}
```

注：`blockGapMS=30000` 与 stage_extract 的常量一致；profile 包不引 pipeline 包（会循环依赖），常量本地写死并注释出处。

- [ ] **Step 4: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/profile/ -v
```
Expected: 包内全部测试 PASS（catalog/fact/gate/extractor/apply/manual/extract_session）。

- [ ] **Step 5: Commit**

```bash
git add internal/profile/extract_session.go internal/profile/extract_session_test.go
git commit -m "feat(profile): Service.ExtractSession——stage 与回填共用的完整抽取入口"
```

---

### Task 14: pipeline——stage_profile + StageDeps/BuildStages 接线

**Files:**
- Create: `internal/pipeline/stage_profile.go`
- Modify: `internal/pipeline/stage_asr.go`（StageDeps 增 Profile 字段；BuildStages 注册）
- Create: `internal/pipeline/stage_profile_test.go`

- [ ] **Step 1: 写失败的集成测试**

`internal/pipeline/stage_profile_test.go`：

```go
package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
)

func TestStageProfile(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	speakers := &repo.SpeakerRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	svc := &profile.Service{
		DB: db,
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Speakers: speakers,
		Persons: persons, Attributes: &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db}, ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
		LLM: &fakeLLM{resps: []string{`{"facts":[
			{"plane":"attribute","subject":{"kind":"self"},"attr_key":"occupation","value":"工程师",
			 "confidence":0.9,"epistemic_type":"observed","block_index":1}
		]}`}},
		Model: "test", Prompt: "sys", PromptVersion: "profile_extraction_v1",
		Window: 10, Gate: profile.GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(ctx, persons, speakers); err != nil {
		t.Fatal(err)
	}

	// 最小 session 夹具
	sess := &repo.AudioSession{Source: "web_upload", Filename: "t.wav", StoragePath: "/tmp/t.wav", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sess.ID, Language: "zh-CN"}
	if err := svc.Transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := svc.Transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "我", Text: "我是一名后端工程师", StartMS: 0, EndMS: 3000},
	}); err != nil {
		t.Fatal(err)
	}

	// BuildStages 注册了 profile；handler 跑通并写 trace
	stages := BuildStages(StageDeps{Profile: svc})
	h, ok := stages["profile"]
	if !ok {
		t.Fatal("profile stage 未注册")
	}
	j := &repo.Job{}
	if err := h(ctx, j, sess.ID); err != nil {
		t.Fatal(err)
	}
	// trace 有 profile 条目
	var entries []repo.TraceEntry
	if err := json.Unmarshal(*j.Trace, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Stage != "profile" || entries[0].PromptVersion != "profile_extraction_v1" {
		t.Fatalf("trace 错误: %+v", entries)
	}
}

func TestStageProfileNilService(t *testing.T) {
	h := stageProfile(StageDeps{})
	if err := h(context.Background(), &repo.Job{}, ids.New()); err == nil {
		t.Fatal("nil service 应报错")
	}
}
```

注：`fakeLLM` 在 pipeline 包测试里已存在的话复用；不存在则在 `stage_profile_test.go` 里定义同款（按序弹响应）。检查 `internal/pipeline/stage_extract_test.go` 里是否已有 fake LLM 实现可复用（大概率有，名字可能不同——复用其名字，不要重复定义）。

- [ ] **Step 2: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/pipeline/ -run TestStageProfile -v
```
Expected: FAIL，`undefined: stageProfile`（编译错误）。

- [ ] **Step 3: 实现 stage_profile.go 并接线 StageDeps**

`internal/pipeline/stage_profile.go`：

```go
// stage_profile 是画像抽取 stage 的薄包装：完整逻辑在 profile.Service.ExtractSession
// （与 API 回填端点共用），这里只做调用与 trace 记录。
// stage 顺序：asr → segment → speaker → extract → profile（main.go 装配）。
package pipeline

import (
	"context"
	"fmt"
	"log"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
)

func stageProfile(d StageDeps) Handler {
	return func(ctx context.Context, j *repo.Job, sessionID ids.ID) error {
		if d.Profile == nil {
			return fmt.Errorf("stage profile: service 未装配")
		}
		begin := time.Now()
		res, err := d.Profile.ExtractSession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("profile: %w", err)
		}
		appendTrace(j, repo.TraceEntry{
			Stage: "profile", MS: msSince(begin),
			Model: d.Profile.Model, PromptVersion: d.Profile.PromptVersion,
			Tokens: res.Tokens, Windows: res.Windows,
			Error: fmt.Sprintf("facts=%d active=%d pending=%d 冲突=%d 佐证=%d 跳过=%d",
				res.Apply.Total, res.Apply.Active, res.Apply.Pending,
				res.Apply.Conflicts, res.Apply.Reaffirmed, res.Apply.Skipped),
		})
		log.Printf("[profile] session=%s facts=%d active=%d pending=%d windows=%d tokens=%d",
			sessionID, res.Apply.Total, res.Apply.Active, res.Apply.Pending, res.Windows, res.Tokens)
		return nil
	}
}
```

`internal/pipeline/stage_asr.go` 两处修改：

① StageDeps 结构体末尾（`VoiceprintThreshold` 之后）追加：

```go
	// ---- profile stage（用户画像 P1）----
	Profile *profile.Service // 画像编排服务（ExtractSession / 手动 CRUD / 确认队列）
```

并在 import 块加 `"zhiwei/internal/profile"`。

② BuildStages 的 map 里追加一行：

```go
		"profile": stageProfile(d),
```

- [ ] **Step 4: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/pipeline/ -run TestStageProfile -v
```
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/stage_profile.go internal/pipeline/stage_profile_test.go internal/pipeline/stage_asr.go
git commit -m "feat(profile): pipeline profile stage（薄包装，共用 Service.ExtractSession）"
```

---

### Task 15: config——ZW_PROFILE_* 配置项

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/config/config_test.go`（若已存在则追加测试函数；先查看现有测试文件名）

- [ ] **Step 1: 查看现有 config 测试**

```bash
ls internal/config/ && grep -n "func Test" internal/config/*_test.go
```
若已有 `config_test.go`，在其中追加测试函数；否则创建。

- [ ] **Step 2: 写失败的测试**

在 `internal/config/config_test.go`（新建或追加）：

```go
package config

import "testing"

func TestProfileDefaults(t *testing.T) {
	t.Setenv("ARK_API_KEY", "k")
	// 不设 ZW_PROFILE_* → 默认值
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ProfileAutoConfidence != 0.75 {
		t.Fatalf("ProfileAutoConfidence 默认应 0.75: %v", c.ProfileAutoConfidence)
	}
	if !c.ProfileExtractEnabled {
		t.Fatal("ProfileExtractEnabled 默认应 true")
	}
	if c.ProfileExtractWindow != 10 {
		t.Fatalf("ProfileExtractWindow 默认应 10: %v", c.ProfileExtractWindow)
	}
}

func TestProfileOverrides(t *testing.T) {
	t.Setenv("ARK_API_KEY", "k")
	t.Setenv("ZW_PROFILE_AUTO_CONFIDENCE", "0.9")
	t.Setenv("ZW_PROFILE_EXTRACT_ENABLED", "false")
	t.Setenv("ZW_PROFILE_EXTRACT_WINDOW", "20")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ProfileAutoConfidence != 0.9 || c.ProfileExtractEnabled || c.ProfileExtractWindow != 20 {
		t.Fatalf("覆盖未生效: %+v", c)
	}
}

func TestGetenvBool(t *testing.T) {
	for k, v := range map[string]bool{"1": true, "true": true, "TRUE": true, "0": false, "yes": false, "": false} {
		t.Setenv("ZW_TEST_BOOL", k)
		if getenvBool("ZW_TEST_BOOL", false) != v {
			t.Fatalf("getenvBool(%q) 期望 %v", k, v)
		}
	}
	if getenvBool("ZW_TEST_UNSET", true) != true {
		t.Fatal("未设置时应返回默认值 true")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

```bash
go test ./internal/config/ -run 'TestProfile|TestGetenvBool' -v
```
Expected: FAIL，`c.ProfileAutoConfidence` undefined（编译错误）。

- [ ] **Step 4: 实现 config 修改**

`internal/config/config.go`：

① struct 末尾（`EnrollMinDurationMS` 之后）追加：

```go
	// ---- profile stage（用户画像 P1）----
	ProfileAutoConfidence float64 // ZW_PROFILE_AUTO_CONFIDENCE：LLM 抽取自动写入 active 的置信阈值（默认 0.75）
	ProfileExtractEnabled bool    // ZW_PROFILE_EXTRACT_ENABLED：是否启用 profile 流水线阶段（默认 true）
	ProfileExtractWindow  int     // ZW_PROFILE_EXTRACT_WINDOW：抽取窗口大小（对话块数，默认 10）
```

② Load() 返回值里（`EnrollMinDurationMS` 行之后）追加：

```go
		// ---- profile stage ----
		ProfileAutoConfidence: getenvFloat("ZW_PROFILE_AUTO_CONFIDENCE", 0.75),
		ProfileExtractEnabled: getenvBool("ZW_PROFILE_EXTRACT_ENABLED", true),
		ProfileExtractWindow:  getenvInt("ZW_PROFILE_EXTRACT_WINDOW", 10),
```

③ 文件末尾追加 helper（import 块加 `"strings"`）：

```go
// getenvBool 读取布尔环境变量：1/true/TRUE（大小写不敏感）为 true，其余为 false；
// 未设置返回默认值。
func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true")
}
```

- [ ] **Step 5: 跑测试确认通过**

```bash
go test ./internal/config/ -v
```
Expected: PASS（含既有测试无回归）。

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(profile): ZW_PROFILE_AUTO_CONFIDENCE/EXTRACT_ENABLED/EXTRACT_WINDOW 配置"
```

---

### Task 16: api——人物/属性/关系/历史/确认队列/回填全部 handler

**Files:**
- Create: `internal/api/person.go`
- Create: `internal/api/person_test.go`

- [ ] **Step 1: 写失败的 API 集成测试**

`internal/api/person_test.go`：

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// profileTestLLM 是 api 包测试用的 fake LLM（回填端点测试时注入预置响应）。
type profileTestLLM struct{ resps []string }

func (f *profileTestLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	if len(f.resps) == 0 {
		return provider.ChatResponse{}, nil
	}
	r := f.resps[0]
	f.resps = f.resps[1:]
	return provider.ChatResponse{Content: r, TotalTokens: 10}, nil
}

func setupPersonAPI(t *testing.T) (http.Handler, *profile.Service) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.Init(1); err != nil {
		t.Fatal(err)
	}
	svc := &profile.Service{
		DB: db,
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Speakers: &repo.SpeakerRepo{DB: db},
		Persons: &repo.PersonRepo{DB: db}, Attributes: &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db}, ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
		LLM: &profileTestLLM{}, Model: "test", Prompt: "sys", Window: 10,
		Gate: profile.GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(context.Background(), svc.Persons, svc.Speakers); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	RegisterPerson(r, &PersonHandler{
		Persons: svc.Persons, Attributes: svc.Attributes,
		Relationships: svc.Relationships, ChangeLogs: svc.ChangeLogs, Service: svc,
	})
	return r, svc
}

func doReq(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPersonAPIFlow(t *testing.T) {
	h, svc := setupPersonAPI(t)
	ctx := context.Background()

	// 名册：至少有 owner「我」
	rec := doReq(t, h, "GET", "/api/persons", nil)
	if rec.Code != 200 {
		t.Fatalf("名册 500: %s", rec.Body.String())
	}
	var listR struct {
		Persons []repo.PersonWithPending `json:"persons"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listR)
	if len(listR.Persons) == 0 || !listR.Persons[0].IsOwner {
		t.Fatalf("名册应含 owner: %+v", listR.Persons)
	}

	// 新建人物
	rec = doReq(t, h, "POST", "/api/persons", map[string]any{"display_name": "张三"})
	if rec.Code != 200 {
		t.Fatalf("新建人物失败: %d %s", rec.Code, rec.Body.String())
	}
	var created repo.Person
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == 0 || created.DisplayName != "张三" {
		t.Fatalf("新建返回错误: %+v", created)
	}
	// 空名 → 400
	if rec := doReq(t, h, "POST", "/api/persons", map[string]any{"display_name": " "}); rec.Code != 400 {
		t.Fatalf("空名应 400: %d", rec.Code)
	}

	// 手动加属性（owner）
	owner, _ := svc.Persons.GetOwner(ctx, 1)
	rec = doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/attributes",
		map[string]any{"attr_key": "city", "value": "北京"})
	if rec.Code != 200 {
		t.Fatalf("加属性失败: %d %s", rec.Code, rec.Body.String())
	}
	var attr repo.PersonAttribute
	_ = json.Unmarshal(rec.Body.Bytes(), &attr)
	if attr.Status != "active" || attr.Source != "manual" {
		t.Fatalf("手动属性错误: %+v", attr)
	}

	// 详情：分组结构 + 属性在「基本」组
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String(), nil)
	if rec.Code != 200 {
		t.Fatalf("详情失败: %d", rec.Code)
	}
	var detail struct {
		Person *repo.Person            `json:"person"`
		Groups []map[string]any        `json:"groups"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail.Person == nil || detail.Person.ID != owner.ID {
		t.Fatalf("详情 person 错误: %+v", detail.Person)
	}
	found := false
	for _, g := range detail.Groups {
		if g["group"] == "基本" {
			found = true
		}
	}
	if !found {
		t.Fatalf("详情缺基本组: %+v", detail.Groups)
	}

	// 改属性值（PATCH = supersede + 新行）
	rec = doReq(t, h, "PATCH", "/api/persons/"+owner.ID.String()+"/attributes/"+attr.ID.String(),
		map[string]any{"attr_key": "city", "value": "上海"})
	if rec.Code != 200 {
		t.Fatalf("改属性失败: %d %s", rec.Code, rec.Body.String())
	}
	var attr2 repo.PersonAttribute
	_ = json.Unmarshal(rec.Body.Bytes(), &attr2)
	if attr2.ValueText != "上海" || attr2.SupersedesID == nil || *attr2.SupersedesID != attr.ID {
		t.Fatalf("改值应 supersede: %+v", attr2)
	}

	// 历史：应有 create + update 记录
	rec = doReq(t, h, "GET", "/api/persons/"+owner.ID.String()+"/history?entity_kind=attribute", nil)
	if rec.Code != 200 {
		t.Fatalf("历史失败: %d", rec.Code)
	}
	var hist struct {
		History []repo.PersonChangeLog `json:"history"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &hist)
	if len(hist.History) < 2 {
		t.Fatalf("历史至少 2 条: %d", len(hist.History))
	}

	// 关系
	rec = doReq(t, h, "POST", "/api/persons/"+owner.ID.String()+"/relationships",
		map[string]any{"relation_type": "朋友", "related_person_id": created.ID.String(), "label": "老张"})
	if rec.Code != 200 {
		t.Fatalf("加关系失败: %d %s", rec.Code, rec.Body.String())
	}

	// 确认队列：给 owner 塞一条 pending（直接走 Service），然后列队 + 确认
	_, err := svc.ApplyFacts(ctx, ids.New(), 1, []profile.Fact{
		{Plane: "attribute", Subject: profile.Subject{Kind: "self"}, AttrKey: "personality",
			Value: "沉稳", Confidence: 0.5, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, h, "GET", "/api/profile/pending", nil)
	if rec.Code != 200 {
		t.Fatalf("队列失败: %d %s", rec.Code, rec.Body.String())
	}
	var pend struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pend)
	if len(pend.Items) == 0 {
		t.Fatal("队列应有 pending")
	}
	item := pend.Items[len(pend.Items)-1]
	itemID, _ := item["id"].(string)
	rec = doReq(t, h, "POST", "/api/profile/pending/attribute/"+itemID+"/confirm", nil)
	if rec.Code != 200 {
		t.Fatalf("确认失败: %d %s", rec.Code, rec.Body.String())
	}

	// 回填：POST /api/profile/extract 带 session_id（session 无转写 → 0 facts 也算成功）
	sess := &repo.AudioSession{Source: "web_upload", Filename: "t.wav", StoragePath: "/tmp/x", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, h, "POST", "/api/profile/extract", map[string]any{"session_id": sess.ID.String()})
	if rec.Code != 200 {
		t.Fatalf("回填失败: %d %s", rec.Code, rec.Body.String())
	}
	// 非法 session_id → 400
	if rec := doReq(t, h, "POST", "/api/profile/extract", map[string]any{"session_id": "abc"}); rec.Code != 400 {
		t.Fatalf("非法 id 应 400: %d", rec.Code)
	}

	// 人物归档（DELETE = dismissed）
	if rec := doReq(t, h, "DELETE", "/api/persons/"+created.ID.String(), nil); rec.Code != 200 {
		t.Fatalf("归档失败: %d", rec.Code)
	}
	if p, _ := svc.Persons.Get(ctx, created.ID); p.Status != "dismissed" {
		t.Fatalf("归档后应 dismissed: %+v", p)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/api/ -run TestPersonAPIFlow -v
```
Expected: FAIL，`undefined: RegisterPerson`（编译错误）。

- [ ] **Step 3: 实现 person.go**

`internal/api/person.go`：

```go
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
)

// PersonHandler 人物画像 API：名册 CRUD、属性/关系手动管理、修改历史、
// 确认队列（跨平面 pending 并集）、按需回填抽取。
// 读操作直连 repo；一切变更走 profile.Service（保证审计+事务只实现一次）。
type PersonHandler struct {
	Persons       *repo.PersonRepo
	Attributes    *repo.PersonAttributeRepo
	Relationships *repo.PersonRelationshipRepo
	ChangeLogs    *repo.PersonChangeLogRepo
	Service       *profile.Service
}

func RegisterPerson(r chi.Router, h *PersonHandler) {
	r.Get("/api/persons", h.List)
	r.Post("/api/persons", h.Create)
	r.Get("/api/persons/{id}", h.Get)
	r.Patch("/api/persons/{id}", h.Patch)
	r.Delete("/api/persons/{id}", h.Delete)

	r.Post("/api/persons/{id}/attributes", h.AddAttribute)
	r.Patch("/api/persons/{id}/attributes/{aid}", h.PatchAttribute)
	r.Delete("/api/persons/{id}/attributes/{aid}", h.DeleteAttribute)
	r.Post("/api/persons/{id}/relationships", h.AddRelationship)
	r.Delete("/api/persons/{id}/relationships/{rid}", h.DeleteRelationship)
	r.Get("/api/persons/{id}/history", h.History)

	r.Get("/api/profile/pending", h.ListPending)
	r.Post("/api/profile/pending/{kind}/{id}/confirm", h.ConfirmPending)
	r.Post("/api/profile/pending/{kind}/{id}/dismiss", h.DismissPending)
	r.Post("/api/profile/extract", h.Extract)
}

// ---- 名册 ----

func (h *PersonHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.Persons.ListWithPending(r.Context(), 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"persons": list})
}

func (h *PersonHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName string  `json:"display_name"`
		SpeakerID   string  `json:"speaker_id"`
		Summary     *string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		http.Error(w, "display_name 不能为空", http.StatusBadRequest)
		return
	}
	var speakerID *ids.ID
	if req.SpeakerID != "" {
		id, err := ids.ParseID(req.SpeakerID)
		if err != nil {
			http.Error(w, "speaker_id 非法", http.StatusBadRequest)
			return
		}
		speakerID = &id
		// 换绑冲突：声纹已被别人绑定 → 409
		if p, err := h.Persons.GetBySpeaker(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if p != nil {
			http.Error(w, "该声纹已绑定人物「"+p.DisplayName+"」", http.StatusConflict)
			return
		}
	}
	p, err := h.Service.ManualCreatePerson(r.Context(), name, speakerID, req.Summary)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, p)
}

// attrGroup 详情页的属性分区。
type attrGroup struct {
	Group string                 `json:"group"`
	Attrs []repo.PersonAttribute `json:"attrs"`
}

type personDetailResp struct {
	Person           *repo.Person              `json:"person"`
	Groups           []attrGroup               `json:"groups"`
	Relationships    []repo.PersonRelationship `json:"relationships"`
	RecentSessionIDs []ids.ID                  `json:"recent_session_ids"`
	PendingCount     int                       `json:"pending_count"`
}

func (h *PersonHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	p, err := h.Persons.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
		return
	}
	attrs, err := h.Attributes.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rels, err := h.Relationships.ListByPerson(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sids, err := h.Persons.RecentSessionIDs(r.Context(), id, 10)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 分组：只展示 active+pending；组顺序按 GroupOrder，目录外 key 的「其他」最后。
	byGroup := map[string][]repo.PersonAttribute{}
	pending := 0
	for _, a := range attrs {
		if a.Status != "active" && a.Status != "pending" {
			continue
		}
		byGroup[profile.Def(a.AttrKey).Group] = append(byGroup[profile.Def(a.AttrKey).Group], a)
		if a.Status == "pending" {
			pending++
		}
	}
	groups := make([]attrGroup, 0, len(byGroup))
	for _, g := range profile.GroupOrder {
		if as := byGroup[g]; len(as) > 0 {
			groups = append(groups, attrGroup{Group: g, Attrs: as})
			delete(byGroup, g)
		}
	}
	relShown := make([]repo.PersonRelationship, 0, len(rels))
	for _, rel := range rels {
		if rel.Status != "active" && rel.Status != "pending" {
			continue
		}
		relShown = append(relShown, rel)
		if rel.Status == "pending" {
			pending++
		}
	}
	writeJSON(w, personDetailResp{
		Person: p, Groups: groups, Relationships: relShown,
		RecentSessionIDs: sids, PendingCount: pending,
	})
}

func (h *PersonHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		DisplayName string  `json:"display_name"`
		SpeakerID   *string `json:"speaker_id"` // nil=不改；""=解绑；"123"=换绑
		Summary     *string `json:"summary"`
		Status      string  `json:"status"` // 传了则走状态流转
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	p, err := h.Persons.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
		return
	}
	if req.Status != "" {
		if err := h.Service.ManualSetPersonStatus(r.Context(), id, req.Status); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	name := p.DisplayName
	if req.DisplayName != "" {
		name = strings.TrimSpace(req.DisplayName)
		if name == "" {
			http.Error(w, "display_name 不能为空", http.StatusBadRequest)
			return
		}
	}
	var speakerID *ids.ID
	if req.SpeakerID != nil && *req.SpeakerID != "" {
		sid, err := ids.ParseID(*req.SpeakerID)
		if err != nil {
			http.Error(w, "speaker_id 非法", http.StatusBadRequest)
			return
		}
		if other, err := h.Persons.GetBySpeaker(r.Context(), sid); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if other != nil && other.ID != id {
			http.Error(w, "该声纹已绑定人物「"+other.DisplayName+"」", http.StatusConflict)
			return
		}
		speakerID = &sid
	} else if req.SpeakerID != nil {
		// 传空串 = 解绑
		speakerID = nil
	} else {
		speakerID = p.SpeakerID // 不改
	}
	if err := h.Service.ManualUpdatePerson(r.Context(), id, name, speakerID, req.Summary); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *PersonHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	if p, err := h.Persons.Get(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if p == nil {
		http.Error(w, "人物不存在", http.StatusNotFound)
	}
	if err := h.Service.ManualSetPersonStatus(r.Context(), id, "dismissed"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 属性 ----

func (h *PersonHandler) AddAttribute(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		AttrKey string `json:"attr_key"`
		Value   string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(req.AttrKey)
	val := strings.TrimSpace(req.Value)
	if key == "" || val == "" {
		http.Error(w, "attr_key 与 value 必填", http.StatusBadRequest)
		return
	}
	a, err := h.Service.ManualAddAttribute(r.Context(), pid, key, val)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, a)
}

func (h *PersonHandler) PatchAttribute(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	aid, err := ids.ParseID(chi.URLParam(r, "aid"))
	if err != nil {
		http.Error(w, "aid 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		AttrKey string `json:"attr_key"`
		Value   string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.AttrKey) == "" || strings.TrimSpace(req.Value) == "" {
		http.Error(w, "attr_key 与 value 必填", http.StatusBadRequest)
		return
	}
	// 校验目标行存在且属于该人物
	a, err := h.Attributes.Get(r.Context(), aid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if a == nil || a.PersonID != pid {
		http.Error(w, "属性不存在", http.StatusNotFound)
		return
	}
	// 手动改值 = 同 key 写新值（ManualAddAttribute 内部 supersede 旧 active 行）
	na, err := h.Service.ManualAddAttribute(r.Context(), pid,
		strings.TrimSpace(req.AttrKey), strings.TrimSpace(req.Value))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, na)
}

func (h *PersonHandler) DeleteAttribute(w http.ResponseWriter, r *http.Request) {
	aid, err := ids.ParseID(chi.URLParam(r, "aid"))
	if err != nil {
		http.Error(w, "aid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteAttribute(r.Context(), aid); err != nil {
		if err == profile.ErrNotFound {
			http.Error(w, "属性不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 关系 ----

func (h *PersonHandler) AddRelationship(w http.ResponseWriter, r *http.Request) {
	pid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	var req struct {
		RelationType    string `json:"relation_type"`
		RelatedPersonID string `json:"related_person_id"`
		Direction       string `json:"direction"`
		OrgName         string `json:"org_name"`
		Label           string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest)
		return
	}
	if !profile.ValidRelations[req.RelationType] {
		http.Error(w, "relation_type 非法", http.StatusBadRequest)
		return
	}
	var related *ids.ID
	if req.RelatedPersonID != "" {
		rid, err := ids.ParseID(req.RelatedPersonID)
		if err != nil {
			http.Error(w, "related_person_id 非法", http.StatusBadRequest)
			return
		}
		related = &rid
	}
	rel, err := h.Service.ManualAddRelationship(r.Context(), pid, req.RelationType,
		related, req.Direction, req.OrgName, req.Label)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rel)
}

func (h *PersonHandler) DeleteRelationship(w http.ResponseWriter, r *http.Request) {
	rid, err := ids.ParseID(chi.URLParam(r, "rid"))
	if err != nil {
		http.Error(w, "rid 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ManualDeleteRelationship(r.Context(), rid); err != nil {
		if err == profile.ErrNotFound {
			http.Error(w, "关系不存在", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- 修改历史 ----

func (h *PersonHandler) History(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	logs, err := h.ChangeLogs.ListByPerson(r.Context(), id,
		r.URL.Query().Get("entity_kind"), r.URL.Query().Get("attr_key"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"history": logs})
}

// ---- 确认队列（跨平面 pending 并集）----

type pendingItem struct {
	Kind          string  `json:"kind"` // attribute|relationship|person
	ID            ids.ID  `json:"id"`
	PersonID      ids.ID  `json:"person_id"`
	PersonName    string  `json:"person_name"`
	AttrKey       string  `json:"attr_key,omitempty"`
	Value         string  `json:"value,omitempty"`         // attribute:建议值 / relationship:类型 / person:名字
	CurrentValue  string  `json:"current_value,omitempty"` // 冲突时的现值（supersedes 行）
	RelationType  string  `json:"relation_type,omitempty"`
	Label         string  `json:"label,omitempty"`
	Confidence    float64 `json:"confidence"`
	EpistemicType string  `json:"epistemic_type"`
	SessionID     *ids.ID `json:"session_id,omitempty"`
	SupersedesID  *ids.ID `json:"supersedes_id,omitempty"`
}

func (h *PersonHandler) ListPending(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	persons, err := h.Persons.List(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nameOf := make(map[ids.ID]string, len(persons))
	for _, p := range persons {
		nameOf[p.ID] = p.DisplayName
	}
	var items []pendingItem

	attrs, err := h.Attributes.ListPending(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, a := range attrs {
		it := pendingItem{
			Kind: "attribute", ID: a.ID, PersonID: a.PersonID, PersonName: nameOf[a.PersonID],
			AttrKey: a.AttrKey, Value: a.ValueText, Confidence: a.Confidence,
			EpistemicType: a.EpistemicType, SessionID: a.SessionID, SupersedesID: a.SupersedesID,
		}
		if a.SupersedesID != nil {
			if cur, err := h.Attributes.Get(ctx, *a.SupersedesID); err == nil && cur != nil {
				it.CurrentValue = cur.ValueText
			}
		}
		items = append(items, it)
	}

	rels, err := h.Relationships.ListPending(ctx, 1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, rel := range rels {
		label := ""
		if rel.Label != nil {
			label = *rel.Label
		}
		items = append(items, pendingItem{
			Kind: "relationship", ID: rel.ID, PersonID: rel.PersonID, PersonName: nameOf[rel.PersonID],
			RelationType: rel.RelationType, Value: rel.RelationType, Label: label,
			Confidence: rel.Confidence, EpistemicType: rel.EpistemicType,
			SessionID: rel.SessionID, SupersedesID: rel.SupersedesID,
		})
	}

	for _, p := range persons {
		if p.Status == "pending" {
			items = append(items, pendingItem{
				Kind: "person", ID: p.ID, PersonID: p.ID, PersonName: p.DisplayName,
				Value: p.DisplayName, Confidence: 0.5, EpistemicType: "observed",
			})
		}
	}
	writeJSON(w, map[string]any{"items": items})
}

func (h *PersonHandler) ConfirmPending(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.ConfirmPending(r.Context(), kind, id); err != nil {
		writePendingErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *PersonHandler) DismissPending(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "id 非法", http.StatusBadRequest)
		return
	}
	if err := h.Service.DismissPending(r.Context(), kind, id); err != nil {
		writePendingErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func writePendingErr(w http.ResponseWriter, err error) {
	if err == profile.ErrNotFound {
		http.Error(w, "记录不存在", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// ---- 按需回填抽取 ----

type extractResult struct {
	SessionID ids.ID `json:"session_id"`
	Facts     int    `json:"facts"`
	Active    int    `json:"active"`
	Pending   int    `json:"pending"`
	Reaffirmed int   `json:"reaffirmed"`
	Conflicts int    `json:"conflicts"`
	Skipped   int    `json:"skipped"`
	Windows   int    `json:"windows"`
	Tokens    int    `json:"tokens"`
	Error     string `json:"error,omitempty"`
}

// Extract 触发画像抽取：带 session_id 抽单个；不带则全量回填最近的 completed
// session（上限 50，防单请求过久）。同步执行（MVP 规模）。
func (h *PersonHandler) Extract(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // 空 body 合法
	ctx := r.Context()

	var sids []ids.ID
	if req.SessionID != "" {
		sid, err := ids.ParseID(req.SessionID)
		if err != nil {
			http.Error(w, "session_id 非法", http.StatusBadRequest)
			return
		}
		sids = []ids.ID{sid}
	} else {
		var err error
		sids, err = h.Service.Sessions.ListCompletedIDs(ctx, 50)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	results := make([]extractResult, 0, len(sids))
	for _, sid := range sids {
		res, err := h.Service.ExtractSession(ctx, sid)
		er := extractResult{SessionID: sid}
		if err != nil {
			er.Error = err.Error() // 单个失败不中断批量
		} else {
			er.Facts, er.Active, er.Pending = res.Apply.Total, res.Apply.Active, res.Apply.Pending
			er.Reaffirmed, er.Conflicts, er.Skipped = res.Apply.Reaffirmed, res.Apply.Conflicts, res.Apply.Skipped
			er.Windows, er.Tokens = res.Windows, res.Tokens
		}
		results = append(results, er)
	}
	writeJSON(w, map[string]any{"processed": len(results), "results": results})
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/api/ -run TestPersonAPIFlow -v
```
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/api/person.go internal/api/person_test.go
git commit -m "feat(profile): 人物/属性/关系/历史/确认队列/回填 REST API"
```

---

### Task 17: main.go 装配 + README + 全量回归

**Files:**
- Modify: `cmd/zhiwei-server/main.go`
- Modify: `README.md`

- [ ] **Step 1: main.go 装配**

`cmd/zhiwei-server/main.go` 修改点（按位置）：

① import 块加 `"zhiwei/internal/profile"`。

② `speakers := &repo.SpeakerRepo{DB: db}` 之后追加（repo 装配 + 启动幂等回填）：

```go
	persons := &repo.PersonRepo{DB: db}
	personAttrs := &repo.PersonAttributeRepo{DB: db}
	personRels := &repo.PersonRelationshipRepo{DB: db}
	personLogs := &repo.PersonChangeLogRepo{DB: db}
	// 画像回填：owner「我」+ speaker→person（幂等，见 repo.EnsurePersonBootstrap）
	if err := repo.EnsurePersonBootstrap(context.Background(), persons, speakers); err != nil {
		log.Fatal("画像 bootstrap 失败: ", err)
	}
```

③ `memoryConsolidateBytes` 读取之后追加（prompt 装配）：

```go
	// 画像抽取 prompt（版本化文件；版本号见文件名）
	profilePromptBytes, err := os.ReadFile("prompts/profile_extraction_v1.md")
	if err != nil {
		log.Fatal("读取画像抽取 prompt 失败: ", err)
	}
	profilePromptVersion := strings.TrimSuffix(filepath.Base("prompts/profile_extraction_v1.md"), ".md")
	profileSvc := &profile.Service{
		DB: db, Sessions: sessions, Transcripts: transcripts, Memories: memories,
		Speakers: speakers, Persons: persons, Attributes: personAttrs,
		Relationships: personRels, ChangeLogs: personLogs,
		LLM: llm, Model: cfg.LLMFastModel, Prompt: string(profilePromptBytes),
		PromptVersion: profilePromptVersion,
		Window: cfg.ProfileExtractWindow, Gate: profile.GateConfig{AutoConf: cfg.ProfileAutoConfidence},
	}
```

④ `BuildStages` 调用的 `StageDeps{...}` 里追加一行：

```go
		Profile: profileSvc,
```

⑤ flow 按开关追加 profile（替换现有 `flow := pipeline.Flow{Stages: []string{"asr", "segment", "speaker", "extract"}}`）：

```go
	// profile stage 按开关追加（ZW_PROFILE_EXTRACT_ENABLED=false 时仅手动+回填端点）
	stagesList := []string{"asr", "segment", "speaker", "extract"}
	if cfg.ProfileExtractEnabled {
		stagesList = append(stagesList, "profile")
	}
	flow := pipeline.Flow{Stages: stagesList}
```

⑥ API 注册区（`api.RegisterTopic` 之后）追加：

```go
	api.RegisterPerson(r, &api.PersonHandler{
		Persons: persons, Attributes: personAttrs, Relationships: personRels,
		ChangeLogs: personLogs, Service: profileSvc,
	})
```

- [ ] **Step 2: 编译 + 冒烟**

```bash
go build ./... && echo BUILD_OK
```
Expected: `BUILD_OK`。

- [ ] **Step 3: README 更新**

`README.md` 三处更新：

① 环境变量列表（`ZW_VOICEPRINT_THRESHOLD` 行后）追加：

```text
  - `ZW_PROFILE_AUTO_CONFIDENCE`（画像 LLM 抽取自动写入的置信阈值，默认 `0.75`）
  - `ZW_PROFILE_EXTRACT_ENABLED`（是否启用画像抽取流水线阶段，默认 `true`）
  - `ZW_PROFILE_EXTRACT_WINDOW`（画像抽取窗口大小，默认 `10`）
```

② API 一览末尾追加：

```text
GET/POST         /api/persons                        人物名册（含 pending 计数）/ 新建
GET/PATCH/DELETE /api/persons/{id}                   详情（分组属性+关系+最近互动）/ 改名·换绑声纹 / 归档
POST             /api/persons/{id}/attributes        手动加属性（source=manual, conf=1.0）
PATCH/DELETE     /api/persons/{id}/attributes/{aid}  手动改值（supersede）/ 删除
POST             /api/persons/{id}/relationships     手动加关系（配偶/子女/上下游/组织…）
DELETE           /api/persons/{id}/relationships/{rid}
GET              /api/persons/{id}/history           修改历史（?entity_kind=&attr_key= 过滤）
GET              /api/profile/pending                确认队列（属性/关系/人物 pending 并集）
POST             /api/profile/pending/{kind}/{id}/confirm|dismiss   确认/放弃
POST             /api/profile/extract                画像抽取/回填（可带 session_id；默认最近 50 个 completed）
```

③ 项目结构 `internal/pipeline/` 注释行下追加一行：

```text
internal/profile/    用户画像（人物系统）领域逻辑：属性目录/抽取/闸门/确认队列
```

- [ ] **Step 4: 全量回归**

```bash
make test && make test-integration
```
Expected: 全部 PASS（单元 + 集成，含既有功能无回归）。

- [ ] **Step 5: Commit**

```bash
git add cmd/zhiwei-server/main.go README.md
git commit -m "feat(profile): main 装配——bootstrap/Service/profile stage/人物 API"
```

---

## 计划自检（writing-plans Self-Review 结论）

1. **Spec 覆盖**：P1 范围内——person/attribute/relationship/change_log 四表（Task 1）、闸门五规则（Task 8+11）、人物归属四类指代（Task 11 resolveSubject）、幂等自然键（Task 3/4 查询 + Task 11/13 测试）、确认队列与冲突 supersede（Task 12）、手动 CRUD 带审计（Task 12）、profile stage + 回填端点（Task 13/14/16/17）、配置三参数（Task 15）。规格 §7 中「共同 Topic/相关 Todo」详情字段延到 P1b（前端一起做，避免后端先做无消费方的查询）——已在计划头部声明为 P1a/P1b 切分的一部分。
2. **占位符**：无 TBD/「适当处理」类步骤；所有代码步骤含完整代码。
3. **类型一致性**：repo 方法名（CreateExt/Get/List*/FindActiveByKey*/FindByNaturalKeyExt/SetStatus*/BumpConfidence*/ListPending）、profile 导出符号（Def/IsList/GroupOrder/ParseFacts/DecideAttribute/DecideRelationship/Service.ApplyFacts/ExtractSession/Manual*/ConfirmPending/DismissPending/ErrNotFound/ValidRelations）、StageDeps.Profile 在各任务间一致；`fakeLLM` 每包只定义一次并已注明。

## 执行交接

计划已保存至 `docs/superpowers/plans/2026-08-24-person-profile-p1a-backend.md`。两种执行方式：

1. **Subagent-Driven（推荐）**——每任务派一个全新 subagent 执行，任务间我做两阶段 review，迭代快
2. **Inline Execution**——本会话内按 executing-plans 批量执行，带 checkpoint

选哪种？
