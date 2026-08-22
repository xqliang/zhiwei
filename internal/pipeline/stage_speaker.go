// stage_speaker 实现 speaker stage：切片→提向→聚合→1:N 检索/登记→回填 segment.speaker_id。
// 解析粒度：按 ASR 说话人标签分组（ASR 原生 diarization 已完成 session 内聚类），
// 每组聚一个代表声纹做跨 session 1:N，避免同一段新说话人被重复登记。
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// runSpeakerStage 是 speaker stage 的可测核心（避开 pool），由 stageSpeaker 包装成 Handler。
//
// 流程：按 ASR speaker_label 分组 → 逐段切片提向 → 组代表声纹 →
// 【session 内聚类】把声纹相近（同人）的不同 ASR 标签合并成一个说话人
// （realtime prompt 式 diarization 可能把一人标成 spk0/spk1，用 WeSpeaker 兜底合并）→
// 每个聚类做跨 session 1:N 检索/登记 → 回填组内段 speaker_id（仅填 NULL，保留手动纠正）。
// 注：聚类需真实 WeSpeaker 才生效——StubEmbedder 的随机向量不会聚拢。
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
		threshold = 0.5
	}

	// 1) 逐组切片+提向，跳过已全部解析的组（幂等：reextract 不重复调 sidecar、不覆盖手动纠正）。
	type groupRep struct {
		label string
		rep   []float32
		vecN  int // 该组有效向量数（用于 sample_count）
	}
	var reps []groupRep
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
		vecs := make([][]float32, 0, len(members))
		for _, s := range members {
			slicePath := filepath.Join(sliceDir, fmt.Sprintf("seg-%d.wav", s.SequenceNo))
			if err := sliceAudio(wavPath, slicePath, s.StartMS, s.EndMS); err != nil {
				continue // 切片失败跳过该段（speaker_id 留 NULL）
			}
			v, err := d.Voiceprint.Embed(ctx, slicePath)
			if err != nil || len(v) != 256 {
				continue // 提向失败跳过
			}
			vecs = append(vecs, v)
		}
		if len(vecs) == 0 {
			continue
		}
		reps = append(reps, groupRep{label: label, rep: aggregateEmbeddings(vecs), vecN: len(vecs)})
	}
	if len(reps) == 0 {
		return nil
	}

	// 2) session 内聚类：声纹相近（cosine≥阈值）的不同 ASR 标签合并为同人。
	// 贪心合并（每 session 说话人数极少，O(n³) 可接受）。
	clusterOf := make([]int, len(reps))
	for i := range clusterOf {
		clusterOf[i] = i
	}
	for i := 0; i < len(reps); i++ {
		for j := i + 1; j < len(reps); j++ {
			if clusterOf[i] == clusterOf[j] {
				continue
			}
			if cosineVec(reps[i].rep, reps[j].rep) >= threshold {
				old := clusterOf[j]
				for k := range clusterOf {
					if clusterOf[k] == old {
						clusterOf[k] = clusterOf[i]
					}
				}
			}
		}
	}

	// 3) 每个聚类：聚代表声纹 → 跨 session 1:N 检索/登记 → 回填组内未解析段。
	seen := make(map[int]bool)
	for i := range reps {
		if seen[clusterOf[i]] {
			continue
		}
		seen[clusterOf[i]] = true
		var memberReps [][]float32
		var memberVecN int
		var labels []string
		for j := range reps {
			if clusterOf[j] == clusterOf[i] {
				memberReps = append(memberReps, reps[j].rep)
				memberVecN += reps[j].vecN
				labels = append(labels, reps[j].label)
			}
		}
		rep := aggregateEmbeddings(memberReps) // 同人代表 = 各组代表再聚合
		matchID, dist, matched, err := d.Voiceprint.Search(ctx, rep)
		if err != nil {
			return fmt.Errorf("voiceprint search: %w", err)
		}
		matched = matched && dist >= threshold
		var speakerID ids.ID
		if matched {
			speakerID = matchID
		} else {
			// 自动登记：name=说话人{5位随机串}，向量 BLOB 灾备
			sp := &repo.Speaker{Name: "说话人" + rand5(), Source: "auto", Embedding: float32Blob(rep), SampleCount: memberVecN}
			if err := d.Speakers.Create(ctx, sp); err != nil {
				return fmt.Errorf("登记 speaker: %w", err)
			}
			if err := d.Voiceprint.Add(ctx, rep, sp.ID); err != nil {
				return fmt.Errorf("voiceprint add: %w", err)
			}
			speakerID = sp.ID
		}
		for _, label := range labels {
			if err := d.Transcripts.SetSegmentSpeaker(ctx, tr.ID, label, speakerID); err != nil {
				return fmt.Errorf("回填 speaker_id: %w", err)
			}
		}
	}
	return nil
}

// cosineVec 两个 L2 归一化向量的余弦相似度（= 内积）。用于 session 内聚类合并同人。
func cosineVec(a, b []float32) float64 {
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
