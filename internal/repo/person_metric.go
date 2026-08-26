package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonMetric 是时序指标平面（spec §4.5）：人物的「测点流」——每个时间戳一行
// （情绪/状态/体重/熬夜/饮食/健康…），没有「当前值」概念，图表按时间轴铺开。
// 与 attribute/event 的区别：attribute 记「有过的」、event 记「某次发生的」、
// metric 记「某时刻测得的一个读数」。同一 metric_key 会有很多行（体重每天一条）。
//
// 注意：metric 表没有 supersedes_id 列——测点不存在「新版本取代旧版本」的语义
// （每个测点都是独立事实，改口就是新测点/或直接 dismiss 旧点），故本 struct 不含该字段。
type PersonMetric struct {
	ID        ids.ID `db:"id" json:"id"`
	UserID    int64  `db:"user_id" json:"user_id"`
	PersonID  ids.ID `db:"person_id" json:"person_id"`
	MetricKey string `db:"metric_key" json:"metric_key"` // emotion|state|weight|sleep_late|diet|health
	// ValueNum 数值副本（图表层用）：体重 72.5、熬夜 0/1 等。类别型测点为 NULL。
	ValueNum *float64 `db:"value_num" json:"value_num,omitempty"`
	// ValueText 展示/自然键用的值串。**双存约定**（本平面核心约定，务必遵守）：
	//   - 数值型测点：value_num 存数值副本（图表层用），value_text 同时存同一数值的
	//     fmt 字符串（如 "72.5"）——展示层优先取 value_text，自然键去重也用 value_text；
	//   - 类别型测点：value_num 为 NULL，value_text 存类别/描述串（如 "焦虑"、"火锅"）。
	// 即：展示层永远读 value_text，图表层永远读 value_num；数值型两列都填，类别型只填 text。
	// CreateExt 不负责把数值 fmt 成串——由 Service 落库时对数值型自行填好 value_num 与
	// value_text（fmt 串）两列，本 repo 原样写入（保持 DAO 纯粹、不掺业务格式化逻辑）。
	ValueText *string `db:"value_text" json:"value_text,omitempty"`
	Unit      *string `db:"unit" json:"unit,omitempty"` // kg / 次 等，可空
	// MeasuredAt 测点时间（NOT NULL）：LLM 未给具体时间时由 Service 落 session 时间。
	// 与横切字段的 CreatedAt 不同——MeasuredAt 是「读数发生的时刻」，用于时序排序/区间查询。
	MeasuredAt time.Time `db:"measured_at" json:"measured_at"`
	// ---- 横切字段（与 attribute/relationship/event 平面一致，spec §3）----
	// 注意：无 SupersedesID（见 struct 顶部说明）。
	Confidence           float64   `db:"confidence" json:"confidence"`
	EpistemicType        string    `db:"epistemic_type" json:"epistemic_type"` // observed|inferred|predicted|suggested
	Source               string    `db:"source" json:"source"`                 // manual|llm
	Status               string    `db:"status" json:"status"`                 // active|pending|superseded|dismissed
	SessionID            *ids.ID   `db:"session_id" json:"session_id,omitempty"`
	MemoryID             *ids.ID   `db:"memory_id" json:"memory_id,omitempty"`
	TranscriptSegmentIDs ids.List  `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	Version              int       `db:"version" json:"version"`
	CreatedAt            time.Time `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time `db:"updated_at" json:"updated_at"`
}

type PersonMetricRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。零值兜底同其他 repo。
// 注：Confidence==0 也兜底为 0.8——闸门已把低置信候选拦在门外，到这里的 0 视为漏填。
// 显式列出 16 列（created_at/updated_at 由 DB 默认值填充，不在写入之列）。
func (r *PersonMetricRepo) CreateExt(ctx context.Context, ext ExecerContext, m *PersonMetric) error {
	m.ID = ids.New()
	if m.UserID == 0 {
		m.UserID = 1
	}
	if m.Confidence == 0 {
		m.Confidence = 0.8
	}
	if m.EpistemicType == "" {
		m.EpistemicType = "observed"
	}
	if m.Source == "" {
		m.Source = "manual"
	}
	if m.Status == "" {
		m.Status = "active"
	}
	if m.Version == 0 {
		m.Version = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_metric
  (id, user_id, person_id, metric_key, value_num, value_text, unit, measured_at,
   confidence, epistemic_type, source, status, session_id, memory_id, transcript_segment_ids, version)
VALUES
  (:id, :user_id, :person_id, :metric_key, :value_num, :value_text, :unit, :measured_at,
   :confidence, :epistemic_type, :source, :status, :session_id, :memory_id, :transcript_segment_ids, :version)`, m)
	return err
}

func (r *PersonMetricRepo) Create(ctx context.Context, m *PersonMetric) error {
	return r.CreateExt(ctx, r.DB, m)
}

// Get 按 id 查；不存在返回 (nil, nil)（与其他 repo 风格一致，调用方判 nil）。
func (r *PersonMetricRepo) Get(ctx context.Context, id ids.ID) (*PersonMetric, error) {
	var m PersonMetric
	err := r.DB.GetContext(ctx, &m, `SELECT * FROM person_metric WHERE id = ?`, id.Int64())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListByPerson 时序查询：某人物某指标在 [from, to) 时间窗内的测点，按测点时间**升序**。
//   - metricKey == "" 表示不限指标（返回该人物全部指标的测点）；
//   - from/to 为**半开区间** [from, to)：measured_at >= from AND measured_at < to，
//     from/to 各自为 nil 时跳过对应边界（都 nil = 不限时间）。用半开区间是为了让相邻时间
//     窗口（如「本月」「下月」）无缝拼接且不重叠——to 取「下一窗口起点」即可，边界点归后一窗口。
//   - 全状态返回（详情/图表要展示 active+pending，历史走 change_log），不过滤 status。
//
// 排序特意用 ASC（升序）：图表按时间从左到右铺开读数，最早在前——这与 event/attribute 的
// ListByPerson 用 DESC（最新在前，列表阅读习惯）**相反**，是时序图表场景的刻意选择。
// 同一 measured_at 再按 id ASC 兜底（先建的在前），保证顺序稳定可复现。
func (r *PersonMetricRepo) ListByPerson(ctx context.Context, personID ids.ID, metricKey string, from, to *time.Time) ([]PersonMetric, error) {
	// 动态拼 WHERE：条件按需追加，参数与占位符一一对应。
	q := `SELECT * FROM person_metric WHERE person_id = ?`
	args := []any{personID.Int64()}
	if metricKey != "" {
		q += ` AND metric_key = ?`
		args = append(args, metricKey)
	}
	if from != nil {
		q += ` AND measured_at >= ?` // 半开区间左闭：含 from
		args = append(args, *from)
	}
	if to != nil {
		q += ` AND measured_at < ?` // 半开区间右开：不含 to
		args = append(args, *to)
	}
	q += ` ORDER BY measured_at ASC, id ASC`
	var list []PersonMetric
	err := r.DB.SelectContext(ctx, &list, q, args...)
	return list, err
}

// FindByNaturalKeyExt 幂等去重：同 session、同 person、同 metric_key、同值、同测点时刻的行
// （任意 status）。重跑同一 session 时命中已有测点，不重复入库（spec §6.3）。
//
// 值以字符串统一比较——按 value_text 匹配（数值型测点的值串按双存约定也在 value_text 里，
// 见 struct 说明；调用方对数值型先 fmt 成串再传入）。value_text 可空，故用 NULL 安全的 `<=>`：
// valueText 传 nil 命中 value_text IS NULL 的行，传非 nil 命中等值行（普通 `= NULL` 恒 UNKNOWN 永不命中）。
//
// measured_at 参与自然键：同一指标同一值、不同时刻是两个独立测点（今天 72.5kg 与明天 72.5kg
// 不能去重成一条），故必须带上时刻精确匹配（measured_at NOT NULL，用 `=`）。
// 无命中返回 (nil, nil)。
func (r *PersonMetricRepo) FindByNaturalKeyExt(ctx context.Context, ext QueryRowxContext, sessionID, personID ids.ID, metricKey string, valueText *string, measuredAt time.Time) (*PersonMetric, error) {
	var m PersonMetric
	// vt 为 nil 时绑定 SQL NULL，配合 <=> 命中 value_text IS NULL 的行。
	var vt any
	if valueText != nil {
		vt = *valueText
	}
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person_metric
WHERE session_id = ? AND person_id = ? AND metric_key = ? AND value_text <=> ? AND measured_at = ?
ORDER BY id LIMIT 1`, sessionID.Int64(), personID.Int64(), metricKey, vt, measuredAt).StructScan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *PersonMetricRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_metric SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonMetricRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// DismissAllByPersonExt 人物归档级联（spec §13 F5）：把该人物在**时序指标平面**上所有活跃态
// （active/pending）的行批量置 dismissed，返回受影响行数（RowsAffected）。供 ManualSetPersonStatus
// 归档分支在事务内调用（ext 传 *sqlx.Tx，随归档事务一起提交/回滚）。
//
// 级联语义——只动 active 与 pending：dismissed 已是终态不再改动；metric 是测点流、无版本取代
// 语义（本表无 supersedes_id，实践中不产生 superseded 行），但仍用与其他平面一致的
// status IN ('active','pending') 圈定活跃态——写法统一、且对未来可能出现的 superseded 行天然安全。
func (r *PersonMetricRepo) DismissAllByPersonExt(ctx context.Context, ext ExecerContext, personID ids.ID) (int64, error) {
	res, err := ext.ExecContext(ctx,
		`UPDATE person_metric SET status = 'dismissed' WHERE person_id = ? AND status IN ('active','pending')`,
		personID.Int64())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListPending 全局确认队列（时序指标平面部分），按 id 升序（先产生的先确认）。
func (r *PersonMetricRepo) ListPending(ctx context.Context, userID int64) ([]PersonMetric, error) {
	var list []PersonMetric
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_metric WHERE user_id = ? AND status = 'pending' ORDER BY id`, userID)
	return list, err
}

// CountPendingByPerson 统计某人物的 pending 测点数（供详情页/名册 pending 角标计数）。
// 详情页不拉 metric 全量列表（时序数据量大、按需查询），故用轻量 COUNT 而非拉表过滤。
// person_id 全局唯一（雪花 ID），无需再带 user_id 限定。
func (r *PersonMetricRepo) CountPendingByPerson(ctx context.Context, personID ids.ID) (int, error) {
	var n int
	err := r.DB.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM person_metric WHERE person_id = ? AND status = 'pending'`, personID.Int64())
	return n, err
}
