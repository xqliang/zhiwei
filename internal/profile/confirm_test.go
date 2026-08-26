package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestManualAndConfirmFlows(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	// 本用例往共享 owner（user_id=1）写 city/personality 属性 + 朋友关系，并新建 Bob（→Bob2）
	// 与「确认人物测试」两个人物（后者最终被确认为 active）。本包共用同一 zhiwei_test 库，若不
	// 清理，这些行会让下一次 -count=1 重跑失败——尤其「确认人物测试」残留为 active 后，重跑时
	// resolveOrCreateByName 会命中它而非新建 pending，断言 cand.Status=="pending" 失败。收尾删掉
	// 这三个人物 + owner 的 city/personality 属性 + 关系 + 审计，恢复干净基线（模式参照
	// extract_session_test.go）。提前用 t.Cleanup 注册，保证 t.Fatal 提前退出时也会清理。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person WHERE user_id = 1 AND display_name IN ('Bob','Bob2','确认人物测试')`)
		if o, err := svc.Persons.GetOwner(cctx, 1); err == nil && o != nil {
			ownerPK := o.ID.Int64()
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ? AND attr_key IN ('city','personality')`, ownerPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_relationship WHERE person_id = ?`, ownerPK)
			_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ?`, ownerPK)
		}
	})

	// ---- 手动建人物 + 手动加属性 ----
	p, err := svc.ManualCreatePerson(ctx, "Bob", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "active" || p.Source != "manual" {
		t.Fatalf("手动人物应 active/manual: %+v", p)
	}
	// 手动加属性：单值 key 无现值 → active conf=1.0 source=manual
	a1, err := svc.ManualAddAttribute(ctx, oid, "city", "北京")
	if err != nil {
		t.Fatal(err)
	}
	if a1.Status != "active" || a1.Confidence != 1.0 || a1.Source != "manual" {
		t.Fatalf("手动属性错误: %+v", a1)
	}
	// 手动改值：旧行 superseded、新行 active 且 supersedes_id 指向旧行
	a2, err := svc.ManualAddAttribute(ctx, oid, "city", "上海")
	if err != nil {
		t.Fatal(err)
	}
	if a2.Status != "active" || a2.SupersedesID == nil || *a2.SupersedesID != a1.ID {
		t.Fatalf("手动改值应 supersede: %+v", a2)
	}
	old, _ := svc.Attributes.Get(ctx, a1.ID)
	if old.Status != "superseded" {
		t.Fatalf("旧值应 superseded: %+v", old)
	}
	// 手动加关系
	rel, err := svc.ManualAddRelationship(ctx, oid, "朋友", &p.ID, "", "", "老朋友")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Status != "active" || rel.Source != "manual" {
		t.Fatalf("手动关系错误: %+v", rel)
	}

	// ---- 确认队列：冲突 pending 确认 → 旧 superseded 新 active ----
	// 此刻 city 的 active 行是 a2（上海）
	sess := ids.New()
	_, err = svc.ApplyFacts(ctx, sess, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "city",
			Value: "深圳", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pend, _ := svc.Attributes.ListPending(ctx, 1)
	var cityPend *ids.ID
	for i := range pend {
		if pend[i].AttrKey == "city" && pend[i].ValueText == "深圳" {
			idv := pend[i].ID
			cityPend = &idv
		}
	}
	if cityPend == nil {
		t.Fatal("city 深圳 pending 未生成")
	}
	if err := svc.ConfirmPending(ctx, "attribute", *cityPend); err != nil {
		t.Fatal(err)
	}
	confirmed, _ := svc.Attributes.Get(ctx, *cityPend)
	if confirmed.Status != "active" {
		t.Fatalf("确认后应 active: %+v", confirmed)
	}
	if confirmed.SupersedesID == nil || *confirmed.SupersedesID != a2.ID {
		t.Fatalf("冲突确认行应 supersedes a2: %+v", confirmed.SupersedesID)
	}
	replaced, _ := svc.Attributes.Get(ctx, a2.ID)
	if replaced.Status != "superseded" {
		t.Fatalf("被替换的上海行应 superseded: %+v", replaced)
	}

	// ---- 手动删属性 → dismissed（放最后：前面冲突流依赖 city 的 active 行）----
	if err := svc.ManualDeleteAttribute(ctx, a2.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Attributes.Get(ctx, a2.ID); d.Status != "dismissed" {
		t.Fatalf("删除应 dismissed: %+v", d)
	}

	// ---- 放弃：pending → dismissed ----
	sess2 := ids.New()
	_, _ = svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "personality",
			Value: "外向", Confidence: 0.5, EpistemicType: "observed"},
	})
	pend2, _ := svc.Attributes.ListPending(ctx, 1)
	if len(pend2) == 0 {
		t.Fatal("应有 pending")
	}
	if err := svc.DismissPending(ctx, "attribute", pend2[0].ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Attributes.Get(ctx, pend2[0].ID); d.Status != "dismissed" {
		t.Fatalf("放弃后应 dismissed: %+v", d)
	}

	// ---- 确认 pending 人物 ----
	// 低置信（0.6<0.75）：自动新建的人物「确认人物测试」是 pending，
	// 且这条朋友关系边也落 pending（关系闸门无冲突路径，不会带 supersedes_id）——
	// 正好给下面的「确认 pending 关系」提供素材。
	sess3 := ids.New()
	_, _ = svc.ApplyFacts(ctx, sess3, 1, []Fact{
		{Plane: "relationship", Subject: Subject{Kind: "self"},
			Related: Subject{Kind: "mentioned", Name: "确认人物测试"}, RelationType: "朋友",
			Confidence: 0.6, EpistemicType: "observed"},
	})
	cand, _ := svc.Persons.FindByName(ctx, 1, "确认人物测试")
	if cand == nil || cand.Status != "pending" {
		t.Fatalf("应为 pending 人物: %+v", cand)
	}
	if err := svc.ConfirmPending(ctx, "person", cand.ID); err != nil {
		t.Fatal(err)
	}
	if c2, _ := svc.Persons.Get(ctx, 1, cand.ID); c2.Status != "active" {
		t.Fatalf("人物确认后应 active: %+v", c2)
	}

	// ---- 确认 pending 关系 → active ----
	relPend, _ := svc.Relationships.ListPending(ctx, 1)
	if len(relPend) == 0 {
		t.Fatal("应有 pending 关系")
	}
	relPendID := relPend[0].ID
	if err := svc.ConfirmPending(ctx, "relationship", relPendID); err != nil {
		t.Fatal(err)
	}
	if rc, _ := svc.Relationships.Get(ctx, relPendID); rc == nil || rc.Status != "active" {
		t.Fatalf("关系确认后应 active: %+v", rc)
	}

	// ---- 手动编辑/归档人物 + 手动删关系（最小覆盖）----
	if err := svc.ManualUpdatePerson(ctx, p.ID, "Bob2", nil, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := svc.Persons.Get(ctx, 1, p.ID); b == nil || b.DisplayName != "Bob2" {
		t.Fatalf("改名后应为 Bob2: %+v", b)
	}
	if err := svc.ManualSetPersonStatus(ctx, p.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if b, _ := svc.Persons.Get(ctx, 1, p.ID); b == nil || b.Status != "dismissed" {
		t.Fatalf("归档后应 dismissed: %+v", b)
	}
	if err := svc.ManualDeleteRelationship(ctx, rel.ID); err != nil {
		t.Fatal(err)
	}
	if r, _ := svc.Relationships.Get(ctx, rel.ID); r == nil || r.Status != "dismissed" {
		t.Fatalf("删关系后应 dismissed: %+v", r)
	}

	// ---- 不存在/状态非法 → 错误 ----
	if err := svc.ConfirmPending(ctx, "attribute", ids.New()); err == nil {
		t.Fatal("不存在的 id 应报错")
	}
	if err := svc.ConfirmPending(ctx, "bogus", a1.ID); err == nil {
		t.Fatal("非法 kind 应报错")
	}
}

func TestConfirmEvent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)
	t.Cleanup(func() {
		// 清理：owner 的 event 行 + event 审计行（防跨包污染）
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_event WHERE person_id = ?", oid.Int64())
		_, _ = svc.DB.ExecContext(context.Background(), "DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'event'", oid.Int64())
	})

	// 造一条 pending 事件（低置信 LLM 路径）
	sess := ids.New()
	if _, err := svc.ApplyFacts(ctx, sess, 1, []Fact{
		{Plane: "event", Subject: Subject{Kind: "self"}, EventType: "里程碑", EventTitle: "确认事件测试-升职",
			OccurredAt: "2026-01-15", Confidence: 0.5, EpistemicType: "observed"},
	}); err != nil {
		t.Fatal(err)
	}
	evs, _ := svc.Events.ListPending(ctx, 1)
	var evID ids.ID
	for _, e := range evs {
		if e.Title == "确认事件测试-升职" {
			evID = e.ID
		}
	}
	if evID == 0 {
		t.Fatal("pending 事件未生成")
	}

	// 确认 → active + confirm 审计
	if err := svc.ConfirmPending(ctx, "event", evID); err != nil {
		t.Fatal(err)
	}
	e, _ := svc.Events.Get(ctx, evID)
	if e.Status != "active" {
		t.Fatalf("确认后应 active: %+v", e)
	}

	// 再造一条 → 放弃 → dismissed
	if _, err := svc.ApplyFacts(ctx, ids.New(), 1, []Fact{
		{Plane: "event", Subject: Subject{Kind: "self"}, EventType: "聚会", EventTitle: "确认事件测试-聚会",
			Confidence: 0.5, EpistemicType: "observed"},
	}); err != nil {
		t.Fatal(err)
	}
	evs2, _ := svc.Events.ListPending(ctx, 1)
	var evID2 ids.ID
	for _, e := range evs2 {
		if e.Title == "确认事件测试-聚会" {
			evID2 = e.ID
		}
	}
	if evID2 == 0 {
		t.Fatal("第二条 pending 事件未生成")
	}
	if err := svc.DismissPending(ctx, "event", evID2); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Events.Get(ctx, evID2); d.Status != "dismissed" {
		t.Fatalf("放弃后应 dismissed: %+v", d)
	}

	// 非 pending 再确认 → 报错；非法 kind → 报错
	if err := svc.ConfirmPending(ctx, "event", evID); err == nil {
		t.Fatal("非 pending 再确认应报错")
	}
	if err := svc.ConfirmPending(ctx, "bogus", evID); err == nil {
		t.Fatal("非法 kind 应报错")
	}
}
