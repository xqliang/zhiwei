package repo

import (
	"os"
	"testing"

	"zhiwei/internal/ids"
)

// TestMain 统一初始化雪花 ID 节点（DAO 会生成主键）。
// 注意：TestMain 必须定义在 _test.go 文件里才会被 test 框架调用。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}
