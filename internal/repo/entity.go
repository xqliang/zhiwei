package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// 实体 kind 枚举（entity_kb.kind）。custom=设置页手动录入的专有名词。
const (
	EntityKindPerson  = "person"  // 人物：person.display_name + 别名/称呼
	EntityKindPet     = "pet"     // 宠物：person_pet.name + nickname
	EntityKindProject = "project" // 项目：person_attribute(current_projects)
	EntityKindTask    = "task"    // 待办：todo.title（未关闭）
	EntityKindTopic   = "topic"   // 话题：topic.name（active）
	EntityKindSpeaker = "speaker" // 已登记说话人名：speaker.name（非随机名）
	EntityKindCustom  = "custom"  // 手动自定义
)

// AllEntityKinds entity kinds 全量清单（entity_settings.auto_sources 的默认值）。
// 注意：不含 custom——custom 只能手动录入，没有「自动入库来源」。
var AllEntityKinds = []string{
	EntityKindPerson, EntityKindPet, EntityKindProject, EntityKindTask, EntityKindTopic, EntityKindSpeaker,
}

// 实体来源枚举（entity_kb.source）。
const (
	EntitySourceAuto   = "auto"   // 流水线刷新重建（ReplaceAuto 全删全落）
	EntitySourceManual = "manual" // 设置页手动录入，刷新不动
)

// Entity 实体知识库一行（ASR 实体纠错的纠正目标）。
// canonical 的唯一键按 ai_ci（大小写不敏感）比对——召回层拉丁串统一小写归一化，
// 「Skynet」/「skynet」视为同一实体正是期望语义。
type Entity struct {
	ID        ids.ID    `db:"id" json:"id"`
	UserID    int64     `db:"user_id" json:"user_id"`
	Canonical string    `db:"canonical" json:"canonical"`
	Kind      string    `db:"kind" json:"kind"`
	Pinyin    *string   `db:"pinyin" json:"pinyin,omitempty"`
	Metaphone *string   `db:"metaphone" json:"metaphone,omitempty"`
	Source    string    `db:"source" json:"source"`
	SourceRef *string   `db:"source_ref" json:"source_ref,omitempty"`
	Enabled   bool      `db:"enabled" json:"enabled"`
	Note      *string   `db:"note" json:"note,omitempty"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// EntityKBRepo 实体知识库存取。所有方法都带 user_id 作用域（多用户隔离）。
type EntityKBRepo struct{ DB *sqlx.DB }

// entityCols 是各查询共享的列清单，避免多处手抄漏列。
const entityCols = `id, user_id, canonical, kind, pinyin, metaphone, source, source_ref, enabled, note, created_at, updated_at`

// ListEnabled 读某用户全部启用实体（correct stage 每会话一次；实体量级几十~几百，无需分页）。
func (r *EntityKBRepo) ListEnabled(ctx context.Context, userID int64) ([]Entity, error) {
	var list []Entity
	err := r.DB.SelectContext(ctx, &list,
		`SELECT `+entityCols+` FROM entity_kb WHERE user_id = ? AND enabled = 1 ORDER BY kind, canonical`, userID)
	return list, err
}

// List 按条件列实体（设置页用）：kind 空串=全部，含禁用行。
func (r *EntityKBRepo) List(ctx context.Context, userID int64, kind string) ([]Entity, error) {
	var list []Entity
	q := `SELECT ` + entityCols + ` FROM entity_kb WHERE user_id = ?`
	args := []any{userID}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY source, kind, canonical`
	err := r.DB.SelectContext(ctx, &list, q, args...)
	return list, err
}

// Get 读单条（带 user_id 作用域；不存在返回 sql.ErrNoRows）。
func (r *EntityKBRepo) Get(ctx context.Context, userID int64, id ids.ID) (*Entity, error) {
	var e Entity
	err := r.DB.GetContext(ctx, &e,
		`SELECT `+entityCols+` FROM entity_kb WHERE user_id = ? AND id = ?`, userID, id.Int64())
	return &e, err
}

// entityInsertSQL 落库语句（ReplaceAuto/CreateManual 共用）。
// 用 INSERT IGNORE：撞唯一键 uk_entity_kb (user_id, canonical, kind) 时静默跳过而非报错。
// 语义要点见 ReplaceAuto——与同名 manual 条目冲突时保留 manual（用户显式录入优先）。
const entityInsertSQL = `
INSERT IGNORE INTO entity_kb (id, user_id, canonical, kind, pinyin, metaphone, source, source_ref, enabled, note)
VALUES (:id, :user_id, :canonical, :kind, :pinyin, :metaphone, :source, :source_ref, :enabled, :note)`

// ReplaceAuto 原子重建某用户某 kind 的全部 auto 实体：事务内删旧 auto（该 kind）→ 落新。
// manual 条目（任何 kind）不动。len(list)==0 也执行删除（来源行已清空时同步清掉残留）。
// 入参实体的 ID/UserID/Kind/Source/Enabled 在此统一回填；pinyin/metaphone 由调用方（种子层）算好。
//
// 两条关键不变量：
//  1. 与 manual 条目同名时保留 manual：DELETE 只删该 kind 的 auto 行，同名 manual 幸存；
//     随后 INSERT IGNORE 撞唯一键（user_id, canonical, kind）→ 该 auto 行被静默跳过，
//     用户显式录入的 manual 优先（source/note 不被覆盖）。批内 canonical 重复也一并去重。
//  2. auto 条目的禁用状态跨刷新保留：刷新前先收集本 kind 中 enabled=0 的 canonical，
//     重插时对同名 canonical 回放 enabled=0——用户用 SetEnabled 禁掉的 auto 实体不会被
//     刷新静默重新启用。注意：删除（Delete）仍是一次性的——auto 删除后下次刷新会回来，
//     想让某 auto 实体持久不参与纠错请用禁用（SetEnabled false）而非删除。
func (r *EntityKBRepo) ReplaceAuto(ctx context.Context, userID int64, kind string, list []Entity) error {
	if !validEntityKind(kind) {
		return fmt.Errorf("非法实体 kind: %q", kind)
	}
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 刷新前收集本 kind 中被手动禁用（enabled=0）的 auto canonical，重插时回放禁用态。
	var disabledNames []string
	if err := tx.SelectContext(ctx, &disabledNames,
		`SELECT canonical FROM entity_kb WHERE user_id = ? AND kind = ? AND source = 'auto' AND enabled = 0`,
		userID, kind); err != nil {
		return err
	}
	disabled := make(map[string]bool, len(disabledNames))
	for _, n := range disabledNames {
		disabled[n] = true
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity_kb WHERE user_id = ? AND kind = ? AND source = 'auto'`, userID, kind); err != nil {
		return err
	}
	seen := make(map[string]bool, len(list)) // 批内 canonical 去重（种子层去重的兜底）
	for i := range list {
		if seen[list[i].Canonical] {
			continue
		}
		seen[list[i].Canonical] = true
		list[i].ID = ids.New()
		list[i].UserID = userID
		list[i].Kind = kind
		list[i].Source = EntitySourceAuto
		// 默认启用；若刷新前该 canonical 被手动禁用过，则回放 enabled=0（跨刷新保留禁用态）。
		list[i].Enabled = !disabled[list[i].Canonical]
		if _, err := tx.NamedExecContext(ctx, entityInsertSQL, list[i]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreateManual 设置页手动新增专有名词（kind=custom 或指定 kind）。回填 ID/Source=manual/Enabled=true。
// 撞唯一键（同名同 kind 已存在）时因 INSERT IGNORE 静默不插入、不报错。
func (r *EntityKBRepo) CreateManual(ctx context.Context, e *Entity) error {
	if !validEntityKind(e.Kind) {
		return fmt.Errorf("非法实体 kind: %q", e.Kind)
	}
	e.ID = ids.New()
	e.Source = EntitySourceManual
	e.Enabled = true // 新建的手动条目默认启用；禁用走 SetEnabled
	// INSERT IGNORE（与 ReplaceAuto 共用语句）撞唯一键时静默跳过、RowsAffected=0。
	// 手动创建须可感知重复：返回明确错误（handler → 400），否则调用方拿到从未落库的幻影 id。
	res, err := r.DB.NamedExecContext(ctx, entityInsertSQL, e)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("实体已存在（canonical=%s kind=%s 重复）", e.Canonical, e.Kind)
	}
	return nil
}

// UpdateManual 改手动实体的规范名/备注/匹配键。只能改 manual 条目；auto 条目（刷新重建）
// 返回 sql.ErrNoRows（RowsAffected=0 判定）。enabled 字段不经此方法（走 SetEnabled）。
// canonical 变更时调用方（handler 层）须用 entity.NormalizePinyin/NormalizeLatin 重算匹配键
// （pinyin/metaphone）一并写入，否则召回仍按旧键比对导致改名后失配。
func (r *EntityKBRepo) UpdateManual(ctx context.Context, userID int64, id ids.ID, canonical, note string, pinyin, metaphone *string) error {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE entity_kb SET canonical = ?, note = ?, pinyin = ?, metaphone = ? WHERE user_id = ? AND id = ? AND source = 'manual'`,
		canonical, entityNullStr(note), pinyin, metaphone, userID, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetEnabled 单条启禁（manual/auto 均可）。auto 条目被禁用后，其禁用态会在下次
// ReplaceAuto 刷新时按 canonical 回放保留（见 ReplaceAuto 不变量 2），不会被静默重启。
func (r *EntityKBRepo) SetEnabled(ctx context.Context, userID int64, id ids.ID, enabled bool) error {
	res, err := r.DB.ExecContext(ctx,
		`UPDATE entity_kb SET enabled = ? WHERE user_id = ? AND id = ?`, enabled, userID, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete 删除实体：manual 删除后永久消失；auto 删除是一次性的——下次 ReplaceAuto 刷新会
// 把它重新落回（且因删除后无 enabled=0 记录可回放，会以启用态回来）。想让某 auto 实体
// 持久不参与纠错，用禁用（SetEnabled false）而非删除。
func (r *EntityKBRepo) Delete(ctx context.Context, userID int64, id ids.ID) error {
	res, err := r.DB.ExecContext(ctx,
		`DELETE FROM entity_kb WHERE user_id = ? AND id = ?`, userID, id.Int64())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountByKind 按 kind 统计启用条数（设置页「自动入库来源」汇总用）。
func (r *EntityKBRepo) CountByKind(ctx context.Context, userID int64) (map[string]int, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT kind, COUNT(*) FROM entity_kb WHERE user_id = ? AND enabled = 1 GROUP BY kind`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		m[k] = n
	}
	return m, rows.Err()
}

// validEntityKind 校验 kind 是否为已知枚举（含 custom）。
func validEntityKind(k string) bool {
	switch k {
	case EntityKindPerson, EntityKindPet, EntityKindProject, EntityKindTask, EntityKindTopic, EntityKindSpeaker, EntityKindCustom:
		return true
	}
	return false
}

// entityNullStr 空串转 NULL（可空列语义：没填 = NULL）。
// 独立命名避免与包内其它辅助撞名。
func entityNullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// EntitySettings 实体纠错功能配置（每用户一行，无行=默认值）。
type EntitySettings struct {
	UserID              int64     `db:"user_id" json:"user_id"`
	CorrectionEnabled   bool      `db:"correction_enabled" json:"correction_enabled"`
	ConfidenceThreshold float64   `db:"confidence_threshold" json:"confidence_threshold"`
	AutoSources         []string  `db:"auto_sources" json:"auto_sources"`
	UpdatedAt           time.Time `db:"updated_at" json:"updated_at"`
}

// EntitySettingsRepo 实体纠错配置存取。
type EntitySettingsRepo struct{ DB *sqlx.DB }

// defaultEntitySettings 返回无行时的默认配置（enabled + 0.8 + 全量 kinds）。
func defaultEntitySettings(userID int64) EntitySettings {
	return EntitySettings{
		UserID:              userID,
		CorrectionEnabled:   true,
		ConfidenceThreshold: 0.8,
		AutoSources:         append([]string(nil), AllEntityKinds...),
	}
}

// Get 读配置；从未配置（无行）返回默认值而非错误（correct stage 直接可用）。
// auto_sources 列是 JSON：手动 Scan 到 []byte 再 Unmarshal（sqlx 对 JSON 列→[]string
// 不做自动转换）；库里空/脏数据退化为全量默认 kinds，不报错。
func (r *EntitySettingsRepo) Get(ctx context.Context, userID int64) (*EntitySettings, error) {
	var s EntitySettings
	var sources []byte
	err := r.DB.QueryRowxContext(ctx,
		`SELECT user_id, correction_enabled, confidence_threshold, auto_sources, updated_at
		 FROM entity_settings WHERE user_id = ?`, userID).
		Scan(&s.UserID, &s.CorrectionEnabled, &s.ConfidenceThreshold, &sources, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		d := defaultEntitySettings(userID)
		return &d, nil
	}
	if err != nil {
		return nil, err
	}
	if len(sources) > 0 {
		_ = json.Unmarshal(sources, &s.AutoSources)
	}
	if len(s.AutoSources) == 0 {
		s.AutoSources = append([]string(nil), AllEntityKinds...)
	}
	return &s, nil
}

// Upsert 写配置（单用户一行）。threshold 越界（∉[0,1]）在应用层拒绝。
// sources nil/空 = 恢复全量默认。custom 不允许出现在 auto_sources（它没有自动来源）。
func (r *EntitySettingsRepo) Upsert(ctx context.Context, userID int64, enabled bool, threshold float64, sources []string) error {
	if threshold < 0 || threshold > 1 {
		return fmt.Errorf("置信度阈值须在 [0,1]，got %v", threshold)
	}
	if len(sources) == 0 {
		sources = append([]string(nil), AllEntityKinds...)
	}
	for _, k := range sources {
		if !validEntityKind(k) || k == EntityKindCustom {
			return fmt.Errorf("auto_sources 不支持 kind: %q", k)
		}
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	_, err = r.DB.ExecContext(ctx, `
INSERT INTO entity_settings (user_id, correction_enabled, confidence_threshold, auto_sources)
VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE correction_enabled = VALUES(correction_enabled),
  confidence_threshold = VALUES(confidence_threshold), auto_sources = VALUES(auto_sources)`,
		userID, enabled, threshold, raw)
	return err
}
