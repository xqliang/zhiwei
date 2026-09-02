package voiceprint

import (
	"testing"

	"zhiwei/internal/repo"
)

// seg 造 [start,end) 的段（label 控制与候选段的同/异标签）。
func seg(label string, start, end int64) repo.TranscriptSegment {
	return repo.TranscriptSegment{SpeakerLabel: label, StartMS: start, EndMS: end}
}

// TestLongestTrimmedChunk 修剪区间运算的表驱动用例：候选段固定 10s [0,10000)
//（容差 = min(1s, 15%×10s) = 1s，交集 ≥1s 才剪）。
func TestLongestTrimmedChunk(t *testing.T) {
	cases := []struct {
		name   string
		others []repo.TranscriptSegment
		want   [2]int64
	}{
		{"无其他标签段", nil, [2]int64{0, 10000}},
		{"同标签段不算交集", []repo.TranscriptSegment{seg("1", 0, 10000)}, [2]int64{0, 10000}},
		{"无交集（相接）", []repo.TranscriptSegment{seg("2", 10000, 12000)}, [2]int64{0, 10000}},
		{"容差内交集不剪（0.9s）", []repo.TranscriptSegment{seg("2", 5000, 5900)}, [2]int64{0, 10000}},
		{"头部交集剪掉", []repo.TranscriptSegment{seg("2", 0, 2000)}, [2]int64{2000, 10000}},
		{"尾部交集剪掉", []repo.TranscriptSegment{seg("2", 8000, 12000)}, [2]int64{0, 8000}},
		{"中部交集取前块（等长取先）", []repo.TranscriptSegment{seg("2", 4000, 6000)}, [2]int64{0, 4000}},
		{"两处交集取最长剩余", []repo.TranscriptSegment{seg("2", 2000, 3000), seg("2", 7000, 8000)}, [2]int64{3000, 7000}},
		{"相邻交集合并剪除", []repo.TranscriptSegment{seg("2", 2000, 4000), seg("2", 4000, 6000)}, [2]int64{6000, 10000}},
		{"被完整覆盖无剩余", []repo.TranscriptSegment{seg("2", 0, 12000)}, [2]int64{0, 0}},
		{"容差内外混合只剪显著的", []repo.TranscriptSegment{seg("2", 1000, 1500), seg("2", 8000, 9000)}, [2]int64{0, 8000}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := LongestTrimmedChunk(seg("1", 0, 10000), append([]repo.TranscriptSegment{seg("1", 500, 800)}, c.others...), "1")
			if got != c.want {
				t.Fatalf("LongestTrimmedChunk = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSignificantOverlap 容差口径：交集时长 ≥ min(1s, a 段×15%) 才算显著，
// 且以 a 段（候选段）时长为基准。
func TestSignificantOverlap(t *testing.T) {
	// 10s 候选段：容差 1s
	a := seg("1", 0, 10000)
	if _, _, sig := SignificantOverlap(a, seg("2", 5000, 5900)); sig {
		t.Fatal("0.9s 交集应低于容差不显著")
	}
	if lo, hi, sig := SignificantOverlap(a, seg("2", 5000, 6000)); !sig || lo != 5000 || hi != 6000 {
		t.Fatalf("1s 交集应显著（≥ 容差），got lo=%d hi=%d sig=%v", lo, hi, sig)
	}
	// 3s 候选段：容差 = 15%×3s = 450ms，0.4s 交集不显著、1s 交集显著
	b := seg("1", 0, 3000)
	if _, _, sig := SignificantOverlap(b, seg("2", 500, 900)); sig {
		t.Fatal("3s 段的 0.4s 交集应低于相对容差(450ms)不显著")
	}
	if _, _, sig := SignificantOverlap(b, seg("2", 500, 1500)); !sig {
		t.Fatal("3s 段的 1s 交集应显著")
	}
	// 无交集
	if _, _, sig := SignificantOverlap(a, seg("2", 10000, 12000)); sig {
		t.Fatal("相接不算交集")
	}
}
