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
// topic 归属走关联表 memory_topic（多对多），本结构不再承载单值 topic_id。
type Memory struct {
	ID                   ids.ID     `db:"id" json:"id"`
	UserID               int64      `db:"user_id" json:"user_id"`
	Type                 string     `db:"type" json:"type"`
	Title                string     `db:"title" json:"title"`
	Content              string     `db:"content" json:"content"`
	EpistemicType        string     `db:"epistemic_type" json:"epistemic_type"`
	Importance           float64    `db:"importance" json:"importance"`
	Confidence           float64    `db:"confidence" json:"confidence"`
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

// MemoryRow 是带 topics 的列表视图（前端卡片直接展示归属）。
// Topics 由 attachTopics 在查询后填充（无 db tag，不参与 SQL 映射）。
type MemoryRow struct {
	Memory
	Topics []TopicInfo `json:"topics,omitempty"`
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

// InsertExt 批量插入（ext 传 *sqlx.Tx 即加入事务）。ID 在此生成（若调用方未预置）并回填。
// 必须传 *Memory 指针切片：值拷贝切片收不到回填的 ID。
func (r *MemoryRepo) InsertExt(ctx context.Context, ext ExecerContext, ms []*Memory) error {
	if len(ms) == 0 {
		return nil
	}
	for i := range ms {
		if ms[i].ID == 0 { // 尊重调用方预置 id（D1 佐证去重需在插入前知道新记忆 id）
			ms[i].ID = ids.New()
		}
		if ms[i].UserID == 0 {
			ms[i].UserID = 1
		}
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO memory (id, user_id, type, title, content, epistemic_type,
  importance, confidence, session_id, transcript_segment_ids, event_at, status)
VALUES (:id, :user_id, :type, :title, :content, :epistemic_type,
  :importance, :confidence, :session_id, :transcript_segment_ids, :event_at, :status)`, ms)
	return err
}

// DeleteBySessionExt 删除一个 session 的全部 memory（extract 重跑幂等用）。
// 必须与 InsertExt 等重跑写入共用同一 *sqlx.Tx，保证 delete+insert 原子
// （extract stage 单事务提交用）。
func (r *MemoryRepo) DeleteBySessionExt(ctx context.Context, ext ExecerContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx, `DELETE FROM memory WHERE session_id = ?`, sessionID.Int64())
	return err
}

// ListActiveTitlesExt 返回该用户全部 active memory 的 id 与标题（D1 佐证去重比对用）。
// 事务内调用传 tx（能看到本事务内 DeleteBySessionExt 已删的本 session 旧 memory，
// 避免重跑时本 session 旧记忆自去重导致幂等失败），事务外调用传 r.DB。
// 与 TodoRepo.ListOpenTitlesExt 同构（T3）。
func (r *MemoryRepo) ListActiveTitlesExt(ctx context.Context, q QueryerContext, userID int64) ([]struct {
	ID    ids.ID `db:"id"`
	Title string `db:"title"`
}, error) {
	var rows []struct {
		ID    ids.ID `db:"id"`
		Title string `db:"title"`
	}
	err := q.SelectContext(ctx, &rows,
		`SELECT id, title FROM memory WHERE user_id = ? AND status = 'active'`, userID)
	return rows, err
}

// BumpConfidenceExt 原子上调 memory 置信度（佐证 +delta，封顶 0.99）。
// SQL 原子算术（LEAST），不读-改-写，满足并发安全约束。ext 传 tx 即加入事务。
func (r *MemoryRepo) BumpConfidenceExt(ctx context.Context, ext ExecerContext, id ids.ID, delta float64) error {
	_, err := ext.ExecContext(ctx,
		`UPDATE memory SET confidence = LEAST(confidence + ?, 0.99) WHERE id = ?`, delta, id.Int64())
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
	if f.Since != nil {
		// 键里带操作符：listWhere 见到含空格的键会按原样拼接（见其注释）
		where["m.event_at >="] = *f.Since
	}
	return r.listWhere(ctx, where, f.TopicID, f.Limit, f.Offset)
}

func (r *MemoryRepo) ListBySession(ctx context.Context, sessionID ids.ID) ([]MemoryRow, error) {
	return r.listWhere(ctx, map[string]any{"m.session_id": sessionID.Int64()}, nil, 200, 0)
}

func (r *MemoryRepo) ListByTopic(ctx context.Context, topicID ids.ID) ([]MemoryRow, error) {
	var rows []MemoryRow
	err := r.DB.SelectContext(ctx, &rows, `
SELECT m.* FROM memory m
WHERE m.status != 'dismissed' AND m.id IN (SELECT memory_id FROM memory_topic WHERE topic_id = ?)
ORDER BY m.event_at DESC, m.id DESC LIMIT 200`, topicID.Int64())
	if err == nil {
		err = r.attachTopics(ctx, rows)
	}
	return rows, err
}

// listWhere 组装 WHERE（条件 AND 连接；map 迭代顺序不影响 AND 语义），
// 基础条件固定排除 dismissed。
// 键约定：默认按「列 = ?」生成；键中含空格（如 "m.event_at >="）视为
// 已带比较操作符，按「列 操作符 ?」拼接——目前仅 since 过滤用到 >=。
// topicID 非 nil 时追加关联表子查询过滤（不走 legacy memory.topic_id）。
func (r *MemoryRepo) listWhere(ctx context.Context, where map[string]any, topicID *ids.ID, limit, offset int) ([]MemoryRow, error) {
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
	if topicID != nil {
		cond += " AND m.id IN (SELECT memory_id FROM memory_topic WHERE topic_id = ?)"
		args = append(args, topicID.Int64())
	}
	if len(conds) > 0 {
		cond += " AND " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, offset)
	var rows []MemoryRow
	err := r.DB.SelectContext(ctx, &rows, fmt.Sprintf(`
SELECT m.* FROM memory m
WHERE %s
ORDER BY m.event_at DESC, m.id DESC
LIMIT ? OFFSET ?`, cond), args...)
	if err != nil {
		return nil, err
	}
	if err := r.attachTopics(ctx, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// attachTopics 给列表行内联 topics[]（走关联表多对多聚合，空列表安全）。
func (r *MemoryRepo) attachTopics(ctx context.Context, rows []MemoryRow) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]ids.ID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	m, err := (&MemoryTopicRepo{DB: r.DB}).ListByMemoryIDs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range rows {
		rows[i].Topics = m[rows[i].ID]
	}
	return nil
}
