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
	sess3 := ids.New()
	_, _ = svc.ApplyFacts(ctx, sess3, 1, []Fact{
		{Plane: "relationship", Subject: Subject{Kind: "self"},
			Related: Subject{Kind: "mentioned", Name: "确认人物测试"}, RelationType: "朋友",
			Confidence: 0.9, EpistemicType: "observed"},
	})
	cand, _ := svc.Persons.FindByName(ctx, 1, "确认人物测试")
	if cand == nil || cand.Status != "pending" {
		t.Fatalf("应为 pending 人物: %+v", cand)
	}
	if err := svc.ConfirmPending(ctx, "person", cand.ID); err != nil {
		t.Fatal(err)
	}
	if c2, _ := svc.Persons.Get(ctx, cand.ID); c2.Status != "active" {
		t.Fatalf("人物确认后应 active: %+v", c2)
	}

	// ---- 不存在/状态非法 → 错误 ----
	if err := svc.ConfirmPending(ctx, "attribute", ids.New()); err == nil {
		t.Fatal("不存在的 id 应报错")
	}
	if err := svc.ConfirmPending(ctx, "bogus", a1.ID); err == nil {
		t.Fatal("非法 kind 应报错")
	}
}
