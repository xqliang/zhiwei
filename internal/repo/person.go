package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Person 是画像主体（人物系统）：owner「我」+ 他人，可选关联 0/1 个声纹。
// 只被提到、从未录音的人（配偶/孩子）也能建档（speaker_id 为 NULL）。
// 状态机：active（正常）| pending（LLM 抽取自动新建，待确认）| merged（已并入他人，P1 不用）| dismissed（归档）。
type Person struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	DisplayName string    `db:"display_name" json:"display_name"`
	SpeakerID   *ids.ID   `db:"speaker_id" json:"speaker_id,omitempty"`
	IsOwner     bool      `db:"is_owner" json:"is_owner"`
	Summary     *string   `db:"summary" json:"summary,omitempty"`
	Source      string    `db:"source" json:"source"` // manual|llm
	Status      string    `db:"status" json:"status"` // active|pending|merged|dismissed
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// PersonWithPending 是名册列表的带计数视图（人物卡角标用）。
type PersonWithPending struct {
	Person
	PendingCount int `db:"pending_count" json:"pending_count"` // 该人物待确认的属性+关系数
}

type PersonRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上创建（ext 传 *sqlx.Tx 即加入事务）。零值兜底同其他 repo。
func (r *PersonRepo) CreateExt(ctx context.Context, ext ExecerContext, p *Person) error {
	p.ID = ids.New()
	if p.UserID == 0 {
		p.UserID = 1
	}
	if p.Source == "" {
		p.Source = "manual"
	}
	if p.Status == "" {
		p.Status = "active"
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person (id, user_id, display_name, speaker_id, is_owner, summary, source, status)
VALUES (:id, :user_id, :display_name, :speaker_id, :is_owner, :summary, :source, :status)`, p)
	return err
}

func (r *PersonRepo) Create(ctx context.Context, p *Person) error {
	return r.CreateExt(ctx, r.DB, p)
}

// Get 按 id 查人物，并强制 user_id 隔离（多租户越权防护）：SQL 追加 AND user_id = ?，
// 用 userID 读他人人物时命中 0 行。沿用本方法既有约定：未命中（含越权）返回 (nil, nil)，
// 供调用方判 nil 转 404（与 FindActiveByNameExt 风格一致）。
func (r *PersonRepo) Get(ctx context.Context, userID int64, id ids.ID) (*Person, error) {
	var p Person
	err := r.DB.GetContext(ctx, &p, `SELECT * FROM person WHERE id = ? AND user_id = ?`, id.Int64(), userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// List 返回非 dismissed 人物（含 pending，名册要展示），is_owner 优先 + 更新时间倒序。
func (r *PersonRepo) List(ctx context.Context, userID int64) ([]Person, error) {
	var list []Person
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person WHERE user_id = ? AND status != 'dismissed'
ORDER BY is_owner DESC, updated_at DESC`, userID)
	return list, err
}

// ListWithPending 名册 + 每人 pending 计数（全六平面：属性/关系/大事记/指标/周期/生活轨迹），
// 供名册角标——须与 /api/profile/pending 队列的并集口径一致，漏平面会少计。
func (r *PersonRepo) ListWithPending(ctx context.Context, userID int64) ([]PersonWithPending, error) {
	var list []PersonWithPending
	err := r.DB.SelectContext(ctx, &list, `
SELECT p.*,
  (SELECT COUNT(*) FROM person_attribute a WHERE a.person_id = p.id AND a.status = 'pending')
+ (SELECT COUNT(*) FROM person_relationship rel WHERE rel.person_id = p.id AND rel.status = 'pending')
+ (SELECT COUNT(*) FROM person_event e WHERE e.person_id = p.id AND e.status = 'pending')
+ (SELECT COUNT(*) FROM person_metric m WHERE m.person_id = p.id AND m.status = 'pending')
+ (SELECT COUNT(*) FROM person_cycle c WHERE c.person_id = p.id AND c.status = 'pending')
+ (SELECT COUNT(*) FROM person_activity act WHERE act.person_id = p.id AND act.status = 'pending') AS pending_count
FROM person p
WHERE p.user_id = ? AND p.status != 'dismissed'
ORDER BY p.is_owner DESC, p.updated_at DESC`, userID)
	return list, err
}

// ListDismissed 已删除人物（status=dismissed 软删行），供「已删除」折叠区展示 + 恢复入口。
// 与 List/ListWithPending 互补：那两个只回非 dismissed，这个只回 dismissed。更新时间倒序。
func (r *PersonRepo) ListDismissed(ctx context.Context, userID int64) ([]Person, error) {
	var list []Person
	err := r.DB.SelectContext(ctx, &list, `
SELECT * FROM person WHERE user_id = ? AND status = 'dismissed'
ORDER BY updated_at DESC`, userID)
	return list, err
}

// GetOwnerExt 返回 is_owner=1 的「我」；不存在返回 (nil, nil)。可在事务连接上执行。
func (r *PersonRepo) GetOwnerExt(ctx context.Context, ext QueryRowxContext, userID int64) (*Person, error) {
	var p Person
	err := ext.QueryRowxContext(ctx,
		`SELECT * FROM person WHERE user_id = ? AND is_owner = 1 LIMIT 1`, userID).StructScan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PersonRepo) GetOwner(ctx context.Context, userID int64) (*Person, error) {
	return r.GetOwnerExt(ctx, r.DB, userID)
}

// GetBySpeakerExt 按声纹找绑定人物；未绑定返回 (nil, nil)。
func (r *PersonRepo) GetBySpeakerExt(ctx context.Context, ext QueryRowxContext, speakerID ids.ID) (*Person, error) {
	var p Person
	err := ext.QueryRowxContext(ctx,
		`SELECT * FROM person WHERE speaker_id = ? LIMIT 1`, speakerID.Int64()).StructScan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PersonRepo) GetBySpeaker(ctx context.Context, speakerID ids.ID) (*Person, error) {
	return r.GetBySpeakerExt(ctx, r.DB, speakerID)
}

// FindByNameExt 按显示名精确匹配 active/pending 人物（画像归属解析用）；无命中返回 nil。
// 只查 display_name；别名匹配（aliases 属性）由上层 profile.Service 扩展，P1 先名精确。
func (r *PersonRepo) FindByNameExt(ctx context.Context, ext QueryRowxContext, userID int64, name string) (*Person, error) {
	var p Person
	err := ext.QueryRowxContext(ctx, `
SELECT * FROM person
WHERE user_id = ? AND display_name = ? AND status IN ('active','pending')
ORDER BY is_owner DESC, id LIMIT 1`, userID, name).StructScan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PersonRepo) FindByName(ctx context.Context, userID int64, name string) (*Person, error) {
	return r.FindByNameExt(ctx, r.DB, userID, name)
}

// UpdateExt 手动编辑：改名/换绑声纹/改备注（speakerID/summary 传 nil 即清空）。
// 可在指定执行器上执行（ext 传 *sqlx.Tx 即与审计同事务，避免变更已提交而审计丢失）。
func (r *PersonRepo) UpdateExt(ctx context.Context, ext ExecerContext, id ids.ID, name string, speakerID *ids.ID, summary *string) error {
	var sp any
	if speakerID != nil {
		sp = speakerID.Int64()
	}
	var sm any
	if summary != nil {
		sm = *summary
	}
	_, err := ext.ExecContext(ctx,
		`UPDATE person SET display_name = ?, speaker_id = ?, summary = ? WHERE id = ?`,
		name, sp, sm, id.Int64())
	return err
}

func (r *PersonRepo) Update(ctx context.Context, id ids.ID, name string, speakerID *ids.ID, summary *string) error {
	return r.UpdateExt(ctx, r.DB, id, name, speakerID, summary)
}

// SetStatusExt 人物状态流转，可在指定执行器上执行（ext 传 *sqlx.Tx 即与审计同事务）。
func (r *PersonRepo) SetStatusExt(ctx context.Context, ext ExecerContext, id ids.ID, status string) error {
	_, err := ext.ExecContext(ctx, `UPDATE person SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *PersonRepo) SetStatus(ctx context.Context, id ids.ID, status string) error {
	return r.SetStatusExt(ctx, r.DB, id, status)
}

// RecentSessionIDs 该人物画像信息溯源涉及的 session（人物页「最近互动」入口），
// 属性+关系两平面 UNION 去重。雪花 ID 时间有序，DESC 即最近优先。
func (r *PersonRepo) RecentSessionIDs(ctx context.Context, personID ids.ID, limit int) ([]ids.ID, error) {
	var out []ids.ID
	err := r.DB.SelectContext(ctx, &out, `
SELECT session_id FROM (
  SELECT session_id FROM person_attribute
   WHERE person_id = ? AND session_id IS NOT NULL AND status != 'dismissed'
  UNION
  SELECT session_id FROM person_relationship
   WHERE person_id = ? AND session_id IS NOT NULL AND status != 'dismissed'
) t ORDER BY session_id DESC LIMIT ?`, personID.Int64(), personID.Int64(), limit)
	return out, err
}

// EnsureOwnerForUser 幂等：确保 userID 域下存在 owner「我」（is_owner=1）。
// 已存在则 no-op；不存在则建 {UserID, DisplayName:"我", IsOwner:true}。
// 供新用户创建引导（cmd/zhiwei-adduser）与启动 bootstrap（user-1）复用。
func EnsureOwnerForUser(ctx context.Context, persons *PersonRepo, userID int64) error {
	owner, err := persons.GetOwner(ctx, userID)
	if err != nil {
		return err
	}
	if owner != nil {
		return nil
	}
	return persons.Create(ctx, &Person{UserID: userID, DisplayName: "我", IsOwner: true})
}

// EnsurePersonBootstrap 幂等回填（main.go 启动时调用，迁移 000005 之后）：
// ① user-1 无 is_owner=1 人物则建「我」（复用 EnsureOwnerForUser）；② 为每个未绑定的
// active speaker 建人物（display_name=声纹名，仍 user-1 域）。查后再建，重跑无副作用。
func EnsurePersonBootstrap(ctx context.Context, persons *PersonRepo, speakers *SpeakerRepo) error {
	if err := EnsureOwnerForUser(ctx, persons, 1); err != nil {
		return err
	}
	list, err := speakers.List(ctx)
	if err != nil {
		return err
	}
	for _, sp := range list {
		if sp.Status != "active" {
			continue
		}
		p, err := persons.GetBySpeaker(ctx, sp.ID)
		if err != nil {
			return err
		}
		if p != nil {
			continue
		}
		sid := sp.ID
		if err := persons.Create(ctx, &Person{DisplayName: sp.Name, SpeakerID: &sid}); err != nil {
			return err
		}
	}
	return nil
}
