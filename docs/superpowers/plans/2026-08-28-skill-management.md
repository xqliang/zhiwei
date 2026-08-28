# 技能（Skills）管理（二期）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 从 skills.sh 搜索/安装 Anthropic 格式技能到本地，dsh skills 插件（启用）热加载生效；设置页提供查看/启禁/删除。

**Architecture:** cordis 基模板一次性启用 dsh skills 插件（`customSkillDirs` ← env `ZW_AGENT_SKILL_DIR` 指向 `data/agent-skills/enabled/`）；管理动作全部是文件操作（安装=tarball 解压落盘、启禁=enabled↔disabled 目录 rename、删除=删目录），chokidar 热生效，**零边车补丁、零 RPC**。DB 表 `agent_skill` 记元数据（000023）。

**Tech Stack:** Go（archive/tar + compress/gzip + net/http 标准库）、chi、sqlx、golang-migrate、Vue 3 CDN。

**规格来源：** `docs/superpowers/specs/2026-08-28-skill-management-design.md`
**分支：** `feat/agent-skill-manage`（worktree 已建，基线 a2cdce1）
**执行方式备注：** 子 agent 配额可能耗尽——配额可用则逐 task 派发，耗尽则主会话内联执行，TDD 步骤不变。
**测试库：** `make init-testdb` + `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true"`。

---

## File Structure

- Create `migrations/000023_agent_skill.up.sql` / `.down.sql`
- Create `internal/repo/agent_skill.go` + `internal/repo/agent_skill_test.go`
- Create `internal/agent/skillinstall.go`（tarball 拉取/解压/校验/原子落盘 + skills.sh 搜索代理 + frontmatter 简易解析）+ `skillinstall_test.go`
- Create `internal/agent/skill_handlers.go`（/api/agent/skills* 端点 + 启禁/删除的磁盘组合）+ `skill_handlers_test.go`
- Modify `services/agent-sidecar/cordis.agent.yml`（skills.enabled true + customSkillDirs env）
- Modify `internal/agent/runtime.go`（RuntimeConfig.SkillDir + dshEnv 注入 ZW_AGENT_SKILL_DIR）
- Modify `internal/config/config.go`（AgentSkillRoot，env ZW_AGENT_SKILL_ROOT，默认 ./data/agent-skills）
- Modify `cmd/zhiwei-server/main.go`（装配 SkillInstaller + handler 字段 + RuntimeConfig.SkillDir + 启动建目录）
- Modify `.gitignore`（data/agent-skills 已被 data/ 覆盖——确认即可，可能无需改）
- Modify `web/index.html` + `web/app.js`（设置页「技能」子区）

---

## Task 1: 迁移 `000023_agent_skill` + `AgentSkillRepo`

**Files:**
- Create: `migrations/000023_agent_skill.up.sql`、`migrations/000023_agent_skill.down.sql`
- Create: `internal/repo/agent_skill.go`
- Test: `internal/repo/agent_skill_test.go`

- [ ] **Step 1: up 迁移**

`migrations/000023_agent_skill.up.sql`:
```sql
-- 全局技能（Agent Skills）清单：dsh skills 插件从 data/agent-skills/enabled/ 热加载 SKILL.md，
-- 本表记元数据（来源/描述/启禁态/全文预览）。安装/启禁/删除经 /api/agent/skills 管理，
-- 磁盘是生效真源、DB 是元数据镜像（先磁盘后 DB，见 spec §6）。
CREATE TABLE agent_skill (
  id           BIGINT UNSIGNED NOT NULL,
  name         VARCHAR(64)  NOT NULL,
  display_name VARCHAR(128) NOT NULL DEFAULT '',
  source       VARCHAR(255) NOT NULL DEFAULT '',
  description  TEXT NOT NULL,
  enabled      TINYINT(1) NOT NULL DEFAULT 1,
  content      MEDIUMTEXT NOT NULL,
  installed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_agent_skill_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- [ ] **Step 2: down 迁移**

`migrations/000023_agent_skill.down.sql`:
```sql
DROP TABLE IF EXISTS agent_skill;
```

- [ ] **Step 3: 失败测试** — `internal/repo/agent_skill_test.go`:
```go
package repo_test

import (
	"context"
	"testing"

	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

func agentSkillRepo(t *testing.T) *repo.AgentSkillRepo {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &repo.AgentSkillRepo{DB: db}
}

func TestAgentSkillCRUD(t *testing.T) {
	r := agentSkillRepo(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = dbx(r).Exec("DELETE FROM agent_skill WHERE name LIKE 'test-%'") })

	s := &repo.AgentSkill{
		Name: "test-git-commit", DisplayName: "Git Commit", Source: "github/awesome-copilot/git-commit",
		Description: "提交规范", Content: "---\nname: test-git-commit\n---\n正文",
	}
	if err := r.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID.Int64() == 0 {
		t.Error("Create 应回填 ID")
	}

	got, err := r.Get(ctx, s.ID)
	if err != nil || got.Name != "test-git-commit" || got.Enabled != true {
		t.Fatalf("Get: %+v err=%v", got, err)
	}

	if err := r.SetEnabled(ctx, s.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, _ = r.Get(ctx, s.ID)
	if got.Enabled {
		t.Error("SetEnabled(false) 未生效")
	}

	if err := r.Delete(ctx, s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, s.ID); err == nil {
		t.Error("删除后 Get 应 ErrNoRows")
	}
}

// dbx 取底层 *sqlx.DB 供 cleanup（repo 不暴露 DB 字段访问器的场景下直接断言表状态）。
func dbx(r *repo.AgentSkillRepo) interface{ Exec(q string, a ...any) (sql.Result, error) } {
	return r.DB
}
```
注：`repo.AgentSkillRepo` 的 `DB` 是导出字段（对齐 `MCPServerRepo`），`dbx` 辅助直接返回 `r.DB`（`*sqlx.DB`），import 需 `database/sql`。若写法别扭，直接在测试里持有 `db`：
```go
db, _ := repo.NewDB(repotest.DSN(t))
r := &repo.AgentSkillRepo{DB: db}
t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_skill WHERE name LIKE 'test-%'") })
```
（采用这个简单写法，删掉 dbx。）

- [ ] **Step 4: 验证 FAIL**：`make init-testdb && TEST_MYSQL_DSN=... go test ./internal/repo/ -run TestAgentSkillCRUD -v` → 编译失败 `undefined: repo.AgentSkillRepo`。

- [ ] **Step 5: 实现** — `internal/repo/agent_skill.go`:
```go
package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentSkill 是一条已安装技能的元数据（磁盘真源在 <skillRoot>/<enabled|disabled>/<name>/，
// 本表是元数据镜像：查看/列表免读盘、启禁态持久化）。
type AgentSkill struct {
	ID          ids.ID     `db:"id" json:"id"`
	Name        string     `db:"name" json:"name"`         // dsh 技能名（=frontmatter name=目录名）
	DisplayName string     `db:"display_name" json:"display_name"`
	Source      string     `db:"source" json:"source"`     // 'owner/repo/skill' 或 'manual'
	Description string     `db:"description" json:"description"`
	Enabled     bool       `db:"enabled" json:"enabled"`
	Content     string     `db:"content" json:"content"`   // SKILL.md 全文（查看用）
	InstalledAt time.Time  `db:"installed_at" json:"installed_at"`
	UpdatedAt   time.Time  `db:"updated_at" json:"updated_at"`
}

type AgentSkillRepo struct{ DB *sqlx.DB }

// List 全部技能，启用在前、按安装时间。
func (r *AgentSkillRepo) List(ctx context.Context) ([]AgentSkill, error) {
	var rows []AgentSkill
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM agent_skill ORDER BY enabled DESC, installed_at ASC`)
	return rows, err
}

// Get 按 id 查。
func (r *AgentSkillRepo) Get(ctx context.Context, id ids.ID) (*AgentSkill, error) {
	var s AgentSkill
	err := r.DB.GetContext(ctx, &s, `SELECT * FROM agent_skill WHERE id = ?`, id.Int64())
	return &s, err
}

// Create 新增（雪花 ID）。name 唯一由 DB 约束保证。
func (r *AgentSkillRepo) Create(ctx context.Context, s *AgentSkill) error {
	s.ID = ids.New()
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO agent_skill (id, name, display_name, source, description, enabled, content)
VALUES (:id, :name, :display_name, :source, :description, :enabled, :content)`, s)
	return err
}

// SetEnabled 启/禁（仅 DB；磁盘 rename 由 service 层做）。不存在 → ErrNoRows。
func (r *AgentSkillRepo) SetEnabled(ctx context.Context, id ids.ID, enabled bool) error {
	res, err := r.DB.ExecContext(ctx, `UPDATE agent_skill SET enabled = ? WHERE id = ?`, enabled, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete 删行（磁盘删除由 service 层做）。不存在 → ErrNoRows。
func (r *AgentSkillRepo) Delete(ctx context.Context, id ids.ID) error {
	res, err := r.DB.ExecContext(ctx, `DELETE FROM agent_skill WHERE id = ?`, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
```

- [ ] **Step 6: 验证 PASS** + `go build ./... && go vet ./internal/repo/...`。

- [ ] **Step 7: Commit** `feat(skill): 000023 agent_skill 表 + AgentSkillRepo`

---

## Task 2: dsh 启用 skills 插件 + 配置/env 链路

**Files:**
- Modify: `services/agent-sidecar/cordis.agent.yml`
- Modify: `internal/agent/runtime.go`
- Modify: `internal/config/config.go`
- Modify: `cmd/zhiwei-server/main.go`

- [ ] **Step 1: cordis 基模板启用 skills**

`cordis.agent.yml` agent-spine 块（原 `skills:\n      enabled: false`）替换为：
```yaml
    skills:
      enabled: true
      filesystem:
        customSkillDirs:
          # 绝对路径由 Go 侧经 env 注入（ZW_AGENT_SKILL_DIR=<skillRoot>/enabled）；
          # 兜底相对路径仅保证无 env 时配置合法（目录不存在=无技能，无害）。
          - !!js process.env.ZW_AGENT_SKILL_DIR ?? './.dsh/skills'
```

- [ ] **Step 2: RuntimeConfig + dshEnv**

`internal/agent/runtime.go`：
- `RuntimeConfig` 加字段：
```go
	SkillDir string // ZW_AGENT_SKILL_DIR（dsh skills 插件的 customSkillDirs，指 <skillRoot>/enabled）
```
- `dshEnv()` 返回切片追加（MCPURL 行后）：
```go
		"ZW_AGENT_SKILL_DIR=" + r.cfg.SkillDir,
```

- [ ] **Step 3: config 字段**

`internal/config/config.go`：
- struct 加：
```go
	AgentSkillRoot string // ZW_AGENT_SKILL_ROOT：技能磁盘根（enabled/ + disabled/ 子目录），dsh 热加载源
```
- Load() 加：
```go
		AgentSkillRoot: getenv("ZW_AGENT_SKILL_ROOT", "./data/agent-skills"),
```

- [ ] **Step 4: main.go 接线**

`cmd/zhiwei-server/main.go`（mcpServerRepo/regenCordis 附近，agentPool 创建前）：
```go
	// 技能磁盘布局：<AgentSkillRoot>/enabled（dsh 监听根，经 env 注入）+ /disabled（移出即对模型不可见）。
	skillEnabledDir := filepath.Join(cfg.AgentSkillRoot, "enabled")
	skillDisabledDir := filepath.Join(cfg.AgentSkillRoot, "disabled")
	if err := os.MkdirAll(skillEnabledDir, 0o755); err != nil {
		log.Fatalf("建技能目录: %v", err)
	}
	_ = os.MkdirAll(skillDisabledDir, 0o755)
```
`agentPool := agent.NewRuntimePool(agent.RuntimeConfig{...})` 加：
```go
		SkillDir:      skillEnabledDir,
```
（确认 `path/filepath` 已 import，main.go 已用 os。）

- [ ] **Step 5: 验证**：`go build ./... && go vet ./...`；`grep -n "skills:" services/agent-sidecar/cordis.agent.yml` 确认 yml 改对。

- [ ] **Step 6: Commit** `feat(skill): dsh 启用 skills 插件（customSkillDirs 经 ZW_AGENT_SKILL_DIR 注入）`

---

## Task 3: 安装器 `skillinstall.go`（tarball + 校验 + 原子落盘 + 搜索代理）

**Files:**
- Create: `internal/agent/skillinstall.go`
- Test: `internal/agent/skillinstall_test.go`

- [ ] **Step 1: 失败测试** — `internal/agent/skillinstall_test.go`:
```go
package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTarGz 构造 codeload 风格 tarball（根目录 <repo>-<ref>/，内含 skills/<name>/...）。
func buildTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for path, content := range entries {
		hdr := &tar.Header{Name: "repo-main/" + path, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const goodSKILL = "---\nname: test-skill\ndescription: 测试技能说明\n---\n\n# 使用说明\n正文。"

func newSkillInstaller(t *testing.T, tarball []byte, statusMain int) (*SkillInstaller, string) {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/refs/heads/main") && statusMain != 0 {
			w.WriteHeader(statusMain)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	inst := NewSkillInstaller(srv.URL, srv.URL, root) // codeloadBase / skillsShBase 可注入（测试指向 httptest）
	return inst, root
}

func TestInstallSkillHappyPath(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"skills/test-skill/SKILL.md":       goodSKILL,
		"skills/test-skill/reference.md":   "# 参考",
		"skills/other-skill/SKILL.md":      "---\nname: other-skill\ndescription: 别的\n---\n正文",
		"README.md":                        "# repo",
	})
	inst, root := newSkillInstaller(t, tarball, 0)
	s, err := inst.Install(context.Background(), "acme/repo/test-skill")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if s.Name != "test-skill" || s.Description != "测试技能说明" || !s.Enabled {
		t.Errorf("安装结果异常: %+v", s)
	}
	// 落盘：enabled/test-skill/{SKILL.md,reference.md}，无关文件（other-skill/README）不落
	for _, f := range []string{"enabled/test-skill/SKILL.md", "enabled/test-skill/reference.md"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("缺文件 %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "enabled/other-skill")); err == nil {
		t.Error("不应落其它技能目录")
	}
	if _, err := os.Stat(filepath.Join(root, "enabled/test-skill/../.tmp")); err == nil {
		t.Error("暂存目录应已清理")
	}
	// 重复安装被拒
	if _, err := inst.Install(context.Background(), "acme/repo/test-skill"); err == nil {
		t.Error("重复安装应报错")
	}
}

func TestInstallSkillBadFrontmatter(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"skills/test-skill/SKILL.md": "---\nname: WrongName\ndescription: x\n---\n正文", // 非 kebab-case
	})
	inst, _ := newSkillInstaller(t, tarball, 0)
	if _, err := inst.Install(context.Background(), "acme/repo/test-skill"); err == nil {
		t.Error("name 非 kebab-case 应报错")
	}
}

func TestInstallSkillRejectsPathTraversal(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"skills/test-skill/SKILL.md":  goodSKILL,
		"skills/test-skill/evil.md":   "x",
	})
	// 构造含 ../ 的恶意条目
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct{ name, body string }{
		{"repo-main/skills/test-skill/SKILL.md", goodSKILL},
		{"repo-main/skills/test-skill/../../evil.sh", "#!/bin/sh"},
	} {
		_ = tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))})
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	inst, root := newSkillInstaller(t, buf.Bytes(), 0)
	if _, err := inst.Install(context.Background(), "acme/repo/test-skill"); err == nil {
		t.Error("路径穿越条目应报错")
	}
	if _, err := os.Stat(filepath.Join(root, "evil.sh")); err == nil {
		t.Error("穿越文件不应落盘")
	}
	// 暂存目录清理
	m, _ := filepath.Glob(filepath.Join(root, ".tmp-*"))
	if len(m) > 0 {
		t.Errorf("暂存应清理: %v", m)
	}
}

func TestInstallSkillMainFallbackMaster(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{"skills/test-skill/SKILL.md": goodSKILL})
	inst, _ := newSkillInstaller(t, tarball, http.StatusNotFound) // main 404 → 回退 master 同一 tarball
	if _, err := inst.Install(context.Background(), "acme/repo/test-skill"); err != nil {
		t.Fatalf("master 回退安装: %v", err)
	}
}

func TestSearchSkillsProxies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "git" {
			t.Errorf("应透传 q: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"skills":[{"id":"a/b/c","name":"c","installs":1,"source":"a/b"}]}`))
	}))
	t.Cleanup(srv.Close)
	inst := NewSkillInstaller("http://x", srv.URL, t.TempDir())
	res, err := inst.Search(context.Background(), "git")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ID != "a/b/c" {
		t.Errorf("搜索结果异常: %+v", res)
	}
}

func TestParseFrontmatter(t *testing.T) {
	// 三种形态：无引号/单引号/双引号 description；缺 name 拒绝
	name, desc, err := parseFrontmatter("---\nname: abc\ndescription: plain text\n---\nbody")
	if err != nil || name != "abc" || desc != "plain text" {
		t.Errorf("plain: %q %q %v", name, desc, err)
	}
	_, desc, _ = parseFrontmatter("---\nname: abc\ndescription: 'quoted'\n---\n")
	if desc != "quoted" {
		t.Errorf("single-quoted: %q", desc)
	}
	if _, _, err := parseFrontmatter("---\ndescription: x\n---\n"); err == nil {
		t.Error("缺 name 应报错")
	}
}
```

- [ ] **Step 2: 验证 FAIL**：`go test ./internal/agent/ -run 'TestInstallSkill|TestSearchSkills|TestParseFrontmatter' -v` → 编译失败 `undefined: NewSkillInstaller` 等。

- [ ] **Step 3: 实现** — `internal/agent/skillinstall.go`:
```go
package agent

// 技能安装器：从 GitHub（codeload tarball）拉取 skills/<name>/ 整目录落盘到
// <SkillRoot>/enabled/<name>/（dsh skills 插件 chokidar 监听该根，落盘即热生效）；
// Search 代理 skills.sh 检索 API（后端代理避免前端跨域）。规格见 skill-management spec §6。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"zhiwei/internal/repo"
)

// skillNameRe dsh 强制的技能名格式（kebab-case，dsh-skill/lib/index.js isSkillName）。
var skillNameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// maxTarBytes 单次 tarball 解包上限（防恶意大仓库）。
const maxTarBytes = 64 << 20 // 64MB

// SkillSearchHit 是 skills.sh /api/search 的单条结果（原样透传给前端）。
type SkillSearchHit struct {
	ID       string `json:"id"`       // 'owner/repo/skill'
	Name     string `json:"name"`
	Installs int64  `json:"installs"`
	Source   string `json:"source"`  // 'owner/repo'
}

// SkillInstaller 拉取/落盘技能。codeloadBase 与 searchBase 生产是官方地址，测试注入 httptest。
type SkillInstaller struct {
	codeloadBase string // 如 https://codeload.github.com
	searchBase   string // 如 https://www.skills.sh
	skillRoot    string // <AgentSkillRoot>（enabled/ 与 disabled/ 的父目录）
	httpClient   *http.Client
}

// NewSkillInstaller 构造。skillRoot 为 AgentSkillRoot（非 enabled 子目录）。
func NewSkillInstaller(codeloadBase, searchBase, skillRoot string) *SkillInstaller {
	return &SkillInstaller{
		codeloadBase: strings.TrimRight(codeloadBase, "/"),
		searchBase:   strings.TrimRight(searchBase, "/"),
		skillRoot:    skillRoot,
		httpClient:   &http.Client{Timeout: 60 * time.Second},
	}
}

// EnabledDir / DisabledDir 返回启/禁子目录路径。
func (si *SkillInstaller) EnabledDir() string  { return filepath.Join(si.skillRoot, "enabled") }
func (si *SkillInstaller) DisabledDir() string { return filepath.Join(si.skillRoot, "disabled") }

// Install 从 source（'owner/repo/skill'）拉取并安装。原子性：先解到 .tmp-<rand>/ 校验，
// 通过后 rename 进 enabled/<name>/；任何失败清理暂存。返回落库用的元数据（调用方负责 Create DB 行）。
func (si *SkillInstaller) Install(ctx context.Context, source string) (*repo.AgentSkill, error) {
	owner, repoName, skillName, err := parseSource(source)
	if err != nil {
		return nil, err
	}
	if !skillNameRe.MatchString(skillName) {
		return nil, fmt.Errorf("技能名 %q 须为 kebab-case（小写字母/数字/连字符）", skillName)
	}
	// 已装（enabled 或 disabled 任一）→ 拒绝
	for _, dir := range []string{si.EnabledDir(), si.DisabledDir()} {
		if _, err := os.Stat(filepath.Join(dir, skillName)); err == nil {
			return nil, fmt.Errorf("技能 %s 已安装（请先删除再重装）", skillName)
		}
	}

	body, err := si.fetchTarball(ctx, owner, repoName)
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp(si.skillRoot, ".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp) // rename 成功后 tmp 已空，RemoveAll 无害；失败路径兜底清理

	skillMeta, err := si.extractSkill(body, skillName, tmp)
	if err != nil {
		return nil, err
	}
	skillMeta.Source = source

	// 原子落位：rename 暂存目录 → enabled/<name>
	dst := filepath.Join(si.EnabledDir(), skillName)
	if err := os.MkdirAll(si.EnabledDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(tmp, skillName), dst); err != nil {
		return nil, fmt.Errorf("落位技能目录: %w", err)
	}
	return skillMeta, nil
}

// parseSource 解析 'owner/repo/skill' 三段。
func parseSource(s string) (owner, repoName, skill string, error error) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("source 须为 owner/repo/skill 形式，got %q", s)
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, " .:@\\") {
			return "", "", "", fmt.Errorf("source 段非法: %q", s)
		}
	}
	return parts[0], parts[1], parts[2], nil
}

// fetchTarball 拉 repo tarball：默认分支 main，404 回退 master。
func (si *SkillInstaller) fetchTarball(ctx context.Context, owner, repoName string) ([]byte, error) {
	for _, branch := range []string{"main", "master"} {
		u := fmt.Sprintf("%s/%s/%s/tar.gz/refs/heads/%s", si.codeloadBase, owner, repoName, branch)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := si.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("拉取 %s/%s: %w", owner, repoName, err)
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("拉取 %s/%s: HTTP %d", owner, repoName, resp.StatusCode)
		}
		// 上限读：超限报错（防恶意大仓库撑爆内存/磁盘）
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxTarBytes+1))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > maxTarBytes {
			return nil, fmt.Errorf("tarball 超过 %dMB 上限", maxTarBytes>>20)
		}
		return body, nil
	}
	return nil, fmt.Errorf("仓库 %s/%s 的 main/master 分支均不可达", owner, repoName)
}

// extractSkill 流式解 tar，取 */skills/<skillName>/ 子树写入 tmp/<skillName>/ 并校验 SKILL.md。
// 防穿越：条目名 Clean 后必须仍以 repo 前缀 + skills/<name>/ 开头；symlink 条目直接跳过。
func (si *SkillInstaller) extractSkill(tarball []byte, skillName, tmp string) (*repo.AgentSkill, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, fmt.Errorf("解 gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	dstBase := filepath.Join(tmp, skillName)
	// tar 内目标前缀（形如 <anything>/skills/<name>/）——首条命中后固定 repo 根，防跨根混入
	var prefix string
	var totalWritten int64
	var skillMD []byte

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读 tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			continue // 跳过链接条目（技能内容不需要）
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			continue
		}
		name := filepath.Clean(hdr.Name)
		// 找仓库根（首段是 <repo>-<ref>/）下的 skills/<name>/ 前缀
		i := strings.Index(name, "skills/"+skillName+"/")
		if i < 0 {
			continue // 仓库其它内容
		}
		root := name[:i] // 如 "repo-main/"
		if prefix == "" {
			prefix = root
		} else if root != prefix {
			continue // 不同根（理论不可能；防御）
		}
		rel := name[i+len("skills/")+len(skillName)+1:] // 目录内相对路径
		if rel != "" && strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("tar 条目路径越界: %s", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(filepath.Join(dstBase, rel), 0o755); err != nil {
				return nil, err
			}
			continue
		}
		totalWritten += hdr.Size
		if totalWritten > maxTarBytes {
			return nil, fmt.Errorf("技能解包超 %dMB 上限", maxTarBytes>>20)
		}
		out := filepath.Join(dstBase, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxTarBytes+1))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return nil, err
		}
		if rel == "SKILL.md" {
			skillMD = data
		}
	}
	if skillMD == nil {
		return nil, fmt.Errorf("仓库里没有 skills/%s/SKILL.md", skillName)
	}
	fmName, fmDesc, err := parseFrontmatter(string(skillMD))
	if err != nil {
		return nil, fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	if fmName != skillName {
		return nil, fmt.Errorf("frontmatter name %q 与目录名 %q 不一致", fmName, skillName)
	}
	return &repo.AgentSkill{
		Name: fmName, DisplayName: fmName, Description: fmDesc,
		Content: string(skillMD), Enabled: true,
	}, nil
}

// parseFrontmatter 简易解析 SKILL.md 头部 YAML（只需 name/description 两行，兼容引号包裹）。
// 不引入 yaml 依赖：dsh 只强制这两个字段，其余键忽略。
func parseFrontmatter(md string) (name, description string, err error) {
	s := strings.TrimSpace(md)
	if !strings.HasPrefix(s, "---") {
		return "", "", fmt.Errorf("缺 frontmatter 头 ---")
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("frontmatter 未闭合")
	}
	block := rest[:end]
	for _, line := range strings.Split(block, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`) // 兼容单/双引号包裹值
		switch strings.TrimSpace(k) {
		case "name":
			name = v
		case "description":
			description = v
		}
	}
	if name == "" || description == "" {
		return "", "", fmt.Errorf("frontmatter 缺 name 或 description")
	}
	return name, description, nil
}

// Search 代理 skills.sh /api/search?q=<kw>（10s 超时）。
func (si *SkillInstaller) Search(ctx context.Context, q string) ([]SkillSearchHit, error) {
	u := si.searchBase + "/api/search?q=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("搜索 skills.sh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索 skills.sh: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Skills []SkillSearchHit `json:"skills"`
	}
	if err := jsonDecodeBody(resp.Body, &out); err != nil {
		return nil, err
	}
	return out.Skills, nil
}
```
（`jsonDecodeBody` 用标准 `json.NewDecoder(resp.Body).Decode(&v)` 内联替代——上面写个 helper 或直接内联。内联即可，删掉 helper。）

- [ ] **Step 4: 验证 PASS**：`go test ./internal/agent/ -run 'TestInstallSkill|TestSearchSkills|TestParseFrontmatter' -v` 全 PASS（注意 `parseSource` 返回签名里 `error` 是变量名遮蔽类型的笔误——实现时改为标准 `(string, string, string, error)`，参数名用 `err` 返回处命名返回或显式 return，参考上面 repo 代码风格自查修正）。

- [ ] **Step 5: Commit** `feat(skill): 技能安装器（codeload tarball + 校验 + 原子落盘 + skills.sh 搜索代理）`

---

## Task 4: API `/api/agent/skills` + 启禁/删除组合

**Files:**
- Create: `internal/agent/skill_handlers.go`
- Modify: `internal/agent/handlers.go`（AgentHandler 加 `Skills *repo.AgentSkillRepo` + `SkillInst *SkillInstaller`；RegisterAgent 挂路由）
- Test: `internal/agent/skill_handlers_test.go`

- [ ] **Step 1: 失败测试** — `internal/agent/skill_handlers_test.go`:
```go
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/repo"
)

func skillHandler(t *testing.T) (*AgentHandler, string) {
	t.Helper()
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM agent_skill WHERE name LIKE 'test-%'") })
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "enabled"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "disabled"), 0o755)
	inst := NewSkillInstaller("http://codeload.invalid", "http://search.invalid", root)
	return &AgentHandler{Skills: &repo.AgentSkillRepo{DB: db}, SkillInst: inst}, root
}

func doSkill(h *AgentHandler, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Use(injectUser(1))
	RegisterAgent(r, h)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestSkillHandlersLifecycle(t *testing.T) {
	h, root := skillHandler(t)

	// 直接造一条已安装技能（磁盘 + DB），走启禁/删除路径（安装路径由 Task 3 测过）
	s := &repo.AgentSkill{Name: "test-demo", DisplayName: "test-demo", Description: "d",
		Content: "---\nname: test-demo\ndescription: d\n---\nx", Enabled: true, Source: "a/b/test-demo"}
	if err := h.Skills.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(root, "enabled", "test-demo")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(s.Content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 列表
	rec := doSkill(h, "GET", "/api/agent/skills", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "test-demo") {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 详情含 content
	rec = doSkill(h, "GET", "/api/agent/skills/"+s.ID.String(), "")
	if !strings.Contains(rec.Body.String(), "name: test-demo") {
		t.Errorf("详情应含 content: %s", rec.Body.String())
	}

	// 禁用：目录应移到 disabled/
	rec = doSkill(h, "PATCH", "/api/agent/skills/"+s.ID.String(), `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "disabled", "test-demo", "SKILL.md")); err != nil {
		t.Errorf("禁用后应在 disabled/: %v", err)
	}
	if _, err := os.Stat(skillDir); err == nil {
		t.Error("禁用后 enabled/ 下不应存在")
	}

	// 重新启用：移回 enabled/
	rec = doSkill(h, "PATCH", "/api/agent/skills/"+s.ID.String(), `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH enable code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "enabled", "test-demo", "SKILL.md")); err != nil {
		t.Errorf("启用后应在 enabled/: %v", err)
	}

	// 删除：磁盘 + DB 全清
	rec = doSkill(h, "DELETE", "/api/agent/skills/"+s.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "enabled", "test-demo")); err == nil {
		t.Error("删除后磁盘应清")
	}
	list, _ := h.Skills.List(context.Background())
	for _, x := range list {
		if x.Name == "test-demo" {
			t.Error("删除后 DB 应清")
		}
	}

	// 安装端点：source 非法 → 400（真实安装已在 Task 3 测，这里测参数校验）
	rec = doSkill(h, "POST", "/api/agent/skills/install", `{"source":"bad-format"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 source 应 400, got %d", rec.Code)
	}

	// 搜索端点不可达 → 502（SkillInst 指向 .invalid）
	rec = doSkill(h, "GET", "/api/agent/skills/search?q=x", "")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("搜索失败应 502, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: 验证 FAIL**：`go test ./internal/agent/ -run TestSkillHandlers -v` → 编译失败 `h.Skills undefined`。

- [ ] **Step 3: 实现**

`internal/agent/handlers.go` — AgentHandler 加字段（OnMCPChange 之后）：
```go
	Skills   *repo.AgentSkillRepo // 已装技能元数据；nil 时技能端点 503
	SkillInst *SkillInstaller     // 安装器（tarball/搜索代理 + 磁盘根）；nil 时安装/搜索/启禁/删 503
```
RegisterAgent 追加路由：
```go
	r.Get("/api/agent/skills", h.listSkills)
	r.Get("/api/agent/skills/search", h.searchSkills) // 注意：必须在 /{id} 之前注册（chi 不冲突但语义清晰）
	r.Post("/api/agent/skills/install", h.installSkill)
	r.Get("/api/agent/skills/{id}", h.getSkill)
	r.Patch("/api/agent/skills/{id}", h.patchSkill)
	r.Delete("/api/agent/skills/{id}", h.deleteSkill)
```

`internal/agent/skill_handlers.go`:
```go
package agent

// 技能管理端点（/api/agent/skills*）：列表/详情只读 DB；安装走 SkillInstaller（tarball 落盘）；
// 启禁 = enabled↔disabled 目录 rename（dsh watcher 热生效）；删除 = 删目录 + 删行。
// 顺序约定「先磁盘后 DB」：rename 成功但 DB 更新失败时回滚 rename（管理操作低频，简单补偿）。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func (h *AgentHandler) skillAvailable(w http.ResponseWriter) bool {
	if h.Skills == nil || h.SkillInst == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "技能管理不可用"})
		return false
	}
	return true
}

func (h *AgentHandler) listSkills(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	rows, err := h.Skills.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": rows})
}

func (h *AgentHandler) getSkill(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	s, err := h.Skills.Get(r.Context(), id)
	if err != nil {
		writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *AgentHandler) searchSkills(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q required"})
		return
	}
	hits, err := h.SkillInst.Search(r.Context(), q)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": hits})
}

func (h *AgentHandler) installSkill(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	var body struct {
		Source string `json:"source"` // 'owner/repo/skill'
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if _, _, _, err := parseSource(body.Source); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s, err := h.SkillInst.Install(r.Context(), body.Source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := h.Skills.Create(r.Context(), s); err != nil {
		// DB 失败回滚磁盘（先磁盘后 DB 的补偿）
		_ = os.RemoveAll(filepath.Join(h.SkillInst.EnabledDir(), s.Name))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *AgentHandler) patchSkill(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	s, err := h.Skills.Get(r.Context(), id)
	if err != nil {
		writeSkillErr(w, err)
		return
	}
	if s.Enabled != body.Enabled {
		if err := h.SkillInst.renameSkill(s.Name, body.Enabled); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := h.Skills.SetEnabled(r.Context(), id, body.Enabled); err != nil {
			_ = h.SkillInst.renameSkill(s.Name, !body.Enabled) // DB 失败回滚磁盘
			writeSkillErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *AgentHandler) deleteSkill(w http.ResponseWriter, r *http.Request) {
	if !h.skillAvailable(w) {
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	s, err := h.Skills.Get(r.Context(), id)
	if err != nil {
		writeSkillErr(w, err)
		return
	}
	if err := h.SkillInst.removeSkill(s.Name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := h.Skills.Delete(r.Context(), id); err != nil {
		writeSkillErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeSkillErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}
```

`internal/agent/skillinstall.go` 追加两个方法：
```go
// renameSkill 启禁的磁盘动作：enabled↔disabled 目录 rename（chokidar 热生效）。
// 目录缺失（磁盘态丢失）返回明确错误。
func (si *SkillInstaller) renameSkill(name string, enable bool) error {
	from := filepath.Join(si.DisabledDir(), name)
	to := filepath.Join(si.EnabledDir(), name)
	if !enable {
		from, to = to, from
	}
	if _, err := os.Stat(from); err != nil {
		return fmt.Errorf("技能目录缺失（%s），请删除后重装: %w", from, err)
	}
	if _, err := os.Stat(to); err == nil {
		return fmt.Errorf("目标目录已存在: %s", to)
	}
	return os.Rename(from, to)
}

// removeSkill 删除磁盘上的技能目录（enabled 与 disabled 下都试）。
func (si *SkillInstaller) removeSkill(name string) error {
	for _, dir := range []string{si.EnabledDir(), si.DisabledDir()} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return os.RemoveAll(p)
		}
	}
	return nil // 磁盘已无（幂等）
}
```
（skill_handlers.go 需补 `strings` import。）

- [ ] **Step 4: 验证 PASS**：`go test ./internal/agent/ -run TestSkillHandlers -v` → PASS；全包回归 `TEST_MYSQL_DSN=... go test ./internal/agent/...`。

- [ ] **Step 5: Commit** `feat(skill): /api/agent/skills 端点（列表/详情/搜索代理/安装/启禁/删除）`

---

## Task 5: main.go 装配 + 前端「技能」子区

**Files:**
- Modify: `cmd/zhiwei-server/main.go`（SkillInstaller 构造 + handler 字段）
- Modify: `web/index.html`、`web/app.js`

- [ ] **Step 1: main.go 装配**

Task 2 的目录创建之后加：
```go
	skillInst := agent.NewSkillInstaller("https://codeload.github.com", "https://www.skills.sh", cfg.AgentSkillRoot)
```
AgentHandler 字面量加（MCPServers/OnMCPChange 旁）：
```go
			Skills:    agentSkills, // &repo.AgentSkillRepo{DB: db}（agentConfigs 旁声明）
			SkillInst: skillInst,
```

- [ ] **Step 2: app.js**（MCP 区后仿写）：
```js
    // ---------- 设置：技能 Skills（全局；从 skills.sh 搜索/手动 GitHub 路径安装，落盘热生效） ----------
    // 后端契约：GET /api/agent/skills → {skills:[AgentSkill]}；GET /skills/search?q= → {skills:[{id,name,installs,source}]}；
    // POST /skills/install {source:'owner/repo/skill'}；PATCH /skills/{id} {enabled}；DELETE /skills/{id}。
    const agentSkills = ref([]);
    const skillSearchQ = ref('');
    const skillResults = ref([]);
    const skillSearching = ref(false);
    const skillManual = ref('');
    const skillErr = ref('');
    const skillView = ref(null); // 展开查看的技能（含 content）
    async function loadSkills() {
      try { const d = await api('GET', '/api/agent/skills'); agentSkills.value = (d && d.skills) || []; }
      catch (e) { showError(e); }
    }
    async function searchSkills() {
      const q = (skillSearchQ.value || '').trim();
      if (!q) return;
      skillSearching.value = true; skillErr.value = '';
      try { const d = await api('GET', '/api/agent/skills/search?q=' + encodeURIComponent(q)); skillResults.value = (d && d.skills) || []; }
      catch (e) { skillErr.value = (e && e.message) || String(e); }
      finally { skillSearching.value = false; }
    }
    async function installSkill(source) {
      skillErr.value = '';
      try { await api('POST', '/api/agent/skills/install', { source }); await loadSkills(); }
      catch (e) { skillErr.value = (e && e.message) || String(e); }
    }
    async function toggleSkill(s) {
      try { await api('PATCH', '/api/agent/skills/' + s.id, { enabled: !s.enabled }); await loadSkills(); }
      catch (e) { showError(e); }
    }
    async function deleteSkill(s) {
      if (!confirm('删除技能「' + s.name + '」？')) return;
      try { await api('DELETE', '/api/agent/skills/' + s.id); skillView.value = null; await loadSkills(); }
      catch (e) { showError(e); }
    }
```
`switchTab('settings')` 分支追加 `loadSkills();`；返回对象追加 `agentSkills, skillSearchQ, skillResults, skillSearching, skillManual, skillErr, skillView, loadSkills, searchSkills, installSkill, toggleSkill, deleteSkill,`。

- [ ] **Step 3: index.html**（MCP 卡片后加卡片，复用样式类）：
```html
    <!-- ============ 技能 Skills（搜索安装/启禁/删除，落盘热生效） ============ -->
    <div class="card">
      <h2 style="margin:0 0 4px">技能（Skills）</h2>
      <div class="muted" style="font-size:var(--fs-sm); margin-bottom:16px">
        从 skills.sh 搜索安装技能（SKILL.md 指令集），安装/启禁/删除<b>下一轮对话即生效</b>（热加载，无需重启）。
        技能内容会作为指令注入模型，<b>仅安装可信来源</b>。
      </div>

      <!-- 已装列表 -->
      <div style="display:flex; flex-direction:column; gap:8px">
        <div v-for="s in agentSkills" :key="s.id"
             style="display:flex; align-items:center; gap:12px; padding:10px 12px; border:1px solid var(--border,#ddd); border-radius:8px">
          <label style="display:flex; align-items:center; gap:6px; cursor:pointer">
            <input type="checkbox" :checked="s.enabled" @change="toggleSkill(s)">
            <code style="font-weight:600">{{ s.name }}</code>
          </label>
          <span class="muted" style="font-size:var(--fs-xs); flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap">{{ s.description }}</span>
          <span class="muted" style="font-size:var(--fs-xs)">{{ s.source || 'manual' }}</span>
          <button class="btn" style="padding:4px 10px; font-size:var(--fs-xs)" @click="skillView = (skillView && skillView.id === s.id ? null : s)">查看</button>
          <button class="btn" style="padding:4px 10px; font-size:var(--fs-xs)" @click="deleteSkill(s)">删除</button>
        </div>
        <div v-if="skillView" style="padding:12px; border:1px solid var(--border,#eee); border-radius:8px; background:var(--bg-soft,#fafafa)">
          <div class="muted" style="font-size:var(--fs-xs); margin-bottom:6px">SKILL.md（{{ skillView.name }}）</div>
          <pre class="chat-code" style="max-height:320px; overflow:auto">{{ skillView.content }}</pre>
        </div>
      </div>

      <!-- 搜索安装 -->
      <div style="margin-top:18px; padding-top:14px; border-top:1px solid var(--border,#eee)">
        <div style="display:flex; gap:8px; align-items:center; flex-wrap:wrap">
          <input class="txt" v-model="skillSearchQ" placeholder="搜索技能（如 git、pdf、code-review）" style="flex:1; min-width:220px" @keyup.enter="searchSkills">
          <button class="btn primary" :disabled="skillSearching" @click="searchSkills">{{ skillSearching ? '搜索中…' : '搜索 skills.sh' }}</button>
        </div>
        <div v-if="skillResults.length" style="margin-top:10px; display:flex; flex-direction:column; gap:6px">
          <div v-for="hit in skillResults" :key="hit.id" style="display:flex; align-items:center; gap:10px; font-size:var(--fs-sm)">
            <code>{{ hit.name }}</code>
            <span class="muted" style="font-size:var(--fs-xs); flex:1">{{ hit.id }}</span>
            <span class="muted" style="font-size:var(--fs-xs)">{{ hit.installs }} 次安装</span>
            <button class="btn" style="padding:4px 10px; font-size:var(--fs-xs)" @click="installSkill(hit.id)">安装</button>
          </div>
        </div>
        <div style="margin-top:12px; display:flex; gap:8px; align-items:center">
          <input class="txt" v-model="skillManual" placeholder="手动安装：owner/repo/skill" style="flex:1; min-width:220px">
          <button class="btn" @click="installSkill(skillManual.trim())" :disabled="!skillManual.trim()">安装</button>
        </div>
        <div v-if="skillErr" class="muted" style="font-size:var(--fs-xs); color:var(--danger,#c33); margin-top:6px">{{ skillErr }}</div>
      </div>
    </div>
```

- [ ] **Step 4: 验证**：`node --check web/app.js`；绑定名与返回对象一致（grep 模板 vs return）；`go build ./...`。

- [ ] **Step 5: Commit** `feat(skill): 设置页「技能」子区（skills.sh 搜索安装/查看/启禁/删除）`

---

## Task 6: 端到端验证 + 收尾

- [ ] **Step 1: 起 dev（隔离库 + 8099）**，安装真实技能：
  设置页（或 curl）：`POST /api/agent/skills/install {"source":"github/awesome-copilot/git-commit"}` → 断言 200、`data/agent-skills/enabled/git-commit/SKILL.md` 存在、DB 有行。
- [ ] **Step 2: agent 热生效验证**（真实 Ark 轮次）：
  问知微发「你有哪些可用的技能？」→ 断言回复列出 git-commit；再发「/git-commit 简单说明你会怎么帮我提交」→ 断言模型加载了技能内容（回复含规范提交相关要点）。
  然后 PATCH 禁用 → 再问「你有哪些技能」→ 断言 git-commit 消失。
- [ ] **Step 3: 回归**：`go build ./... && go vet ./... && make init-testdb && TEST_MYSQL_DSN=... go test ./...`（15 包全 ok）。
- [ ] **Step 4: 清理冒烟产物**（隔离库 DROP、临时二进制、生成的技能目录若在 data/ 下则属运行时数据不提交）。
- [ ] **Step 5: 收尾**：finishing-a-development-branch（合并前确认 main 迁移号未撞 000023；若撞则重排）。

---

## Self-Review 结果（对照 spec）

- **规格覆盖**：§4 边车配置→T2；§5 数据模型→T1；§6 安装器+启禁删除→T3/T4；§7 API→T4；§8 前端→T5；§9 安全（穿越防护/白名单域名/大小上限/一致性校验）→T3；§10 测试→各 task + T6。§11 开放问题按默认值落（64MB、500 字符截断不动）。
- **占位符扫描**：无 TBD；唯一标注的实现笔误（parseSource 返回签名）已在 T3 Step 4 显式指出修正方式。
- **类型一致**：`AgentSkill`/`SkillInstaller`/`SkillSearchHit`/`parseSource`/`parseFrontmatter`/`renameSkill`/`removeSkill`/`EnabledDir`/`DisabledDir` 跨任务命名一致；handler 字段 `Skills`/`SkillInst` 与 T5 装配一致。
- **已知简化**：搜索端点 `/skills/search` 与 `/{id}` 路由次序（chi 静态优先，实际无冲突，仍按语义排序注册）。
