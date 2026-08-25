package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// ProposalDeps 是提议确认/放弃端点的依赖（主服务装配注入）。
type ProposalDeps struct {
	DB         *sqlx.DB
	Proposals  *repo.AgentProposalRepo
	Memories   *repo.MemoryRepo
	Topics     *repo.TopicRepo
	Todos      *repo.TodoRepo
	TodoTopics *repo.TodoTopicRepo
}

// RegisterProposals 挂载写-提议闸门的人审侧端点（spec §8）：列出/确认/放弃。
func RegisterProposals(r chi.Router, d ProposalDeps) {
	r.Get("/api/agent/proposals", d.listProposals)
	r.Post("/api/agent/proposals/{id}/confirm", d.confirmProposal)
	r.Post("/api/agent/proposals/{id}/dismiss", d.dismissProposal)
}

func (d ProposalDeps) listProposals(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Proposals.ListPending(r.Context(), toolUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": rows})
}

// confirmProposal 在单事务内落库并把提议置 applied（apply-once, spec §8）：
// 领域写 + Resolve 同事务；Resolve 返回 false（并发/重复确认的输方）则回滚领域写。
func (d ProposalDeps) confirmProposal(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	p, err := d.Proposals.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "proposal not found"})
		return
	}
	if p.Status != "pending" { // 幂等：已终态（applied/dismissed/expired）直接回当前状态
		writeJSON(w, http.StatusOK, p)
		return
	}
	tx, err := d.DB.BeginTxx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op

	appliedRef, err := d.applyInTx(r.Context(), tx, p)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	applied, err := d.Proposals.Resolve(r.Context(), tx, id, "applied", appliedRef)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !applied { // 输掉并发/重复确认：回滚领域写, 保证 apply-once
		writeJSON(w, http.StatusConflict, map[string]string{"error": "提议已被处理"})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if p2, err := d.Proposals.Get(r.Context(), id); err == nil {
		writeJSON(w, http.StatusOK, p2)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (d ProposalDeps) dismissProposal(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	p, err := d.Proposals.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "proposal not found"})
		return
	}
	if p.Status == "dismissed" { // 幂等
		writeJSON(w, http.StatusOK, p)
		return
	}
	if p.Status != "pending" { // 已 applied/expired 不能再放弃
		writeJSON(w, http.StatusConflict, map[string]string{"error": "提议已被处理"})
		return
	}
	ok, err := d.Proposals.Resolve(r.Context(), d.DB, id, "dismissed", nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok { // 并发 confirm 抢先(pending 检查与 Resolve 之间)：状态码与 body 一致→409
		writeJSON(w, http.StatusConflict, map[string]string{"error": "提议已被处理"})
		return
	}
	if p2, err := d.Proposals.Get(r.Context(), id); err == nil {
		writeJSON(w, http.StatusOK, p2)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// proposalPayload 解析 {old,new} 载荷；apply 只用 new。
type proposalPayload struct {
	New map[string]any `json:"new"`
}

// applyInTx 按 kind 在事务内落领域改动, 返回 appliedRef（新建/修改行 id, 写进 proposal.applied_ref）。
// 全程用 *Ext（事务版）repo 方法, 与 Resolve 同事务, 保证 apply-once。
func (d ProposalDeps) applyInTx(ctx context.Context, tx *sqlx.Tx, p *repo.AgentProposal) (*ids.ID, error) {
	var pl proposalPayload
	_ = json.Unmarshal(p.Payload, &pl)
	newStr := func(k string) string {
		if pl.New == nil {
			return ""
		}
		if v, ok := pl.New[k].(string); ok {
			return v
		}
		return ""
	}
	switch p.Kind {
	case "memory_update":
		if p.TargetID == nil {
			return nil, fmt.Errorf("memory_update 缺 target_id")
		}
		m, err := d.Memories.GetExt(ctx, tx, *p.TargetID)
		if err != nil {
			return nil, err
		}
		if v := newStr("title"); v != "" {
			m.Title = v
		}
		if v := newStr("content"); v != "" {
			m.Content = v
		}
		m.Version++ // 记入 memory 版本机制（spec §8）
		if err := d.Memories.SaveExt(ctx, tx, m); err != nil {
			return nil, err
		}
		return p.TargetID, nil
	case "memory_dismiss":
		if p.TargetID == nil {
			return nil, fmt.Errorf("memory_dismiss 缺 target_id")
		}
		m, err := d.Memories.GetExt(ctx, tx, *p.TargetID)
		if err != nil {
			return nil, err
		}
		m.Status = "dismissed"
		m.Version++
		if err := d.Memories.SaveExt(ctx, tx, m); err != nil {
			return nil, err
		}
		return p.TargetID, nil
	case "topic_rename":
		if p.TargetID == nil {
			return nil, fmt.Errorf("topic_rename 缺 target_id")
		}
		name := newStr("name")
		if name == "" {
			return nil, fmt.Errorf("topic_rename 缺 new.name")
		}
		if err := d.Topics.UpdateNameExt(ctx, tx, *p.TargetID, name); err != nil {
			return nil, err
		}
		return p.TargetID, nil
	case "topic_confirm":
		if p.TargetID == nil {
			return nil, fmt.Errorf("topic_confirm 缺 target_id")
		}
		if err := d.Topics.UpdateStatusExt(ctx, tx, *p.TargetID, "active"); err != nil {
			return nil, err
		}
		return p.TargetID, nil
	case "topic_dismiss":
		if p.TargetID == nil {
			return nil, fmt.Errorf("topic_dismiss 缺 target_id")
		}
		if err := d.Topics.UpdateStatusExt(ctx, tx, *p.TargetID, "dismissed"); err != nil {
			return nil, err
		}
		return p.TargetID, nil
	case "todo_create":
		title := newStr("title")
		if title == "" {
			return nil, fmt.Errorf("todo_create 缺 new.title")
		}
		td := &repo.Todo{Title: title, Status: "confirmed", Confidence: 1} // 用户确认→confirmed
		if s := newStr("due_at"); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				td.DueAt = &t
			}
		}
		if err := d.Todos.InsertExt(ctx, tx, []*repo.Todo{td}); err != nil { // InsertExt 生成 td.ID
			return nil, err
		}
		if s := newStr("topic_id"); s != "" {
			if tid, err := ids.ParseID(s); err == nil {
				if err := d.TodoTopics.InsertExt(ctx, tx, []*repo.TodoTopicLink{{TodoID: td.ID, TopicID: tid, Source: "user"}}); err != nil {
					return nil, err
				}
			}
		}
		ref := td.ID
		return &ref, nil
	case "todo_status":
		if p.TargetID == nil {
			return nil, fmt.Errorf("todo_status 缺 target_id")
		}
		st := newStr("status")
		if st == "" {
			return nil, fmt.Errorf("todo_status 缺 new.status")
		}
		td, err := d.Todos.GetExt(ctx, tx, *p.TargetID) // 锁行读当前状态
		if err != nil {
			return nil, err
		}
		if !repo.CanTransition(td.Status, st) { // 闸门须与 REST 端点同样守状态机(评审 I1)
			return nil, fmt.Errorf("非法待办状态流转: %s → %s", td.Status, st)
		}
		if err := d.Todos.UpdateStatusExt(ctx, tx, *p.TargetID, st); err != nil {
			return nil, err
		}
		return p.TargetID, nil
	default:
		return nil, fmt.Errorf("未知提议 kind: %s", p.Kind)
	}
}
