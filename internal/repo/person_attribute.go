package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonAttribute 是画像的类型化属性行（spec §4.2）。
// 列表型属性（爱好/书单…）= 同 attr_key 多行 active，每元素独立
// 置信度/来源/溯源/确认；单值型 = 同 key 至多一行 active。
// 单值 vs 列表由 internal/profile 目录的 Cardinality 声明，表结构不区分。
type PersonAttribute struct {
	ID                   ids.ID    `db:"id" json:"id"`
	UserID               int64     `db:"user_id" json:"user_id"`
	PersonID             ids.ID    `db:"person_id" json:"person_id"`
	AttrKey              string    `db:"attr_key" json:"attr_key"`
	ValueText            string    `db:"value_text" json:"value_text"`
	ValueType            string    `db:"value_type" json:"value_type"` // text|enum|bool|date|number
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

type PersonAttributeRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。零值兜底。
// 注：Confidence==0 也兜底为 0.8——闸门已把低置信候选拦在门外，到这里的 0 视为漏填。
func (r *PersonAttributeRepo) CreateExt(ctx context.Context, ext ExecerContext, a *PersonAttribute) error {
	a.ID = ids.New()
	if a.UserID == 0 {
		a.UserID = 1
	}
	if a.ValueType == "" {
		a.ValueType = "text"
	}
	if a.Confidence == 0 {
		a.Confidence = 0.8
	}
	if a.EpistemicType == "" {
		a.EpistemicType = "observed"
	}
	if a.Source == "" {
		a.Source = "manual"
	}
	if a.Status == "" {
		a.Status = "active"
	}
	if a.Version == 0 {
		a.Version = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_attribute
  (id, user_id, person_id, attr_key, value_text, value_type, confidence, epistemic_type,
   source, status, session_id, memory_id, transcript_segment_ids, supersedes_id, version)
VALUES
  (:id, :user_id, :person_id, :attr_key, :value_text, :value_type, :confidence, :epistemic_type,
   :source, :status, :session_id, :memory_id, :transcript_segment_ids, :supersedes_id, :version)`, a)
	return err
}

func (r *PersonAttributeRepo) Create(ctx context.Context, a *PersonAttribute) error {
	return r.CreateExt(ctx, r.DB, a)
}

func (r *PersonAttributeRepo) Get(ctx context.Context, id ids.ID) (*PersonAttribute, error) {
	var a PersonAttribute
	err := r.DB.GetContext(ctx, &a, `SELECT * FROM person_attribute WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListByPerson 全状态列表（详情页要展示 active+pending，历史走 change_log），按 key、id 排序。
func (r *PersonAttributeRepo) ListByPerson(ctx context.Context, personID ids.ID) ([]PersonAttribute, error) {
	var list []PersonAttribute
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_attribute WHERE person_id = ? ORDER BY attr_key, id`, personID.Int64())
	return list, err
}

// FindActiveByKeyExt 单值型 key 的当前 active 行；无返回 nil。可在事务连接上执行。
func (r *PersonAttributeRepo) FindActiveByKeyExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, attrKey string) (*PersonAttribute, error) {
	var a PersonAttribute
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_attribute
WHERE person_id = ? AND attr_key = ? AND status = 'active'
ORDER BY id DESC LIMIT 1`, personID.Int64(), attrKey).StructScan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *PersonAttributeRepo) FindActiveByKey(ctx context.Context, personID ids.ID, attrKey string) (*PersonAttribute, error) {
	return r.FindActiveByKeyExt(ctx, r.DB, personID, attrKey)
}

// FindActiveByKeyValueExt 列表型 key 的同值 active 行（无则 nil）；闸门「同值→佐证」判定用。
func (r *PersonAttributeRepo) FindActiveByKeyValueExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, attrKey, value string) (*PersonAttribute, error) {
	var a PersonAttribute
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_attribute
WHERE person_id = ? AND attr_key = ? AND value_text = ? AND status = 'active'
ORDER BY id DESC LIMIT 1`, personID.Int64(), attrKey, value).StructScan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *PersonAttributeRepo) FindActiveByKeyValue(ctx context.Context, personID ids.ID, attrKey, value string) (*PersonAttribute, error) {
	return r.FindActiveByKeyValueExt(ctx, r.DB, personID, attrKey, value)
}

// FindByNaturalKeyExt 幂等去重查询：同 session 同 person 同 key 同原始值（任意 status）
// 已有行则返回该行——重跑同一 session 不重复建 pending / 不重复 bump（spec §6.3）。
func (r *PersonAttributeRepo) FindByNaturalKeyExt(ctx context.Context, ext QueryRowxContext, sessionID, personID ids.ID, attrKey, value string) (*PersonAttribute, error) {
	var a PersonAttribute
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_attribute
WHERE session_id = ? AND person_id = ? AND attr_key = ? AND value_text = ?
ORDER BY id LIMIT 1`, sessionID.Int64(), personID.Int64(), attrKey, value).StructScan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *PersonAttributeRepo) FindByNaturalKey(ctx context.Context, sessionID, personID ids.ID, attrKey, value string) (*PersonAttribute, error) {
	return r.FindByNaturalKeyExt(ctx, r.DB, sessionID, personID, attrKey, value)
}

func (r *PersonAttributeRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_attribute SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonAttributeRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// BumpConfidenceExt 佐证上调置信度，封顶 0.99（同 memory 的佐证模式）。
func (r *PersonAttributeRepo) BumpConfidenceExt(ctx context.Context, ext ExecerContext, id ids.ID, delta float64) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_attribute SET confidence = LEAST(confidence + ?, 0.99) WHERE id = ?`,
		delta, id.Int64())
	return err
}

func (r *PersonAttributeRepo) BumpConfidence(ctx context.Context, id ids.ID, delta float64) error {
	return r.BumpConfidenceExt(ctx, r.DB, id, delta)
}

// ListPending 全局确认队列（属性平面部分），按 id 升序（先产生的先确认）。
func (r *PersonAttributeRepo) ListPending(ctx context.Context, userID int64) ([]PersonAttribute, error) {
	var list []PersonAttribute
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_attribute WHERE user_id = ? AND status = 'pending' ORDER BY id`, userID)
	return list, err
}
