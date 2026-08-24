package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentProposal 是 agent 提议的一处修改，人审前只落此表（绝不静默写）。
// 状态流：pending →(用户确认并落库) applied ｜ →(放弃) dismissed ｜ →(超期) expired。
// 设计里的 "confirmed" 语义由 applied 承载（确认与落库在同一事务原子完成）。
type AgentProposal struct {
	ID             ids.ID          `db:"id" json:"id"`
	UserID         int64           `db:"user_id" json:"user_id"`
	ConversationID *ids.ID         `db:"conversation_id" json:"conversation_id,omitempty"`
	MessageID      *ids.ID         `db:"message_id" json:"message_id,omitempty"`
	Kind           string          `db:"kind" json:"kind"`
	TargetKind     string          `db:"target_kind" json:"target_kind"`
	TargetID       *ids.ID         `db:"target_id" json:"target_id,omitempty"`
	Payload        json.RawMessage `db:"payload" json:"payload"`
	Rationale      string          `db:"rationale" json:"rationale"`
	Status         string          `db:"status" json:"status"`
	AppliedRef     *ids.ID         `db:"applied_ref" json:"applied_ref,omitempty"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
	ResolvedAt     *time.Time      `db:"resolved_at" json:"resolved_at,omitempty"`
}

// ValidProposalStatus 校验提议状态枚举。
func ValidProposalStatus(s string) bool {
	switch s {
	case "pending", "applied", "dismissed", "expired":
		return true
	}
	return false
}

type AgentProposalRepo struct{ DB *sqlx.DB }

// Create 新建提议：生成 ID，UserID 默认 1，Status 强制 pending，Payload 空时置 "{}"。
func (r *AgentProposalRepo) Create(ctx context.Context, p *AgentProposal) error {
	p.ID = ids.New()
	if p.UserID == 0 {
		p.UserID = 1
	}
	p.Status = "pending"
	if len(p.Payload) == 0 {
		p.Payload = json.RawMessage("{}")
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO agent_proposal
  (id, user_id, conversation_id, message_id, kind, target_kind, target_id, payload, rationale, status)
VALUES
  (:id, :user_id, :conversation_id, :message_id, :kind, :target_kind, :target_id, :payload, :rationale, :status)`, p)
	return err
}

func (r *AgentProposalRepo) Get(ctx context.Context, id ids.ID) (*AgentProposal, error) {
	var p AgentProposal
	err := r.DB.GetContext(ctx, &p, `SELECT * FROM agent_proposal WHERE id = ?`, id.Int64())
	return &p, err
}

// ListPending 返回某用户全部待确认提议（最新优先）。
func (r *AgentProposalRepo) ListPending(ctx context.Context, userID int64) ([]AgentProposal, error) {
	var rows []AgentProposal
	err := r.DB.SelectContext(ctx, &rows, `
SELECT * FROM agent_proposal
WHERE user_id = ? AND status = 'pending'
ORDER BY id DESC LIMIT 200`, userID)
	return rows, err
}

// Resolve 把 pending 提议置为终态（applied/dismissed/expired），设 resolved_at；
// applied 时回填 appliedRef。收 ExecerContext：确认端点会在「落库到
// memory/topic/todo」的同一事务内调用（事务外调用传 r.DB）。
// 返回 (true,nil) 表示本次确实把一条 pending 行转为终态；(false,nil) 表示该行
// 已非 pending（并发确认/重复确认的输方）——调用方据此回滚其领域写入，保证 apply-once。
func (r *AgentProposalRepo) Resolve(ctx context.Context, ext ExecerContext, id ids.ID, status string, appliedRef *ids.ID) (bool, error) {
	if status == "pending" || !ValidProposalStatus(status) {
		return false, fmt.Errorf("非法提议终态: %q", status)
	}
	var ref any
	if appliedRef != nil {
		ref = appliedRef.Int64()
	}
	res, err := ext.ExecContext(ctx, `
UPDATE agent_proposal
SET status = ?, applied_ref = ?, resolved_at = CURRENT_TIMESTAMP(3)
WHERE id = ? AND status = 'pending'`, status, ref, id.Int64())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
