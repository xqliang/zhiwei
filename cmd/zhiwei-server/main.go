// zhiwei-server 是知微云端 MVP 的唯一入口：HTTP API + 异步 pipeline worker。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"zhiwei/internal/api"
	"zhiwei/internal/config"
	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
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
	if cfg.StepFunAPIKey == "" {
		log.Fatal("STEPFUN_API_KEY 未设置：ASR 不可用。请先 source .env（set -a; source .env; set +a）再启动")
	}
	db, err := repo.NewDB(cfg.MySQLDSN)
	if err != nil {
		log.Fatal(err)
	}

	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	memories := &repo.MemoryRepo{DB: db}
	todos := &repo.TodoRepo{DB: db}
	topics := &repo.TopicRepo{DB: db}

	// 抽取 prompt（版本化文件，运行时读取；版本号见文件名与文件首行）
	promptBytes, err := os.ReadFile("prompts/extraction_v1.md")
	if err != nil {
		log.Fatal("读取抽取 prompt 失败: ", err)
	}

	// pipeline 装配：ASR 用 StepFun realtime（见 asr-protocol-notes.md），
	// LLM 走 Ark OpenAI 兼容接口（Tier 1 flash）
	asr := provider.NewStepFunASR(cfg.StepFunASREndpoint, cfg.StepFunAPIKey)
	llm := provider.NewArkLLM(cfg.ARKBaseURL, cfg.ARKAPIKey)
	stages := pipeline.BuildStages(pipeline.StageDeps{
		Sessions: sessions, Transcripts: transcripts, ASR: asr, DataDir: cfg.DataDir,
		DB: db, Memories: memories, Todos: todos, Topics: topics,
		LLM: llm, LLMModel: cfg.LLMFastModel,
		Prompt:        string(promptBytes),
		ExtractWindow: cfg.ExtractWindow,
		Gate:          memory.GateConfig{MinConf: cfg.QualityMinConf, TodoConf: cfg.QualityTodoConf},
	})
	flow := pipeline.Flow{Stages: []string{"asr", "segment", "extract"}}
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
		Memories: memories, Todos: todos,
	})
	api.RegisterMemory(r, &api.MemoryHandler{Memories: memories, Topics: topics})
	api.RegisterTodo(r, &api.TodoHandler{Todos: todos})
	api.RegisterTopic(r, &api.TopicHandler{Topics: topics, Memories: memories, Todos: todos})

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
