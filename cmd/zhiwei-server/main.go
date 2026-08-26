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

	"zhiwei/internal/agent"
	"zhiwei/internal/api"
	"zhiwei/internal/config"
	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/pipeline"
	"zhiwei/internal/profile"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/review"
	"zhiwei/internal/storage"
	"zhiwei/internal/voiceprint"
)

// promptPath 是抽取 prompt 的版本化文件路径；版本号 = 去掉扩展名的文件名
// （如 extraction_v1），运行时从文件名推导并写进 job.trace。
const promptPath = "prompts/extraction_v3.md"

// nameInferPromptPath 说话人名字推断 prompt（speakername stage 用，版本号见文件名）。
const nameInferPromptPath = "prompts/speaker_naming_v1.md"

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
	reviews := &repo.ReviewRepo{DB: db}
	topicStatuses := &repo.TopicStatusRepo{DB: db}
	nameCandidates := &repo.SpeakerNameCandidateRepo{DB: db}

	persons := &repo.PersonRepo{DB: db}
	personAttrs := &repo.PersonAttributeRepo{DB: db}
	personRels := &repo.PersonRelationshipRepo{DB: db}
	personEvents := &repo.PersonEventRepo{DB: db}
	personLogs := &repo.PersonChangeLogRepo{DB: db}
	// 画像回填：owner「我」+ speaker→person（幂等，见 repo.EnsurePersonBootstrap）
	if err := repo.EnsurePersonBootstrap(context.Background(), persons, speakers); err != nil {
		log.Fatal("画像 bootstrap 失败: ", err)
	}

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

	// 报告 prompt（日/周/话题状态）+ 对话转记忆 prompt（版本化文件；版本号见文件名）。
	reviewDailyBytes, err := os.ReadFile("prompts/review_daily_v1.md")
	if err != nil {
		log.Fatal("读取日报 prompt 失败: ", err)
	}
	reviewWeeklyBytes, err := os.ReadFile("prompts/review_weekly_v1.md")
	if err != nil {
		log.Fatal("读取周报 prompt 失败: ", err)
	}
	topicStatusBytes, err := os.ReadFile("prompts/topic_status_v1.md")
	if err != nil {
		log.Fatal("读取话题状态 prompt 失败: ", err)
	}
	convExtractBytes, err := os.ReadFile("prompts/conversation_extraction_v1.md")
	if err != nil {
		log.Fatal("读取对话转记忆 prompt 失败: ", err)
	}

	// 说话人名字推断 prompt（版本化文件，speakername stage 用）
	nameInferBytes, err := os.ReadFile(nameInferPromptPath)
	if err != nil {
		log.Fatal("读取名字推断 prompt 失败: ", err)
	}

	// 画像抽取 prompt（版本化文件；版本号见文件名）
	profilePromptBytes, err := os.ReadFile("prompts/profile_extraction_v2.md")
	if err != nil {
		log.Fatal("读取画像抽取 prompt 失败: ", err)
	}
	profilePromptVersion := strings.TrimSuffix(filepath.Base("prompts/profile_extraction_v2.md"), ".md")

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
	// Agent / 报告共用模型：ZW_AGENT_MODEL 优先，未配则回退强模型（见 config §14）。
	agentModel := cfg.AgentModel
	if agentModel == "" {
		agentModel = cfg.LLMStrongModel
	}
	// 报告引擎（日/周报 + 话题状态）：直连 Ark LLM（非 dsh 边车），复用现有 repo。
	// 用更长超时的客户端——报告是大 prompt + 结构化长输出，默认 60s 会 context deadline exceeded。
	reviewLLM := provider.NewArkLLMForReports(cfg.ARKBaseURL, cfg.ARKAPIKey)
	reviewer := review.NewGenerator(reviewLLM, agentModel,
		string(reviewDailyBytes), "review_daily_v1",
		string(reviewWeeklyBytes), "review_weekly_v1",
		string(topicStatusBytes), "topic_status_v1",
		reviews, topicStatuses, memories, todos, topics, sessions, transcripts)
	profileSvc := &profile.Service{
		DB: db, Sessions: sessions, Transcripts: transcripts, Memories: memories,
		Speakers: speakers, Persons: persons, Attributes: personAttrs,
		Relationships: personRels, Events: personEvents, ChangeLogs: personLogs,
		LLM: llm, Model: cfg.LLMFastModel, Prompt: string(profilePromptBytes),
		PromptVersion: profilePromptVersion,
		Window:        cfg.ProfileExtractWindow, Gate: profile.GateConfig{AutoConf: cfg.ProfileAutoConfidence},
	}
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
		NameInferPrompt:       string(nameInferBytes),
		SpeakerNameCandidates: nameCandidates,
		NameInferWindowMin:    cfg.NameInferWindowMin,
		NameInferMaxSegments:  cfg.NameInferMaxSegments,
		Profile:               profileSvc,
	})
	// profile stage 按开关追加（ZW_PROFILE_EXTRACT_ENABLED=false 时仅手动+回填端点）
	stagesList := []string{"asr", "segment", "speaker", "speakername", "extract"}
	if cfg.ProfileExtractEnabled {
		stagesList = append(stagesList, "profile")
	}
	flow := pipeline.Flow{Stages: stagesList}
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

	// 日报 cron（内部 time.Timer；仅解析 cron 的时/分，见 review.NewScheduler 注释）。
	review.NewScheduler(reviewer, cfg.ReviewDailyCron).Start(ctx)

	r := api.NewRouter()
	api.RegisterAudio(r, sessions, jobs, cfg.DataDir)
	api.RegisterQuery(r, &api.QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers,
		SpeakerNameCandidates: nameCandidates,
	})
	api.RegisterSpeaker(r, &api.SpeakerHandler{
		Speakers: speakers, Transcripts: transcripts,
		Voiceprint: voiceprintCli, DataDir: cfg.DataDir,
		EnrollMinDurationMS:   cfg.EnrollMinDurationMS,
		VoiceprintThreshold:   cfg.VoiceprintThreshold,
		SpeakerNameCandidates: nameCandidates,
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
	// 报告 API：/api/reviews/daily|weekly（取最新或生成/强制重生）+ /api/topics/{id}/status。
	api.RegisterReviews(r, reviewer)
	api.RegisterPerson(r, &api.PersonHandler{
		Persons: persons, Attributes: personAttrs, Relationships: personRels,
		Events: personEvents, ChangeLogs: personLogs, Service: profileSvc,
	})

	// MCP 工具端点（仅供本机 dsh 边车经 streamable-http 连回；不对外）。
	// 进程内运行、复用上面已开库的 repo 实例（同一个 DB 池）。
	agentProposals := &repo.AgentProposalRepo{DB: db}
	mcpSrv := agent.NewMCPServer(agent.MCPDeps{
		Memory:     memories,
		Session:    sessions,
		Transcript: transcripts,
		Topic:      topics,
		Todo:       todos,
		Proposals:  agentProposals,
		// 画像读工具 + propose_profile_* 读现值用（P2）
		Persons:          persons,
		PersonAttributes: personAttrs,
		PersonEvents:     personEvents,
	})
	// 报告工具（generate_report / get_topic_status）注册进同一 MCP server，供 dsh agent 调用。
	review.RegisterReportTools(mcpSrv, reviewer)
	mcpHandler := agent.MCPHandler(mcpSrv)
	r.Handle("/internal/mcp", mcpHandler)
	r.Handle("/internal/mcp/*", mcpHandler)

	// 写-提议闸门人审端点：列出/确认/放弃 agent 提议（POST confirm 单事务 apply-once, spec §8）。
	agent.RegisterProposals(r, agent.ProposalDeps{
		DB: db, Proposals: agentProposals,
		Memories: memories, Topics: topics, Todos: todos, TodoTopics: todoTopics,
		// 画像确认落库（P2）：confirm 单事务经 profile.Service 的 Ext 变体写画像 + Resolve。
		Profile: profileSvc, Persons: persons,
	})

	// Agent 运行时（惰性 spawn dsh；首次对话时启动，此时 /internal/mcp 已监听）。
	if cfg.AgentEnabled {
		rt := agent.NewDSHRuntime(agent.RuntimeConfig{
			CordisConfig: cfg.AgentCordisConfig,
			Model:        agentModel, // 解析后的模型(ZW_AGENT_MODEL 空则回退 LLMStrongModel), 与报告/抽取一致(评审 I2)
			SessionRoot:  cfg.DSHSessionRoot,
			SystemPrompt: cfg.DSHSystemPrompt,
			MCPURL:       "http://127.0.0.1:" + cfg.Port + "/internal/mcp",
		})
		defer rt.Close()
		agentConvs := &repo.AgentConversationRepo{DB: db}
		agentMsgs := &repo.AgentMessageRepo{DB: db}
		// Orchestrator 装配可选的画像上下文头（每轮把 owner 概要 + 关键属性 + 当天日期前置到
		// 「发给 dsh 的文本」，让 agent 天然「认识我」；不改落库，见 agent/context.go）。
		orch := agent.NewOrchestrator(rt, agentConvs, agentMsgs)
		orch.Ctx = &agent.ProfileContext{Persons: persons, Attributes: personAttrs}
		agent.RegisterAgent(r, &agent.AgentHandler{
			Orch:          orch,
			Conversations: agentConvs,
			Messages:      agentMsgs,
		})
		// 对话转记忆端点：POST /api/agent/conversations/{cid}/extract（幂等，直连 Ark LLM）。
		agent.RegisterExtract(r, memory.ConversationExtractDeps{
			DB: db, AgentMessages: agentMsgs, Topics: topics, Memories: memories, MemoryTopics: memoryTopics,
			LLM: llm, Model: agentModel,
			Prompt: string(convExtractBytes), PromptVersion: "conversation_extraction_v1",
			Window: cfg.ExtractWindow,
			Gate:   memory.GateConfig{MinConf: cfg.QualityMinConf, TodoConf: cfg.QualityTodoConf},
		})
	}

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
