package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"zhiwei/internal/auth"
	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
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
	// ---- 画像确认落库（P2）----
	// Profile 提供 ManualAddAttributeExt/ManualAddEventExt/ManualAddRelationshipExt/
	// ManualCreatePersonExt（事务版），在 confirm 单事务里把画像写并进来（apply-once）。
	// Persons 供 profile_relationship 确认时按名解析关联人（FindByNameExt，未命中则经
	// ManualCreatePersonExt 新建），以及 owner 相关校验。
	Profile *profile.Service
	Persons *repo.PersonRepo
}

// RegisterProposals 挂载写-提议闸门的人审侧端点（spec §8）：列出/确认/放弃。
func RegisterProposals(r chi.Router, d ProposalDeps) {
	r.Get("/api/agent/proposals", d.listProposals)
	r.Post("/api/agent/proposals/{id}/confirm", d.confirmProposal)
	r.Post("/api/agent/proposals/{id}/dismiss", d.dismissProposal)
}

func (d ProposalDeps) listProposals(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rows, err := d.Proposals.ListPending(r.Context(), uid.Int64())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": rows})
}

// confirmProposal 在单事务内落库并把提议置 applied（apply-once, spec §8）：
// 领域写 + Resolve 同事务；Resolve 返回 false（并发/重复确认的输方）则回滚领域写。
func (d ProposalDeps) confirmProposal(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
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
	// IDOR 归属校验（2B-B）：Proposals.Get 不带 user 过滤，须在此比对提议归属；不匹配按「不存在」
	// 返回 404（不泄露「存在但非你的」）。防止 A 确认/查看 B 的提议、进而经 applyInTx 改 B 的数据。
	if p.UserID != uid.Int64() {
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
	uid, ok := auth.UserID(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
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
	// IDOR 归属校验（2B-B，同 confirm）：不匹配按「不存在」返回 404，杜绝跨用户放弃他人提议。
	if p.UserID != uid.Int64() {
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
	resolved, err := d.Proposals.Resolve(r.Context(), d.DB, id, "dismissed", nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !resolved { // 并发 confirm 抢先(pending 检查与 Resolve 之间)：状态码与 body 一致→409
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
	// newFloatPtr 从 pl.New 取 *float64：JSON 数字进 map[string]any 是 float64，存在且是 float64
	// 则取其地址副本，否则 nil（缺键/非数字）。供 profile_metric 的 value_num（可空数值读数）用。
	newFloatPtr := func(k string) *float64 {
		if pl.New == nil {
			return nil
		}
		if v, ok := pl.New[k].(float64); ok {
			vv := v
			return &vv
		}
		return nil
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
	case "profile_attr":
		// 画像属性：target_id=owner person id（propose 时定）。走 profile.Service 的事务版
		// ManualAddAttributeExt，把「属性写(active/manual/审计 + 单值 supersede) + Resolve」并进
		// 本 confirm 事务，apply-once。ChangedBy 记 user（用户点了确认，语义等同手动改画像）。
		if p.TargetID == nil {
			return nil, fmt.Errorf("profile_attr 缺 target_id")
		}
		attrKey := newStr("attr_key")
		value := newStr("value")
		if attrKey == "" || value == "" {
			return nil, fmt.Errorf("profile_attr 缺 new.attr_key/value")
		}
		if !isKnownAttrKey(attrKey) { // 双保险：confirm 端也校验 catalog(propose 已校，但防未来别的提议源不 gate 就静默写入非法键)
			return nil, fmt.Errorf("profile_attr 非法属性键: %s", attrKey)
		}
		row, err := d.Profile.ManualAddAttributeExt(ctx, tx, p.UserID, *p.TargetID, attrKey, value)
		if err != nil {
			return nil, err
		}
		return &row.ID, nil
	case "profile_event":
		// 画像大事记：走事务版 ManualAddEventExt（内部再校验 event_type 合法 + title 非空，
		// 与 propose 端校验双保险）。occurred_at 是原始字符串，parseEventAt 尽力解析、失败存 NULL。
		if p.TargetID == nil {
			return nil, fmt.Errorf("profile_event 缺 target_id")
		}
		eventType := newStr("event_type")
		title := newStr("title")
		occurredAt := newStr("occurred_at")
		row, err := d.Profile.ManualAddEventExt(ctx, tx, p.UserID, *p.TargetID, eventType, title, "", occurredAt, "", "", nil)
		if err != nil {
			return nil, err
		}
		return &row.ID, nil
	case "profile_relationship":
		// 画像关系：target_id=owner person id（propose 时定）。在本 confirm 单事务内完成
		// 「解析或新建关联人 + 写关系 + Resolve」（apply-once，设计 D1）：
		//   ① related_person_name 命中已有人物 → 用其 id；
		//   ② 未命中 → ManualCreatePersonExt 新建人物（active/manual）再用其 id；
		//   ③ 仅 org_name（无 related_person_name）→ 组织关系，relatedPersonID=nil。
		// 全部走 tx，与 Proposals.Resolve 同事务，任一步失败整体回滚；重复 confirm 因
		// status!=pending 在 confirmProposal 早返回、不进本函数，故天然幂等（apply-once 靠 Resolve CAS）。
		if p.TargetID == nil {
			return nil, fmt.Errorf("profile_relationship 缺 target_id")
		}
		relationType := newStr("relation_type")
		if !profile.ValidRelations[relationType] { // 双保险：与 propose 端一致，防未来别的提议源不 gate 就写入非法关系类型
			return nil, fmt.Errorf("profile_relationship 非法关系类型: %s", relationType)
		}
		var relID *ids.ID
		if name := newStr("related_person_name"); name != "" {
			ex, err := d.Persons.FindByNameExt(ctx, tx, p.UserID, name)
			if err != nil {
				return nil, err
			}
			if ex != nil { // 命中已有 active/pending 人物
				id := ex.ID
				relID = &id
			} else { // 未命中 → 在同事务内新建人物（active/manual），再用其 id 建关系
				np, err := d.Profile.ManualCreatePersonExt(ctx, tx, p.UserID, name, nil, nil)
				if err != nil {
					return nil, err
				}
				relID = &np.ID
			}
		}
		row, err := d.Profile.ManualAddRelationshipExt(ctx, tx, p.UserID, *p.TargetID, relationType, relID,
			newStr("direction"), newStr("org_name"), newStr("label"))
		if err != nil {
			return nil, err
		}
		return &row.ID, nil
	case "profile_metric":
		// 画像指标（第 5 平面 person_metric）：target_id=owner person id（propose 时定）。走事务版
		// ManualAddMetricExt（内部再校验 metric_key 合法 + 数值/类别值约束 + measured_at 非零，与
		// propose 端双保险）。value_num 为可空数值读数（JSON 数字 → float64）；measured_at 是原始
		// 字符串，尽力解析(RFC3339/"2006-01-02 15:04"/"2006-01-02")，失败或空取 now（列 NOT NULL）。
		// append-only：每次 confirm 追加一行、不 supersede；重复 confirm 因 status!=pending 在
		// confirmProposal 早返回、不进本函数，故天然幂等（apply-once 靠 Resolve CAS）。
		if p.TargetID == nil {
			return nil, fmt.Errorf("profile_metric 缺 target_id")
		}
		metricKey := newStr("metric_key")
		if !profile.ValidMetricKey(metricKey) { // 双保险：与 propose 端一致，防未来别的提议源不 gate 就写入非法键
			return nil, fmt.Errorf("profile_metric 非法指标键: %s", metricKey)
		}
		measuredAt := time.Now() // ManualAddMetricExt 要求非零；空/解析失败即回退 now
		if t, ok := parseMetricMeasuredAt(newStr("measured_at")); ok {
			measuredAt = t
		}
		row, err := d.Profile.ManualAddMetricExt(ctx, tx, p.UserID, *p.TargetID, metricKey,
			newFloatPtr("value_num"), newStr("value_text"), newStr("unit"), measuredAt)
		if err != nil {
			return nil, err
		}
		return &row.ID, nil
	default:
		return nil, fmt.Errorf("未知提议 kind: %s", p.Kind)
	}
}

// parseMetricMeasuredAt 尽力解析测点时间，**保留时刻精度**（metric 是连续时序，同日多次测量
// 靠时刻区分）。依次尝试 RFC3339（带时区/时刻）、"2006-01-02 15:04:05"、"2006-01-02 15:04"、
// "2006-01-02T15:04:05"、"2006-01-02"；空串或全部失败返回 (_, false)，调用方回退 time.Now()
// （ManualAddMetricExt 要求 measured_at 非零）。与 profile.parseMetricAt 的解析口径一致，但那是
// profile 包内私有函数、不可跨包调用，故此处按同样格式集独立实现。
func parseMetricMeasuredAt(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
