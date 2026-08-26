# 用户画像 P8（F6 完整并行：测试库按包隔离）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 解锁 `go test ./...` 不带 `-p 1` 并行全绿——把共享 `zhiwei_test` 换成**按测试二进制（Go 包）隔离的数据库** `zhiwei_test_<pkg>`，两个根因（pipeline Pool 全局抢 pending job + user_id=1 跨包去重污染）一并消解。

**根因回顾（P6 实证）**：
1. `ClaimNext` 是全局 `SELECT ... WHERE status='pending' ORDER BY id LIMIT 1`——pipeline 包测试起的真实 Pool 会抢走 repo 包测试造的 job（各自都在同一个 zhiwei_test 库）
2. extract 按 user_id=1 跨 session 归一化佐证——并行包的同名 memory 被合并，计数断言失败

**方案（单点改造，零调用方改动）**：全部 34 个测试文件的 DB 连接都经 `repo.TestDSN(t)` 单点取得——在该函数内把 DSN 的库名换成 `zhiwei_test_<调用方包名>`（`runtime.Caller` 取调用方包目录），首次遇到某库名时自动 `CREATE DATABASE IF NOT EXISTS` + 进程内跑迁移（golang-migrate iofs，成熟开源方案）。不同包的测试二进制从此各用各的库，并行天然安全。

**不做**：生产代码零改动（ClaimNext 不加 user_id 过滤——那是多用户就绪的大改，测试隔离不需要动它）；不做按测试函数级隔离（包内本就串行，包级隔离即可）。

**工作目录：** worktree `.worktrees/person-p8`（分支 `feat/person-p8`，基线 main=c860510）。

---

### Task 1: migrations embed + TestDSN 按包建库 + Makefile

**Files:** Create `migrations/embed.go`、改造 `internal/repo/testutil.go`（或新建 `internal/repo/testdb.go`）、`Makefile`；go.mod 加依赖

**做法**：

1. **依赖**：`go get github.com/golang-migrate/migrate/v4`（与 Makefile 用的 migrate CLI 同源同格式——schema_migrations 表完全兼容）
2. **migrations/embed.go**：
```go
// Package migrations 把 SQL 迁移文件嵌入二进制，供测试库懒建时进程内执行
// （生产仍走 make migrate-up 的 CLI 路径，两路同源同一份文件）。
package migrations

import "embed"

//go:embed *.up.sql *.down.sql
var FS embed.FS
```
3. **internal/repo/testdb.go**（TestDSN 迁入或改造 testutil.go）：
```go
// TestDSN 返回集成测试 DSN；未设置 TEST_MYSQL_DSN 时跳过调用方测试。
//
// F6 完整并行（按包隔离库）：把 DSN 的库名换成 zhiwei_test_<调用方包名>，
// 首次遇到该库时自动 CREATE DATABASE + 跑全部迁移（幂等——golang-migrate 自记
// schema_migrations，二次调用秒过）。这样每个测试二进制（Go 包）各用各的库，
// go test 并行跑包时互不可见——P6 实证的两个根因（ClaimNext 全局抢 job、
// user_id=1 跨包去重污染）都被库级隔离消解。
//
// 包名取法：runtime.Caller(1) 拿调用方（测试文件）的目录名——所有调用点都是
// 测试函数内直接调 repo.TestDSN(t)， Caller(1) 恰为该测试文件。这是有意的
// 零调用方改动设计（34 个测试文件不动）；魔法集中在这一处并配本注释。
//
// 建库权限：init-testdb 给 zhiwei 用户授予 zhiwei_test_%.* 通配符权限
// （MySQL 通配符 GRANT 对「尚不存在」的库生效）。
func TestDSN(t *testing.T) string
```
   实现：mysql.ParseDSN（go-sql-driver 已是依赖）→ cfg.DBName 换 `zhiwei_test_<pkg>` → 先用无库名连接 CREATE DATABASE IF NOT EXISTS `xxx` CHARACTER SET utf8mb4 → golang-migrate `migrate.NewWithSourceInstance(iofs.New(migrations.FS, "."), "mysql://user:pass@tcp(host)/db")` Up()（ErrNoChange 吞掉）→ 返回 cfg.FormatDSN()。**包级 sync.Map 缓存**（dbname → 已就绪），同库二次调用直接返回
4. **Makefile**：
   - init-testdb：加通配符授权 `GRANT ALL PRIVILEGES ON \`zhiwei_test_%\`.* TO 'zhiwei'@'%'`；加清理循环（root 删所有 `zhiwei_test_%` 库——`SHOW DATABASES LIKE 'zhiwei_test_%'` 逐个 DROP）；原 zhiwei_test 建库+迁移保留（兜底：未升级的旧流程仍可用）
   - test-integration：**去掉 `-p 1`**，注释更新（并行安全由按包隔离保证）
5. **验证**：`make init-testdb` 后单包 `TEST_MYSQL_DSN=... go test ./internal/repo/ -count=1`（应自动建出 zhiwei_test_repo 并全绿）；`docker exec zhiwei-mvp-mysql mysql -uroot -proot -e "SHOW DATABASES LIKE 'zhiwei_test_%'"` 确认建库
6. commit：`feat(test): 测试库按包隔离——TestDSN 懒建 zhiwei_test_<pkg> + 进程内迁移（F6）`

### Task 2: 并行验收 + 收尾

1. **验收（P6 立的标准）**：`TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./... -count=1` **不带 -p 1** 连跑 3 次全绿（先 make init-testdb 清库；三次中间不 re-init——脏库连跑也要绿，因为包内隔离 + 各包 t.Cleanup 纪律仍在）
   - 若有测试隐含依赖共享库既有数据（非 schema）：逐一修复（应无——init-testdb 只跑迁移无种子）
2. `go build ./... && go vet ./...`
3. spec §13 F6 条目更新：「✅ 完整并行已于 P8 解决（2026-08-26）——测试库按包隔离（zhiwei_test_<pkg> 懒建 + 进程内迁移）；`-p 1` 已从 Makefile 移除；nodeID 隔离（P6）+ 库隔离（P8）双保险」
4. memory 文件测试坑更新：`-p 1` 要求删除，改为「按包隔离库自动建，直接 go test ./...」
5. commit：`docs: P8 收尾——F6 完整并行已解决标注`

---

## 计划自检

1. **覆盖**：两根因经库级隔离一并消解；生产零改动；34 个调用方零改动（单点魔法有注释）。
2. **依赖**：golang-migrate 是 Makefile migrate CLI 的同源库（成熟开源），schema_migrations 兼容。
3. **风险点已核**：zhiwei 用户建库权限靠通配符 GRANT（MySQL 对未来库生效）；embed 不能引用父目录 → embed.go 放 migrations/ 目录内；runtime.Caller(1) 的调用深度以实际单测验证（TestDSN 内 Caller(1) 是调用方测试文件——写个小单测断言包名正确）；脏库连跑依赖包内 t.Cleanup 纪律（既有）。
