package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestPersonChangeLogAppendOnly(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	logs := &PersonChangeLogRepo{DB: db}

	p := &Person{DisplayName: "审计测试人物"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	attrID := ids.New()
	sess := ids.New()
	oldV := `"教师"`
	newV := `"医生"`

	// create + update 两条审计
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "attribute", EntityID: &attrID, AttrKey: strp("occupation"),
		ChangeType: "create", ChangedBy: "llm", NewValue: strp(oldV), SessionID: &sess,
		Confidence: fp(0.9),
	}); err != nil {
		t.Fatal(err)
	}
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "attribute", EntityID: &attrID, AttrKey: strp("occupation"),
		ChangeType: "update", ChangedBy: "user", OldValue: strp(oldV), NewValue: strp(newV),
	}); err != nil {
		t.Fatal(err)
	}

	// ListByPerson：2 条，按时间正序
	rows, err := logs.ListByPerson(ctx, p.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应 2 条审计: %d", len(rows))
	}
	if rows[0].ChangeType != "create" || rows[0].ChangedBy != "llm" {
		t.Fatalf("第一条审计错误: %+v", rows[0])
	}
	// entity_kind 过滤
	only, err := logs.ListByPerson(ctx, p.ID, "attribute", "occupation")
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 2 {
		t.Fatalf("attr_key 过滤应 2 条: %d", len(only))
	}
	none, err := logs.ListByPerson(ctx, p.ID, "attribute", "city")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("city 过滤应 0 条: %d", len(none))
	}
}

// fp float64 取址小工具（测试专用；strp 已在 person_relationship_test.go 定义，复用）。
func fp(f float64) *float64 { return &f }
