package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestPersonAttributeQueries(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	attrs := &PersonAttributeRepo{DB: db}

	p := &Person{DisplayName: "属性测试人物"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	sess := ids.New()

	// 单值 key：两条不同值（模拟 active + 冲突 pending）
	a1 := &PersonAttribute{PersonID: p.ID, AttrKey: "city", ValueText: "北京", Status: "active", SessionID: &sess}
	if err := attrs.Create(ctx, a1); err != nil {
		t.Fatal(err)
	}
	a2 := &PersonAttribute{PersonID: p.ID, AttrKey: "city", ValueText: "上海", Status: "pending", SessionID: &sess, SupersedesID: &a1.ID}
	if err := attrs.Create(ctx, a2); err != nil {
		t.Fatal(err)
	}
	// 列表 key：两个元素
	a3 := &PersonAttribute{PersonID: p.ID, AttrKey: "hobbies", ValueText: "游泳", Status: "active", SessionID: &sess}
	if err := attrs.Create(ctx, a3); err != nil {
		t.Fatal(err)
	}

	// FindActiveByKey：单值当前值 = a1
	got, err := attrs.FindActiveByKey(ctx, p.ID, "city")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != a1.ID {
		t.Fatalf("FindActiveByKey 错误: %+v", got)
	}
	// FindActiveByKeyValue：列表同值命中 / 未命中
	g2, err := attrs.FindActiveByKeyValue(ctx, p.ID, "hobbies", "游泳")
	if err != nil {
		t.Fatal(err)
	}
	if g2 == nil || g2.ID != a3.ID {
		t.Fatalf("FindActiveByKeyValue 未命中: %+v", g2)
	}
	g3, err := attrs.FindActiveByKeyValue(ctx, p.ID, "hobbies", "篮球")
	if err != nil {
		t.Fatal(err)
	}
	if g3 != nil {
		t.Fatalf("FindActiveByKeyValue 不应命中: %+v", g3)
	}
	// FindByNaturalKey：同 session 同 key 同值（任意 status）命中
	g4, err := attrs.FindByNaturalKey(ctx, sess, p.ID, "city", "上海")
	if err != nil {
		t.Fatal(err)
	}
	if g4 == nil || g4.ID != a2.ID {
		t.Fatalf("FindByNaturalKey 未命中: %+v", g4)
	}
	// ListByPerson：3 行全在
	rows, err := attrs.ListByPerson(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("ListByPerson 应 3 行: %d", len(rows))
	}
	// ListPending：仅 a2
	pend, err := attrs.ListPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range pend {
		if r.ID == a2.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("ListPending 未包含 pending 行")
	}
	// SetStatus + BumpConfidence（封顶 0.99）
	if err := attrs.SetStatus(ctx, a1.ID, "superseded"); err != nil {
		t.Fatal(err)
	}
	if err := attrs.BumpConfidence(ctx, a1.ID, 0.05); err != nil {
		t.Fatal(err)
	}
	// Get 校验
	g5, err := attrs.Get(ctx, a1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g5.Status != "superseded" || g5.Confidence <= 0.8 {
		t.Fatalf("SetStatus/BumpConfidence 未生效: %+v", g5)
	}
}
