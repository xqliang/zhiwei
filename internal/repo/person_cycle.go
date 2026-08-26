package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonCycle 是周期/日程平面（spec §4.6）：人物的周期性事项（生理期/服药/打针/复诊），
// 含「下次预测」（next_predicted_at = anchor_date + period_days，纯时间估算、非医疗建议，spec §9）。
// 敏感数据：本地存储、前端默认折叠展示。
//
// 与 metric 的区别：metric 是离散测点流，cycle 是「一个持续的周期性安排」——同一安排会随
// 用药调整/周期变化被新版本取代，故 cycle **有** supersedes_id（冲突 pending 指向当前 active 行），
// 语义与 attribute/relationship/event 一致，与 metric（无 supersedes_id）相反。
type PersonCycle struct {
	ID        ids.ID  `db:"id" json:"id"`
	UserID    int64   `db:"user_id" json:"user_id"`
	PersonID  ids.ID  `db:"person_id" json:"person_id"`
	CycleType string  `db:"cycle_type" json:"cycle_type"` // menstrual|medication|injection|followup
	Label     *string `db:"label" json:"label,omitempty"` // 药名/针名/'生理期'；自然键成分，NULL 视为「无标签」
	// AnchorDate 上次起始日（预测锚点）。DATE 列——映射 *time.Time，回读为当日 00:00（UTC）。
	AnchorDate    *time.Time `db:"anchor_date" json:"anchor_date,omitempty"`
	PeriodDays    *int       `db:"period_days" json:"period_days,omitempty"`     // 周期天数
	DurationDays  *int       `db:"duration_days" json:"duration_days,omitempty"` // 单次持续天数
	Dosage        *string    `db:"dosage" json:"dosage,omitempty"`
	FrequencyText *string    `db:"frequency_text" json:"frequency_text,omitempty"` // 频次（'每日两次'）
	// NextPredictedAt 下次预测日（= anchor+period，估算非医疗建议）。同为 DATE 列，映射 *time.Time。
	NextPredictedAt *time.Time `db:"next_predicted_at" json:"next_predicted_at,omitempty"`
	// ---- 横切字段（与既有平面一致，spec §3），含 SupersedesID ----
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

type PersonCycleRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。零值兜底同其他 repo。
// 注：Confidence==0 也兜底为 0.8——闸门已把低置信候选拦在门外，到这里的 0 视为漏填。
// 显式列出 20 列（created_at/updated_at 由 DB 默认值填充，不在写入之列）。
func (r *PersonCycleRepo) CreateExt(ctx context.Context, ext ExecerContext, c *PersonCycle) error {
	c.ID = ids.New()
	if c.UserID == 0 {
		c.UserID = 1
	}
	if c.Confidence == 0 {
		c.Confidence = 0.8
	}
	if c.EpistemicType == "" {
		c.EpistemicType = "observed"
	}
	if c.Source == "" {
		c.Source = "manual"
	}
	if c.Status == "" {
		c.Status = "active"
	}
	if c.Version == 0 {
		c.Version = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_cycle
  (id, user_id, person_id, cycle_type, label, anchor_date, period_days, duration_days, dosage,
   frequency_text, next_predicted_at, confidence, epistemic_type, source, status,
   session_id, memory_id, transcript_segment_ids, supersedes_id, version)
VALUES
  (:id, :user_id, :person_id, :cycle_type, :label, :anchor_date, :period_days, :duration_days, :dosage,
   :frequency_text, :next_predicted_at, :confidence, :epistemic_type, :source, :status,
   :session_id, :memory_id, :transcript_segment_ids, :supersedes_id, :version)`, c)
	return err
}

func (r *PersonCycleRepo) Create(ctx context.Context, c *PersonCycle) error {
	return r.CreateExt(ctx, r.DB, c)
}

// Get 按 id 查；不存在返回 (nil, nil)（与其他 repo 风格一致，调用方判 nil）。
func (r *PersonCycleRepo) Get(ctx context.Context, id ids.ID) (*PersonCycle, error) {
	var c PersonCycle
	err := r.DB.GetContext(ctx, &c, `SELECT * FROM person_cycle WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListByPerson 主体维度的全状态周期列表（详情页展示 active+pending，历史走 change_log）。
// 按 cycle_type 分组、组内按 id 排序，让同类周期（如多种药物）聚在一起，展示稳定。
func (r *PersonCycleRepo) ListByPerson(ctx context.Context, personID ids.ID) ([]PersonCycle, error) {
	var list []PersonCycle
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_cycle WHERE person_id = ? ORDER BY cycle_type, id`, personID.Int64())
	return list, err
}

// FindActiveByKeyExt 按（主体, 类型, 标签）找当前 active 行；供事务内判定「该周期是否已有
// active 版本」（有则走冲突/更新，无则新增）。label 可空且是自然键成分，故用 NULL 安全的 `<=>`：
// label 传 nil 命中 label IS NULL 的行（如无标签的生理期），传非 nil 命中等值行（如某药名）。
// 说明：普通 `= NULL` 恒为 UNKNOWN 永不命中，故对可空标签必须用 `<=>`（同 relationship 的对端匹配）。
// 无命中返回 (nil, nil)。只提供 Ext 版本：消费方全程在事务内，与其他平面的 FindActive*Ext 一致。
func (r *PersonCycleRepo) FindActiveByKeyExt(ctx context.Context, ext QueryRowxContext, personID ids.ID, cycleType string, label *string) (*PersonCycle, error) {
	var c PersonCycle
	// lb 为 nil 时绑定 SQL NULL，配合 <=> 命中 label IS NULL 的行。
	var lb any
	if label != nil {
		lb = *label
	}
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_cycle
WHERE person_id = ? AND cycle_type = ? AND label <=> ? AND status = 'active'
ORDER BY id DESC LIMIT 1`, personID.Int64(), cycleType, lb).StructScan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// FindByNaturalKeyExt 幂等去重：同 session 同（主体, 类型, 标签）任意 status 的行。
// 重跑同一 session 时命中已有周期，不重复建 pending / 不重复入库（spec §6.3）。
// anchor/period/dosage 等不进自然键（同一周期补充/调整这些字段视为同一条）。
// label 可空，同样用 NULL 安全的 `<=>` 匹配。无命中返回 (nil, nil)。
func (r *PersonCycleRepo) FindByNaturalKeyExt(ctx context.Context, ext QueryRowxContext, sessionID, personID ids.ID, cycleType string, label *string) (*PersonCycle, error) {
	var c PersonCycle
	var lb any
	if label != nil {
		lb = *label
	}
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_cycle
WHERE session_id = ? AND person_id = ? AND cycle_type = ? AND label <=> ?
ORDER BY id LIMIT 1`, sessionID.Int64(), personID.Int64(), cycleType, lb).StructScan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *PersonCycleRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_cycle SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonCycleRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// ListPending 全局确认队列（周期平面部分），按 id 升序（先产生的先确认）。
func (r *PersonCycleRepo) ListPending(ctx context.Context, userID int64) ([]PersonCycle, error) {
	var list []PersonCycle
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_cycle WHERE user_id = ? AND status = 'pending' ORDER BY id`, userID)
	return list, err
}

// CountPendingByPerson 统计某人物的 pending 周期数（供详情页/名册 pending 角标计数）。
// 与 metric 一致用轻量 COUNT（详情页 cycle 列表按需拉，计数不必拉全表）。
// person_id 全局唯一（雪花 ID），无需再带 user_id 限定。
func (r *PersonCycleRepo) CountPendingByPerson(ctx context.Context, personID ids.ID) (int, error) {
	var n int
	err := r.DB.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM person_cycle WHERE person_id = ? AND status = 'pending'`, personID.Int64())
	return n, err
}
