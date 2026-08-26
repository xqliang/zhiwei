package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonEvent 是事件平面（spec §4.4）：人物的大事记——有日期的一次性事件
// （结婚/毕业/旅行/聚会/会议/生病/学会…）。与 list 属性互补：属性记「有过的」，
// event 记「某次发生的」。related_person_ids 为 MVP 单元素或空（多对多留后续）。
type PersonEvent struct {
	ID          ids.ID     `db:"id" json:"id"`
	UserID      int64      `db:"user_id" json:"user_id"`
	PersonID    ids.ID     `db:"person_id" json:"person_id"`
	EventType   string     `db:"event_type" json:"event_type"` // 里程碑|聚会|会议|旅行|健康|成就|挫折|负面|其他
	Title       string     `db:"title" json:"title"`
	Description *string    `db:"description" json:"description,omitempty"`
	OccurredAt  *time.Time `db:"occurred_at" json:"occurred_at,omitempty"` // 可能只精确到日/月；LLM 给不出时间为 NULL
	EndAt       *time.Time `db:"end_at" json:"end_at,omitempty"`           // 跨天事件（旅行/会议）
	Location    *string    `db:"location" json:"location,omitempty"`
	// RelatedPersonIDs 同场人物（DB 侧 JSON 列）；ids.List 为 nil 时写 NULL，非 nil 写 JSON 数组。
	RelatedPersonIDs ids.List `db:"related_person_ids" json:"related_person_ids"`
	Importance       float64  `db:"importance" json:"importance"` // 事件重要度 0~1，排序/展示权重用
	// ---- 横切字段（与 attribute/relationship 平面一致，spec §3）----
	Confidence           float64   `db:"confidence" json:"confidence"`
	EpistemicType        string    `db:"epistemic_type" json:"epistemic_type"` // observed|inferred|predicted|suggested
	Source               string    `db:"source" json:"source"`                 // manual|llm
	Status               string    `db:"status" json:"status"`                 // active|pending|superseded|dismissed
	SessionID            *ids.ID   `db:"session_id" json:"session_id,omitempty"`
	MemoryID             *ids.ID   `db:"memory_id" json:"memory_id,omitempty"`
	TranscriptSegmentIDs ids.List  `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	SupersedesID         *ids.ID   `db:"supersedes_id" json:"supersedes_id,omitempty"` // 冲突 pending 指向当前 active 行
	Version              int       `db:"version" json:"version"`
	CreatedAt            time.Time `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time `db:"updated_at" json:"updated_at"`
}

type PersonEventRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。零值兜底同其他 repo。
// 注：Confidence==0 也兜底为 0.8——闸门已把低置信候选拦在门外，到这里的 0 视为漏填；
// Importance==0 兜底为 0.5（中性重要度），避免漏填时事件被排序/展示逻辑当成「最不重要」。
func (r *PersonEventRepo) CreateExt(ctx context.Context, ext ExecerContext, e *PersonEvent) error {
	e.ID = ids.New()
	if e.UserID == 0 {
		e.UserID = 1
	}
	if e.Confidence == 0 {
		e.Confidence = 0.8
	}
	if e.Importance == 0 {
		e.Importance = 0.5
	}
	if e.EpistemicType == "" {
		e.EpistemicType = "observed"
	}
	if e.Source == "" {
		e.Source = "manual"
	}
	if e.Status == "" {
		e.Status = "active"
	}
	if e.Version == 0 {
		e.Version = 1
	}
	// 显式列出 20 列（created_at/updated_at 由 DB 默认值填充，不在写入之列）。
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_event
  (id, user_id, person_id, event_type, title, description, occurred_at, end_at, location,
   related_person_ids, importance, confidence, epistemic_type, source, status,
   session_id, memory_id, transcript_segment_ids, supersedes_id, version)
VALUES
  (:id, :user_id, :person_id, :event_type, :title, :description, :occurred_at, :end_at, :location,
   :related_person_ids, :importance, :confidence, :epistemic_type, :source, :status,
   :session_id, :memory_id, :transcript_segment_ids, :supersedes_id, :version)`, e)
	return err
}

func (r *PersonEventRepo) Create(ctx context.Context, e *PersonEvent) error {
	return r.CreateExt(ctx, r.DB, e)
}

// Get 按 id 查；不存在返回 (nil, nil)（与其他 repo 风格一致，调用方判 nil）。
func (r *PersonEventRepo) Get(ctx context.Context, id ids.ID) (*PersonEvent, error) {
	var e PersonEvent
	err := r.DB.GetContext(ctx, &e, `SELECT * FROM person_event WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ListByPerson 主体维度的全状态大事记，按事件发生时间倒序（最新在前）。
// 详情页要展示 active+pending，历史走 change_log，故这里不过滤 status。
//
// 关于 occurred_at 为 NULL 的排序：MySQL 在排序时把 NULL 当作「比任何非 NULL 都小」，
// 因此 ORDER BY ... DESC 时 NULL 排在最后（ASC 则排最前）。事件里 occurred_at 可能为
// NULL（LLM 给不出确切时间），我们期望「有时间的按时间倒序在前、无时间的沉底」，
// 恰好是 DESC 的天然行为，无需额外 CASE/COALESCE（已由测试 e4 验证）。
// 同一 occurred_at（含同为 NULL）再按 id DESC 兜底，后建的在前，保证顺序稳定。
func (r *PersonEventRepo) ListByPerson(ctx context.Context, personID ids.ID) ([]PersonEvent, error) {
	var list []PersonEvent
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_event WHERE person_id = ? ORDER BY occurred_at DESC, id DESC`, personID.Int64())
	return list, err
}

// FindActiveByKeyExt 事件的「当前 active 行」查询：按（主体, 类型, 标题）定位。
// 事件的当前身份是 event_type + title（如「旅行 / 去云南旅游」），同一身份至多一条 active，
// 供 Task 6 事务内判定「这条事件是否已有 active 版本」（有则走冲突/更新，无则新增）。
// 无命中返回 (nil, nil)。只提供 Ext 版本：消费方（ApplyFacts）全程在事务内，
// 与 person_relationship 的 FindActiveByTypeExt 保持一致，故不额外包非事务版。
func (r *PersonEventRepo) FindActiveByKeyExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, eventType, title string) (*PersonEvent, error) {
	var e PersonEvent
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_event
WHERE person_id = ? AND event_type = ? AND title = ? AND status = 'active'
ORDER BY id DESC LIMIT 1`, personID.Int64(), eventType, title).StructScan(&e)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// FindByNaturalKeyExt 幂等去重：同 session 同（主体, 类型, 标题）任意 status 的行。
// 重跑同一 session 时命中已有行，不重复建 pending / 不重复入库（spec §6.3），
// 与 attribute/relationship 的自然键语义一致。occurred_at/location/description 不进
// 自然键（同一事件补充时间地点视为同一条）。无命中返回 (nil, nil)。
func (r *PersonEventRepo) FindByNaturalKeyExt(ctx context.Context, ext QueryRowxContext, sessionID, personID ids.ID, eventType, title string) (*PersonEvent, error) {
	var e PersonEvent
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_event
WHERE session_id = ? AND person_id = ? AND event_type = ? AND title = ?
ORDER BY id LIMIT 1`, sessionID.Int64(), personID.Int64(), eventType, title).StructScan(&e)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *PersonEventRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_event SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonEventRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// ListPending 全局确认队列（事件平面部分），按 id 升序（先产生的先确认）。
func (r *PersonEventRepo) ListPending(ctx context.Context, userID int64) ([]PersonEvent, error) {
	var list []PersonEvent
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_event WHERE user_id = ? AND status = 'pending' ORDER BY id`, userID)
	return list, err
}
