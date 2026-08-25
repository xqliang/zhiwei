package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"zhiwei/internal/ids"
)

// MemoryTopicLink 是 memory↔topic 多对多关联行。
type MemoryTopicLink struct {
	MemoryID  ids.ID    `db:"memory_id" json:"-"`
	TopicID   ids.ID    `db:"topic_id" json:"-"`
	Source    string    `db:"source" json:"source"` // ai|user
	CreatedAt time.Time `db:"created_at" json:"-"`
}

// TopicInfo 是给前端展示的 topic 摘要（列表行内联）。
type TopicInfo struct {
	ID     ids.ID `db:"id" json:"id"`
	Name   string `db:"name" json:"name"`
	Status string `db:"status" json:"status"`
	Source string `db:"source" json:"source"` // ai|user
}

type MemoryTopicRepo struct{ DB *sqlx.DB }

// InsertExt 批量插关联（INSERT IGNORE 幂等，PK 去重）。ext 传 *sqlx.Tx 入事务。
func (r *MemoryTopicRepo) InsertExt(ctx context.Context, ext ExecerContext, rows []*MemoryTopicLink) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := ext.NamedExecContext(ctx,
		`INSERT IGNORE INTO memory_topic (memory_id, topic_id, source) VALUES (:memory_id, :topic_id, :source)`, rows)
	return err
}

// AddLink 单条加关联（手动，source='user'）。幂等：已存在不报错不重复。
func (r *MemoryTopicRepo) AddLink(ctx context.Context, memoryID, topicID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`INSERT IGNORE INTO memory_topic (memory_id, topic_id, source) VALUES (?, ?, 'user')`,
		memoryID.Int64(), topicID.Int64())
	return err
}

// RemoveLink 单条移除关联。
func (r *MemoryTopicRepo) RemoveLink(ctx context.Context, memoryID, topicID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM memory_topic WHERE memory_id = ? AND topic_id = ?`, memoryID.Int64(), topicID.Int64())
	return err
}

// DeleteBySessionExt 删某 session 全部 memory 的关联（extract 重跑清理用，事务内，
// 须在删 memory 之前调用——子查询依赖 memory 行仍存在）。
func (r *MemoryTopicRepo) DeleteBySessionExt(ctx context.Context, ext ExecerContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx,
		`DELETE FROM memory_topic WHERE memory_id IN (SELECT id FROM memory WHERE session_id = ?)`,
		sessionID.Int64())
	return err
}

// DeleteByConversationExt 删该对话全部记忆的 topic 关联（对话抽取重跑清理用；
// 须先于删 memory——子查询依赖 memory 行仍存在）。
// 合法性：子查询的是 memory 表、删的是 memory_topic 表（不同表），MySQL 允许。
func (r *MemoryTopicRepo) DeleteByConversationExt(ctx context.Context, ext ExecerContext, convID ids.ID) error {
	_, err := ext.ExecContext(ctx,
		`DELETE FROM memory_topic WHERE memory_id IN (SELECT id FROM memory WHERE conversation_id = ?)`,
		convID.Int64())
	return err
}

// MemoryUserLink 是快照手动关联用的行（带自然键成分 segment_ids+title）。
type MemoryUserLink struct {
	TopicID    ids.ID   `db:"topic_id"`
	SegmentIDs ids.List `db:"transcript_segment_ids"`
	Title      string   `db:"title"`
}

// SnapshotUserBySessionExt 读取某 session 待删 memory 的 source='user' 关联，
// 带自然键成分，供 commit 按自然键重链（spec §6）。事务内读保证一致性。
func (r *MemoryTopicRepo) SnapshotUserBySessionExt(ctx context.Context, ext QueryerContext, sessionID ids.ID) ([]MemoryUserLink, error) {
	var rows []MemoryUserLink
	err := ext.SelectContext(ctx, &rows, `
SELECT mt.topic_id, m.transcript_segment_ids, m.title
FROM memory_topic mt JOIN memory m ON mt.memory_id = m.id
WHERE mt.source = 'user' AND m.session_id = ?`, sessionID.Int64())
	return rows, err
}

// ListByMemoryIDs 按一批 memory_id 聚合 topic 摘要（列表接口内联 topics[] 用）。
func (r *MemoryTopicRepo) ListByMemoryIDs(ctx context.Context, memIDs []ids.ID) (map[ids.ID][]TopicInfo, error) {
	out := map[ids.ID][]TopicInfo{}
	if len(memIDs) == 0 {
		return out, nil
	}
	q, args, err := sqlx.In(`
SELECT mt.memory_id AS owner_id, t.id, t.name, t.status, mt.source
FROM memory_topic mt JOIN topic t ON mt.topic_id = t.id
WHERE mt.memory_id IN (?)`, memIDs)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		OwnerID ids.ID `db:"owner_id"`
		TopicInfo
	}
	if err := r.DB.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.OwnerID] = append(out[x.OwnerID], x.TopicInfo)
	}
	return out, nil
}
