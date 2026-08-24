package pipeline

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// fakeVoiceprint 实现 voiceprint.Client，可控 matched/matchID，记录 Add 调用 + 各方法调用计数。
type fakeVoiceprint struct {
	matched     bool
	matchID     ids.ID
	added       []ids.ID
	embedOK     bool
	sameVec     bool // true→各段返回相同向量(测同人聚类)；false→逐段正交(测独立登记)
	embedCalls  int
	searchCalls int
}

func (f *fakeVoiceprint) Embed(_ context.Context, _ string) ([]float32, error) {
	f.embedCalls++
	v := make([]float32, 256)
	if f.sameVec {
		// 常量向量：各段声纹相同 → 同人不同 ASR 标签会被聚类合并（测同人合并）
		v[0] = 1.0
	} else {
		// 逐段正交 one-hot：不同段不同向量 → 不同组不会被误聚类（测独立登记）
		v[(f.embedCalls-1)%256] = 1.0
	}
	return v, nil
}
func (f *fakeVoiceprint) Search(_ context.Context, _ []float32) (ids.ID, float64, bool, error) {
	f.searchCalls++
	return f.matchID, 0.9, f.matched, nil
}
func (f *fakeVoiceprint) Add(_ context.Context, _ []float32, id ids.ID) error {
	f.added = append(f.added, id)
	return nil
}
func (f *fakeVoiceprint) Remove(_ context.Context, _ ids.ID) error { return nil }

var _ voiceprint.Client = (*fakeVoiceprint)(nil) // 编译期接口符合性

// seedSpeakerStage 准备 session + transcript + 3 段(标签 1/1/2) + DataDir 里的 transcoded wav。
// 返回 (sid, tr, dataDir, transcripts, speakers)。wav 复用 ../../testdata/speech.wav。
func seedSpeakerStage(t *testing.T) (ids.ID, *repo.Transcript, string, *repo.TranscriptRepo, *repo.SpeakerRepo) {
	t.Helper()
	requireFFmpeg(t)
	db, err := repo.NewDB(repo.TestDSN(t))
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
	segs := []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "明天发邮件", StartMS: 0, EndMS: 2000},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "1", Text: "确认会议", StartMS: 2100, EndMS: 3600},
		{TranscriptID: tr.ID, SequenceNo: 3, SpeakerLabel: "2", Text: "好的", StartMS: 3800, EndMS: 4200},
	}
	if err := transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}

	// 准备 stage 切片源 wav：{dataDir}/transcoded/{sid}.wav
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

// TestStageSpeakerClustersSamePerson 验证 session 内聚类：realtime prompt 式 diarization 可能
// 把同一人标成 spk0/spk1，stage 用声纹相似度兜底合并——fake 让两组返回相同向量（同人），
// 应聚成 1 个 speaker（只登记 1 次，3 段都归到同一个人），而非拆成 2 个。
// 注：真实场景需 WeSpeaker 真向量才生效，StubEmbedder 随机向量聚不出；本测用可控向量验证聚类逻辑本身。
func TestStageSpeakerClustersSamePerson(t *testing.T) {
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	fv := &fakeVoiceprint{matched: false, sameVec: true} // 各段同向量 → 两组同人 → 聚类合并
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(fv.added) != 1 { // 2 个 ASR 标签同人 → 聚成 1 个 → 只登记 1 次
		t.Fatalf("同人合并应只登记 1 个，实际 %d", len(fv.added))
	}
	segs, _ := transcripts.ListSegments(context.Background(), tr.ID)
	speakerIDs := map[ids.ID]bool{}
	for _, s := range segs {
		if s.SpeakerID == nil {
			t.Fatalf("段 %d 未回填 speaker_id", s.SequenceNo)
		}
		speakerIDs[*s.SpeakerID] = true
	}
	if len(speakerIDs) != 1 { // 3 段都归到同一个人
		t.Fatalf("同人合并后 3 段应归到 1 个 speaker，实际 %d", len(speakerIDs))
	}
}
