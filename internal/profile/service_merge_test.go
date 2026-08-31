// service_merge_test 「作为别名并入」人物合并（2026-08-31 需求）：
// 源名字成为目标别名、八平面数据全量转移、关系双向改指+自环清理、
// speaker 绑定移动、源标 merged、审计留痕。
package profile

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// mkPerson 建一个 active 人物（名册卡目标/源通用于例内）。
func mkPerson(t *testing.T, svc *Service, name string) *repo.Person {
	t.Helper()
	p := &repo.Person{UserID: 1, DisplayName: name, Source: "manual"}
	if err := svc.Persons.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManualMergeAsAlias(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	src := mkPerson(t, svc, "老保") // LLM 按别名误建的 pending 人物
	tgt := mkPerson(t, svc, "解保功")

	// 源名下数据：属性（pending）+ 关系（与 owner）+ 记忆归属
	_ = svc.Attributes.Create(ctx, &repo.PersonAttribute{UserID: 1, PersonID: src.ID, AttrKey: "occupation", ValueText: "电工", Source: "llm", Status: "pending"})
	owner, _ := svc.Persons.GetOwner(ctx, 1)
	_ = svc.Relationships.Create(ctx, &repo.PersonRelationship{UserID: 1, PersonID: src.ID, RelatedPersonID: &owner.ID, RelationType: "朋友", Source: "llm", Status: "pending"})
	mem := &repo.Memory{UserID: 1, Type: "fact", Title: "老保是电工", Content: "老保 提到他是电工", Status: "active", PersonID: &src.ID}
	_ = svc.Memories.InsertExt(ctx, svc.DB, []*repo.Memory{mem})

	// 目标已有一个 active 别名（验证不重复）；再给目标绑声纹（验证源无绑定不影响）
	_ = svc.Attributes.Create(ctx, &repo.PersonAttribute{UserID: 1, PersonID: tgt.ID, AttrKey: "aliases", ValueText: "保叔", Source: "manual", Status: "active"})

	if err := svc.ManualMergeAsAlias(ctx, 1, src.ID, tgt.ID); err != nil {
		t.Fatalf("合并失败: %v", err)
	}

	// 源标 merged；名册/队列均不再含它
	got, _ := svc.Persons.Get(ctx, 1, src.ID)
	if got == nil || got.Status != "merged" {
		t.Fatalf("源应标 merged，实际 %+v", got)
	}

	// 属性转移（pending 状态不动 → 进目标确认队列）
	attrs, _ := svc.Attributes.ListByPerson(ctx, tgt.ID)
	var occ *repo.PersonAttribute
	for i := range attrs {
		if attrs[i].AttrKey == "occupation" {
			occ = &attrs[i]
		}
	}
	if occ == nil || occ.ValueText != "电工" || occ.Status != "pending" {
		t.Fatalf("源的 pending 属性应原样转移到目标，实际 %+v", occ)
	}
	// 别名行：原名「老保」成为目标 active 别名；已有「保叔」不受影响
	aliasVals := map[string]bool{}
	for _, a := range attrs {
		if a.AttrKey == "aliases" && a.Status == "active" {
			aliasVals[a.ValueText] = true
		}
	}
	if !aliasVals["老保"] || !aliasVals["保叔"] {
		t.Fatalf("目标应有别名 老保+保叔，实际 %+v", aliasVals)
	}

	// 关系转移：源与 owner 的关系 → 目标与 owner
	rels, _ := svc.Relationships.ListByPerson(ctx, tgt.ID)
	found := false
	for _, r := range rels {
		if r.RelatedPersonID != nil && *r.RelatedPersonID == owner.ID && r.RelationType == "朋友" {
			found = true
		}
	}
	if !found {
		t.Fatalf("源的关系应转移到目标，实际 %+v", rels)
	}

	// 记忆归属转移
	rows, _ := svc.Memories.ListByPerson(ctx, tgt.ID, 10)
	if len(rows) != 1 || rows[0].Title != "老保是电工" {
		t.Fatalf("源的记忆归属应转移到目标，实际 %+v", rows)
	}

	// 双向审计
	tLogs, _ := svc.ChangeLogs.ListByPerson(ctx, tgt.ID, "person", "")
	sLogs, _ := svc.ChangeLogs.ListByPerson(ctx, src.ID, "person", "")
	if len(tLogs) == 0 || len(sLogs) == 0 {
		t.Fatalf("应双向留审计：目标 %d 条 / 源 %d 条", len(tLogs), len(sLogs))
	}
}

func TestManualMergeAsAliasSelfLoopAndSpeaker(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	src := mkPerson(t, svc, "甲")
	tgt := mkPerson(t, svc, "乙")
	third := mkPerson(t, svc, "丙")

	// 关系自环素材：源和目标都与丙有朋友关系（转移后两条都指向丙，非自环）；另造一条
	// 「源 的对端是 源自己」不会被造出，但「目标 的对端是 源」转移后变自环 → 应删除。
	_ = svc.Relationships.Create(ctx, &repo.PersonRelationship{UserID: 1, PersonID: src.ID, RelatedPersonID: &third.ID, RelationType: "朋友", Source: "manual", Status: "active"})
	_ = svc.Relationships.Create(ctx, &repo.PersonRelationship{UserID: 1, PersonID: tgt.ID, RelatedPersonID: &src.ID, RelationType: "同事", Source: "manual", Status: "active"})

	// 源绑声纹（目标无）→ 绑定应移给目标并同步声纹名
	sp := &repo.Speaker{UserID: 1, Name: "甲", Source: "enrolled"}
	_ = svc.Speakers.Create(ctx, sp)
	if _, err := svc.DB.ExecContext(ctx, `UPDATE person SET speaker_id = ? WHERE id = ?`, sp.ID.Int64(), src.ID); err != nil {
		t.Fatal(err)
	}

	if err := svc.ManualMergeAsAlias(ctx, 1, src.ID, tgt.ID); err != nil {
		t.Fatalf("合并失败: %v", err)
	}

	// 自环清理：「乙 与 甲(→乙) 同事」变自环 → 删除；「乙 与 丙 朋友」保留
	rels, _ := svc.Relationships.ListByPerson(ctx, tgt.ID)
	for _, r := range rels {
		if r.RelatedPersonID != nil && *r.RelatedPersonID == tgt.ID {
			t.Fatalf("自环关系应删除，实际 %+v", r)
		}
	}
	hasThird := false
	for _, r := range rels {
		if r.RelatedPersonID != nil && *r.RelatedPersonID == third.ID {
			hasThird = true
		}
	}
	if !hasThird {
		t.Fatalf("源与丙的关系应转移到目标，实际 %+v", rels)
	}

	// 绑定移动 + 声纹名同步（speaker.name = 目标 display_name）
	gotTgt, _ := svc.Persons.Get(ctx, 1, tgt.ID)
	if gotTgt.SpeakerID == nil || *gotTgt.SpeakerID != sp.ID {
		t.Fatalf("声纹绑定应移给目标，实际 %+v", gotTgt.SpeakerID)
	}
	gotSp, _ := svc.Speakers.Get(ctx, sp.ID)
	if gotSp.Name != "乙" {
		t.Fatalf("声纹名应同步为目标名「乙」，实际 %q", gotSp.Name)
	}
	_ = svc.Speakers.Delete(ctx, sp.ID)
}

func TestManualMergeAsAliasErrors(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	owner, _ := svc.Persons.GetOwner(ctx, 1)
	a := mkPerson(t, svc, "A")
	b := mkPerson(t, svc, "B")

	if err := svc.ManualMergeAsAlias(ctx, 1, a.ID, a.ID); err != ErrSamePerson {
		t.Fatalf("同 id 应 ErrSamePerson，实际 %v", err)
	}
	if err := svc.ManualMergeAsAlias(ctx, 1, owner.ID, a.ID); err != ErrOwnerUnmergeable {
		t.Fatalf("owner 被并入应 ErrOwnerUnmergeable，实际 %v", err)
	}
	if err := svc.ManualMergeAsAlias(ctx, 1, a.ID, ids.New()); err != ErrNotFound {
		t.Fatalf("目标不存在应 ErrNotFound，实际 %v", err)
	}
	// 并入已 merged 的目标 → ErrBadMergeTarget
	_ = svc.Persons.SetStatus(ctx, b.ID, "merged")
	if err := svc.ManualMergeAsAlias(ctx, 1, a.ID, b.ID); err != ErrBadMergeTarget {
		t.Fatalf("merged 目标应 ErrBadMergeTarget，实际 %v", err)
	}
	_ = a
}
