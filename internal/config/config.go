// Package config 从环境变量读取配置，全部有默认值，除 ARK_API_KEY 外可不设置。
package config

import (
	"errors"
	"os"
	"strconv"
)

type Config struct {
	Port     string // HTTP 监听端口
	DataDir  string // 音频文件存储目录
	MySQLDSN string // MySQL 连接串

	ARKAPIKey   string // 火山方舟 API Key（必填，LLM 用）
	ARKBaseURL  string // Ark OpenAI 兼容接口地址
	ASREndpoint string // ASR WebSocket 地址

	StepFunAPIKey      string // StepFun API Key（ASR 用，来自 .env）
	StepFunASREndpoint string // stepaudio realtime 端点

	// File ASR（异步，原生 diarization + ms 时间戳）
	StepFunASRFileAPIKey string // File ASR 专用 Key（STEPFUN_ASR_FILE_API_KEY）
	StepFunASRBase       string // File ASR 基址（ZW_STEPFUN_ASR_BASE，默认 https://api.c.ibasemind.com/v1）
	StepFunASRModel      string // stepaudio-2.5-asr（ZW_STEPFUN_ASR_MODEL）
	ASRProvider          string // realtime | file（ZW_ASR_PROVIDER，默认 file）

	LLMFastModel   string // Tier1：抽取/分类
	LLMStrongModel string // Tier2：Agent/Review
	EmbedModel     string
	EmbedAPIKey    string // ARK_AUDIO_API_KEY：向量端点专用 key（≠ ARK_API_KEY；未设→不启用向量）
	EmbedBaseURL   string // ZW_EMBED_BASE_URL：向量 base，默认 https://ark.cn-beijing.volces.com/api/plan/v3
	ASRModel       string // Ark 上的 ASR 模型；若账号需要 endpoint 形式（ep-xxx），直接配成 endpoint id

	// ---- Sprint 2：抽取参数（见 Sprint 2 设计文档 §3） ----
	ExtractWindow   int     // 抽取窗口切分大小（对话块数），超过则分多次 LLM 调用
	QualityMinConf  float64 // 质量闸门：候选最低置信度，低于丢弃
	QualityTodoConf float64 // todo 直接入库为 confirmed 的置信度阈值，低于降级 suggested

	// ---- speaker stage（说话人声纹）----
	TOSAccessKey            string  // 火山引擎 TOS AK（环境变量 TOS_ACCESS_KEY）
	TOSSecretKey            string  // 火山引擎 TOS SK（环境变量 TOS_SECRET_KEY）
	TOSRegion               string  // cn-shanghai
	TOSBucket               string  // user-growth
	TOSEndpoint             string  // tos-cn-shanghai.volces.com
	TOSKeyPrefix            string  // zhiwei/
	VoiceprintSidecarURL    string  // 声纹 sidecar 地址（http://127.0.0.1:8010）
	VoiceprintThreshold     float64 // 1:N 余弦匹配阈值，低于则视为未命中→自动登记
	VoiceprintCorrectMargin float64 // 幽灵历史声纹纠正领先幅度门槛，0→默认 0.06
	EnrollMinDurationMS     int64   // 从转写段音频录入声纹的最小时长（毫秒，默认 3000=3s；WeSpeaker LM 对 >3s 更稳）

	// ---- Agent / Chatbot（P1；设计见 agent-chatbot spec §14）----
	AgentEnabled         bool   // ZW_AGENT_ENABLED，关掉则不 spawn dsh（报告等仍可用）
	AgentModel           string // ZW_AGENT_MODEL：Ark 上的 DeepSeek 模型/endpoint id（agent 与报告/抽取共用）
	AgentSidecarCmd      string // ZW_AGENT_SIDECAR_CMD：dsh 边车启动命令
	AgentCordisConfig    string // ZW_AGENT_CORDIS_CONFIG：cordis.yml 基模板路径（生成文件的输入，不直接给 dsh 用）
	AgentCordisGenerated string // ZW_AGENT_CORDIS_GENERATED：生成的 cordis 配置路径（基模板 + 启用的外部 MCP 块；dsh 实际读它）
	AgentMCPURL          string // ZW_AGENT_MCP_URL：供 cordis.yml 连回的 MCP-HTTP 地址
	AgentSkillRoot       string // ZW_AGENT_SKILL_ROOT：技能磁盘根（enabled/ + disabled/ 子目录），dsh 热加载源
	DSHSessionRoot       string // DSH_SESSION_ROOT：dsh 内部会话日志目录
	DSHSystemPrompt      string // DSH_SYSTEM_PROMPT：dsh 进程级人设
	AgentRetrieveTopK    int    // ZW_AGENT_RETRIEVE_TOPK：上下文头检索种子条数
	AgentMaxUsers        int    // ZW_AGENT_MAX_USERS：每用户 dsh 进程池上限（超出按 LRU 关最久未用，默认 8）
	ReviewDailyCron      string // ZW_REVIEW_DAILY_CRON：日报定时

	// ---- 报告漫画（P4）----
	ComicEnabled bool   // ZW_COMIC_ENABLED：是否启用报告漫画（默认 false）
	ComicModel   string // ZW_COMIC_MODEL：文生图模型（默认 doubao-seedream-4-0-250828）

	// ---- speakername stage（名字推断，main 合入）----
	NameInferWindowMin   int // 名字推断上下文回看窗口（分钟，ZW_NAME_INFER_WINDOW_MIN，默认 10）
	NameInferMaxSegments int // 名字推断上下文段数上限（ZW_NAME_INFER_MAX_SEGMENTS，默认 400）

	// ---- audioscene stage（P1 音频场景与情绪理解）----
	AudioInsightEnabled  bool   // ZW_AUDIO_INSIGHT_ENABLED：是否启用音频洞察阶段（默认 true）
	AudioInsightModel    string // ZW_AUDIO_INSIGHT_MODEL：音频理解模型（默认 stepaudio-2.5-chat）
	AudioInsightBase     string // ZW_AUDIO_INSIGHT_BASE：接口基址（默认代理 https://api.c.ibasemind.com/v1）
	AudioInsightAPIKey   string // ZW_AUDIO_INSIGHT_API_KEY：API Key（默认回退 STEPFUN_ASR_FILE_API_KEY；为空则不装配 provider→stage no-op）
	AudioInsightChunkSec int    // ZW_AUDIO_INSIGHT_CHUNK_SEC：分块识别的时长阈值（秒，默认 600）

	// ---- emotionprofile stage（P2 人物情绪汇总）----
	EmotionProfileEnabled bool // ZW_EMOTION_PROFILE_ENABLED：是否启用人物情绪汇总阶段（默认 true）

	// ---- profile stage（用户画像 P1）----
	ProfileAutoConfidence float64 // ZW_PROFILE_AUTO_CONFIDENCE：LLM 抽取自动写入 active 的置信阈值（默认 0.75）
	ProfileExtractEnabled bool    // ZW_PROFILE_EXTRACT_ENABLED：是否启用 profile 流水线阶段（默认 true）
	ProfileExtractWindow  int     // ZW_PROFILE_EXTRACT_WINDOW：抽取窗口大小（对话块数，默认 10）

	// ---- correct stage（ASR 实体纠错）----
	EntityCorrectEnabled bool    // ZW_ENTITY_CORRECT_ENABLED：是否启用 correct stage（默认 true）
	EntityCorrectWindow  int     // ZW_ENTITY_CORRECT_WINDOW：LLM 上下文前后段数（默认 2）
	EntityCorrectTopK    int     // ZW_ENTITY_CORRECT_TOPK：召回 Top-K（默认 5）
	EntityCorrectMinSim  float64 // ZW_ENTITY_CORRECT_MIN_SIM：召回相似度下限 [0,1]（默认 0.6）
	EntityCorrectMaxLLM  int     // ZW_ENTITY_CORRECT_MAX_LLM：逐段 LLM 调用的会话级上限（默认 500）

	// ---- 多用户鉴权（阶段1：cookie+session）----
	OwnerPassword  string // ZW_OWNER_PASSWORD：首启引导 owner(id=1) 口令（其 password_hash 空时用它设置）
	CookieSecure   bool   // ZW_COOKIE_SECURE：session cookie 是否 Secure（默认 true；本地 http 调试可设 false）
	SessionTTLDays int    // ZW_SESSION_TTL_DAYS：会话有效期天数（默认 30）
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func Load() (*Config, error) {
	key := os.Getenv("ARK_API_KEY")
	if key == "" {
		return nil, errors.New("ARK_API_KEY 未设置")
	}
	return &Config{
		Port:          getenv("ZW_PORT", "8080"),
		DataDir:       getenv("ZW_DATA_DIR", "./data"),
		MySQLDSN:      getenv("ZW_MYSQL_DSN", "zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei?parseTime=true&charset=utf8mb4"),
		ARKAPIKey:     key,
		ARKBaseURL:    getenv("ZW_ARK_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3"),
		StepFunAPIKey: os.Getenv("STEPFUN_API_KEY"),
		StepFunASREndpoint: getenv("ZW_STEPFUN_ASR_ENDPOINT",
			"wss://api.stepfun.com/step_plan/v1/realtime?model=stepaudio-2.5-realtime"),
		// Ark 实测（2026-08-18）：本账号仅 doubao-seed-1-6-flash-250828 可用；
		// 强模型与 embedding 需控制台开通后用环境变量覆盖。
		LLMFastModel:   getenv("ZW_LLM_FAST", "doubao-seed-1-6-flash-250828"),
		LLMStrongModel: getenv("ZW_LLM_STRONG", "doubao-seed-1-6-flash-250828"),
		EmbedModel:     getenv("ZW_EMBED_MODEL", "doubao-embedding-vision"),
		// 向量检索用独立 Ark 账号（实测确认）：key 来自 ARK_AUDIO_API_KEY（≠ ARK_API_KEY），
		// 为空时上层不启用向量检索；base 注意是 /api/plan/v3，不是 /api/v3。
		EmbedAPIKey:  os.Getenv("ARK_AUDIO_API_KEY"),
		EmbedBaseURL: getenv("ZW_EMBED_BASE_URL", "https://ark.cn-beijing.volces.com/api/plan/v3"),
		ASRModel:     getenv("ZW_ASR_MODEL", "doubao-seed-asr-2-0"),

		// ---- Sprint 2：抽取参数 ----
		ExtractWindow:   getenvInt("ZW_EXTRACT_WINDOW", 10),
		QualityMinConf:  getenvFloat("ZW_QUALITY_MIN_CONF", 0.6),
		QualityTodoConf: getenvFloat("ZW_QUALITY_TODO_CONF", 0.85),

		// ---- speaker stage ----
		TOSAccessKey:         os.Getenv("TOS_ACCESS_KEY"),
		TOSSecretKey:         os.Getenv("TOS_SECRET_KEY"),
		TOSRegion:            getenv("ZW_TOS_REGION", "cn-shanghai"),
		TOSBucket:            getenv("ZW_TOS_BUCKET", "user-growth"),
		TOSEndpoint:          getenv("ZW_TOS_ENDPOINT", "tos-cn-shanghai.volces.com"),
		TOSKeyPrefix:         getenv("ZW_TOS_KEY_PREFIX", "zhiwei/"),
		StepFunASRFileAPIKey: os.Getenv("STEPFUN_ASR_FILE_API_KEY"),
		// 开发环境默认 https://api.c.ibasemind.com/v1；生产设 ZW_STEPFUN_ASR_BASE=https://api.stepfun.com/v1
		StepFunASRBase:          getenv("ZW_STEPFUN_ASR_BASE", "https://api.c.ibasemind.com/v1"),
		StepFunASRModel:         getenv("ZW_STEPFUN_ASR_MODEL", "stepaudio-2.5-asr"),
		ASRProvider:             getenv("ZW_ASR_PROVIDER", "file"),
		VoiceprintSidecarURL:    getenv("ZW_VOICEPRINT_SIDECAR_URL", "http://127.0.0.1:8010"),
		VoiceprintThreshold:     getenvFloat("ZW_VOICEPRINT_THRESHOLD", 0.8),
		VoiceprintCorrectMargin: getenvFloat("ZW_VOICEPRINT_CORRECT_MARGIN", 0.06),
		EnrollMinDurationMS:     int64(getenvInt("ZW_ENROLL_MIN_DURATION_MS", 3000)),

		// ---- Agent / Chatbot ----
		AgentEnabled:         getenvBool("ZW_AGENT_ENABLED", true),
		AgentModel:           getenv("ZW_AGENT_MODEL", ""),
		AgentSidecarCmd:      getenv("ZW_AGENT_SIDECAR_CMD", "node services/agent-sidecar/node_modules/.bin/dsh-jsonrpc-agent"),
		AgentCordisConfig:    getenv("ZW_AGENT_CORDIS_CONFIG", "services/agent-sidecar/cordis.agent.yml"),
		AgentCordisGenerated: getenv("ZW_AGENT_CORDIS_GENERATED", "services/agent-sidecar/cordis.generated.yml"),
		AgentMCPURL:          getenv("ZW_AGENT_MCP_URL", "http://127.0.0.1:8080/internal/mcp"),
		AgentSkillRoot:       getenv("ZW_AGENT_SKILL_ROOT", "./data/agent-skills"),
		DSHSessionRoot:       getenv("DSH_SESSION_ROOT", "./data/dsh-sessions"),
		DSHSystemPrompt: getenv("DSH_SYSTEM_PROMPT", `你是知微(zhiwei)，用户的个人助理，用简体中文亲切、简洁地回答。
请按问题类型分场景处理：
1) 一般知识、专业术语、名词解释、常识等问题：直接基于你自己的知识回答，不要调用读取用户数据的工具，也不要生硬地关联到用户的记忆或指标。
2) 只有问题明确关于用户本人（含「我/我的」或涉及其日程/记录/指标/待办等）时，才调用工具读取该用户的数据作答。
3) 不确定或不懂时：如实说明，不要编造，也不要用用户的数据拼凑答案。
只有在需要用户本人数据时才调用工具；不要臆测用户没有的记忆或数据。`),
		AgentRetrieveTopK: getenvInt("ZW_AGENT_RETRIEVE_TOPK", 10),
		AgentMaxUsers:     getenvInt("ZW_AGENT_MAX_USERS", 8),
		ReviewDailyCron:   getenv("ZW_REVIEW_DAILY_CRON", "0 22 * * *"),

		// ---- 报告漫画（P4）----
		ComicEnabled: getenvBool("ZW_COMIC_ENABLED", false),
		ComicModel:   getenv("ZW_COMIC_MODEL", "doubao-seedream-4-0-250828"),

		// ---- speakername stage ----
		NameInferWindowMin:   getenvInt("ZW_NAME_INFER_WINDOW_MIN", 10),
		NameInferMaxSegments: getenvInt("ZW_NAME_INFER_MAX_SEGMENTS", 400),

		// ---- audioscene stage（P1 音频场景与情绪理解）----
		// key 默认回退 STEPFUN_ASR_FILE_API_KEY（StepFun 主账号欠费，走代理）；为空则上层不装配 provider→stage no-op。
		AudioInsightEnabled:  getenvBool("ZW_AUDIO_INSIGHT_ENABLED", true),
		AudioInsightModel:    getenv("ZW_AUDIO_INSIGHT_MODEL", "stepaudio-2.5-chat"),
		AudioInsightBase:     getenv("ZW_AUDIO_INSIGHT_BASE", "https://api.c.ibasemind.com/v1"),
		AudioInsightAPIKey:   getenv("ZW_AUDIO_INSIGHT_API_KEY", os.Getenv("STEPFUN_ASR_FILE_API_KEY")),
		AudioInsightChunkSec: getenvInt("ZW_AUDIO_INSIGHT_CHUNK_SEC", 600),

		// ---- emotionprofile stage（P2 人物情绪汇总）----
		EmotionProfileEnabled: getenvBool("ZW_EMOTION_PROFILE_ENABLED", true),

		// ---- profile stage ----
		ProfileAutoConfidence: getenvFloat("ZW_PROFILE_AUTO_CONFIDENCE", 0.75),
		ProfileExtractEnabled: getenvBool("ZW_PROFILE_EXTRACT_ENABLED", true),
		ProfileExtractWindow:  getenvInt("ZW_PROFILE_EXTRACT_WINDOW", 10),

		// ---- correct stage（ASR 实体纠错）----
		EntityCorrectEnabled: getenvBool("ZW_ENTITY_CORRECT_ENABLED", true),
		EntityCorrectWindow:  getenvInt("ZW_ENTITY_CORRECT_WINDOW", 2),
		EntityCorrectTopK:    getenvInt("ZW_ENTITY_CORRECT_TOPK", 5),
		EntityCorrectMinSim:  getenvFloat("ZW_ENTITY_CORRECT_MIN_SIM", 0.6),
		EntityCorrectMaxLLM:  getenvInt("ZW_ENTITY_CORRECT_MAX_LLM", 500),

		// ---- 多用户鉴权（阶段1）----
		OwnerPassword:  os.Getenv("ZW_OWNER_PASSWORD"),
		CookieSecure:   getenvBool("ZW_COOKIE_SECURE", true),
		SessionTTLDays: getenvInt("ZW_SESSION_TTL_DAYS", 30),
	}, nil
}

// getenvInt 读取整型环境变量；值为正整数时返回，否则返回默认值。
func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// getenvFloat 读取 [0,1] 区间的浮点环境变量；越界或解析失败返回默认值。
func getenvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			return f
		}
	}
	return def
}

// getenvBool 读取布尔环境变量；用 strconv.ParseBool 解析（接受 1/t/T/TRUE/true/False 等），
// 解析失败或未设置返回默认值——与 getenvInt/getenvFloat 的回退语义一致。
func getenvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
