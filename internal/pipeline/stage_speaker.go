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
// 每组（= 一个 ASR 说话人）做跨 session 1:N 检索/登记 → 回填组内段 speaker_id（仅填 NULL，保留手动纠正）。
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
	type groupRep struct {
		label string
		rep   []float32 // 组代表声纹（全部段向量均值）——1:N 检索用
		vecN  int       // 该组有效向量数（用于 sample_count）
		// clean 登记优先向量：组内「时长最长、与其他说话人段无时间交集且 ≥3s」的单段向量。
		// nil=无干净段（登记时退回 rep）。只影响**新声纹登记**，不影响上面 rep 的检索。
		clean []float32
	}
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
		reps = append(reps, groupRep{
			label: label, rep: aggregateEmbeddings(vecs), vecN: len(vecs),
			clean: pickCleanSegVec(svs, segs, label),
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

	// 2) 每组（= 一个 ASR 说话人）独立做跨 session 1:N 检索/登记 → 回填该组未解析段。
	// 信任 ASR diarization：不同 ASR 标签一律视为不同说话人，不在本地按声纹相似度合并。
	for _, g := range reps {
		res, err := d.Voiceprint.Search(ctx, g.rep)
		if err != nil {
			return fmt.Errorf("voiceprint search: %w", err)
		}
		// 同一人判定（两级规则，见 voiceprint.Matched）：强命中 sim≥阈值；
		// 或区分性弱命中 sim≥0.72 且明显领先第二名（top1−top2≥0.06）——
		// 分数略低于阈值但明显是同一个人的也复用，减少真匹配被误登记成新声纹。
		matched := res.Matched && voiceprint.Matched(res.Distance, res.SecondDistance, threshold)
		var speakerID ids.ID
		if matched {
			speakerID = res.SpeakerID
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
		if err := d.Transcripts.SetSegmentSpeaker(ctx, tr.ID, g.label, speakerID); err != nil {
			return fmt.Errorf("回填 speaker_id: %w", err)
		}
	}
	return nil
}

// minCleanSegMS 干净段（登记声纹优先来源）的最短时长：3s——太短的段声纹特征不稳。
const minCleanSegMS = 3000

// segVec 一个 ASR 段与其声纹向量（干净段挑选的输入单元）。
type segVec struct {
	seg repo.TranscriptSegment
	vec []float32
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
//（半开区间 [start,end) 判交：s1.start < s2.end && s2.start < s1.end）。
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
