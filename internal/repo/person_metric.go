package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonMetric 是画像第 5 平面（spec §4.5）：时间序列个人指标——情绪/体重/睡眠/饮食/
// 健康等「连续测点」。与 person_event 同构，但为测点特化：
//   - value_num 存数值（体重 kg / 情绪 -1..1 / 睡眠 h），曲线只画非空者；
//   - value_text 存类别描述（情绪='焦虑' / 饮食='火锅'）；两者可同时或单独出现；
//   - unit 存单位（kg|h|…）；measured_at 为全精度测点时间（勿抹平到当天零点）。
//
// append-only：每测点一行，正常写入不做单值 supersede（supersedes_id 仅手动纠错用）；
// 自然键含 measured_at + 值（见 FindByPointExt），同一次抽取给出多条读数时不会互相塌缩。
type PersonMetric struct {
	ID        ids.ID `db:"id" json:"id"`
	UserID    int64  `db:"user_id" json:"user_id"`
	PersonID  ids.ID `db:"person_id" json:"person_id"`
	MetricKey string `db:"metric_key" json:"metric_key"` // emotion|weight|sleep|mood_energy|diet|health（catalog）
	// 可空数值/文本/单位：sqlx safe 模式把 NULL 扫进值类型会报错，故一律用指针，nil 即 NULL。
	ValueNum   *float64  `db:"value_num" json:"value_num,omitempty"`   // 数值读数；仅它非空的行才进曲线
	ValueText  *string   `db:"value_text" json:"value_text,omitempty"` // 类别描述读数
	Unit       *string   `db:"unit" json:"unit,omitempty"`             // kg|h|…
	MeasuredAt time.Time `db:"measured_at" json:"measured_at"`         // 测点时间（全精度）
	// ---- 横切字段（与 attribute/relationship/event 平面一致，spec §3）----
	Confidence           float64   `db:"confidence" json:"confidence"`         // 抽取确定性（与 value 载荷分离）
	EpistemicType        string    `db:"epistemic_type" json:"epistemic_type"` // observed|inferred|predicted|suggested
	Source               string    `db:"source" json:"source"`                 // manual|extract
	Status               string    `db:"status" json:"status"`                 // active|pending|superseded|dismissed
	// PreDismissStatus 人物删除级联 dismiss 前的状态（active|pending）；NULL=非级联（手动删/正常行）。
	// 恢复人物时据此区分「级联删的（要恢复）」和「手动删的（保持 dismissed）」，见 RestoreArchivedExt。
	// 合并对账（全范围）：由 000015_person_restore ALTER 引入，与 attribute/event/relationship/cycle/activity 一致。
	PreDismissStatus     *string   `db:"pre_dismiss_status" json:"pre_dismiss_status,omitempty"`
	SessionID            *ids.ID   `db:"session_id" json:"session_id,omitempty"`
	MemoryID             *ids.ID   `db:"memory_id" json:"memory_id,omitempty"`
	TranscriptSegmentIDs ids.List  `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	SupersedesID         *ids.ID   `db:"supersedes_id" json:"supersedes_id,omitempty"` // 仅手动纠错用（正常写入不置）
	Note                 *string   `db:"note" json:"note,omitempty"`
	Version              int       `db:"version" json:"version"`
	CreatedAt            time.Time `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time `db:"updated_at" json:"updated_at"`
}

type PersonMetricRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。append-only：每次都是新行，
// m.ID 为 0 时生成雪花 id（允许调用方预先指定 id）。零值兜底同其他 repo。
//
// 注意：confidence 不在兜底之列——DB 列虽有 DEFAULT 1.000，但该默认仅在「不写该列」时生效，
// 而这里显式写入 confidence，故其取值完全由调用方负责（extract 侧带出、manual 侧自填）；
// 若调用方漏填则如实落 0.000，不在 repo 层臆造置信度。
func (r *PersonMetricRepo) CreateExt(ctx context.Context, ext ExecerContext, m *PersonMetric) error {
	if m.ID == 0 {
		m.ID = ids.New()
	}
	if m.UserID == 0 {
		m.UserID = 1
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
	// TranscriptSegmentIDs 为 nil 时兜底为空数组（写 [] 而非 NULL），
	// 让前端稳定拿到数组、无需判 null。
	if m.TranscriptSegmentIDs == nil {
		m.TranscriptSegmentIDs = ids.List{}
	}
	// 显式列出 18 个业务列（created_at/updated_at 由 DB 默认值填充，不在写入之列）。
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_metric
  (id, user_id, person_id, metric_key, value_num, value_text, unit, measured_at,
   confidence, epistemic_type, source, status,
   session_id, memory_id, transcript_segment_ids, supersedes_id, note, version)
VALUES
  (:id, :user_id, :person_id, :metric_key, :value_num, :value_text, :unit, :measured_at,
   :confidence, :epistemic_type, :source, :status,
   :session_id, :memory_id, :transcript_segment_ids, :supersedes_id, :note, :version)`, m)
	return err
}

// Create 事务外便捷入口（走 r.DB），与其他 repo 一致。
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

// ListByPerson 主体维度的测点序列，只取 active+pending（superseded/dismissed 走 change_log）。
// 排序：先按 metric_key 分组，组内按 measured_at 升序——正是画时间序列曲线的天然顺序
// （从早到晚），且同一指标的读数聚在一起，前端拿到即可直接分组绘制。
func (r *PersonMetricRepo) ListByPerson(ctx context.Context, personID ids.ID) ([]PersonMetric, error) {
	var list []PersonMetric
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_metric
WHERE person_id = ? AND status IN ('active', 'pending')
ORDER BY metric_key, measured_at`, personID.Int64())
	return list, err
}

func (r *PersonMetricRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE person_metric SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonMetricRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// ListPending 全局确认队列（指标平面部分），按 measured_at 倒序（最近的测点先确认）。
func (r *PersonMetricRepo) ListPending(ctx context.Context, userID int64) ([]PersonMetric, error) {
	var list []PersonMetric
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person_metric WHERE user_id = ? AND status = 'pending' ORDER BY measured_at DESC`, userID)
	return list, err
}

// FindByPointExt 自然键去重（含时间 + 值）：定位同一测点——(主体, 指标, 测量时间, 数值, 文本)
// 完全一致且未被 dismissed 的行。供调用方「命中则幂等跳过」：重跑同一次抽取时，同一测点
// 不重复入库；而不同 measured_at 或不同读数则视为新测点、照常追加（append-only）。
//
// value_num / value_text 用 <=>（NULL 安全等号）比较：直接把 valueNum(*float64)、
// valueText(*string) 作参数传入即可——nil→NULL，<=> 会正确处理「两侧都 NULL 视为相等、
// 一侧 NULL 一侧非 NULL 视为不等」，无需在 Go 侧分支拼不同 SQL。
//
// 已 dismissed 的行被排除在外：用户拒绝过的测点，再次抽到时应作为新候选重新提出，
// 而非被旧的 dismissed 行挡住。无命中返回 (nil, nil)。
//
// 仅提供 Ext 版本：消费方（ApplyFacts）全程在事务内，与其他平面的 FindActive*Ext 一致。
// 形参 q 为 QueryerContext（仅 SelectContext），故以 LIMIT 1 + 切片判空实现「取一行或无」，
// 语义等同 event.Get 的 sql.ErrNoRows→(nil,nil)。
func (r *PersonMetricRepo) FindByPointExt(ctx context.Context, q QueryerContext, personID ids.ID, metricKey string, measuredAt time.Time, valueNum *float64, valueText *string) (*PersonMetric, error) {
	var list []PersonMetric
	err := q.SelectContext(ctx, &list, `
SELECT * FROM person_metric
WHERE person_id = ? AND metric_key = ? AND measured_at = ?
  AND value_num <=> ? AND value_text <=> ? AND status != 'dismissed'
LIMIT 1`, personID.Int64(), metricKey, measuredAt, valueNum, valueText)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// DismissAllByPersonExt 人物删除级联（spec §13 F5）：把该人物在**时序指标平面**上所有活跃态
// （active/pending）的行批量置 dismissed，返回受影响行数（RowsAffected）。供 ManualSetPersonStatus
// 删除分支在事务内调用（ext 传 *sqlx.Tx，随删除事务一起提交/回滚）。
//
// 同时把行 dismiss 前的状态记入 pre_dismiss_status（active|pending），供 RestoreArchivedExt
// 级联恢复——手动删除的行不走这里，pre_dismiss_status 保持 NULL，恢复时天然不被误恢复。
// 合并对账（全范围）：从 main 移植，与 attribute/event/relationship/cycle/activity 平面一致。
func (r *PersonMetricRepo) DismissAllByPersonExt(ctx context.Context, ext ExecerContext, personID ids.ID) (int64, error) {
	res, err := ext.ExecContext(ctx,
		`UPDATE person_metric SET pre_dismiss_status = status, status = 'dismissed' WHERE person_id = ? AND status IN ('active','pending')`,
		personID.Int64())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RestoreArchivedExt 人物恢复级联：把删除时被级联置 dismissed 的行翻回**原状态**
// （pre_dismiss_status 记录的 active|pending），并清空标记（防止残留导致下次恢复误判）。
// 只动 status='dismissed' AND pre_dismiss_status IS NOT NULL 的行——手动删除的行
// pre_dismiss_status 为 NULL，不受影响。供 ManualSetPersonStatus 恢复分支在事务内调用。
func (r *PersonMetricRepo) RestoreArchivedExt(ctx context.Context, ext ExecerContext, personID ids.ID) (int64, error) {
	res, err := ext.ExecContext(ctx,
		`UPDATE person_metric SET status = pre_dismiss_status, pre_dismiss_status = NULL WHERE person_id = ? AND status = 'dismissed' AND pre_dismiss_status IS NOT NULL`,
		personID.Int64())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
