package repo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentMessage 是对话流里的一条消息（用户/助手），可携带引用与工具载荷。
// Citations/ToolPayload 是可空 JSON 列，用 *json.RawMessage（对齐 job.go 的 Trace）：
// 值类型 json.RawMessage 扫描 NULL 会报错，故可空 JSON 用指针（NULL→nil）。
type AgentMessage struct {
	ID             ids.ID           `db:"id" json:"id"`
	UserID         int64            `db:"user_id" json:"user_id"`
	ConversationID *ids.ID          `db:"conversation_id" json:"conversation_id,omitempty"`
	Role           string           `db:"role" json:"role"` // user|assistant
	Kind           string           `db:"kind" json:"kind"` // text|tool_call|tool_result|card
	Content        string           `db:"content" json:"content"`
	Citations      *json.RawMessage `db:"citations" json:"citations,omitempty"`
	ToolPayload    *json.RawMessage `db:"tool_payload" json:"tool_payload,omitempty"`
	DSHSeq         *int             `db:"dsh_seq" json:"dsh_seq,omitempty"`
	CreatedAt      time.Time        `db:"created_at" json:"created_at"`
}

type AgentMessageRepo struct{ DB *sqlx.DB }

// Append 追加一条消息：生成 ID，UserID 默认 1，Kind 空时默认 text。
func (r *AgentMessageRepo) Append(ctx context.Context, m *AgentMessage) error {
	m.ID = ids.New()
	if m.UserID == 0 {
		m.UserID = 1
	}
	if m.Kind == "" {
		m.Kind = "text"
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO agent_message (id, user_id, conversation_id, role, kind, content, citations, tool_payload, dsh_seq)
VALUES (:id, :user_id, :conversation_id, :role, :kind, :content, :citations, :tool_payload, :dsh_seq)`, m)
	return err
}

// ListByConversation 按 id 升序（= 时间顺序）返回一段对话的全部消息。
// 强制 user_id 隔离（AND user_id = ?）：即使调用方拿到他人的 convID，因消息行 user_id 不匹配
// 也只会返回空列表，防越权读到他人对话内容。
func (r *AgentMessageRepo) ListByConversation(ctx context.Context, userID int64, convID ids.ID) ([]AgentMessage, error) {
	var rows []AgentMessage
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM agent_message WHERE conversation_id = ? AND user_id = ? ORDER BY id ASC`, convID.Int64(), userID)
	return rows, err
}

// CountByConversation 统计某会话的 user 消息数（判定是否到第 2 轮）。行级 user_id 过滤。
func (r *AgentMessageRepo) CountByConversation(ctx context.Context, userID int64, convID ids.ID) (int, error) {
	var n int
	err := r.DB.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM agent_message WHERE conversation_id=? AND user_id=? AND role='user'`,
		convID.Int64(), userID)
	return n, err
}
