package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// AgentSkill 是一条已安装技能的元数据（磁盘真源在 <skillRoot>/<enabled|disabled>/<name>/，
// 本表是元数据镜像：查看/列表免读盘、启禁态持久化）。
type AgentSkill struct {
	ID          ids.ID    `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	DisplayName string    `db:"display_name" json:"display_name"`
	Source      string    `db:"source" json:"source"`
	Description string    `db:"description" json:"description"`
	Enabled     bool      `db:"enabled" json:"enabled"`
	Content     string    `db:"content" json:"content"`
	InstalledAt time.Time `db:"installed_at" json:"installed_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

type AgentSkillRepo struct{ DB *sqlx.DB }

// List 全部技能，启用在前、按安装时间。
func (r *AgentSkillRepo) List(ctx context.Context) ([]AgentSkill, error) {
	var rows []AgentSkill
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM agent_skill ORDER BY enabled DESC, installed_at ASC`)
	return rows, err
}

// Get 按 id 查。
func (r *AgentSkillRepo) Get(ctx context.Context, id ids.ID) (*AgentSkill, error) {
	var s AgentSkill
	err := r.DB.GetContext(ctx, &s, `SELECT * FROM agent_skill WHERE id = ?`, id.Int64())
	return &s, err
}

// Create 新增（雪花 ID）。name 唯一由 DB 约束保证。
// 安装即启用：新装技能落到磁盘 enabled/ 目录，故建行时强制 enabled=true（对齐迁移列
// DEFAULT 1）；这样即便调用方传的是 AgentSkill 零值（Enabled=false），也不会把 :enabled
// 绑成 0 覆盖掉默认启用，且回填后的内存结构体与库内行保持一致。禁用改走 SetEnabled。
// 与 MCPServerRepo.Create 在建行时强制 Builtin=false 同理。
func (r *AgentSkillRepo) Create(ctx context.Context, s *AgentSkill) error {
	s.ID = ids.New()
	s.Enabled = true
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO agent_skill (id, name, display_name, source, description, enabled, content)
VALUES (:id, :name, :display_name, :source, :description, :enabled, :content)`, s)
	return err
}

// SetEnabled 启/禁（仅 DB；磁盘 rename 由 service 层做）。不存在 → ErrNoRows。
func (r *AgentSkillRepo) SetEnabled(ctx context.Context, id ids.ID, enabled bool) error {
	res, err := r.DB.ExecContext(ctx, `UPDATE agent_skill SET enabled = ? WHERE id = ?`, enabled, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete 删行（磁盘删除由 service 层做）。不存在 → ErrNoRows。
func (r *AgentSkillRepo) Delete(ctx context.Context, id ids.ID) error {
	res, err := r.DB.ExecContext(ctx, `DELETE FROM agent_skill WHERE id = ?`, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
