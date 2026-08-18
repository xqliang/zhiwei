package repo

import (
	"os"
	"testing"

	"zhiwei/internal/ids"
)

// TestMain 统一初始化雪花 ID 节点（DAO 会生成主键）。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}

// TestDSN 返回集成测试 DSN；未设置 TEST_MYSQL_DSN 时跳过调用方测试。
// 用法：make test-integration（自动起 docker MySQL + 迁移 + 设置 DSN）。
func TestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	return dsn
}
