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

// fakeVoiceprint 实现 voiceprint.Client，可控 matched/matchID，记录 Add 调用。
type fakeVoiceprint struct {
	matched bool
	matchID ids.ID
	added   []ids.ID
	embedOK bool
}

func (f *fakeVoiceprint) Embed(_ context.Context, _ string) ([]float32, error) {
	// 返回非零向量（同组聚合后仍稳定；fake Search 不看内容）
	v := make([]float32, 256)
	for i := range v {
		v[i] = 0.1
	}
	return v, nil
}
func (f *fakeVoiceprint) Search(_ context.Context, _ []float32) (ids.ID, float64, bool, error) {
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
