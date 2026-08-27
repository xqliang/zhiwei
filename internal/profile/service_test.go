package profile

import (
	"context"
	"os"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// fakeLLM 已在 extractor_test.go 定义（同包共享），此处不重复定义。

// TestMain 统一初始化雪花 ID 节点：本包此前的测试（extractor/gate/fact/catalog）
// 都不生成主键，故无需 ids.Init；service 测试要落库（EnsurePersonBootstrap、
// ids.New() 造 session），必须先设好 snowflake 节点，否则 node 为 nil 会 panic。
// TestMain 必须定义在 _test.go 里才会被 test 框架调用（一个包仅一个）。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}

// newTestService 建好 Service 并跑 bootstrap（owner「我」必备）。
// Memories/Speakers 必须给：ApplyFacts 读 session memories，speaker 归属解析查名册。
func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		DB: db, Persons: &repo.PersonRepo{DB: db},
		Sessions:      &repo.SessionRepo{DB: db},
		Memories:      &repo.MemoryRepo{DB: db},
		Speakers:      &repo.SpeakerRepo{DB: db},
		Attributes:    &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db},
		Events:        &repo.PersonEventRepo{DB: db},
		Metrics:       &repo.PersonMetricRepo{DB: db},
		Cycles:        &repo.PersonCycleRepo{DB: db},
		Activities:    &repo.PersonActivityRepo{DB: db},
		Pets:          &repo.PersonPetRepo{DB: db},
		ChangeLogs:    &repo.PersonChangeLogRepo{DB: db},
		Gate:          GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(context.Background(), svc.Persons, &repo.SpeakerRepo{DB: db}); err != nil {
		t.Fatal(err)
	}
	return svc
}

func ownerID(t *testing.T, svc *Service) ids.ID {
	t.Helper()
	o, err := svc.Persons.GetOwner(context.Background(), 1)
	if err != nil || o == nil {
		t.Fatalf("owner 缺失: %v %v", o, err)
	}
	return o.ID
}

func TestApplyFactsGatePaths(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	// 本用例把 occupation/personality/hobbies 写到共享 owner（user_id=1），并经关系事实
	// 自动新建 pending 人物 Alice（带 occupation=医生 + owner→Alice 配偶关系）。本包所有测试
	// 共用同一 zhiwei_test 库、不逐个重置，靠各用例使用不相交的 key/人名共存；但这些行若留到
	// 下一次 -count=1 重跑，会让本用例（occupation 撞现值→reaffirm、Alice 已 active）与
	// TestExtractSession（occupation 撞现值→冲突）等断言失败。收尾删掉 Alice + owner 的这三类
	// 属性 + 关系 + 审计，恢复干净基线（模式参照 extract_session_test.go）。提前用 t.Cleanup
	// 注册，保证任一断言 t.Fatal 提前退出时也会清理。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person WHERE user_id = 1 AND display_name = 'Alice'`)
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			ownerPK := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ? AND attr_key IN ('occupation','personality','hobbies')`, ownerPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_relationship WHERE person_id = ?`, ownerPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ?`, ownerPK)
		}
	})

	sess := ids.New()

	facts := []Fact{
		// ① 无现值高置信 observed → active
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "工程师", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ② 无现值低置信 → pending
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "personality",
			Value: "内向", Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ③ 列表低置信 → pending
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "hobbies",
			Value: "游泳", Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ④ 关系：mentioned:Alice 高置信 → active + 自动建 pending 人物 Alice
		{Plane: "relationship", Subject: Subject{Kind: "self"},
			Related: Subject{Kind: "mentioned", Name: "Alice"}, RelationType: "配偶",
			Label: "老婆", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ⑤ 关系指代 subject：属性挂到 owner 的配偶（=上一步新建的 Alice）身上
		{Plane: "attribute", Subject: Subject{Kind: "relation", Relation: "配偶"},
			AttrKey: "occupation", Value: "医生", Confidence: 0.9, EpistemicType: "observed",
			SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	// ①④⑤ active；②③ pending
	if st.Active != 3 || st.Pending != 2 || st.Skipped != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	// 校验：occupation=工程师 active；personality/hobbies pending
	oa, _ := svc.Attributes.FindActiveByKey(ctx, oid, "occupation")
	if oa == nil || oa.ValueText != "工程师" || oa.Source != "llm" || oa.SessionID == nil || *oa.SessionID != sess {
		t.Fatalf("occupation active 行错误: %+v", oa)
	}
	pa, _ := svc.Attributes.FindActiveByKey(ctx, oid, "personality")
	if pa != nil {
		t.Fatalf("低置信不应 active: %+v", pa)
	}
	// Alice：pending 人物 + 配偶关系 active + occupation active
	alice, _ := svc.Persons.FindByName(ctx, 1, "Alice")
	if alice == nil || alice.Status != "pending" || alice.Source != "llm" {
		t.Fatalf("Alice 人物错误: %+v", alice)
	}
	rel, err := svc.Relationships.FindActiveByTypeExt(ctx, svc.DB, oid, "配偶", &alice.ID)
	if err != nil || rel == nil {
		t.Fatalf("配偶关系未建立: %v %v", rel, err)
	}
	ao, _ := svc.Attributes.FindActiveByKey(ctx, alice.ID, "occupation")
	if ao == nil || ao.ValueText != "医生" {
		t.Fatalf("Alice 职业错误: %+v", ao)
	}
	// 审计：owner 侧至少 create(attribute×3) 条目 + person create(Alice) + relationship create
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "attribute", "")
	if len(logs) < 3 {
		t.Fatalf("owner 属性审计不足: %d", len(logs))
	}

	// 幂等：同 session 重跑全部 skip
	st2, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != len(facts) || st2.Active != 0 || st2.Pending != 0 || st2.Reaffirmed != 0 {
		t.Fatalf("重跑应全部 skip: %+v", st2)
	}
	// Alice 不被重复创建
	if a2, _ := svc.Persons.FindByName(ctx, 1, "Alice"); a2.ID != alice.ID {
		t.Fatal("Alice 被重复创建")
	}

	// 冲突：另一 session 说 occupation=教师（高置信）→ pending + supersedes 指向现值
	sess2 := ids.New()
	st3, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "教师", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3.Pending != 1 || st3.Conflicts != 1 {
		t.Fatalf("冲突统计错误: %+v", st3)
	}
	// supersedes 链接：冲突产生的 pending「教师」行须指向当前 active「工程师」行（oa），
	// 不静默覆盖现值——供确认队列展示「替换谁」。
	pend, _ := svc.Attributes.FindByNaturalKey(ctx, sess2, oid, "occupation", "教师")
	if pend == nil || pend.Status != "pending" || pend.SupersedesID == nil || *pend.SupersedesID != oa.ID {
		t.Fatalf("冲突 pending 应指向 active 现值行: pend=%+v 期望 supersedes=%d", pend, oa.ID)
	}
	// 佐证：sess2 重申 occupation=工程师（active 现值）→ reaffirm
	st4, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "工程师", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st4.Reaffirmed != 1 {
		t.Fatalf("同值重申应 reaffirm: %+v", st4)
	}
}

func TestApplyEventFacts(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	// 本用例把旅行/升职/健康事件写到共享 owner（user_id=1），并经 related 自动新建 pending
	// 人物「旅行同伴」。本包所有测试共用同一 zhiwei_test 库、不逐个重置；这些行若留到下一次
	// -count=1 重跑（或后续 api/repo 包测试跑在本包之后），会让事件计数/reaffirm/审计断言失真。
	// 收尾删掉 owner 的 person_event、owner 的 event 审计条目、以及「旅行同伴」人物，恢复干净
	// 基线（模式参照 extract_session_test.go / TestApplyFactsGatePaths）。提前用 t.Cleanup 注册，
	// 保证任一断言 t.Fatal 提前退出时也会清理。
	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			ownerPK := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_event WHERE person_id = ?`, ownerPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'event'`, ownerPK)
		}
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person WHERE user_id = 1 AND display_name = '旅行同伴'`)
	})

	sess := ids.New()

	facts := []Fact{
		// ① 高置信旅行事件 → active；occurred_at/end_at 解析成功
		{Plane: "event", Subject: Subject{Kind: "self"}, EventType: "旅行", EventTitle: "去云南旅游",
			EventDescription: "和家人自驾", OccurredAt: "2026-07-20", EndAt: "2026-07-27",
			EventLocation: "云南", Related: Subject{Kind: "mentioned", Name: "旅行同伴"},
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ② 低置信里程碑 → pending；occurred_at 烂串 → NULL 仍创建
		{Plane: "event", Subject: Subject{Kind: "self"}, EventType: "里程碑", EventTitle: "升职",
			OccurredAt: "去年夏天", Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != 1 || st.Pending != 1 || st.Skipped != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	evs, err := svc.Events.ListByPerson(ctx, oid)
	if err != nil || len(evs) != 2 {
		t.Fatalf("应 2 条事件: %d %v", len(evs), err)
	}
	// ListByPerson 时间倒序：有时间的旅行在前，NULL 的升职在后
	trip := evs[0]
	if trip.EventType != "旅行" || trip.Status != "active" || trip.Source != "llm" {
		t.Fatalf("旅行事件错误: %+v", trip)
	}
	if trip.OccurredAt == nil || trip.EndAt == nil || trip.Location == nil || trip.Description == nil {
		t.Fatalf("旅行事件可选字段应解析: %+v", trip)
	}
	// related 解析：自动新建 pending 人物「旅行同伴」
	comp, _ := svc.Persons.FindByName(ctx, 1, "旅行同伴")
	if comp == nil || comp.Status != "pending" {
		t.Fatalf("related 应建 pending 人物: %+v", comp)
	}
	if len(trip.RelatedPersonIDs) != 1 || trip.RelatedPersonIDs[0] != comp.ID {
		t.Fatalf("RelatedPersonIDs 错误: %v", trip.RelatedPersonIDs)
	}
	promo := evs[1]
	if promo.Status != "pending" || promo.OccurredAt != nil {
		t.Fatalf("升职应 pending 且时间为 NULL: %+v", promo)
	}

	// 幂等：同 session 重跑全 skip
	st2, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != 2 || st2.Active != 0 || st2.Pending != 0 {
		t.Fatalf("重跑应全 skip: %+v", st2)
	}

	// 佐证：另一 session 同键 → reaffirm（不 bump、不加行）
	sess2 := ids.New()
	st3, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "event", Subject: Subject{Kind: "self"}, EventType: "旅行", EventTitle: "去云南旅游",
			Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3.Reaffirmed != 1 {
		t.Fatalf("同键应 reaffirm: %+v", st3)
	}
	evs2, _ := svc.Events.ListByPerson(ctx, oid)
	if len(evs2) != 2 {
		t.Fatalf("reaffirm 不应加行: %d", len(evs2))
	}

	// M1/M2 回归：parseEventAt 时区截断 + 扩展格式（用不同 title 避开自然键去重）
	sess3 := ids.New()
	st5, err := svc.ApplyFacts(ctx, sess3, 1, []Fact{
		// ① RFC3339 带 +08:00 凌晨：应截断到当日零点，落库日期仍是 07-20（防 M1 时区偏移回归——
		//    直存会在驱动转 UTC 时把 05:00+08 偏移到前一天 07-19）
		{Plane: "event", Subject: Subject{Kind: "self"}, EventType: "会议", EventTitle: "季度评审会",
			OccurredAt: "2026-07-20T05:00:00+08:00", Confidence: 0.9, EpistemicType: "observed"},
		// ② 斜杠日期：扩展格式应解析成功、非 NULL（防 M2 常见 LLM 格式静默丢时间）
		{Plane: "event", Subject: Subject{Kind: "self"}, EventType: "聚会", EventTitle: "老友饭局",
			OccurredAt: "2026/07/20", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st5.Active != 2 {
		t.Fatalf("扩展格式两条应 active: %+v", st5)
	}
	rev, _ := svc.Events.FindActiveByNormalizedTitleExt(ctx, svc.DB, oid, "会议", "季度评审会")
	if rev == nil || rev.OccurredAt == nil {
		t.Fatalf("会议事件应有 occurred_at: %+v", rev)
	}
	if got := rev.OccurredAt.UTC().Format("2006-01-02"); got != "2026-07-20" {
		t.Fatalf("M1 时区截断回归：+08:00 凌晨应落库 2026-07-20，实得 %s", got)
	}
	party, _ := svc.Events.FindActiveByNormalizedTitleExt(ctx, svc.DB, oid, "聚会", "老友饭局")
	if party == nil || party.OccurredAt == nil {
		t.Fatalf("M2 斜杠日期应解析成功（非 NULL）: %+v", party)
	}
	if got := party.OccurredAt.UTC().Format("2006-01-02"); got != "2026-07-20" {
		t.Fatalf("M2 斜杠日期应落库 2026-07-20，实得 %s", got)
	}

	// 手动加/删事件
	me, err := svc.ManualAddEvent(ctx, 1, oid, "健康", "确诊高血压", "长期服药", "2025-06-01", "", "北京协和", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if me.Status != "active" || me.Source != "manual" || me.OccurredAt == nil {
		t.Fatalf("手动事件错误: %+v", me)
	}
	if err := svc.ManualDeleteEvent(ctx, 1, me.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Events.Get(ctx, me.ID); d.Status != "dismissed" {
		t.Fatalf("删除应 dismissed: %+v", d)
	}
	// 审计：event 平面条目（llm create×2 + reaffirm + user create + delete）
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "event", "")
	if len(logs) < 5 {
		t.Fatalf("event 审计不足: %d", len(logs))
	}
}
