package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestPersonRelationshipQueries(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	rels := &PersonRelationshipRepo{DB: db}

	a := &Person{DisplayName: "关系测试-甲"}
	b := &Person{DisplayName: "关系测试-乙"}
	if err := persons.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := persons.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	sess := ids.New()

	// owner(甲) → 乙 的配偶关系（active）
	r1 := &PersonRelationship{PersonID: a.ID, RelatedPersonID: &b.ID, RelationType: "配偶", Status: "active", SessionID: &sess}
	if err := rels.Create(ctx, r1); err != nil {
		t.Fatal(err)
	}
	// 组织关系：无对端人物，只有 org_name
	r2 := &PersonRelationship{PersonID: a.ID, RelationType: "组织", OrgName: strp("校友会"), Status: "active", SessionID: &sess}
	if err := rels.Create(ctx, r2); err != nil {
		t.Fatal(err)
	}

	// FindActiveByTypeExt：按类型+对端命中
	got, err := rels.FindActiveByTypeExt(ctx, db, a.ID, "配偶", &b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != r1.ID {
		t.Fatalf("FindActiveByTypeExt 未命中: %+v", got)
	}
	// 组织类型、对端为 nil 的命中
	got2, err := rels.FindActiveByTypeExt(ctx, db, a.ID, "组织", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got2 == nil || got2.ID != r2.ID {
		t.Fatalf("FindActiveByTypeExt(组织,nil) 未命中: %+v", got2)
	}
	// 自然键去重：同 session 同三元组（任意 status）命中
	got3, err := rels.FindByNaturalKeyExt(ctx, db, sess, a.ID, "配偶", &b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got3 == nil || got3.ID != r1.ID {
		t.Fatalf("FindByNaturalKeyExt 未命中: %+v", got3)
	}
	// ListByPerson / ListPending / SetStatus
	rows, err := rels.ListByPerson(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListByPerson 应 2 行: %d", len(rows))
	}
	r3 := &PersonRelationship{PersonID: a.ID, RelatedPersonID: &b.ID, RelationType: "同事", Status: "pending", SessionID: &sess}
	if err := rels.Create(ctx, r3); err != nil {
		t.Fatal(err)
	}
	pend, err := rels.ListPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range pend {
		if r.ID == r3.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("ListPending 未包含 pending 关系")
	}
	if err := rels.SetStatus(ctx, r3.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if g4, _ := rels.Get(ctx, r3.ID); g4.Status != "dismissed" {
		t.Fatalf("SetStatus 未生效: %+v", g4)
	}
}

// strp 字符串取址小工具（测试专用）。
func strp(s string) *string { return &s }
