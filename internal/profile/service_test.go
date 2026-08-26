package profile

import (
	"context"
	"os"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
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
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		DB: db, Persons: &repo.PersonRepo{DB: db},
		Sessions:      &repo.SessionRepo{DB: db}, // metric measured_at 兜底取 session.created_at
		Memories:      &repo.MemoryRepo{DB: db},
		Speakers:      &repo.SpeakerRepo{DB: db},
		Attributes:    &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db},
		Events:        &repo.PersonEventRepo{DB: db},
		Metrics:       &repo.PersonMetricRepo{DB: db},
		Cycles:        &repo.PersonCycleRepo{DB: db},
		Activities:    &repo.PersonActivityRepo{DB: db},
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
	rev, _ := svc.Events.FindActiveByKeyExt(ctx, svc.DB, oid, "会议", "季度评审会")
	if rev == nil || rev.OccurredAt == nil {
		t.Fatalf("会议事件应有 occurred_at: %+v", rev)
	}
	if got := rev.OccurredAt.UTC().Format("2006-01-02"); got != "2026-07-20" {
		t.Fatalf("M1 时区截断回归：+08:00 凌晨应落库 2026-07-20，实得 %s", got)
	}
	party, _ := svc.Events.FindActiveByKeyExt(ctx, svc.DB, oid, "聚会", "老友饭局")
	if party == nil || party.OccurredAt == nil {
		t.Fatalf("M2 斜杠日期应解析成功（非 NULL）: %+v", party)
	}
	if got := party.OccurredAt.UTC().Format("2006-01-02"); got != "2026-07-20" {
		t.Fatalf("M2 斜杠日期应落库 2026-07-20，实得 %s", got)
	}

	// 手动加/删事件
	me, err := svc.ManualAddEvent(ctx, oid, "健康", "确诊高血压", "长期服药", "2025-06-01", "", "北京协和", nil)
	if err != nil {
		t.Fatal(err)
	}
	if me.Status != "active" || me.Source != "manual" || me.OccurredAt == nil {
		t.Fatalf("手动事件错误: %+v", me)
	}
	if err := svc.ManualDeleteEvent(ctx, me.ID); err != nil {
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

func TestApplyMetricFacts(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	// metric 平面把体重/情绪测点写到共享 owner（user_id=1）。本包所有测试共用同一 zhiwei_test
	// 库、不逐个重置；这些行若留到下一次 -count=1 重跑会让测点计数/审计断言失真。收尾删掉 owner
	// 的 person_metric、owner 的 metric 审计条目、以及本用例造的 session。提前用 t.Cleanup 注册，
	// 保证任一断言 t.Fatal 提前退出时也会清理（模式参照 TestApplyEventFacts）。
	var sessPK int64
	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			ownerPK := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_metric WHERE person_id = ?`, ownerPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'metric'`, ownerPK)
		}
		if sessPK != 0 {
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM audio_session WHERE id = ?`, sessPK)
		}
	})

	// metric 的 measured_at 兜底取 session.created_at，故须造真实 session（不能用裸 ids.New()）：
	// created_at 稳定才能保证「空 measured_at」测点重跑时自然键命中而 skip（time.Now() 兜底会漂移）。
	sess := &repo.AudioSession{ID: ids.New(), Source: "web_upload", Filename: "m.wav", StoragePath: "/tmp/m.wav", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sessPK = sess.ID.Int64()
	ss, err := svc.Sessions.Get(ctx, sess.ID) // 读回 DB 默认填充的 created_at
	if err != nil {
		t.Fatal(err)
	}

	facts := []Fact{
		// ① 数值型 weight 高置信 → active；value_num/value_text 双存一致
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "weight",
			MetricValue: "72.5", MetricUnit: "kg", MeasuredAt: "2026-08-20",
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ② 类别型 emotion 低置信 → pending；value_num 为 NULL
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "emotion",
			MetricValue: "焦虑", MeasuredAt: "2026-08-20",
			Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ③ MeasuredAt 空 → measured_at 落 sessionTime（= session.created_at）
		{Plane: "metric", Subject: Subject{Kind: "self"}, MetricKey: "weight",
			MetricValue: "70", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess.ID, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	// ①③ active；② pending
	if st.Active != 2 || st.Pending != 1 || st.Skipped != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	ws, err := svc.Metrics.ListByPerson(ctx, oid, "weight", nil, nil)
	if err != nil || len(ws) != 2 {
		t.Fatalf("应 2 条 weight 测点: %d %v", len(ws), err)
	}
	var w725, w70 *repo.PersonMetric
	for i := range ws {
		if ws[i].ValueText == nil {
			continue
		}
		switch *ws[i].ValueText {
		case "72.5":
			w725 = &ws[i]
		case "70":
			w70 = &ws[i]
		}
	}
	if w725 == nil || w70 == nil {
		t.Fatalf("weight 测点值缺失（双存 value_text）: %+v", ws)
	}
	// 数值型双存一致：value_num=72.5 且 value_text="72.5"（同源，防漂移）
	if w725.Status != "active" || w725.Source != "llm" {
		t.Fatalf("weight 72.5 应 llm/active: %+v", w725)
	}
	if w725.ValueNum == nil || *w725.ValueNum != 72.5 {
		t.Fatalf("value_num 应 72.5: %v", w725.ValueNum)
	}
	if w725.Unit == nil || *w725.Unit != "kg" {
		t.Fatalf("unit 应 kg: %v", w725.Unit)
	}
	if w725.MeasuredAt.UTC().Format("2006-01-02") != "2026-08-20" {
		t.Fatalf("measured_at 应 2026-08-20，实得 %s", w725.MeasuredAt.UTC().Format("2006-01-02"))
	}
	// 类别型：value_num NULL，value_text 存类别串
	es, _ := svc.Metrics.ListByPerson(ctx, oid, "emotion", nil, nil)
	if len(es) != 1 {
		t.Fatalf("应 1 条 emotion 测点: %d", len(es))
	}
	if em := es[0]; em.Status != "pending" || em.ValueNum != nil || em.ValueText == nil || *em.ValueText != "焦虑" {
		t.Fatalf("emotion 类别型测点错误（应 pending/value_num NULL/value_text=焦虑）: %+v", em)
	}
	// ③ 空 measured_at → 落 session.created_at 日期
	if w70.Status != "active" || w70.ValueNum == nil || *w70.ValueNum != 70 {
		t.Fatalf("weight 70 应 active 且 value_num=70: %+v", w70)
	}
	if got, want := w70.MeasuredAt.UTC().Format("2006-01-02"), ss.CreatedAt.UTC().Format("2006-01-02"); got != want {
		t.Fatalf("空 measured_at 应落 session 时间 %s，实得 %s", want, got)
	}

	// 幂等：同 session 重跑全 skip（数值经 formatMetricValue 串比较，sessionTime 稳定）
	st2, err := svc.ApplyFacts(ctx, sess.ID, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != 3 || st2.Active != 0 || st2.Pending != 0 {
		t.Fatalf("重跑应全 skip: %+v", st2)
	}

	// 手动加（value="73.2" → 双存）删
	mm, err := svc.ManualAddMetric(ctx, oid, "weight", "73.2", "kg", "")
	if err != nil {
		t.Fatal(err)
	}
	if mm.Status != "active" || mm.Source != "manual" {
		t.Fatalf("手动测点应 manual/active: %+v", mm)
	}
	if mm.ValueNum == nil || *mm.ValueNum != 73.2 || mm.ValueText == nil || *mm.ValueText != "73.2" {
		t.Fatalf("手动数值型双存应一致（73.2/\"73.2\"）: num=%v text=%v", mm.ValueNum, mm.ValueText)
	}
	if err := svc.ManualDeleteMetric(ctx, mm.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Metrics.Get(ctx, mm.ID); d == nil || d.Status != "dismissed" {
		t.Fatalf("删除应 dismissed: %+v", d)
	}
	// 审计：metric 平面条目（llm create×3 + user create + delete = 5）
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "metric", "")
	if len(logs) < 5 {
		t.Fatalf("metric 审计不足: %d", len(logs))
	}
}

func TestApplyCycleFacts(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	// cycle 平面把服药/生理期/随访周期写到共享 owner（user_id=1）。含冲突后 pending 行、手动
	// dismissed 行——测试内自己造的数据自己清，按 person_id 全删即可（模式参照 TestApplyEventFacts）。
	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			ownerPK := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_cycle WHERE person_id = ?`, ownerPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'cycle'`, ownerPK)
		}
	})

	sess1 := ids.New()
	facts := []Fact{
		// ① medication 降压药 anchor 2026-08-01 + period 30 高置信 → active
		{Plane: "cycle", Subject: Subject{Kind: "self"}, CycleType: "medication",
			CycleLabel: "降压药", AnchorDate: "2026-08-01", PeriodDays: 30,
			Dosage: "5mg", FrequencyText: "每日一次",
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ② menstrual 空 label 合法 → active（label nil）
		{Plane: "cycle", Subject: Subject{Kind: "self"}, CycleType: "menstrual",
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess1, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != 2 || st.Pending != 0 || st.Skipped != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	cs, err := svc.Cycles.ListByPerson(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	var med, men *repo.PersonCycle
	for i := range cs {
		switch cs[i].CycleType {
		case "medication":
			med = &cs[i]
		case "menstrual":
			men = &cs[i]
		}
	}
	if med == nil || men == nil {
		t.Fatalf("周期记录缺失: %+v", cs)
	}
	if med.Status != "active" || med.Source != "llm" || med.Label == nil || *med.Label != "降压药" {
		t.Fatalf("medication 行错误: %+v", med)
	}
	if med.PeriodDays == nil || *med.PeriodDays != 30 {
		t.Fatalf("period_days 应 30: %v", med.PeriodDays)
	}
	if med.AnchorDate == nil || med.AnchorDate.UTC().Format("2006-01-02") != "2026-08-01" {
		t.Fatalf("anchor_date 应 2026-08-01: %v", med.AnchorDate)
	}
	// next_predicted_at = anchor + period = 2026-08-01 + 30d = 2026-08-31
	if med.NextPredictedAt == nil || med.NextPredictedAt.UTC().Format("2006-01-02") != "2026-08-31" {
		t.Fatalf("next_predicted_at 应 2026-08-31: %v", med.NextPredictedAt)
	}
	// 空 label 的 menstrual：label nil
	if men.Status != "active" || men.Label != nil {
		t.Fatalf("menstrual 空 label 应 nil: %+v", men)
	}
	medID := med.ID

	// 幂等：同 session 重跑全 skip
	st2, err := svc.ApplyFacts(ctx, sess1, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != 2 || st2.Active != 0 {
		t.Fatalf("重跑应全 skip: %+v", st2)
	}

	// 同参重提：另一 session、同 anchor/period/dosage/frequency（参数没变）→ Reaffirm（仅审计、
	// 不加行、不进队列），防「还在吃降压药」每 session 造一条冲突 pending 的确认疲劳（review Important）。
	sessSame := ids.New()
	st3a, err := svc.ApplyFacts(ctx, sessSame, 1, []Fact{
		{Plane: "cycle", Subject: Subject{Kind: "self"}, CycleType: "medication",
			CycleLabel: "降压药", AnchorDate: "2026-08-01", PeriodDays: 30,
			Dosage: "5mg", FrequencyText: "每日一次",
			Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3a.Reaffirmed != 1 || st3a.Active != 0 || st3a.Pending != 0 || st3a.Conflicts != 0 {
		t.Fatalf("同参重提应 Reaffirm 不加行: %+v", st3a)
	}
	// 不加行：medication 仍只有 sess1 那条 active
	csa, _ := svc.Cycles.ListByPerson(ctx, oid)
	nMed := 0
	for _, c := range csa {
		if c.CycleType == "medication" {
			nMed++
		}
	}
	if nMed != 1 {
		t.Fatalf("同参佐证不应加行，medication 应仍 1 条，实得 %d", nMed)
	}

	// 详细记录后「裸重提」：另一 session 只给 type+label（anchor/period/dosage/frequency 全空、
	// 高置信）——缺省兼容语义下「新事实未给的参数不主张变化」，与现值任意值兼容 → Reaffirm，
	// 不再因「anchor/period 有→无」被误判为变化而进 ConflictPending（确认疲劳边界修复；review Important）。
	sessBare := ids.New()
	st3b, err := svc.ApplyFacts(ctx, sessBare, 1, []Fact{
		{Plane: "cycle", Subject: Subject{Kind: "self"}, CycleType: "medication",
			CycleLabel: "降压药", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3b.Reaffirmed != 1 || st3b.Active != 0 || st3b.Pending != 0 || st3b.Conflicts != 0 {
		t.Fatalf("详细记录后裸重提应 Reaffirm 不进冲突: %+v", st3b)
	}
	// 不加行：medication 仍只有 sess1 那条 active
	csb, _ := svc.Cycles.ListByPerson(ctx, oid)
	nMedBare := 0
	for _, c := range csb {
		if c.CycleType == "medication" {
			nMedBare++
		}
	}
	if nMedBare != 1 {
		t.Fatalf("裸重提不应加行，medication 应仍 1 条，实得 %d", nMedBare)
	}

	// 冲突：另一 session 同 (type,label) 不同参数 → ConflictPending + supersedes 指向 active 行
	sess2 := ids.New()
	st3, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "cycle", Subject: Subject{Kind: "self"}, CycleType: "medication",
			CycleLabel: "降压药", AnchorDate: "2026-09-01", PeriodDays: 20,
			Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3.Conflicts != 1 || st3.Pending != 1 {
		t.Fatalf("冲突统计错误: %+v", st3)
	}
	pend, err := svc.Cycles.FindByNaturalKeyExt(ctx, svc.DB, sess2, oid, "medication", strPtr("降压药"))
	if err != nil {
		t.Fatal(err)
	}
	if pend == nil || pend.Status != "pending" || pend.SupersedesID == nil || *pend.SupersedesID != medID {
		t.Fatalf("冲突 pending 应指向 active 现值行: pend=%+v 期望 supersedes=%d", pend, medID)
	}

	// 纯低置信 pending（新 cycle 无 existing、无冲突）+ partial 参数（仅 period 无 anchor →
	// next_predicted nil）——两条不同 (type,label) 一批落库。
	sessMisc := ids.New()
	st4, err := svc.ApplyFacts(ctx, sessMisc, 1, []Fact{
		// 低置信 → pending（injection 无现值，非冲突路径）
		{Plane: "cycle", Subject: Subject{Kind: "self"}, CycleType: "injection",
			CycleLabel: "胰岛素", Confidence: 0.5, EpistemicType: "observed"},
		// partial：仅 period 无 anchor，高置信 → active，next_predicted 应 nil
		{Plane: "cycle", Subject: Subject{Kind: "self"}, CycleType: "followup",
			CycleLabel: "复查", PeriodDays: 90, Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st4.Pending != 1 || st4.Active != 1 || st4.Conflicts != 0 {
		t.Fatalf("低置信 pending + partial active 统计错误: %+v", st4)
	}
	inj, _ := svc.Cycles.FindByNaturalKeyExt(ctx, svc.DB, sessMisc, oid, "injection", strPtr("胰岛素"))
	if inj == nil || inj.Status != "pending" {
		t.Fatalf("低置信应 pending: %+v", inj)
	}
	fu, _ := svc.Cycles.FindByNaturalKeyExt(ctx, svc.DB, sessMisc, oid, "followup", strPtr("复查"))
	if fu == nil || fu.Status != "active" {
		t.Fatalf("partial 高置信应 active: %+v", fu)
	}
	if fu.PeriodDays == nil || *fu.PeriodDays != 90 {
		t.Fatalf("partial period 应 90: %v", fu.PeriodDays)
	}
	if fu.NextPredictedAt != nil {
		t.Fatalf("partial（仅 period 无 anchor）不应有 next_predicted: %v", fu.NextPredictedAt)
	}

	// 手动加删（period 0 无 next_predicted）
	mc, err := svc.ManualAddCycle(ctx, oid, "followup", "复诊", "", "每月一次", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if mc.Status != "active" || mc.Source != "manual" || mc.Label == nil || *mc.Label != "复诊" {
		t.Fatalf("手动周期应 manual/active/label=复诊: %+v", mc)
	}
	if mc.NextPredictedAt != nil {
		t.Fatalf("无 anchor/period 不应有 next_predicted: %v", mc.NextPredictedAt)
	}
	if err := svc.ManualDeleteCycle(ctx, mc.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Cycles.Get(ctx, mc.ID); d == nil || d.Status != "dismissed" {
		t.Fatalf("删除应 dismissed: %+v", d)
	}
	// 审计：cycle 平面条目（llm create×5 + reaffirm×2〔同参重提 + 裸重提〕 + user create + delete = 9）
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "cycle", "")
	if len(logs) < 5 {
		t.Fatalf("cycle 审计不足: %d", len(logs))
	}
}

func TestApplyActivityFacts(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	// activity 平面把通勤/写代码/游泳活动写到共享 owner（user_id=1）。本包所有测试共用同一
	// zhiwei_test 库、不逐个重置；这些行若留到下一次 -count=1 重跑会让活动计数/审计断言失真。收尾
	// 删掉 owner 的 person_activity、owner 的 activity 审计条目、以及本用例造的 session。提前用
	// t.Cleanup 注册，保证任一断言 t.Fatal 提前退出时也会清理（模式参照 TestApplyMetricFacts）。
	var sessPK int64
	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			ownerPK := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_activity WHERE person_id = ?`, ownerPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'activity'`, ownerPK)
		}
		if sessPK != 0 {
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM audio_session WHERE id = ?`, sessPK)
		}
	})

	// activity 的 started_at 兜底取 session.created_at（同 metric measured_at），故须造真实 session
	// （不能用裸 ids.New()）：created_at 稳定才能保证「空 started_at」活动重跑时自然键命中而 skip
	// （time.Now() 兜底会漂移）。
	sess := &repo.AudioSession{ID: ids.New(), Source: "web_upload", Filename: "a.wav", StoragePath: "/tmp/a.wav", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sessPK = sess.ID.Int64()
	ss, err := svc.Sessions.Get(ctx, sess.ID) // 读回 DB 默认填充的 created_at
	if err != nil {
		t.Fatal(err)
	}

	facts := []Fact{
		// ① 通勤 高置信 → active；commute_mode=地铁，tool/location 空(NULL)，duration=40
		{Plane: "activity", Subject: Subject{Kind: "self"}, ActivityText: "通勤",
			CommuteMode: "地铁", StartedAt: "2026-08-20", DurationMin: 40,
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ② 写代码 低置信 → pending；tool=电脑 location=公司，duration 未给(≤0→NULL)
		{Plane: "activity", Subject: Subject{Kind: "self"}, ActivityText: "写代码",
			Tool: "电脑", Location: "公司", StartedAt: "2026-08-20",
			Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ③ StartedAt 空 → started_at 落 sessionTime（= session.created_at）；四个可空全空→NULL
		{Plane: "activity", Subject: Subject{Kind: "self"}, ActivityText: "游泳",
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess.ID, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	// ①③ active；② pending
	if st.Active != 2 || st.Pending != 1 || st.Skipped != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	acts, err := svc.Activities.ListByPerson(ctx, oid, nil, nil)
	if err != nil || len(acts) != 3 {
		t.Fatalf("应 3 条活动: %d %v", len(acts), err)
	}
	// 按 activity 取（不依赖顺序：ListByPerson 升序，但 session 兜底日期与 8-20 相对顺序随运行日变化）
	var commute, coding, swim *repo.PersonActivity
	for i := range acts {
		switch acts[i].Activity {
		case "通勤":
			commute = &acts[i]
		case "写代码":
			coding = &acts[i]
		case "游泳":
			swim = &acts[i]
		}
	}
	if commute == nil || coding == nil || swim == nil {
		t.Fatalf("活动缺失: %+v", acts)
	}
	// ① 通勤：active/llm，commute_mode=地铁，tool/location NULL，duration=40，started_at=8-20
	if commute.Status != "active" || commute.Source != "llm" {
		t.Fatalf("通勤应 llm/active: %+v", commute)
	}
	if commute.CommuteMode == nil || *commute.CommuteMode != "地铁" {
		t.Fatalf("commute_mode 应 地铁: %v", commute.CommuteMode)
	}
	if commute.Tool != nil || commute.Location != nil {
		t.Fatalf("通勤 tool/location 应 NULL: tool=%v loc=%v", commute.Tool, commute.Location)
	}
	if commute.DurationMin == nil || *commute.DurationMin != 40 {
		t.Fatalf("duration 应 40: %v", commute.DurationMin)
	}
	if commute.StartedAt.UTC().Format("2006-01-02") != "2026-08-20" {
		t.Fatalf("started_at 应 2026-08-20，实得 %s", commute.StartedAt.UTC().Format("2006-01-02"))
	}
	// ② 写代码：pending，tool=电脑 location=公司，duration NULL（未给不臆造 0）
	if coding.Status != "pending" || coding.Tool == nil || *coding.Tool != "电脑" ||
		coding.Location == nil || *coding.Location != "公司" {
		t.Fatalf("写代码 pending + tool/location 错误: %+v", coding)
	}
	if coding.DurationMin != nil {
		t.Fatalf("未给 duration 应 NULL（不臆造 0）: %v", coding.DurationMin)
	}
	// ③ 空 started_at → 落 session.created_at 日期；四个可空全 NULL
	if swim.Status != "active" || swim.Tool != nil || swim.Location != nil || swim.CommuteMode != nil || swim.DurationMin != nil {
		t.Fatalf("游泳 active + 全可空 NULL 错误: %+v", swim)
	}
	if got, want := swim.StartedAt.UTC().Format("2006-01-02"), ss.CreatedAt.UTC().Format("2006-01-02"); got != want {
		t.Fatalf("空 started_at 应落 session 时间 %s，实得 %s", want, got)
	}

	// 幂等：同 session 重跑全 skip。游泳那条 tool/location/commute/duration 全 NULL，仍被
	// FindByNaturalKeyExt 的 <=> 命中而 skip——验证可空列 NULL 自然键幂等（started_at 稳定：
	// 8-20 显式 + 游泳落 session.created_at 稳定）。
	st2, err := svc.ApplyFacts(ctx, sess.ID, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != 3 || st2.Active != 0 || st2.Pending != 0 {
		t.Fatalf("重跑应全 skip（可空 NULL 自然键命中）: %+v", st2)
	}

	// 手动加删（含可空串留空 → NULL；duration=30）
	ma, err := svc.ManualAddActivity(ctx, oid, "打球", "", "健身房", "", "2026-08-21", 30)
	if err != nil {
		t.Fatal(err)
	}
	if ma.Status != "active" || ma.Source != "manual" || ma.Confidence != 1.0 {
		t.Fatalf("手动活动应 manual/active/conf=1.0: %+v", ma)
	}
	if ma.Tool != nil {
		t.Fatalf("手动空 tool 应 NULL: %v", ma.Tool)
	}
	if ma.Location == nil || *ma.Location != "健身房" || ma.DurationMin == nil || *ma.DurationMin != 30 {
		t.Fatalf("手动 location/duration 错误: loc=%v dur=%v", ma.Location, ma.DurationMin)
	}
	if err := svc.ManualDeleteActivity(ctx, ma.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Activities.Get(ctx, ma.ID); d == nil || d.Status != "dismissed" {
		t.Fatalf("删除应 dismissed: %+v", d)
	}
	// 审计：activity 平面条目（llm create×3 + user create + delete = 5）
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "activity", "")
	if len(logs) < 5 {
		t.Fatalf("activity 审计不足: %d", len(logs))
	}
}
