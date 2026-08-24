package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentConversation 是一次「问知微」对话；映射到 dsh 的 sessionId（重启可换新）。
type AgentConversation struct {
	ID           ids.ID    `db:"id" json:"id"`
	UserID       int64     `db:"user_id" json:"user_id"`
	Title        string    `db:"title" json:"title"`
	DSHSessionID string    `db:"dsh_session_id" json:"dsh_session_id"`
	Status       string    `db:"status" json:"status"` // active|archived
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	LastActiveAt time.Time `db:"last_active_at" json:"last_active_at"`
}

type AgentConversationRepo struct{ DB *sqlx.DB }

// Create 新建会话：生成雪花 ID，UserID 默认 1，DSHSessionID 为空时回退成会话 ID 字符串。
func (r *AgentConversationRepo) Create(ctx context.Context, c *AgentConversation) error {
	c.ID = ids.New()
	if c.UserID == 0 {
		c.UserID = 1
	}
	if c.DSHSessionID == "" {
		c.DSHSessionID = c.ID.String()
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO agent_conversation (id, user_id, title, dsh_session_id)
VALUES (:id, :user_id, :title, :dsh_session_id)`, c)
	return err
}

func (r *AgentConversationRepo) Get(ctx context.Context, id ids.ID) (*AgentConversation, error) {
	var c AgentConversation
	err := r.DB.GetContext(ctx, &c, `SELECT * FROM agent_conversation WHERE id = ?`, id.Int64())
	return &c, err
}

// List 返回某用户的活跃会话，最近活跃优先。
func (r *AgentConversationRepo) List(ctx context.Context, userID int64) ([]AgentConversation, error) {
	var rows []AgentConversation
	err := r.DB.SelectContext(ctx, &rows, `
SELECT * FROM agent_conversation
WHERE user_id = ? AND status = 'active'
ORDER BY last_active_at DESC LIMIT 200`, userID)
	return rows, err
}

// Touch 刷新 last_active_at（每轮对话后调用）。不存在返回 nil（UPDATE 0 行）。
func (r *AgentConversationRepo) Touch(ctx context.Context, id ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE agent_conversation SET last_active_at = CURRENT_TIMESTAMP(3) WHERE id = ?`, id.Int64())
	return err
}

// SetDSHSession 更新映射的 dsh sessionId（边车重启后换新 session 时用）。
func (r *AgentConversationRepo) SetDSHSession(ctx context.Context, id ids.ID, dshSessionID string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE agent_conversation SET dsh_session_id = ? WHERE id = ?`, dshSessionID, id.Int64())
	return err
}
