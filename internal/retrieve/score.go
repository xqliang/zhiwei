package retrieve

import (
	"math"
	"time"
)

// 混合打分权重（spec §10；无向量版是 0.5kw+0.3recency+0.2imp，本期加向量项，可调）。
const (
	wSim        = 0.5
	wKw         = 0.2
	wRecency    = 0.15
	wImportance = 0.15
)

// cosine 余弦相似度；维度不符/空/零范数 → 0。
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	r := dot / (math.Sqrt(na) * math.Sqrt(nb))
	// 防御：外部损坏的 blob 可能是「长度合法但 bit pattern 为 NaN/Inf」的向量，
	// 会让 dot/范数算出 NaN/Inf；NaN 会污染排序（与任何值比较恒 false）。归零剔除。
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return 0
	}
	return r
}

// recencyScore 线性时新度：clamp(1 - ageDays/365, 0, 1)。at 为 nil → 0。
func recencyScore(at *time.Time, now time.Time) float64 {
	if at == nil {
		return 0
	}
	ageDays := now.Sub(*at).Hours() / 24
	return clamp01(1 - ageDays/365)
}

// blend 混合四项分数（sim 已 clamp 到 [0,1]）。
func blend(sim, kw, recency, importance float64) float64 {
	return wSim*clamp01(sim) + wKw*clamp01(kw) + wRecency*clamp01(recency) + wImportance*clamp01(importance)
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
