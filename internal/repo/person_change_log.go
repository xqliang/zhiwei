package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// PersonChangeLog 是跨平面统一审计（spec §4.8）：谁（changed_by）、何时（created_at）、
// 从什么（old_value）改成什么（new_value）、关联哪条 timeline/事件（session_id/memory_id/
// transcript_segment_ids）。只追加，永不 update/delete——repo 不提供修改方法。
type PersonChangeLog struct {
	ID                   ids.ID    `db:"id" json:"id"`
	UserID               int64     `db:"user_id" json:"user_id"`
	PersonID             ids.ID    `db:"person_id" json:"person_id"`
	EntityKind           string    `db:"entity_kind" json:"entity_kind"` // person|attribute|relationship（P2+ 扩 event/metric/…）
	EntityID             *ids.ID   `db:"entity_id" json:"entity_id,omitempty"`
	AttrKey              *string   `db:"attr_key" json:"attr_key,omitempty"`
	ChangeType           string    `db:"change_type" json:"change_type"`       // create|update|confirm|dismiss|supersede|delete|reaffirm
	ChangedBy            string    `db:"changed_by" json:"changed_by"`         // user|llm
	OldValue             *string   `db:"old_value" json:"old_value,omitempty"` // JSON 快照文本（如 "医生"）
	NewValue             *string   `db:"new_value" json:"new_value,omitempty"`
	Confidence           *float64  `db:"confidence" json:"confidence,omitempty"`
	EpistemicType        *string   `db:"epistemic_type" json:"epistemic_type,omitempty"`
	SessionID            *ids.ID   `db:"session_id" json:"session_id,omitempty"`
	MemoryID             *ids.ID   `db:"memory_id" json:"memory_id,omitempty"`
	TranscriptSegmentIDs ids.List  `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	Note                 *string   `db:"note" json:"note,omitempty"`
	CreatedAt            time.Time `db:"created_at" json:"created_at"`
}

type PersonChangeLogRepo struct{ DB *sqlx.DB }

// CreateExt 在指定执行器上追加一条审计（ext 传 *sqlx.Tx 即加入事务）。
// 只追加语义：本 repo 刻意不提供 Update/Delete 方法（审计不可篡改）。
func (r *PersonChangeLogRepo) CreateExt(ctx context.Context, ext ExecerContext, l *PersonChangeLog) error {
	l.ID = ids.New()
	if l.UserID == 0 {
		l.UserID = 1
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO person_change_log
  (id, user_id, person_id, entity_kind, entity_id, attr_key, change_type, changed_by,
   old_value, new_value, confidence, epistemic_type, session_id, memory_id, transcript_segment_ids, note)
VALUES
  (:id, :user_id, :person_id, :entity_kind, :entity_id, :attr_key, :change_type, :changed_by,
   :old_value, :new_value, :confidence, :epistemic_type, :session_id, :memory_id, :transcript_segment_ids, :note)`, l)
	return err
}

func (r *PersonChangeLogRepo) Create(ctx context.Context, l *PersonChangeLog) error {
	return r.CreateExt(ctx, r.DB, l)
}

// ListByPerson 人物的全平面审计历史，按 id（≈时间）正序；
// entityKind/attrKey 为空则不过滤（供详情页「变更历史」按平面或按字段下钻）。
func (r *PersonChangeLogRepo) ListByPerson(ctx context.Context, personID ids.ID, entityKind, attrKey string) ([]PersonChangeLog, error) {
	q := `SELECT * FROM person_change_log WHERE person_id = ?`
	args := []any{personID.Int64()}
	if entityKind != "" {
		q += ` AND entity_kind = ?`
		args = append(args, entityKind)
	}
	if attrKey != "" {
		q += ` AND attr_key = ?`
		args = append(args, attrKey)
	}
	q += ` ORDER BY id`
	var list []PersonChangeLog
	err := r.DB.SelectContext(ctx, &list, q, args...)
	return list, err
}
