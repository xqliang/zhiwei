// zhiwei-server 是知微云端 MVP 的唯一入口：HTTP API + 异步 pipeline worker。
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/agent"
	"zhiwei/internal/api"
	"zhiwei/internal/auth"
	"zhiwei/internal/config"
	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/pipeline"
	"zhiwei/internal/profile"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/retrieve"
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
	speakerEmbeddings := &repo.SpeakerEmbeddingRepo{DB: db}
	// 多条声纹回填：既有 speaker.embedding（单向量时代）幂等物化成首条样本行
	// （幂等，见 repo.EnsureSpeakerEmbeddingBootstrap；迁移 000012 的 Go 侧回填）
	if n, err := speakerEmbeddings.EnsureSpeakerEmbeddingBootstrap(context.Background()); err != nil {
		log.Fatal("声纹样本 bootstrap 失败: ", err)
	} else if n > 0 {
		log.Printf("[speaker] 声纹样本回填 %d 条（speaker.embedding → 首条样本）", n)
	}

	persons := &repo.PersonRepo{DB: db}
	personAttrs := &repo.PersonAttributeRepo{DB: db}
	personRels := &repo.PersonRelationshipRepo{DB: db}
	personEvents := &repo.PersonEventRepo{DB: db}
	personMetrics := &repo.PersonMetricRepo{DB: db}
	personCycles := &repo.PersonCycleRepo{DB: db}
	personActivities := &repo.PersonActivityRepo{DB: db}
	personPets := &repo.PersonPetRepo{DB: db}
	personLogs := &repo.PersonChangeLogRepo{DB: db}
	// 画像回填：owner「我」+ speaker→person（幂等，见 repo.EnsurePersonBootstrap）
	if err := repo.EnsurePersonBootstrap(context.Background(), persons, speakers); err != nil {
		log.Fatal("画像 bootstrap 失败: ", err)
	}

	// 鉴权（阶段1：cookie+session）。owner(id=1) 口令引导：其 password_hash 为空且配了
	// ZW_OWNER_PASSWORD 时设置，便于首次登录（存量数据全 user_id=1，归 owner）。
	authStore := &auth.Store{DB: db}
	if cfg.OwnerPassword != "" {
		if u, err := authStore.GetUser(context.Background(), ids.ID(1)); err == nil && u != nil && u.PasswordHash == "" {
			if h, herr := auth.HashPassword(cfg.OwnerPassword); herr == nil {
				if err := authStore.SetPasswordHash(context.Background(), ids.ID(1), h); err != nil {
					log.Printf("[auth] owner 口令引导失败: %v", err)
				} else {
					log.Println("[auth] owner(id=1) 口令已由 ZW_OWNER_PASSWORD 引导设置")
				}
			}
		}
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
	profilePromptBytes, err := os.ReadFile("prompts/profile_extraction_v4.md")
	if err != nil {
		log.Fatal("读取画像抽取 prompt 失败: ", err)
	}
	profilePromptVersion := strings.TrimSuffix(filepath.Base("prompts/profile_extraction_v4.md"), ".md")

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
		Metrics: personMetrics, Cycles: personCycles, Activities: personActivities, Pets: personPets,
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
		VoiceprintCorrectMargin: cfg.VoiceprintCorrectMargin,
		SpeakerEmbeddings:       speakerEmbeddings,
		NameInferPrompt:         string(nameInferBytes),
		SpeakerNameCandidates:   nameCandidates,
		NameInferWindowMin:      cfg.NameInferWindowMin,
		NameInferMaxSegments:    cfg.NameInferMaxSegments,
		Profile:                 profileSvc,
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
	// 鉴权端点（登录/登出/当前用户）。这三条豁免于 authGate（自行处理未登录态）。
	sessionTTL := time.Duration(cfg.SessionTTLDays) * 24 * time.Hour
	r.Post("/api/auth/login", auth.LoginHandler(authStore, sessionTTL, cfg.CookieSecure))
	r.Post("/api/auth/logout", auth.LogoutHandler(authStore, cfg.CookieSecure))
	r.Get("/api/auth/me", auth.MeHandler(authStore))
	api.RegisterAudio(r, sessions, jobs, cfg.DataDir)
	api.RegisterQuery(r, &api.QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers,
		SpeakerNameCandidates: nameCandidates,
		VoiceprintThreshold:   cfg.VoiceprintThreshold, // timeline 列表「整段声纹」两级判定用
		SpeakerEmbeddings:     speakerEmbeddings,       // 多向量匹配（每人任意样本命中即命中）
	})
	api.RegisterSpeaker(r, &api.SpeakerHandler{
		Speakers: speakers, Transcripts: transcripts,
		Voiceprint: voiceprintCli, DataDir: cfg.DataDir,
		SpeakerEmbeddings:     speakerEmbeddings,
		EnrollMinDurationMS:   cfg.EnrollMinDurationMS,
		VoiceprintThreshold:   cfg.VoiceprintThreshold,
		SpeakerNameCandidates: nameCandidates,
		Persons:               persons,
		Service:               profileSvc,
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
		Persons: persons, Speakers: speakers, Attributes: personAttrs, Relationships: personRels,
		Events: personEvents, Metrics: personMetrics, Cycles: personCycles,
		Activities: personActivities, Pets: personPets, ChangeLogs: personLogs, Service: profileSvc,
	})

	// MCP 工具端点（仅供本机 dsh 边车经 streamable-http 连回；不对外）。
	// 进程内运行、复用上面已开库的 repo 实例（同一个 DB 池）。
	agentProposals := &repo.AgentProposalRepo{DB: db}
	// 向量检索（可选）：仅当 ARK_AUDIO_API_KEY 配置时启用（doubao-embedding-vision，实测走
	// /api/plan/v3 的 /embeddings/multimodal，见 embed_vision.go）；否则 retriever=nil，
	// search_memory 与上下文头种子整条链路降级回关键词/无种子（每个消费点都判 nil）。
	var retriever *retrieve.Retriever
	if cfg.EmbedAPIKey != "" {
		embedder := provider.NewArkVisionEmbed(cfg.EmbedBaseURL, cfg.EmbedAPIKey, cfg.EmbedModel)
		retriever = &retrieve.Retriever{Memories: memories, Embedder: embedder, TopK: cfg.AgentRetrieveTopK}
		// backfill sweep：后台 goroutine，启动先跑一次、之后每 5 分钟一次，把「active 且未嵌」的
		// 记忆补上向量（不侵入抽取事务；新记忆最迟一个 tick 内可被语义检索到）。ctx 退出即停。
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				if n, err := retriever.Backfill(context.Background(), 1, 500); err != nil {
					log.Printf("[retrieve] backfill 失败: %v", err)
				} else if n > 0 {
					log.Printf("[retrieve] backfill 回填 %d 条记忆向量", n)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	// MCP 工具依赖（进程内运行、复用上面已开库的 repo 实例；同一个 DB 池）。
	mcpDeps := agent.MCPDeps{
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
		PersonMetrics:    personMetrics,
		// 语义检索（可选，nil 则 search_memory 走关键词）
		Retrieve: retriever,
	}
	// MCP 服务管理（全局）：读基模板 + DB 启用服务 → 生成 cordis.generated.yml（dsh spawn 实际读
	// 的文件）；配置变更时重生成（给将来新 spawn）+ 对在用运行时 mcp/apply 热插拔（给当前进程）。
	mcpServerRepo := &repo.MCPServerRepo{DB: db}
	baseCordis, err := os.ReadFile(cfg.AgentCordisConfig)
	if err != nil {
		log.Fatalf("读 cordis 基模板 %s: %v", cfg.AgentCordisConfig, err)
	}
	regenCordis := func(ctx context.Context) error {
		servers, err := mcpServerRepo.Enabled(ctx)
		if err != nil {
			return err
		}
		out, err := agent.GenerateCordis(string(baseCordis), servers)
		if err != nil {
			return err
		}
		return os.WriteFile(cfg.AgentCordisGenerated, []byte(out), 0o644)
	}
	if err := regenCordis(context.Background()); err != nil {
		log.Fatalf("初次生成 cordis 配置: %v", err)
	}

	// 2B-B：每登录用户一个 dsh 运行时 + 一个 MCP token 的进程池。baseCfg 是模板——
	// CordisConfig/Model/SystemPrompt 全用户共享；MCPURL 留空、SessionRoot 作父目录，由 pool
	// 按每用户 token 派生（MCPURL=mcpBaseURL+"/"+token、SessionRoot=base/u<uid>）。cap 超出按 LRU
	// 关最久未用。pool 始终创建（MCP handler 要用它按 token 反查用户）；AgentEnabled=false 时无人
	// 调 Get、不会 spawn 任何 dsh。CordisConfig 指向【生成文件】（基模板 + 外部 MCP 块）。
	mcpBaseURL := "http://127.0.0.1:" + cfg.Port + "/internal/mcp"
	agentPool := agent.NewRuntimePool(agent.RuntimeConfig{
		CordisConfig: cfg.AgentCordisGenerated,
		Model:        agentModel, // 解析后的模型(ZW_AGENT_MODEL 空则回退 LLMStrongModel), 与报告/抽取一致
		SessionRoot:  cfg.DSHSessionRoot,
		SystemPrompt: cfg.DSHSystemPrompt,
	}, mcpBaseURL, cfg.AgentMaxUsers, func(c agent.RuntimeConfig) agent.AgentRuntime {
		return agent.NewDSHRuntime(c)
	})
	defer agentPool.Close()

	// MCP 端点：按请求路径末段的 token → userID 懒建/缓存该用户的 MCP server（2B-B 多用户隔离）。
	// 报告工具经 customize 闭包注册进每个 per-user server（供 dsh agent 调用）。
	// 注意（残留）：report 工具当前仍是单用户口径（internal/review 固定 user 1），未随 token 分用户；
	// 收敛需改 review 包，超出本步(2B-B)范围，后续处理。
	mcpHandler := agent.MCPHandler(mcpDeps, agentPool.TokenUserID, func(s *mcp.Server) {
		review.RegisterReportTools(s, reviewer)
	})
	r.Handle("/internal/mcp", mcpHandler)
	r.Handle("/internal/mcp/*", mcpHandler)

	// 写-提议闸门人审端点：列出/确认/放弃 agent 提议（POST confirm 单事务 apply-once, spec §8）。
	agent.RegisterProposals(r, agent.ProposalDeps{
		DB: db, Proposals: agentProposals,
		Memories: memories, Topics: topics, Todos: todos, TodoTopics: todoTopics,
		// 画像确认落库（P2）：confirm 单事务经 profile.Service 的 Ext 变体写画像 + Resolve。
		Profile: profileSvc, Persons: persons,
	})

	// Agent 编排（惰性 spawn dsh；首次对话时 pool.Get→首个 Prompt 启动，此时 /internal/mcp 已监听）。
	if cfg.AgentEnabled {
		agentConvs := &repo.AgentConversationRepo{DB: db}
		agentMsgs := &repo.AgentMessageRepo{DB: db}
		agentConfigs := &repo.AgentConfigRepo{DB: db}
		// Orchestrator 按 conv.UserID 经 pool.Get 选运行时（2B-B：每用户独立 dsh + MCP token）。
		// 装配可选的画像上下文头（每轮把 owner 概要 + 关键属性 + 当天日期前置到「发给 dsh 的文本」，
		// 让 agent 天然「认识我」；Head/Seeds 现按 conv.UserID 取，不改落库，见 agent/context.go）。
		orch := agent.NewOrchestrator(agentPool.Get, agentConvs, agentMsgs)
		orch.Ctx = &agent.ProfileContext{Persons: persons, Attributes: personAttrs, Retrieve: retriever}
		// 人设注入：每轮把可配置的 identity/soul 前置到发给 dsh 的文本（读全局 agent_config；空则不注入）。
		// 动态生效、不重启 dsh——配置改了下一条消息即用新人设。读失败/为空退化为进程级 persona。
		orch.Persona = func(ctx context.Context) string {
			c, err := agentConfigs.Get(ctx)
			if err != nil || c == nil {
				return ""
			}
			return agent.AssemblePersona(c.Identity, c.Soul)
		}
		agent.RegisterAgent(r, &agent.AgentHandler{
			Orch:          orch,
			Conversations: agentConvs,
			Messages:      agentMsgs,
			Configs:       agentConfigs,
			SystemPrompt:  cfg.DSHSystemPrompt, // 只读展示：进程级 persona
			Ctx:           orch.Ctx,            // 同一份 ProfileContext：算 owner 画像头供整体 prompt 预览
			MCPServers:    mcpServerRepo,       // 设置页 MCP 服务清单管理
			// MCP 配置变更生效：重生成 cordis（新 spawn）+ 对在用运行时热插拔 mcp/apply（当前
			// 进程）；热插拔失败的运行时由 pool 内部空闲兜底摘除（下一轮 respawn 读新配置）。
			OnMCPChange: func(ctx context.Context) {
				if err := regenCordis(ctx); err != nil {
					log.Printf("[agent] 重生成 cordis 失败: %v", err)
					return
				}
				rows, err := mcpServerRepo.Enabled(ctx)
				if err != nil {
					log.Printf("[agent] 读 MCP 服务清单失败: %v", err)
					return
				}
				agentPool.ApplyMCPAll(ctx, agent.SpecsFromServers(rows))
			},
		})
		// 预热 owner(id=1) 的 dsh 边车：启动后后台 spawn + initialize 握手，把 node 启动的一次性
		// 延迟从「首条消息」挪到启动阶段（best-effort：失败仅记日志，首条消息会自行懒启动）。
		go func() {
			wctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := agentPool.Get(1).Warm(wctx); err != nil {
				log.Printf("[agent] 预热 dsh 边车失败(首条消息将现启动): %v", err)
			} else {
				log.Println("[agent] dsh 边车已预热")
			}
		}()
		// 对话转记忆端点：POST /api/agent/conversations/{cid}/extract（幂等，直连 Ark LLM）。
		agent.RegisterExtract(r, memory.ConversationExtractDeps{
			DB: db, AgentMessages: agentMsgs, Topics: topics, Memories: memories, MemoryTopics: memoryTopics,
			LLM: llm, Model: agentModel,
			Prompt: string(convExtractBytes), PromptVersion: "conversation_extraction_v1",
			Window: cfg.ExtractWindow,
			Gate:   memory.GateConfig{MinConf: cfg.QualityMinConf, TodoConf: cfg.QualityTodoConf},
		})
	}

	// 会话过期清理：后台每小时删一次过期 session（GetValid 已惰性过滤，这里只做行清理）。
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := authStore.DeleteExpiredSessions(context.Background()); err == nil && n > 0 {
					log.Printf("[auth] 清理过期会话 %d 条", n)
				}
			}
		}
	}()

	// authGate 包裹整个 mux：豁免路径（健康检查/静态页/鉴权端点/内部 MCP 回连）放行，
	// 其余要求有效会话并把 userID 注入 ctx（未登录 401）。放在 http.Server 层而非 chi.Use，
	// 因业务路由在 NewRouter 之后平铺注册、Use 须先于路由（否则 chi panic）。
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: authGate(authStore, r)}
	go func() {
		log.Println("zhiwei-server listening on :" + cfg.Port)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	_ = srv.Close()
}

// authGate 用 auth 中间件包裹 mux，但豁免不需登录的路径：健康检查、静态页、鉴权端点自身、
// 内部 MCP 回连（dsh 走 loopback，不做用户鉴权）。其余路径要求有效会话（注入 userID，否则 401）。
// 放在 http.Server 层而非 chi.Use：业务路由在 NewRouter 之后平铺注册，chi 要求 Use 先于路由。
func authGate(store *auth.Store, next http.Handler) http.Handler {
	protected := auth.Middleware(store)(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// /internal/mcp[/*]：仅本机 dsh 边车经 loopback 回连，绝不对外——非环回一律 403（C1）。
		// 服务可能绑 0.0.0.0，故不能靠「豁免鉴权」等同「仅本机可达」，必须显式校验 RemoteAddr 环回。
		if p == "/internal/mcp" || strings.HasPrefix(p, "/internal/mcp/") {
			if !isLoopbackReq(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		exempt := p == "/api/health" ||
			p == "/api/auth/login" || p == "/api/auth/logout" || p == "/api/auth/me" ||
			p == "/" || p == "/index.html" ||
			strings.HasPrefix(p, "/app/")
		if exempt {
			next.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// isLoopbackReq 判定请求来自本机环回（127.0.0.0/8 或 ::1）——用于把内部 MCP 端点锁在本机。
func isLoopbackReq(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
