package retrieve

import (
	"os"
	"testing"

	"zhiwei/internal/ids"
)

// TestMain 统一初始化雪花 ID 节点（retriever_test 的 seed 走 InsertExt → ids.New()）。
// 与 internal/repo/main_test.go 同构：TestMain 必须定义在 _test.go 里才会被 test 框架调用。
// 说明：任务给定的 retriever_test.go 未含此初始化，独立测试包必须自备，否则 ids.New() 因
// 节点未初始化而 panic（nil *snowflake.Node）。这是唯一偏离，见收尾报告。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}
