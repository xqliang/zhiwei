package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentConversation 是一次「问知微」对话；映射到 dsh 的 sessionId（重启可换新）。
type AgentConversation struct {
	ID           ids.ID    `db:"id" json:"id"`
	UserID       int64     `db:"user_id" json:"user_id"`
	Title        string    `db:"title" json:"title"`
	TitleSource  string    `db:"title_source" json:"title_source"` // ''|manual|auto
	DSHSessionID string    `db:"dsh_session_id" json:"dsh_session_id"`
	Status       string    `db:"status" json:"status"` // active|archived
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	LastActiveAt time.Time `db:"last_active_at" json:"last_active_at"`
}

type AgentConversationRepo struct{ DB *sqlx.DB }

// Create 新建会话：生成雪花 ID，UserID 默认 1，DSHSessionID 为空时回退成会话 ID 字符串。
// 注意：DB 默认列（status/created_at/last_active_at）不会回填到 c，需要读回请 Create 后再 Get；且预设 c.Status 会被忽略（INSERT 未含该列）。
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

// Get 按 id 查会话，并强制 user_id 隔离（多租户越权防护）：SQL 追加 AND user_id = ?，
// 用 userID 读他人会话时命中 0 行，沿用 GetContext 既有语义返回 sql.ErrNoRows（handler 转 404）。
func (r *AgentConversationRepo) Get(ctx context.Context, userID int64, id ids.ID) (*AgentConversation, error) {
	var c AgentConversation
	err := r.DB.GetContext(ctx, &c, `SELECT * FROM agent_conversation WHERE id = ? AND user_id = ?`, id.Int64(), userID)
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

// SetDSHSession 更新映射的 dsh sessionId（边车重启后换新 session 时用）。不存在返回 nil（UPDATE 0 行）。
func (r *AgentConversationRepo) SetDSHSession(ctx context.Context, id ids.ID, dshSessionID string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE agent_conversation SET dsh_session_id = ? WHERE id = ?`, dshSessionID, id.Int64())
	return err
}

// UpdateTitle 改标题并标记来源（manual|auto）。行级 user_id 过滤（IDOR）：越权/不存在 → 0 行 → ErrNoRows。
func (r *AgentConversationRepo) UpdateTitle(ctx context.Context, userID int64, id ids.ID, title, source string) error {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE agent_conversation SET title=?, title_source=? WHERE id=? AND user_id=?`,
		title, source, id.Int64(), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// TitleState 取标题与来源（自动生成判定用）。行级 user_id 过滤：越权/不存在 → ErrNoRows。
func (r *AgentConversationRepo) TitleState(ctx context.Context, userID int64, id ids.ID) (title, source string, err error) {
	err = r.DB.QueryRowContext(ctx,
		`SELECT title, title_source FROM agent_conversation WHERE id=? AND user_id=?`,
		id.Int64(), userID).Scan(&title, &source)
	return title, source, err
}

// Archive 软删除：status→archived。幂等（已是 archived 则 0 行、返回 nil，不报错）。
// 行级 user_id 过滤：越权行 n=0 同样返回 nil（软删语义无「不存在即错」）。
func (r *AgentConversationRepo) Archive(ctx context.Context, userID int64, id ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE agent_conversation SET status='archived' WHERE id=? AND user_id=? AND status='active'`,
		id.Int64(), userID)
	return err
}
