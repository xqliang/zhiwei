// Package config 从环境变量读取配置，全部有默认值，除 ARK_API_KEY 外可不设置。
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
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
	ASRModel       string // Ark 上的 ASR 模型；若账号需要 endpoint 形式（ep-xxx），直接配成 endpoint id

	// ---- Sprint 2：抽取参数（见 Sprint 2 设计文档 §3） ----
	ExtractWindow   int     // 抽取窗口切分大小（对话块数），超过则分多次 LLM 调用
	QualityMinConf  float64 // 质量闸门：候选最低置信度，低于丢弃
	QualityTodoConf float64 // todo 直接入库为 confirmed 的置信度阈值，低于降级 suggested

	// ---- speaker stage（说话人声纹）----
	TOSAccessKey         string  // 火山引擎 TOS AK（环境变量 TOS_ACCESS_KEY）
	TOSSecretKey         string  // 火山引擎 TOS SK（环境变量 TOS_SECRET_KEY）
	TOSRegion            string  // cn-shanghai
	TOSBucket            string  // user-growth
	TOSEndpoint          string  // tos-cn-shanghai.volces.com
	TOSKeyPrefix         string  // zhiwei/
	VoiceprintSidecarURL string  // 声纹 sidecar 地址（http://127.0.0.1:8010）
	VoiceprintThreshold  float64 // 1:N 余弦匹配阈值，低于则视为未命中→自动登记
	EnrollMinDurationMS  int64   // 从转写段音频录入声纹的最小时长（毫秒，默认 3000=3s；WeSpeaker LM 对 >3s 更稳）

	// ---- profile stage（用户画像 P1）----
	ProfileAutoConfidence float64 // ZW_PROFILE_AUTO_CONFIDENCE：LLM 抽取自动写入 active 的置信阈值（默认 0.75）
	ProfileExtractEnabled bool    // ZW_PROFILE_EXTRACT_ENABLED：是否启用 profile 流水线阶段（默认 true）
	ProfileExtractWindow  int     // ZW_PROFILE_EXTRACT_WINDOW：抽取窗口大小（对话块数，默认 10）
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
		EmbedModel:     getenv("ZW_EMBED_MODEL", "doubao-embedding-large-text-250515"),
		ASRModel:       getenv("ZW_ASR_MODEL", "doubao-seed-asr-2-0"),

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
		StepFunASRBase:       getenv("ZW_STEPFUN_ASR_BASE", "https://api.c.ibasemind.com/v1"),
		StepFunASRModel:      getenv("ZW_STEPFUN_ASR_MODEL", "stepaudio-2.5-asr"),
		ASRProvider:          getenv("ZW_ASR_PROVIDER", "file"),
		VoiceprintSidecarURL: getenv("ZW_VOICEPRINT_SIDECAR_URL", "http://127.0.0.1:8010"),
		VoiceprintThreshold:  getenvFloat("ZW_VOICEPRINT_THRESHOLD", 0.5),
		EnrollMinDurationMS:  int64(getenvInt("ZW_ENROLL_MIN_DURATION_MS", 3000)),

		// ---- profile stage ----
		ProfileAutoConfidence: getenvFloat("ZW_PROFILE_AUTO_CONFIDENCE", 0.75),
		ProfileExtractEnabled: getenvBool("ZW_PROFILE_EXTRACT_ENABLED", true),
		ProfileExtractWindow:  getenvInt("ZW_PROFILE_EXTRACT_WINDOW", 10),
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

// getenvBool 读取布尔环境变量：1/true/TRUE（大小写不敏感）为 true，其余为 false；
// 未设置返回默认值。
func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true")
}
