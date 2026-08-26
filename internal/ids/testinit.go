// testinit.go 提供「测试专用」的雪花节点初始化入口 InitForTest。
//
// 为什么放在普通 .go 文件、而不是放在 _test.go 里：
// Go 里定义在 *_test.go 的符号只在其所属包内部可见，无法被“其它包的测试”导入。
// 而 api / repo / profile / pipeline / voiceprint 这些包的测试都要初始化雪花节点，
// 需要共用同一个可跨包导入的入口，所以 InitForTest 只能定义在可导出的普通源文件里。
// 代价是它会被编译进生产二进制，但生产代码从不调用它（生产固定 ids.Init(1)，
// 见 cmd/zhiwei-server/main.go），函数本身也极轻量，这个取舍可接受。
package ids

import (
	"os"

	"github.com/bwmarrin/snowflake"
)

// InitForTest 为「测试进程」初始化雪花节点，使用基于进程 PID 的进程内唯一 node。
//
// 解决的问题（F6）：`go test ./...` 默认按包并行（并行度 = GOMAXPROCS ≈ CPU 数），
// 每个被测包各自编译成独立的测试二进制、以独立进程运行。若所有进程都 ids.Init(1)，
// 就是「同 nodeID(=1) + 同毫秒 + 同 step」——雪花算法必然生成完全相同的 ID；而这些
// 进程又共享同一个 zhiwei_test 库，于是并发写入时撞主键。历史上靠 `go test -p 1`
//（强制串行、单进程）规避，代价是整套测试变慢。这里从源头消除「同 node」这一撞库因素。
//
// 方案：用 PID 对 node 值域取模，给每个测试进程分配一个互异的 node。
//
// 碰撞面评估（为什么可接受）：
//   - snowflake 默认 NodeBits=10，node 值域是 [0, 1023]，共 1024 个取值。
//   - go test 同一时刻只并行跑 GOMAXPROCS 个测试进程（个位数量级）。
//   - 同一批被 OS 近乎同时创建的进程，PID 通常是递增相邻的；两个并发进程的 PID
//     恰好对 1024 同余（即相差 1024 的整数倍）的概率极低。
//   - 即便万一同余，也只是退回到「与旧方案相同」的撞库概率，不会更糟，重跑即可。
//   因此不引入更复杂的分配（如 PID + 纳秒混合），PID 取模已足够且更易理解。
//
// 返回值与 ids.Init 保持一致（error），便于调用点把 `ids.Init(1)` 原样替换为
// `ids.InitForTest()`——无论写法是 `_ = ids.InitForTest()` 还是
// `if err := ids.InitForTest(); err != nil`。
func InitForTest() error {
	// nodeRange = 2^NodeBits（默认 1024）。用库里的 NodeBits 计算、而非写死 1024，
	// 这样将来即使调整了 NodeBits，node 依然会落在合法范围 [0, nodeMax] 内。
	nodeRange := int64(1) << snowflake.NodeBits
	// os.Getpid() 在所有平台都返回正整数，取模结果落在 [0, nodeRange-1] = [0, nodeMax]，
	// 恒为合法 nodeID，故这里 Init 不会因 node 越界而报错。
	node := int64(os.Getpid()) % nodeRange
	return Init(node)
}
