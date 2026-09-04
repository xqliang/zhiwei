// pair.go：说话人两两相似度（纯函数，声纹页「手动合并」前的相似度预检用）。
package voiceprint

// Cosine 两个 L2 归一向量的余弦相似度（= 内积）。声纹向量由 sidecar 归一化，
// 与 sidecar FAISS IndexFlatIP(内积) 等价——BLOB 与索引同向量，结果一致。
//
// 相似度语义收敛到本包做单一事实源：原 internal/api/speaker.go 的 cosine 已改为
// 转调本函数（Task 2）。另有一份同实现拷贝 internal/pipeline/stage_speaker.go 的
// dotSim，属另一包且不参与本次改动，保持原样。
func Cosine(a, b []float32) float64 {
	var s float64
	n := len(a)
	if len(b) < n {
		n = len(b) // 维度不一致取较短者：防御脏数据，不报错（真实向量恒 256 维）
	}
	for i := 0; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// MaxCosine 两组向量间的最大余弦相似度（多向量语义：与对方任意一条样本的最高分）。
// 与 Matched / MatchPreview 的「多向量取 max」同口径——感冒/哑嗓变体不会稀释分数。
//
// 任一方无样本返回 0：「不可比」与「完全不同」在纯向量域无法区分，交由调用方（handler）
// 用「该说话人是否存在于样本表」另行判定后以 null 表达。
func MaxCosine(a, b [][]float32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var best float64
	for _, x := range a {
		for _, y := range b {
			if s := Cosine(x, y); s > best {
				best = s
			}
		}
	}
	return best
}
