package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// AgentConfig 是知微 agent 的全局配置（单份，行 id 恒为 1）：
// Identity/Soul 人设（身份定位/性格语气，每轮注入「发给 dsh 的文本」前）；
// SearchEngine/SearchAPIKey 联网搜索配置（Phase 2，web_search 工具每次调用读最新值）。
type AgentConfig struct {
	Identity     string `db:"identity" json:"identity"`
	Soul         string `db:"soul" json:"soul"`
	SearchEngine string `db:"search_engine" json:"search_engine"` // auto|bing|duckduckgo|tavily
	// SearchAPIKey 搜索后端 API key（tavily 用）；NULL=未配置（免 key 引擎）。指针列用指针承接。
	SearchAPIKey *string   `db:"search_api_key" json:"search_api_key,omitempty"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// SearchKey 返回搜索 API key（NULL/nil → 空串），调用方免判空。
func (c *AgentConfig) SearchKey() string {
	if c == nil || c.SearchAPIKey == nil {
		return ""
	}
	return *c.SearchAPIKey
}

// normalizeEngine 空串归一为 auto（默认免 key 引擎链）。
func normalizeEngine(s string) string {
	if v := strings.TrimSpace(s); v != "" {
		return v
	}
	return "auto"
}

type AgentConfigRepo struct{ DB *sqlx.DB }

// Get 读全局人设配置；从未配置（无行）时返回空 identity/soul 的零值配置而非错误
// （调用方据此退化为进程级 persona，不注入人设前言）。
func (r *AgentConfigRepo) Get(ctx context.Context) (*AgentConfig, error) {
	var c AgentConfig
	err := r.DB.GetContext(ctx, &c, `SELECT identity, soul, search_engine, search_api_key, updated_at FROM agent_config WHERE id = 1`)
	if errors.Is(err, sql.ErrNoRows) {
		return &AgentConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Upsert 写全局配置（单例 id=1，存在即更新全部四列，updated_at 由 DB 自动刷新）。
// 调用方（putConfig）负责「读现值→指针合并」；本方法整行覆盖。
func (r *AgentConfigRepo) Upsert(ctx context.Context, c AgentConfig) error {
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO agent_config (id, identity, soul, search_engine, search_api_key)
VALUES (1, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  identity = VALUES(identity), soul = VALUES(soul),
  search_engine = VALUES(search_engine), search_api_key = VALUES(search_api_key)`,
		c.Identity, c.Soul, normalizeEngine(c.SearchEngine), c.SearchAPIKey)
	return err
}
