package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestPersonChangeLogAppendOnly(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
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

func TestPersonChangeLogListBySession(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	logs := &PersonChangeLogRepo{DB: db}

	p := &Person{DisplayName: "session审计测试人物"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	sessA := ids.New()
	sessB := ids.New()

	// sessA 两条（attribute + pet）
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "attribute", AttrKey: strp("occupation"),
		ChangeType: "create", ChangedBy: "llm", NewValue: strp(`"工程师"`), SessionID: &sessA, Confidence: fp(0.9),
	}); err != nil {
		t.Fatal(err)
	}
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "pet",
		ChangeType: "create", ChangedBy: "llm", NewValue: strp(`"泡泡（猫）"`), SessionID: &sessA,
	}); err != nil {
		t.Fatal(err)
	}
	// sessB 一条
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "pet",
		ChangeType: "create", ChangedBy: "llm", NewValue: strp(`"豆豆（狗）"`), SessionID: &sessB,
	}); err != nil {
		t.Fatal(err)
	}
	// 一条无 session（手动改值，不应被 ListBySession 命中）
	if err := logs.Create(ctx, &PersonChangeLog{
		PersonID: p.ID, EntityKind: "pet",
		ChangeType: "update", ChangedBy: "user", NewValue: strp(`"手动改"`),
	}); err != nil {
		t.Fatal(err)
	}

	// ListBySession(sessA)：2 条，按 id 正序
	rowsA, err := logs.ListBySession(ctx, sessA)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsA) != 2 {
		t.Fatalf("sessA 应 2 条: %d", len(rowsA))
	}
	if rowsA[0].EntityKind != "attribute" || rowsA[1].EntityKind != "pet" {
		t.Fatalf("sessA 应按 id 正序: %+v", rowsA)
	}
	// ListBySession(sessB)：1 条
	rowsB, err := logs.ListBySession(ctx, sessB)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsB) != 1 || rowsB[0].NewValue == nil || *rowsB[0].NewValue != `"豆豆（狗）"` {
		t.Fatalf("sessB 应 1 条豆豆: %+v", rowsB)
	}

	// 显式守护：无 session 的手动改值行（new_value JSON 存为 `"手动改"`，含引号）绝不出现在任一 session 的结果里
	for _, r := range append(append([]PersonChangeLog{}, rowsA...), rowsB...) {
		if r.NewValue != nil && *r.NewValue == `"手动改"` {
			t.Fatalf("无 session 的手动行不应被 ListBySession 命中: %+v", r)
		}
	}
}
