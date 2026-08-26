package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonRelationship 是关系平面（spec §4.3）：person 与对端人物/组织的边。
// 「老婆做什么的」= 配偶关系边 + 对端 person 的 occupation 属性；
// 「几个孩子/几岁/生日」= N 条子女边 + 各子女 person 的 age/birthday 属性。
type PersonRelationship struct {
	ID                   ids.ID    `db:"id" json:"id"`
	UserID               int64     `db:"user_id" json:"user_id"`
	PersonID             ids.ID    `db:"person_id" json:"person_id"` // 主体
	RelatedPersonID      *ids.ID   `db:"related_person_id" json:"related_person_id,omitempty"`
	RelationType         string    `db:"relation_type" json:"relation_type"`   // 配偶|子女|父母|兄弟姐妹|亲戚|朋友|同事|领导|下属|客户|供应商|合作方|组织|其他
	Direction            *string   `db:"direction" json:"direction,omitempty"` // upstream|downstream|peer
	OrgName              *string   `db:"org_name" json:"org_name,omitempty"`
	Label                *string   `db:"label" json:"label,omitempty"` // 自由称呼（「大儿子」「张总」）
	Confidence           float64   `db:"confidence" json:"confidence"`
	EpistemicType        string    `db:"epistemic_type" json:"epistemic_type"`
	Source               string    `db:"source" json:"source"` // manual|llm
	Status               string    `db:"status" json:"status"` // active|pending|superseded|dismissed
	SessionID            *ids.ID   `db:"session_id" json:"session_id,omitempty"`
	MemoryID             *ids.ID   `db:"memory_id" json:"memory_id,omitempty"`
	TranscriptSegmentIDs ids.List  `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	SupersedesID         *ids.ID   `db:"supersedes_id" json:"supersedes_id,omitempty"`
	Version              int       `db:"version" json:"version"`
	CreatedAt            time.Time `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time `db:"updated_at" json:"updated_at"`
}

type PersonRelationshipRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。零值兜底同其他 repo。
// 注：Confidence==0 也兜底为 0.8——闸门已把低置信候选拦在门外，到这里的 0 视为漏填。
func (r *PersonRelationshipRepo) CreateExt(ctx context.Context, ext ExecerContext, rel *PersonRelationship) error {
	rel.ID = ids.New()
	if rel.UserID == 0 {
		rel.UserID = 1
	}
	if rel.Confidence == 0 {
		rel.Confidence = 0.8
	}
	if rel.EpistemicType == "" {
		rel.EpistemicType = "observed"
	}
	if rel.Source == "" {
		rel.Source = "manual"
	}
	if rel.Status == "" {
		rel.Status = "active"
	}
	if rel.Version == 0 {
		rel.Version = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_relationship
  (id, user_id, person_id, related_person_id, relation_type, direction, org_name, label,
   confidence, epistemic_type, source, status, session_id, memory_id, transcript_segment_ids, supersedes_id, version)
VALUES
  (:id, :user_id, :person_id, :related_person_id, :relation_type, :direction, :org_name, :label,
   :confidence, :epistemic_type, :source, :status, :session_id, :memory_id, :transcript_segment_ids, :supersedes_id, :version)`, rel)
	return err
}

func (r *PersonRelationshipRepo) Create(ctx context.Context, rel *PersonRelationship) error {
	return r.CreateExt(ctx, r.DB, rel)
}

// Get 按 id 查；不存在返回 (nil, nil)（与其他 repo 风格一致，调用方判 nil）。
func (r *PersonRelationshipRepo) Get(ctx context.Context, id ids.ID) (*PersonRelationship, error) {
	var rel PersonRelationship
	err := r.DB.GetContext(ctx, &rel, `SELECT * FROM person_relationship WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rel, nil
}

// ListByPerson 主体维度的全状态关系列表（详情页展示 active+pending，历史走 change_log）。
func (r *PersonRelationshipRepo) ListByPerson(ctx context.Context, personID ids.ID) ([]PersonRelationship, error) {
	var list []PersonRelationship
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_relationship WHERE person_id = ? ORDER BY id`, personID.Int64())
	return list, err
}

// FindActiveByTypeExt 按（主体, 类型, 对端）找 active 行；对端用 NULL 安全比较
// （<=>）。组织关系（对端 nil）与人物关系都能命中。无返回 nil。
// 说明：普通 `= NULL` 恒为 UNKNOWN 永不命中，故对端可空场景必须用 `<=>`。
func (r *PersonRelationshipRepo) FindActiveByTypeExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, relationType string, relatedPersonID *ids.ID) (*PersonRelationship, error) {
	var rel PersonRelationship
	// rid 为 nil 时绑定 SQL NULL，配合 <=> 命中 related_person_id IS NULL 的行。
	var rid any
	if relatedPersonID != nil {
		rid = relatedPersonID.Int64()
	}
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_relationship
WHERE person_id = ? AND relation_type = ? AND related_person_id <=> ? AND status = 'active'
ORDER BY id DESC LIMIT 1`, personID.Int64(), relationType, rid).StructScan(&rel)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rel, nil
}

// FindActiveRelatedPersonIDExt 找主体指定类型的最早 active 关系对端人物 id
// （「我老婆」→ 配偶 person；多条同类型取最老一条，id 升序稳定选择）。必须传 tx 以看到
// 本事务内未提交的关系行（ApplyFacts 批内 relation 指代解析依赖此语义：同批「我老婆是医生」
// 挂到刚新建、尚未提交的配偶关系对端上，走非事务读会看不到而误判解析不到）。
// 只返回对端为具体 person（related_person_id 非 NULL）的关系；无命中返回 (nil, nil)。
// 注：FindActiveByTypeExt 传 nil 对端只匹配 related_person_id IS NULL 的组织关系，语义相反，
// 故此处单列一个查询，专供「按关系找对端 person」，不复用它。
func (r *PersonRelationshipRepo) FindActiveRelatedPersonIDExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, relationType string) (*ids.ID, error) {
	var rid ids.ID
	err := ext.QueryRowxContext(ctx, `
SELECT related_person_id FROM person_relationship
WHERE person_id = ? AND relation_type = ? AND status = 'active' AND related_person_id IS NOT NULL
ORDER BY id LIMIT 1`, personID.Int64(), relationType).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rid, nil
}

// FindByNaturalKeyExt 幂等去重：同 session 同（主体,类型,对端）任意 status 的行。
// label 不进自然键（同一关系不同称呼视为同一条）。对端可空，同样用 `<=>` 匹配。
func (r *PersonRelationshipRepo) FindByNaturalKeyExt(ctx context.Context, ext QueryRowxContext, sessionID, personID ids.ID, relationType string, relatedPersonID *ids.ID) (*PersonRelationship, error) {
	var rel PersonRelationship
	var rid any
	if relatedPersonID != nil {
		rid = relatedPersonID.Int64()
	}
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_relationship
WHERE session_id = ? AND person_id = ? AND relation_type = ? AND related_person_id <=> ?
ORDER BY id LIMIT 1`, sessionID.Int64(), personID.Int64(), relationType, rid).StructScan(&rel)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rel, nil
}

func (r *PersonRelationshipRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_relationship SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonRelationshipRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// DismissAllByPersonExt 人物归档级联（spec §13 F5）：把该人物在**关系平面**上所有活跃态
// （active/pending）的行批量置 dismissed，返回受影响行数（RowsAffected）。供 ManualSetPersonStatus
// 归档分支在事务内调用（ext 传 *sqlx.Tx，随归档事务一起提交/回滚）。
//
// 级联语义——只动 active 与 pending：superseded（已被新版本取代）与 dismissed（已放弃/已归档）
// 都是**终态**，不再改动；否则会把「历史被取代行」也翻成 dismissed，既无意义又污染 supersedes
// 链的历史可读性。故用 status IN ('active','pending') 精确圈定活跃态。
// 注：这里只级联「以该人物为主体（person_id）」的关系边；反向边（其他人物 related_person_id
// 指向本人）不动——那属于对端人物的关系，归档本人不应替对端做主（留跟进）。
func (r *PersonRelationshipRepo) DismissAllByPersonExt(ctx context.Context, ext ExecerContext, personID ids.ID) (int64, error) {
	res, err := ext.ExecContext(ctx,
		`UPDATE person_relationship SET status = 'dismissed' WHERE person_id = ? AND status IN ('active','pending')`,
		personID.Int64())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListPending 全局确认队列（关系平面部分），按 id 升序（先产生的先确认）。
func (r *PersonRelationshipRepo) ListPending(ctx context.Context, userID int64) ([]PersonRelationship, error) {
	var list []PersonRelationship
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_relationship WHERE user_id = ? AND status = 'pending' ORDER BY id`, userID)
	return list, err
}
