// zhiwei-server 是知微云端 MVP 的唯一入口：HTTP API + 异步 pipeline worker。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"zhiwei/internal/api"
	"zhiwei/internal/config"
	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/pipeline"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/storage"
	"zhiwei/internal/voiceprint"
)

// promptPath 是抽取 prompt 的版本化文件路径；版本号 = 去掉扩展名的文件名
// （如 extraction_v1），运行时从文件名推导并写进 job.trace。
const promptPath = "prompts/extraction_v3.md"

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := ids.Init(1); err != nil {
		log.Fatal(err)
	}
	// ASR key 校验：file 用 STEPFUN_ASR_FILE_API_KEY，realtime 用 STEPFUN_API_KEY
	if cfg.ASRProvider == "realtime" {
		if cfg.StepFunAPIKey == "" {
			log.Fatal("STEPFUN_API_KEY 未设置（ZW_ASR_PROVIDER=realtime）。请先 source .env")
		}
	} else {
		if cfg.StepFunASRFileAPIKey == "" {
			log.Fatal("STEPFUN_ASR_FILE_API_KEY 未设置。请先 source .env")
		}
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
	memoryTopics := &repo.MemoryTopicRepo{DB: db}
	todoTopics := &repo.TodoTopicRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}

	// 抽取 prompt（版本化文件，运行时读取；版本号见文件名与文件首行）
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		log.Fatal("读取抽取 prompt 失败: ", err)
	}
	// prompt 版本从文件名推导（extraction_v1.md → extraction_v1），写 job.trace 用
	promptVersion := strings.TrimSuffix(filepath.Base(promptPath), ".md")

	// topic 合并 prompt（版本化文件，Consolidate handler 用；版本号见文件名）
	consolidateBytes, err := os.ReadFile("prompts/topic_consolidate_v1.md")
	if err != nil {
		log.Fatal("读取合并 prompt 失败: ", err)
	}

	// memory 整理 prompt（版本化文件，MemoryHandler.Consolidate 用）
	memoryConsolidateBytes, err := os.ReadFile("prompts/memory_consolidate_v1.md")
	if err != nil {
		log.Fatal("读取记忆整理 prompt 失败: ", err)
	}

	// pipeline 装配：ASR 默认 file（StepFun 异步文件 ASR，原生 diarization + ms 时间戳）。
	// ZW_ASR_PROVIDER=realtime 切回 WebSocket 方案（免 TOS、靠 prompt diarization）。
	// File ASR 需 TOS 上传音频换公网 URL + STEPFUN_ASR_FILE_API_KEY。
	var asr provider.ASRProvider
	switch cfg.ASRProvider {
	case "realtime":
		asr = provider.NewStepFunASR(cfg.StepFunASREndpoint, cfg.StepFunAPIKey)
	default: // file
		if cfg.TOSAccessKey == "" || cfg.TOSSecretKey == "" {
			log.Fatal("ASR_PROVIDER=file 需 TOS_ACCESS_KEY/TOS_SECRET_KEY 上传音频换公网 URL")
		}
		if cfg.StepFunASRFileAPIKey == "" {
			log.Fatal("ASR_PROVIDER=file 需 STEPFUN_ASR_FILE_API_KEY（.env 中配置）")
		}
		tosClient, err := storage.NewTOSClient(storage.TOSConfig{
			AccessKey: cfg.TOSAccessKey, SecretKey: cfg.TOSSecretKey,
			Region: cfg.TOSRegion, Bucket: cfg.TOSBucket, Endpoint: cfg.TOSEndpoint, KeyPrefix: cfg.TOSKeyPrefix,
		})
		if err != nil {
			log.Fatal("TOS 客户端构造: ", err)
		}
		asr = provider.NewStepFunFileASR(cfg.StepFunASRBase, cfg.StepFunASRFileAPIKey, cfg.StepFunASRModel, tosClient, nil)
	}
	voiceprintCli := voiceprint.NewClient(cfg.VoiceprintSidecarURL)
	llm := provider.NewArkLLM(cfg.ARKBaseURL, cfg.ARKAPIKey)
	stages := pipeline.BuildStages(pipeline.StageDeps{
		Sessions: sessions, Transcripts: transcripts, ASR: asr, DataDir: cfg.DataDir,
		DB: db, Memories: memories, Todos: todos, Topics: topics,
		MemoryTopics: memoryTopics, TodoTopics: todoTopics,
		LLM: llm, LLMModel: cfg.LLMFastModel,
		Prompt:        string(promptBytes),
		PromptVersion: promptVersion,
		ExtractWindow: cfg.ExtractWindow,
		Gate:          memory.GateConfig{MinConf: cfg.QualityMinConf, TodoConf: cfg.QualityTodoConf},
		Voiceprint:    voiceprintCli, Speakers: speakers, VoiceprintThreshold: cfg.VoiceprintThreshold,
	})
	flow := pipeline.Flow{Stages: []string{"asr", "segment", "speaker", "extract"}}
	pool := pipeline.NewPool(jobs, flow, stages)
	pool.OnDone(func(ctx context.Context, sid ids.ID) {
		_ = sessions.UpdateStatus(ctx, sid, "completed")
	})
	pool.OnFail(func(ctx context.Context, sid ids.ID) {
		_ = sessions.UpdateStatus(ctx, sid, "failed")
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool.Start(ctx)

	r := api.NewRouter()
	api.RegisterAudio(r, sessions, jobs, cfg.DataDir)
	api.RegisterQuery(r, &api.QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers,
	})
	api.RegisterSpeaker(r, &api.SpeakerHandler{
		Speakers: speakers, Transcripts: transcripts,
		Voiceprint: voiceprintCli, DataDir: cfg.DataDir,
		EnrollMinDurationMS: cfg.EnrollMinDurationMS,
		VoiceprintThreshold: cfg.VoiceprintThreshold,
	})
	api.RegisterMemory(r, &api.MemoryHandler{
		Memories: memories, Topics: topics, MemoryTopics: memoryTopics,
		LLM: llm, LLMModel: cfg.LLMFastModel, ConsolidatePrompt: string(memoryConsolidateBytes),
	})
	api.RegisterTodo(r, &api.TodoHandler{Todos: todos, TodoTopics: todoTopics, Topics: topics})
	api.RegisterTopic(r, &api.TopicHandler{
		Topics: topics, Memories: memories, Todos: todos,
		LLM: llm, LLMModel: cfg.LLMFastModel, ConsolidatePrompt: string(consolidateBytes),
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
