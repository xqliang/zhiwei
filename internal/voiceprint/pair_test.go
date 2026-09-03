package voiceprint

import (
	"math"
	"testing"
)

// TestMaxCosinePicksHighestAcrossSamples 多向量语义：与对方任意一条样本的最高分，
// 不是与聚合代表（质心）的分数——感冒/哑嗓变体不得稀释分数（与 Matched/MatchPreview 同口径）。
func TestMaxCosinePicksHighestAcrossSamples(t *testing.T) {
	// 3 维且全部 L2 归一化，点积即余弦。
	a := [][]float32{{1, 0, 0}, {0, 1, 0}}
	b := [][]float32{{0.6, 0.8, 0}, {0, 0, 1}, {0.8, 0.6, 0}}
	// 最高分两处同为 0.8：a1·b3 与 a2·b1；b2 与两者正交=0，不得成为最高分。
	if got := MaxCosine(a, b); math.Abs(got-0.8) > 1e-6 {
		t.Fatalf("MaxCosine = %v, want 0.8", got)
	}
}

// TestMaxCosineSymmetric 相似度必须对称（前端按 id 升序生成上三角对，顺序无关）。
func TestMaxCosineSymmetric(t *testing.T) {
	a := [][]float32{{1, 0, 0}, {0, 1, 0}}
	b := [][]float32{{0.6, 0.8, 0}, {0, 0, 1}}
	if fwd, rev := MaxCosine(a, b), MaxCosine(b, a); math.Abs(fwd-rev) > 1e-9 {
		t.Fatalf("不对称: fwd=%v rev=%v", fwd, rev)
	}
}

// TestMaxCosineEmptySideIsZero 任一方无样本返回 0——纯向量域无法表达「不可比」，
// handler 须用「样本表是否有该人」另行判定后以 null 呈现（spec §3/§7）。
func TestMaxCosineEmptySideIsZero(t *testing.T) {
	one := [][]float32{{1, 0, 0}}
	for _, tc := range []struct {
		name string
		a, b [][]float32
	}{
		{"a 空", nil, one},
		{"b 空", one, nil},
		{"双侧空", nil, nil},
	} {
		if got := MaxCosine(tc.a, tc.b); got != 0 {
			t.Fatalf("%s: want 0, got %v", tc.name, got)
		}
	}
}

// TestCosineTruncatesToShorterDim 维度不一致取较短者，与旧 api.cosine 行为一致
// （防御脏数据不报错；真实向量恒为 256 维，此分支仅兜底）。
func TestCosineTruncatesToShorterDim(t *testing.T) {
	if got := Cosine([]float32{1, 0, 0}, []float32{1, 0, 0, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("同向 want 1, got %v", got)
	}
	if got := Cosine([]float32{0, 1, 0}, []float32{1, 0, 0, 0}); math.Abs(got-0) > 1e-9 {
		t.Fatalf("正交 want 0, got %v", got)
	}
	// 反向：a 比 b 长，走 pair.go 里 `if len(b) < n { n = len(b) }` 这条截断分支
	// （上两组 a 恒短，n 始终取 len(a)，该分支从不执行；补此组才真正覆盖守卫的另一侧）。
	if got := Cosine([]float32{1, 0, 0, 0}, []float32{1, 0, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("同向(a 长) want 1, got %v", got)
	}
	if got := Cosine([]float32{0, 0, 1, 0}, []float32{1, 0, 0}); math.Abs(got-0) > 1e-9 {
		t.Fatalf("正交(a 长) want 0, got %v", got)
	}
}

// BenchmarkMaxCosine 佐证成本可忽略（CLAUDE.md：性能须有数据）。
// 造 10×10 样本对（256 维，贴近单人样本量上限）——handler 端 N=10 时要跑 45 对。
func BenchmarkMaxCosine(b *testing.B) {
	a := make([][]float32, 10)
	c := make([][]float32, 10)
	for i := range a {
		va := make([]float32, 256)
		vc := make([]float32, 256)
		for j := range va {
			va[j] = float32((i*7 + j) % 13)
			vc[j] = float32((i*5 + j*3) % 11)
		}
		a[i], c[i] = va, vc
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MaxCosine(a, c)
	}
}
