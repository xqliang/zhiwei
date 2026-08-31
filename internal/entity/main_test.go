package entity

import (
	"os"
	"testing"

	"zhiwei/internal/ids"
)

// TestMain 统一初始化雪花 ID 节点：seed 测试造 person/pet/todo 等行会生成主键
// （ids.New），未初始化节点会 panic（见 internal/repo/main_test.go 同款约定）。
func TestMain(m *testing.M) {
	_ = ids.InitForTest()
	os.Exit(m.Run())
}
