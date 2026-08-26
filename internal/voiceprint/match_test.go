package voiceprint

import "testing"

// TestMatched 两级命中规则的判定表：强命中走阈值；弱命中需「够高 + 明显领先第二名」
// （GapMin=0.06，2026-08-26 修正——初值 0.6 在余弦域几乎不可达，弱命中分支实际是死的）。
func TestMatched(t *testing.T) {
	cases := []struct {
		name      string
		top1      float64
		top2      float64
		threshold float64
		want      bool
	}{
		{"强命中：过阈值", 0.82, 0.7, 0.8, true},
		{"强命中：过阈值且第二名也近", 0.85, 0.84, 0.8, true}, // 强命中不要求区分度
		{"弱命中：0.75 明显领先 0.1", 0.75, 0.1, 0.8, true},
		{"弱命中：单声纹库（top2=0）", 0.72, 0.0, 0.8, true},
		{"弱命中：领先刚够 0.06", 0.75, 0.69, 0.8, true}, // gap=0.06 ≥ 0.06
		{"弱命中：第二名也不低但差距足", 0.75, 0.5, 0.8, true}, // gap=0.25，两可场景按规则仍命中
		{"拒：相似度不足 0.72", 0.71, 0.0, 0.8, false},
		{"拒：0.75 但第二名 0.71（区分度不足）", 0.75, 0.71, 0.8, false}, // gap=0.04 < 0.06
		{"拒：gap 恰好差一点", 0.75, 0.7001, 0.8, false},                  // gap≈0.0499 < 0.06
		{"边界：gap 恰好 0.06", 0.75, 0.69, 0.8, true}, // gap=0.60 ≥ 0.6（浮点 0.75-0.69=0.06000000000000005）
		{"边界：top1 恰好 0.72", 0.72, 0.1, 0.8, true},
		{"低分拒绝（远低于软下限）", 0.26, 0.01, 0.8, false},
	}
	for _, c := range cases {
		if got := Matched(c.top1, c.top2, c.threshold); got != c.want {
			t.Errorf("%s: Matched(%.2f, %.2f, %.2f) = %v, want %v",
				c.name, c.top1, c.top2, c.threshold, got, c.want)
		}
	}
}
