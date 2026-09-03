// Package repotest 提供集成测试的隔离测试库支持（建库 + 内嵌迁移 + 按包隔离 DSN）。
//
// 为什么单独成包：本包 import 了 testing、golang-migrate、以及内嵌全部 SQL 的
// zhiwei/migrations。若把这些放在生产包 internal/repo 里的非 _test 文件（旧
// testdb.go），服务器二进制 cmd/zhiwei-server 会被动链入 golang-migrate + 22 个
// SQL——纯测试设施污染生产依赖。repotest 是「正常包但只被各包的 _test.go 引用」，
// 生产二进制不会链入它（与标准库 net/http/httptest 同模式）。
//
// 分层约束：repotest 不 import repo（否则与 repo 包内测试文件 `package repo` 形成
// 导入环）。DSN 只返回字符串，测试侧照旧 repo.NewDB(repotest.DSN(t))。
package repotest

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migmysql "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"zhiwei/migrations"
)

// DSN 返回集成测试 DSN；未设置 TEST_MYSQL_DSN 时跳过调用方测试。
// 用法：make test-integration（自动起 docker MySQL + init-testdb 授权 + 设置 DSN）。
//
// F6 完整并行（按包隔离库）：把 DSN 的库名换成 zhiwei_test_<调用方包名>，
// 首次遇到该库时自动 CREATE DATABASE + 跑全部迁移（幂等——golang-migrate 自记
// schema_migrations，二次调用秒过并被包级缓存直接跳过）。这样每个测试二进制
// （Go 包）各用各的库，go test 并行跑包时互不可见——P6 实证的两个根因都被库级
// 隔离消解：
//  1. pipeline.Pool 的 ClaimNext 是全局 SELECT ... WHERE status='pending' LIMIT 1，
//     并行包共享同一库时会抢走别包造的 job；
//  2. extract 按 user_id=1 跨 session 归一化佐证，并行包的同名 memory 被合并致计数错。
//
// 包名取法：runtime.Caller 拿调用方（测试文件）所在目录名——所有调用点都是测试
// 函数内直接调 repotest.DSN(t)，故调用方即该测试文件，其目录名恰为包名（如
// internal/repo → repo → zhiwei_test_repo；internal/api → api → zhiwei_test_api）。
// 把这处 runtime.Caller 魔法与建库/迁移集中在本包并配注释，调用深度由
// TestCallerPkgDBName 单测锁定（见 testdb_test.go）。
//
// 建库权限：init-testdb 给 zhiwei 用户授予 `zhiwei_test_%`.* 通配符权限——MySQL
// 通配符 GRANT 对「尚不存在」的库也生效，故此处懒建 zhiwei_test_<pkg> 时 zhiwei
// 用户已有 CREATE/全权限，无需 root。
// 形参用 testing.TB（而非 *testing.T）：benchmark（*testing.B）也要用隔离测试库，
// 二者都满足 TB；本函数只用到 Helper/Skip/Fatalf，均在 TB 接口上，widening 无副作用。
func DSN(t testing.TB) string {
	t.Helper()
	raw := os.Getenv("TEST_MYSQL_DSN")
	if raw == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}

	// 解析基准 DSN（go-sql-driver 本就是依赖），把库名替换成按包隔离的库名。
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		t.Fatalf("解析 TEST_MYSQL_DSN 失败: %v", err)
	}
	// Caller(2)：本函数 DSN(skip=1) 的调用方（测试文件）在 callerPkgDBName 视角下
	// 为 skip=2（callerPkgDBName 自身 skip=0、DSN skip=1、测试文件 skip=2）。
	cfg.DBName = callerPkgDBName(2)

	// 首次遇到该库：CREATE DATABASE + 进程内迁移，仅执行一次（sync.Once/库名维度）。
	ensureTestDB(t, cfg)

	return cfg.FormatDSN()
}

// callerPkgDBName 按 skip 层调用栈的源文件目录名拼出隔离库名 zhiwei_test_<pkg>。
// 抽成独立函数是为了让 TestCallerPkgDBName 能在不连库的前提下断言「Caller 深度 +
// 目录名解析」正确（计划点名的验证项）。runtime.Caller 自 Go1.12 起对内联透明，
// 逻辑帧计数稳定，不受编译器内联影响。
func callerPkgDBName(skip int) string {
	_, file, _, ok := runtime.Caller(skip)
	if !ok {
		return "zhiwei_test_unknown"
	}
	return "zhiwei_test_" + filepath.Base(filepath.Dir(file))
}

// dbReady 缓存「某隔离库名 → 准备该库的 sync.Once」，保证同一库的建库+迁移在
// 进程内只跑一次；即便并行子测试同时首调 DSN 也只有一个 goroutine 真正建库。
var dbReady sync.Map // map[string]*sync.Once

// ensureTestDB 对 cfg.DBName 指向的隔离库执行「建库(幂等) + 迁移(幂等)」，仅一次。
// 失败经 t.Fatalf 终止调用方测试（不返回 error——调用点都是测试内，直接失败最清晰）。
func ensureTestDB(t testing.TB, cfg *mysql.Config) {
	t.Helper()
	onceAny, _ := dbReady.LoadOrStore(cfg.DBName, &sync.Once{})
	var setupErr error
	onceAny.(*sync.Once).Do(func() {
		setupErr = setupTestDB(cfg)
	})
	if setupErr != nil {
		// 建库失败属环境级错误：清掉缓存的 Once，让后续调用可重试（如授权补齐后）。
		dbReady.Delete(cfg.DBName)
		t.Fatalf("准备隔离测试库 %s 失败: %v", cfg.DBName, setupErr)
	}
}

// setupTestDB 真正干活：无库名连接 CREATE DATABASE IF NOT EXISTS，再用 golang-migrate
// 把内嵌迁移全量 Up 到该库。golang-migrate 与 Makefile 的 migrate CLI 同源（均 v4），
// schema_migrations 表结构与版本记录完全一致——两路可互换、无格式漂移。
func setupTestDB(cfg *mysql.Config) error {
	// 1) 用「不指定库」的连接建库。Clone 会深拷贝 Params，改这份不影响返回给测试的 cfg。
	adminCfg := cfg.Clone()
	adminCfg.DBName = ""
	adminDB, err := sql.Open("mysql", adminCfg.FormatDSN())
	if err != nil {
		return err
	}
	defer adminDB.Close()
	// 反引号包裹库名防注入/保留字；utf8mb4 与生产 & init-testdb 一致。
	if _, err := adminDB.Exec("CREATE DATABASE IF NOT EXISTS `" + cfg.DBName + "` CHARACTER SET utf8mb4"); err != nil {
		return err
	}

	// 2) 迁移连接必须 multiStatements=true（单个迁移文件含多条 CREATE TABLE，
	//    WithInstance 明确要求）。基准 DSN 已带该参数，这里再显式置一次兜底。
	migCfg := cfg.Clone()
	migCfg.MultiStatements = true
	migDB, err := sql.Open("mysql", migCfg.FormatDSN())
	if err != nil {
		return err
	}
	defer migDB.Close()

	dbDriver, err := migmysql.WithInstance(migDB, &migmysql.Config{})
	if err != nil {
		return err
	}
	srcDriver, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "mysql", dbDriver)
	if err != nil {
		return err
	}
	// 已是最新版本时 Up 返回 ErrNoChange，视作成功（幂等）。
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
