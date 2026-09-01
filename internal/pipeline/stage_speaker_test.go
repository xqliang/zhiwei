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
// TestStageSpeakerSameVoiceLabelsMergeInSession 同声 ASR 标签在场归并（2026-08-31 需求，反转旧语义）：
// 旧设计「信任 ASR diarization、本录音内不同标签一律不本地合并」在过度切分场景会为同一人
// 重复登记声纹（库污染源头）。新语义：未命中组与本场更主要（durMS 更长）说话人的声纹
// 够像（≥ InSessionMin 0.72）→ 并入，不登记新声纹。本用例各段同向量（cos=1.0）：
// 空库下 label1(3.5s 主) 先登记 1 个，label2(3.1s) 并入 → added==1、两组段同一 speaker。
func TestStageSpeakerSameVoiceLabelsMergeInSession(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	fv := &fakeVoiceprint{matched: false, sameVec: true} // 各段同向量：同一个人的声音被 ASR 拆成两个标签
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 1 { // 只登记主要说话人 1 个（label2 并入，不再各标签各建）
		t.Fatalf("同人标签应归并只登记 1 个，实际 %d", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(context.Background(), tr.ID)
	speakerIDs := map[ids.ID]bool{}
	for _, s := range segs {
		if s.SpeakerID == nil {
			t.Fatalf("段 %d 未回填 speaker_id", s.SequenceNo)
		}
		speakerIDs[*s.SpeakerID] = true
	}
	if len(speakerIDs) != 1 {
		t.Fatalf("两组同人标签应归并到同一 speaker，实际 %d 个", len(speakerIDs))
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
	// 检索与登记同基准（2026-09-01 修正）：优先干净段 A 的向量 e0（下标0=1，下标1=0）；
	// 旧实现用全组聚合（下标0/1≈0.707）——碎段污染会把对真身的领先压缩到宽松命中门槛以下。
	if len(fv.searchVecs) != 2 {
		t.Fatalf("应检索 2 次，实际 %d", len(fv.searchVecs))
	}
	rep := fv.searchVecs[0]
	if math.Abs(float64(rep[0])-1) > 1e-6 || rep[1] != 0 {
		t.Fatalf("检索应优先干净段向量（下标0=1, 下标1=0），实际 %v/%v", rep[0], rep[1])
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
// 空库 + 两个 ASR 标签（不同人）。旧逻辑（边搜边登记）：label1 先登记 → label2 的 Search 看到 label1 刚登记的声纹、top2=0，令弱命中门槛
// 退化成 0.72 → label2 被并入 label1 → 只建 1 个。
// 修复（先全部检索、再统一登记）：两组都只对「本 run 开始前的库」检索（此处为空）→ 均未命中 → 各建 1 个。
// 注：两向量余弦取 0.6（2026-08-31 调整，原 0.75）：「碎片在场优先」上线后 ≥0.72 的同场组会被视为
// 同一人并入（实测同场不同人 ≤0.67、同人 0.76+），0.75 属同人区间不再代表「两个不同人」。
func TestStageSpeakerFirstMultiSpeakerEmptyLibrary(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vA := make([]float32, 256)
	vA[0] = 1
	vB := make([]float32, 256)
	vB[0] = 0.6
	vB[1] = float32(math.Sqrt(1 - 0.6*0.6)) // cos(vA,vB)=0.6（不同人区间）
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
// 注：vB·vA 取 0.6（2026-08-31 调整，原 0.9）：≥0.72 的同场组现会被「在场归并」并入
// （同人语义），0.9 属同人区间；0.6 代表真实的两个不同人。
func TestStageSpeakerNeverCorrectsAutoRegistered(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vA := make([]float32, 256)
	vA[0] = 1
	vB := make([]float32, 256)
	vB[0] = 0.6
	vB[1] = float32(math.Sqrt(1 - 0.36))                                   // cos(vA,vB)=0.6（不同人区间）
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
	// 幽灵组用长段（12s，非碎片）：「碎片在场改判」（2026-08-31）对 <10s 的碎片组用
	// 「锚点不比归属差即改判」的宽松判据，会吃掉本用例的 margin 边界——边界纪律只对
	// 非碎片组生效，故本用例须把幽灵组造长。
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "真人说话一", StartMS: 0, EndMS: 2000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "真人说话二", StartMS: 2100, EndMS: 4100},
		{SequenceNo: 3, SpeakerLabel: "2", Text: "幽灵长段", StartMS: 4200, EndMS: 16200}, // 12s：非碎片
	})
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

// ---------- 碎片在场优先（2026-08-31 需求） ----------

// mkUnitVec 造 256 维单位向量，给定前三个分量（其余 0）。碎片在场用例的相似度几何构造用。
func mkUnitVec(x, y, z float64) []float32 {
	return mkVec([2]float64{0, x}, [2]float64{1, y}, [2]float64{2, z})
}

// TestStageSpeakerUnmatchedFragmentMergesIntoAnchor 未命中碎片并入在场锚点（思敏/说话人ghqhg case）：
// 主组(8s) 强命中历史人思敏；碎片组(4s) rep 检索 top1=思敏 0.76、top2=干扰人文生 0.71，
// gap=0.05 < 0.06 弱命中差一口气 → 未命中。守门：碎片 vs 思敏样本 segMax=0.76 ≥ 0.72 →
// 并入思敏、不登记新声纹（旧逻辑正是这里登记出「说话人ghqhg」类脏声纹）。
func TestStageSpeakerUnmatchedFragmentMergesIntoAnchor(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "思敏主讲一", StartMS: 0, EndMS: 5000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "思敏主讲二", StartMS: 5100, EndMS: 8100}, // 组 8.1s = 锚点
		{SequenceNo: 3, SpeakerLabel: "2", Text: "碎片短句", StartMS: 8200, EndMS: 12200}, // 4s 碎片
	})
	vSimin := mkUnitVec(1, 0, 0)                        // 历史人思敏 = e0
	vMain := mkUnitVec(0.95, math.Sqrt(1-0.95*0.95), 0) // 主组段：vs 思敏 0.95 强命中
	// 碎片段：vs 思敏 0.76（≥0.72 守门过）、vs 干扰人 0.71（gap 0.05 令检索弱命中失败）。
	// 干扰人 decoy 须贴近碎片（构成 top2）但远离主组（0.715）与思敏（0.508），
	// 否则会变成主组检索的幽灵 top1（DB 无此人 → 主组被误判未命中）。
	vFrag := mkUnitVec(0.76, 0.0596, math.Sqrt(1-0.76*0.76-0.0596*0.0596))
	vOther := mkUnitVec(0.5076, 0.7453, 0.4323)
	if s := cosineSim(vFrag, vOther); math.Abs(s-0.71) > 0.01 {
		t.Fatalf("几何构造误差: vFrag·vOther=%.4f want 0.71", s)
	}
	if s := cosineSim(vMain, vOther); s > 0.8 {
		t.Fatalf("decoy 离主组太近(%.4f)，会变成主组检索的幽灵 top1", s)
	}
	simin := &repo.Speaker{Name: "思敏", Source: "auto", Embedding: float32Blob(vSimin), SampleCount: 1}
	if err := speakers.Create(ctx, simin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), simin.ID) })
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vMain, 2: vMain, 3: vFrag},
		entries:  []libEntry{{id: simin.ID, vec: vSimin}, {id: ids.New(), vec: vOther}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 0 {
		t.Fatalf("碎片应并入在场思敏、不登记新声纹，实际登记 %d 个", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SpeakerID == nil || *s.SpeakerID != simin.ID {
			t.Fatalf("段 %d 应并入思敏 %v，实际 %+v", s.SequenceNo, simin.ID, s.SpeakerID)
		}
	}
}

// TestStageSpeakerUnmatchedFragmentRegistersWhenDissimilar 负例：碎片与在场说话人不够像
// （0.65 < 0.72，真实不同人区间）→ 照常登记新声纹（在场归并不能吞掉真人新说话人）。
func TestStageSpeakerUnmatchedFragmentRegistersWhenDissimilar(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "思敏主讲一", StartMS: 0, EndMS: 5000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "思敏主讲二", StartMS: 5100, EndMS: 8100},
		{SequenceNo: 3, SpeakerLabel: "2", Text: "另一位短句", StartMS: 8200, EndMS: 12200},
	})
	vSimin := mkUnitVec(1, 0, 0)
	vMain := mkUnitVec(0.95, math.Sqrt(1-0.95*0.95), 0)
	vFrag := mkUnitVec(0.65, math.Sqrt(1-0.65*0.65), 0) // vs 思敏 0.65：既不命中也过不了守门
	simin := &repo.Speaker{Name: "思敏", Source: "auto", Embedding: float32Blob(vSimin), SampleCount: 1}
	if err := speakers.Create(ctx, simin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), simin.ID) })
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vMain, 2: vMain, 3: vFrag},
		entries:  []libEntry{{id: simin.ID, vec: vSimin}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 1 {
		t.Fatalf("不够像的短组应照常登记新声纹，实际登记 %d 个", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	bySeq := map[int]repo.TranscriptSegment{}
	for _, s := range segs {
		bySeq[s.SequenceNo] = s
	}
	if s := bySeq[3]; s.SpeakerID == nil || *s.SpeakerID == simin.ID {
		t.Fatalf("碎片应属新说话人，实际 %+v", s.SpeakerID)
	}
}

// TestStageSpeakerFragmentOrderIndependence 碎片标签在前也能并入：2b 按总时长降序处理，
// 主说话人（无论 ASR 标签顺序）先解析/登记，碎片随后才有锚点可并。空库场景。
func TestStageSpeakerFragmentOrderIndependence(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "碎片在前", StartMS: 0, EndMS: 4000},     // 碎片 4s（标签靠前）
		{SequenceNo: 2, SpeakerLabel: "2", Text: "主讲一", StartMS: 4100, EndMS: 10000},  // 主 5.9s
		{SequenceNo: 3, SpeakerLabel: "2", Text: "主讲二", StartMS: 10100, EndMS: 16000}, // 主共 11.8s
	})
	vMain := mkUnitVec(1, 0, 0)
	vFrag := mkUnitVec(0.8, 0.6, 0) // cos(vFrag,vMain)=0.8 ≥ 0.72（同人碎片）
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vFrag, 2: vMain, 3: vMain}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 1 {
		t.Fatalf("空库主+同人碎片应只登记主说话人 1 个，实际 %d", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	speakerIDs := map[ids.ID]bool{}
	for _, s := range segs {
		if s.SpeakerID == nil {
			t.Fatalf("段 %d 未回填", s.SequenceNo)
		}
		speakerIDs[*s.SpeakerID] = true
	}
	if len(speakerIDs) != 1 {
		t.Fatalf("碎片应并入主说话人（标签顺序无关），实际 %d 个 speaker", len(speakerIDs))
	}
}

// TestStageSpeakerFragmentOverrideToPresentSpeaker 命中碎片被在场锚点改判（铉晔/杰辉 case）：
// 紧声纹 cohort——库内杰辉与铉晔互 0.75。主组(12.1s)强命中杰辉；碎片组(3s) rep（两段均值）
// 检索弱命中铉晔（rep vs 铉晔 0.763、vs 杰辉 0.572，gap 大），但段级 vs 杰辉 0.74 ≥ vs 铉晔
// (归属) 0.73 → 「锚点不比归属差」整组改判给在场杰辉（旧逻辑命中即定终身，正是误判 case）。
// 两段碎片镜像构造：均值指向铉晔（rep 检索命中它）、单段微倾杰辉（段级改判依据）。
func TestStageSpeakerFragmentOverrideToPresentSpeaker(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "杰辉主讲一", StartMS: 0, EndMS: 6000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "杰辉主讲二", StartMS: 6100, EndMS: 12100}, // 主组 12.1s 锚点
		{SequenceNo: 3, SpeakerLabel: "2", Text: "碎片句一", StartMS: 12200, EndMS: 13700}, // 碎片 1.5s
		{SequenceNo: 4, SpeakerLabel: "2", Text: "碎片句二", StartMS: 13800, EndMS: 15300}, // 碎片组共 3s
	})
	vXuanye := mkUnitVec(1, 0, 0)                                        // 铉晔 = e0
	vJiehui := mkUnitVec(0.75, 0.6614, 0)                                // 杰辉：与铉晔互 0.75（紧 cohort）
	vMainJ := mkUnitVec(0.82, 0.431, math.Sqrt(1-0.82*0.82-0.431*0.431)) // vs 杰辉 0.9 强命中
	vF1 := mkUnitVec(0.73, 0.291, math.Sqrt(1-0.73*0.73-0.291*0.291))    // 段级 vs 铉晔 0.73 / vs 杰辉 0.74
	vF2 := mkUnitVec(0.73, -0.291, math.Sqrt(1-0.73*0.73-0.291*0.291))   // e1 分量镜像：均值 rep 指向铉晔
	if s := cosineSim(vF1, vJiehui); math.Abs(s-0.74) > 0.01 {
		t.Fatalf("几何构造误差: vF1·vJiehui=%.4f want 0.74", s)
	}
	xuanye := &repo.Speaker{Name: "铉晔", Source: "auto", Embedding: float32Blob(vXuanye), SampleCount: 1}
	jiehui := &repo.Speaker{Name: "杰辉", Source: "auto", Embedding: float32Blob(vJiehui), SampleCount: 1}
	for _, sp := range []*repo.Speaker{xuanye, jiehui} {
		if err := speakers.Create(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = speakers.Delete(context.Background(), xuanye.ID)
		_ = speakers.Delete(context.Background(), jiehui.ID)
	})
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vMainJ, 2: vMainJ, 3: vF1, 4: vF2},
		entries:  []libEntry{{id: xuanye.ID, vec: vXuanye}, {id: jiehui.ID, vec: vJiehui}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 0 {
		t.Fatalf("两组都命中库内声纹，不应新登记，实际 %d", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo >= 3 {
			if s.SpeakerID == nil || *s.SpeakerID != jiehui.ID {
				t.Fatalf("碎片段 %d 应改判给在场杰辉 %v，实际 %+v", s.SequenceNo, jiehui.ID, s.SpeakerID)
			}
			if s.CorrectedFromSpeakerID == nil || *s.CorrectedFromSpeakerID != xuanye.ID {
				t.Fatalf("碎片段 %d 应有 corrected_from=铉晔，实际 %+v", s.SequenceNo, s.CorrectedFromSpeakerID)
			}
		}
	}
}

// TestStageSpeakerFragmentKeepsOwnMatchWhenSelfHigher 负例：真身碎片段级仍显著更像归属
// （vs 归属 0.80 > vs 在场 0.70）→ 保持归属，不被在场改判吞掉。
func TestStageSpeakerFragmentKeepsOwnMatchWhenSelfHigher(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "杰辉主讲一", StartMS: 0, EndMS: 6000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "杰辉主讲二", StartMS: 6100, EndMS: 12100},
		{SequenceNo: 3, SpeakerLabel: "2", Text: "铉晔本人碎片", StartMS: 12200, EndMS: 15200}, // 3s 碎片
	})
	vXuanye := mkUnitVec(1, 0, 0)
	vJiehui := mkUnitVec(0.75, 0.6614, 0)
	vMainJ := mkUnitVec(0.82, 0.431, math.Sqrt(1-0.82*0.82-0.431*0.431))
	// 铉晔本人碎片：vs 铉晔 0.80（强命中）、vs 杰辉 0.70 → self 高于锚点，保持
	vFrag := mkUnitVec(0.80, 0.1513, math.Sqrt(1-0.80*0.80-0.1513*0.1513))
	if s := cosineSim(vFrag, vJiehui); math.Abs(s-0.70) > 0.01 {
		t.Fatalf("几何构造误差: vFrag·vJiehui=%.4f want 0.70", s)
	}
	xuanye := &repo.Speaker{Name: "铉晔", Source: "auto", Embedding: float32Blob(vXuanye), SampleCount: 1}
	jiehui := &repo.Speaker{Name: "杰辉", Source: "auto", Embedding: float32Blob(vJiehui), SampleCount: 1}
	for _, sp := range []*repo.Speaker{xuanye, jiehui} {
		if err := speakers.Create(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = speakers.Delete(context.Background(), xuanye.ID)
		_ = speakers.Delete(context.Background(), jiehui.ID)
	})
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vMainJ, 2: vMainJ, 3: vFrag},
		entries:  []libEntry{{id: xuanye.ID, vec: vXuanye}, {id: jiehui.ID, vec: vJiehui}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo == 3 {
			if s.SpeakerID == nil || *s.SpeakerID != xuanye.ID {
				t.Fatalf("铉晔本人碎片应保持铉晔，实际 %+v", s.SpeakerID)
			}
			if s.CorrectedFromSpeakerID != nil {
				t.Fatalf("不应改判，实际标记 %+v", s.CorrectedFromSpeakerID)
			}
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

// mkVec 造单位向量：indices/vals 指定分量（其余 0）。用于段级改判用例的相似度几何。
func mkVec(pairs ...[2]float64) []float32 {
	v := make([]float32, 256)
	for _, p := range pairs {
		v[int(p[0])] = float32(p[1])
	}
	return v
}

// TestStageSpeakerReattributesSegmentToBetterPresentSpeaker 段级改判：seq2 归属 A 但对在场 B 相似 0.85、
// 对 A 仅 0.40（领先 0.45≥0.06 且 0.85≥0.72）→ 改判 B，corrected_reason=mismatch，corrected_from=A。
func TestStageSpeakerReattributesSegmentToBetterPresentSpeaker(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "A 的干净长段", StartMS: 0, EndMS: 3500},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "其实更像 B 的一段", StartMS: 3600, EndMS: 4000},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "B 的干净长段", StartMS: 5000, EndMS: 8200},
	})
	vA := mkVec([2]float64{0, 1})
	vB := mkVec([2]float64{1, 1})
	vMix := mkVec([2]float64{0, 0.40}, [2]float64{1, 0.85}, [2]float64{2, math.Sqrt(1 - 0.40*0.40 - 0.85*0.85)})
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vMix, 3: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	bySeq := map[int]repo.TranscriptSegment{}
	for _, s := range segs {
		bySeq[s.SequenceNo] = s
	}
	aID, bID := bySeq[1].SpeakerID, bySeq[3].SpeakerID
	if aID == nil || bID == nil {
		t.Fatal("A/B 未回填")
	}
	seg2 := bySeq[2]
	if seg2.SpeakerID == nil || *seg2.SpeakerID != *bID {
		t.Fatalf("seq2 应改判给 B %v，实际 %+v", *bID, seg2.SpeakerID)
	}
	if seg2.CorrectedReason == nil || *seg2.CorrectedReason != "mismatch" {
		t.Fatalf("seq2 应 corrected_reason=mismatch，实际 %+v", seg2.CorrectedReason)
	}
	if seg2.CorrectedFromSpeakerID == nil || *seg2.CorrectedFromSpeakerID != *aID {
		t.Fatalf("seq2 应 corrected_from=A %v，实际 %+v", *aID, seg2.CorrectedFromSpeakerID)
	}
	if bySeq[1].CorrectedReason != nil || bySeq[3].CorrectedReason != nil {
		t.Fatalf("seq1/seq3 不应被改判")
	}
}

// TestStageSpeakerReattributeKeepsWhenLeadBelowGap best_other≥0.72 但领先<0.06 → 不动。
func TestStageSpeakerReattributeKeepsWhenLeadBelowGap(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "A 的干净长段", StartMS: 0, EndMS: 3500},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "临界段", StartMS: 3600, EndMS: 4000},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "B 的干净长段", StartMS: 5000, EndMS: 8200},
	})
	vA := mkVec([2]float64{0, 1})
	vB := mkVec([2]float64{1, 1})
	vMix := mkVec([2]float64{0, 0.68}, [2]float64{1, 0.73}, [2]float64{2, math.Sqrt(1 - 0.68*0.68 - 0.73*0.73)})
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vMix, 3: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo == 2 && s.CorrectedReason != nil {
			t.Fatalf("领先<0.06 不应改判，实际 %+v", s.CorrectedReason)
		}
	}
}

// TestStageSpeakerReattributeKeepsWhenBelowMinSim best_other<segReattributeMinSim(0.6) → 不动。
func TestStageSpeakerReattributeKeepsWhenBelowMinSim(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "A 的干净长段", StartMS: 0, EndMS: 3500},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "都不太像的段", StartMS: 3600, EndMS: 4000},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "B 的干净长段", StartMS: 5000, EndMS: 8200},
	})
	vA := mkVec([2]float64{0, 1})
	vB := mkVec([2]float64{1, 1})
	// cos 到 A=0.40、到 B=0.55（best_other 0.55<0.6 下限）
	vMix := mkVec([2]float64{0, 0.40}, [2]float64{1, 0.55}, [2]float64{2, math.Sqrt(1 - 0.40*0.40 - 0.55*0.55)})
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vMix, 3: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo == 2 && s.CorrectedReason != nil {
			t.Fatalf("best_other<0.6 不应改判，实际 %+v", s.CorrectedReason)
		}
	}
}

// TestStageSpeakerReattributesAtLoweredFloor 下限降到 0.6 后，0.6~0.72 区间也改判：
// seq2 归属 A(cur 0.30)、对在场 B 相似 0.65（≥0.6 且领先 0.35≥0.06）→ 改判 B、mismatch。
func TestStageSpeakerReattributesAtLoweredFloor(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "A 的干净长段", StartMS: 0, EndMS: 3500},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "0.65 更像 B 的一段", StartMS: 3600, EndMS: 4000},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "B 的干净长段", StartMS: 5000, EndMS: 8200},
	})
	vA := mkVec([2]float64{0, 1})
	vB := mkVec([2]float64{1, 1})
	// cos 到 A=0.30、到 B=0.65（0.6≤0.65<0.72，领先 0.35≥0.06）
	vMix := mkVec([2]float64{0, 0.30}, [2]float64{1, 0.65}, [2]float64{2, math.Sqrt(1 - 0.30*0.30 - 0.65*0.65)})
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vMix, 3: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	bySeq := map[int]repo.TranscriptSegment{}
	for _, s := range segs {
		bySeq[s.SequenceNo] = s
	}
	bID := bySeq[3].SpeakerID
	seg2 := bySeq[2]
	if bID == nil || seg2.SpeakerID == nil || *seg2.SpeakerID != *bID {
		t.Fatalf("0.65≥0.6 应改判给 B，实际 %+v", seg2.SpeakerID)
	}
	if seg2.CorrectedReason == nil || *seg2.CorrectedReason != "mismatch" {
		t.Fatalf("应 corrected_reason=mismatch，实际 %+v", seg2.CorrectedReason)
	}
}

// TestStageSpeakerSearchPrefersCleanSeg：1:N 检索基准应优先「干净段」而非全组聚合。
// 复刻 2026-09-01 实测 case（session 2094724818275405824 误登记「说话人prbiv」）：
// 主力段对既有声纹领先 0.19~0.31，但 0~1s 短碎段更像另一说话人，把聚合的领先压缩到
// 0.0676 < LooseGap 0.1 → 三条命中规则全不中 → 整组被登记成新声纹。
// 向量构造：主力段 e0 = 历史说话人 P1，碎段 e1 = 历史说话人 P2（正交）；聚合
// [0.707,0.707] 与 P1/P2 双双 0.707 平手（gap=0 必不命中）；干净段 e0 对 P1=1.0 强命中。
func TestStageSpeakerSearchPrefersCleanSeg(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "主力长段", StartMS: 0, EndMS: 5000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "短碎段", StartMS: 5100, EndMS: 5500},
	})
	ctx := context.Background()
	// 历史库两位说话人：P1 与主力段同声、P2 与碎段同声。
	p1 := &repo.Speaker{Name: "P1", Source: "auto"}
	p2 := &repo.Speaker{Name: "P2", Source: "auto"}
	if err := speakers.Create(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := speakers.Create(ctx, p2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = speakers.Delete(context.Background(), p1.ID)
		_ = speakers.Delete(context.Background(), p2.ID)
	})
	e0, e1 := make([]float32, 256), make([]float32, 256)
	e0[0], e1[1] = 1, 1
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: e0, 2: e1},
		entries:  []libEntry{{id: p1.ID, vec: e0}, {id: p2.ID, vec: e1}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 0 {
		t.Fatalf("干净段对 P1 强命中应复用既有声纹、零登记（聚合平手 gap=0 才会误登记），实际登记 %d 个", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SpeakerID == nil || *s.SpeakerID != p1.ID {
			t.Fatalf("段 %d 应归属既有声纹 P1，实际 %+v", s.SequenceNo, s.SpeakerID)
		}
	}
	if fv.searchCalls != 1 {
		t.Fatalf("单组应检索 1 次，实际 %d", fv.searchCalls)
	}
}
