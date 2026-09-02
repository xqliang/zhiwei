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
	"sort"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// runSpeakerStage 是 speaker stage 的可测核心（避开 pool），由 stageSpeaker 包装成 Handler。
//
// 流程：按 ASR speaker_label 分组 → 逐段切片提向 → 组代表声纹 →
// 每组（= 一个 ASR 说话人）做跨 session 1:N（先对全部组检索、再统一登记未命中的，见步骤 2 注释）
// → 回填组内段 speaker_id（仅填 NULL，保留手动纠正）→ 纠正 pass（幽灵历史声纹/碎片在场/过短并入/逐段改判）。
//
// ASR 原生 diarization 的标签**大体**可信，但过度切分（同一人被拆成第二标签的短碎片）是
// 主要失败模式：碎片组要么误命中库里嗓音最像的他人、要么 gap 差一口气未命中而登记成新声纹
// （库污染的自我强化源头）。2026-08-31「碎片在场优先」：同场内与本场更主要说话人声纹够像
// （≥ InSessionMin 0.72，实测同人 0.76+、不同人 ≤0.67）的组并入该说话人，不再各标签各登记；
// 跨 session 归并仍只认 1:N 检索。检索与登记向量都优先取「干净段」（见 pickCleanSegVec
// 的三级优先）：时长最长、与其他说话人段无**显著**时间交集（亚秒嵌套碎片有容差，见
// overlapsOtherLabel）且 ≥3s 的单段；无原生干净段时尝试**修剪**（剪掉显著交集后剩余
// ≥3s 则重新切片提向）。聚合向量会被 diarization 切错/混入
// 他人语音的段污染（2026-09-01 实测：碎段把对既有声纹的领先压缩到宽松命中门槛以下，
// 整组被误登记成新声纹）；修剪/聚合都不可用才退回全组聚合（2026-08-26 需求起登记如此、
// 2026-09-01 起检索同基准）。
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
			clean:   pickCleanSegVec(ctx, d, wavPath, sliceDir, svs, segs, label),
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
		// 检索基准与登记同源：优先「干净段」向量，无干净段才退回全组聚合。
		// 聚合会被短碎段污染（实测 2026-09-01 session 2094724818275405824：主力段对既有
		// 声纹领先 0.19~0.31，被 0~1s 碎段拽平到 0.0676 < LooseGap 0.1，整组被误登记成
		// 新声纹「说话人prbiv」）；干净段无此污染，见 pickCleanSegVec。
		searchVec := g.rep
		if g.clean != nil {
			searchVec = g.clean
		}
		res, err := d.Voiceprint.Search(ctx, searchVec)
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
			continue
		}
		// 段级多数投票兜底：干净段单向量可能恰好选中最不具区分性的那段（实测
		// 2026-09-02 session 2095037034107244544：8s 干净段 top1 领先仅 0.014 未命中、
		// 误登记「说话人ffwvo」，而 ≥1s 段里 6/8 段 top1=清亮、3 段段级即满足命中）。
		// 保守门槛见 segmentVoteMatch。
		if vid, ok := segmentVoteMatch(ctx, d, g, threshold); ok {
			if sp, gerr := d.Speakers.Get(ctx, vid); gerr == nil && sp != nil {
				matched[i], matchedID[i] = true, vid
			}
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
	// 过短噪声组预标记（与处理顺序无关，先行判定）：不建 speaker/不入 FAISS，pass3 并入最近在场说话人。
	deferred := make([]bool, len(reps))
	for i, g := range reps {
		if !matched[i] && hasTarget && g.durMS < minCleanSegMS {
			deferred[i] = true
		}
	}
	fragmentMS := d.VoiceprintFragmentMS
	if fragmentMS <= 0 {
		fragmentMS = defaultFragmentMS
	}
	inSessionMin := d.VoiceprintInSessionMin
	if inSessionMin <= 0 {
		inSessionMin = defaultInSessionMin
	}

	// 第二趟分两步走（2026-08-31「碎片在场优先」需求）：
	// 2a) 命中组先解析——在场锚点说话人（库内确认身份）先就位；
	// 2b) 未命中非 deferred 组再处理，按 durMS **降序**（主要说话人先登记，碎片才有锚点可并）。
	// 2b 里登记新声纹前先做在场锚点守门（见 mergeIntoInSessionAnchor）：碎片组的 rep 检索
	// 常因 gap 差一口气（实测 0.059 vs GapMin 0.06）而未命中，若其声纹与本场已确认说话人
	// 足够像（segMax ≥ InSessionMin），并入该说话人而不是登记新声纹——否则库里会不断
	// 积累「同人碎片声纹」（铉晔/未知同事/说话人ghqhg 均为此污染），且越积越难命中。
	resolvedID := make([]ids.ID, len(reps)) // 每组最终 speaker（deferred 组留零值，不作目标）
	for i, g := range reps {
		if deferred[i] || !matched[i] {
			continue
		}
		resolvedID[i] = matchedID[i]
		if err := d.Transcripts.SetSegmentSpeaker(ctx, tr.ID, g.label, resolvedID[i]); err != nil {
			return fmt.Errorf("回填 speaker_id: %w", err)
		}
	}
	// 2b 处理顺序：未命中非 deferred 组按总时长降序（下标排序，resolvedID 仍按原下标写）。
	unmatched := make([]int, 0, len(reps))
	for i := range reps {
		if !matched[i] && !deferred[i] {
			unmatched = append(unmatched, i)
		}
	}
	sort.Slice(unmatched, func(a, b int) bool { return reps[unmatched[a]].durMS > reps[unmatched[b]].durMS })
	for _, i := range unmatched {
		g := reps[i]
		// 在场锚点守门：与本场已解析说话人（锚点须 durMS 更大=更主要）声纹足够像 → 并入，
		// 不登记新声纹（修「思敏碎片登记成 说话人ghqhg」类 case）。
		if anchorID, anchorSim := bestInSessionAnchor(ctx, d, reps, resolvedID, i); anchorID != 0 && anchorSim >= inSessionMin {
			log.Printf("[speaker] 未命中碎片并入在场说话人 label=%s speaker=%s sim=%.4f（不登记新声纹）", g.label, anchorID, anchorSim)
			resolvedID[i] = anchorID
			if err := d.Transcripts.SetSegmentSpeaker(ctx, tr.ID, g.label, anchorID); err != nil {
				return fmt.Errorf("回填 speaker_id: %w", err)
			}
			continue
		}
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
		resolvedID[i] = sp.ID
		if err := d.Transcripts.SetSegmentSpeaker(ctx, tr.ID, g.label, resolvedID[i]); err != nil {
			return fmt.Errorf("回填 speaker_id: %w", err)
		}
	}

	// 3) 纠正 pass：先幽灵历史声纹纠正（含碎片在场改判），再过短段并入。两者共享「各在场说话人样本向量」。
	samples := buildGroupSamples(ctx, d, reps, matched, resolvedID)
	margin := d.VoiceprintCorrectMargin
	if margin == 0 {
		margin = defaultCorrectMargin
	}
	if err := correctPhantomHistoricalMatches(ctx, d, tr, reps, matched, deferred, resolvedID, samples, margin, fragmentMS, inSessionMin); err != nil {
		return err
	}
	if err := mergeShortGroups(ctx, d, tr, reps, deferred, resolvedID, samples); err != nil {
		return err
	}
	// 4) 逐段声纹改判：某段声纹明显更像另一在场说话人时改判该段（见 correctSegmentsByVoiceprint）。
	if err := correctSegmentsByVoiceprint(ctx, d, tr); err != nil {
		return err
	}
	return nil
}

// minCleanSegMS 干净段（登记声纹优先来源）的最短时长：3s——太短的段声纹特征不稳。
const minCleanSegMS = 3000

// voteSegMinMS 段级投票的最短段时长：1s——亚秒碎段声纹不稳，投票噪声大
// （实测案例里 0s 段 top1 随机漂到第三名）。
const voteSegMinMS = 1000

// cleanOverlapTolMS / cleanOverlapTolFrac 已上移 internal/voiceprint（OverlapTolMS /
// OverlapTolFrac）：手动录入（api EnrollFromSegment）与自动登记共用同一容差口径，
// 单一事实源见该包 segtrim.go 注释（含 2026-09-02 session 2095044361807990784
// 「幽灵质心」完整根因）。

// segmentVoteMatch 段级多数投票兜底：单向量基准（干净段/聚合）未命中时，让 ≥1s 的
// 段各自对历史库取 top1 说话人计票。**保守门槛**（防在相近声纹间瞎猜）：
//   - 得票严格过半（>50%）且 ≥2 票；
//   - 至少一段对该说话人**段级满足 voiceprint.Matched**（强证据：不只票多，
//     还要有段落能独立通过两级命中规则）。
//
// 全库在首趟检索阶段尚未加入本 run 新声纹（两趟设计），各段票面对的是同一历史库。
// 复刻 case：8s 干净段近平手（gap 0.014）未命中误登记新声纹，而 6/8 段 top1=清亮、
// 3 段段级即命中。返回 (speakerID, true) 表示投票归属；未决返回 (0, false)。
func segmentVoteMatch(ctx context.Context, d StageDeps, g groupRep, threshold float64) (ids.ID, bool) {
	// 预检短路：≥1s 段不足 2 个时投票不可能过「≥2 票」门槛，直接返回，省无效检索
	//（单人短组/碎片组常只有 1 个合格段，逐段检索纯浪费 sidecar 调用）。
	eligible := 0
	for _, sv := range g.segVecs {
		if sv.seg.EndMS-sv.seg.StartMS >= voteSegMinMS {
			eligible++
		}
	}
	if eligible < 2 {
		return 0, false
	}
	votes := map[ids.ID]int{}
	strong := map[ids.ID]bool{}
	total := 0
	for _, sv := range g.segVecs {
		if sv.seg.EndMS-sv.seg.StartMS < voteSegMinMS {
			continue
		}
		res, err := d.Voiceprint.Search(ctx, sv.vec)
		if err != nil || !res.Matched || res.SpeakerID == 0 {
			continue // 检索失败/空库：该段弃权
		}
		total++
		votes[res.SpeakerID]++
		if voiceprint.Matched(res.Distance, res.SecondDistance, threshold) {
			strong[res.SpeakerID] = true
		}
	}
	if total < 2 {
		return 0, false
	}
	var bestID ids.ID
	bestN := 0
	for sid, n := range votes {
		if n > bestN {
			bestID, bestN = sid, n
		}
	}
	if bestN >= 2 && bestN*2 > total && strong[bestID] {
		return bestID, true
	}
	return 0, false
}

// defaultFragmentMS 「碎片组」判定默认阈值（10s）：组内段总时长小于此值视为 ASR diarization
// 碎片（同一人被切成第二标签的短句，或噪声句）。实测两个误判 case 的碎片组分别为 3.9s/6.3s，
// 而同场真身组 ≥11s——10s 居中留裕。过大会把真人的简短发言也当碎片并入他人，宁小勿大。
const defaultFragmentMS = 10000

// defaultInSessionMin 碎片在场归并的默认最低相似度：与 voiceprint.SoftMin 同值（0.72）——
// 「分数略低于强命中但明确是同一人」的既有语义复用到同场判定。实测依据：同场同人组间
// segMax 0.76~0.83，同场不同人 ≤0.67，0.72 两侧均有余量。
const defaultInSessionMin = voiceprint.SoftMin

// defaultCorrectMargin 幽灵历史声纹纠正的默认领先幅度门槛（沿用 voiceprint.GapMin 经验值）。
// max 相似度口径下，真人在幽灵段上需比历史人自身 max 领先该幅度才改判，挡住接近平局的噪声翻转。
const defaultCorrectMargin = 0.06

// correctScoreEps 判定容差：声纹向量是 float32（相对精度 ~1e-7），逐维内积在「恰好等于
// self+margin」的边界上会被 float32 表示误差顶过阈值（如 float32(0.79)=0.790000021）。
// 加此容差保证「严格大于才纠正」的语义对边界稳健——领先幅度需真正超过 margin，float32
// 噪声级别的微弱超出不触发翻转。远大于噪声的正常改判（领先 ≥0.09）不受影响。
const correctScoreEps = 1e-6

// segReattributeMinSim 逐段声纹改判（pass4）的绝对相似度下限：0.6——比 1:N 弱命中的
// voiceprint.SoftMin(0.72) 更低（2026-08-28 需求）。pass4 只在「另一在场说话人比当前归属明显
// 领先（> GapMin）」时才改判，故这里的绝对下限可放宽到 0.6：段级归属靠「相对领先」把关，
// 下限只用于挡掉两边都很低（都不像）的噪声段。不复用 SoftMin：SoftMin 是 1:N 登记/复用阈值，
// 改它会影响声纹匹配与 phantom 判定，故 pass4 用独立常量。
const segReattributeMinSim = 0.6

// correctPhantomHistoricalMatches 幽灵历史声纹纠正（2026-08-27 需求）+ 碎片在场改判（2026-08-31 需求）：
//
// 幽灵纠正：ASR 过度切分出的幽灵组常命中历史库某真人；若该组名下的段被同录音另一在场说话人
// 匹配得更好（max 相似度口径，与详情页 topVoiceMatchesVec 同口径），判为幽灵、整组改判
// 给那个人，段写 corrected_from。仅**历史命中组**（matched[i]）参与——新登记组的声纹是
// 从自己段建出来的、天生在自己段上最高，不可能被判幽灵。
//
// 碎片在场改判：durMS < fragmentMS 的**命中**碎片组，其库内命中本身不可信（紧声纹 cohort 里
// 碎片向量常命中嗓音最像的库内他人而非真身，如「铉晔组实为杰辉」case：vs 铉晔 0.7354、
// vs 在场杰辉 0.7395）——若某在场锚点（durMS 更大=更主要的说话人）的 segMax 不低于归属
// 自己的得分且 ≥ inSessionMin，整组改判给锚点。锚点须 durMS 更大：主要说话人吸收碎片，
// 反向（长组并入碎片）无意义。
//
// 两条规则先算全部判定（基于本趟归属快照）、再统一应用，避免链式/互换改判抖动。
func correctPhantomHistoricalMatches(ctx context.Context, d StageDeps, tr *repo.Transcript,
	reps []groupRep, matched []bool, deferred []bool, resolvedID []ids.ID, samples [][][]float32,
	margin float64, fragmentMS int64, inSessionMin float64) error {
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
		isFragment := g.durMS < fragmentMS
		// 找在场其他说话人里，在本组段上得分最高者（碎片候选的锚点额外要求 durMS 更大）
		bestScore, bestJ := -1.0, -1
		for j := range reps {
			if j == i || deferred[j] || resolvedID[j] == resolvedID[i] {
				continue // 跳过自己、过短缓起组(无有效 speaker)、解析到同一 speaker 的组
			}
			if isFragment && reps[j].durMS <= g.durMS {
				continue // 碎片改判的锚点必须是更主要（总时长更长）的说话人
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
		fire := false
		if bestJ >= 0 {
			if isFragment {
				// 碎片：锚点不比归属差（容差同 correctScoreEps，平局=无显著区别→在场优先）
				// 且达到在场归并最低相似度，即改判。
				fire = bestScore >= inSessionMin && bestScore >= self-correctScoreEps
			} else {
				// 幽灵：须明显更好（> self + margin），规则不变。
				fire = bestScore > self+margin+correctScoreEps
			}
		}
		if fire {
			fixes = append(fixes, fix{label: g.label, from: resolvedID[i], to: resolvedID[bestJ]})
		}
	}
	for _, f := range fixes {
		if err := d.Transcripts.CorrectSegmentSpeaker(ctx, tr.ID, f.label, f.from, f.to); err != nil {
			// best-effort：纠正失败仅 log 不致命。此时段已回填到（幽灵）说话人、并非无归属；
			// 若返回错误让 job 重试，重试时段已 assigned → reps 为空 → 纠正永不重跑，反而是「既失败又丢纠正」
			// 的最坏情况。与本 stage 样本行落库失败(SpeakerEmbeddings.Create)的 best-effort+log 处理一致。
			log.Printf("[speaker] 幽灵历史声纹/碎片在场纠正失败 label=%s from=%s to=%s: %v", f.label, f.from, f.to, err)
			continue
		}
		// 改判生效后同步内存 resolvedID：后续 mergeShortGroups 等 pass 以最新归属为准
		//（否则过短组会并入已被搬空的旧说话人）。
		for i, g := range reps {
			if g.label == f.label {
				resolvedID[i] = f.to
				break
			}
		}
	}
	return nil
}

// bestInSessionAnchor 找未命中组（2b）的「在场锚点」：已解析组中 durMS 大于本组者，取与本组
// 段向量 segMax 相似度最高的锚点说话人。返回 (speakerID, 相似度)；无锚点返回 (0, 0)。
// 锚点样本与 phantom/详情页同口径（loadSpeakerSampleVecs：多条样本回退聚合代表）。
func bestInSessionAnchor(ctx context.Context, d StageDeps, reps []groupRep, resolvedID []ids.ID, i int) (ids.ID, float64) {
	bestID, bestSim := ids.ID(0), 0.0
	for j := range reps {
		if j == i || resolvedID[j] == 0 || reps[j].durMS <= reps[i].durMS {
			continue // 锚点须已解析且比本组更主要
		}
		sv := loadSpeakerSampleVecs(ctx, d, resolvedID[j])
		if len(sv) == 0 {
			continue
		}
		sc := 0.0
		for _, seg := range reps[i].segVecs {
			if s := segMaxScore(seg.vec, sv); s > sc {
				sc = s
			}
		}
		if sc > bestSim {
			bestSim, bestID = sc, resolvedID[j]
		}
	}
	return bestID, bestSim
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

// correctSegmentsByVoiceprint 逐段声纹改判（2026-08-28 需求）：ASR 分组内某段的段级声纹若明显更像
// 另一个**在场说话人**（相似度 ≥ segReattributeMinSim(0.6) 且比当前归属领先 > voiceprint.GapMin），
// 把该单段改判给那个人（corrected_reason='mismatch'）。自包含：重列段拿最终归属+
// 逐段向量，不依赖 pass1-3 的内存模型（归属已被 phantom/short 改动过）。候选仅在场说话人（本录音各段
// 非空 speaker_id 去重）——不改判给历史库里不在场的人。先算完全部再统一应用（避免本趟内相互影响判据）。
//
// 领先幅度判据用 `> GapMin+correctScoreEps`（严格大于 + float32 容差），与 pass3
// correctPhantomHistoricalMatches 同纪律：本函数逐维内积同样是 float32（相对精度 ~1e-7），在「领先恰好
// = GapMin」的边界上会被表示误差顶过阈值（实测某构造用例领先算得 0.0600000162 而非 0.06）。voiceprint.Matched
// 的弱命中用 `>= GapMin` 是因其比较 sidecar 直接给出的 top1/top2 距离、无本地重算误差；此处重算内积，须与
// pass3 一样加容差，否则同一条领先量在 pass3(不改判)/pass4(改判) 间出现边界不一致。
func correctSegmentsByVoiceprint(ctx context.Context, d StageDeps, tr *repo.Transcript) error {
	segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return fmt.Errorf("逐段改判读 segments: %w", err)
	}
	samples := map[ids.ID][][]float32{}
	for _, s := range segs {
		if s.SpeakerID != nil {
			if _, ok := samples[*s.SpeakerID]; !ok {
				samples[*s.SpeakerID] = loadSpeakerSampleVecs(ctx, d, *s.SpeakerID)
			}
		}
	}
	if len(samples) < 2 {
		return nil // 少于两个在场说话人，无可比对象
	}
	type fix struct {
		segID, from, to ids.ID
	}
	var fixes []fix
	for _, s := range segs {
		if s.SpeakerID == nil || len(s.Embedding) == 0 {
			continue
		}
		vec, ok := decodeEmbedding(s.Embedding)
		if !ok || len(vec) != 256 {
			continue
		}
		assigned := *s.SpeakerID
		cur := segMaxScore(vec, samples[assigned])
		bestOther, bestID, hasBest := 0.0, ids.ID(0), false
		for spID, sv := range samples {
			if spID == assigned {
				continue
			}
			if sc := segMaxScore(vec, sv); !hasBest || sc > bestOther {
				bestOther, bestID, hasBest = sc, spID, true
			}
		}
		if hasBest && bestOther >= segReattributeMinSim && bestOther-cur > voiceprint.GapMin+correctScoreEps {
			fixes = append(fixes, fix{segID: s.ID, from: assigned, to: bestID})
		}
	}
	for _, f := range fixes {
		if err := d.Transcripts.ReattributeSegmentByVoiceprint(ctx, tr.ID, f.segID, f.from, f.to); err != nil {
			log.Printf("[speaker] 逐段声纹改判失败 seg=%s from=%s to=%s: %v", f.segID, f.from, f.to, err)
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
	rep   []float32 // 组代表声纹（全部段向量均值）——无干净段时的检索/登记兜底基准
	vecN  int       // 该组有效向量数（用于 sample_count）
	// clean 登记与检索的首选向量：pickCleanSegVec 三级优先的结果——原生干净段
	//（无显著交集的最长 ≥3s 段）或修剪段（剪掉显著交集后 ≥3s 的剩余块，重新提向）。
	// nil=两者皆无（检索/登记退回 rep）。聚合向量会被 diarization 切错/混入他人语音的段
	// 污染（2026-09-01 实测：碎段把对真身的领先压缩到宽松命中门槛以下，整组误登记成新声纹），
	// 故 1:N 检索（步骤 2 首趟）与登记（步骤 2b）同基准、都优先干净段。
	clean []float32
	// segVecs 组内各段与其向量（纠正 pass 用：逐段对各在场说话人打分）。
	segVecs []segVec
	// durMS 组内 segVecs 段时长之和（ms）——过短并入判定：<minCleanSegMS 视为过短噪声组。
	durMS int64
}

// pickCleanSegVec 从组内段向量中挑「干净段」向量（检索/登记/纠正样本共用的基准）。
// 三级优先（多候选按时长取最长）：
//  1. 原生干净段：≥3s 且与其他标签段无显著时间交集（亚秒嵌套碎片有容差，见
//     overlapsOtherLabel）→ 直接用已提好的段向量，零额外开销；
//  2. 修剪段：有显著交集、但剪掉与其他标签段的交集并集后最长剩余块仍 ≥3s →
//     对该块重新切片+提向（sidecar 调用）。修的是容差覆盖不到的场景——长主力段
//     中间嵌着超过容差的真实插话（如 30s 段中 5s 他人语音）：整段向量被稀释、
//     又没有别的干净段可换时，与其退回混音聚合（幽灵质心源头），不如剪掉混音
//     部分用剩余纯音频。切片/提向失败静默跳过（与段提向失败的降级一致，退回 3）；
//  3. 无 → nil（调用方退回全组聚合）。
//
// 注意：只要存在原生干净段就用它（不再与更长的修剪候选比较）——原生段零成本且
// 纯度有保证；修剪候选只在「组内没有任何原生干净段」时兜底。
func pickCleanSegVec(ctx context.Context, d StageDeps, wavPath, sliceDir string, svs []segVec, all []repo.TranscriptSegment, label string) []float32 {
	// 1) 原生干净段
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
	if best != nil {
		return best
	}
	// 2) 修剪段：剪掉显著交集后最长剩余块 ≥3s 者，重新切片+提向
	for _, sv := range svs {
		if sv.seg.EndMS-sv.seg.StartMS < minCleanSegMS {
			continue
		}
		chunk := voiceprint.LongestTrimmedChunk(sv.seg, all, label)
		chunkDur := chunk[1] - chunk[0]
		if chunkDur < minCleanSegMS || chunkDur <= bestDur {
			continue // 剩余太短，或不比已选修剪候选长（时长比较在切片前，省 sidecar 调用）
		}
		trimPath := filepath.Join(sliceDir, fmt.Sprintf("trim-%d.wav", sv.seg.SequenceNo))
		if err := sliceAudio(wavPath, trimPath, chunk[0], chunk[1]); err != nil {
			continue
		}
		v, err := d.Voiceprint.Embed(ctx, trimPath)
		if err != nil || len(v) != 256 {
			continue
		}
		best, bestDur = v, chunkDur
	}
	return best
}

// overlapsOtherLabel 判断段是否与「其他 speaker_label」的段在时间上**显著**相交
// （半开区间 [start,end) 判交）。交集时长低于容差（亚秒嵌套碎片是 diarization 噪声，
// 不代表候选段音频真的混入了他人语音；容差与判定见 voiceprint.SignificantOverlap，
// 手动录入共用同口径）的其他标签段被忽略。
// 空 label 的段也算「其他」——单人录音通常全空标签，此时组内即全体段、天然无交集判定对象。
func overlapsOtherLabel(seg repo.TranscriptSegment, all []repo.TranscriptSegment, label string) bool {
	for _, o := range all {
		if o.SpeakerLabel == label {
			continue
		}
		if _, _, sig := voiceprint.SignificantOverlap(seg, o); sig {
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
