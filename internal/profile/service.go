package profile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
			return st, fmt.Errorf("应用事实(plane=%s key=%s): %w", f.Plane, f.AttrKey, err)
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
		if err := s.Relationships.SetStatusExt(ctx, tx, existing.ID, "active"); err != nil { // no-op touch（updated_at 刷新，确认可见）
			return err
		}
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
// 必须在事务连接 tx 上查：同一批 ApplyFacts 里，「我老婆是医生」这类属性事实的主体
// （relation:配偶）依赖同批刚新建、尚未提交的配偶关系——走 r.DB 的非事务读看不到本事务
// 未提交行，会把该事实误判为「主体解析不到」而跳过。故此处用 tx 直接查询（sqlx Get 走 tx）。
//
// 语义：取该类型、对端为具体 person（related_person_id 非 NULL）、active 的最早一条
// （id 升序稳定选择，如多个子女取最老一条）。repo.FindActiveByTypeExt 传 nil 对端只能匹配
// related_person_id IS NULL 的组织关系，不适合「找关系对端 person」，故这里不复用它。
func (s *Service) personByOwnerRelation(ctx context.Context, tx *sqlx.Tx, relationType string) (ids.ID, error) {
	owner, err := s.Persons.GetOwnerExt(ctx, tx, 1)
	if err != nil || owner == nil {
		return 0, err
	}
	var rid ids.ID
	err = tx.GetContext(ctx, &rid, `
SELECT related_person_id FROM person_relationship
WHERE person_id = ? AND relation_type = ? AND status = 'active' AND related_person_id IS NOT NULL
ORDER BY id LIMIT 1`, owner.ID.Int64(), relationType)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rid, nil
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
