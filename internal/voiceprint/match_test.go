package voiceprint

import "testing"

// TestMatched 两级命中规则的判定表：强命中走阈值；弱命中需「够高 + 明显领先第二名」。
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
		{"拒：相似度不足 0.72", 0.71, 0.0, 0.8, false},
		{"拒：0.75 但第二名 0.5（区分度不足）", 0.75, 0.5, 0.8, false},
		{"拒：gap 恰好差一点", 0.75, 0.16, 0.8, false}, // gap=0.59 < 0.6
		{"边界：gap 恰好 0.6", 0.75, 0.15, 0.8, true},   // gap=0.60 ≥ 0.6
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
