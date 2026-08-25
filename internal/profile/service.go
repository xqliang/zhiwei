package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// ErrNotFound 目标行不存在（API 层映射 404）。
var ErrNotFound = errors.New("记录不存在")

// Service 是画像域的编排服务：pipeline profile stage 与 API（回填/确认/手动 CRUD）
// 共用同一入口，保证「写必带审计 + 单事务 + 闸门」三件事只实现一次。
type Service struct {
	DB            *sqlx.DB
	Sessions      *repo.SessionRepo // ExtractSession 用（Task 13）
	Transcripts   *repo.TranscriptRepo
	Memories      *repo.MemoryRepo
	Speakers      *repo.SpeakerRepo
	Persons       *repo.PersonRepo
	Attributes    *repo.PersonAttributeRepo
	Relationships *repo.PersonRelationshipRepo
	Events        *repo.PersonEventRepo // event 平面（P2 大事记）
	ChangeLogs    *repo.PersonChangeLogRepo

	LLM           provider.LLMProvider // ExtractSession 用（Task 13）；手动 CRUD 不需要
	Model         string
	Prompt        string
	PromptVersion string
	Window        int
	Gate          GateConfig
}

// Provenance 一条事实的溯源信息。
type Provenance struct {
	SessionID  ids.ID
	SegmentIDs []ids.ID
}

// ApplyStats 一次 ApplyFacts 的决策统计（trace 与日志用）。
type ApplyStats struct {
	Total      int
	Active     int // 直接写入 active
	Pending    int // 低置信/冲突待确认
	Reaffirmed int // 同值佐证（置信度上调）
	Conflicts  int // Pending 中的冲突条数
	Skipped    int // 幂等跳过 / 主体解析不到
}

// ApplyFacts 把一批 LLM 事实应用到库：人物归属解析 → 闸门 → 单事务写入
// （含 change_log）。幂等靠自然键去重（spec §6.3）——同 session 重跑不重复
// 建 pending、不重复 bump；用户此前的 confirm/dismiss 决定保留。
func (s *Service) ApplyFacts(ctx context.Context, sessionID ids.ID, userID int64, facts []Fact) (ApplyStats, error) {
	var st ApplyStats
	st.Total = len(facts)
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后 Rollback 是 no-op

	// 本 session 的 memories：供 memory_id 溯源（按 segment 交集最大匹配）。
	// 事务外读即可（只读，不依赖事务内一致性）。
	memRows, err := s.Memories.ListBySession(ctx, sessionID)
	if err != nil {
		return st, fmt.Errorf("读 session memories: %w", err)
	}

	for _, f := range facts {
		prov := Provenance{SessionID: sessionID, SegmentIDs: f.SegmentIDs}
		if err := s.applyFact(ctx, tx, userID, f, prov, memRows, &st); err != nil {
			return st, fmt.Errorf("应用事实(plane=%s key=%s relation=%s subject=%s): %w",
				f.Plane, f.AttrKey, f.RelationType, f.Subject.Kind, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

func (s *Service) applyFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	prov Provenance, memRows []repo.MemoryRow, st *ApplyStats) error {

	personID, err := s.resolveSubject(ctx, tx, f.Subject, prov)
	if err != nil {
		return err
	}
	if personID == 0 {
		st.Skipped++ // 主体解析不到（如无名 relation 指代且查不到对端）
		return nil
	}
	memID := matchMemory(memRows, f.SegmentIDs)

	if f.Plane == "event" {
		return s.applyEventFact(ctx, tx, userID, f, personID, memID, prov, st)
	}
	if f.Plane == "relationship" {
		return s.applyRelationshipFact(ctx, tx, userID, f, personID, memID, prov, st)
	}
	return s.applyAttributeFact(ctx, tx, userID, f, personID, memID, prov, st)
}

// ---- 属性平面 ----

func (s *Service) applyAttributeFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	d := Def(f.AttrKey)
	isList := d.Cardinality == CardinalityList

	var existing *repo.PersonAttribute
	var err error
	if isList {
		existing, err = s.Attributes.FindActiveByKeyValueExt(ctx, tx, personID, f.AttrKey, f.Value)
	} else {
		existing, err = s.Attributes.FindActiveByKeyExt(ctx, tx, personID, f.AttrKey)
	}
	if err != nil {
		return err
	}
	dedup, err := s.Attributes.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID, f.AttrKey, f.Value)
	if err != nil {
		return err
	}

	switch DecideAttribute(f, existing, isList, dedup != nil, s.Gate) {
	case DecisionSkip:
		st.Skipped++
	case DecisionReaffirm:
		if err := s.Attributes.BumpConfidenceExt(ctx, tx, existing.ID, 0.05); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, reaffirmAttrLog(personID, existing, memID, prov)); err != nil {
			return err
		}
		st.Reaffirmed++
	case DecisionCreateActive:
		row := attrRow(userID, personID, f, d, "active", nil, memID, prov)
		if err := s.Attributes.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createAttrLog(personID, row, memID, prov, "")); err != nil {
			return err
		}
		st.Active++
	case DecisionCreatePending, DecisionConflictPending:
		var sup *ids.ID
		note := ""
		if existing != nil {
			idv := existing.ID
			sup = &idv
			note = "conflict: 与现值「" + existing.ValueText + "」冲突，待人工确认"
			st.Conflicts++
		}
		row := attrRow(userID, personID, f, d, "pending", sup, memID, prov)
		if err := s.Attributes.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createAttrLog(personID, row, memID, prov, note)); err != nil {
			return err
		}
		st.Pending++
	}
	return nil
}

// ---- 关系平面 ----

// applyRelationshipFact 写一条关系事实。
//
// 已知限制：关系自然键不含 org_name（见 repo.PersonRelationshipRepo.FindByNaturalKeyExt），
// 同 session 内同类型的多个组织关系（对端 person 均为 NULL）会因自然键相同而塌缩去重，
// 只落库首条（P1 边界场景，接受；后续如需区分再把 org_name 纳入自然键）。
func (s *Service) applyRelationshipFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	relatedID, err := s.resolveSubject(ctx, tx, f.Related, prov)
	if err != nil {
		return err
	}
	if relatedID == 0 && f.OrgName == "" && f.Related.Name == "" {
		st.Skipped++ // 对端完全解析不到且无组织名
		return nil
	}

	existing, err := s.Relationships.FindActiveByTypeExt(ctx, tx, personID, f.RelationType, idPtr(relatedID))
	if err != nil {
		return err
	}
	dedup, err := s.Relationships.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID, f.RelationType, idPtr(relatedID))
	if err != nil {
		return err
	}

	dec := DecideRelationship(f, existing, dedup != nil, s.Gate)
	switch dec {
	case DecisionSkip:
		st.Skipped++
	case DecisionReaffirm:
		// reaffirm 的持久化效果=审计记录（change_log）；关系平面不上调置信度、不 touch
		//（gate 注释已声明两平面差异）。实测 MySQL：对已 active 行 UPDATE status='active'
		// 无值变更，不触发 ON UPDATE CURRENT_TIMESTAMP，是纯 no-op SQL，故不写。
		if err := s.ChangeLogs.CreateExt(ctx, tx, reaffirmRelLog(personID, existing, memID, prov)); err != nil {
			return err
		}
		st.Reaffirmed++
	default: // DecisionCreateActive / DecisionCreatePending
		status := "pending"
		if dec == DecisionCreateActive {
			status = "active"
		}
		row := relRow(userID, personID, f, relatedID, status, memID, prov)
		if err := s.Relationships.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createRelLog(personID, row, memID, prov)); err != nil {
			return err
		}
		if status == "active" {
			st.Active++
		} else {
			st.Pending++
		}
	}
	return nil
}

// ---- 事件平面（P2 大事记）----

// applyEventFact 事件落库：闸门（同键佐证/新键按置信度）+ 单事务 + 审计。
// related 为可选增强（解析不到存空 RelatedPersonIDs，不阻断事件创建——见 fact.go 注释）。
func (s *Service) applyEventFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	existing, err := s.Events.FindActiveByKeyExt(ctx, tx, personID, f.EventType, f.EventTitle)
	if err != nil {
		return err
	}
	dedup, err := s.Events.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID, f.EventType, f.EventTitle)
	if err != nil {
		return err
	}

	dec := DecideEvent(f, existing, dedup != nil, s.Gate)
	switch dec {
	case DecisionSkip:
		st.Skipped++
	case DecisionReaffirm:
		// 事件无置信佐证语义（不 bump），持久化效果=审计条目——同 relationship reaffirm 模式
		if err := s.ChangeLogs.CreateExt(ctx, tx, reaffirmEventLog(personID, existing, memID, prov)); err != nil {
			return err
		}
		st.Reaffirmed++
	default:
		status := "pending"
		if dec == DecisionCreateActive {
			status = "active"
		}
		// related 解析（可选）：解析不到存空，不 skip 事件
		var relatedIDs ids.List
		if f.Related.Kind != "" {
			if rid, err := s.resolveSubject(ctx, tx, f.Related, prov); err != nil {
				return err
			} else if rid != 0 {
				relatedIDs = ids.List{rid}
			}
		}
		row := eventRow(userID, personID, f, relatedIDs, status, memID, prov)
		if err := s.Events.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createEventLog(personID, row, memID, prov)); err != nil {
			return err
		}
		if status == "active" {
			st.Active++
		} else {
			st.Pending++
		}
	}
	return nil
}

// ---- 人物归属解析（spec §6.2）----

// resolveSubject 把 LLM 的 subject 指代解析为 person id（事务内执行）：
//
//	self → owner；speaker:名 → 声纹名册按名找 speaker 再找绑定 person，找不到回落按名解析；
//	relation:类型 → owner 的该类型 active 关系对端；查不到且有名字则按名解析；
//	mentioned:名 → 按名找，找不到新建 source=llm status=pending 人物（走确认防噪声）。
//
// 返回 0 表示解析不到（调用方跳过该事实）。
func (s *Service) resolveSubject(ctx context.Context, tx *sqlx.Tx, subj Subject, prov Provenance) (ids.ID, error) {
	switch subj.Kind {
	case "self":
		return s.ownerID(ctx, tx)
	case "speaker":
		if pid, err := s.personBySpeakerName(ctx, tx, subj.Name); err != nil {
			return 0, err
		} else if pid != 0 {
			return pid, nil
		}
		return s.resolveOrCreateByName(ctx, tx, subj.Name, prov)
	case "relation":
		if pid, err := s.personByOwnerRelation(ctx, tx, subj.Relation); err != nil {
			return 0, err
		} else if pid != 0 {
			return pid, nil
		}
		if subj.Name != "" {
			return s.resolveOrCreateByName(ctx, tx, subj.Name, prov)
		}
		return 0, nil
	case "mentioned":
		return s.resolveOrCreateByName(ctx, tx, subj.Name, prov)
	}
	return 0, nil
}

func (s *Service) ownerID(ctx context.Context, tx *sqlx.Tx) (ids.ID, error) {
	owner, err := s.Persons.GetOwnerExt(ctx, tx, 1)
	if err != nil {
		return 0, err
	}
	if owner == nil {
		return 0, fmt.Errorf("owner person 缺失（EnsurePersonBootstrap 未跑）")
	}
	return owner.ID, nil
}

// personBySpeakerName 声纹名册按名找 active speaker → 绑定的 person。
// 名册规模小（MVP），直接全量遍历；speaker 名通常就是 person 名。
func (s *Service) personBySpeakerName(ctx context.Context, tx *sqlx.Tx, name string) (ids.ID, error) {
	if name == "" {
		return 0, nil
	}
	list, err := s.Speakers.List(ctx)
	if err != nil {
		return 0, err
	}
	for _, sp := range list {
		if sp.Status != "active" || sp.Name != name {
			continue
		}
		p, err := s.Persons.GetBySpeakerExt(ctx, tx, sp.ID)
		if err != nil {
			return 0, err
		}
		if p != nil {
			return p.ID, nil
		}
	}
	return 0, nil
}

// personByOwnerRelation owner 的指定类型 active 关系对端（「我老婆」→ 配偶 person）。
//
// 走 tx（而非 r.DB）以看到本事务内未提交的关系：同一批 ApplyFacts 里，「我老婆是医生」
// 这类属性事实的主体（relation:配偶）依赖同批刚新建、尚未提交的配偶关系；非事务读看不到
// 未提交行，会把该事实误判为「主体解析不到」而跳过。查询语义（取该类型、对端为具体 person、
// active 的最老一条）下沉到 repo.FindActiveRelatedPersonIDExt——业务层不写裸 SQL（见 db.go）。
func (s *Service) personByOwnerRelation(ctx context.Context, tx *sqlx.Tx, relationType string) (ids.ID, error) {
	owner, err := s.Persons.GetOwnerExt(ctx, tx, 1)
	if err != nil || owner == nil {
		return 0, err
	}
	rid, err := s.Relationships.FindActiveRelatedPersonIDExt(ctx, tx, owner.ID, relationType)
	if err != nil {
		return 0, err
	}
	if rid == nil {
		return 0, nil // 无对端为具体 person 的该类型 active 关系
	}
	return *rid, nil
}

// resolveOrCreateByName 按显示名找 active/pending 人物；找不到新建
// source=llm status=pending 的人物并记审计（spec §2 决策 2：自动建档走确认）。
func (s *Service) resolveOrCreateByName(ctx context.Context, tx *sqlx.Tx, name string, prov Provenance) (ids.ID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	p, err := s.Persons.FindByNameExt(ctx, tx, 1, name)
	if err != nil {
		return 0, err
	}
	if p != nil {
		return p.ID, nil
	}
	p = &repo.Person{DisplayName: name, Source: "llm", Status: "pending"}
	if err := s.Persons.CreateExt(ctx, tx, p); err != nil {
		return 0, err
	}
	sid := prov.SessionID
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: p.ID, EntityKind: "person", EntityID: &p.ID,
		ChangeType: "create", ChangedBy: "llm", NewValue: snap(p.DisplayName),
		SessionID: &sid, Note: strPtr("LLM 抽取自动新建人物，待确认"),
	}); err != nil {
		return 0, err
	}
	return p.ID, nil
}

// ---- 行构造与审计构造小工具 ----

func attrRow(userID int64, personID ids.ID, f Fact, d AttrDef, status string,
	supersedes, memID *ids.ID, prov Provenance) *repo.PersonAttribute {
	return &repo.PersonAttribute{
		UserID: userID, PersonID: personID, AttrKey: f.AttrKey, ValueText: f.Value,
		ValueType: d.ValueType, Confidence: f.Confidence, EpistemicType: f.EpistemicType,
		Source: "llm", Status: status, SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs), SupersedesID: supersedes,
	}
}

func relRow(userID int64, personID ids.ID, f Fact, relatedID ids.ID, status string,
	memID *ids.ID, prov Provenance) *repo.PersonRelationship {
	row := &repo.PersonRelationship{
		UserID: userID, PersonID: personID, RelationType: f.RelationType,
		Confidence: f.Confidence, EpistemicType: f.EpistemicType,
		Source: "llm", Status: status, SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
	if relatedID != 0 {
		row.RelatedPersonID = &relatedID
	}
	if f.Direction != "" {
		row.Direction = strPtr(f.Direction)
	}
	if f.OrgName != "" {
		row.OrgName = strPtr(f.OrgName)
	}
	if f.Label != "" {
		row.Label = strPtr(f.Label)
	}
	return row
}

func eventRow(userID int64, personID ids.ID, f Fact, relatedIDs ids.List, status string,
	memID *ids.ID, prov Provenance) *repo.PersonEvent {
	row := &repo.PersonEvent{
		UserID: userID, PersonID: personID,
		EventType: f.EventType, Title: f.EventTitle,
		Confidence: f.Confidence, EpistemicType: f.EpistemicType,
		Source: "llm", Status: status, SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
		RelatedPersonIDs:     relatedIDs,
		// MVP：importance 用 confidence 代（手动路径 1.0/LLM 路径闸后值）——
		// 独立重要度建模留后续，spec §13 已记
		Importance: f.Confidence,
	}
	if f.EventDescription != "" {
		row.Description = strPtr(f.EventDescription)
	}
	if t, ok := parseEventAt(f.OccurredAt); ok {
		row.OccurredAt = &t
	}
	if t, ok := parseEventAt(f.EndAt); ok {
		row.EndAt = &t
	}
	if f.EventLocation != "" {
		row.Location = strPtr(f.EventLocation)
	}
	return row
}

func createEventLog(personID ids.ID, row *repo.PersonEvent, memID *ids.ID, prov Provenance) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "event", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "llm", NewValue: snap(row.Title),
		Confidence: fp(row.Confidence), SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
}

func reaffirmEventLog(personID ids.ID, row *repo.PersonEvent, memID *ids.ID, prov Provenance) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "event", EntityID: &row.ID,
		ChangeType: "reaffirm", ChangedBy: "llm", NewValue: snap(row.Title),
		SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
}

func createAttrLog(personID ids.ID, row *repo.PersonAttribute, memID *ids.ID, prov Provenance, note string) *repo.PersonChangeLog {
	l := &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "attribute", EntityID: &row.ID,
		AttrKey: strPtr(row.AttrKey), ChangeType: "create", ChangedBy: "llm",
		NewValue: snap(row.ValueText), Confidence: fp(row.Confidence),
		EpistemicType: strPtr(row.EpistemicType),
		SessionID:     &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
	if note != "" {
		l.Note = strPtr(note)
	}
	return l
}

func reaffirmAttrLog(personID ids.ID, row *repo.PersonAttribute, memID *ids.ID, prov Provenance) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "attribute", EntityID: &row.ID,
		AttrKey: strPtr(row.AttrKey), ChangeType: "reaffirm", ChangedBy: "llm",
		NewValue: snap(row.ValueText), SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
		Note:                 strPtr("同值佐证：置信度 +0.05（封顶 0.99）"),
	}
}

func createRelLog(personID ids.ID, row *repo.PersonRelationship, memID *ids.ID, prov Provenance) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "relationship", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "llm", NewValue: snap(row.RelationType),
		Confidence: fp(row.Confidence), SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
}

// reaffirmRelLog 关系佐证审计。关系平面 reaffirm 既不上调置信度也不 touch 关系行
// （见 applyRelationshipFact 的 DecisionReaffirm 分支），这条 change_log 就是佐证的
// 唯一持久化效果——与属性平面 reaffirmAttrLog（伴随置信度 +0.05）刻意不同。
func reaffirmRelLog(personID ids.ID, row *repo.PersonRelationship, memID *ids.ID, prov Provenance) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "relationship", EntityID: &row.ID,
		ChangeType: "reaffirm", ChangedBy: "llm", NewValue: snap(row.RelationType),
		SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
}

// matchMemory 按 segment 交集给事实找最相关的 memory（溯源 memory_id）；无交集返回 nil。
func matchMemory(rows []repo.MemoryRow, segIDs []ids.ID) *ids.ID {
	var best *ids.ID
	bestN := 0
	for i := range rows {
		n := 0
		for _, sid := range rows[i].TranscriptSegmentIDs {
			for _, f := range segIDs {
				if sid == f {
					n++
				}
			}
		}
		if n > bestN {
			idv := rows[i].ID
			best = &idv
			bestN = n
		}
	}
	return best
}

// ---- 小工具 ----

// snap 把任意值序列化为 JSON 文本快照（change_log old/new_value 用）。
func snap(v any) *string {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func strPtr(s string) *string { return &s }

func fp(f float64) *float64 { return &f }

// idPtr 0 → nil（SQL NULL 安全传参）。
func idPtr(id ids.ID) *ids.ID {
	if id == 0 {
		return nil
	}
	return &id
}

// parseEventAt 尽力解析事件时间：RFC3339 → YYYY-MM-DD → YYYY-MM（月份精度）；
// 全部失败返回 ok=false（调用方存 NULL——事件仍创建，标题里常含时间信息）。
// 解析职责在此而非 ParseFacts：Fact 是传输载体，时间精度策略属落库层。
func parseEventAt(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
