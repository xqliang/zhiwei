package retrieve

import (
	"math"
	"testing"
	"time"
)

func TestCosine(t *testing.T) {
	if v := cosine([]float32{1, 0}, []float32{1, 0}); v < 0.999 {
		t.Errorf("同向应≈1, got %v", v)
	}
	if v := cosine([]float32{1, 0}, []float32{0, 1}); v > 0.001 {
		t.Errorf("正交应≈0, got %v", v)
	}
	if v := cosine([]float32{1, 0}, nil); v != 0 {
		t.Errorf("维度不符/空应 0, got %v", v)
	}
	// 损坏 blob 解出的 NaN 向量：cosine 必须归零（否则 NaN 污染排序）。
	if v := cosine([]float32{float32(math.NaN()), 0}, []float32{1, 0}); v != 0 {
		t.Errorf("NaN 向量应归零, got %v", v)
	}
}

func TestRecency(t *testing.T) {
	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	today := now
	old := now.AddDate(-1, 0, 0)
	if recencyScore(&today, now) < 0.99 {
		t.Error("当天应≈1")
	}
	if r := recencyScore(&old, now); r > 0.05 {
		t.Errorf("一年前应≈0, got %v", r)
	}
	if recencyScore(nil, now) != 0 {
		t.Error("无时间应 0")
	}
}

func TestBlendRanksSemanticFirst(t *testing.T) {
	a := blend(0.9, 0, 0.0, 0.0)
	b := blend(0.1, 0, 1.0, 1.0)
	if a <= b {
		t.Errorf("高相似(%.3f) 应 > 低相似高时新(%.3f)", a, b)
	}
}
