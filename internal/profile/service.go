package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// ErrNotFound 目标行不存在（API 层映射 404）。
var ErrNotFound = errors.New("记录不存在")

// ErrPersonHasSpeaker 目标人物已绑定其他声纹（API 层映射 409；一人至多一声纹）。
var ErrPersonHasSpeaker = errors.New("该人物已绑定其他声纹")

// ErrPetNameExists 手动改名为已有同名宠物（active）时拒绝（API 层映射 409）。
var ErrPetNameExists = errors.New("同名宠物已存在")

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
	Events        *repo.PersonEventRepo    // event 平面（P2 大事记）
	Metrics       *repo.PersonMetricRepo   // metric 平面（P3 时序个人指标）
	Cycles        *repo.PersonCycleRepo    // cycle 平面（P3 周期/日程，敏感）
	Activities    *repo.PersonActivityRepo // activity 平面（P4 生活轨迹，测点流语义）
	Pets          *repo.PersonPetRepo      // pet 平面（宠物）
	ChangeLogs    *repo.PersonChangeLogRepo

	LLM           provider.LLMProvider // ExtractSession 用（Task 13）；手动 CRUD 不需要
	Model         string
	Prompt        string
	PromptVersion string
	Window        int
	Gate          GateConfig

	// Now 注入「当前时间」（测试可固定），供 metric 平面 measured_at 缺省时回退。
	// nil 时用 time.Now（见 now()）——measured_at 列 NOT NULL，回退保证非零。
	Now func() time.Time
}

// now 返回当前时间：优先注入的 s.Now（测试可固定），否则 time.Now。
// 供 applyMetricFact 在 measured_at 缺省/解析失败时提供非零 fallback（硬约束 4：列 NOT NULL）。
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Provenance 一条事实的溯源信息。
type Provenance struct {
	SessionID  ids.ID
	SegmentIDs []ids.ID
	// FallbackAt 是 metric 平面 measured_at 缺省/解析失败时的回退时间。必须「同 session 重跑
	// 稳定」（用本 session 的 created_at），否则 metric 自然键(含 measured_at)每次重跑都变 →
	// FindByPointExt 不命中 → 重复插测点（评审 C1）。session 不存在时才退墙钟 s.now()。
	FallbackAt time.Time
}

// ApplyStats 一次 ApplyFacts 的决策统计（trace 与日志用）。
type ApplyStats struct {
	Total      int
	Active     int // 直接写入 active
	Pending    int // 低置信/冲突待确认
	Reaffirmed int // 同值佐证（置信度上调）
	Conflicts  int // Pending 中的冲突条数
	Skipped    int // 幂等跳过 / 主体解析不到
	// StaleRemoved 残留清理删除的本 session 旧行数（见 ApplyFacts 的残留清理：
	// 用户改 ASR 重新提取后，旧文本独有、未被新事实命中的画像行）。
	StaleRemoved int

	// touched 残留清理的「保留白名单」：本次落库过程中被读到的已有行 id
	//（dedup 命中 / refine 目标 / reaffirm 目标——含跨 session 行，多收只多不少删）。
	// key=表名（person_attribute 等）。unexported：不参与 API 序列化，纯内部机制。
	touched map[string]map[ids.ID]bool
}

// touch 记录一个被本次事实处理过的已有行（残留清理时不删）。惰性初始化。
func (st *ApplyStats) touch(table string, id ids.ID) {
	if st.touched == nil {
		st.touched = map[string]map[ids.ID]bool{}
	}
	if st.touched[table] == nil {
		st.touched[table] = map[ids.ID]bool{}
	}
	st.touched[table][id] = true
}

// ApplyFacts 把一批 LLM 事实应用到库：人物归属解析 → 闸门 → 单事务写入
// （含 change_log）。幂等三层（spec §6.3）：① 自然键去重——同 session 重跑不重复
// 建 pending、不重复 bump；② 同键变化 refine / 敏感平面 pending-supersedes
// （reextract_dedup 契约）；③ 残留清理——同 session 重跑时，旧文本独有、未被本次
// 事实命中的行连同 change_log 删除（改 ASR 重新提取后画像以最新文本为准）。
// 用户此前的 confirm/dismiss 决定只对「仍被新事实命中的行」保留；随旧文本消失的
// 行（含其上的确认状态）一并删除——源头文本已改，确认失去依据。
func (s *Service) ApplyFacts(ctx context.Context, sessionID ids.ID, userID int64, facts []Fact) (ApplyStats, error) {
	var st ApplyStats
	st.Total = len(facts)
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback() }() // Commit 后 Rollback 是 no-op

	// 残留清理（快照步）：先快照本 session 在各画像平面的现有行 id；落库后与
	// 白名单（本次事实触碰过的行，见 ApplyStats.touch）求差，差集 = 旧文本独有、
	// 新文本下已消失的事实 → 删除（连同 change_log）。用户改 ASR 重新提取后，
	// 画像与 memory/todo 一样以最新文本为准（2026-08-31「划船→化妆」实录 bug）。
	snapRows, err := snapshotSessionRows(ctx, tx, sessionID)
	if err != nil {
		return st, fmt.Errorf("快照 session 画像行: %w", err)
	}

	// 本 session 的 memories：供 memory_id 溯源（按 segment 交集最大匹配）。
	// 事务外读即可（只读，不依赖事务内一致性）。
	memRows, err := s.Memories.ListBySession(ctx, sessionID)
	if err != nil {
		return st, fmt.Errorf("读 session memories: %w", err)
	}

	// metric/activity 平面 measured_at/started_at 缺省时的稳定回退：优先本 session 的 created_at
	// （同 session 重跑得同一时间 → 自然键命中、幂等不重复插；评审 C1）。session 读不到才退
	// s.now()（墙钟，仅测试/无 session 场景；生产 extract 恒有真 session）。
	fallbackAt := s.now()
	if s.Sessions != nil {
		// 用 threaded userID（本次 ApplyFacts 的登录用户）读 session，与写归属一致：
		// 越权读他人 session 命中 0 行 → 回退墙钟，不泄漏他人 created_at。
		if sess, err := s.Sessions.Get(ctx, userID, sessionID); err == nil && sess != nil && !sess.CreatedAt.IsZero() {
			fallbackAt = sess.CreatedAt
		}
	}

	// F2（spec §13）：两趟落库——relationship 平面先行。
	//
	// 背景：subject=relation:TYPE 的事实（如「我老婆是儿科医生」的属性、「陪我老婆去产检」的
	// 活动）靠 resolveSubject→personByOwnerRelation 查 owner 的该类型 active 关系对端来解析归属，
	// 依赖同批里先落的关系行（personByOwnerRelation 走 tx、能读到本事务内未提交的关系）。但 LLM
	// 输出顺序不保证关系事实排在依赖它的事实之前；单趟循环遇乱序（属性在前、关系在后）时，属性会
	// 因当时查不到关系对端被判「主体解析不到」→ st.Skipped（非破坏，但白丢一次抽取机会）。
	//
	// 故拆两趟：第一趟只落 relationship 平面（建立 owner↔对端 关系行），第二趟落其余全部平面
	// （此时同批关系行已在 tx 内可见，relation:TYPE 归属可解析）。两趟共用同一 tx、不改事务边界，
	// 任一趟任一条失败仍整体回滚，失败语义不变。
	applyOne := func(f Fact) error {
		prov := Provenance{SessionID: sessionID, SegmentIDs: f.SegmentIDs, FallbackAt: fallbackAt}
		if err := s.applyFact(ctx, tx, userID, f, prov, memRows, &st); err != nil {
			return fmt.Errorf("应用事实(plane=%s key=%s relation=%s subject=%s): %w",
				f.Plane, f.AttrKey, f.RelationType, f.Subject.Kind, err)
		}
		return nil
	}
	// 第一趟：只落 relationship 平面（供第二趟解析 relation:TYPE 归属）。
	for _, f := range facts {
		if f.Plane != "relationship" {
			continue
		}
		if err := applyOne(f); err != nil {
			return st, err
		}
	}
	// 第二趟：落其余所有平面（attribute/event/metric/cycle/activity）。
	for _, f := range facts {
		if f.Plane == "relationship" {
			continue
		}
		if err := applyOne(f); err != nil {
			return st, err
		}
	}

	// 残留清理（删除步）：快照 - 白名单 = 旧文本独有行 → 删除（含 change_log 级联）。
	// 放两趟落库之后：refine/supersedes 路径可能新建行或更新旧行，先落完再算差集，
	// 白名单在 applyXXX 内随 dedup/existing 命中实时收集。
	if n, err := deleteStaleRows(ctx, tx, sessionID, snapRows, st.touched); err != nil {
		return st, err
	} else if n > 0 {
		st.StaleRemoved = n
	}

	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

func (s *Service) applyFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	prov Provenance, memRows []repo.MemoryRow, st *ApplyStats) error {

	personID, err := s.resolveSubject(ctx, tx, userID, f.Subject, prov)
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
	if f.Plane == "metric" {
		return s.applyMetricFact(ctx, tx, userID, f, personID, memID, prov, st)
	}
	if f.Plane == "activity" {
		return s.applyActivityFact(ctx, tx, userID, f, personID, memID, prov, st)
	}
	if f.Plane == "cycle" {
		return s.applyCycleFact(ctx, tx, userID, f, personID, memID, prov, st)
	}
	if f.Plane == "pet" {
		return s.applyPetFact(ctx, tx, userID, f, personID, memID, prov, st)
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

	// F4 写入端校验/规范化（单点闸，见 validate.go）：gender=「男性」、smokes=「是」、
	// birthday=「八月三号」这类脏值在此拦下。规范化后的值贯穿后续 existing/dedup 查询、
	// DecideAttribute 闸门比较与 attrRow 落库——闸门比较链全程用同一规范值，避免「按原值比较、
	// 按规范值落库」的口径漂移（f 是值传递的本地副本，改 f.Value 只影响本次调用链）。
	// 校验失败 → Skipped 且不落库不进队列（宁少勿错：脏值不入库、不制造确认噪声；后续会话
	// 若抽到规范值仍可正常落）。
	norm, err := NormalizeAttrValue(d, f.Value)
	if err != nil {
		st.Skipped++
		return nil
	}
	f.Value = norm

	isList := d.Cardinality == CardinalityList

	var existing *repo.PersonAttribute
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
	// 残留清理白名单：读到的已有行（existing 可能是本 session 上轮所建）不参与残留删除。
	if existing != nil {
		st.touch("person_attribute", existing.ID)
	}
	if dedup != nil {
		st.touch("person_attribute", dedup.ID)
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

	relatedID, err := s.resolveSubject(ctx, tx, userID, f.Related, prov)
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
	if existing != nil {
		st.touch("person_relationship", existing.ID) // 残留清理白名单
	}
	dedup, err := s.Relationships.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID, f.RelationType, idPtr(relatedID))
	if err != nil {
		return err
	}
	if dedup != nil {
		st.touch("person_relationship", dedup.ID) // 残留清理白名单
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
// related 为可选增强（P2a② 多人）：related_people 数组逐人解析、解析不到存空 RelatedPersonIDs，
// 均不阻断事件创建；数组空时回退旧 Related 单人字段（见下方解析段与 fact.go Fact.EventRelated 注释）。
//
// P2a③ 标题归一化去重：两个查询职责不同、刻意用不同的匹配口径——
//   - dedup（FindByNaturalKeyExt）：**精确**标题的同 session 自然键，防同一 session 重跑重复建行。
//     刻意不归一化——同 session 严格幂等只认精确重复；归一化留给跨 session 的字面近重复。
//   - existing（FindActiveByNormalizedTitleExt）：**归一化**标题的当前 active 行匹配。
//     使 LLM 跨 session 出的字面近重复标题（「去云南旅游」/「去云南旅游！」/「去 云南 旅游」）
//     命中同一 active 事件走佐证（reaffirm），而非各建一条 active。镜像 attribute 平面
//     「Go 侧 NormalizeTitle 比较 reaffirm」（gate.go DecideAttribute）的既有模式。
//
// 决策顺序由 DecideEvent 保证：dedupHit 优先 Skip（精确幂等）→ existing 非空 Reaffirm（归一化佐证）
// → 否则按置信度新建。归一化匹配是精确匹配的超集，故只需一次归一化查询即两用（既判去重又判佐证）。
func (s *Service) applyEventFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	existing, err := s.Events.FindActiveByNormalizedTitleExt(ctx, tx, personID, f.EventType, f.EventTitle)
	if err != nil {
		return err
	}
	if existing != nil {
		st.touch("person_event", existing.ID) // 残留清理白名单
	}
	dedup, err := s.Events.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID, f.EventType, f.EventTitle)
	if err != nil {
		return err
	}
	if dedup != nil {
		st.touch("person_event", dedup.ID) // 残留清理白名单
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
		// related 解析（可选，P2a② 多人）：related_people 数组逐个 resolveSubject（单人解析不到
		// 跳过该人、不阻断事件——多人场景「宁少记一人不丢事件」）；数组为空时回退旧 Related 单人字段
		// （prompt few-shot 与历史输出向后兼容）。整体解析不到就存空 RelatedPersonIDs，事件照常创建
		//（见 applyEventFact 顶注释与 fact.go Fact.EventRelated）。
		var relatedIDs ids.List
		if len(f.EventRelated) > 0 {
			for _, sub := range f.EventRelated {
				// 逐人容错：resolveSubject 出错或解析到 0（无法归属）都跳过该人，继续下一个——
				// 不 return err 中断整批（真正的 DB 故障会在后续 Events.CreateExt 处再次暴露并回滚，
				// 不会因这里吞掉个别人的错误而落下半条脏数据）。
				if rid, err := s.resolveSubject(ctx, tx, userID, sub, prov); err == nil && rid != 0 {
					relatedIDs = append(relatedIDs, rid)
				}
			}
		} else if f.Related.Kind != "" {
			// 旧单人路径原样保留：单人解析的 DB 错误照旧上抛（这条不是多人容错语义）。
			if rid, err := s.resolveSubject(ctx, tx, userID, f.Related, prov); err != nil {
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

// ---- 指标平面（P3 时序个人指标）----

// applyMetricFact 时序指标落库（硬约束 1-6）：与 applyEventFact 同签名（带 provenance），
// 但语义为「连续测点」，与单值属性/事件都不同：
//   - append-only：绝不 supersede 旧行，每个测点一行（硬约束 1）；
//   - 恒 create、无 reaffirm/conflict：命中完全同点（自然键含 measured_at + 值）则幂等跳过，
//     否则按闸门建行（硬约束 3）；
//   - measured_at 保留时刻精度（parseMetricAt，不复用抹平到当天零点的 parseEventAt），
//     缺省时回退 prov.FallbackAt（本 session created_at），保证列 NOT NULL 非零（硬约束 4）；
//   - confidence 存抽取确定性，value_num/value_text 才是主载荷；repo 不给 confidence 兜底，
//     故建行必显式设 confidence（硬约束 5）。
func (s *Service) applyMetricFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	// measured_at：保留时刻精度；解析失败/缺省回退 prov.FallbackAt（本 session created_at，
	// 同 session 重跑稳定 → 幂等命中，评审 C1）。
	measuredAt := parseMetricAt(f.MeasuredAt, prov.FallbackAt)

	// 幂等（硬约束 2/3）：自然键含 measured_at + 值，命中完全同点则直接跳过（no-op），
	// 绝不像单值属性那样 supersede 旧行。value_text 空存 nil，交给 <=> NULL 安全比较。
	ex, err := s.Metrics.FindByPointExt(ctx, tx, personID, f.MetricKey, measuredAt, f.ValueNum, textPtr(f.MetricValueText))
	if err != nil {
		return err
	}
	if ex != nil {
		st.touch("person_metric", ex.ID) // 残留清理白名单（幂等同点命中=本 session 上轮所建）
	}
	if ex != nil {
		st.Skipped++
		return nil
	}

	// 闸门：无冲突/现值分支，只 active/pending（硬约束 3）。
	status := DecideMetric(f.Confidence, f.EpistemicType, s.Gate)

	// 单位：事实未给则回退目录单位（体重→kg、睡眠→h；情绪/精力/饮食/健康无量纲→nil）。
	unit := f.Unit
	if unit == "" {
		unit = MetricDefOf(f.MetricKey).Unit
	}

	row := &repo.PersonMetric{
		UserID: userID, PersonID: personID, MetricKey: f.MetricKey,
		ValueNum: f.ValueNum, ValueText: textPtr(f.MetricValueText), Unit: textPtr(unit),
		MeasuredAt:    measuredAt,
		Confidence:    f.Confidence, // 硬约束 5：显式设 confidence（repo 不兜底）
		EpistemicType: f.EpistemicType, Source: "extract", Status: status,
		SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
	if err := s.Metrics.CreateExt(ctx, tx, row); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, createMetricLog(personID, row, memID, prov, "llm")); err != nil {
		return err
	}
	if status == "active" {
		st.Active++
	} else {
		st.Pending++
	}
	return nil
}

// ---- activity 平面（P4 生活轨迹）----

// applyActivityFact 测点流语义（完全对齐 applyMetricFact）：每条活动独立一行，无当前值/无冲突/
// 无佐证——纯置信闸门 + 自然键防重跑。started_at 解析链：parseEventAt(f.StartedAt) 成功用之，
// 失败 → prov.FallbackAt（本 session created_at，对话发生时即活动时刻，比留 NULL 更符合时间线
// 语义——时间线按时间排布不能没有锚点，同 metric 的 measured_at）。三个可空串（tool/location/
// commute_mode）trim 后空串→nil（对齐 cycle label 的 <=> NULL 约定）；duration>0 才落，≤0 视为
// LLM 未给→nil（同 cycle period/duration「未给不设」）。
func (s *Service) applyActivityFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	// started_at 先定（自然键成分）：解析失败落 prov.FallbackAt。
	startedAt := prov.FallbackAt
	if t, ok := parseEventAt(f.StartedAt); ok {
		startedAt = t
	}
	// activity 恒非空（ParseFacts 已强制），仍取指针——与三个可空串统一走 repo 的 <=> 匹配。
	activity := strings.TrimSpace(f.ActivityText)
	tool := trimToPtr(f.Tool)
	location := trimToPtr(f.Location)
	commuteMode := trimToPtr(f.CommuteMode)
	// duration>0 才落，≤0 视为未给（不臆造 0 分钟）。同为自然键成分，nil 走 <=> 命中 IS NULL。
	var durationMin *int
	if f.DurationMin > 0 {
		dm := f.DurationMin
		durationMin = &dm
	}
	dedup, err := s.Activities.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID,
		&activity, tool, location, commuteMode, startedAt, durationMin)
	if err != nil {
		return err
	}
	if dedup != nil {
		st.touch("person_activity", dedup.ID) // 残留清理白名单
	}

	dec := DecideActivity(f, dedup != nil, s.Gate)
	if dec == DecisionSkip {
		st.Skipped++
		return nil
	}
	// 测点无冲突无佐证，Active/Pending 两路径只差 status（对齐 applyMetricFact 模式）。
	status := "pending"
	if dec == DecisionCreateActive {
		status = "active"
	}
	row := activityRow(userID, personID, f, tool, location, commuteMode, durationMin, startedAt, status, memID, prov)
	if err := s.Activities.CreateExt(ctx, tx, row); err != nil {
		return err
	}
	if err := s.ChangeLogs.CreateExt(ctx, tx, createActivityLog(personID, row, memID, prov)); err != nil {
		return err
	}
	if status == "active" {
		st.Active++
	} else {
		st.Pending++
	}
	return nil
}

// ---- cycle 平面（P3 周期/日程，敏感）----

// applyCycleFact 单值语义（同 person+type+label 至多一条 active）：冲突 pending+supersedes
// 绝不静默覆盖（对齐 attribute 单值模式）。next_predicted_at = anchor_date + period_days
// （两者齐才算；纯估算非医疗建议，spec §9，算在 cycleRow→applyCycleParams 内）。
// label 统一 trim 后空串→nil（repo <=> NULL 匹配；混用空串与 NULL 会产生重复 active）。
func (s *Service) applyCycleFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	var label *string
	if l := strings.TrimSpace(f.CycleLabel); l != "" {
		label = &l
	}
	existing, err := s.Cycles.FindActiveByKeyExt(ctx, tx, personID, f.CycleType, label)
	if err != nil {
		return err
	}
	if existing != nil {
		st.touch("person_cycle", existing.ID) // 残留清理白名单
	}
	dedup, err := s.Cycles.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID, f.CycleType, label)
	if err != nil {
		return err
	}
	if dedup != nil {
		st.touch("person_cycle", dedup.ID) // 残留清理白名单
	}

	// 同参短路（对齐 attribute 的「同值→佐证」语义）：existing 的关键参数与新事实完全一致时
	// 视为佐证（仅审计，不加行不进队列）——否则后续 session 每次重提同一周期（「还在吃降压药」，
	// 参数没变）都会造一条冲突 pending，造成确认疲劳。放在 dedup 判断之后：同 session 重跑由
	// 自然键 Skip 优先（纯幂等），仅跨 session 未命中自然键但同参时才走佐证（统计上归 Reaffirmed）。
	if dedup == nil && existing != nil && cycleParamsEqual(existing, f) {
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: personID, EntityKind: "cycle", EntityID: &existing.ID,
			ChangeType: "reaffirm", ChangedBy: "llm", NewValue: snap(existing.CycleType),
			SessionID: &prov.SessionID, MemoryID: memID,
			TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
			Note:                 strPtr("同参佐证：周期未变化"),
		}); err != nil {
			return err
		}
		st.Reaffirmed++
		return nil
	}

	// dedupEffective 语义同 pet（见 applyPetFact 注释）：本 session 已处理过同 key **且**
	// 与现值参数一致 → 真幂等 skip；有变化（改 ASR 重新提取，如剂量/频次调整）则放行到
	// DecisionConflictPending，走「pending 行 supersedes 现值」待人工确认（周期敏感，保守不静默覆盖）。
	dedupEffective := dedup != nil
	if existing != nil {
		dedupEffective = dedup != nil && cycleParamsEqual(existing, f)
	}

	dec := DecideCycle(f, existing, dedupEffective, s.Gate)
	switch dec {
	case DecisionSkip:
		st.Skipped++
	case DecisionConflictPending:
		// DecideCycle 仅在 existing != nil 时返回 ConflictPending，故此处 existing 必非空；
		// 仍防御性判空（与 applyAttributeFact 冲突分支同构）。
		var sup *ids.ID
		note := ""
		if existing != nil {
			idv := existing.ID
			sup = &idv
			note = "conflict: 与现有周期记录冲突，待人工确认"
			st.Conflicts++
		}
		row := cycleRow(userID, personID, f, label, "pending", sup, memID, prov)
		if err := s.Cycles.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createCycleLog(personID, row, memID, prov, note)); err != nil {
			return err
		}
		st.Pending++
	default: // DecisionCreateActive / DecisionCreatePending
		status := "pending"
		if dec == DecisionCreateActive {
			status = "active"
		}
		row := cycleRow(userID, personID, f, label, status, nil, memID, prov)
		if err := s.Cycles.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createCycleLog(personID, row, memID, prov, "")); err != nil {
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

// ---- pet 平面（宠物）----

// applyPetFact 宠物落库：同名单值语义（同 person+name 至多一条 active）+ 字段级合并整行重写。
//   - 新宠物（无同名 active）→ 按置信度 active/pending（对齐 cycle）；
//   - 同名现值存在：先同值短路（petFieldsEqual：fact 提到的字段与现值全一致 → 仅 reaffirm 审计，
//     防确认疲劳，对齐 cycle 同参短路）；有变化 → mergePetRow 字段合并（提到的覆盖、未提到沿用）——
//     高置信：合并行直接 active + 旧行 superseded（整只替换，对齐手动改值路径 ManualUpdatePet）；
//     低置信：合并行 pending + supersedes 指向旧行（确认后替换，绝不静默覆盖）。
func (s *Service) applyPetFact(ctx context.Context, tx *sqlx.Tx, userID int64, f Fact,
	personID ids.ID, memID *ids.ID, prov Provenance, st *ApplyStats) error {

	name := strings.TrimSpace(f.PetName)
	existing, err := s.Pets.FindActiveByNameExt(ctx, tx, personID, name)
	if err != nil {
		return err
	}
	if existing != nil {
		st.touch("person_pet", existing.ID) // 残留清理白名单
	}
	dedup, err := s.Pets.FindByNaturalKeyExt(ctx, tx, prov.SessionID, personID, name)
	if err != nil {
		return err
	}
	if dedup != nil {
		st.touch("person_pet", dedup.ID) // 残留清理白名单
	}

	// 同值佐证短路：跨 session 未命中自然键、但 fact 提到的字段与现值全一致（「我家猫小花」
	// 裸重提）→ 仅审计，不加行不进队列。
	if dedup == nil && existing != nil && petFieldsEqual(existing, f) {
		if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
			PersonID: personID, EntityKind: "pet", EntityID: &existing.ID,
			ChangeType: "reaffirm", ChangedBy: "llm", NewValue: snap(petSummary(existing)),
			SessionID: &prov.SessionID, MemoryID: memID,
			TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
			Note:                 strPtr("同值佐证：宠物信息未变化"),
		}); err != nil {
			return err
		}
		st.Reaffirmed++
		return nil
	}

	// dedupEffective = 「本 session 已处理过同 key」**且** 与现值一致 → 真幂等，可 skip。
	// 关键：dedup 仅命中自然键（同 session 同 name）不代表内容无变化——用户改 ASR 后点
	// 「重新提取」（同一 session 重跑）会抽出**payload 有变化**的 fact（如性别 公→母）。
	// 若此处沿用 dedup!=nil 直接 skip，memory 走「删旧重插」能更新、pet 走 skip 不能，
	// 就出现「记忆改成母猫、宠物还是公猫」的不一致。故有变化时 dedupEffective=false，
	// 放行到下面的 DecisionConflictPending + autoWritable 分支走 mergePetRow 整只替换。
	// existing==nil（无 active 现值，仅 superseded/pending 历史行）保持原 skip 语义。
	dedupEffective := dedup != nil
	if existing != nil {
		dedupEffective = dedup != nil && petFieldsEqual(existing, f)
	}

	dec := DecidePet(f, existing, dedupEffective, s.Gate)
	switch dec {
	case DecisionSkip:
		st.Skipped++
	case DecisionCreateActive, DecisionCreatePending:
		status := "pending"
		if dec == DecisionCreateActive {
			status = "active"
		}
		row := petRow(userID, personID, f, status, nil, memID, prov)
		if err := s.Pets.CreateExt(ctx, tx, row); err != nil {
			return err
		}
		if err := s.ChangeLogs.CreateExt(ctx, tx, createPetLog(personID, row, memID, prov, "")); err != nil {
			return err
		}
		if status == "active" {
			st.Active++
		} else {
			st.Pending++
		}
	case DecisionConflictPending:
		// DecidePet 仅在 existing != nil 时返回 ConflictPending，此处必非空；仍防御性判空
		//（与 applyAttributeFact 冲突分支同构）。
		if existing == nil {
			st.Skipped++
			return nil
		}
		idv := existing.ID
		if autoWritable(f, s.Gate) {
			// 高置信：合并行直接 active，旧行 superseded（整只替换——LLM 高置信新信息
			// 无须人工确认即可合并，对齐手动改值路径的落库形态）。
			if err := s.Pets.SetStatusExt(ctx, tx, existing.ID, "superseded"); err != nil {
				return err
			}
			row := mergePetRow(userID, existing, f, "active", &idv, memID, prov)
			if err := s.Pets.CreateExt(ctx, tx, row); err != nil {
				return err
			}
			if err := s.ChangeLogs.CreateExt(ctx, tx, createPetLog(personID, row, memID, prov, "合并更新：新信息覆盖，未提到字段沿用")); err != nil {
				return err
			}
			st.Active++
		} else {
			// 低置信：pending 指向现值，确认后替换（绝不静默覆盖）。
			row := mergePetRow(userID, existing, f, "pending", &idv, memID, prov)
			if err := s.Pets.CreateExt(ctx, tx, row); err != nil {
				return err
			}
			if err := s.ChangeLogs.CreateExt(ctx, tx, createPetLog(personID, row, memID, prov, "conflict: 与现有宠物记录待合并，待人工确认")); err != nil {
				return err
			}
			st.Pending++
			st.Conflicts++
		}
	}
	return nil
}

// petRow 构造一条新宠物行（新宠物路径）：可空串 trim 空→nil（<=> NULL 约定，对齐 activity）；
// species 归一（缺省/非法→其他）；birthday 走 parseEventAt（UTC 零点归一，失败存 NULL——
// 生日是估算增强信息，解析不到不阻断宠物创建，age_text 已保留原始表述）。
func petRow(userID int64, personID ids.ID, f Fact, status string,
	sup, memID *ids.ID, prov Provenance) *repo.PersonPet {
	row := &repo.PersonPet{
		UserID: userID, PersonID: personID,
		Name:       strings.TrimSpace(f.PetName),
		Nickname:   trimToPtr(f.PetNickname),
		Species:    NormalizeSpecies(f.Species),
		Breed:      trimToPtr(f.Breed),
		Gender:     trimToPtr(f.Gender),
		AgeText:    trimToPtr(f.AgeText),
		Likes:      trimToPtr(f.Likes),
		Confidence: f.Confidence, EpistemicType: f.EpistemicType,
		Source: "llm", Status: status, SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs), SupersedesID: sup,
	}
	if t, ok := parseEventAt(f.Birthday); ok {
		row.Birthday = &t
	}
	return row
}

// mergePetRow 字段级合并构造（同名现值 + 新事实 → 整只替换的新版本行）：
// f 提到的字段（trim 非空）覆盖，未提到的字段从 existing 沿用。name 恒用 existing
// （同名才走合并；改名属手动路径 ManualUpdatePet）。
func mergePetRow(userID int64, existing *repo.PersonPet, f Fact, status string,
	sup, memID *ids.ID, prov Provenance) *repo.PersonPet {
	row := &repo.PersonPet{
		UserID: userID, PersonID: existing.PersonID,
		Name:     existing.Name,
		Nickname: existing.Nickname, Species: existing.Species,
		Breed: existing.Breed, Gender: existing.Gender,
		AgeText: existing.AgeText, Birthday: existing.Birthday, Likes: existing.Likes,
		Confidence: f.Confidence, EpistemicType: f.EpistemicType,
		Source: "llm", Status: status, SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs), SupersedesID: sup,
	}
	if v := trimToPtr(f.PetNickname); v != nil {
		row.Nickname = v
	}
	if v := strings.TrimSpace(f.Species); v != "" {
		row.Species = NormalizeSpecies(v)
	}
	if v := trimToPtr(f.Breed); v != nil {
		row.Breed = v
	}
	if v := trimToPtr(f.Gender); v != nil {
		row.Gender = v
	}
	if v := trimToPtr(f.AgeText); v != nil {
		row.AgeText = v
	}
	if v := trimToPtr(f.Likes); v != nil {
		row.Likes = v
	}
	if t, ok := parseEventAt(f.Birthday); ok {
		row.Birthday = &t
	}
	return row
}

// petFieldsEqual 判断 fact 提到的字段与现值是否全一致（同值佐证短路的判据）。
// 缺省兼容（对齐 cycleParamsEqual）：f 未给的字段（trim 空）不主张变化、与现值任意值兼容。
func petFieldsEqual(e *repo.PersonPet, f Fact) bool {
	if v := strings.TrimSpace(f.PetNickname); v != "" && derefStr(e.Nickname) != v {
		return false
	}
	if v := strings.TrimSpace(f.Species); v != "" && e.Species != NormalizeSpecies(v) {
		return false
	}
	if v := strings.TrimSpace(f.Breed); v != "" && derefStr(e.Breed) != v {
		return false
	}
	if v := strings.TrimSpace(f.Gender); v != "" && derefStr(e.Gender) != v {
		return false
	}
	if v := strings.TrimSpace(f.AgeText); v != "" && derefStr(e.AgeText) != v {
		return false
	}
	if v := strings.TrimSpace(f.Birthday); v != "" {
		if e.Birthday == nil {
			return false
		}
		// 依赖 DSN 未配 loc（DATE 按 UTC 读回），parseEventAt 也归一到 UTC 零点，两侧同 loc
		//可直接 Equal——与既有平面（event/cycle 的日期比较）一致。
		if t, ok := parseEventAt(v); !ok || !t.Equal(*e.Birthday) {
			return false
		}
	}
	if v := strings.TrimSpace(f.Likes); v != "" && derefStr(e.Likes) != v {
		return false
	}
	return true
}

// petSummary 宠物行摘要（change_log new_value / 确认队列展示用）：名（类别·品种）。
func petSummary(p *repo.PersonPet) string {
	b := p.Name + "（" + p.Species
	if p.Breed != nil {
		b += "·" + *p.Breed
	}
	b += "）"
	return b
}

// createPetLog 宠物创建审计（LLM 路径）。
func createPetLog(personID ids.ID, row *repo.PersonPet, memID *ids.ID, prov Provenance, note string) *repo.PersonChangeLog {
	l := &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "pet", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "llm", NewValue: snap(petSummary(row)),
		Confidence: fp(row.Confidence), SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
	if note != "" {
		l.Note = strPtr(note)
	}
	return l
}

// ---- 人物归属解析（spec §6.2）----

// resolveSubject 把 LLM 的 subject 指代解析为 person id（事务内执行）：
//
//	self → owner；speaker:名 → 声纹名册按名找 speaker 再找绑定 person，找不到回落按名解析；
//	relation:类型 → owner 的该类型 active 关系对端；查不到且有名字则按名解析；
//	mentioned:名 → 按名找，找不到新建 source=llm status=pending 人物（走确认防噪声）。
//
// 返回 0 表示解析不到（调用方跳过该事实）。
func (s *Service) resolveSubject(ctx context.Context, tx *sqlx.Tx, userID int64, subj Subject, prov Provenance) (ids.ID, error) {
	switch subj.Kind {
	case "self":
		return s.ownerID(ctx, tx, userID)
	case "speaker":
		if pid, err := s.personBySpeakerName(ctx, tx, subj.Name); err != nil {
			return 0, err
		} else if pid != 0 {
			return pid, nil
		}
		pid, _, err := s.resolveOrCreateByName(ctx, tx, userID, subj.Name, prov)
		return pid, err
	case "relation":
		if pid, err := s.personByOwnerRelation(ctx, tx, userID, subj.Relation); err != nil {
			return 0, err
		} else if pid != 0 {
			return pid, nil
		}
		if subj.Name != "" {
			pid, _, err := s.resolveOrCreateByName(ctx, tx, userID, subj.Name, prov)
			return pid, err
		}
		return 0, nil
	case "mentioned":
		pid, _, err := s.resolveOrCreateByName(ctx, tx, userID, subj.Name, prov)
		return pid, err
	}
	return 0, nil
}

func (s *Service) ownerID(ctx context.Context, tx *sqlx.Tx, userID int64) (ids.ID, error) {
	owner, err := s.Persons.GetOwnerExt(ctx, tx, userID)
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
func (s *Service) personByOwnerRelation(ctx context.Context, tx *sqlx.Tx, userID int64, relationType string) (ids.ID, error) {
	owner, err := s.Persons.GetOwnerExt(ctx, tx, userID)
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
// 名字经 NormalizePersonName 硬校验兜底（prompt「人物名字规则」已要求 LLM 只给单人名，
// 这里是防「老保一家」类口语粘连的第二道防线）：空名/代词/纯集合名词/超长（>8 rune）
// → 拒绝新建（返回 0，调用方跳过该事实）。
// 人物解析含**别名兜底**（FindByNameOrAliasExt，2026-08-31）：提到人物的已确认别名
// （如「老保」之于解保功）直接归到该人物，不再重复建 pending 新人物。
// resolveOrCreateByName 按名（或别名）找已有人物，找不到新建 source=llm status=pending
// 人物（走确认防噪声）。第二个返回值表示是否**新建**（false=命中既有）。
func (s *Service) resolveOrCreateByName(ctx context.Context, tx *sqlx.Tx, userID int64, name string, prov Provenance) (ids.ID, bool, error) {
	name = NormalizePersonName(name)
	if name == "" {
		return 0, false, nil
	}
	p, err := s.Persons.FindByNameOrAliasExt(ctx, tx, userID, name)
	if err != nil {
		return 0, false, err
	}
	if p != nil {
		return p.ID, false, nil
	}
	p = &repo.Person{UserID: userID, DisplayName: name, Source: "llm", Status: "pending"}
	if err := s.Persons.CreateExt(ctx, tx, p); err != nil {
		return 0, false, err
	}
	sid := prov.SessionID
	if err := s.ChangeLogs.CreateExt(ctx, tx, &repo.PersonChangeLog{
		PersonID: p.ID, EntityKind: "person", EntityID: &p.ID,
		ChangeType: "create", ChangedBy: "llm", NewValue: snap(p.DisplayName),
		SessionID: &sid, Note: strPtr("LLM 抽取自动新建人物，待确认"),
	}); err != nil {
		return 0, false, err
	}
	return p.ID, true, nil
}

// mentionedNameMaxRunes 提及人名的长度上限（与 prompt 人物名字规则一致：超 8 字一律
// 不建人物）——Go 侧兜底，防模型偶尔违规输出长串。
const mentionedNameMaxRunes = 8

// ApplyMentionedNames 收录「本场提及但无画像事实」的人名：按名/别名命中已有人物
// 则 no-op，否则新建 source=llm status=pending 人物（进待确认队列，用户确认才 active）。
// 幂等：同 session 重跑时 FindByNameOrAliasExt 命中既有 pending，不重复建。
// 返回（收录名数, 新建数）。
func (s *Service) ApplyMentionedNames(ctx context.Context, sessionID ids.ID, userID int64, names []string) (int, int, error) {
	kept := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || utf8.RuneCountInString(n) > mentionedNameMaxRunes || seen[n] {
			continue
		}
		seen[n] = true
		kept = append(kept, n)
	}
	if len(kept) == 0 {
		return 0, 0, nil
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	prov := Provenance{SessionID: sessionID}
	created := 0
	for _, n := range kept {
		_, isNew, err := s.resolveOrCreateByName(ctx, tx, userID, n, prov)
		if err != nil {
			return 0, 0, fmt.Errorf("收录提及人名 %q: %w", n, err)
		}
		if isNew {
			created++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return len(kept), created, nil
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

// defaultImportance 事件类型的默认重要度（P2a①：importance 不再用 confidence 代偿）。
//
// 重要度衡量「这件事在人生里的分量」，与 confidence（抽取把握）**正交**：一句「今天中午随便吃了个
// 火锅」可能被高置信抽出（confidence 高），但它的人生分量很低（importance 低）——旧代偿把二者混为
// 一谈是错的（spec §13 P2a①）。类型分级依据 spec §4.4 事件语义：
//   - 里程碑/成就（女儿出生/考上研究生/晋升）——人生大事 → 0.9
//   - 挫折/负面/健康（被骗/离职/确诊/生病）——影响深远的负向事件 → 0.8
//   - 旅行/聚会/会议——值得记的经历，分量中等 → 0.5
//   - 其他（及未知类型）——日常琐事，给略低地板 → 0.4
//
// 兜底 0.4（而非 repo CreateExt 的 0.5）：未命中枚举的多是琐碎「其他」，比中性再低半档更贴语义；
// 且本函数恒返回 >0，eventRow/ManualAddEvent 落库的 importance 必非零，repo 的 0→0.5 兜底不会触发。
func defaultImportance(eventType string) float64 {
	switch eventType {
	case "里程碑", "成就":
		return 0.9
	case "挫折", "负面", "健康":
		return 0.8
	case "旅行", "聚会", "会议":
		return 0.5
	default: // 其他 / 未知类型
		return 0.4
	}
}

// eventImportanceOrDefault 事件重要度取值链（P2a①）：LLM/手动显式给值（>0）优先并 clamp 到 (0,1]，
// 未给（<=0）走事件类型默认。LLM 路径（eventRow）与手动路径（ManualAddEvent）共用，保证取值口径单点。
func eventImportanceOrDefault(explicit float64, eventType string) float64 {
	if explicit > 0 {
		return clamp01(explicit)
	}
	return defaultImportance(eventType)
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
		// P2a①：重要度独立建模，取值链 = LLM 显式值(>0) > 事件类型默认；不再用 confidence 代偿
		//（重要度=人生分量，置信度=抽取把握，两者正交——见 defaultImportance）。
		Importance: eventImportanceOrDefault(f.EventImportance, f.EventType),
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

// createMetricLog 指标测点创建审计。changedBy 区分抽取（"llm"）与手动（"user"）路径。
// new_value 为「metric_key + 值摘要」（如 "weight=70kg"、"emotion=-0.6/焦虑"，见 metricSummary），
// 便于确认队列/变更历史直读。metric 平面 append-only，无 reaffirm/supersede，故仅此一个 create 构造。
func createMetricLog(personID ids.ID, row *repo.PersonMetric, memID *ids.ID, prov Provenance, changedBy string) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "metric", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: changedBy, NewValue: snap(metricSummary(row)),
		Confidence: fp(row.Confidence), EpistemicType: strPtr(row.EpistemicType),
		SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
}

// ---- cycle 平面行与审计构造 ----

func cycleRow(userID int64, personID ids.ID, f Fact, label *string, status string,
	sup *ids.ID, memID *ids.ID, prov Provenance) *repo.PersonCycle {
	row := &repo.PersonCycle{
		UserID: userID, PersonID: personID, CycleType: f.CycleType, Label: label,
		Confidence: f.Confidence, EpistemicType: f.EpistemicType,
		Source: "llm", Status: status, SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs), SupersedesID: sup,
	}
	applyCycleParams(row, f.AnchorDate, f.PeriodDays, f.DurationDays, f.Dosage, f.FrequencyText)
	return row
}

// applyCycleParams 把周期的可选参数（anchor/period/duration/dosage/frequency）填入 row，
// 并据 anchor+period 计算 next_predicted_at。LLM 路径（cycleRow）与手动路径（ManualAddCycle）
// 共用，保证「下次预测」算法与「LLM 未给的 0/空不落列」规则单点——散落两处易漂移。
// anchor 走 parseEventAt（UTC 午夜归一，防 DSN 转 UTC 偏移，见其注释）；period/duration<=0
// 视为 LLM 未给，不设列；next_predicted = anchor + period（两者齐才算，估算非医疗建议 spec §9）。
func applyCycleParams(row *repo.PersonCycle, anchorDate string, periodDays, durationDays int, dosage, frequency string) {
	if t, ok := parseEventAt(anchorDate); ok {
		row.AnchorDate = &t
	}
	if periodDays > 0 {
		pd := periodDays
		row.PeriodDays = &pd
	}
	if durationDays > 0 {
		dd := durationDays
		row.DurationDays = &dd
	}
	if d := strings.TrimSpace(dosage); d != "" {
		row.Dosage = &d
	}
	if fr := strings.TrimSpace(frequency); fr != "" {
		row.FrequencyText = &fr
	}
	if row.AnchorDate != nil && row.PeriodDays != nil {
		nxt := row.AnchorDate.AddDate(0, 0, *row.PeriodDays)
		row.NextPredictedAt = &nxt
	}
}

// cycleParamsEqual 判断已存在 active 周期的关键参数与新事实是否一致。
// 缺省兼容：新事实未给的参数（空串/0）不主张变化，与现值任意值兼容——
// 否则「详细记录后裸重提」（fact 只有 type+label）会被误判为变化而进冲突队列。
// 仅当新事实显式给出参数且与现值不同才视为变化。
func cycleParamsEqual(e *repo.PersonCycle, f Fact) bool {
	if fa := strings.TrimSpace(f.AnchorDate); fa != "" {
		if e.AnchorDate == nil {
			return false
		}
		if t, ok := parseEventAt(fa); !ok || !t.Equal(*e.AnchorDate) {
			return false
		}
	}
	if f.PeriodDays > 0 && derefInt(e.PeriodDays) != f.PeriodDays {
		return false
	}
	if f.DurationDays > 0 && derefInt(e.DurationDays) != f.DurationDays {
		return false
	}
	if d := strings.TrimSpace(f.Dosage); d != "" && derefStr(e.Dosage) != d {
		return false
	}
	if fr := strings.TrimSpace(f.FrequencyText); fr != "" && derefStr(e.FrequencyText) != fr {
		return false
	}
	return true
}

// ---- activity 平面行与审计构造（P4 生活轨迹，测点流语义，对齐 createMetricLog）----

// activityRow 构造一条 person_activity 行。tool/location/commuteMode/durationMin 由调用方
// （applyActivityFact / ManualAddActivity）trim/判空后传入（空→nil，走 repo 的 <=> NULL 匹配），
// 此处只负责组装——单点 trim、单点判空，避免散落两处漂移。activity 平面无 SupersedesID
// （测点流无版本取代语义，见 repo.PersonActivity 顶部说明），故行里不设该字段。
func activityRow(userID int64, personID ids.ID, f Fact, tool, location, commuteMode *string,
	durationMin *int, startedAt time.Time, status string, memID *ids.ID, prov Provenance) *repo.PersonActivity {
	return &repo.PersonActivity{
		UserID: userID, PersonID: personID,
		Activity:    strings.TrimSpace(f.ActivityText),
		Tool:        tool,
		Location:    location,
		CommuteMode: commuteMode,
		StartedAt:   startedAt,
		DurationMin: durationMin,
		Confidence:  f.Confidence, EpistemicType: f.EpistemicType,
		Source: "llm", Status: status, SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
}

func createActivityLog(personID ids.ID, row *repo.PersonActivity, memID *ids.ID, prov Provenance) *repo.PersonChangeLog {
	return &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "activity", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "llm", NewValue: snap(row.Activity),
		Confidence: fp(row.Confidence), SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
}

func createCycleLog(personID ids.ID, row *repo.PersonCycle, memID *ids.ID, prov Provenance, note string) *repo.PersonChangeLog {
	l := &repo.PersonChangeLog{
		PersonID: personID, EntityKind: "cycle", EntityID: &row.ID,
		ChangeType: "create", ChangedBy: "llm", NewValue: snap(row.CycleType),
		Confidence: fp(row.Confidence), SessionID: &prov.SessionID, MemoryID: memID,
		TranscriptSegmentIDs: ids.List(prov.SegmentIDs),
	}
	if note != "" {
		l.Note = strPtr(note)
	}
	return l
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

// trimToPtr trim 后空串→nil，非空→指向 trim 结果的指针。activity 三个可空串列
// （tool/location/commute_mode）的 <=> NULL 约定专用——空串与 NULL 混用会破坏自然键幂等，
// 故统一「空即 NULL」（对齐 cycle label 的 trim→nil 处理）。手动路径 ManualAddActivity 也复用。
func trimToPtr(s string) *string {
	if t := strings.TrimSpace(s); t != "" {
		return &t
	}
	return nil
}

func fp(f float64) *float64 { return &f }

// derefInt / derefStr 解指针取值，nil → 零值（周期同参比较用，见 cycleParamsEqual）。
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// formatMetricValue 数值型测点的规范字符串形态——value_num 与 value_text 的唯一格式化点
// （双存约定：自然键去重按 value_text 字符串比较，两列必须同源，散落 fmt 会漂移破坏幂等）。
func formatMetricValue(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// idPtr 0 → nil（SQL NULL 安全传参）。
func idPtr(id ids.ID) *ids.ID {
	if id == 0 {
		return nil
	}
	return &id
}

// parseEventAt 尽力解析事件时间并归一到「UTC 当日零点」（事件只需日期精度）。
// 支持格式：RFC3339（带时区）、2006-01-02T15:04:05（无时区 ISO）、2006-01-02、
// 2006/01/02（斜杠）、2006-01（月份精度）；全部失败返回 ok=false（调用方存 NULL——
// 事件仍创建，标题里常含时间信息）。解析职责在此而非 ParseFacts：Fact 是传输载体，
// 时间精度策略属落库层。
//
// 为何统一锚定 UTC 零点而非直存解析结果：occurred_at 是 DATETIME(3)，DSN 未配 loc → 驱动
// 写库时按 UTC 转换（v.In(UTC)）。带时区/带时刻的串直存会在转 UTC 时把凌晨偏移到前一天
// （实测 +08:00 05:00 → 前一日）。故取解析出的「原时区日历日」Y/M/D，重建为 UTC 当日零点，
// 保证落库日期＝用户书写的日历日。注意用 time.UTC 而非 t.Location()：后者（如 +08 零点）
// 转 UTC 仍落到前一日 16:00，修不掉偏移。
func parseEventAt(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
		"2006/01/02",
		"2006-01",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), true
		}
	}
	return time.Time{}, false
}

// parseMetricAt 解析测点时间，**保留时刻精度**（与 parseEventAt 刻意不同——后者抹平到 UTC
// 当天零点，丢弃时刻；而 metric 是连续时序，同一天多次测量要靠时刻区分，硬约束 4）。
// 依次尝试 RFC3339（带时区/时刻）、"2006-01-02 15:04"（日期+时分）、"2006-01-02"（纯日期）；
// 全部失败返回 fallback（调用方传 s.now()，保证 measured_at 列 NOT NULL 非零）。
//
// 说明：DSN 未配 loc，驱动写库按 UTC 转换——带时区的 RFC3339 会转成对应 UTC 瞬时存储，
// 瞬时语义正确（metric 关心的是「何时测的」这个时间点，而非日历日）；纯日期串按 UTC 零点存。
func parseMetricAt(s string, fallback time.Time) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return fallback
}

// textPtr 把字符串转指针：空串 → nil（存 SQL NULL），非空 → 指向该串。
// 供 metric 平面 value_text/unit 落库（空值存 NULL，而非空字符串）——与 strPtr（恒返回指针）不同。
func textPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// metricSummary 生成测点值摘要（change_log new_value 用）：
//
//	纯数值：weight=70kg（带单位）/ mood_energy=0.8（无量纲）
//	纯文本：diet=火锅
//	数值+文本兼有：emotion=-0.6/焦虑
//
// 数值用最短十进制表示（strconv -1 精度），避免 70 打成 70.000。
func metricSummary(m *repo.PersonMetric) string {
	b := m.MetricKey + "="
	if m.ValueNum != nil {
		b += strconv.FormatFloat(*m.ValueNum, 'f', -1, 64)
		if m.Unit != nil {
			b += *m.Unit
		}
	}
	if m.ValueText != nil {
		if m.ValueNum != nil {
			b += "/"
		}
		b += *m.ValueText
	}
	return b
}
