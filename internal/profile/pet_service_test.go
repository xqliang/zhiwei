package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestApplyPetFacts 覆盖 pet 平面抽取落库：
// ① 新宠物高置信 → active；② 同 session 重跑 → 自然键 skip；
// ③ 跨 session 同名新信息（高置信）→ 字段合并整只替换（新行 active、旧行 superseded）；
// ④ 跨 session 同名低置信 → 冲突 pending（supersedes 指向现值，绝不静默覆盖）；
// ⑤ 跨 session 同名但字段全一致 → reaffirm 短路（不加行）。
func TestApplyPetFacts(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)

	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_pet WHERE person_id = ?`, oid.Int64())
		_, _ = svc.DB.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'pet'`, oid.Int64())
	})

	// ① 新宠物（部分字段）→ active
	sess := ids.New()
	f := Fact{Plane: "pet", Subject: Subject{Kind: "self"}, PetName: "小花", Species: "猫",
		AgeText: "3岁", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}}
	st, err := svc.ApplyFacts(ctx, sess, 1, []Fact{f})
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != 1 {
		t.Fatalf("① 新宠物应 active: %+v", st)
	}
	list, err := svc.Pets.ListByPerson(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "active" || list[0].Name != "小花" ||
		list[0].Species != "猫" || list[0].AgeText == nil || *list[0].AgeText != "3岁" {
		t.Fatalf("① 宠物行错误: %+v", list)
	}
	if list[0].Breed != nil {
		t.Fatalf("① 未提到的字段应为 NULL: %+v", list[0])
	}

	// ② 同 session 重跑 → skip
	st2, err := svc.ApplyFacts(ctx, sess, 1, []Fact{f})
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != 1 || st2.Active != 0 {
		t.Fatalf("② 同 session 重跑应 skip: %+v", st2)
	}

	// ③ 跨 session 同名补充品种（高置信）→ 合并整只替换：新行 active 含旧字段+新字段，旧行 superseded
	sess3 := ids.New()
	st3, err := svc.ApplyFacts(ctx, sess3, 1, []Fact{
		{Plane: "pet", Subject: Subject{Kind: "self"}, PetName: "小花", Breed: "布偶",
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3.Active != 1 {
		t.Fatalf("③ 高置信合并应 active: %+v", st3)
	}
	list3, err := svc.Pets.ListByPerson(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list3) != 2 {
		t.Fatalf("③ 应 2 行（旧 superseded + 新 active）: %d", len(list3))
	}
	var newAct, oldSup *repo.PersonPet
	for i := range list3 {
		if list3[i].Status == "active" {
			newAct = &list3[i]
		}
		if list3[i].Status == "superseded" {
			oldSup = &list3[i]
		}
	}
	if newAct == nil || oldSup == nil {
		t.Fatalf("③ 应一 active 一 superseded: %+v", list3)
	}
	// 合并校验：新行 breed=布偶（新信息）且 age_text=3岁（沿用旧行未提到字段）
	if newAct.Breed == nil || *newAct.Breed != "布偶" {
		t.Fatalf("③ 合并后 breed 应为布偶: %+v", newAct)
	}
	if newAct.AgeText == nil || *newAct.AgeText != "3岁" {
		t.Fatalf("③ 合并后 age_text 应沿用 3岁: %+v", newAct)
	}
	if newAct.SupersedesID == nil || *newAct.SupersedesID != oldSup.ID {
		t.Fatalf("③ 新行应 supersedes 旧行: %+v", newAct)
	}

	// ④ 跨 session 同名低置信变化 → 冲突 pending（现值不动）
	sess4 := ids.New()
	st4, err := svc.ApplyFacts(ctx, sess4, 1, []Fact{
		{Plane: "pet", Subject: Subject{Kind: "self"}, PetName: "小花", Likes: "不吃鱼",
			Confidence: 0.5, EpistemicType: "observed", SegmentIDs: []ids.ID{3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st4.Pending != 1 || st4.Conflicts != 1 {
		t.Fatalf("④ 低置信变化应冲突 pending: %+v", st4)
	}
	pend, err := svc.Pets.FindByNaturalKeyExt(ctx, svc.DB, sess4, oid, "小花")
	if err != nil {
		t.Fatal(err)
	}
	if pend == nil || pend.Status != "pending" || pend.SupersedesID == nil || *pend.SupersedesID != newAct.ID {
		t.Fatalf("④ pending 应指向现值: %+v", pend)
	}
	if pend.Likes == nil || *pend.Likes != "不吃鱼" {
		t.Fatalf("④ pending 行应含新信息: %+v", pend)
	}

	// ⑤ 跨 session 同名、字段全一致（age_text=3岁 已在现值里）→ reaffirm 短路
	sess5 := ids.New()
	st5, err := svc.ApplyFacts(ctx, sess5, 1, []Fact{
		{Plane: "pet", Subject: Subject{Kind: "self"}, PetName: "小花", AgeText: "3岁",
			Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st5.Reaffirmed != 1 || st5.Active != 0 {
		t.Fatalf("⑤ 字段一致应 reaffirm: %+v", st5)
	}
	list5, err := svc.Pets.ListByPerson(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list5) != 3 { // 不新增行：③ 的 superseded+active 两行 + ④ 的 pending 一行
		t.Fatalf("⑤ reaffirm 不应加行: %d", len(list5))
	}
}
