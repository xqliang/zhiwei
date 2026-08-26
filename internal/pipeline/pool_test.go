package pipeline

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// 冒烟：pending 任务被领取执行，成功后推进到 done
func TestPoolRunsJobToDone(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
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
	jobID := latestJobID(t, jobs, sid)

	done := make(chan ids.ID, 1)
	handlers := map[string]Handler{
		"asr":     func(ctx context.Context, _ *repo.Job, _ ids.ID) error { return nil },
		"segment": func(ctx context.Context, _ *repo.Job, _ ids.ID) error { return nil },
	}
	p := NewPool(jobs, Flow{Stages: []string{"asr", "segment"}}, handlers)
	p.OnDone(func(_ context.Context, s ids.ID) { done <- s })
	p.Start(ctx)

	// 轮询本 job 的终态（其他测试可能并发写 pending job，领取顺序不保证）
	deadline := time.Now().Add(15 * time.Second)
	var final repo.Job
	for {
		if err := jobs.DB.Get(&final,
			`SELECT * FROM pipeline_job WHERE id = ?`, jobID.Int64()); err != nil {
			t.Fatal(err)
		}
		if final.Status == "done" || final.Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("15s 内未完成，当前状态 %s", final.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()

	if final.Status != "done" {
		t.Fatalf("job status = %s, last_error = %v", final.Status, final.LastError)
	}
	// onDone 至少触发过一次
	select {
	case <-done:
	default:
		t.Fatal("onDone 未触发")
	}
}

// latestJobID 取指定 session 最新一条 job 的 ID。
func latestJobID(t *testing.T, jobs *repo.JobRepo, sid ids.ID) ids.ID {
	t.Helper()
	var j repo.Job
	if err := jobs.DB.Get(&j,
		`SELECT * FROM pipeline_job WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sid.Int64()); err != nil {
		t.Fatal(err)
	}
	return j.ID
}
