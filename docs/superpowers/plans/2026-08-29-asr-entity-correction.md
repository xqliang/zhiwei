# ASR 实体纠错（专有名词后处理纠正）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在流水线 `asr` 与 `segment` 之间新增 `correct` stage，用实体知识库（人名/花名/宠物/项目/话题/待办 + 手动自定义）对 ASR 转写做后置实体纠错，达阈值自动改写并标记（`corrected_reason='entity'` + `entity_edits` 明细，前端展示原文→纠正对照）。

**Architecture:** 拼音/音素召回 Top-K 候选白名单（`go-pinyin` 归一化 + `go-edlib` Jaro-Winkler 相似度）→ LLM 一程裁决（`prompts/asr_correction_v1.md`，只允许替换白名单内实体）→ 双重门控（置信度 + orig 原样在段内）后局部替换。实体库 `entity_kb` 每次刷新重建 auto 条目、保留 manual；设置页「专有名词」子区管理开关/阈值/手动实体。设计 spec：`docs/superpowers/specs/2026-08-29-asr-entity-correction-design.md`。

**Tech Stack:** Go（chi + sqlx + golang-migrate）、`github.com/mozillazg/go-pinyin` v0.21.0、`github.com/hbollon/go-edlib` v1.7.0、Vue3 CDN（无打包，改 `app.js` 后 `make hash-web`）。

**关键约束（全任务通用）：**
- 本工作树调试/测试库：`repotest.DSN(t)` 自动隔离到 `zhiwei_test_<pkg>`（`make test-integration` 起容器并设 `TEST_MYSQL_DSN`）；**不要碰共享 `zhiwei` 库**。
- 迁移号 **000025**（main 已到 000024）。`transcript_segment` 加列必须与 `TranscriptSegment` struct 字段同任务落地（sqlx safe 模式：`SELECT *` 遇到无字段列会扫描报错，拆开会让全库测试红）。
- 单元测试（无 DB）直接 `go test`；集成测试无 `TEST_MYSQL_DSN` 时自动 skip。
- 注释用中文、写给新人看（项目惯例）。

---

### Task 1: 引入拼音与相似度依赖

**Files:**
- Modify: `go.mod` / `go.sum`（经 `go get`）

- [ ] **Step 1: 拉取依赖**

```bash
go get github.com/mozillazg/go-pinyin@v0.21.0 github.com/hbollon/go-edlib@v1.7.0
go mod tidy
```

- [ ] **Step 2: 验证编译**

Run: `go build ./...`
Expected: 无输出（成功）

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "chore(deps): 引入 go-pinyin + go-edlib（实体纠错：拼音归一化 + 发音相似度）"
```

---

### Task 2: 迁移 000025 + TranscriptSegment.EntityEdits 字段

**Files:**
- Create: `migrations/000025_entity_kb.up.sql`
- Create: `migrations/000025_entity_kb.down.sql`
- Modify: `internal/repo/transcript.go`（TranscriptSegment 加 EntityEdits 字段）

- [ ] **Step 1: 写 up 迁移**

`migrations/000025_entity_kb.up.sql`：

```sql
-- 实体知识库（ASR 实体纠错用）：每用户一份。canonical=规范名（纠正目标）。
-- source: auto=流水线刷新时从 person/pet/topic/todo 等同步重建；manual=设置页手动录入（刷新不动）。
-- pinyin=归一化拼音（小写无声调、音节空格分隔，CJK 匹配键）；metaphone=拉丁名/代号的
-- 归一化形（仅小写字母数字，拉丁匹配键）——本列不再用 Double Metaphone 算法（无成熟
-- 维护的 Go 实现），拉丁相似度直接走 Jaro-Winkler（见 internal/entity/phonetic.go）。
CREATE TABLE entity_kb (
  id         BIGINT UNSIGNED NOT NULL,
  user_id    BIGINT UNSIGNED NOT NULL,
  canonical  VARCHAR(128) NOT NULL,
  kind       VARCHAR(32)  NOT NULL,
  pinyin     VARCHAR(256) NULL,
  metaphone  VARCHAR(64)  NULL,
  source     VARCHAR(16)  NOT NULL DEFAULT 'auto',
  source_ref VARCHAR(64)  NULL,
  enabled    TINYINT(1)   NOT NULL DEFAULT 1,
  note       VARCHAR(256) NULL,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_entity_kb (user_id, canonical, kind),
  KEY idx_entity_kb_user (user_id, enabled)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 实体纠错功能配置（每用户一份）。
-- auto_sources: JSON 数组，自动入库的 kind 列表（如 ["person","pet","project","task","topic","speaker"]）。
CREATE TABLE entity_settings (
  user_id              BIGINT UNSIGNED NOT NULL,
  correction_enabled   TINYINT(1)   NOT NULL DEFAULT 1,
  confidence_threshold DECIMAL(3,2) NOT NULL DEFAULT 0.80,
  auto_sources         JSON NULL,
  updated_at           TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 转写段实体纠错明细：该段被应用的纠正数组
-- [{orig, corrected, canonical, confidence}]，配合 corrected_reason='entity'（徽章+对照展示）。
ALTER TABLE transcript_segment
  ADD COLUMN entity_edits JSON NULL COMMENT '实体纠错明细数组';
```

- [ ] **Step 2: 写 down 迁移**

`migrations/000025_entity_kb.down.sql`：

```sql
ALTER TABLE transcript_segment DROP COLUMN entity_edits;
DROP TABLE IF EXISTS entity_settings;
DROP TABLE IF EXISTS entity_kb;
```

- [ ] **Step 3: TranscriptSegment 加 EntityEdits 字段**

`internal/repo/transcript.go` 的 `TranscriptSegment` struct，在 `CorrectedReason` 字段注释块后（`Embedding` 之前）插入：

```go
	// EntityEdits 该段的实体纠错明细 JSON（000025 迁移加列）：数组
	// [{orig, corrected, canonical, confidence}]，由 correct stage 应用纠正时写入，
	// 前端转写详情据此渲染「原文(删除线)→纠正后」对照。未纠正段为 NULL。
	// json:"-" 不外泄，API 层按需转成明文数组（同 Embedding 的处理方式）。
	EntityEdits []byte `db:"entity_edits" json:"-"`
```

- [ ] **Step 4: 验证（迁移被 repotest 自动嵌入，跑既有集成测试即验证加列+字段对齐）**

Run: `make test-integration 2>&1 | tail -20`
Expected: 全部 PASS（`internal/repo` 等包的 `SELECT *` 扫描不报 entity_edits 缺字段错）。
若本机 Docker/MySQL 已在跑，也可 `TEST_MYSQL_DSN=... go test ./internal/repo/ -run TestTranscript -v` 抽查。

- [ ] **Step 5: Commit**

```bash
git add migrations/000025_entity_kb.up.sql migrations/000025_entity_kb.down.sql internal/repo/transcript.go
git commit -m "feat(repo): 迁移 000025 entity_kb/entity_settings/entity_edits 列 + TranscriptSegment 字段"
```

---

### Task 3: repo 层——EntityKBRepo / EntitySettingsRepo

**Files:**
- Create: `internal/repo/entity.go`
- Test: `internal/repo/entity_test.go`

- [ ] **Step 1: 写失败测试**

`internal/repo/entity_test.go`：

```go
package repo

import (
	"context"
	"testing"

	"zhiwei/internal/repotest"
)

// TestEntityKBRepo 集成测试：ReplaceAuto 重建 auto（删旧+落新、manual 不动）、
// manual CRUD、ListEnabled 过滤、CountByKind 统计。
func TestEntityKBRepo(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &EntityKBRepo{DB: db}
	const uid int64 = 1
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid) })
	_, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid)

	// 1) auto 批量落库 + ListEnabled 读回（enabled 默认 true）。
	auto1 := []Entity{
		{UserID: uid, Canonical: "张梦瑜", Kind: EntityKindPerson, Source: EntitySourceAuto, Pinyin: sp("zhang meng yu"), SourceRef: sp("person:1")},
		{UserID: uid, Canonical: "阿黄", Kind: EntityKindPet, Source: EntitySourceAuto, Pinyin: sp("a huang")},
	}
	if err := r.ReplaceAuto(ctx, uid, EntityKindPerson, auto1[:1]); err != nil {
		t.Fatalf("ReplaceAuto person: %v", err)
	}
	if err := r.ReplaceAuto(ctx, uid, EntityKindPet, auto1[1:]); err != nil {
		t.Fatalf("ReplaceAuto pet: %v", err)
	}
	got, err := r.ListEnabled(ctx, uid)
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条实体, got %d", len(got))
	}

	// 2) 再 ReplaceAuto(person)：旧 person auto 全删重建（张梦瑜→王芳），pet 不受影响。
	auto2 := []Entity{{UserID: uid, Canonical: "王芳", Kind: EntityKindPerson, Source: EntitySourceAuto, Pinyin: sp("wang fang")}}
	if err := r.ReplaceAuto(ctx, uid, EntityKindPerson, auto2); err != nil {
		t.Fatalf("ReplaceAuto 重建: %v", err)
	}
	got, _ = r.ListEnabled(ctx, uid)
	if len(got) != 2 {
		t.Fatalf("重建后应仍是 2 条（person 换 1 条 + pet 1 条）, got %d", len(got))
	}
	names := map[string]bool{}
	for _, e := range got {
		names[e.Canonical] = true
	}
	if names["张梦瑜"] || !names["王芳"] || !names["阿黄"] {
		t.Fatalf("重建结果不对: %v", names)
	}

	// 3) manual CRUD：创建/读回/改名/禁用/删除；auto 条目不能被 UpdateManual 改。
	m := &Entity{UserID: uid, Canonical: "天枢项目", Kind: EntityKindCustom, Source: EntitySourceManual, Pinyin: sp("tian shu xiang mu"), Note: sp("内部代号 TS")}
	if err := r.CreateManual(ctx, m); err != nil {
		t.Fatalf("CreateManual: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("CreateManual 应回填 id")
	}
	full, err := r.Get(ctx, uid, m.ID)
	if err != nil || full.Canonical != "天枢项目" || full.Source != EntitySourceManual {
		t.Fatalf("Get: %v %+v", err, full)
	}
	if err := r.UpdateManual(ctx, uid, m.ID, "天璇项目", "内部代号 TX"); err != nil {
		t.Fatalf("UpdateManual: %v", err)
	}
	if full, _ = r.Get(ctx, uid, m.ID); full.Canonical != "天璇项目" {
		t.Fatalf("改名后应读回天璇项目: %+v", full)
	}
	// auto 条目被 UpdateManual → sql.ErrNoRows 语义（应报错而非静默成功）。
	autoID := got[0].ID
	if err := r.UpdateManual(ctx, uid, autoID, "xxx", ""); err == nil {
		t.Fatal("UpdateManual 不应能改 auto 条目")
	}
	if err := r.SetEnabled(ctx, uid, m.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if list, _ := r.ListEnabled(ctx, uid); len(list) != 2 {
		t.Fatalf("禁用后 ListEnabled 应只剩 2 条, got %d", len(list))
	}
	if err := r.Delete(ctx, uid, m.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, uid, m.ID); err == nil {
		t.Fatal("删除后 Get 应报错")
	}

	// 4) CountByKind：按 kind 统计 enabled 条数（设置页汇总用）。
	counts, err := r.CountByKind(ctx, uid)
	if err != nil {
		t.Fatalf("CountByKind: %v", err)
	}
	if counts[EntityKindPerson] != 1 || counts[EntityKindPet] != 1 {
		t.Fatalf("计数不对: %v", counts)
	}
}

// TestEntitySettingsRepo：默认值（无行时零值+默认）、Upsert 读回。
func TestEntitySettingsRepo(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &EntitySettingsRepo{DB: db}
	const uid int64 = 2 // 与 TestEntityKBRepo 隔离（同包并行库不同行即可）
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM entity_settings WHERE user_id = ?", uid) })
	_, _ = db.Exec("DELETE FROM entity_settings WHERE user_id = ?", uid)

	// 1) 无行 → 默认值（enabled=true、threshold=0.8、auto_sources=全量 kinds）。
	s, err := r.Get(ctx, uid)
	if err != nil {
		t.Fatalf("Get 默认: %v", err)
	}
	if !s.CorrectionEnabled || s.ConfidenceThreshold != 0.8 {
		t.Fatalf("默认值不对: %+v", s)
	}
	if len(s.AutoSources) != 6 {
		t.Fatalf("默认 auto_sources 应为 6 种 kind: %v", s.AutoSources)
	}

	// 2) Upsert 后读回。
	if err := r.Upsert(ctx, uid, false, 0.9, []string{"person", "pet"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	s, _ = r.Get(ctx, uid)
	if s.CorrectionEnabled || s.ConfidenceThreshold != 0.9 || len(s.AutoSources) != 2 {
		t.Fatalf("读回不符: %+v", s)
	}
	// 3) 阈值越界被拒绝（DB DECIMAL(3,2) 也兜不住 >9.99，应用层先挡）。
	if err := r.Upsert(ctx, uid, true, 1.5, nil); err == nil {
		t.Fatal("阈值 1.5 应被拒绝")
	}
}

// sp 字符串转指针的测试辅助（pinyin/note/source_ref 是 *string 列）。
func sp(s string) *string { return &s }
```

- [ ] **Step 2: 跑测试确认编译失败**

Run: `go test ./internal/repo/ -run TestEntityKBRepo -v`
Expected: 编译失败 `undefined: EntityKBRepo`（未实现）。

- [ ] **Step 3: 实现 internal/repo/entity.go**

```go
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// 实体 kind 枚举（entity_kb.kind）。custom=设置页手动录入的专有名词。
const (
	EntityKindPerson  = "person"  // 人物：person.display_name + 别名/称呼
	EntityKindPet     = "pet"     // 宠物：person_pet.name + nickname
	EntityKindProject = "project" // 项目：person_attribute(current_projects)
	EntityKindTask    = "task"    // 待办：todo.title（未关闭）
	EntityKindTopic   = "topic"   // 话题：topic.name（active）
	EntityKindSpeaker = "speaker" // 已登记说话人名：speaker.name（非随机名）
	EntityKindCustom  = "custom"  // 手动自定义
)

// entity kinds 全量清单（entity_settings.auto_sources 的默认值）。
var AllEntityKinds = []string{
	EntityKindPerson, EntityKindPet, EntityKindProject, EntityKindTask, EntityKindTopic, EntityKindSpeaker,
}

// 实体来源枚举（entity_kb.source）。
const (
	EntitySourceAuto   = "auto"   // 流水线刷新重建（ReplaceAuto 全删全落）
	EntitySourceManual = "manual" // 设置页手动录入，刷新不动
)

// Entity 实体知识库一行（ASR 实体纠错的纠正目标）。
type Entity struct {
	ID        ids.ID    `db:"id" json:"id"`
	UserID    int64     `db:"user_id" json:"user_id"`
	Canonical string    `db:"canonical" json:"canonical"`
	Kind      string    `db:"kind" json:"kind"`
	Pinyin    *string   `db:"pinyin" json:"pinyin,omitempty"`
	Metaphone *string   `db:"metaphone" json:"metaphone,omitempty"`
	Source    string    `db:"source" json:"source"`
	SourceRef *string   `db:"source_ref" json:"source_ref,omitempty"`
	Enabled   bool      `db:"enabled" json:"enabled"`
	Note      *string   `db:"note" json:"note,omitempty"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// EntityKBRepo 实体知识库存取。所有方法都带 user_id 作用域（多用户隔离）。
type EntityKBRepo struct{ DB *sqlx.DB }

// ListEnabled 读某用户全部启用实体（correct stage 每会话一次；实体量级几十~几百，无需分页）。
func (r *EntityKBRepo) ListEnabled(ctx context.Context, userID int64) ([]Entity, error) {
	var list []Entity
	err := r.DB.SelectContext(ctx, &list,
		`SELECT id, user_id, canonical, kind, pinyin, metaphone, source, source_ref, enabled, note, created_at, updated_at
		 FROM entity_kb WHERE user_id = ? AND enabled = 1 ORDER BY kind, canonical`, userID)
	return list, err
}

// List 按条件列实体（设置页用）：kind 空串=全部，含禁用行。
func (r *EntityKBRepo) List(ctx context.Context, userID int64, kind string) ([]Entity, error) {
	var list []Entity
	q := `SELECT id, user_id, canonical, kind, pinyin, metaphone, source, source_ref, enabled, note, created_at, updated_at
	      FROM entity_kb WHERE user_id = ?`
	args := []any{userID}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY source, kind, canonical`
	err := r.DB.SelectContext(ctx, &list, q, args...)
	return list, err
}

// Get 读单条（带 user_id 作用域；不存在返回 sql.ErrNoRows）。
func (r *EntityKBRepo) Get(ctx context.Context, userID int64, id ids.ID) (*Entity, error) {
	var e Entity
	err := r.DB.GetContext(ctx, &e,
		`SELECT id, user_id, canonical, kind, pinyin, metaphone, source, source_ref, enabled, note, created_at, updated_at
		 FROM entity_kb WHERE user_id = ? AND id = ?`, userID, id.Int64())
	return &e, err
}

// ReplaceAuto 原子重建某用户某 kind 的全部 auto 实体：事务内删旧 auto（该 kind）→ 落新。
// manual 条目（任何 kind）不动。len(list)==0 也执行删除（来源行已清空时同步清掉残留）。
// 入参实体的 ID/pinyin 由调用方负责（种子层算拼音，id 在此回填）。
func (r *EntityKBRepo) ReplaceAuto(ctx context.Context, userID int64, kind string, list []Entity) error {
	if !validEntityKind(kind) {
		return fmt.Errorf("非法实体 kind: %q", kind)
	}
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity_kb WHERE user_id = ? AND kind = ? AND source = 'auto'`, userID, kind); err != nil {
		return err
	}
	for i := range list {
		list[i].ID = ids.New()
		list[i].UserID = userID
		list[i].Kind = kind
		list[i].Source = EntitySourceAuto
		if _, err := tx.NamedExecContext(ctx, `
INSERT INTO entity_kb (id, user_id, canonical, kind, pinyin, metaphone, source, source_ref, enabled, note)
VALUES (:id, :user_id, :canonical, :kind, :pinyin, :metaphone, :source, :source_ref, :enabled, :note)`, list[i]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreateManual 设置页手动新增专有名词（kind=custom 或指定 kind）。回填 ID。
func (r *EntityKBRepo) CreateManual(ctx context.Context, e *Entity) error {
	if !validEntityKind(e.Kind) {
		return fmt.Errorf("非法实体 kind: %q", e.Kind)
	}
	e.ID = ids.New()
	e.Source = EntitySourceManual
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO entity_kb (id, user_id, canonical, kind, pinyin, metaphone, source, source_ref, enabled, note)
VALUES (:id, :user_id, :canonical, :kind, :pinyin, :metaphone, :source, :source_ref, :enabled, :note)`, e)
	return err
}

// UpdateManual 改手动实体的规范名/备注（kind/pinyin 保持创建时口径，canonical 变了拼音
// 由 handler 层重算后一并写）。只能改 manual 条目；auto 条目（刷新重建）返回错误。
func (r *EntityKBRepo) UpdateManual(ctx context.Context, userID, id ids.ID, canonical, note string) error {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE entity_kb SET canonical = ?, note = ? WHERE user_id = ? AND id = ? AND source = 'manual'`,
		canonical, nullStr(note), userID, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetEnabled 单条启禁（manual/auto 均可——auto 也可临时禁掉不参与纠错，刷新不覆盖 enabled）。
func (r *EntityKBRepo) SetEnabled(ctx context.Context, userID, id ids.ID, enabled bool) error {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE entity_kb SET enabled = ? WHERE user_id = ? AND id = ?`, enabled, userID, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete 删除实体（manual 删除后消失；auto 删除后下次刷新会回来——想禁用用 SetEnabled）。
func (r *EntityKBRepo) Delete(ctx context.Context, userID, id ids.ID) error {
	res, err := r.DB.ExecContext(ctx,
		`DELETE FROM entity_kb WHERE user_id = ? AND id = ?`, userID, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountByKind 按 kind 统计启用条数（设置页「自动入库来源」汇总用）。
func (r *EntityKBRepo) CountByKind(ctx context.Context, userID int64) (map[string]int, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT kind, COUNT(*) FROM entity_kb WHERE user_id = ? AND enabled = 1 GROUP BY kind`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		m[k] = n
	}
	return m, rows.Err()
}

func validEntityKind(k string) bool {
	switch k {
	case EntityKindPerson, EntityKindPet, EntityKindProject, EntityKindTask, EntityKindTopic, EntityKindSpeaker, EntityKindCustom:
		return true
	}
	return false
}

// nullStr 空串转 NULL（可空列语义：没填 = NULL）。
func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// EntitySettings 实体纠错功能配置（每用户一行，无行=默认值）。
type EntitySettings struct {
	UserID              int64     `db:"user_id" json:"user_id"`
	CorrectionEnabled   bool      `db:"correction_enabled" json:"correction_enabled"`
	ConfidenceThreshold float64   `db:"confidence_threshold" json:"confidence_threshold"`
	AutoSources         []string  `db:"auto_sources" json:"auto_sources"`
	UpdatedAt           time.Time `db:"updated_at" json:"updated_at"`
}

// EntitySettingsRepo 实体纠错配置存取。
type EntitySettingsRepo struct{ DB *sqlx.DB }

// defaults 返回无行时的默认配置（enabled + 0.8 + 全量 kinds）。
func defaultEntitySettings(userID int64) EntitySettings {
	return EntitySettings{
		UserID:              userID,
		CorrectionEnabled:   true,
		ConfidenceThreshold: 0.8,
		AutoSources:         append([]string(nil), AllEntityKinds...),
	}
}

// Get 读配置；从未配置（无行）返回默认值而非错误（correct stage 直接可用）。
func (r *EntitySettingsRepo) Get(ctx context.Context, userID int64) (*EntitySettings, error) {
	var s EntitySettings
	var sources []byte
	err := r.DB.QueryRowxContext(ctx,
		`SELECT user_id, correction_enabled, confidence_threshold, auto_sources, updated_at
		 FROM entity_settings WHERE user_id = ?`, userID).
		Scan(&s.UserID, &s.CorrectionEnabled, &s.ConfidenceThreshold, &sources, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		d := defaultEntitySettings(userID)
		return &d, nil
	}
	if err != nil {
		return nil, err
	}
	if len(sources) > 0 {
		_ = json.Unmarshal(sources, &s.AutoSources) // 库里脏数据不致命：退化为空数组走默认 kinds
	}
	if len(s.AutoSources) == 0 {
		s.AutoSources = append([]string(nil), AllEntityKinds...)
	}
	return &s, nil
}

// Upsert 写配置（单用户一行）。threshold 越界（∉[0,1]）在应用层拒绝。
// sources nil/空 = 恢复全量默认。
func (r *EntitySettingsRepo) Upsert(ctx context.Context, userID int64, enabled bool, threshold float64, sources []string) error {
	if threshold < 0 || threshold > 1 {
		return fmt.Errorf("置信度阈值须在 [0,1]，got %v", threshold)
	}
	if len(sources) == 0 {
		sources = append([]string(nil), AllEntityKinds...)
	}
	for _, k := range sources {
		if !validEntityKind(k) || k == EntityKindCustom {
			return fmt.Errorf("auto_sources 不支持 kind: %q", k)
		}
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	_, err = r.DB.ExecContext(ctx, `
INSERT INTO entity_settings (user_id, correction_enabled, confidence_threshold, auto_sources)
VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE correction_enabled = VALUES(correction_enabled),
  confidence_threshold = VALUES(confidence_threshold), auto_sources = VALUES(auto_sources)`,
		userID, enabled, threshold, raw)
	return err
}
```

注意：文件头 import 需补 `"encoding/json"` 和 `"time"`（上面 struct 用到 `time.Time`）。import 块完整版：

```go
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)
```

另：`nullStr` 若与包内既有辅助重名，改名为 `entityNullStr` 并同步 UpdateManual 调用处。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/repo/ -run 'TestEntityKBRepo|TestEntitySettingsRepo' -v`
Expected: PASS（需 `TEST_MYSQL_DSN`；未设则 SKIP——用 `make test-integration` 或先 `make compose-up init-testdb`）。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/entity.go internal/repo/entity_test.go
git commit -m "feat(repo): EntityKBRepo/EntitySettingsRepo——实体知识库与纠错配置存取"
```
---

### Task 4: repo 层——TranscriptRepo.ApplyEntityCorrections

**Files:**
- Modify: `internal/repo/transcript.go`（追加方法）
- Test: `internal/repo/transcript_test.go`（追加用例；若该文件不存在则新建）

- [ ] **Step 1: 写失败测试**

在 `internal/repo/transcript_test.go` 追加：

```go
// TestApplyEntityCorrections 验证实体纠错落库：text 改写 + corrected_reason='entity' +
// entity_edits JSON + 不动 speaker 归属；RecomputeFullText 后 full_text 反映纠正结果。
func TestApplyEntityCorrections(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &TranscriptRepo{DB: db}
	const uid int64 = 1
	t.Cleanup(func() { _, _ = db.Exec(`DELETE tr, seg FROM transcript tr JOIN transcript_segment seg ON seg.transcript_id = tr.id WHERE tr.user_id = ?`, uid) })

	tr := &Transcript{UserID: uid, SessionID: ids.New(), Language: "zh-CN"}
	if err := r.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	segs := []TranscriptSegment{{
		TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1",
		Text: "长梦鱼你看到我的邮件了吗", StartMS: 0, EndMS: 3000,
	}}
	if err := r.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	segID := segs[0].ID

	edits := `[{"orig":"长梦鱼","corrected":"张梦瑜","canonical":"张梦瑜","confidence":0.92}]`
	if err := r.ApplyEntityCorrections(ctx, tr.ID, segID, "张梦瑜你看到我的邮件了吗", []byte(edits)); err != nil {
		t.Fatalf("ApplyEntityCorrections: %v", err)
	}
	after, err := r.GetSegment(ctx, segID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Text != "张梦瑜你看到我的邮件了吗" {
		t.Fatalf("text 未改写: %q", after.Text)
	}
	if after.CorrectedReason == nil || *after.CorrectedReason != "entity" {
		t.Fatalf("corrected_reason 应为 entity: %v", after.CorrectedReason)
	}
	if string(after.EntityEdits) != edits {
		t.Fatalf("entity_edits 不符: %s", after.EntityEdits)
	}
	if after.SpeakerID != nil {
		t.Fatal("实体纠错不应动 speaker_id")
	}

	// RecomputeFullText 反映纠正后的文本。
	if err := r.RecomputeFullText(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}
	full, err := r.GetBySession(ctx, tr.SessionID)
	if err != nil || full.FullText == nil || !strings.Contains(*full.FullText, "张梦瑜") {
		t.Fatalf("full_text 应含纠正后文本: %v %v", err, full.FullText)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run TestApplyEntityCorrections -v`
Expected: 编译失败 `r.ApplyEntityCorrections undefined`。

- [ ] **Step 3: 实现（transcript.go 追加）**

在 `RecomputeFullText` 方法后追加：

```go
// ApplyEntityCorrections 实体纠错落库（correct stage 用）：单条 UPDATE 原子写
// text + corrected_reason='entity' + entity_edits 明细。带 transcript_id 作用域
// 防跨会话误写；edits 为 nil 时 entity_edits 置 NULL（语义：无明细）。
// 与声纹纠正共用 corrected_reason 列（前端按值区分 tooltip），corrected_from_speaker_id
// 不动（实体纠错与说话人归属无关）。单行 UPDATE 原子、并发安全。
func (r *TranscriptRepo) ApplyEntityCorrections(ctx context.Context, transcriptID, segID ids.ID, newText string, edits []byte) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET text = ?, corrected_reason = 'entity', entity_edits = ? WHERE id = ? AND transcript_id = ?`,
		newText, edits, segID.Int64(), transcriptID.Int64())
	return err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/repo/ -run TestApplyEntityCorrections -v`
Expected: PASS（或无 DSN 时 SKIP）。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/transcript.go internal/repo/transcript_test.go
git commit -m "feat(repo): ApplyEntityCorrections——实体纠错原子落库(text+reason+edits)"
```

---

### Task 5: internal/entity 包——拼音归一化与发音相似度

**Files:**
- Create: `internal/entity/phonetic.go`
- Test: `internal/entity/phonetic_test.go`

- [ ] **Step 1: 写失败测试**

`internal/entity/phonetic_test.go`：

```go
package entity

import "testing"

// TestNormalizePinyin 拼音归一化：中文逐字转无声调拼音空格分隔；连续 ASCII 字母/数字
// 归并为一个词（大小写不敏感）；标点/空白丢弃；空串返回空串。
func TestNormalizePinyin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"张梦瑜", "zhang meng yu"},
		{"阿黄", "a huang"},
		{"Tom猫", "tom mao"},
		{"Alpha-2项目", "alpha 2 xiang mu"},
		{"张三，你好！", "zhang san ni hao"},
		{"  ", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizePinyin(c.in); got != c.want {
			t.Errorf("NormalizePinyin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeLatin 拉丁归一化：仅保留小写字母数字，其余丢弃。
func TestNormalizeLatin(t *testing.T) {
	if got := NormalizeLatin("Sky-Net_v2"); got != "skynetv2" {
		t.Errorf("NormalizeLatin = %q", got)
	}
	if got := NormalizeLatin("…"); got != "" {
		t.Errorf("纯标点应归一化为空串, got %q", got)
	}
}

// TestSimilarity 相似度：相等=1；空串=0；同音错字相近；完全不相关低。
func TestSimilarity(t *testing.T) {
	if got := Similarity("zhang meng yu", "zhang meng yu"); got != 1 {
		t.Errorf("相等应=1, got %v", got)
	}
	if got := Similarity("", "abc"); got != 0 {
		t.Errorf("空串应=0, got %v", got)
	}
	// 典型 ASR 同音错：张梦瑜(zhang meng yu) vs 长梦鱼(chang meng yu)——首音节不同、中尾相同。
	if got := Similarity("chang meng yu", "zhang meng yu"); got < 0.7 {
		t.Errorf("同音错字应≥0.7, got %v", got)
	}
	// 完全不相关。
	if got := Similarity("zhang meng yu", "kao ya rou"); got > 0.5 {
		t.Errorf("不相关应低, got %v", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/entity/ -v`
Expected: 编译失败（函数未定义）。

- [ ] **Step 3: 实现 internal/entity/phonetic.go**

```go
// Package entity 提供 ASR 实体纠错的基础件：发音归一化、候选召回、知识库种子刷新。
// 设计见 docs/superpowers/specs/2026-08-29-asr-entity-correction-design.md。
package entity

import (
	"strings"

	"github.com/hbollon/go-edlib"
	"github.com/mozillazg/go-pinyin"
)

// pinyinArgs 无声调拼音参数（包级复用，避免逐次构造）。
var pinyinArgs = pinyin.NewArgs()

func init() { pinyinArgs.Style = pinyin.Normal }

// NormalizePinyin 把任意串转「发音键」：中文字符逐字转无声调拼音（音节空格分隔），
// 连续 ASCII 字母/数字归并为一个小写词（混合名「Tom猫」→「tom mao」），其余（标点、
// 空白、其它符号）丢弃。该键用于召回阶段的 CJK 发音相似度比对。
func NormalizePinyin(s string) string {
	var parts []string
	var latin strings.Builder
	flush := func() {
		if latin.Len() > 0 {
			parts = append(parts, latin.String())
			latin.Reset()
		}
	}
	for _, r := range s {
		if r < 128 {
			lr := strings.ToLower(string(r))
			if (lr >= "a" && lr <= "z") || (lr >= "0" && lr <= "9") {
				latin.WriteString(lr)
				continue
			}
			flush() // 标点/空白：切断拉丁词
			continue
		}
		flush()
		if py := pinyin.SinglePinyin(r, pinyinArgs); len(py) > 0 {
			parts = append(parts, py[0])
		}
		// 非汉字的其它 Unicode 字符（生僻符号等）忽略。
	}
	flush()
	return strings.Join(parts, " ")
}

// NormalizeLatin 拉丁归一化：仅保留小写字母数字（丢弃标点/下划线/连字符）。
// 存 entity_kb.metaphone 列，用于拉丁名/内部代号的匹配（替代 Double Metaphone：
// 无成熟维护的 Go 实现，拉丁串本身短，直接 Jaro-Winkler 相似度足够）。
func NormalizeLatin(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 128 {
			continue
		}
		lr := strings.ToLower(string(r))
		if (lr >= "a" && lr <= "z") || (lr >= "0" && lr <= "9") {
			b.WriteString(lr)
		}
	}
	return b.String()
}

// Similarity 发音相似度：edlib Jaro-Winkler（对短串的公共前缀/单字符差异敏感，
// 契合「同音错字」场景），返回 [0,1]。相等=1，任一为空=0；算法错误降级 0（保守）。
func Similarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	s, err := edlib.StringsSimilarity(a, b, edlib.JaroWinkler)
	if err != nil {
		return 0
	}
	return float64(s)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/entity/ -v`
Expected: 3 个测试全 PASS（纯单元测试，无需 DB）。

- [ ] **Step 5: Commit**

```bash
git add internal/entity/phonetic.go internal/entity/phonetic_test.go
git commit -m "feat(entity): 发音归一化(拼音/拉丁)+Jaro-Winkler 相似度"
```

---

### Task 6: internal/entity 包——候选召回（白名单构建）

**Files:**
- Create: `internal/entity/recall.go`
- Test: `internal/entity/recall_test.go`

- [ ] **Step 1: 写失败测试**

`internal/entity/recall_test.go`：

```go
package entity

import (
	"testing"

	"zhiwei/internal/repo"
)

func testEntity(id int64, canonical, kind, py, mt string) repo.Entity {
	e := repo.Entity{Canonical: canonical, Kind: kind}
	if py != "" {
		e.Pinyin = &py
	}
	if mt != "" {
		e.Metaphone = &mt
	}
	return e
}

// TestRecallCandidates 中文召回：ASR 听错的「长梦鱼」应召回实体「张梦瑜」（拼音相似），
// minSim 过滤生效。
func TestRecallCandidates(t *testing.T) {
	ents := []repo.Entity{
		testEntity(1, "张梦瑜", "person", "zhang meng yu", ""),
		testEntity(2, "王芳", "person", "wang fang", ""),
	}
	cands := RecallCandidates("明天长梦鱼要来开会", ents, 5, 0.7)
	if len(cands) == 0 {
		t.Fatal("应召回候选")
	}
	if cands[0].Canonical != "张梦瑜" {
		t.Fatalf("Top-1 应为张梦瑜, got %s", cands[0].Canonical)
	}
	if cands[0].Similarity < 0.7 {
		t.Fatalf("相似度应≥0.7, got %v", cands[0].Similarity)
	}
}

// TestRecallCandidatesLatin 拉丁召回：实体 metaphone 键与段内英文词匹配。
func TestRecallCandidatesLatin(t *testing.T) {
	ents := []repo.Entity{
		testEntity(3, "Skynet", "custom", "skynet", "skynet"),
	}
	// ASR 常见错：大小写/连写差异。
	cands := RecallCandidates("我们在做 Sky-net 的二期", ents, 5, 0.85)
	if len(cands) == 0 || cands[0].Canonical != "Skynet" {
		t.Fatalf("应召回 Skynet, got %+v", cands)
	}
}

// TestRecallCandidatesEmpty 无匹配（段内没有相近发音）→ 空白名单（stage 据此跳过 LLM）。
func TestRecallCandidatesEmpty(t *testing.T) {
	ents := []repo.Entity{testEntity(5, "张梦瑜", "person", "zhang meng yu", "")}
	if cands := RecallCandidates("今天天气不错", ents, 5, 0.7); len(cands) != 0 {
		t.Fatalf("无关文本不应召回, got %+v", cands)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/entity/ -run TestRecallCandidates -v`
Expected: 编译失败 `undefined: RecallCandidates`。

- [ ] **Step 3: 实现 internal/entity/recall.go**

```go
package entity

import (
	"sort"
	"strings"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// Candidate 召回出的一个候选实体（白名单成员）：Similarity=该段文本与实体的
// 最佳发音相似度，用于排序与 minSim 过滤；EntityID 供 LLM 输出回指（门控校验）。
type Candidate struct {
	EntityID   ids.ID
	Canonical  string
	Kind       string
	Similarity float64
}

// isCJK 判断 rune 是否汉字（召回窗口只对汉字串滑窗）。
func isCJK(r rune) bool { return r >= 0x4E00 && r <= 0x9FFF }

// containsCJK 串里是否含汉字（决定走拼音比对还是拉丁比对）。
func containsCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// RecallCandidates 对一段转写文本召回 Top-K 候选实体，构成 LLM 合法白名单。
//
// 切片策略：
//   - 汉字连续块按 2..4 字滑窗取子串（中文姓名 2-4 字覆盖绝大多数；1 字太短误召回高）；
//   - ASCII 连续字母数字按整词取（拉丁代号场景，如 Skynet）；
//
// 比对策略：含汉字子串 → NormalizePinyin 对实体 Pinyin；纯拉丁子串 → NormalizeLatin
// 对实体 Metaphone。同一实体保留最高分；sim ≥ minSim 才入围；按 sim 降序取 Top-K。
// 返回空 = 白名单为空（correct stage 跳过该段 LLM 调用——省成本且避免误改）。
func RecallCandidates(text string, entities []repo.Entity, topK int, minSim float64) []Candidate {
	if text == "" || len(entities) == 0 {
		return nil
	}
	if topK <= 0 {
		topK = 5
	}
	// 1) 切子串：汉字块滑窗 + ASCII 词。
	var subs []string
	runes := []rune(text)
	start := -1 // 当前汉字块起点
	flushCJK := func(end int) {
		if start < 0 {
			return
		}
		block := runes[start:end]
		for l := 2; l <= 4 && l <= len(block); l++ {
			for i := 0; i+l <= len(block); i++ {
				subs = append(subs, string(block[i:i+l]))
			}
		}
		start = -1
	}
	var word strings.Builder
	flushWord := func() {
		if word.Len() > 0 {
			subs = append(subs, word.String())
			word.Reset()
		}
	}
	for i, r := range runes {
		switch {
		case isCJK(r):
			flushWord()
			if start < 0 {
				start = i
			}
		case r < 128 && ((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')):
			flushCJK(i)
			word.WriteRune(r)
		default:
			flushWord()
			flushCJK(i)
		}
	}
	flushWord()
	flushCJK(len(runes))
	if len(subs) == 0 {
		return nil
	}

	// 2) 逐实体取最佳子串相似度（子串数远小于 实体×子串 全积时也无需缓存优化：
	// 实体几十~几百 × 子串几十 = 万级 Similarity 调用，毫秒级）。
	var out []Candidate
	for _, e := range entities {
		var ep, em string
		if e.Pinyin != nil {
			ep = *e.Pinyin
		}
		if e.Metaphone != nil {
			em = *e.Metaphone
		}
		if ep == "" && em == "" {
			continue // 无发音键（脏数据）不参与
		}
		var top float64
		for _, s := range subs {
			var sim float64
			if containsCJK(s) && ep != "" {
				sim = Similarity(NormalizePinyin(s), ep)
			} else if !containsCJK(s) && em != "" {
				sim = Similarity(NormalizeLatin(s), em)
			}
			if sim > top {
				top = sim
			}
		}
		if top >= minSim {
			out = append(out, Candidate{EntityID: e.ID, Canonical: e.Canonical, Kind: e.Kind, Similarity: top})
		}
	}
	// 3) 排序 + Top-K（每实体至多一条，天然无重复）。
	sort.Slice(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/entity/ -v`
Expected: 全 PASS（纯单元测试）。若 `chang meng yu` vs `zhang meng yu` 的 Jaro-Winkler 相似度实测低于 0.7（3 音节差 1 个），把测试断言与默认 minSim 调到实测合理值（如 0.6）并在注释记录实测数字——**以真实数据为准，别硬凑**。

- [ ] **Step 5: Commit**

```bash
git add internal/entity/recall.go internal/entity/recall_test.go
git commit -m "feat(entity): RecallCandidates——滑窗子串发音召回 Top-K 白名单"
```

---

### Task 7: internal/entity 包——知识库种子刷新（RefreshAuto）

**Files:**
- Create: `internal/entity/seed.go`
- Test: `internal/entity/seed_test.go`

- [ ] **Step 1: 写失败测试（集成，走 repotest.DSN）**

写测试前先核对造数结构体的必填字段（`grep -n "type PersonPet struct" -A 20 internal/repo/person_pet.go`，同法看 PersonAttribute/Speaker/Topic/Todo），照真实字段名写。`internal/entity/seed_test.go`：

```go
package entity

import (
	"context"
	"testing"

	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// TestRefreshAuto 集成测试：造 person(+别名/项目属性)/pet/speaker/todo/topic 数据，
// 刷新后 entity_kb 各 kind 正确入库带拼音；重复刷新幂等（删旧重建不叠加）；
// manual 条目不受刷新影响；来源行清空后再刷新，对应 auto 实体消失。
func TestRefreshAuto(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const uid int64 = 1
	kb := &repo.EntityKBRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	attrs := &repo.PersonAttributeRepo{DB: db}
	pets := &repo.PersonPetRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}
	todos := &repo.TodoRepo{DB: db}
	topics := &repo.TopicRepo{DB: db}
	rels := &repo.PersonRelationshipRepo{DB: db}
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid)
		_, _ = db.Exec("DELETE FROM person_attribute")
		_, _ = db.Exec("DELETE FROM person_pet")
		_, _ = db.Exec("DELETE FROM person WHERE user_id = ?", uid)
		_, _ = db.Exec("DELETE FROM todo WHERE user_id = ?", uid)
		_, _ = db.Exec("DELETE FROM topic WHERE user_id = ?", uid)
	})
	_, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid)

	// 造数：person 张梦瑜（+别名「梦梦」+项目「天枢」）、宠物「阿黄」、
	// 说话人「李工」、未关闭 todo「评审Skynet方案」、topic「周末骑行」。
	p := &repo.Person{UserID: uid, DisplayName: "张梦瑜"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := attrs.Create(ctx, &repo.PersonAttribute{PersonID: p.ID, AttrKey: "aliases", ValueText: "梦梦", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := attrs.Create(ctx, &repo.PersonAttribute{PersonID: p.ID, AttrKey: "current_projects", ValueText: "天枢", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := pets.Create(ctx, &repo.PersonPet{PersonID: p.ID, Name: "阿黄", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := speakers.Create(ctx, &repo.Speaker{Name: "李工"}); err != nil {
		t.Fatal(err)
	}
	if err := todos.InsertExt(ctx, db, []*repo.Todo{{UserID: uid, Title: "评审Skynet方案", Status: "suggested"}}); err != nil {
		t.Fatal(err)
	}
	if err := topics.Create(ctx, &repo.Topic{UserID: uid, Name: "周末骑行", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	d := SeedDeps{KB: kb, Persons: persons, Attributes: attrs, Relationships: rels,
		Pets: pets, Speakers: speakers, Todos: todos, Topics: topics}
	if err := RefreshAuto(ctx, d, uid, repo.AllEntityKinds); err != nil {
		t.Fatalf("RefreshAuto: %v", err)
	}

	list, _ := kb.ListEnabled(ctx, uid)
	got := map[string]bool{}
	for _, e := range list {
		got[e.Canonical+"/"+e.Kind] = true
		if e.Source != repo.EntitySourceAuto {
			t.Fatalf("应全为 auto: %+v", e)
		}
		if e.Pinyin == nil || *e.Pinyin == "" {
			t.Fatalf("实体应带拼音: %+v", e)
		}
	}
	for _, want := range []string{
		"张梦瑜/person", "梦梦/person", "阿黄/pet", "天枢/project",
		"评审Skynet方案/task", "周末骑行/topic", "李工/speaker",
	} {
		if !got[want] {
			t.Fatalf("缺少实体 %s，实际 %v", want, got)
		}
	}

	// 幂等：再刷一遍数量不叠加（按 kind 删旧重建）。
	if err := RefreshAuto(ctx, d, uid, repo.AllEntityKinds); err != nil {
		t.Fatalf("RefreshAuto 二次: %v", err)
	}
	list2, _ := kb.ListEnabled(ctx, uid)
	if len(list2) != len(list) {
		t.Fatalf("二次刷新数量不应变化: %d -> %d", len(list), len(list2))
	}

	// manual 条目不受刷新影响。
	m := &repo.Entity{UserID: uid, Canonical: "内部代号X", Kind: repo.EntityKindCustom, Source: repo.EntitySourceManual, Pinyin: strPtr("nei bu dai hao x")}
	if err := kb.CreateManual(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := RefreshAuto(ctx, d, uid, repo.AllEntityKinds); err != nil {
		t.Fatal(err)
	}
	if _, err := kb.Get(ctx, uid, m.ID); err != nil {
		t.Fatalf("manual 条目不应被刷新删除: %v", err)
	}

	// 来源行清空后刷新该 kind：对应 auto 实体消失（宠物全 dismissed）。
	if _, err := db.Exec("UPDATE person_pet SET status='dismissed'"); err != nil {
		t.Fatal(err)
	}
	if err := RefreshAuto(ctx, d, uid, []string{repo.EntityKindPet}); err != nil {
		t.Fatal(err)
	}
	after, _ := kb.ListEnabled(ctx, uid)
	for _, e := range after {
		if e.Kind == repo.EntityKindPet {
			t.Fatal("宠物来源清空后 auto 实体应消失")
		}
	}
}

func strPtr(s string) *string { return &s }
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/entity/ -run TestRefreshAuto -v`
Expected: 编译失败 `undefined: SeedDeps` / `RefreshAuto`。

- [ ] **Step 3: 实现 internal/entity/seed.go**

```go
package entity

import (
	"context"
	"strings"

	"zhiwei/internal/repo"
)

// SeedDeps 种子刷新依赖（correct stage 每次运行前刷新实体库）。
// 各 repo 为 nil 时对应 kind 跳过（测试/降级装配）；KB 为 nil 时整个刷新 no-op。
// 注意：speaker 表当前无 user_id（历史设计），kind=speaker 暂按全量名册非随机名
// 入库（随机名「说话人xxxxx」无纠错价值，跳过）——人名重复入库无害。
type SeedDeps struct {
	KB            *repo.EntityKBRepo
	Persons       *repo.PersonRepo
	Attributes    *repo.PersonAttributeRepo
	Relationships *repo.PersonRelationshipRepo
	Pets          *repo.PersonPetRepo
	Speakers      *repo.SpeakerRepo
	Todos         *repo.TodoRepo
	Topics        *repo.TopicRepo
}

// RefreshAuto 重建用户 auto 实体：对 kinds 里每个 kind，收集当前来源行的名字
// → ReplaceAuto（事务内删旧 auto 该 kind + 落新，带拼音/音素键）。
// manual 条目与 enabled 标记不动（ReplaceAuto 只碰 source='auto' 行）。
// 任一 kind 刷新失败即返回错误——调用方（correct stage）吞错降级用库内旧实体继续。
func RefreshAuto(ctx context.Context, d SeedDeps, userID int64, kinds []string) error {
	if d.KB == nil {
		return nil
	}
	enabled := map[string]bool{}
	for _, k := range kinds {
		enabled[k] = true
	}
	// person 聚合：display_name + 别名(aliases) + 称呼(relationship.label)；同一轮顺带
	// 收集 current_projects（归 project kind，person 遍历一次少查一遍属性表）。
	if enabled[repo.EntityKindPerson] && d.Persons != nil {
		var persons, projects []repo.Entity
		if ps, err := d.Persons.List(ctx, userID); err == nil {
			for _, p := range ps {
				if p.Status != "active" && p.Status != "pending" {
					continue
				}
				addSeedEntity(&persons, p.DisplayName, repo.EntityKindPerson, "person:"+p.ID.String())
				if d.Attributes != nil {
					if attrs, err := d.Attributes.ListByPerson(ctx, p.ID); err == nil {
						for _, a := range attrs {
							if a.Status != "active" || a.ValueText == "" {
								continue
							}
							switch a.AttrKey {
							case "aliases":
								addSeedEntity(&persons, a.ValueText, repo.EntityKindPerson, "person_attr:"+a.ID.String())
							case "current_projects":
								addSeedEntity(&projects, a.ValueText, repo.EntityKindProject, "person_attr:"+a.ID.String())
							}
						}
					}
				}
				if d.Relationships != nil {
					if rels, err := d.Relationships.ListByPerson(ctx, p.ID); err == nil {
						for _, rel := range rels {
							if rel.Label != nil && *rel.Label != "" {
								addSeedEntity(&persons, *rel.Label, repo.EntityKindPerson, "person_rel:"+rel.ID.String())
							}
						}
					}
				}
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindPerson, dedupeSeed(persons)); err != nil {
			return err
		}
		if enabled[repo.EntityKindProject] {
			if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindProject, dedupeSeed(projects)); err != nil {
				return err
			}
		}
	} else if enabled[repo.EntityKindProject] && d.Persons != nil && d.Attributes != nil {
		// person 关了但 project 开着：单独跑一遍属性收集（少见配置，简单实现）。
		var projects []repo.Entity
		if ps, err := d.Persons.List(ctx, userID); err == nil {
			for _, p := range ps {
				if attrs, err := d.Attributes.ListByPerson(ctx, p.ID); err == nil {
					for _, a := range attrs {
						if a.Status == "active" && a.AttrKey == "current_projects" && a.ValueText != "" {
							addSeedEntity(&projects, a.ValueText, repo.EntityKindProject, "person_attr:"+a.ID.String())
						}
					}
				}
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindProject, dedupeSeed(projects)); err != nil {
			return err
		}
	}
	// pet：name + nickname（按用户的人物遍历；pet 表挂在 person 下）。
	if enabled[repo.EntityKindPet] && d.Pets != nil && d.Persons != nil {
		var list []repo.Entity
		if ps, err := d.Persons.List(ctx, userID); err == nil {
			for _, p := range ps {
				if petList, err := d.Pets.ListByPerson(ctx, p.ID); err == nil {
					for _, pet := range petList {
						if pet.Status != "active" {
							continue
						}
						addSeedEntity(&list, pet.Name, repo.EntityKindPet, "pet:"+pet.ID.String())
						if pet.Nickname != nil && *pet.Nickname != "" {
							addSeedEntity(&list, *pet.Nickname, repo.EntityKindPet, "pet:"+pet.ID.String())
						}
					}
				}
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindPet, dedupeSeed(list)); err != nil {
			return err
		}
	}
	// speaker：全量名册非随机名（「说话人xxxxx」是自动登记占位名，无纠错价值）。
	if enabled[repo.EntityKindSpeaker] && d.Speakers != nil {
		var list []repo.Entity
		if sps, err := d.Speakers.List(ctx); err == nil {
			for _, sp := range sps {
				if isAutoSpeakerName(sp.Name) {
					continue
				}
				addSeedEntity(&list, sp.Name, repo.EntityKindSpeaker, "speaker:"+sp.ID.String())
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindSpeaker, dedupeSeed(list)); err != nil {
			return err
		}
	}
	// task：未关闭 todo 标题（ListOpenTitlesExt 只回 suggested+confirmed）。
	if enabled[repo.EntityKindTask] && d.Todos != nil {
		var list []repo.Entity
		if titles, err := d.Todos.ListOpenTitlesExt(ctx, d.Todos.DB, userID); err == nil {
			for _, t := range titles {
				addSeedEntity(&list, t, repo.EntityKindTask, "")
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindTask, dedupeSeed(list)); err != nil {
			return err
		}
	}
	// topic：active 话题名。
	if enabled[repo.EntityKindTopic] && d.Topics != nil {
		var list []repo.Entity
		if ts, err := d.Topics.ListActive(ctx, userID, 500); err == nil {
			for _, tp := range ts {
				addSeedEntity(&list, tp.Name, repo.EntityKindTopic, "topic:"+tp.ID.String())
			}
		}
		if err := d.KB.ReplaceAuto(ctx, userID, repo.EntityKindTopic, dedupeSeed(list)); err != nil {
			return err
		}
	}
	return nil
}

// addSeedEntity 追加一个待落库实体（算好拼音/音素键；canonical 去首尾空白）。
// 空/超长（>128 rune，DB VARCHAR(128) 上限）丢弃——宁缺勿错，不让单条脏数据
// 炸掉整批 ReplaceAuto 事务。
func addSeedEntity(list *[]repo.Entity, canonical, kind, sourceRef string) {
	canonical = strings.TrimSpace(canonical)
	if canonical == "" || len([]rune(canonical)) > 128 {
		return
	}
	e := repo.Entity{Canonical: canonical, Kind: kind, Enabled: true}
	py := NormalizePinyin(canonical)
	if py != "" {
		e.Pinyin = &py
	}
	// 含拉丁成分才落 metaphone 键（纯中文实体该列为 NULL，召回只走拼音）。
	if lt := NormalizeLatin(canonical); lt != "" && lt != py {
		e.Metaphone = &lt
	}
	if sourceRef != "" {
		e.SourceRef = &sourceRef
	}
	*list = append(*list, e)
}

// dedupeSeed 同 canonical 去重（不同来源行可能产出同名，如 person 别名=pet 名；
// 唯一键 (user_id, canonical, kind) 下重复插入会炸事务）。
func dedupeSeed(list []repo.Entity) []repo.Entity {
	seen := map[string]bool{}
	out := list[:0]
	for _, e := range list {
		if seen[e.Canonical] {
			continue
		}
		seen[e.Canonical] = true
		out = append(out, e)
	}
	return out
}

// isAutoSpeakerName 自动登记的随机说话人名（stage_speaker.go 的 rand5 产物形态：
// 「说话人」+5位[a-z0-9]）。不 import pipeline（避免 pipeline→entity→pipeline 环），
// 正则形态自持一份；形态变更时两处同步。
func isAutoSpeakerName(name string) bool {
	const prefix = "说话人"
	if len(name) != len(prefix)+5 || !strings.HasPrefix(name, prefix) {
		return false
	}
	for _, r := range name[len(prefix):] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
```

注意两点：
1. `Person`/`PersonPet`/`Topic`/`Todo` struct 的 Status 字段名/取值以实际代码为准（`grep -n "Status" internal/repo/person.go`）——`person` 表 status 是 active|pending|merged|dismissed，过滤条件照实际枚举写。
2. `d.Todos.DB` 若编译报不可访问（小写），在 `internal/repo/todo.go` 加非 Ext 包装方法 `ListOpenTitles(ctx, userID) ([]string, error)`（透传 `ListOpenTitlesExt(ctx, r.DB, userID)`），seed.go 改调它。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/entity/ -v`
Expected: 全 PASS（TestRefreshAuto 需 TEST_MYSQL_DSN，否则 SKIP）。

- [ ] **Step 5: Commit**

```bash
git add internal/entity/seed.go internal/entity/seed_test.go internal/repo/todo.go
git commit -m "feat(entity): RefreshAuto——从 person/pet/speaker/todo/topic 重建 auto 实体库"
```

---

### Task 8: 纠错 prompt + LLM 输出解析

**Files:**
- Create: `prompts/asr_correction_v1.md`
- Create: `internal/pipeline/stage_correct.go`（先只含解析部分；stage 主体 Task 9 补全）
- Test: `internal/pipeline/stage_correct_test.go`（先只含解析用例）

- [ ] **Step 1: 写 prompt**

`prompts/asr_correction_v1.md`：

```markdown
# ASR 实体纠错 prompt（版本：asr_correction_v1）

你是语音转写的专有名词纠错器。输入是一段 ASR 转写（带【本段】标记，可能附前后几段上下文）
和一个「合法实体白名单」。你的唯一任务：找出【本段】里**明显是白名单中某个实体被 ASR 听错**
的片段，纠正成白名单里的规范名。

## 判定规则（核心，逐条硬约束）

1. 只纠**读音相近**的实体：某片段与白名单某实体的普通话发音明显相近，且上下文里指的就是这个实体。
2. `corrected` 必须**逐字等于**白名单中某条目的 canonical，一个字都不能差；白名单之外的名字一律不得输出。
3. `orig` 必须**逐字来自【本段】文本**（不得跨段、不得改写拼接、不得包含不在段内的字符）。
4. `entity_id` 填该白名单条目的 id（原样复制）。
5. **除被纠正的实体片段外不得改动任何其他文字**——不改标点、不改语序、不改语气词、不改正常词。
   你只输出替换建议，不输出改写后的整句。
6. 拿不准就不纠：宁可漏纠，不可错改。无任何纠错输出 {"edits":[]}。

## 例子

输入：
【前文】明天下午的评审记得叫上大家。
【本段】常梦瑜你看到我的邮件了吗，发你了三版方案。
【后文】收到了，我看完回复你。
合法实体白名单：
- id=9527 canonical=张梦瑜 kind=person
- id=9528 canonical=阿黄 kind=pet

输出：
{"edits":[{"orig":"常梦瑜","corrected":"张梦瑜","entity_id":"9527","confidence":0.9,"reason":"读音相近（changmengyu/zhangmengyu）且上下文在称呼人"}]}

## 输出格式

只输出 JSON，不要任何其他文字和代码围栏。

{"edits":[
  {"orig":"<段内原片段>","corrected":"<白名单 canonical>","entity_id":"<白名单 id>","confidence":0.0-1.0,"reason":"<简短依据，≤40字>"}
]}

- confidence：读音相近且上下文指向明确 ≥0.85；读音相近但上下文无佐证 0.6~0.85；仅读音擦边 <0.6。
```

- [ ] **Step 2: 写解析的失败测试**

`internal/pipeline/stage_correct_test.go`：

```go
package pipeline

import (
	"testing"
)

// TestParseCorrectionEdits 解析 LLM 输出：正常/带围栏废话/空 edits/非法 JSON。
func TestParseCorrectionEdits(t *testing.T) {
	// 1) 正常输出。
	got, err := ParseCorrectionEdits(`{"edits":[{"orig":"常梦瑜","corrected":"张梦瑜","entity_id":"9527","confidence":0.9,"reason":"读音相近"}]}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 1 || got[0].Orig != "常梦瑜" || got[0].Corrected != "张梦瑜" || got[0].EntityID != "9527" {
		t.Fatalf("解析不符: %+v", got)
	}

	// 2) 带代码围栏与前后废话（容错：截首{尾}，同 ParseNameCandidates）。
	got, err = ParseCorrectionEdits("好的，以下是纠错结果：\n```json\n{\"edits\":[]}\n```")
	if err != nil || len(got) != 0 {
		t.Fatalf("围栏容错失败: %v %+v", err, got)
	}

	// 3) 清洗：置信度越界 clamp、空 orig/corrected 丢弃。
	got, err = ParseCorrectionEdits(`{"edits":[{"orig":"","corrected":"张梦瑜","entity_id":"9527","confidence":1.5},{"orig":"阿黄","corrected":"阿皇","entity_id":"9528","confidence":0.8}]}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("空 orig 应丢弃: %+v", got)
	}

	// 4) 彻底非法 JSON → error（调用方吞错跳过该段）。
	if _, err := ParseCorrectionEdits(`这不是JSON`); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/pipeline/ -run TestParseCorrectionEdits -v`
Expected: 编译失败 `undefined: ParseCorrectionEdits`。

- [ ] **Step 4: 实现 stage_correct.go（先解析部分）**

`internal/pipeline/stage_correct.go`：

```go
// stage_correct 实现 correct stage（ASR 实体纠错）：拼音/音素召回候选白名单 →
// LLM 一程裁决（只改白名单内实体）→ 双重门控后局部替换并标记 corrected_reason='entity'。
// 设计见 docs/superpowers/specs/2026-08-29-asr-entity-correction-design.md。
package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
)

// correctionEdit LLM 输出的一条纠错建议（asr_correction_v1 契约）。
type correctionEdit struct {
	Orig       string  `json:"orig"`       // 段内原片段（门控：必须原样出现在段文本里）
	Corrected  string  `json:"corrected"`  // 替换目标（门控：必须逐字等于白名单 canonical）
	EntityID   string  `json:"entity_id"`  // 白名单条目 id（门控：必须在白名单内）
	Confidence float64 `json:"confidence"` // 0~1，clamp
	Reason     string  `json:"reason"`     // 简短依据（存 entity_edits 供前端 tooltip）
}

// correctionResult LLM 输出整体。
type correctionResult struct {
	Edits []correctionEdit `json:"edits"`
}

// ParseCorrectionEdits 解析 LLM 纠错输出（纯函数，可单测）。
// 容错同 ParseNameCandidates：截取首 { 到末 }，剥掉围栏与前后废话；
// 彻底非法 JSON 返回 error（调用方吞错跳过该段，不 fail session）。
// 清洗：置信度 clamp 到 [0,1]；空 orig/corrected 丢弃（无法应用）。
func ParseCorrectionEdits(raw string) ([]correctionEdit, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out correctionResult
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("纠错输出解析失败: %w", err)
	}
	edits := make([]correctionEdit, 0, len(out.Edits))
	for _, e := range out.Edits {
		if strings.TrimSpace(e.Orig) == "" || strings.TrimSpace(e.Corrected) == "" {
			continue
		}
		e.Confidence = clampConfidence(e.Confidence) // 复用 stage_speaker_name.go 的 clamp
		edits = append(edits, e)
	}
	return edits, nil
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/pipeline/ -run TestParseCorrectionEdits -v`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add prompts/asr_correction_v1.md internal/pipeline/stage_correct.go internal/pipeline/stage_correct_test.go
git commit -m "feat(pipeline): asr_correction_v1 prompt + LLM 纠错输出解析"
```

---

### Task 9: correct stage 主体（召回 → LLM → 门控应用）

**Files:**
- Modify: `internal/pipeline/stage_correct.go`（补 stage 主体）
- Modify: `internal/pipeline/stage_asr.go`（StageDeps 加字段 + BuildStages 注册）
- Test: `internal/pipeline/stage_correct_test.go`（追加用例）

- [ ] **Step 1: 写失败测试（fake LLM + 内存库走 repotest.DSN）**

先看 `internal/pipeline/main_test.go` 既有的 fake LLM/测试装配（`grep -n "fakeLLM\|stubLLM\|type.*LLM" internal/pipeline/*_test.go`），有则复用；没有则在 stage_correct_test.go 自带：

```go
// fakeCorrectLLM 可编程的 LLM 桩：按序返回预设响应。
type fakeCorrectLLM struct {
	resps []string // 每次调用弹出一个；耗尽返回错误
	calls []string // 记录每次收到的 user message（断言 prompt 组装用）
	err   error
}

func (f *fakeCorrectLLM) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	f.calls = append(f.calls, req.User)
	if f.err != nil {
		return provider.ChatResponse{}, f.err
	}
	if len(f.resps) == 0 {
		return provider.ChatResponse{}, errors.New("no more canned responses")
	}
	r := f.resps[0]
	f.resps = f.resps[1:]
	return provider.ChatResponse{Content: r}, nil
}
```

追加主链路用例：

```go
// TestStageCorrectHappyPath 端到端（DB + fake LLM）：实体库有「张梦瑜」，段文本「常梦瑜…」，
// LLM 返回纠错 → 门控通过 → text 改写 + corrected_reason='entity' + entity_edits + full_text 重算。
func TestStageCorrectHappyPath(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const uid int64 = 1
	transcripts := &repo.TranscriptRepo{DB: db}
	sessions := &repo.SessionRepo{DB: db}
	kb := &repo.EntityKBRepo{DB: db}
	settings := &repo.EntitySettingsRepo{DB: db}
	// 造 session + transcript + 2 段（第 2 段含错名）。
	sess := &repo.AudioSession{UserID: uid, Filename: "t.wav", StoragePath: "x", Status: "completed"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE tr, seg FROM transcript tr JOIN transcript_segment seg ON seg.transcript_id = tr.id WHERE tr.user_id = ?`, uid)
		_, _ = db.Exec("DELETE FROM audio_session WHERE id = ?", sess.ID.Int64())
		_, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid)
	})
	tr := &repo.Transcript{UserID: uid, SessionID: sess.ID, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	py := "zhang meng yu"
	if err := kb.ReplaceAuto(ctx, uid, repo.EntityKindPerson, []repo.Entity{{Canonical: "张梦瑜", Pinyin: &py}}); err != nil {
		t.Fatal(err)
	}
	segs := []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "明天开会", StartMS: 0, EndMS: 2000},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "1", Text: "常梦瑜你看到我的邮件了吗", StartMS: 2000, EndMS: 6000},
	}
	if err := transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}

	llm := &fakeCorrectLLM{resps: []string{
		`{"edits":[{"orig":"常梦瑜","corrected":"张梦瑜","entity_id":"` + segs0EntityID(t, ctx, kb, uid) + `","confidence":0.92,"reason":"读音相近"}]}`,
	}}
	d := StageDeps{
		Sessions: sessions, Transcripts: transcripts, DB: db,
		LLM: llm, LLMModel: "test-model",
		EntityKB: kb, EntitySettings: settings,
		EntitySeed: entity.SeedDeps{KB: kb}, // 只给 KB：刷新 no-op（实体已手工备好）
		CorrectPrompt:    "system prompt",
		CorrectWindow:    2, CorrectTopK: 5, CorrectMinSim: 0.6,
	}
	j := &repo.Job{SessionID: sess.ID}
	if err := runCorrectStage(ctx, d, j, sess.ID); err != nil {
		t.Fatalf("runCorrectStage: %v", err)
	}
	after, _ := transcripts.ListSegments(ctx, tr.ID)
	if after[1].Text != "张梦瑜你看到我的邮件了吗" {
		t.Fatalf("第 2 段应被纠正: %q", after[1].Text)
	}
	if after[1].CorrectedReason == nil || *after[1].CorrectedReason != "entity" {
		t.Fatalf("corrected_reason 应为 entity: %v", after[1].CorrectedReason)
	}
	var edits []map[string]any
	if err := json.Unmarshal(after[1].EntityEdits, &edits); err != nil || len(edits) != 1 || edits[0]["orig"] != "常梦瑜" {
		t.Fatalf("entity_edits 不符: %s %v", after[1].EntityEdits, err)
	}
	if after[0].CorrectedReason != nil {
		t.Fatal("第 1 段无候选不应被触碰")
	}
	// 白名单为空的段不调 LLM（只有第 2 段有相近发音）。
	if len(llm.calls) != 1 {
		t.Fatalf("应恰好 1 次 LLM 调用, got %d", len(llm.calls))
	}
	// 幂等：再跑一遍不再调 LLM（已纠正段跳过）。
	if err := runCorrectStage(ctx, d, j, sess.ID); err != nil {
		t.Fatal(err)
	}
	if len(llm.calls) != 1 {
		t.Fatalf("二次运行不应再调 LLM, got %d", len(llm.calls))
	}
}

// segs0EntityID 取库内「张梦瑜」实体的 id 字符串（拼进 fake LLM 响应）。
func segs0EntityID(t *testing.T, ctx context.Context, kb *repo.EntityKBRepo, uid int64) string {
	t.Helper()
	list, err := kb.ListEnabled(ctx, uid)
	if err != nil || len(list) == 0 {
		t.Fatalf("实体未入库: %v", err)
	}
	return list[0].ID.String()
}

// TestStageCorrectGate 低置信度不改写；orig 不在段内不改写；corrected 不在白名单不改写。
func TestStageCorrectGate(t *testing.T) {
	// 建库同上（抽公共 helper setupCorrectFixture(t) 返回 (d, sess, tr, llm)，此处省略重复——
	// 实现时把 HappyPath 的建库段抽成 helper 供三个用例共用）。
	t.Run("低置信度", func(t *testing.T) {
		// llm 响应 confidence=0.5（<默认 0.8）→ 段文本不变、无标记。
	})
	t.Run("orig不在段内", func(t *testing.T) {
		// llm 响应 orig="王大锤"（段里没有）→ 不改写。
	})
	t.Run("corrected不在白名单", func(t *testing.T) {
		// llm 响应 corrected="张梦宇"（白名单是张梦瑜）→ 不改写（防幻觉实体）。
	})
	t.Run("LLM失败不阻塞", func(t *testing.T) {
		// llm 返回 error → runCorrectStage 返回 nil（best-effort），段不动。
	})
}
```

> **注意**：`TestStageCorrectGate` 的四个子用例骨架如上；实现时复用 `setupCorrectFixture` helper（建库/造段逻辑与 HappyPath 相同，仅 LLM 响应与断言不同），每个子用例都是完整可运行的代码（含全部断言），不要留成空壳。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/pipeline/ -run TestStageCorrect -v`
Expected: 编译失败（`runCorrectStage`/`StageDeps` 新字段未定义）。

- [ ] **Step 3: 实现 stage 主体（stage_correct.go 追加）**

```go
// runCorrectStage 是 correct stage 的可测核心（避开 pool），由 stageCorrect 包装。
//
// 流程：读设置（关→no-op）→ 刷新实体库（失败降级用旧库）→ 读段（跳过已 entity 纠正段=幂等）
// → 逐段：召回白名单（空→跳过 LLM）→ 组上下文 → LLM → 解析 → 门控应用 → 落库。
// 全程 best-effort（同 speakername）：LLM/解析失败 log+trace 后继续下一段，不 fail session；
// 真 DB 错误（读段/写段/读设置）返回 error 交 pool 重试。
func runCorrectStage(ctx context.Context, d StageDeps, j *repo.Job, sessionID ids.ID) error {
	if d.EntityKB == nil || d.EntitySettings == nil || d.LLM == nil {
		return nil // 依赖未装配（测试/降级）→ no-op
	}
	s, err := d.Sessions.Get(ctx, 1, sessionID) // 阶段1：后台流水线无请求上下文，暂 user-1
	if err != nil {
		return fmt.Errorf("读 session: %w", err)
	}
	st, err := d.EntitySettings.Get(ctx, s.UserID)
	if err != nil {
		return fmt.Errorf("读实体纠错设置: %w", err)
	}
	if !st.CorrectionEnabled {
		return nil
	}
	// 刷新 auto 实体（失败不阻断：用库内旧实体继续纠错——旧库总比不纠好）。
	if err := entity.RefreshAuto(ctx, d.EntitySeed, s.UserID, st.AutoSources); err != nil {
		log.Printf("[correct] session=%s 实体库刷新失败（降级用旧库）: %v", sessionID, err)
		appendTrace(j, repo.TraceEntry{Stage: "correct", Error: fmt.Sprintf("实体库刷新失败（降级）: %v", err)})
	}
	entities, err := d.EntityKB.ListEnabled(ctx, s.UserID)
	if err != nil {
		return fmt.Errorf("读实体库: %w", err)
	}
	if len(entities) == 0 {
		return nil // 空库无事可做
	}
	tr, err := d.Transcripts.GetBySession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("读 transcript: %w", err)
	}
	segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return fmt.Errorf("读 segments: %w", err)
	}

	window := d.CorrectWindow
	if window <= 0 {
		window = 2
	}
	topK := d.CorrectTopK
	if topK <= 0 {
		topK = 5
	}
	minSim := d.CorrectMinSim
	if minSim <= 0 {
		minSim = 0.6 // 默认召回下限（Task 6 实测校准值）
	}

	changed := false
	for i := range segs {
		sg := &segs[i]
		if sg.CorrectedReason != nil && *sg.CorrectedReason == "entity" {
			continue // 幂等：已纠正段跳过（显式跳过比「召回为空」更省）
		}
		if strings.TrimSpace(sg.Text) == "" {
			continue
		}
		cands := entity.RecallCandidates(sg.Text, entities, topK, minSim)
		if len(cands) == 0 {
			continue // 白名单为空 → 不调 LLM（省成本 + 避免无约束改写）
		}
		edits := correctSegment(ctx, d, j, sessionID, sg, cands, segs, i, window, st.ConfidenceThreshold)
		if len(edits) == 0 {
			continue
		}
		// 门控通过的 edits 逐个应用到段文本副本（首次出现处替换，最小改动）。
		text := sg.Text
		var applied []appliedEdit
		for _, e := range edits {
			if !strings.Contains(text, e.Orig) {
				continue // 前一个替换改变了文本后可能不再包含（位置竞争），跳过
			}
			text = strings.Replace(text, e.Orig, e.Corrected, 1)
			applied = append(applied, appliedEdit{
				Orig: e.Orig, Corrected: e.Corrected,
				Canonical: e.Corrected, Confidence: e.Confidence, Reason: e.Reason,
			})
		}
		if len(applied) == 0 {
			continue
		}
		raw, err := json.Marshal(applied)
		if err != nil {
			log.Printf("[correct] session=%s 序列化 edits 失败: %v", sessionID, err)
			continue
		}
		if err := d.Transcripts.ApplyEntityCorrections(ctx, tr.ID, sg.ID, text, raw); err != nil {
			return fmt.Errorf("落库纠错段 %d: %w", sg.SequenceNo, err) // DB 写失败：真基础设施问题，交 pool 重试
		}
		changed = true
	}
	if changed {
		if err := d.Transcripts.RecomputeFullText(ctx, tr.ID); err != nil {
			return fmt.Errorf("重算 full_text: %w", err)
		}
	}
	return nil
}

// appliedEdit 落库到 entity_edits 的明细（前端对照展示用）。
type appliedEdit struct {
	Orig       string  `json:"orig"`      // 原片段（删除线展示）
	Corrected  string  `json:"corrected"` // 纠正后（=白名单 canonical）
	Canonical  string  `json:"canonical"` // 命中的实体规范名（语义同 corrected，冗余存储便于前端直接用）
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason,omitempty"`
}

// correctSegment 单段纠错：组上下文 + 白名单 → LLM → 解析 → 门控（置信度 ≥ threshold、
// orig 原样在段内、corrected/entity_id 在白名单内）→ 返回通过的 edits。
// LLM/解析失败：log + trace + 返回 nil（best-effort，不 fail session）。
func correctSegment(ctx context.Context, d StageDeps, j *repo.Job, sessionID ids.ID,
	sg *repo.TranscriptSegment, cands []entity.Candidate, segs []repo.TranscriptSegment,
	i, window int, threshold float64) []correctionEdit {

	// 白名单索引（门控校验用）。
	byCanonical := make(map[string]bool, len(cands))
	byID := make(map[string]bool, len(cands))
	var sb strings.Builder
	sb.WriteString("合法实体白名单（corrected 只能取自这里）：\n")
	for _, c := range cands {
		fmt.Fprintf(&sb, "- id=%s canonical=%s kind=%s\n", c.EntityID, c.Canonical, c.Kind)
		byCanonical[c.Canonical] = true
		byID[c.EntityID.String()] = true
	}
	sb.WriteString("\n对话转写（【本段】是要纠错的段落，其余为上下文参考）：\n")
	for k := i - window; k <= i+window; k++ {
		if k < 0 || k >= len(segs) || k == i {
			continue
		}
		fmt.Fprintf(&sb, "【前文/后文】%s\n", segs[k].Text)
	}
	fmt.Fprintf(&sb, "【本段】%s\n", sg.Text)

	begin := time.Now()
	resp, err := d.LLM.Chat(ctx, provider.ChatRequest{
		Model: d.LLMModel, System: d.CorrectPrompt, User: sb.String(), Temperature: 0.1,
	})
	if err != nil {
		log.Printf("[correct] session=%s 段%d LLM 失败（尽力而为）: %v", sessionID, sg.SequenceNo, err)
		appendTrace(j, repo.TraceEntry{Stage: "correct", Error: fmt.Sprintf("段%d LLM 失败（尽力而为）: %v", sg.SequenceNo, err)})
		return nil
	}
	appendTrace(j, repo.TraceEntry{Stage: "correct:llm", Model: d.LLMModel, MS: msSince(begin), Tokens: resp.TotalTokens})
	edits, err := ParseCorrectionEdits(resp.Content)
	if err != nil {
		log.Printf("[correct] session=%s 段%d 解析失败（尽力而为）: %v", sessionID, sg.SequenceNo, err)
		appendTrace(j, repo.TraceEntry{Stage: "correct", Error: fmt.Sprintf("段%d 解析失败（尽力而为）: %v", sg.SequenceNo, err)})
		return nil
	}
	// 双重门控：阈值 + orig 在段内 + corrected/entity_id 在白名单内。
	var pass []correctionEdit
	for _, e := range edits {
		if e.Confidence < threshold {
			continue
		}
		if !strings.Contains(sg.Text, e.Orig) {
			continue // 幻觉 orig：段里根本没有这个片段
		}
		if !byCanonical[e.Corrected] || !byID[e.EntityID] {
			continue // 幻觉实体：白名单里没有
		}
		pass = append(pass, e)
	}
	return pass
}

// stageCorrect 是 pool 用的 Handler 包装。
func stageCorrect(d StageDeps) Handler {
	return func(ctx context.Context, j *repo.Job, sessionID ids.ID) error {
		return runCorrectStage(ctx, d, j, sessionID)
	}
}
```

stage_correct.go 的 import 块（完整）：

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"zhiwei/internal/entity"
	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)
```

- [ ] **Step 4: StageDeps 加字段 + BuildStages 注册**

`internal/pipeline/stage_asr.go`：

1) `StageDeps` struct 在 speakername 字段块后追加：

```go
	// ---- correct stage（ASR 实体纠错）----
	EntityKB       *repo.EntityKBRepo       // 实体知识库；nil = no-op（兼容旧装配/测试）
	EntitySettings  *repo.EntitySettingsRepo // 纠错配置（每用户开关/阈值/auto_sources）
	EntitySeed      entity.SeedDeps          // 种子刷新依赖（各来源 repo + KB）
	CorrectPrompt   string                   // prompts/asr_correction_v1.md 内容（system prompt）
	CorrectWindow   int                      // 上下文前后段数，0 = 默认 2
	CorrectTopK     int                      // 召回 Top-K，0 = 默认 5
	CorrectMinSim   float64                  // 召回相似度下限，0 = 默认 0.6
```

2) `BuildStages` 的 map 里加一行：

```go
		"correct":     stageCorrect(d),
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/pipeline/ -run TestStageCorrect -v`
Expected: HappyPath + Gate 全 PASS。
再跑全包：`go test ./internal/pipeline/`（确认既有 stage 测试未被 StageDeps 变更破坏）。

- [ ] **Step 6: Commit**

```bash
git add internal/pipeline/stage_correct.go internal/pipeline/stage_correct_test.go internal/pipeline/stage_asr.go
git commit -m "feat(pipeline): correct stage——召回+LLM 裁决+双重门控的实体纠错"
```

---

### Task 10: config + main.go 装配（stage 插入流水线）

**Files:**
- Modify: `internal/config/config.go`
- Modify: `cmd/zhiwei-server/main.go`

- [ ] **Step 1: config 加字段**

`internal/config/config.go`：`Config` struct 的 profile 字段块后追加：

```go
	// ---- correct stage（ASR 实体纠错）----
	EntityCorrectEnabled bool    // ZW_ENTITY_CORRECT_ENABLED：是否启用 correct stage（默认 true）
	EntityCorrectWindow  int     // ZW_ENTITY_CORRECT_WINDOW：LLM 上下文前后段数（默认 2）
	EntityCorrectTopK    int     // ZW_ENTITY_CORRECT_TOPK：召回 Top-K（默认 5）
	EntityCorrectMinSim  float64 // ZW_ENTITY_CORRECT_MIN_SIM：召回相似度下限（默认 0.6）
```

`Load()` 的 profile 装配块后追加：

```go
		// ---- correct stage（ASR 实体纠错）----
		EntityCorrectEnabled: getenvBool("ZW_ENTITY_CORRECT_ENABLED", true),
		EntityCorrectWindow:  getenvInt("ZW_ENTITY_CORRECT_WINDOW", 2),
		EntityCorrectTopK:    getenvInt("ZW_ENTITY_CORRECT_TOPK", 5),
		EntityCorrectMinSim:  getenvFloat("ZW_ENTITY_CORRECT_MIN_SIM", 0.6),
```

注意：`getenvFloat` 只接受 [0,1]，0.6 合法；若想允许 >1 的相似度需用新 helper——当前需求 [0,1] 够用。

- [ ] **Step 2: main.go 装配**

`cmd/zhiwei-server/main.go`，在 `stages := pipeline.BuildStages(...)` 之前（约 :211 处，profile prompt 读取之后）加 prompt 读取与 repo 构造：

```go
	// ASR 实体纠错（correct stage）：拼音/音素召回 + LLM 裁决，只改实体并标记。
	correctPromptBytes, err := os.ReadFile("prompts/asr_correction_v1.md")
	if err != nil {
		log.Fatalf("读 asr_correction_v1.md: %v", err)
	}
	entityKB := &repo.EntityKBRepo{DB: db}
	entitySettings := &repo.EntitySettingsRepo{DB: db}
	entitySeed := entity.SeedDeps{
		KB: entityKB, Persons: persons, Attributes: personAttrs, Relationships: personRels,
		Pets: personPets, Speakers: speakers, Todos: todos, Topics: topics,
	}
```

`pipeline.StageDeps{...}` 字面量里追加：

```go
		EntityKB:       entityKB,
		EntitySettings: entitySettings,
		EntitySeed:     entitySeed,
		CorrectPrompt:  string(correctPromptBytes),
		CorrectWindow:  cfg.EntityCorrectWindow,
		CorrectTopK:    cfg.EntityCorrectTopK,
		CorrectMinSim:  cfg.EntityCorrectMinSim,
```

`stagesList` 改为（`correct` 插在 `asr` 后、`segment` 前；env 关掉则不插，仿 profile 开关模式）：

```go
	// profile stage 按开关追加（ZW_PROFILE_EXTRACT_ENABLED=false 时仅手动+回填端点）
	stagesList := []string{"asr"}
	if cfg.EntityCorrectEnabled {
		stagesList = append(stagesList, "correct")
	}
	stagesList = append(stagesList, "segment", "speaker", "speakername", "extract")
	if cfg.ProfileExtractEnabled {
		stagesList = append(stagesList, "profile")
	}
```

import 块加 `"zhiwei/internal/entity"`。

- [ ] **Step 3: 验证编译 + 全包测试**

Run: `go build ./... && go vet ./... && go test ./internal/pipeline/ ./internal/config/`
Expected: 编译通过、测试 PASS（main.go 的既有行号会位移，正常）。

- [ ] **Step 4: Commit**

```bash
git add internal/config/config.go cmd/zhiwei-server/main.go
git commit -m "feat(pipeline): correct stage 装配进流水线(asr 后/segment 前)+env 开关"
```

---

### Task 11: API——entity-settings + entities CRUD

**Files:**
- Create: `internal/agent/entity.go`
- Modify: `internal/agent/handlers.go`（AgentHandler 字段 + 路由注册）
- Test: `internal/agent/entity_test.go`

- [ ] **Step 1: 写失败测试**

先看既有 handler 测试的装配模式（`grep -n "RegisterAgent\|httptest" internal/agent/handlers_test.go | head`），照抄鉴权注入方式。`internal/agent/entity_test.go` 骨架（装配细节对齐既有测试）：

```go
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// newEntityTestServer 装配只含实体端点的测试服务器（鉴权用与既有 handler 测试相同
// 的注入方式——若既有测试用 auth.WithUser 之类的 ctx 注入 helper，照抄；这里以
// chi route + 手动注入 uid 的模式示意，落地时对齐）。
func newEntityTestServer(t *testing.T) (http.Handler, *repo.EntityKBRepo, *repo.EntitySettingsRepo) {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	kb := &repo.EntityKBRepo{DB: db}
	st := &repo.EntitySettingsRepo{DB: db}
	h := &AgentHandler{EntityKB: kb, EntitySettings: st}
	r := chi.NewRouter()
	registerEntityRoutes(r, h)
	return r, kb, st
}

// TestEntitySettingsAPI：GET 默认值 → PUT 改配置 → GET 读回；非法阈值 400。
func TestEntitySettingsAPI(t *testing.T) {
	r, _, _ := newEntityTestServer(t)
	// GET 默认
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/api/agent/entity-settings", nil))
	if rec.Code != 200 {
		t.Fatalf("GET 默认: %d", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["correction_enabled"] != true || got["confidence_threshold"] != 0.8 {
		t.Fatalf("默认配置不符: %v", got)
	}
	// PUT
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/agent/entity-settings",
		strings.NewReader(`{"correction_enabled":false,"confidence_threshold":0.9,"auto_sources":["person","pet"]}`)))
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	// GET 读回
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/api/agent/entity-settings", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["correction_enabled"] != false {
		t.Fatalf("读回不符: %v", got)
	}
	// 非法阈值
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/agent/entity-settings",
		strings.NewReader(`{"confidence_threshold":1.5}`)))
	if rec.Code != 400 {
		t.Fatalf("非法阈值应 400: %d", rec.Code)
	}
}

// TestEntityCRUDAPI：POST 手动实体 → GET 列表 → PATCH 改名/禁用 → DELETE。
func TestEntityCRUDAPI(t *testing.T) {
	r, _, _ := newEntityTestServer(t)
	// POST
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/api/agent/entities",
		strings.NewReader(`{"canonical":"天枢","kind":"custom","note":"内部代号"}`)))
	if rec.Code != 200 {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body.String())
	}
	var created repo.Entity
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == 0 || created.Source != repo.EntitySourceManual {
		t.Fatalf("创建不符: %+v", created)
	}
	// GET
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/api/agent/entities", nil))
	var list struct{ Entities []repo.Entity }
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Entities) != 1 {
		t.Fatalf("列表应 1 条: %+v", list)
	}
	id := created.ID.String()
	// PATCH 改名
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("PATCH", "/api/agent/entities/"+id,
		strings.NewReader(`{"canonical":"天璇","note":"改名了"}`)))
	if rec.Code != 200 {
		t.Fatalf("PATCH: %d", rec.Code)
	}
	// DELETE
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/agent/entities/"+id, nil))
	if rec.Code != 204 {
		t.Fatalf("DELETE: %d", rec.Code)
	}
}
```

> 注意：`registerEntityRoutes` 的 uid 来源——既有 `/api/agent/*` 端点都用 `reqUserID(r)`（authGate 注入）。测试里直接调 handler 时 ctx 无 uid 会 401；照既有 handlers_test 的做法注入（先 `grep -n "auth.UserID\|WithUser\|uidKey" internal/auth/*.go internal/agent/handlers_test.go` 找到现成注入 helper 再用）。落地时以能跑通为准调整装配，断言不变。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run TestEntity -v`
Expected: 编译失败 `registerEntityRoutes undefined`。

- [ ] **Step 3: 实现 internal/agent/entity.go**

```go
// entity 端点：设置页「专有名词」子区的后端——实体纠错配置 + 手动实体 CRUD。
// 路由挂在 RegisterAgent（见 handlers.go）；鉴权/JSON 辅助复用 handlers.go 的 reqUserID/writeJSON。
package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/entity"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// registerEntityRoutes 挂实体端点（RegisterAgent 调用；测试单独装配也走这里）。
func registerEntityRoutes(r chi.Router, h *AgentHandler) {
	r.Get("/api/agent/entity-settings", h.getEntitySettings)
	r.Put("/api/agent/entity-settings", h.putEntitySettings)
	r.Get("/api/agent/entities", h.listEntities)
	r.Post("/api/agent/entities", h.createEntity)
	r.Patch("/api/agent/entities/{id}", h.patchEntity)
	r.Delete("/api/agent/entities/{id}", h.deleteEntity)
}

// getEntitySettings 返回纠错配置 + 各 kind 实体数汇总（设置页一次拉齐）。
func (h *AgentHandler) getEntitySettings(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntitySettings == nil || h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	st, err := h.EntitySettings.Get(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	counts, err := h.EntityKB.CountByKind(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"correction_enabled": st.CorrectionEnabled,
		"confidence_threshold": st.ConfidenceThreshold,
		"auto_sources":        st.AutoSources,
		"counts_by_kind":      counts,
	})
}

// putEntitySettings 保存纠错配置。threshold 越界 400。
func (h *AgentHandler) putEntitySettings(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntitySettings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	var body struct {
		CorrectionEnabled   *bool     `json:"correction_enabled"`
		ConfidenceThreshold *float64  `json:"confidence_threshold"`
		AutoSources         *[]string `json:"auto_sources"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	// 按指针合并：未传的字段保持现值（与 PUT 语义对齐——只改传了的）。
	cur, err := h.EntitySettings.Get(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	enabled, threshold, sources := cur.CorrectionEnabled, cur.ConfidenceThreshold, cur.AutoSources
	if body.CorrectionEnabled != nil {
		enabled = *body.CorrectionEnabled
	}
	if body.ConfidenceThreshold != nil {
		if *body.ConfidenceThreshold < 0 || *body.ConfidenceThreshold > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confidence_threshold 须在 [0,1]"})
			return
		}
		threshold = *body.ConfidenceThreshold
	}
	if body.AutoSources != nil {
		sources = *body.AutoSources
	}
	if err := h.EntitySettings.Upsert(r.Context(), uid, enabled, threshold, sources); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"correction_enabled": enabled, "confidence_threshold": threshold, "auto_sources": sources,
	})
}

// listEntities 列实体（?kind= 过滤；含 auto+manual+禁用行，前端分组展示）。
func (h *AgentHandler) listEntities(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	list, err := h.EntityKB.List(r.Context(), uid, r.URL.Query().Get("kind"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entities": list})
}

// createEntity 手动新增专有名词（拼音/音素键在服务端算，客户端不传）。
func (h *AgentHandler) createEntity(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	var body struct {
		Canonical string `json:"canonical"`
		Kind      string `json:"kind"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	body.Canonical = strings.TrimSpace(body.Canonical)
	if body.Canonical == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "canonical required"})
		return
	}
	if body.Kind == "" {
		body.Kind = repo.EntityKindCustom
	}
	e := &repo.Entity{UserID: uid, Canonical: body.Canonical, Kind: body.Kind, Enabled: true}
	py := entity.NormalizePinyin(body.Canonical)
	if py != "" {
		e.Pinyin = &py
	}
	if lt := entity.NormalizeLatin(body.Canonical); lt != "" && lt != py {
		e.Metaphone = &lt
	}
	if body.Note != "" {
		e.Note = &body.Note
	}
	if err := h.EntityKB.CreateManual(r.Context(), e); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// patchEntity 改手动实体（canonical/note/enabled；改名重算拼音键）。auto 条目只接受 enabled。
func (h *AgentHandler) patchEntity(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var body struct {
		Canonical *string `json:"canonical"`
		Note      *string `json:"note"`
		Enabled   *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	// enabled：manual/auto 都可改（临时禁用某 auto 实体）。
	if body.Enabled != nil {
		if err := h.EntityKB.SetEnabled(r.Context(), uid, id, *body.Enabled); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
			return
		}
	}
	// canonical/note：只许改 manual（auto 由刷新重建，改了也会被覆盖）。
	if body.Canonical != nil || body.Note != nil {
		cur, err := h.EntityKB.Get(r.Context(), uid, id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
			return
		}
		if cur.Source != repo.EntitySourceManual {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "自动同步的实体不可改名，可禁用或改来源数据"})
			return
		}
		canonical, note := cur.Canonical, ""
		if cur.Note != nil {
			note = *cur.Note
		}
		if body.Canonical != nil {
			canonical = strings.TrimSpace(*body.Canonical)
			if canonical == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "canonical required"})
				return
			}
		}
		if body.Note != nil {
			note = *body.Note
		}
		if err := h.EntityKB.UpdateManual(r.Context(), uid, id, canonical, note); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	full, err := h.EntityKB.Get(r.Context(), uid, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
		return
	}
	writeJSON(w, http.StatusOK, full)
}

// deleteEntity 删除实体（manual 删除；auto 也可删但下次刷新会回来——禁用用 PATCH enabled=false）。
func (h *AgentHandler) deleteEntity(w http.ResponseWriter, r *http.Request) {
	uid, ok := reqUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if h.EntityKB == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "专有名词配置不可用"})
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := h.EntityKB.Delete(r.Context(), uid, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

import 补 `"database/sql"`；`errors` 已有则不重复。

- [ ] **Step 4: handlers.go 注册**

1) `AgentHandler` struct 加字段（Skills 字段后）：

```go
	EntityKB       *repo.EntityKBRepo       // 实体知识库（设置页「专有名词」）；nil 时实体端点 503
	EntitySettings *repo.EntitySettingsRepo // 实体纠错配置；nil 时 503
```

2) `RegisterAgent` 里、技能路由块后加一行：

```go
	registerEntityRoutes(r, h) // 专有名词：纠错配置 + 手动实体 CRUD（设置页）
```

- [ ] **Step 5: main.go 装配 + 跑测试**

`cmd/zhiwei-server/main.go` 的 `agent.RegisterAgent(...)` 字面量加：

```go
			EntityKB:       entityKB,
			EntitySettings: entitySettings,
```

（`entityKB`/`entitySettings` 已在 Task 10 构造；注意它们在 `if cfg.AgentEnabled` 块外、RegisterAgent 在块内——若作用域不通，把两个 repo 的构造挪进块内或提前到块外皆可，以编译通过为准。）

Run: `go build ./... && go test ./internal/agent/ -run TestEntity -v`
Expected: 编译通过、测试 PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/agent/entity.go internal/agent/entity_test.go internal/agent/handlers.go cmd/zhiwei-server/main.go
git commit -m "feat(api): /api/agent/entity-settings + entities CRUD——设置页专有名词后端"
```

---

### Task 12: 会话详情下发 entity_edits

**Files:**
- Modify: `internal/api/query.go`（segmentView 加字段 + GetSession 富化）

- [ ] **Step 1: segmentView 加字段**

`internal/api/query.go` 的 `segmentView` struct，`CorrectedReason` 字段后追加：

```go
	// EntityEdits 实体纠错明细（correct stage 落库的 JSON 原样透传，corrected_reason='entity'
	// 时非空）：[{orig, corrected, canonical, confidence}]，前端渲染「原文(删除线)→纠正后」对照。
	EntityEdits json.RawMessage `json:"entity_edits,omitempty"`
```

import 补 `"encoding/json"`（若未引入）。

- [ ] **Step 2: GetSession 富化**

`GetSession` 的段循环里（`if sg.CorrectedReason != nil {...}` 块后）追加：

```go
			if len(sg.EntityEdits) > 0 {
				views[i].EntityEdits = json.RawMessage(sg.EntityEdits)
			}
```

- [ ] **Step 3: 验证**

Run: `go build ./... && go test ./internal/api/`
Expected: 编译通过、既有测试 PASS（若有 GetSession 相关测试跑之；纯增量字段不破坏既有断言）。

- [ ] **Step 4: Commit**

```bash
git add internal/api/query.go
git commit -m "feat(api): 会话详情段下发 entity_edits 纠错明细"
```

---

### Task 13: 前端——设置页「专有名词」子区 + 转写纠错对照

**Files:**
- Modify: `web/index.html`（设置面板加子区；转写段渲染加对照）
- Modify: `web/app.js`（状态/加载/保存函数 + setup 暴露）

- [ ] **Step 1: app.js 加状态与函数**

在 `saveAgentConfig` 函数后追加（约 :2972 处）：

```js
    // ---------- 设置：专有名词（实体纠错；后端 /api/agent/entity-settings + /api/agent/entities） ----------
    // 后端契约：GET /api/agent/entity-settings → {correction_enabled, confidence_threshold, auto_sources, counts_by_kind}；
    // PUT 同名字段（未传字段保持现值）；GET /api/agent/entities → {entities:[...]}；
    // POST {canonical, kind, note}；PATCH /{id} {canonical?,note?,enabled?}；DELETE /{id}。
    const entCfg = ref({ correction_enabled: true, confidence_threshold: 0.8, auto_sources: [], counts_by_kind: {} });
    const entSaving = ref(false);
    const entList = ref([]);           // 全部实体（auto+manual）
    const entNewCanonical = ref('');   // 手动新增输入
    const entNewNote = ref('');
    const entKindLabels = { person: '人物', pet: '宠物', project: '项目', task: '待办', topic: '话题', speaker: '说话人', custom: '自定义' };
    async function loadEntities() {
      try {
        const [st, list] = await Promise.all([api('GET', '/api/agent/entity-settings'), api('GET', '/api/agent/entities')]);
        entCfg.value = Object.assign(entCfg.value, st, { counts_by_kind: st.counts_by_kind || {} });
        entList.value = (list && list.entities) || [];
      } catch (e) { showError(e); }
    }
    async function saveEntitySettings() {
      if (entSaving.value) return;
      entSaving.value = true;
      try {
        await api('PUT', '/api/agent/entity-settings', {
          correction_enabled: entCfg.value.correction_enabled,
          confidence_threshold: entCfg.value.confidence_threshold,
          auto_sources: entCfg.value.auto_sources,
        });
        await loadEntities();
        notify('专有名词设置已保存', 2000);
      } catch (e) { showError(e); }
      finally { entSaving.value = false; }
    }
    async function addEntity() {
      const name = (entNewCanonical.value || '').trim();
      if (!name) return;
      try {
        await api('POST', '/api/agent/entities', { canonical: name, kind: 'custom', note: (entNewNote.value || '').trim() });
        entNewCanonical.value = ''; entNewNote.value = '';
        await loadEntities();
      } catch (e) { showError(e); }
    }
    async function toggleEntity(e) {
      try { await api('PATCH', '/api/agent/entities/' + e.id, { enabled: !e.enabled }); await loadEntities(); }
      catch (err) { showError(err); }
    }
    async function removeEntity(e) {
      try { await api('DELETE', '/api/agent/entities/' + e.id); await loadEntities(); }
      catch (err) { showError(err); }
    }
```

注意：`notify` 若与该文件既有提示 helper 名不同（先 `grep -n "function notify" web/app.js`），换成实际名。

`switchTab` 的 settings 分支（约 :3218 `if (name === 'settings') { loadAgentConfig(); loadMCP(); loadSkills(); }`）追加 `loadEntities()`：

```js
      if (name === 'settings') { loadAgentConfig(); loadMCP(); loadSkills(); loadEntities(); }
```

`setup()` 的 return 对象（约 :3272 附近，`loadAgentConfig, saveAgentConfig,` 行后）追加：

```js
      entCfg, entSaving, entList, entNewCanonical, entNewNote, entKindLabels,
      loadEntities, saveEntitySettings, addEntity, toggleEntity, removeEntity,
```

- [ ] **Step 2: index.html 设置面板加子区**

在「知微人设」面板块（`<h2 style="margin:0 0 4px">知微人设</h2>` 所在 card）之后、设置面板容器内追加一个新 card：

```html
      <!-- ============ 专有名词（ASR 实体纠错） ============ -->
      <div class="card" style="margin-top:14px">
        <h2 style="margin:0 0 4px">专有名词</h2>
        <div class="muted" style="font-size:var(--fs-xs); margin-bottom:8px">
          ASR 常把人名/花名/项目代号听错。开启后，转写中发音相近的片段会按下方实体库自动纠正，纠正处在时间线里标注。
        </div>
        <div style="display:flex; align-items:center; gap:8px; margin-bottom:8px; flex-wrap:wrap">
          <label style="display:flex; align-items:center; gap:4px">
            <input type="checkbox" v-model="entCfg.correction_enabled"> 实体纠错
          </label>
          <label style="display:flex; align-items:center; gap:4px; font-size:var(--fs-xs)" title="低于该置信度的纠正建议不落地">
            置信度阈值 <input type="number" v-model.number="entCfg.confidence_threshold" min="0" max="1" step="0.05" style="width:64px">
          </label>
          <button class="btn" @click="saveEntitySettings" :disabled="entSaving">{{ entSaving ? '保存中…' : '保存设置' }}</button>
        </div>
        <div class="muted" style="font-size:var(--fs-xs); margin-bottom:8px">
          自动收录（下次录音时刷新）：
          <span v-for="(n, k) in entCfg.counts_by_kind" :key="k" style="margin-right:8px">{{ entKindLabels[k] || k }} {{ n }}</span>
        </div>
        <div style="display:flex; gap:6px; margin-bottom:8px; flex-wrap:wrap">
          <input class="txt" v-model="entNewCanonical" placeholder="补充专有名词（内部代号/产品别名等）" style="flex:1; min-width:160px">
          <input class="txt" v-model="entNewNote" placeholder="备注（可选）" style="width:120px">
          <button class="btn" @click="addEntity">添加</button>
        </div>
        <div v-for="e in entList" :key="e.id" style="display:flex; align-items:center; gap:6px; padding:3px 0; border-top:1px solid var(--line)">
          <span :class="{muted: !e.enabled}" :style="e.enabled ? '' : 'text-decoration:line-through'">{{ e.canonical }}</span>
          <span class="muted" style="font-size:var(--fs-xs)">{{ entKindLabels[e.kind] || e.kind }} · {{ e.source === 'manual' ? '手动' : '自动' }}<template v-if="e.note"> · {{ e.note }}</template></span>
          <span style="flex:1"></span>
          <button class="btn btn-sm" @click="toggleEntity(e)" :title="e.enabled ? '禁用（不参与纠错）' : '启用'">{{ e.enabled ? '禁用' : '启用' }}</button>
          <button class="btn btn-sm" @click="removeEntity(e)">删除</button>
        </div>
      </div>
```

（`card`/`btn-sm` 等 class 若该文件没有，先 `grep -n "btn-sm\|class=\"card\"" web/index.html` 对齐既有样式类；没有就内联样式替代。）

- [ ] **Step 3: 转写段纠错对照展示**

`web/index.html` 转写详情段渲染处（:657 `corrected-badge` 所在行），徽章 tooltip 与对照行。把该 `v-if` 行的 tooltip 表达式改为含 entity 分支（原样保留其余分支），并在徽章 `<span ...>已修改</span>` 后追加对照明细：

```html
                <span v-if="sg.corrected_reason || sg.corrected_from" class="corrected-badge"
                      :title="sg.corrected_reason === 'short' ? '过短段自动并入最近说话人（声纹自动纠正）' : (sg.corrected_reason === 'entity' ? '实体纠错：专有名词已按实体库自动纠正（原文见删除线）' : (sg.corrected_from_name ? ('原判定：' + sg.corrected_from_name + '（声纹自动纠正）') : '声纹自动纠正（原说话人已不可用）'))">已修改</span>
                <!-- 实体纠错对照：原文(删除线) → 纠正后（entity_edits 来自后端，corrected_reason='entity' 才有） -->
                <template v-if="sg.corrected_reason === 'entity' && (sg.entity_edits||[]).length">
                  <span v-for="(ed, ei) in sg.entity_edits" :key="ei"
                        :title="'纠错依据：' + (ed.reason || '发音相近') + '（置信度 ' + (ed.confidence || 0).toFixed(2) + '）'"
                        style="margin-left:4px">
                    <s style="opacity:.55">{{ ed.orig }}</s> → <b>{{ ed.corrected }}</b>
                  </span>
                </template>
```

同时把该行外层容器 `v-if`（:646 `(sg.voice_matches||[]).length || sg.corrected_reason || sg.corrected_from`）与 `:title`（:647）里的 tooltip 同步加 entity 分支（复用上面同一表达式，保持两处 tooltip 一致——该行容器原本就有 `sg.corrected_reason` 分支结构，把 entity 分支插在 'short' 判断旁边即可）。

- [ ] **Step 4: 重新 hash 前端 + 手工验证**

```bash
make hash-web
```

Run: `make dev-restart`（或 `go run ./cmd/zhiwei-server`），浏览器打开设置页验证：
1. 「专有名词」子区出现，默认开关开、各 kind 计数显示（初始可能全 0——实体库在下次录音的 correct stage 才刷新；可手动添加一个实体立即看到列表行）。
2. 添加/禁用/删除手动实体生效。
3. 上传一段含专有名词的录音走完流水线后，时间线详情段上出现「已修改」徽章 + 原文删除线对照。

- [ ] **Step 5: Commit**

```bash
git add web/index.html web/app.js
git commit -m "feat(web): 设置页「专有名词」子区 + 转写实体纠错对照展示"
```

---

### Task 14: 收尾全量验证

- [ ] **Step 1: 全量构建 + vet + 单测**

```bash
go build ./... && go vet ./... && go test ./internal/entity/ ./internal/pipeline/ ./internal/repo/ ./internal/agent/ ./internal/api/
```
Expected: 全部 PASS。

- [ ] **Step 2: 集成测试（docker MySQL）**

```bash
make test-integration 2>&1 | tail -30
```
Expected: 全部 PASS（迁移 000025 在隔离库自动生效）。

- [ ] **Step 3: e2e 冒烟（可选但推荐）**

准备一段含已知专有名词的录音（或 TTS 生成「明天让张梦瑜评审天枢方案」），`ZW_ENTITY_CORRECT_ENABLED=true` 起服务上传，确认：
- `transcript_segment.entity_edits` 有明细、`corrected_reason='entity'`；
- 时间线详情显示对照；`full_text` 含纠正后文本；
- 关掉 `ZW_ENTITY_CORRECT_ENABLED` 重启后，流水线 stage 列表无 correct（日志可证）。

- [ ] **Step 4: 更新 memory 与收尾 commit**

把「ASR 实体纠错」特性要点写入 memory（分支、迁移号、关键决策：LLM 一程裁决/直接改写标记/Jaro-Winkler 替代 Double Metaphone/召回默认 minSim 实测值），然后：

```bash
git add -A
git commit -m "chore: ASR 实体纠错一期收尾"
```

---

## 验收清单（对照 spec）

- [ ] 链路：原始 ASR 文本 → 拼音/音素召回白名单 → LLM 严格约束修正 → 门控改写（spec §1）
- [ ] 只改实体不改整句、保留时序/断句（orig 首次出现替换、不跨段）（spec §2）
- [ ] `entity_kb`/`entity_settings`/`entity_edits` 落库（spec §5）
- [ ] 种子来源全覆盖：person+aliases+relationship.label、pet name+nickname、current_projects、todo 标题、topic、speaker（spec §6）
- [ ] 白名单为空跳过 LLM（spec §7）
- [ ] 双重门控：confidence ≥ 阈值 + orig 原样在段内 + corrected ∈ 白名单（spec §9）
- [ ] best-effort 不阻塞流水线、幂等重跑（spec §13）
- [ ] 前端徽章 + 原文删除线对照（spec §10）
- [ ] 设置页：开关/阈值/自动来源汇总/手动 CRUD（spec §11）
- [ ] env 开关 ZW_ENTITY_CORRECT_ENABLED（spec §12）
- [ ] 测试库隔离，不碰共享 zhiwei 库（memory 约定）
