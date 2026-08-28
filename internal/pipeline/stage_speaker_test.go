package pipeline

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
	"zhiwei/internal/voiceprint"
)

// fakeVoiceprint 实现 voiceprint.Client，可控 matched/matchID/相似度，记录 Add 调用 + 各方法调用计数。
type fakeVoiceprint struct {
	matched     bool
	matchID     ids.ID
	searchSim   float64 // Search 返回的 top-1 相似度；0 → 兜底 0.9（清晰命中，兼容既有用例）
	secondSim   float64 // Search 返回的 top-2 相似度（区分性弱命中规则用，默认 0=单声纹库）
	added       []ids.ID
	embedOK     bool
	sameVec     bool // true→各段返回相同向量；false→逐段正交（各段不同向量）
	embedCalls  int
	searchCalls int
	searchVecs  [][]float32 // 每次 Search 收到的代表向量（断言检索用聚合而非干净段用）
	addVecs     [][]float32 // 每次 Add 收到的向量（断言登记用干净段）
}

func (f *fakeVoiceprint) Embed(_ context.Context, _ string) ([]float32, error) {
	f.embedCalls++
	v := make([]float32, 256)
	if f.sameVec {
		// 常量向量：各段声纹完全相同（同人）——用于验证「即便声纹相同也不本地合并 ASR 标签」
		v[0] = 1.0
	} else {
		// 逐段正交 one-hot：不同段不同向量 → 各组彼此独立
		v[(f.embedCalls-1)%256] = 1.0
	}
	return v, nil
}
func (f *fakeVoiceprint) Search(_ context.Context, vec []float32) (voiceprint.SearchResult, error) {
	f.searchCalls++
	cp := append([]float32(nil), vec...)
	f.searchVecs = append(f.searchVecs, cp)
	sim := f.searchSim
	if sim == 0 {
		sim = 0.9 // 兜底：默认返回清晰命中相似度，兼容既有用例
	}
	return voiceprint.SearchResult{
		SpeakerID: f.matchID, Distance: sim, SecondDistance: f.secondSim, Matched: f.matched,
	}, nil
}
func (f *fakeVoiceprint) Add(_ context.Context, vec []float32, id ids.ID) error {
	f.addVecs = append(f.addVecs, append([]float32(nil), vec...))
	f.added = append(f.added, id)
	return nil
}
func (f *fakeVoiceprint) Remove(_ context.Context, _ ids.ID) error { return nil }
func (f *fakeVoiceprint) IDs(_ context.Context) ([]ids.ID, error)  { return nil, nil }

var _ voiceprint.Client = (*fakeVoiceprint)(nil) // 编译期接口符合性

// seedSpeakerStageSegs 建 session+transcript+指定段并复制切片源 wav；返回 (sid, tr, dataDir, transcripts, speakers)。
// 供需要自定义段时长的测试（如过短并入）复用。
func seedSpeakerStageSegs(t *testing.T, segs []repo.TranscriptSegment) (ids.ID, *repo.Transcript, string, *repo.TranscriptRepo, *repo.SpeakerRepo) {
	t.Helper()
	requireFFmpeg(t)
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}
	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "speech.wav",
		StoragePath: "../../testdata/speech.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	for i := range segs {
		segs[i].TranscriptID = tr.ID
	}
	if err := transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	transcodedDir := filepath.Join(dataDir, "transcoded")
	if err := os.MkdirAll(transcodedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open("../../testdata/speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(transcodedDir, sid.String()+".wav"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	return sid, tr, dataDir, transcripts, speakers
}

// seedSpeakerStage 默认三段（seq1,2=label"1" 共 3.5s；seq3=label"2" 3.1s——两组都 ≥3s，
// 不触发过短并入，保持既有多说话人测试语义）。
func seedSpeakerStage(t *testing.T) (ids.ID, *repo.Transcript, string, *repo.TranscriptRepo, *repo.SpeakerRepo) {
	return seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "明天发邮件", StartMS: 0, EndMS: 2000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "确认会议", StartMS: 2100, EndMS: 3600},
		{SequenceNo: 3, SpeakerLabel: "2", Text: "好的", StartMS: 3800, EndMS: 6900},
	})
}

func TestStageSpeakerEnrollsWhenNoMatch(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: &fakeVoiceprint{matched: false}, DataDir: dataDir}
	if err := runSpeakerStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	fv := d.Voiceprint.(*fakeVoiceprint)
	if len(fv.added) != 2 { // 两个标签组(1 和 2)，都未命中 → 登记 2 个
		t.Fatalf("应登记 2 个(每组一个)，实际 %d", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(context.Background(), tr.ID)
	assigned := map[ids.ID]bool{}
	for _, s := range segs {
		if s.SpeakerID == nil {
			t.Fatalf("段 %d 未回填 speaker_id", s.SequenceNo)
		}
		assigned[*s.SpeakerID] = true
	}
	if len(assigned) != 2 {
		t.Fatalf("应回填到 2 个不同 speaker，实际 %d", len(assigned))
	}
}

func TestStageSpeakerMatchesExisting(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	ctx := context.Background()
	// 预置一个已登记 speaker
	sp := &repo.Speaker{Name: "张三", Source: "enrolled"}
	if err := speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	// 该 speaker 全程 active 且不绑定任何 person，会残留在共享 zhiwei_test 库里。repo 包
	// （字典序在本包之后）TestPersonLifecycle 跑 EnsurePersonBootstrap 时，会把每个未绑定的
	// active speaker 物化成同名 active person——于是凭空多出一个 id 更小的「张三」person，令
	// 该用例的 FindByName(张三) 命中错误行。收尾删掉这个 speaker，堵住物化来源。
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), sp.ID) })
	// fake 全部命中该 speaker
	fv := &fakeVoiceprint{matched: true, matchID: sp.ID}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 0 { // 命中不应登记
		t.Fatalf("命中时不应登记，实际 %d", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SpeakerID == nil || *s.SpeakerID != sp.ID {
			t.Fatalf("段 %d 未回填到命中 speaker: %+v", s.SequenceNo, s.SpeakerID)
		}
	}
}

// TestStageSpeakerIdempotentSkip 验证幂等：段已全部解析到说话人后，重跑（如 reextract）
// 不再调 sidecar（Embed/Search/Add 计数为 0）、不覆盖既有 speaker_id。
// 对应 reextract 的 segment→speaker→extract 链路：speaker stage 对已处理 session 是 no-op。
func TestStageSpeakerIdempotentSkip(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	ctx := context.Background()
	// 先跑一遍（全部未命中→自动登记），让所有段都拿到 speaker_id
	first := &fakeVoiceprint{matched: false}
	d1 := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: first, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d1, sid, tr); err != nil {
		t.Fatalf("首次 stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SpeakerID == nil {
			t.Fatalf("首次后段 %d 仍无 speaker_id", s.SequenceNo)
		}
	}
	firstAssigned := map[ids.ID]bool{}
	for _, s := range segs {
		firstAssigned[*s.SpeakerID] = true
	}

	// 重跑：fake 若被调则计数；幂等应全部跳过
	second := &fakeVoiceprint{matched: true, matchID: ids.New()} // 即便配成"命中别的人"也不应被调
	d2 := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: second, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d2, sid, tr); err != nil {
		t.Fatalf("重跑 stage: %v", err)
	}
	if second.embedCalls != 0 || second.searchCalls != 0 || len(second.added) != 0 {
		t.Fatalf("重跑应 no-op，实际 embed=%d search=%d add=%d", second.embedCalls, second.searchCalls, len(second.added))
	}
	// speaker_id 不变
	segs2, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs2 {
		if s.SpeakerID == nil || !firstAssigned[*s.SpeakerID] {
			t.Fatalf("重跑后段 %d speaker_id 被改: %+v", s.SequenceNo, s.SpeakerID)
		}
	}
}

// TestStageSpeakerNoLocalMergeSameVoiceprint 验证：ASR 已足够准，stage 不再用声纹在本地
// 合并 ASR 返回的说话人。即便两个 ASR 标签声纹完全相同（fake 各段返回同一向量），只要 ASR
// 标成两个说话人（标签 1 和 2），就各自独立做跨 session 1:N（此处均未命中）→ 分别登记，
// 而不是像旧逻辑那样按声纹相似度本地聚成 1 个。
func TestStageSpeakerNoLocalMergeSameVoiceprint(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	fv := &fakeVoiceprint{matched: false, sameVec: true} // 各段同向量：即便同人也不应本地合并
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 2 { // 2 个 ASR 标签 → 各自独立登记，不再本地合并
		t.Fatalf("不应本地合并，应按 ASR 标签登记 2 个，实际 %d", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(context.Background(), tr.ID)
	speakerIDs := map[ids.ID]bool{}
	for _, s := range segs {
		if s.SpeakerID == nil {
			t.Fatalf("段 %d 未回填 speaker_id", s.SequenceNo)
		}
		speakerIDs[*s.SpeakerID] = true
	}
	if len(speakerIDs) != 2 { // 标签 1 的两段归一个人、标签 2 归另一个人 → 2 个 speaker
		t.Fatalf("应回填到 2 个不同 speaker，实际 %d", len(speakerIDs))
	}
}

// TestStageSpeakerEnrollsWhenSimilarityBelowThreshold 验证「同一人阈值 >= 0.8」：
// sidecar 报命中(matched=true)但相似度 0.7 < 0.8 → 视为不同人 → 登记新声纹、不复用 matchID。
func TestStageSpeakerEnrollsWhenSimilarityBelowThreshold(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	preset := ids.New()
	fv := &fakeVoiceprint{matched: true, matchID: preset, searchSim: 0.7} // 0.7 < 0.8
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) == 0 {
		t.Fatalf("相似度 0.7 < 0.8 应登记新声纹，实际未登记")
	}
	segs, _ := transcripts.ListSegments(context.Background(), tr.ID)
	for _, s := range segs {
		if s.SpeakerID != nil && *s.SpeakerID == preset {
			t.Fatalf("相似度 0.7 < 0.8 不应复用 matchID(%v)", preset)
		}
	}
}

// TestStageSpeakerWeakMatchByDistinctiveness 区分性弱命中（两级规则的弱命中支路，
// GapMin=0.06——2026-08-26 修正，初值 0.6 在余弦域几乎不可达、弱命中分支实际是死的）：
// sim 0.75（低于强阈值 0.8）且明显领先第二名（top1−top2 ≥ 0.06）→ 复用既有声纹；
// 同样 0.75 但第二名 0.72（领先不足，两个相近声纹的模糊匹配）→ 仍登记新声纹。
func TestStageSpeakerWeakMatchByDistinctiveness(t *testing.T) {
	ctx := context.Background()

	// 场景一：0.75 vs 0.1 → gap 0.65 ≥ 0.06 → 命中复用既有声纹
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	sp := &repo.Speaker{Name: "张三", Source: "enrolled"}
	if err := speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	// 收尾删掉这个 active 同名 speaker：repo 包 TestPersonLifecycle 经 EnsurePersonBootstrap
	// 会把未绑定的 active 同名 speaker 物化成 person（id 更小），残留会令其 FindByName(张三)
	// 命中错误行（跨包共享 zhiwei_test 库）。对齐本文件 TestStageSpeakerMatchesExisting 的 cleanup。
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), sp.ID) })
	fv := &fakeVoiceprint{matched: true, matchID: sp.ID, searchSim: 0.75, secondSim: 0.1}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 0 {
		t.Fatalf("区分性弱命中应复用既有声纹，实际登记了 %d 个新声纹", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SpeakerID == nil || *s.SpeakerID != sp.ID {
			t.Fatalf("段 %d 应归属既有声纹 %s，实际 %+v", s.SequenceNo, sp.ID, s.SpeakerID)
		}
	}

	// 场景二：0.75 vs 0.72 → gap 0.03 < 0.06 → 登记新声纹
	sid2, tr2, dataDir2, transcripts2, speakers2 := seedSpeakerStage(t)
	fv2 := &fakeVoiceprint{matched: true, matchID: ids.New(), searchSim: 0.75, secondSim: 0.72}
	d2 := StageDeps{Transcripts: transcripts2, Speakers: speakers2, Voiceprint: fv2, DataDir: dataDir2}
	if err := runSpeakerStage(ctx, d2, sid2, tr2); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv2.added) == 0 {
		t.Fatal("领先不足（模糊匹配）时应登记新声纹")
	}
}

// TestStageSpeakerSavesSegmentEmbeddings 验证逐段声纹向量落库（000007 列）：
// stage 处理后每段应有 embedding BLOB（256×float32=1024B），供详情页按段
// 展示与声纹库的相似度 top-N（一句话可能混多人，段级才能审计切分/归属）。
func TestStageSpeakerSavesSegmentEmbeddings(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: &fakeVoiceprint{matched: false}, DataDir: dataDir}
	if err := runSpeakerStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(context.Background(), tr.ID)
	if len(segs) == 0 {
		t.Fatal("无段")
	}
	for _, s := range segs {
		if len(s.Embedding) != 1024 {
			t.Fatalf("段 %d 应有逐段声纹向量 BLOB(1024B)，实际 %d 字节", s.SequenceNo, len(s.Embedding))
		}
	}
}

// seedCleanSegStage 造「可验证干净段挑选」的会话：label "1" 两段（A 长 5s + B 短 0.4s）、
// label "2" 一段（C 短 0.5s）。overlapC 控制 A 是否与 C 时间相交（true→C 改为与 A 尾部重叠）。
// fake Embed 按调用顺序给正交 one-hot：A=e0、B=e1、C=e2。
func seedCleanSegStage(t *testing.T, overlapC bool) (ids.ID, *repo.Transcript, *repo.TranscriptRepo, *repo.SpeakerRepo, string) {
	t.Helper()
	requireFFmpeg(t)
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "speech.wav",
		StoragePath: "../../testdata/speech.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	cStart := int64(6000)
	if overlapC {
		cStart = 4800 // 与 A [0,5000) 相交 → A 不再「干净」
	}
	// C 时长 ≥3s：C 是「另一 ASR 说话人」支撑段（scenario2 用来与 A 交集使 A 不干净），
	// 需照常登记成第二个说话人。若 <3s 会被 Task2 的过短并入规则缓起并入 label1，令 addVecs 少一个、
	// 破坏本用例（干净段挑选）无关的登记数断言。cStart 不变，交集关系保持。
	cEnd := cStart + 3200
	segs := []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "长干净段", StartMS: 0, EndMS: 5000},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "1", Text: "短段", StartMS: 5100, EndMS: 5500},
		{TranscriptID: tr.ID, SequenceNo: 3, SpeakerLabel: "2", Text: "对方", StartMS: cStart, EndMS: cEnd},
	}
	if err := transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	transcodedDir := filepath.Join(dataDir, "transcoded")
	if err := os.MkdirAll(transcodedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open("../../testdata/speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(transcodedDir, sid.String()+".wav"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	return sid, tr, transcripts, speakers, dataDir
}

// decodeVec pipeline 测试内联的 BLOB→向量解码（256×float32 LE），与 float32Blob 互逆。
func decodeVec(t *testing.T, blob []byte) []float32 {
	t.Helper()
	if len(blob) != 1024 {
		t.Fatalf("BLOB 应 1024 字节，实际 %d", len(blob))
	}
	v := make([]float32, 256)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return v
}

// TestStageSpeakerEnrollPrefersCleanSeg 验证新声纹登记优先用「干净段」（2026-08-26 需求）：
// 优先选时长最长、与其他说话人段无时间交集且 ≥3s 的段；无满足段才退回全组聚合。
// 检索（Search）仍用全组聚合代表声纹——登记向量与检索向量分离，互不影响。
func TestStageSpeakerEnrollPrefersCleanSeg(t *testing.T) {
	ctx := context.Background()

	// 场景一：A(5s, 与 C 无交集) 是唯一干净段 → label1 登记向量 = A 的向量（one-hot e0）
	sid, tr, transcripts, speakers, dataDir := seedCleanSegStage(t, false)
	fv := &fakeVoiceprint{matched: false} // 不命中 → 登记新声纹
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	var sp1ID ids.ID
	for _, s := range segs {
		if s.SpeakerLabel == "1" {
			if s.SpeakerID == nil {
				t.Fatal("label1 段未回填 speaker_id")
			}
			sp1ID = *s.SpeakerID
		}
	}
	sp1, err := speakers.Get(ctx, sp1ID)
	if err != nil || sp1 == nil {
		t.Fatalf("登记的 speaker 读取失败: %v", err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), sp1ID) })
	emb := decodeVec(t, sp1.Embedding)
	// 干净段 A 的向量是 e0（Embed 第 1 次调用）：登记向量应只在下标 0 处非零。
	// 若误用聚合（e0+e1 均值），下标 1 也会有分量——据此区分。
	if math.Abs(float64(emb[0])-1) > 1e-6 {
		t.Fatalf("登记向量应为干净段 A 的向量（下标0≈1），实际 emb[0]=%v", emb[0])
	}
	for i := 1; i < 256; i++ {
		if emb[i] != 0 {
			t.Fatalf("登记向量应只有下标 0 非零（单段干净向量），下标 %d=%v", i, emb[i])
		}
	}
	if sp1.SampleCount != 1 {
		t.Fatalf("干净段登记 sample_count 应为 1，实际 %d", sp1.SampleCount)
	}
	// Add 收到的向量 = 登记向量（同一干净段向量）
	if len(fv.addVecs) != 2 {
		t.Fatalf("应登记 2 个新声纹（label1/label2），实际 %d", len(fv.addVecs))
	}
	if fv.addVecs[0][0] != 1 || fv.addVecs[0][1] != 0 {
		t.Fatalf("Add 收到的应是干净段向量（下标0=1, 下标1=0），实际 %v/%v", fv.addVecs[0][0], fv.addVecs[0][1])
	}
	// 检索仍用聚合代表：label1 组 rep = mean(e0,e1) 归一 → 下标 0/1 各 ≈0.707
	if len(fv.searchVecs) != 2 {
		t.Fatalf("应检索 2 次，实际 %d", len(fv.searchVecs))
	}
	rep := fv.searchVecs[0]
	if math.Abs(float64(rep[0])-0.7071) > 1e-3 || math.Abs(float64(rep[1])-0.7071) > 1e-3 {
		t.Fatalf("检索应使用聚合代表声纹（下标0/1≈0.707），实际 %v/%v", rep[0], rep[1])
	}

	// 场景二：A 与 C 时间相交 → 无干净段 → label1 登记向量退回全组聚合（e0+e1 均值）
	sid2, tr2, transcripts2, speakers2, dataDir2 := seedCleanSegStage(t, true)
	fv2 := &fakeVoiceprint{matched: false}
	d2 := StageDeps{Transcripts: transcripts2, Speakers: speakers2, Voiceprint: fv2, DataDir: dataDir2}
	if err := runSpeakerStage(ctx, d2, sid2, tr2); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs2, _ := transcripts2.ListSegments(ctx, tr2.ID)
	var sp2ID ids.ID
	for _, s := range segs2 {
		if s.SpeakerLabel == "1" {
			if s.SpeakerID == nil {
				t.Fatal("label1 段未回填 speaker_id")
			}
			sp2ID = *s.SpeakerID
		}
	}
	sp2, err := speakers2.Get(ctx, sp2ID)
	if err != nil || sp2 == nil {
		t.Fatalf("登记的 speaker 读取失败: %v", err)
	}
	t.Cleanup(func() { _ = speakers2.Delete(context.Background(), sp2ID) })
	emb2 := decodeVec(t, sp2.Embedding)
	if math.Abs(float64(emb2[0])-0.7071) > 1e-3 || math.Abs(float64(emb2[1])-0.7071) > 1e-3 {
		t.Fatalf("无干净段时登记向量应为聚合（下标0/1≈0.707），实际 %v/%v", emb2[0], emb2[1])
	}
	if sp2.SampleCount != 2 {
		t.Fatalf("聚合登记 sample_count 应为 2（两段），实际 %d", sp2.SampleCount)
	}
}

// libEntry 有状态 fake 库里的一条声纹（说话人 id → 向量）。
type libEntry struct {
	id  ids.ID
	vec []float32
}

// libVoiceprint 有状态 fake：模拟真实 sidecar/FAISS —— Add 把 (speaker_id, 向量) 入库，
// Search 对库内向量算余弦、按说话人取 max 得 top-1，次高取「另一个说话人」的最高分作 top-2
// （与真实 sidecar second_distance 语义一致；库不足 2 人时 top-2=0）。
// 静态 fakeVoiceprint 的 Search 返回固定结果、与查询向量/库状态无关，无法覆盖
// 「本 run 内 Add 后对后续组 Search 可见」这类时序相关行为——正是本 bug 的触发条件，故单独造此 fake。
// 向量按段 SequenceNo 指定（Embed 从切片路径 seg-{N}.wav 解析 N），模拟不同说话人的声纹。
type libVoiceprint struct {
	vecBySeq    map[int][]float32
	entries     []libEntry // 已登记声纹（预置 = 历史库；Add 追加 = 本 run 新登记）
	added       []ids.ID
	searchCalls int
}

func (f *libVoiceprint) Embed(_ context.Context, path string) ([]float32, error) {
	base := filepath.Base(path) // seg-{N}.wav
	numStr := strings.TrimSuffix(strings.TrimPrefix(base, "seg-"), ".wav")
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return nil, err
	}
	v, ok := f.vecBySeq[n]
	if !ok {
		return nil, fmt.Errorf("libVoiceprint: 未为 seq %d 配置向量", n)
	}
	return append([]float32(nil), v...), nil
}

func (f *libVoiceprint) Search(_ context.Context, vec []float32) (voiceprint.SearchResult, error) {
	f.searchCalls++
	if len(f.entries) == 0 {
		return voiceprint.SearchResult{Matched: false}, nil // 空库
	}
	// 每个说话人取与 query 的最高余弦（多向量按人去重）
	bySpk := map[ids.ID]float64{}
	for _, e := range f.entries {
		if c := cosineSim(vec, e.vec); c > bySpk[e.id] {
			bySpk[e.id] = c
		}
	}
	var top1ID ids.ID
	top1, top2 := -1.0, -1.0
	for id, c := range bySpk {
		if c > top1 {
			top2, top1, top1ID = top1, c, id
		} else if c > top2 {
			top2 = c
		}
	}
	if top2 < 0 {
		top2 = 0 // 库中不足 2 人 → 次高为 0（对齐真实 sidecar 契约）
	}
	return voiceprint.SearchResult{SpeakerID: top1ID, Distance: top1, SecondDistance: top2, Matched: true}, nil
}

func (f *libVoiceprint) Add(_ context.Context, vec []float32, id ids.ID) error {
	f.entries = append(f.entries, libEntry{id: id, vec: append([]float32(nil), vec...)})
	f.added = append(f.added, id)
	return nil
}
func (f *libVoiceprint) Remove(_ context.Context, _ ids.ID) error { return nil }
func (f *libVoiceprint) IDs(_ context.Context) ([]ids.ID, error) {
	seen := map[ids.ID]bool{}
	var ids2 []ids.ID
	for _, e := range f.entries {
		if !seen[e.id] {
			seen[e.id] = true
			ids2 = append(ids2, e.id)
		}
	}
	return ids2, nil
}

var _ voiceprint.Client = (*libVoiceprint)(nil)

// cosineSim 余弦相似度（fake 内联，供 libVoiceprint.Search 打分）。
func cosineSim(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// TestStageSpeakerFirstMultiSpeakerEmptyLibrary 复现并守护「声纹库为空时首次多人录入只建 1 个声纹」的 bug：
// 空库 + 两个 ASR 标签（不同人，互相余弦 0.75，落在弱命中 [0.72,0.8) 区间）。
// 旧逻辑（边搜边登记）：label1 先登记 → label2 的 Search 看到 label1 刚登记的声纹、top2=0，令弱命中门槛
// 退化成 0.72 → label2 被并入 label1 → 只建 1 个。
// 修复（先全部检索、再统一登记）：两组都只对「本 run 开始前的库」检索（此处为空）→ 均未命中 → 各建 1 个。
func TestStageSpeakerFirstMultiSpeakerEmptyLibrary(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vA := make([]float32, 256)
	vA[0] = 1
	vB := make([]float32, 256)
	vB[0] = 0.75
	vB[1] = float32(math.Sqrt(1 - 0.75*0.75)) // cos(vA,vB)=0.75
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vA, 3: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 2 {
		t.Fatalf("空库首次两人应各建 1 个声纹，实际登记 %d 个（弱命中把第二人并进了第一人）", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(context.Background(), tr.ID)
	distinct := map[ids.ID]bool{}
	for _, s := range segs {
		if s.SpeakerID == nil {
			t.Fatalf("段 %d 未回填 speaker_id", s.SequenceNo)
		}
		distinct[*s.SpeakerID] = true
	}
	if len(distinct) != 2 {
		t.Fatalf("应回填到 2 个不同说话人，实际 %d", len(distinct))
	}
}

// TestStageSpeakerHistoricalSingleVoiceprintWeakMatchReuses 守护修复不误伤「历史单声纹库的弱命中重认」：
// 库里已有 1 个历史说话人（上次录音登记），本次某人与其余弦 0.75、top2=0 → 应复用该历史声纹、不建新。
// 这正是弱命中规则存在的目的（同一人换环境相似度掉到 0.72~0.8 仍能重认）；B 方案只屏蔽「本 run 内新登记」
// 的声纹对后续组的可见性，历史声纹的弱命中保持不变。
func TestStageSpeakerHistoricalSingleVoiceprintWeakMatchReuses(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	hist := &repo.Speaker{Name: "历史人", Source: "auto"}
	if err := speakers.Create(ctx, hist); err != nil {
		t.Fatal(err)
	}
	// 未绑定 active speaker 会被 repo 包 EnsurePersonBootstrap 物化成 person，跨包污染共享库——收尾删掉
	// （对齐本文件 TestStageSpeakerMatchesExisting 的 cleanup）。
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), hist.ID) })
	vHist := make([]float32, 256)
	vHist[0] = 1
	vNear := make([]float32, 256)
	vNear[0] = 0.75
	vNear[1] = float32(math.Sqrt(1 - 0.75*0.75)) // cos(vNear,vHist)=0.75
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vNear, 2: vNear, 3: vNear},
		entries:  []libEntry{{id: hist.ID, vec: vHist}}, // 预置历史库（本 run 开始前已存在）
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 0 {
		t.Fatalf("与历史单声纹弱命中应复用、不建新，实际登记 %d 个", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SpeakerID == nil || *s.SpeakerID != hist.ID {
			t.Fatalf("段 %d 应复用历史声纹 %s，实际 %+v", s.SequenceNo, hist.ID, s.SpeakerID)
		}
	}
}

// buildCorrectionVecs 构造纠正 pass 测试用的三组单位向量：
// vR   真人/新登记说话人方向（e0）
// vHist 历史库说话人方向（与 vR 余弦 = corrHistR，模拟两人声纹相关）
// vSeg  某幽灵段向量：cos(vSeg,vR)=simR、cos(vSeg,vHist)=simHist
// 返回三者（256 维，L2 归一）。要求 simR、simHist、corrHistR 几何可解（z²≥0）。
func buildCorrectionVecs(t *testing.T, corrHistR, simR, simHist float64) (vR, vHist, vSeg []float32) {
	t.Helper()
	vR = make([]float32, 256)
	vR[0] = 1
	h1 := math.Sqrt(1 - corrHistR*corrHistR)
	vHist = make([]float32, 256)
	vHist[0] = float32(corrHistR)
	vHist[1] = float32(h1)
	// vSeg = x*e0 + y*e1 + z*e2，x=simR；x*corrHistR + y*h1 = simHist
	x := simR
	y := (simHist - x*corrHistR) / h1
	z2 := 1 - x*x - y*y
	if z2 < 0 {
		t.Fatalf("几何不可解: simR=%v simHist=%v corr=%v (z²=%v)", simR, simHist, corrHistR, z2)
	}
	vSeg = make([]float32, 256)
	vSeg[0] = float32(x)
	vSeg[1] = float32(y)
	vSeg[2] = float32(math.Sqrt(z2))
	return vR, vHist, vSeg
}

// TestStageSpeakerCorrectsPhantomHistoricalMatch 幽灵历史声纹纠正主链路：
// label "1"(seq1,2)=真人 → 空历史库中登记为新声纹；label "2"(seq3)=幽灵 → 弱命中历史人铉晔。
// 铉晔在幽灵段上 max=0.73，真人在幽灵段上 max=0.88 > 0.73+0.06 → 整组改判给真人，写 corrected_from=铉晔。
func TestStageSpeakerCorrectsPhantomHistoricalMatch(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vR, vHist, vSeg := buildCorrectionVecs(t, 0.7, 0.88, 0.73)
	ghost := &repo.Speaker{Name: "铉晔", Source: "auto", Embedding: float32Blob(vHist), SampleCount: 1}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), ghost.ID) })
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vR, 2: vR, 3: vSeg},
		entries:  []libEntry{{id: ghost.ID, vec: vHist}}, // 本 run 开始前的历史库
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	bySeq := map[int]repo.TranscriptSegment{}
	for _, s := range segs {
		bySeq[s.SequenceNo] = s
	}
	realID := bySeq[1].SpeakerID // 真人 = seq1 归属
	if realID == nil {
		t.Fatal("seq1 未回填")
	}
	seg3 := bySeq[3]
	if seg3.SpeakerID == nil || *seg3.SpeakerID != *realID {
		t.Fatalf("幽灵段 seq3 应改判给真人 %v，实际 %+v", *realID, seg3.SpeakerID)
	}
	if seg3.CorrectedFromSpeakerID == nil || *seg3.CorrectedFromSpeakerID != ghost.ID {
		t.Fatalf("幽灵段应有 corrected_from=铉晔 %v，实际 %+v", ghost.ID, seg3.CorrectedFromSpeakerID)
	}
	if len(fv.added) != 1 {
		t.Fatalf("应只新登记真人 1 个声纹，实际 %d", len(fv.added))
	}
}

// TestStageSpeakerKeepsHistoricalMatchWhenSelfHighest 历史命中且在自己段上最高 → 不纠正。
// 幽灵段 cos 到铉晔=0.85(强命中)、到真人=0.5：0.5 不 > 0.85+0.06 → 保持铉晔、无标记。
func TestStageSpeakerKeepsHistoricalMatchWhenSelfHighest(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vR, vHist, vSeg := buildCorrectionVecs(t, 0.7, 0.5, 0.85)
	ghost := &repo.Speaker{Name: "铉晔", Source: "auto", Embedding: float32Blob(vHist), SampleCount: 1}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), ghost.ID) })
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vR, 2: vR, 3: vSeg},
		entries:  []libEntry{{id: ghost.ID, vec: vHist}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo == 3 {
			if s.SpeakerID == nil || *s.SpeakerID != ghost.ID {
				t.Fatalf("seq3 应保持铉晔，实际 %+v", s.SpeakerID)
			}
			if s.CorrectedFromSpeakerID != nil {
				t.Fatalf("未纠正段不应有标记，实际 %+v", s.CorrectedFromSpeakerID)
			}
		}
	}
}

// TestStageSpeakerNeverCorrectsAutoRegistered 空历史库 → 两组都是新登记(非历史命中)：
// 即使一组段更像另一组，也不参与纠正（只有历史命中组是候选）。断言无任何 corrected_from。
func TestStageSpeakerNeverCorrectsAutoRegistered(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vA := make([]float32, 256)
	vA[0] = 1
	vB := make([]float32, 256)
	vB[0] = 0.9
	vB[1] = float32(math.Sqrt(1 - 0.81))                                   // cos(vA,vB)=0.9
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vA, 3: vB}} // 空历史库
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.CorrectedFromSpeakerID != nil {
			t.Fatalf("新登记组不应被纠正，seq%d 却有标记 %+v", s.SequenceNo, s.CorrectedFromSpeakerID)
		}
	}
	if len(fv.added) != 2 {
		t.Fatalf("空库两人应各建 1 个，实际 %d", len(fv.added))
	}
}

// TestStageSpeakerCorrectionMarginBoundary 边界：真人在幽灵段上恰好 = self+margin（严格大于才纠正）→ 不纠正。
// self(铉晔)=0.73，margin=0.06 → 真人=0.79 恰好不触发。
func TestStageSpeakerCorrectionMarginBoundary(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vR, vHist, vSeg := buildCorrectionVecs(t, 0.7, 0.79, 0.73)
	ghost := &repo.Speaker{Name: "铉晔", Source: "auto", Embedding: float32Blob(vHist), SampleCount: 1}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), ghost.ID) })
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vR, 2: vR, 3: vSeg},
		entries:  []libEntry{{id: ghost.ID, vec: vHist}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo == 3 && s.CorrectedFromSpeakerID != nil {
			t.Fatalf("恰好等于 self+margin 不应触发（需严格大于），实际标记 %+v", s.CorrectedFromSpeakerID)
		}
	}
}

// TestStageSpeakerMergesShortGroupIntoNearest 过短噪声组并入最近在场说话人。
func TestStageSpeakerMergesShortGroupIntoNearest(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "正常说话一", StartMS: 0, EndMS: 2000},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "正常说话二", StartMS: 2100, EndMS: 4100},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "嗯。", StartMS: 4200, EndMS: 4600}, // 0.4s 噪声
	})
	vReal := make([]float32, 256)
	vReal[0] = 1
	vNoise := make([]float32, 256)
	vNoise[0] = 0.69
	vNoise[1] = float32(math.Sqrt(1 - 0.69*0.69))
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vReal, 2: vReal, 3: vNoise}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	bySeq := map[int]repo.TranscriptSegment{}
	for _, s := range segs {
		bySeq[s.SequenceNo] = s
	}
	realID := bySeq[1].SpeakerID
	if realID == nil {
		t.Fatal("真人组未回填")
	}
	seg3 := bySeq[3]
	if seg3.SpeakerID == nil || *seg3.SpeakerID != *realID {
		t.Fatalf("过短段应并入真人 %v，实际 %+v", *realID, seg3.SpeakerID)
	}
	if seg3.CorrectedReason == nil || *seg3.CorrectedReason != "short" {
		t.Fatalf("过短段应 corrected_reason=short，实际 %+v", seg3.CorrectedReason)
	}
	if seg3.CorrectedFromSpeakerID != nil {
		t.Fatalf("过短并入 corrected_from 应为 NULL，实际 %+v", seg3.CorrectedFromSpeakerID)
	}
	if len(fv.added) != 1 {
		t.Fatalf("过短组不应登记声纹，应只登记真人 1 个，实际 %d", len(fv.added))
	}
}

// TestStageSpeakerLongNewGroupStillRegisters ≥3s 新组照常登记、不并入、无 corrected_reason。
func TestStageSpeakerLongNewGroupStillRegisters(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "长段一", StartMS: 0, EndMS: 2000},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "长段二", StartMS: 2100, EndMS: 4100},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "也很长的一段独立说话", StartMS: 4200, EndMS: 7500},
	})
	vA := make([]float32, 256)
	vA[0] = 1
	vB := make([]float32, 256)
	vB[1] = 1
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vA, 3: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.CorrectedReason != nil {
			t.Fatalf("非过短组不应有 corrected_reason，seq%d=%+v", s.SequenceNo, s.CorrectedReason)
		}
	}
	if len(fv.added) != 2 {
		t.Fatalf("两个 ≥3s 新组应各登记 1 个，实际 %d", len(fv.added))
	}
}

// TestStageSpeakerAllShortFallbackRegisters 全部组过短 → 无并入目标 → 退回照常登记。
func TestStageSpeakerAllShortFallbackRegisters(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "嗯", StartMS: 0, EndMS: 500},
		{SequenceNo: 2, SpeakerLabel: "B", Text: "啊", StartMS: 600, EndMS: 1000},
	})
	vA := make([]float32, 256)
	vA[0] = 1
	vB := make([]float32, 256)
	vB[1] = 1
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SpeakerID == nil {
			t.Fatalf("全过短退回登记后每段应有归属，seq%d 仍 NULL", s.SequenceNo)
		}
		if s.CorrectedReason != nil {
			t.Fatalf("全过短退回登记不应打 short 标记，seq%d=%+v", s.SequenceNo, s.CorrectedReason)
		}
	}
	if len(fv.added) != 2 {
		t.Fatalf("全过短退回：两组各登记 1 个，实际 %d", len(fv.added))
	}
}

// TestStageSpeakerMergesShortGroupIntoHistoricalMatch 过短噪声组并入的目标是「命中历史库的真人」：
// label"A"(seq1,2)=真人且命中历史声纹 H（复用，不新建）；label"B"(seq3)=0.4s 噪声 → 并入 H。
// 覆盖 mergeShortGroups 目标为 matched 组（resolvedID 指向历史 speaker）的路径。
func TestStageSpeakerMergesShortGroupIntoHistoricalMatch(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "真人说话一", StartMS: 0, EndMS: 2000},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "真人说话二", StartMS: 2100, EndMS: 4100},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "嗯。", StartMS: 4200, EndMS: 4600},
	})
	vHist := make([]float32, 256)
	vHist[0] = 1
	hist := &repo.Speaker{Name: "历史真人", Source: "auto", Embedding: float32Blob(vHist), SampleCount: 1}
	if err := speakers.Create(ctx, hist); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), hist.ID) })
	vNoise := make([]float32, 256)
	vNoise[0] = 0.69
	vNoise[1] = float32(math.Sqrt(1 - 0.69*0.69))
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vHist, 2: vHist, 3: vNoise}, // A 命中历史 H；B 噪声最近 H
		entries:  []libEntry{{id: hist.ID, vec: vHist}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	bySeq := map[int]repo.TranscriptSegment{}
	for _, s := range segs {
		bySeq[s.SequenceNo] = s
	}
	// A 复用历史 H
	if s := bySeq[1]; s.SpeakerID == nil || *s.SpeakerID != hist.ID {
		t.Fatalf("真人组应复用历史 H %v，实际 %+v", hist.ID, s.SpeakerID)
	}
	// B(过短) 并入 H + short
	seg3 := bySeq[3]
	if seg3.SpeakerID == nil || *seg3.SpeakerID != hist.ID {
		t.Fatalf("过短段应并入历史 H %v，实际 %+v", hist.ID, seg3.SpeakerID)
	}
	if seg3.CorrectedReason == nil || *seg3.CorrectedReason != "short" {
		t.Fatalf("过短段应 corrected_reason=short，实际 %+v", seg3.CorrectedReason)
	}
	if len(fv.added) != 0 {
		t.Fatalf("A 命中历史、B 缓起并入，应无新登记，实际 %d", len(fv.added))
	}
}
