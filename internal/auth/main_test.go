package auth

import (
	"os"
	"testing"

	"zhiwei/internal/ids"
)

// TestMain 统一初始化雪花 ID 节点。
// 说明：本包测试当前不直接用 ids.New() 生成主键（session token 走 NewToken，
// app_user 主键由迁移播种），但为与 repo/retrieve 的测试装配保持一致、并为将来
// 可能的 ids.New() 调用兜底，这里仍显式 Init。TestMain 必须定义在 _test.go 里
// 才会被 test 框架调用。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}
