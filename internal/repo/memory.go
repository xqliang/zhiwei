package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Memory 是从对话中抽取的一条记忆。embedding 列 Sprint 3 启用，本期留空。
type Memory struct {
	ID                   ids.ID     `db:"id" json:"id"`
	UserID               int64      `db:"user_id" json:"user_id"`
	Type                 string     `db:"type" json:"type"`
	Title                string     `db:"title" json:"title"`
	Content              string     `db:"content" json:"content"`
	EpistemicType        string     `db:"epistemic_type" json:"epistemic_type"`
	Importance           float64    `db:"importance" json:"importance"`
	Confidence           float64    `db:"confidence" json:"confidence"`
	TopicID              *ids.ID    `db:"topic_id" json:"topic_id,omitempty"`
	SessionID            ids.ID     `db:"session_id" json:"session_id"`
	TranscriptSegmentIDs ids.List   `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	EventAt              *time.Time `db:"event_at" json:"event_at,omitempty"`
	Status               string     `db:"status" json:"status"`
	// Embedding 本期恒 NULL（Sprint 3 启用）；必须保留 db 映射，
	// 否则 SELECT * 会因缺目标列报 missing destination name。
	Embedding []byte    `db:"embedding" json:"-"`
	Version   int       `db:"version" json:"version"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// MemoryRow 是带 topic 名称的列表视图（前端卡片直接展示归属）。
type MemoryRow struct {
	Memory
	TopicName *string `db:"topic_name" json:"topic_name,omitempty"`
}

// MemoryFilter 是列表查询条件，零值字段不参与过滤。
type MemoryFilter struct {
	Type    string
	TopicID *ids.ID
	Since   *time.Time // 事件时间下界（含等于），spec §4 的 since 过滤
	Limit   int
	Offset  int
}

type MemoryRepo struct{ DB *sqlx.DB }

// InsertExt 批量插入（ext 传 *sqlx.Tx 即加入事务）。ID 在此生成并回填。
// 必须传 *Memory 指针切片：值拷贝切片收不到回填的 ID。
func (r *MemoryRepo) InsertExt(ctx context.Context, ext ExecerContext, ms []*Memory) error {
	if len(ms) == 0 {
		return nil
	}
	for i := range ms {
		ms[i].ID = ids.New()
		if ms[i].UserID == 0 {
			ms[i].UserID = 1
		}
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO memory (id, user_id, type, title, content, epistemic_type,
  importance, confidence, topic_id, session_id, transcript_segment_ids, event_at, status)
VALUES (:id, :user_id, :type, :title, :content, :epistemic_type,
  :importance, :confidence, :topic_id, :session_id, :transcript_segment_ids, :event_at, :status)`, ms)
	return err
}

// DeleteBySessionExt 删除一个 session 的全部 memory（extract 重跑幂等用）。
// 必须与 InsertExt 等重跑写入共用同一 *sqlx.Tx，保证 delete+insert 原子
// （extract stage 单事务提交用）。
func (r *MemoryRepo) DeleteBySessionExt(ctx context.Context, ext ExecerContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx, `DELETE FROM memory WHERE session_id = ?`, sessionID.Int64())
	return err
}

func (r *MemoryRepo) Get(ctx context.Context, id ids.ID) (*Memory, error) {
	var m Memory
	err := r.DB.GetContext(ctx, &m, `SELECT * FROM memory WHERE id = ?`, id.Int64())
	return &m, err
}

// Save 保存用户修正（version 由调用方 +1 后整体写回）。
func (r *MemoryRepo) Save(ctx context.Context, m *Memory) error {
	_, err := r.DB.ExecContext(ctx, `
UPDATE memory SET title = ?, content = ?, status = ?, version = ? WHERE id = ?`,
		m.Title, m.Content, m.Status, m.Version, m.ID.Int64())
	return err
}

func (r *MemoryRepo) List(ctx context.Context, f MemoryFilter) ([]MemoryRow, error) {
	where := map[string]any{}
	if f.Type != "" {
		where["m.type"] = f.Type
	}
	if f.TopicID != nil {
		where["m.topic_id"] = f.TopicID.Int64()
	}
	if f.Since != nil {
		// 键里带操作符：listWhere 见到含空格的键会按原样拼接（见其注释）
		where["m.event_at >="] = *f.Since
	}
	return r.listWhere(ctx, where, f.Limit, f.Offset)
}

func (r *MemoryRepo) ListBySession(ctx context.Context, sessionID ids.ID) ([]MemoryRow, error) {
	return r.listWhere(ctx, map[string]any{"m.session_id": sessionID.Int64()}, 200, 0)
}

func (r *MemoryRepo) ListByTopic(ctx context.Context, topicID ids.ID) ([]MemoryRow, error) {
	return r.listWhere(ctx, map[string]any{"m.topic_id": topicID.Int64()}, 200, 0)
}

// listWhere 组装 WHERE（条件 AND 连接；map 迭代顺序不影响 AND 语义），
// 基础条件固定排除 dismissed。
// 键约定：默认按「列 = ?」生成；键中含空格（如 "m.event_at >="）视为
// 已带比较操作符，按「列 操作符 ?」拼接——目前仅 since 过滤用到 >=。
func (r *MemoryRepo) listWhere(ctx context.Context, where map[string]any, limit, offset int) ([]MemoryRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var conds []string
	var args []any
	for col, val := range where {
		if strings.Contains(col, " ") {
			conds = append(conds, col+" ?") // 键自带操作符（如 >=）
		} else {
			conds = append(conds, col+" = ?")
		}
		args = append(args, val)
	}
	cond := "m.status != 'dismissed'"
	if len(conds) > 0 {
		cond += " AND " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, offset)
	var rows []MemoryRow
	err := r.DB.SelectContext(ctx, &rows, fmt.Sprintf(`
SELECT m.*, t.name AS topic_name
FROM memory m LEFT JOIN topic t ON m.topic_id = t.id
WHERE %s
ORDER BY m.event_at DESC, m.id DESC
LIMIT ? OFFSET ?`, cond), args...)
	return rows, err
}
