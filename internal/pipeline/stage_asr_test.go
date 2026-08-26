package pipeline

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

type fakeASR struct{}

func (fakeASR) Transcribe(_ context.Context, _ string) ([]provider.TranscriptPiece, error) {
	return []provider.TranscriptPiece{
		{SpeakerLabel: "1", Text: "明天记得给 Tom 发邮件", StartMS: 0, EndMS: 2000, Confidence: 0.95},
		{SpeakerLabel: "1", Text: "还有确认会议时间", StartMS: 2100, EndMS: 3600, Confidence: 0.92},
		{SpeakerLabel: "2", Text: "好的", StartMS: 3800, EndMS: 4200, Confidence: 0.9},
	}, nil
}

// 无 ffmpeg 环境跳过（stage 内部会调用 ffmpeg 转码）
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
}

func TestStagesASRAndSegment(t *testing.T) {
	requireFFmpeg(t)
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "speech.wav",
		StoragePath: "../../testdata/speech.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Create(ctx, &repo.Job{SessionID: sid, Stage: "asr", Status: "pending"}); err != nil {
		t.Fatal(err)
	}

	handlers := BuildStages(StageDeps{
		Sessions: sessions, Transcripts: transcripts, ASR: fakeASR{}, DataDir: "data",
	})
	pool := NewPool(jobs, Flow{Stages: []string{"asr", "segment"}}, handlers)

	runDone := make(chan ids.ID, 1)
	pool.OnDone(func(_ context.Context, s ids.ID) { runDone <- s })
	pool.Start(ctx)

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("30s 未跑完")
	}

	// 断言（注意：cancel 前执行，避免 context canceled）
	qctx := context.Background()
	tr, err := transcripts.GetBySession(qctx, sid)
	if err != nil {
		t.Fatalf("GetBySession: %v", err)
	}
	if tr.FullText == nil || *tr.FullText == "" {
		t.Fatal("full_text 为空")
	}
	segs, _ := transcripts.ListSegments(qctx, tr.ID)
	if len(segs) != 3 {
		t.Fatalf("segments = %d", len(segs))
	}
	cancel()
	os.RemoveAll("data/transcoded")
}
