// segtrim.go：声纹录入的「时间交集修剪」原语——自动登记（pipeline speaker stage）与
// 手动录入（api EnrollFromSegment）共用，保证「哪些交集算混音证据、剪掉后剩什么」
// 两条路径判定一致。
package voiceprint

import (
	"sort"

	"zhiwei/internal/repo"
)

// OverlapTolMS / OverlapTolFrac 干净音频判定的交集时长容差：候选段与其他标签段的
// **实际交集时长** < min(1s, 候选段时长×15%) 时，视为 diarization 噪声（亚秒嵌套碎片/
// 标签边界渗出）而非真实混音证据，不因此否决该候选段。
//
// 背景（2026-09-02 实测 session 2095044361807990784）：9.7s 的杰辉主力段因内嵌 0.2s
// 噪声碎片「吧？」被判定不干净，整组退回全组聚合——聚合又混进 0.9s 的另一人碎段，
// 对杰辉相似度从 0.8466（强命中）掉到 0.7823、对第二名胡志涛的领先只剩 0.036 <
// GapMin，三条命中规则全败，被误登记成新声纹「说话人gif3n」。更糟的是两个人向量的
// 均值经归一化放大后成了「幽灵质心」——它比两个本体都更像第三方（0.867 > 0.846），
// 另一人的组又被在场锚点归并吸进这个新声纹，最终一条三人录音只剩一个说话人。
// 亚秒嵌套碎片本是 ASR 过度切分的典型噪声（自动流程里会被过短并入规则吸收），用最
// 不可信的标签去否决最可信的长段向量，是本 case 的根因。相对分量 15% 保护短候选段：
// 3s 段内嵌 0.9s（30% 混音）仍会否决，只有长段才放宽到绝对 1s。
//
// 超过容差的显著交集也不必全盘放弃候选段：调用方（pipeline pickCleanSegVec /
// api EnrollFromSegment）会**修剪**——剪掉显著交集的并集后剩余 ≥ 最小时长，就用剩余
// 块重新切片提向（见 LongestTrimmedChunk）。
const (
	// OverlapTolMS 显著交集的绝对容差上限（ms）。
	OverlapTolMS = int64(1000)
	// OverlapTolFrac 显著交集的相对容差（占候选段时长的比例）。
	OverlapTolFrac = 0.15
)

// SignificantOverlap 计算两段的显著交集区间 [lo,hi)：交集时长 ≥ 容差（以 a 段时长为
// 口径：min(OverlapTolMS, a 段时长×OverlapTolFrac)）返回 sig=true，否则 (0,0,false)。
// 干净段判否决（pipeline.overlapsOtherLabel）与修剪剪除（LongestTrimmedChunk）共用
// 此口径，保证「哪些交集算混音证据」在两处判定一致。半开区间 [start,end) 判交。
func SignificantOverlap(a, b repo.TranscriptSegment) (lo, hi int64, sig bool) {
	lo = max(a.StartMS, b.StartMS)
	hi = min(a.EndMS, b.EndMS)
	if hi <= lo {
		return 0, 0, false
	}
	if hi-lo < min(OverlapTolMS, int64(float64(a.EndMS-a.StartMS)*OverlapTolFrac)) {
		return 0, 0, false
	}
	return lo, hi, true
}

// LongestTrimmedChunk 把段 [start,end) 减去与其他标签段（all 中 SpeakerLabel != label
// 者，空 label 也算「其他」——单人录音通常全空标签，组内即全体段）的**显著**时间交集
// 的并集，返回剩余最长块的 [start,end)。无剩余（被完整覆盖）返回 {0,0}；无显著交集
// 返回整段。插话在段中间时剩余不连续，只取最长一块——足够提向，也避免多块多次提向
// 的复杂度。
func LongestTrimmedChunk(seg repo.TranscriptSegment, all []repo.TranscriptSegment, label string) [2]int64 {
	var cuts [][2]int64
	for _, o := range all {
		if o.SpeakerLabel == label {
			continue
		}
		if lo, hi, sig := SignificantOverlap(seg, o); sig {
			cuts = append(cuts, [2]int64{lo, hi})
		}
	}
	// cuts 按起点排序后扫过，收集「并集之外的间隙」并跟踪最长
	sort.Slice(cuts, func(i, j int) bool { return cuts[i][0] < cuts[j][0] })
	var best [2]int64
	cur := seg.StartMS
	for _, c := range cuts {
		if c[0] > cur && c[0]-cur > best[1]-best[0] {
			best = [2]int64{cur, c[0]}
		}
		if c[1] > cur {
			cur = c[1] // 相邻/重叠的 cut 合并推进
		}
	}
	if seg.EndMS > cur && seg.EndMS-cur > best[1]-best[0] {
		best = [2]int64{cur, seg.EndMS}
	}
	return best
}
