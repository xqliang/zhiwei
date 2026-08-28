// stage_speaker 实现 speaker stage：切片→提向→聚合→1:N 检索/登记→回填 segment.speaker_id。
// 解析粒度：按 ASR 说话人标签分组（ASR 原生 diarization 已完成 session 内聚类），
// 每组聚一个代表声纹做跨 session 1:N，避免同一段新说话人被重复登记。
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// runSpeakerStage 是 speaker stage 的可测核心（避开 pool），由 stageSpeaker 包装成 Handler。
//
// 流程：按 ASR speaker_label 分组 → 逐段切片提向 → 组代表声纹 →
// 每组（= 一个 ASR 说话人）做跨 session 1:N（先对全部组检索、再统一登记未命中的，见步骤 2 注释）
// → 回填组内段 speaker_id（仅填 NULL，保留手动纠正）。
//
// ASR 原生 diarization 已足够准，此处直接信任其说话人标签：不再用声纹在本地把不同 ASR 标签
// 合并成同一人。是否同一人只由跨 session 1:N 判定——相似度 ≥ 阈值（默认 0.8）视为命中、复用该
// speaker；否则登记为新声纹。登记向量优先取「干净段」（见 pickCleanSegVec）：时长最长、
// 与其他说话人段无时间交集且 ≥3s 的单段——聚合向量会被 diarization 切错/混入他人语音的段
// 污染；无干净段才退回全组聚合（2026-08-26 需求）。
func runSpeakerStage(ctx context.Context, d StageDeps, sessionID ids.ID, tr *repo.Transcript) error {
	segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return fmt.Errorf("读 segments: %w", err)
	}
	// 按 speaker_label 分组（空 label 归 ""，对应单人/未标注录音）
	groups := map[string][]repo.TranscriptSegment{}
	var order []string // 保留出现顺序，避免 map 遍历乱序
	for _, s := range segs {
		if _, ok := groups[s.SpeakerLabel]; !ok {
			order = append(order, s.SpeakerLabel)
		}
		groups[s.SpeakerLabel] = append(groups[s.SpeakerLabel], s)
	}

	sliceDir := filepath.Join(d.DataDir, "slices", sessionID.String())
	_ = os.MkdirAll(sliceDir, 0o755)
	defer os.RemoveAll(sliceDir) // 清理切片（best-effort）

	wavPath := filepath.Join(d.DataDir, "transcoded", sessionID.String()+".wav")
	threshold := d.VoiceprintThreshold
	if threshold == 0 {
		threshold = 0.8 // 同一人判定阈值：cosine ≥ 0.8 视为同人，否则登记新声纹
	}

	// 1) 逐组切片+提向，跳过已全部解析的组（幂等：reextract 不重复调 sidecar、不覆盖手动纠正）。
	var reps []groupRep
	// 逐段声纹向量 BLOB（segID→blob）：speaker stage 提取向量后落库，
	// 供详情页按「每个 ASR 段」展示与声纹库的相似度 top-N——一句话可能混多个人，
	// 段级相似度才能审计 diarization 切分/归属是否正确。
	segEmbeds := map[ids.ID][]byte{}
	for _, label := range order {
		members := groups[label]
		allAssigned := len(members) > 0
		for _, s := range members {
			if s.SpeakerID == nil {
				allAssigned = false
				break
			}
		}
		if allAssigned {
			continue
		}
		svs := make([]segVec, 0, len(members))
		for _, s := range members {
			slicePath := filepath.Join(sliceDir, fmt.Sprintf("seg-%d.wav", s.SequenceNo))
			if err := sliceAudio(wavPath, slicePath, s.StartMS, s.EndMS); err != nil {
				continue // 切片失败跳过该段（speaker_id 留 NULL）
			}
			v, err := d.Voiceprint.Embed(ctx, slicePath)
			if err != nil || len(v) != 256 {
				continue // 提向失败跳过
			}
			svs = append(svs, segVec{seg: s, vec: v})
			segEmbeds[s.ID] = float32Blob(v)
		}
		if len(svs) == 0 {
			continue
		}
		vecs := make([][]float32, 0, len(svs))
		for _, sv := range svs {
			vecs = append(vecs, sv.vec)
		}
		var durMS int64
		for _, sv := range svs {
			durMS += sv.seg.EndMS - sv.seg.StartMS
		}
		reps = append(reps, groupRep{
			label: label, rep: aggregateEmbeddings(vecs), vecN: len(vecs),
			clean:   pickCleanSegVec(svs, segs, label),
			segVecs: svs,
			durMS:   durMS,
		})
	}
	if len(reps) == 0 {
		return nil
	}
	// 逐段声纹向量落库（段级相似度审计的数据来源；失败按 DB 错误走 pool 重试）
	if len(segEmbeds) > 0 {
		if err := d.Transcripts.SaveSegmentEmbeddings(ctx, tr.ID, segEmbeds); err != nil {
			return fmt.Errorf("落库逐段声纹: %w", err)
		}
	}

	// 2) 每组（= 一个 ASR 说话人）做跨 session 1:N 检索/登记 → 回填该组未解析段。
	// 分两趟：**先对全部组做检索、再统一登记未命中的**——保证「本 run 内新登记的声纹对
	// 后续组的 Search 不可见」。否则先登记的说话人会出现在后续组的检索结果里：当声纹库
	// 原本为空、这次又是多人录音时，库里唯一那条恰是本次的另一个人，弱命中的 top2=0（库中
	// 无第二名 → gap 规则恒真）会把门槛从 threshold 降到 SoftMin，把第二个真人并进第一个人
	// → 首次多人只建 1 个声纹（且自我强化：一直并入使库始终只有 1 条，陷阱关不上）。
	// 只对「本 run 开始前的历史库」检索，与既有设计一致：信任 ASR diarization，本 session
	// 内不同标签一律视为不同人（本录音内误拆靠手动「合并」），跨 session 才靠 1:N 归并；
	// 历史库的弱命中重认（同一人换环境相似度掉到 0.72~0.8）不受影响。
	matched := make([]bool, len(reps))     // 该组是否命中既有声纹
	matchedID := make([]ids.ID, len(reps)) // 命中的既有 speaker id
	for i, g := range reps {
		res, err := d.Voiceprint.Search(ctx, g.rep)
		if err != nil {
			return fmt.Errorf("voiceprint search: %w", err)
		}
		// 同一人判定（两级规则，见 voiceprint.Matched）：强命中 sim≥阈值；
		// 或区分性弱命中 sim≥0.72 且明显领先第二名（top1−top2≥0.06）——
		// 分数略低于阈值但明显是同一个人的也复用，减少真匹配被误登记成新声纹。
		if res.Matched && voiceprint.Matched(res.Distance, res.SecondDistance, threshold) {
			// 防御性校验：FAISS 索引可能残留已删说话人（删 DB 时 /remove 失败/旧装配未清），
			// 此时 Search 返回幽灵 id——必须确认该 speaker 仍存在于 DB，否则按未命中登记新声纹。
			if sp, gerr := d.Speakers.Get(ctx, res.SpeakerID); gerr != nil || sp == nil {
				log.Printf("[speaker] Search 命中幽灵说话人 %s（DB 已不存在，按未命中登记新声纹）: %v", res.SpeakerID, gerr)
				continue
			}
			matched[i], matchedID[i] = true, res.SpeakerID
		}
	}
	// 预判是否存在可作「过短并入」目标的组（命中历史库 or 非过短新组）。
	// 全部组都过短时不缓起——退回照常登记，保证段有归属、库不空。
	hasTarget := false
	for i, g := range reps {
		if matched[i] || g.durMS >= minCleanSegMS {
			hasTarget = true
			break
		}
	}
	// 第二趟：命中的复用；非过短未命中的登记新声纹；过短未命中的缓起(deferred)——不登记、段留 NULL，pass3 并入。
	resolvedID := make([]ids.ID, len(reps)) // 每组最终 speaker（deferred 组留零值，不作目标）
	deferred := make([]bool, len(reps))
	for i, g := range reps {
		if !matched[i] && hasTarget && g.durMS < minCleanSegMS {
			deferred[i] = true // 过短噪声组：不建 speaker/不入 FAISS，pass3 并入最近在场说话人
			continue
		}
		var speakerID ids.ID
		if matched[i] {
			speakerID = matchedID[i]
		} else {
			// 自动登记：name=说话人{5位随机串}，向量 BLOB 灾备。
			// 登记向量优先用干净段（pickCleanSegVec 的结果）：混入他人语音的段会污染
			// 聚合向量，新声纹「出厂即脏」；无干净段才退回聚合代表。
			embVec, sampleN := g.rep, g.vecN
			if g.clean != nil {
				embVec, sampleN = g.clean, 1
			}
			sp := &repo.Speaker{Name: "说话人" + rand5(), Source: "auto", Embedding: float32Blob(embVec), SampleCount: sampleN}
			if err := d.Speakers.Create(ctx, sp); err != nil {
				return fmt.Errorf("登记 speaker: %w", err)
			}
			if err := d.Voiceprint.Add(ctx, embVec, sp.ID); err != nil {
				return fmt.Errorf("voiceprint add: %w", err)
			}
			// 样本行落库（多条声纹模型；nil = 旧装配跳过。失败仅 log 不致命——speaker/FAISS
			// 已就绪，样本行缺失只影响后续聚合重算来源，可用启动 bootstrap 兜底补齐）
			if d.SpeakerEmbeddings != nil {
				e := &repo.SpeakerEmbedding{SpeakerID: sp.ID, Embedding: float32Blob(embVec), SampleCount: sampleN, Source: "auto"}
				if err := d.SpeakerEmbeddings.Create(ctx, e); err != nil {
					log.Printf("[speaker] 自动登记后样本行落库失败 speaker=%s: %v", sp.ID, err)
				}
			}
			speakerID = sp.ID
		}
		resolvedID[i] = speakerID
		if err := d.Transcripts.SetSegmentSpeaker(ctx, tr.ID, g.label, speakerID); err != nil {
			return fmt.Errorf("回填 speaker_id: %w", err)
		}
	}

	// 3) 纠正 pass：先幽灵历史声纹纠正，再过短段并入。两者共享「各在场说话人样本向量」。
	samples := buildGroupSamples(ctx, d, reps, matched, resolvedID)
	margin := d.VoiceprintCorrectMargin
	if margin == 0 {
		margin = defaultCorrectMargin
	}
	if err := correctPhantomHistoricalMatches(ctx, d, tr, reps, matched, deferred, resolvedID, samples, margin); err != nil {
		return err
	}
	if err := mergeShortGroups(ctx, d, tr, reps, deferred, resolvedID, samples); err != nil {
		return err
	}
	return nil
}

// minCleanSegMS 干净段（登记声纹优先来源）的最短时长：3s——太短的段声纹特征不稳。
const minCleanSegMS = 3000

// defaultCorrectMargin 幽灵历史声纹纠正的默认领先幅度门槛（沿用 voiceprint.GapMin 经验值）。
// max 相似度口径下，真人在幽灵段上需比历史人自身 max 领先该幅度才改判，挡住接近平局的噪声翻转。
const defaultCorrectMargin = 0.06

// correctScoreEps 判定容差：声纹向量是 float32（相对精度 ~1e-7），逐维内积在「恰好等于
// self+margin」的边界上会被 float32 表示误差顶过阈值（如 float32(0.79)=0.790000021）。
// 加此容差保证「严格大于才纠正」的语义对边界稳健——领先幅度需真正超过 margin，float32
// 噪声级别的微弱超出不触发翻转。远大于噪声的正常改判（领先 ≥0.09）不受影响。
const correctScoreEps = 1e-6

// correctPhantomHistoricalMatches 幽灵历史声纹纠正（2026-08-27 需求）：
// ASR 过度切分出的幽灵组常命中历史库某真人；若该组名下的段被同录音另一在场说话人
// 匹配得更好（max 相似度口径，与详情页 topVoiceMatchesVec 同口径），判为幽灵、整组改判
// 给那个人，段写 corrected_from。仅**历史命中组**（matched[i]）参与——新登记组的声纹是
// 从自己段建出来的、天生在自己段上最高，不可能被判幽灵。先算全部判定（基于本趟归属快照）、
// 再统一应用，避免链式/互换改判抖动。
func correctPhantomHistoricalMatches(ctx context.Context, d StageDeps, tr *repo.Transcript,
	reps []groupRep, matched []bool, deferred []bool, resolvedID []ids.ID, samples [][][]float32, margin float64) error {
	if len(reps) < 2 {
		return nil // 少于两个在场说话人无可比对象
	}
	type fix struct {
		label    string
		from, to ids.ID
	}
	var fixes []fix
	for i, g := range reps {
		if !matched[i] || len(g.segVecs) == 0 || len(samples[i]) == 0 {
			continue // 仅历史命中组是候选
		}
		// self = 该组段对「历史人自己」的最高相似度（对样本取 max，再对段取 max）
		self := 0.0
		for _, sv := range g.segVecs {
			if s := segMaxScore(sv.vec, samples[i]); s > self {
				self = s
			}
		}
		// 找在场其他说话人里，在本组段上得分最高者
		bestScore, bestJ := -1.0, -1
		for j := range reps {
			if j == i || deferred[j] || resolvedID[j] == resolvedID[i] {
				continue // 跳过自己、过短缓起组(无有效 speaker)、解析到同一 speaker 的组
			}
			sc := 0.0
			for _, sv := range g.segVecs {
				if s := segMaxScore(sv.vec, samples[j]); s > sc {
					sc = s
				}
			}
			if sc > bestScore {
				bestScore, bestJ = sc, j
			}
		}
		if bestJ >= 0 && bestScore > self+margin+correctScoreEps {
			fixes = append(fixes, fix{label: g.label, from: resolvedID[i], to: resolvedID[bestJ]})
		}
	}
	for _, f := range fixes {
		if err := d.Transcripts.CorrectSegmentSpeaker(ctx, tr.ID, f.label, f.from, f.to); err != nil {
			// best-effort：纠正失败仅 log 不致命。此时段已回填到（幽灵）说话人、并非无归属；
			// 若返回错误让 job 重试，重试时段已 assigned → reps 为空 → 纠正永不重跑，反而是「既失败又丢纠正」
			// 的最坏情况。与本 stage 样本行落库失败(SpeakerEmbeddings.Create)的 best-effort+log 处理一致。
			log.Printf("[speaker] 幽灵历史声纹纠正失败 label=%s from=%s to=%s: %v", f.label, f.from, f.to, err)
		}
	}
	return nil
}

// buildGroupSamples 为每组构造「该说话人的样本向量集合」（详情页同口径打分用）：
// 命中历史库 → 库内多条样本(回退聚合代表)；其余(新登记/deferred) → 登记向量(clean 优先，否则 rep)。
// deferred 组的样本不会被用作并入目标（调用方按 deferred 跳过），此处一并构造无害。
func buildGroupSamples(ctx context.Context, d StageDeps, reps []groupRep, matched []bool, resolvedID []ids.ID) [][][]float32 {
	samples := make([][][]float32, len(reps))
	for i, g := range reps {
		if matched[i] {
			samples[i] = loadSpeakerSampleVecs(ctx, d, resolvedID[i])
		} else {
			embVec := g.rep
			if g.clean != nil {
				embVec = g.clean
			}
			samples[i] = [][]float32{embVec}
		}
	}
	return samples
}

// mergeShortGroups 过短噪声段并入（2026-08-28 需求）：把 pass2 缓起(deferred)的过短组
// 整组并入本录音里最匹配的「非过短在场说话人」（max 余弦，详情页同口径），无阈值——噪声句总要
// 归给对话中某人。目标候选排除其他 deferred 组。best-effort：失败仅 log（段已在 pass2 留 NULL，
// 并入失败则维持 NULL，不致命）。hasTarget 保证存在非 deferred 组；若其样本恰好全为空(历史匹配且
// 向量不可取，极端)则 bestJ=-1，该段维持 NULL（与提向失败同等降级，不致命）。
func mergeShortGroups(ctx context.Context, d StageDeps, tr *repo.Transcript,
	reps []groupRep, deferred []bool, resolvedID []ids.ID, samples [][][]float32) error {
	type fix struct {
		label string
		to    ids.ID
	}
	var fixes []fix
	for i, g := range reps {
		if !deferred[i] || len(g.segVecs) == 0 {
			continue
		}
		bestScore, bestJ := -1.0, -1
		for j := range reps {
			if j == i || deferred[j] || len(samples[j]) == 0 {
				continue // 目标须是非过短、有样本的在场说话人
			}
			sc := 0.0
			for _, sv := range g.segVecs {
				if s := segMaxScore(sv.vec, samples[j]); s > sc {
					sc = s
				}
			}
			if sc > bestScore {
				bestScore, bestJ = sc, j
			}
		}
		if bestJ >= 0 {
			// resolvedID[bestJ] 指向真实存在的 speaker；即便该目标组的段刚被幽灵纠正搬走(本录音内可能零段)，并入引用仍有效。
			fixes = append(fixes, fix{label: g.label, to: resolvedID[bestJ]})
		}
	}
	for _, f := range fixes {
		if err := d.Transcripts.MergeShortGroup(ctx, tr.ID, f.label, f.to); err != nil {
			log.Printf("[speaker] 过短段并入失败 label=%s to=%s: %v", f.label, f.to, err)
		}
	}
	return nil
}

// segMaxScore 段向量对某说话人「多条样本取最大余弦」——与详情页 topVoiceMatchesVec 同口径，
// 保证纠正判定与用户在详情页看到的段级相似度数字一致。样本为空返回 0。
func segMaxScore(seg []float32, sampleVecs [][]float32) float64 {
	best := 0.0
	for _, sv := range sampleVecs {
		if s := dotSim(seg, sv); s > best {
			best = s
		}
	}
	return best
}

// dotSim 两个 L2 归一向量的内积（= 余弦）。声纹向量由 sidecar 归一化，与 api.cosine 同实现。
func dotSim(a, b []float32) float64 {
	var s float64
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// loadSpeakerSampleVecs 取说话人的多条样本向量（详情页同口径）；无样本行 / 未装配 repo
// 回退聚合代表（speaker.embedding）。
func loadSpeakerSampleVecs(ctx context.Context, d StageDeps, spID ids.ID) [][]float32 {
	var vecs [][]float32
	if d.SpeakerEmbeddings != nil {
		if es, err := d.SpeakerEmbeddings.ListBySpeaker(ctx, spID); err == nil {
			for _, e := range es {
				if v, ok := decodeEmbedding(e.Embedding); ok && len(v) == 256 {
					vecs = append(vecs, v)
				}
			}
		}
	}
	if len(vecs) == 0 && d.Speakers != nil {
		if sp, err := d.Speakers.Get(ctx, spID); err == nil {
			if v, ok := decodeEmbedding(sp.Embedding); ok && len(v) == 256 {
				vecs = append(vecs, v)
			}
		}
	}
	return vecs
}

// decodeEmbedding []byte(256×float32 LE) → []float32（与 float32Blob 互逆）。
func decodeEmbedding(blob []byte) ([]float32, bool) {
	if len(blob) == 0 || len(blob)%4 != 0 {
		return nil, false
	}
	v := make([]float32, len(blob)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return v, true
}

// segVec 一个 ASR 段与其声纹向量（干净段挑选的输入单元）。
type segVec struct {
	seg repo.TranscriptSegment
	vec []float32
}

// groupRep 一个 ASR 说话人标签组的检索/登记/纠正所需信息（runSpeakerStage 内构建）。
type groupRep struct {
	label string
	rep   []float32 // 组代表声纹（全部段向量均值）——1:N 检索用
	vecN  int       // 该组有效向量数（用于 sample_count）
	// clean 登记优先向量：组内「时长最长、与其他说话人段无时间交集且 ≥3s」的单段向量。
	// nil=无干净段（登记时退回 rep）。只影响**新声纹登记**，不影响上面 rep 的检索。
	clean []float32
	// segVecs 组内各段与其向量（纠正 pass 用：逐段对各在场说话人打分）。
	segVecs []segVec
	// durMS 组内 segVecs 段时长之和（ms）——过短并入判定：<minCleanSegMS 视为过短噪声组。
	durMS int64
}

// pickCleanSegVec 从组内段向量中挑「干净段」向量：时长 ≥3s 且与本 session **其他标签**的
// 段无时间交集（时间交集=音频上混有他人语音，diarization 切错的典型痕迹），取其中时长
// 最长的一段。无满足条件的段返回 nil（调用方退回全组聚合）。
// all 传本 session 全部段（含其他标签），用于交集判定。
func pickCleanSegVec(svs []segVec, all []repo.TranscriptSegment, label string) []float32 {
	var best []float32
	var bestDur int64
	for _, sv := range svs {
		dur := sv.seg.EndMS - sv.seg.StartMS
		if dur < minCleanSegMS {
			continue
		}
		if overlapsOtherLabel(sv.seg, all, label) {
			continue
		}
		if dur > bestDur {
			best, bestDur = sv.vec, dur
		}
	}
	return best
}

// overlapsOtherLabel 判断段是否与「其他 speaker_label」的任何段在时间上相交
// （半开区间 [start,end) 判交：s1.start < s2.end && s2.start < s1.end）。
// 空 label 的段也算「其他」——单人录音通常全空标签，此时组内即全体段、天然无交集判定对象。
func overlapsOtherLabel(seg repo.TranscriptSegment, all []repo.TranscriptSegment, label string) bool {
	for _, o := range all {
		if o.SpeakerLabel == label {
			continue
		}
		if seg.StartMS < o.EndMS && o.StartMS < seg.EndMS {
			return true
		}
	}
	return false
}

// stageSpeaker 是 pool 用的 Handler 包装。
func stageSpeaker(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读 transcript: %w", err)
		}
		return runSpeakerStage(ctx, d, sessionID, tr)
	}
}

// sliceAudio 用 ffmpeg 从 transcoded wav 按毫秒切出片段。源是 16k mono s16 PCM；
// -ss/-to 定位起止（-to 是绝对时间，非相对），-c copy 不重编码、最快。
// 注：-c copy 在 WAV 上是包粒度，边界可能偏几十毫秒，对声纹聚合无影响；需样本级精度时改重编码。
func sliceAudio(src, dst string, startMS, endMS int64) error {
	args := []string{"-y",
		"-ss", fmt.Sprintf("%.3f", float64(startMS)/1000),
		"-to", fmt.Sprintf("%.3f", float64(endMS)/1000),
		"-i", src, "-c", "copy", dst}
	out, err := exec.Command("ffmpeg", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg slice: %w: %s", err, out)
	}
	return nil
}

// aggregateEmbeddings 多段向量取均值再 L2 归一，得代表声纹。
func aggregateEmbeddings(vecs [][]float32) []float32 {
	rep := make([]float32, 256)
	for _, v := range vecs {
		for i := 0; i < 256; i++ {
			rep[i] += v[i]
		}
	}
	n := float32(len(vecs))
	var sumSq float64
	for i := 0; i < 256; i++ {
		rep[i] /= n
		sumSq += float64(rep[i]) * float64(rep[i])
	}
	if sumSq > 0 {
		inv := float32(1.0 / math.Sqrt(sumSq))
		for i := 0; i < 256; i++ {
			rep[i] *= inv
		}
	}
	return rep
}

// float32Blob 256×float32 → []byte（Little-Endian），存 speaker.embedding 灾备 BLOB。
func float32Blob(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}

// rand5 生成 5 位 [a-z0-9] 随机串，用于自动登记的默认名 说话人{xxxxx}。
func rand5() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000" // 极端兜底，保证 name 非空
	}
	out := make([]byte, 5)
	for i := range b {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(out)
}
