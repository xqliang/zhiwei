// match.go：1:N 声纹匹配的两级命中判定（纯函数，供 speaker stage 与 match 预览共用）。
package voiceprint

// 两级命中规则的参数（2026-08-25 需求）：
//
// ① 强命中：top1 ≥ threshold（ZW_VOICEPRINT_THRESHOLD，默认 0.8）；
// ② 区分性弱命中：top1 ≥ SoftMin 且 top1−top2 ≥ GapMin——分数略低于阈值但
//   明显领先第二名（说明不是两个相近声纹之间的模糊匹配）时也复用既有声纹，
//   减少 0.72~0.8 区间的真匹配被误登记成新说话人/新声纹。
// 数值为经验初值，后续用真实录音 benchmark 实调（对齐 ZW_VOICEPRINT_THRESHOLD 的调法）。
const (
	// SoftMin 弱命中的最低相似度下限。
	SoftMin = 0.72
	// GapMin top1 相对第二名（top2）的最小领先幅度。
	GapMin = 0.6
)

// Matched 判定一次 1:N 检索是否命中既有声纹。
// top1/top2 为最高与次高相似度（余弦，L2 归一向量的内积）；top2 传 0 表示
// 库中无第二名（单声纹库——无混淆对象，天然视为「明显领先」）。
// threshold 为强命中阈值（配置注入）。
func Matched(top1, top2, threshold float64) bool {
	if top1 >= threshold {
		return true // ① 强命中：只看分数，不要求区分度
	}
	return top1 >= SoftMin && top1-top2 >= GapMin // ② 区分性弱命中
}
