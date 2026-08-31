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
	ID            ids.ID  `db:"id" json:"id"`
	UserID        int64   `db:"user_id" json:"user_id"`
	Type          string  `db:"type" json:"type"`
	Title         string  `db:"title" json:"title"`
	Content       string  `db:"content" json:"content"`
	EpistemicType string  `db:"epistemic_type" json:"epistemic_type"`
	Importance    float64 `db:"importance" json:"importance"`
	Confidence    float64 `db:"confidence" json:"confidence"`
	// SessionID 改为可空指针：对话来源的记忆此列为 NULL（见 spec §6.3）。
	// 录音来源仍写 session_id（stage_extract 传 &sessionID）。
	// 用指针而非值类型：sqlx safe 模式下 SELECT * 扫描 NULL 进非指针 int64 会报错。
	SessionID *ids.ID `db:"session_id" json:"session_id,omitempty"`
	// ConversationID 对话溯源（可空）：仅对话转记忆写此列，录音来源为 NULL。
	ConversationID       *ids.ID    `db:"conversation_id" json:"conversation_id,omitempty"`
	TranscriptSegmentIDs ids.List   `db:"transcript_segment_ids" json:"transcript_segment_ids"`
	EventAt              *time.Time `db:"event_at" json:"event_at,omitempty"`
	Status               string     `db:"status" json:"status"`
	// PersonID 记忆归属人（2026-08-31，迁移 000027）：录音来源记忆 = 来源段说话人绑定的
	// person（extract stage 多数投票），「思敏说的话是思敏的记忆」；对话来源/无法归属为 NULL。
	PersonID *ids.ID `db:"person_id" json:"person_id,omitempty"`
	// PersonName 归属人显示名（查询时 LEFT JOIN person 富化，非落库列；未 JOIN 的查询恒空）。
	PersonName string `db:"person_name" json:"person_name,omitempty"`
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
	// UserID 多租户隔离（必填）：List 强制按 m.user_id = ? 过滤，防越权读到他人记忆。
	// 为 0 时视为「未指定用户」，List 直接返回空结果（安全默认）——刻意不做
	// 「0 = 不过滤」的危险回退，避免调用方忘传 userID 时全表泄漏。
	UserID  int64
	Type    string
	TopicID *ids.ID
	Since   *time.Time // 事件时间下界（含等于），spec §4 的 since 过滤
	// Before 事件时间上界（不含）。可选（nil=无上界）：与 Since 配合可把
	// 时间窗口 [Since, Before) 完整下推到 SQL。这是新增字段，既有调用方留 nil
	// 即保持原语义（无上界），不受影响（详见 review.gather 的窗口化修复）。
	Before *time.Time
	Limit  int
	Offset int
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
  importance, confidence, session_id, transcript_segment_ids, event_at, status, person_id)
VALUES (:id, :user_id, :type, :title, :content, :epistemic_type,
  :importance, :confidence, :session_id, :transcript_segment_ids, :event_at, :status, :person_id)`, ms)
	return err
}

// DeleteBySessionExt 删除一个 session 的全部 memory（extract 重跑幂等用）。
// 必须与 InsertExt 等重跑写入共用同一 *sqlx.Tx，保证 delete+insert 原子
// （extract stage 单事务提交用）。
func (r *MemoryRepo) DeleteBySessionExt(ctx context.Context, ext ExecerContext, sessionID ids.ID) error {
	_, err := ext.ExecContext(ctx, `DELETE FROM memory WHERE session_id = ?`, sessionID.Int64())
	return err
}

// InsertConversationExt 批量插入对话来源的记忆：统一盖 conversation_id、session_id 置 NULL。
// 与 InsertExt 同构（ext 传 *sqlx.Tx 入事务；预置非零 id 被尊重，供批内 dedup/佐证引用）。
// 单独一条 INSERT（含 conversation_id 列、不含 session_id 列→默认 NULL），
// 保持 InsertExt 原样不变（session 路径不受影响）。
func (r *MemoryRepo) InsertConversationExt(ctx context.Context, ext ExecerContext, convID ids.ID, ms []*Memory) error {
	if len(ms) == 0 {
		return nil
	}
	for i := range ms {
		if ms[i].ID == 0 {
			ms[i].ID = ids.New()
		}
		if ms[i].UserID == 0 {
			ms[i].UserID = 1
		}
		cid := convID // 每条都指向同一对话；用局部变量取地址避免共享循环变量
		ms[i].ConversationID = &cid
		ms[i].SessionID = nil // 对话来源无 session
	}
	_, err := ext.NamedExecContext(ctx, `
INSERT INTO memory (id, user_id, type, title, content, epistemic_type,
  importance, confidence, conversation_id, transcript_segment_ids, event_at, status)
VALUES (:id, :user_id, :type, :title, :content, :epistemic_type,
  :importance, :confidence, :conversation_id, :transcript_segment_ids, :event_at, :status)`, ms)
	return err
}

// DeleteByConversationExt 删一个 conversation 的全部对话记忆（对话抽取重跑幂等；须与 Insert 同 tx）。
// 镜像 DeleteBySessionExt，只是过滤列换成 conversation_id。
func (r *MemoryRepo) DeleteByConversationExt(ctx context.Context, ext ExecerContext, convID ids.ID) error {
	_, err := ext.ExecContext(ctx, `DELETE FROM memory WHERE conversation_id = ?`, convID.Int64())
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

// Get 按 id 查记忆，并强制 user_id 隔离（多租户越权防护）：SQL 追加 AND user_id = ?，
// 用 userID 读他人记忆时命中 0 行，沿用 GetContext 既有语义返回 sql.ErrNoRows（handler 转 404）。
func (r *MemoryRepo) Get(ctx context.Context, userID int64, id ids.ID) (*Memory, error) {
	var m Memory
	err := r.DB.GetContext(ctx, &m, `SELECT * FROM memory WHERE id = ? AND user_id = ?`, id.Int64(), userID)
	return &m, err
}

// GetExt 事务内加行锁读取（SELECT ... FOR UPDATE）。写-提议闸门的 memory_update/dismiss
// 是「读-改-写全行」，必须在同一事务内锁行读，否则并发确认/并发编辑会丢更新、撞 version
// （评审 I2）。事务外调用（传 r.DB）时 FOR UPDATE 为 autocommit 无害。
func (r *MemoryRepo) GetExt(ctx context.Context, q QueryRowxContext, id ids.ID) (*Memory, error) {
	var m Memory
	err := q.QueryRowxContext(ctx, `SELECT * FROM memory WHERE id = ? FOR UPDATE`, id.Int64()).StructScan(&m)
	return &m, err
}

// SaveExt 是 Save 的事务版：SQL 与非事务版 Save 完全一致，只把执行器由 r.DB 换成 ext
// （传 *sqlx.Tx 即加入调用方事务）。供「写-提议闸门」的确认端点在与
// AgentProposalRepo.Resolve 同一事务内落库用：领域写 + 提议置终态原子提交（apply-once）。
//
// 注意（与 Save 保持一致的约束）：本 SQL 只更新 title/content/status/version，不含 type 列。
// memory.type 仅在插入时写定；如需改 type 需另加迁移 + 新方法，本期确认闸门的 memory_update
// 只落 title/content（详见 mcp_write_tools.go / proposals.go 的说明）。
func (r *MemoryRepo) SaveExt(ctx context.Context, ext ExecerContext, m *Memory) error {
	_, err := ext.ExecContext(ctx, `
UPDATE memory SET title = ?, content = ?, status = ?, version = ? WHERE id = ?`,
		m.Title, m.Content, m.Status, m.Version, m.ID.Int64())
	return err
}

// Save 保存用户修正（version 由调用方 +1 后整体写回）。委托 SaveExt（传 r.DB，非事务），
// 行为与重构前完全一致。
func (r *MemoryRepo) Save(ctx context.Context, m *Memory) error {
	return r.SaveExt(ctx, r.DB, m)
}

func (r *MemoryRepo) List(ctx context.Context, f MemoryFilter) ([]MemoryRow, error) {
	// 多租户隔离：UserID 必填。为 0 视为「未指定用户」，直接返回空而非全表扫描，
	// 避免调用方漏传 userID 时越权读到他人记忆（安全默认，见 MemoryFilter.UserID 注释）。
	if f.UserID == 0 {
		return nil, nil
	}
	// 把 user_id 作为普通等值条件塞进 where map，交由 listWhere 生成 "m.user_id = ?"。
	// 不改 listWhere 签名，从而不影响同样复用它的 ListBySession/ListByTopic（靠父键隔离）。
	where := map[string]any{"m.user_id": f.UserID}
	if f.Type != "" {
		where["m.type"] = f.Type
	}
	if f.Since != nil {
		// 键里带操作符：listWhere 见到含空格的键会按原样拼接（见其注释）
		where["m.event_at >="] = *f.Since
	}
	if f.Before != nil {
		// 事件时间上界（不含）：与 Since 一起把窗口 [Since, Before) 完整下推到 SQL，
		// 复用 listWhere 的「键自带操作符」机制（键含空格 → 按原样拼接为 "列 < ?"）。
		// 与 Since 键不同（">=" vs "<"），二者在 map 中并存互不覆盖。
		where["m.event_at <"] = *f.Before
	}
	return r.listWhere(ctx, where, f.TopicID, f.Limit, f.Offset)
}

func (r *MemoryRepo) ListBySession(ctx context.Context, sessionID ids.ID) ([]MemoryRow, error) {
	return r.listWhere(ctx, map[string]any{"m.session_id": sessionID.Int64()}, nil, 200, 0)
}

// ListByPerson 某归属人的记忆（人物页「相关记忆」小节用）：按事件时间倒序取最近 limit 条，
// 排除 dismissed。软删（dismissed）person 的记忆仍可查（人物恢复后即可见）。
func (r *MemoryRepo) ListByPerson(ctx context.Context, personID ids.ID, limit int) ([]MemoryRow, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	var rows []MemoryRow
	err := r.DB.SelectContext(ctx, &rows, `
SELECT m.*, COALESCE(p.display_name, '') AS person_name FROM memory m
LEFT JOIN person p ON p.id = m.person_id
WHERE m.person_id = ? AND m.status != 'dismissed'
ORDER BY m.event_at DESC, m.id DESC
LIMIT ?`, personID.Int64(), limit)
	if err != nil {
		return nil, err
	}
	if err := r.attachTopics(ctx, rows); err != nil {
		return nil, err
	}
	return rows, nil
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
	// 占位符顺序必须与 args 追加顺序严格一致。SQL 里 topic 子查询的 ? 排在 where 条件之前，
	// 故 args 也必须先追加 topicID、再追加 where 值（否则多租户 user_id 条件加入后，
	// topic 的 ? 会错绑到 user_id 值——修 TestMemoryListAndFilter 的 topic 过滤 bug）。
	var conds []string
	var args []any
	cond := "m.status != 'dismissed'"
	if topicID != nil {
		cond += " AND m.id IN (SELECT memory_id FROM memory_topic WHERE topic_id = ?)"
		args = append(args, topicID.Int64())
	}
	for col, val := range where {
		if strings.Contains(col, " ") {
			conds = append(conds, col+" ?") // 键自带操作符（如 >=）
		} else {
			conds = append(conds, col+" = ?")
		}
		args = append(args, val)
	}
	if len(conds) > 0 {
		cond += " AND " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, offset)
	// LEFT JOIN person 富化 person_name（记忆卡「👤谁的记忆」chip 用）；person_id 为 NULL
	// 或 person 已删（status 任意——软删的人保留历史记忆的显示名）时该列为 NULL → 空串。
	var rows []MemoryRow
	err := r.DB.SelectContext(ctx, &rows, fmt.Sprintf(`
SELECT m.*, COALESCE(p.display_name, '') AS person_name FROM memory m
LEFT JOIN person p ON p.id = m.person_id
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

// ListActive 返回该用户全部 active 记忆（排除 superseded/dismissed），按 event_at 倒序，
// 供 D2 整理 LLM 输入。limit 上限保护（默认/上限 500）。
func (r *MemoryRepo) ListActive(ctx context.Context, userID int64, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var rows []Memory
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM memory WHERE user_id = ? AND status = 'active' ORDER BY event_at DESC LIMIT ?`,
		userID, limit)
	return rows, err
}

// Search 按关键词（title/content LIKE）检索该用户 active 记忆，可选 type 过滤，
// 按 event_at 倒序。空 query 退化为仅 type 过滤。limit 默认/上限 50。
// 关键词做 LIKE 转义（% _ \），防止用户词里的通配符改变语义。
func (r *MemoryRepo) Search(ctx context.Context, userID int64, query, typ string, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	conds := []string{"user_id = ?", "status = 'active'"}
	args := []any{userID}
	if q := strings.TrimSpace(query); q != "" {
		esc := escapeLike(q)
		conds = append(conds, `(title LIKE ? ESCAPE '\\' OR content LIKE ? ESCAPE '\\')`)
		args = append(args, "%"+esc+"%", "%"+esc+"%")
	}
	if typ != "" {
		conds = append(conds, "type = ?")
		args = append(args, typ)
	}
	args = append(args, limit)
	var rows []Memory
	err := r.DB.SelectContext(ctx, &rows, `
SELECT * FROM memory WHERE `+strings.Join(conds, " AND ")+`
ORDER BY event_at DESC, id DESC LIMIT ?`, args...)
	return rows, err
}

// ListForEmbedding 返回该用户「active 且尚无 embedding」的记忆（backfill 待嵌队列）。
// 按 id 倒序、limit 上限 500。显式列出列（含 embedding），避免 SELECT * 顺序耦合。
func (r *MemoryRepo) ListForEmbedding(ctx context.Context, userID int64, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var rows []Memory
	err := r.DB.SelectContext(ctx, &rows, `
SELECT id, user_id, type, title, content, epistemic_type, importance, confidence,
  session_id, conversation_id, transcript_segment_ids, event_at, status, embedding, version, created_at, updated_at
FROM memory WHERE user_id = ? AND status = 'active' AND embedding IS NULL
ORDER BY id DESC LIMIT ?`, userID, limit)
	return rows, err
}

// ListEmbeddedCandidates 返回该用户「active 且已有 embedding」的记忆（含 embedding blob），
// 供检索时在 Go 侧暴力 cosine。按 event_at 倒序取最近 limit 条。limit 默认/上限 2000。
func (r *MemoryRepo) ListEmbeddedCandidates(ctx context.Context, userID int64, limit int) ([]Memory, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	var rows []Memory
	err := r.DB.SelectContext(ctx, &rows, `
SELECT * FROM memory WHERE user_id = ? AND status = 'active' AND embedding IS NOT NULL
ORDER BY event_at DESC, id DESC LIMIT ?`, userID, limit)
	return rows, err
}

// SetEmbeddingExt 写入某记忆的向量 blob（ext 传 tx 即入事务；backfill 用 r.DB 逐条提交）。
func (r *MemoryRepo) SetEmbeddingExt(ctx context.Context, ext ExecerContext, id ids.ID, blob []byte) error {
	_, err := ext.ExecContext(ctx, `UPDATE memory SET embedding = ? WHERE id = ?`, blob, id.Int64())
	return err
}

// escapeLike 转义 LIKE 通配符，使用户输入按字面量匹配（配合 SQL 默认 \ 转义符）。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// ConsolidationReq 是 D2 整理落库请求（用户编辑后的 LLM 提议）。
type ConsolidationReq struct {
	Merges      []MemoryMerge      `json:"merges"`
	Adjustments []MemoryAdjustment `json:"adjustments"`
}

// MemoryMerge：语义同一条事实的组。CanonicalID 保留 active，MemberIDs 并入后置 superseded。
type MemoryMerge struct {
	CanonicalID ids.ID   `json:"canonical_id"`
	MemberIDs   []ids.ID `json:"member_ids"`
}

// MemoryAdjustment：每条记忆的关系判定 + 理由 + 证据 memory id。
// Kind: corroborate(被佐证更可信)|contradict(被新信息否定)|outdated(被新信息取代应 superseded)。
type MemoryAdjustment struct {
	MemoryID    ids.ID   `json:"memory_id"`
	Kind        string   `json:"kind"`
	Reason      string   `json:"reason"`
	EvidenceIDs []ids.ID `json:"evidence_ids"`
}

// ApplyConsolidation 单事务落库整理：先 merges（member 的 memory_topic 关联迁到 canonical、
// 删 member 关联、member 置 superseded），后 adjustments（跳过已被 merge 置 superseded 的
// member；对其余 active 按 kind 规则算 confidence，SQL 原子）。merges 优先避免重复处理。
// 返回 (被 supersede 的 member 数, 应用的 confidence 调整数)。LLM 只判关系，confidence 数字
// 由规则算（可审计可复现）。
func (r *MemoryRepo) ApplyConsolidation(ctx context.Context, req ConsolidationReq) (merged, adjusted int, err error) {
	tx, err := r.DB.BeginTxx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 先 merges：记录被 supersede 的 member，adjustments 跳过它们
	superseded := map[ids.ID]bool{}
	for _, g := range req.Merges {
		canon := g.CanonicalID
		for _, mid := range g.MemberIDs {
			if mid == canon {
				continue
			}
			// member 的 memory_topic 关联迁到 canonical（INSERT IGNORE，PK 去重）
			if _, err := tx.ExecContext(ctx,
				`INSERT IGNORE INTO memory_topic (memory_id, topic_id, source)
				 SELECT ?, topic_id, source FROM memory_topic WHERE memory_id = ?`,
				canon.Int64(), mid.Int64()); err != nil {
				return 0, 0, err
			}
			// 删 member 的 memory_topic 关联
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM memory_topic WHERE memory_id = ?`, mid.Int64()); err != nil {
				return 0, 0, err
			}
			// member 置 superseded（行保留审计）
			if _, err := tx.ExecContext(ctx,
				`UPDATE memory SET status = 'superseded' WHERE id = ?`, mid.Int64()); err != nil {
				return 0, 0, err
			}
			superseded[mid] = true
			merged++
		}
	}
	// 后 adjustments：跳过已被 merge 置 superseded 的 member，对其余 active 按 kind 算 confidence
	for _, a := range req.Adjustments {
		if superseded[a.MemoryID] {
			continue
		}
		switch a.Kind {
		case "corroborate":
			if _, err := tx.ExecContext(ctx,
				`UPDATE memory SET confidence = LEAST(confidence + 0.05, 0.99) WHERE id = ?`,
				a.MemoryID.Int64()); err != nil {
				return 0, 0, err
			}
		case "contradict":
			if _, err := tx.ExecContext(ctx,
				`UPDATE memory SET confidence = GREATEST(confidence - 0.10, 0.10) WHERE id = ?`,
				a.MemoryID.Int64()); err != nil {
				return 0, 0, err
			}
		case "outdated":
			if _, err := tx.ExecContext(ctx,
				`UPDATE memory SET status = 'superseded', confidence = GREATEST(confidence * 0.5, 0.05) WHERE id = ?`,
				a.MemoryID.Int64()); err != nil {
				return 0, 0, err
			}
		default:
			continue // 未知 kind 跳过
		}
		adjusted++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return merged, adjusted, nil
}
