package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonPet 是宠物平面：一只宠物一行，挂人物名下（spec 2026-08-27-pet-plane-design.md）。
// 自然键 = (person_id, name)：同人同名视为同一只（nickname 不参与匹配）；同名更新走
// 「字段级合并 + 整行 supersede」（版本取代语义，同 attribute 单值模式），不同名 = 新宠物追加。
// species 是开放枚举（狗|猫|鸟|鱼|兔|仓鼠|爬行|其他），缺省/非法由解析层收敛「其他」，列 NOT NULL。
// age_text 存原始表述（「3岁」「8个月」）；birthday 是 LLM 按年龄估算的 DATE（手动录入必填）。
type PersonPet struct {
	ID       ids.ID `db:"id" json:"id"`
	UserID   int64  `db:"user_id" json:"user_id"`
	PersonID ids.ID `db:"person_id" json:"person_id"`
	Name     string `db:"name" json:"name"` // 宠物名，NOT NULL
	// 以下可空列：LLM/用户没说就为 NULL（trim 空→nil，走 <=> NULL 匹配——对齐 activity 平面约定）。
	Nickname *string    `db:"nickname" json:"nickname,omitempty"` // 小名
	Species  string     `db:"species" json:"species"`             // 类别（狗|猫|…|其他），NOT NULL
	Breed    *string    `db:"breed" json:"breed,omitempty"`       // 品种自由文本
	Gender   *string    `db:"gender" json:"gender,omitempty"`     // 公|母
	AgeText  *string    `db:"age_text" json:"age_text,omitempty"` // 年龄原始表述
	Birthday *time.Time `db:"birthday" json:"birthday,omitempty"` // 生日（LLM 估算/手动必填）
	Likes    *string    `db:"likes" json:"likes,omitempty"`       // 喜好/习惯
	// ---- 横切字段（与 attribute/relationship/cycle 等平面一致，spec §3）----
	Confidence           float64   `db:"confidence" json:"confidence"`
	EpistemicType        string    `db:"epistemic_type" json:"epistemic_type"` // observed|inferred|predicted|suggested
	Source               string    `db:"source" json:"source"`                 // manual|llm
	Status               string    `db:"status" json:"status"`                 // active|pending|superseded|dismissed
	PreDismissStatus     *string   `db:"pre_dismiss_status" json:"pre_dismiss_status,omitempty"`
	SessionID            *ids.ID   `db:"session_id" json:"session_id,omitempty"`
	MemoryID             *ids.ID   `db:"memory_id" json:"memory_id,omitempty"`
	TranscriptSegmentIDs ids.List  `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	SupersedesID         *ids.ID   `db:"supersedes_id" json:"supersedes_id,omitempty"`
	Version              int       `db:"version" json:"version"`
	CreatedAt            time.Time `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time `db:"updated_at" json:"updated_at"`
}

type PersonPetRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。零值兜底同其他 repo。
// 注：Confidence==0 也兜底为 0.8——闸门已把低置信候选拦在门外，到这里的 0 视为漏填。
// 显式列出 20 列（created_at/updated_at 由 DB 默认值填充）。
func (r *PersonPetRepo) CreateExt(ctx context.Context, ext ExecerContext, p *PersonPet) error {
	p.ID = ids.New()
	if p.UserID == 0 {
		p.UserID = 1
	}
	if p.Species == "" {
		p.Species = "其他"
	}
	if p.Confidence == 0 {
		p.Confidence = 0.8
	}
	if p.EpistemicType == "" {
		p.EpistemicType = "observed"
	}
	if p.Source == "" {
		p.Source = "manual"
	}
	if p.Status == "" {
		p.Status = "active"
	}
	if p.Version == 0 {
		p.Version = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_pet
  (id, user_id, person_id, name, nickname, species, breed, gender, age_text, birthday, likes,
   confidence, epistemic_type, source, status, pre_dismiss_status, session_id, memory_id,
   transcript_segment_ids, supersedes_id, version)
VALUES
  (:id, :user_id, :person_id, :name, :nickname, :species, :breed, :gender, :age_text, :birthday, :likes,
   :confidence, :epistemic_type, :source, :status, :pre_dismiss_status, :session_id, :memory_id,
   :transcript_segment_ids, :supersedes_id, :version)`, p)
	return err
}

func (r *PersonPetRepo) Create(ctx context.Context, p *PersonPet) error {
	return r.CreateExt(ctx, r.DB, p)
}

// Get 按 id 查；不存在返回 (nil, nil)（与其他 repo 风格一致，调用方判 nil）。
func (r *PersonPetRepo) Get(ctx context.Context, id ids.ID) (*PersonPet, error) {
	var p PersonPet
	err := r.DB.GetContext(ctx, &p, `SELECT * FROM person_pet WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListByPerson 某人物全部宠物（全状态，按 id 升序=先养的在前）；详情/列表端点在
// handler 层过滤 active+pending（对齐 cycle 的 ListByPerson 约定）。
func (r *PersonPetRepo) ListByPerson(ctx context.Context, personID ids.ID) ([]PersonPet, error) {
	var list []PersonPet
	err := r.DB.SelectContext(ctx, &list,
		`SELECT * FROM person_pet WHERE person_id = ? ORDER BY id ASC`, personID.Int64())
	return list, err
}

// FindActiveByNameExt 同名现值：同 person、同 name 的 active 行（单值语义的「现值」查询，
// 对齐 cycle 的 FindActiveByKeyExt）。pending 同名行不算现值（冲突路径由 supersedes 指向现值）。
func (r *PersonPetRepo) FindActiveByNameExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, name string) (*PersonPet, error) {
	var p PersonPet
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_pet
WHERE person_id = ? AND name = ? AND status = 'active'
ORDER BY id DESC LIMIT 1`, personID.Int64(), name).StructScan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// FindByNaturalKeyExt 幂等去重：同 session、同 person、同 name 任意 status 的行。
// 重跑同一 session 时命中已有宠物，不重复入库（spec §6.3）。name 是 NOT NULL 精确匹配。
func (r *PersonPetRepo) FindByNaturalKeyExt(ctx context.Context, ext QueryRowxContext, sessionID, personID ids.ID, name string) (*PersonPet, error) {
	var p PersonPet
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_pet
WHERE session_id = ? AND person_id = ? AND name = ?
ORDER BY id LIMIT 1`, sessionID.Int64(), personID.Int64(), name).StructScan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PersonPetRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_pet SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonPetRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// DismissAllByPersonExt 人物删除级联：该人物在宠物平面上所有 active/pending 行批量置
// dismissed（pre_dismiss_status 记录原状态），返回受影响行数。语义与 activity 平面一致。
func (r *PersonPetRepo) DismissAllByPersonExt(ctx context.Context, ext ExecerContext, personID ids.ID) (int64, error) {
	res, err := ext.ExecContext(ctx,
		`UPDATE person_pet SET pre_dismiss_status = status, status = 'dismissed' WHERE person_id = ? AND status IN ('active','pending')`,
		personID.Int64())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RestoreArchivedExt 人物恢复级联：被级联 dismiss 的行翻回原状态并清标记（手动删的行
// pre_dismiss_status 为 NULL 不受影响）。语义与 activity 平面一致。
func (r *PersonPetRepo) RestoreArchivedExt(ctx context.Context, ext ExecerContext, personID ids.ID) (int64, error) {
	res, err := ext.ExecContext(ctx,
		`UPDATE person_pet SET status = pre_dismiss_status, pre_dismiss_status = NULL WHERE person_id = ? AND status = 'dismissed' AND pre_dismiss_status IS NOT NULL`,
		personID.Int64())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListPending 全局确认队列（宠物平面部分），按 id 升序（先产生的先确认）。
func (r *PersonPetRepo) ListPending(ctx context.Context, userID int64) ([]PersonPet, error) {
	var list []PersonPet
	err := r.DB.SelectContext(ctx, &list,
		`SELECT * FROM person_pet WHERE user_id = ? AND status = 'pending' ORDER BY id`, userID)
	return list, err
}

// CountPendingByPerson 统计某人物的 pending 宠物数（详情页角标）。
func (r *PersonPetRepo) CountPendingByPerson(ctx context.Context, personID ids.ID) (int, error) {
	var n int
	err := r.DB.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM person_pet WHERE person_id = ? AND status = 'pending'`, personID.Int64())
	return n, err
}
