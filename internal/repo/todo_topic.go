package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"zhiwei/internal/ids"
)

type TodoTopicLink struct {
	TodoID    ids.ID    `db:"todo_id" json:"-"`
	TopicID   ids.ID    `db:"topic_id" json:"-"`
	Source    string    `db:"source" json:"source"`
	CreatedAt time.Time `db:"created_at" json:"-"`
}

type TodoTopicRepo struct{ DB *sqlx.DB }

func (r *TodoTopicRepo) InsertExt(ctx context.Context, ext ExecerContext, rows []*TodoTopicLink) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := ext.NamedExecContext(ctx,
		`INSERT IGNORE INTO todo_topic (todo_id, topic_id, source) VALUES (:todo_id, :topic_id, :source)`, rows)
	return err
}

func (r *TodoTopicRepo) AddLink(ctx context.Context, todoID, topicID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`INSERT IGNORE INTO todo_topic (todo_id, topic_id, source) VALUES (?, ?, 'user')`,
		todoID.Int64(), topicID.Int64())
	return err
}

func (r *TodoTopicRepo) RemoveLink(ctx context.Context, todoID, topicID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM todo_topic WHERE todo_id = ? AND topic_id = ?`, todoID.Int64(), topicID.Int64())
	return err
}

// DeleteBySessionExt 删某 session 派生 todo 的关联（事务内，须在删 todo 之前调用）。
func (r *TodoTopicRepo) DeleteBySessionExt(ctx context.Context, ext ExecerContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx, `
DELETE FROM todo_topic WHERE todo_id IN (
  SELECT t.id FROM todo t
  JOIN memory m ON t.source_memory_id = m.id WHERE m.session_id = ?)`, sessionID.Int64())
	return err
}

// TodoUserLink 快照行：自然键成分取自 source memory（todo 无自身 segment）。
type TodoUserLink struct {
	TopicID    ids.ID  `db:"topic_id"`
	SegmentIDs ids.List `db:"transcript_segment_ids"`
	Title      string  `db:"title"` // 来源 memory 的 title（候选共享 title）
}

func (r *TodoTopicRepo) SnapshotUserBySessionExt(ctx context.Context, ext QueryerContext, sessionID ids.ID) ([]TodoUserLink, error) {
	var rows []TodoUserLink
	err := ext.SelectContext(ctx, &rows, `
SELECT tt.topic_id, m.transcript_segment_ids, m.title
FROM todo_topic tt
JOIN todo t ON tt.todo_id = t.id
JOIN memory m ON t.source_memory_id = m.id
WHERE tt.source = 'user' AND m.session_id = ?`, sessionID.Int64())
	return rows, err
}

func (r *TodoTopicRepo) ListByTodoIDs(ctx context.Context, todoIDs []ids.ID) (map[ids.ID][]TopicInfo, error) {
	out := map[ids.ID][]TopicInfo{}
	if len(todoIDs) == 0 {
		return out, nil
	}
	q, args, err := sqlx.In(`
SELECT tt.todo_id AS owner_id, t.id, t.name, t.status, tt.source
FROM todo_topic tt JOIN topic t ON tt.topic_id = t.id
WHERE tt.todo_id IN (?)`, todoIDs)
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
