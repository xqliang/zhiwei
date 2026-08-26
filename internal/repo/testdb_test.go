package repo

import "testing"

// TestCallerPkgDBName 锁定 TestDSN 的核心魔法：runtime.Caller 深度 + 目录名解析。
//
// 本测试文件位于 internal/repo/，直接调 callerPkgDBName(1)——skip=1 即本测试函数
//（callerPkgDBName 自身为 skip=0），其源文件目录名为 repo，故应得 zhiwei_test_repo。
// 这同时验证：
//  1. 目录名 → 库名的拼接正确（filepath.Dir 取目录、Base 取包名、加 zhiwei_test_ 前缀）；
//  2. Caller 深度约定成立——TestDSN 内以 callerPkgDBName(2) 上溯一层到「调用 TestDSN
//     的测试文件」，与此处 skip=1 差且仅差 TestDSN 这一帧，故 TestDSN 的取值同样正确。
//
// 该测试不连数据库，无 TEST_MYSQL_DSN 也能跑（纯栈帧/字符串断言）。
func TestCallerPkgDBName(t *testing.T) {
	const want = "zhiwei_test_repo"
	if got := callerPkgDBName(1); got != want {
		t.Fatalf("包名解析错误：want=%s got=%s", want, got)
	}
}
