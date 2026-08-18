package pipeline

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// 冒烟：pending 任务被领取执行，成功后推进到 done
func TestPoolRunsJobToDone(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	jobs := &repo.JobRepo{DB: db}
	sessions := &repo.SessionRepo{DB: db}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "x.wav",
		StoragePath: "/tmp/x.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Create(ctx, &repo.Job{SessionID: sid, Stage: "asr", Status: "pending"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan ids.ID, 1)
	handlers := map[string]Handler{
		"asr":     func(ctx context.Context, s ids.ID) error { return nil },
		"segment": func(ctx context.Context, s ids.ID) error { return nil },
	}
	p := NewPool(jobs, Flow{Stages: []string{"asr", "segment"}}, handlers)
	p.OnDone(func(_ context.Context, s ids.ID) { done <- s })
	p.Start(ctx)

	select {
	case got := <-done:
		if got != sid {
			t.Fatalf("done session = %d", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("10s 内未跑到 done")
	}
	cancel()

	var j repo.Job
	if err := jobs.DB.Get(&j,
		`SELECT * FROM pipeline_job WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sid.Int64()); err != nil {
		t.Fatal(err)
	}
	if j.Status != "done" {
		t.Fatalf("job status = %s", j.Status)
	}
}
