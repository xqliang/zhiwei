// zhiwei-server 是知微云端 MVP 的唯一入口：HTTP API + 异步 pipeline worker。
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"zhiwei/internal/api"
	"zhiwei/internal/config"
	"zhiwei/internal/ids"
	"zhiwei/internal/pipeline"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := ids.Init(1); err != nil {
		log.Fatal(err)
	}
	db, err := repo.NewDB(cfg.MySQLDSN)
	if err != nil {
		log.Fatal(err)
	}

	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}

	// pipeline 装配：ASR 用 StepFun realtime（见 asr-protocol-notes.md）
	asr := provider.NewStepFunASR(cfg.StepFunASREndpoint, cfg.StepFunAPIKey)
	stages := pipeline.BuildStages(pipeline.StageDeps{
		Sessions: sessions, Transcripts: transcripts, ASR: asr, DataDir: cfg.DataDir,
	})
	flow := pipeline.Flow{Stages: []string{"asr", "segment"}}
	pool := pipeline.NewPool(jobs, flow, stages)
	pool.OnDone(func(ctx context.Context, sid ids.ID) {
		_ = sessions.UpdateStatus(ctx, sid, "completed")
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool.Start(ctx)

	r := api.NewRouter()
	api.RegisterAudio(r, sessions, jobs, cfg.DataDir)
	api.RegisterQuery(r, &api.QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
	})

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r}
	go func() {
		log.Println("zhiwei-server listening on :" + cfg.Port)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	_ = srv.Close()
}
