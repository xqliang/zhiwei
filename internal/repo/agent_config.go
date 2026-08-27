package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

// AgentConfig 是知微 agent 的人设配置（全局单份，行 id 恒为 1）：
// Identity 身份定位（名字/角色/职责边界）、Soul 性格与说话风格。
// 供每轮注入到「发给 dsh 的文本」前（不改进程级 persona、不重启 dsh，编辑即时生效）。
type AgentConfig struct {
	Identity  string    `db:"identity" json:"identity"`
	Soul      string    `db:"soul" json:"soul"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

type AgentConfigRepo struct{ DB *sqlx.DB }

// Get 读全局人设配置；从未配置（无行）时返回空 identity/soul 的零值配置而非错误
//（调用方据此退化为进程级 persona，不注入人设前言）。
func (r *AgentConfigRepo) Get(ctx context.Context) (*AgentConfig, error) {
	var c AgentConfig
	err := r.DB.GetContext(ctx, &c, `SELECT identity, soul, updated_at FROM agent_config WHERE id = 1`)
	if errors.Is(err, sql.ErrNoRows) {
		return &AgentConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Upsert 写全局人设（单例 id=1，存在即更新 identity/soul，updated_at 由 DB 自动刷新）。
func (r *AgentConfigRepo) Upsert(ctx context.Context, identity, soul string) error {
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO agent_config (id, identity, soul) VALUES (1, ?, ?)
ON DUPLICATE KEY UPDATE identity = VALUES(identity), soul = VALUES(soul)`, identity, soul)
	return err
}
