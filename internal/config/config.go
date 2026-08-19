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

	ARKAPIKey  string // 火山方舟 API Key（必填，LLM 用）
	ARKBaseURL string // Ark OpenAI 兼容接口地址
	ASREndpoint string // ASR WebSocket 地址

	StepFunAPIKey       string // StepFun API Key（ASR 用，来自 .env）
	StepFunASREndpoint  string // stepaudio realtime 端点

	LLMFastModel   string // Tier1：抽取/分类
	LLMStrongModel string // Tier2：Agent/Review
	EmbedModel     string
	ASRModel       string // Ark 上的 ASR 模型；若账号需要 endpoint 形式（ep-xxx），直接配成 endpoint id

	// ---- Sprint 2：抽取参数（见 Sprint 2 设计文档 §3） ----
	ExtractWindow   int     // 抽取窗口切分大小（对话块数），超过则分多次 LLM 调用
	QualityMinConf  float64 // 质量闸门：候选最低置信度，低于丢弃
	QualityTodoConf float64 // todo 直接入库为 confirmed 的置信度阈值，低于降级 suggested
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
		Port:       getenv("ZW_PORT", "8080"),
		DataDir:    getenv("ZW_DATA_DIR", "./data"),
		MySQLDSN:   getenv("ZW_MYSQL_DSN", "zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei?parseTime=true&charset=utf8mb4"),
		ARKAPIKey:  key,
		ARKBaseURL: getenv("ZW_ARK_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3"),
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
