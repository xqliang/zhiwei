// Package config 从环境变量读取配置，全部有默认值，除 ARK_API_KEY 外可不设置。
package config

import (
	"errors"
	"os"
)

type Config struct {
	Port     string // HTTP 监听端口
	DataDir  string // 音频文件存储目录
	MySQLDSN string // MySQL 连接串

	ARKAPIKey  string // 火山方舟 API Key（必填）
	ARKBaseURL string // Ark OpenAI 兼容接口地址
	ASREndpoint string // ASR WebSocket 地址（Spike 后按需调整）

	LLMFastModel   string // Tier1：抽取/分类
	LLMStrongModel string // Tier2：Agent/Review
	EmbedModel     string
	ASRModel       string // Ark 上的 ASR 模型；若账号需要 endpoint 形式（ep-xxx），直接配成 endpoint id
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
		MySQLDSN:   getenv("ZW_MYSQL_DSN", "zhiwei:zhiwei@tcp(127.0.0.1:3306)/zhiwei?parseTime=true&charset=utf8mb4"),
		ARKAPIKey:  key,
		ARKBaseURL: getenv("ZW_ARK_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3"),
		ASREndpoint: getenv("ZW_ASR_ENDPOINT", "wss://ark.cn-beijing.volces.com/api/v3/asr"),
		// Ark 实测（2026-08-18）：本账号仅 doubao-seed-1-6-flash-250828 可用；
		// 强模型与 embedding 需控制台开通后用环境变量覆盖。
		LLMFastModel:   getenv("ZW_LLM_FAST", "doubao-seed-1-6-flash-250828"),
		LLMStrongModel: getenv("ZW_LLM_STRONG", "doubao-seed-1-6-flash-250828"),
		EmbedModel:     getenv("ZW_EMBED_MODEL", "doubao-embedding-large-text-250515"),
		ASRModel:       getenv("ZW_ASR_MODEL", "doubao-seed-asr-2-0"),
	}, nil
}
