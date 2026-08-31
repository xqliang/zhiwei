// match.go：1:N 声纹匹配的两级命中判定（纯函数，供 speaker stage、match 预览与
// timeline 列表「整段声纹」判定共用）。
package voiceprint

// 两级命中规则的参数（2026-08-25 需求，2026-08-26 复核修订）：
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
	// 2026-08-26 修正：初值 0.6 在余弦域（[-1,1]）几乎不可达——top1≥0.72 且领先 0.6 意味着
	// top2≤0.12，等于弱命中分支实际永不触发。按需求方明确的规则改为 0.06。
	GapMin = 0.06

	// LooseMin / LooseGap 宽松命中（2026-08-29 需求）：分数明显偏低（0.4~0.72）但相对
	// 第二名区分度足够（领先 ≥ 0.1）时也算命中——说明虽整体音色不够接近，但明确偏向
	// 某一位既有说话人而非两人模糊之间，避免这类段被误登记成新声纹。
	LooseMin = 0.4
	LooseGap = 0.1
)

// Matched 判定一次 1:N 检索是否命中既有声纹。
// top1/top2 为最高与次高相似度（余弦，L2 归一向量的内积）；top2 传 0 表示
// 库中无第二名（单声纹库——无混淆对象，天然视为「明显领先」）。
// threshold 为强命中阈值（配置注入）。
func Matched(top1, top2, threshold float64) bool {
	if top1 >= threshold {
		return true // ① 强命中：只看分数，不要求区分度
	}
	if top1 >= SoftMin && top1-top2 >= GapMin {
		return true // ② 区分性弱命中
	}
	// ③ 宽松命中（低分但区分度够）。top2=0（库中仅 1 人）时**不生效**：没有第二名就谈不上
	// 「相对第二名区分度足够」，规则退化为「≥0.4 即命中」——而实测不同人声纹互余弦
	// 0.55~0.79 很常见，单声纹库会把新出现的不同人误归并给库里唯一那个人（2026-08-31
	// 修复：曾令 main 的 phantom/过短并入两用例回归失红）。弱命中②的 top2=0 语义保留
	//（≥0.72 本就是同人区间，单声纹库无混淆对象）。
	return top2 > 0 && top1 >= LooseMin && top1-top2 >= LooseGap
}
