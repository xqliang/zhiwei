package repo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentMessage 是对话流里的一条消息（用户/助手），可携带引用与工具载荷。
// Citations/ToolPayload 对应可空 JSON 列：写入 nil→NULL 安全；但值类型
// json.RawMessage 无法直接扫描 NULL（database/sql 会报 unsupported Scan），
// 故读取由 ListByConversation 用 COALESCE 把 NULL 归一成空串（详见该方法）。
type AgentMessage struct {
	ID             ids.ID          `db:"id" json:"id"`
	UserID         int64           `db:"user_id" json:"user_id"`
	ConversationID *ids.ID         `db:"conversation_id" json:"conversation_id,omitempty"`
	Role           string          `db:"role" json:"role"` // user|assistant
	Kind           string          `db:"kind" json:"kind"` // text|tool_call|tool_result|card
	Content        string          `db:"content" json:"content"`
	Citations      json.RawMessage `db:"citations" json:"citations,omitempty"`
	ToolPayload    json.RawMessage `db:"tool_payload" json:"tool_payload,omitempty"`
	DSHSeq         *int            `db:"dsh_seq" json:"dsh_seq,omitempty"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
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
// 说明：citations/tool_payload 为可空 JSON 列，而字段是值类型 json.RawMessage，
// database/sql 无法把 NULL 扫进它（会报 unsupported Scan）。故用 COALESCE 把 NULL
// 归一成空串（len==0，等价于「无引用」；配合 omitempty 序列化时省略）。不用 SELECT *。
func (r *AgentMessageRepo) ListByConversation(ctx context.Context, convID ids.ID) ([]AgentMessage, error) {
	var rows []AgentMessage
	err := r.DB.SelectContext(ctx, &rows, `
SELECT id, user_id, conversation_id, role, kind, content,
       COALESCE(CAST(citations AS CHAR), '')    AS citations,
       COALESCE(CAST(tool_payload AS CHAR), '') AS tool_payload,
       dsh_seq, created_at
FROM agent_message WHERE conversation_id = ? ORDER BY id ASC`, convID.Int64())
	return rows, err
}
