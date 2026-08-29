package entity

import (
	"context"
	"testing"

	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// TestRefreshAuto 集成测试：造 person(+别名/项目属性)/pet/speaker/todo/topic 数据，
// 刷新后 entity_kb 各 kind 正确入库带拼音；重复刷新幂等；manual 条目不受刷新影响；
// 来源行清空后再刷新该 kind，对应 auto 实体消失。
func TestRefreshAuto(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const uid int64 = 1
	kb := &repo.EntityKBRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	attrs := &repo.PersonAttributeRepo{DB: db}
	pets := &repo.PersonPetRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}
	todos := &repo.TodoRepo{DB: db}
	topics := &repo.TopicRepo{DB: db}
	rels := &repo.PersonRelationshipRepo{DB: db}
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid)
		_, _ = db.Exec("DELETE FROM person_attribute")
		_, _ = db.Exec("DELETE FROM person_pet")
		_, _ = db.Exec("DELETE FROM person_relationship")
		_, _ = db.Exec("DELETE FROM person WHERE user_id = ?", uid)
		_, _ = db.Exec("DELETE FROM speaker")
		_, _ = db.Exec("DELETE FROM todo WHERE user_id = ?", uid)
		_, _ = db.Exec("DELETE FROM topic WHERE user_id = ?", uid)
	})
	_, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid)

	// 造数：person 张梦瑜（+别名「梦梦」+项目「天枢」）、宠物「阿黄」（nickname 小黄）、
	// 说话人「李工」、未关闭 todo「评审Skynet方案」、topic「周末骑行」。
	p := &repo.Person{UserID: uid, DisplayName: "张梦瑜"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := attrs.Create(ctx, &repo.PersonAttribute{PersonID: p.ID, AttrKey: "aliases", ValueText: "梦梦", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := attrs.Create(ctx, &repo.PersonAttribute{PersonID: p.ID, AttrKey: "current_projects", ValueText: "天枢", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := pets.Create(ctx, &repo.PersonPet{PersonID: p.ID, Name: "阿黄", Nickname: strPtr("小黄"), Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := speakers.Create(ctx, &repo.Speaker{Name: "李工"}); err != nil {
		t.Fatal(err)
	}
	if err := todos.InsertExt(ctx, db, []*repo.Todo{{UserID: uid, Title: "评审Skynet方案", Status: "suggested"}}); err != nil {
		t.Fatal(err)
	}
	if err := topics.Create(ctx, &repo.Topic{UserID: uid, Name: "周末骑行", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	d := SeedDeps{KB: kb, Persons: persons, Attributes: attrs, Relationships: rels,
		Pets: pets, Speakers: speakers, Todos: todos, Topics: topics}
	if err := RefreshAuto(ctx, d, uid, repo.AllEntityKinds); err != nil {
		t.Fatalf("RefreshAuto: %v", err)
	}

	list, _ := kb.ListEnabled(ctx, uid)
	got := map[string]bool{}
	for _, e := range list {
		got[e.Canonical+"/"+e.Kind] = true
		if e.Source != repo.EntitySourceAuto {
			t.Fatalf("应全为 auto: %+v", e)
		}
		if e.Pinyin == nil || *e.Pinyin == "" {
			t.Fatalf("实体应带拼音: %+v", e)
		}
	}
	for _, want := range []string{
		"张梦瑜/person", "梦梦/person", "阿黄/pet", "小黄/pet", "天枢/project",
		"评审Skynet方案/task", "周末骑行/topic", "李工/speaker",
	} {
		if !got[want] {
			t.Fatalf("缺少实体 %s，实际 %v", want, got)
		}
	}

	// 幂等：再刷一遍数量不叠加。
	if err := RefreshAuto(ctx, d, uid, repo.AllEntityKinds); err != nil {
		t.Fatalf("RefreshAuto 二次: %v", err)
	}
	list2, _ := kb.ListEnabled(ctx, uid)
	if len(list2) != len(list) {
		t.Fatalf("二次刷新数量不应变化: %d -> %d", len(list), len(list2))
	}

	// manual 条目不受刷新影响。
	m := &repo.Entity{UserID: uid, Canonical: "内部代号X", Kind: repo.EntityKindCustom, Source: repo.EntitySourceManual, Pinyin: strPtr("nei bu dai hao x")}
	if err := kb.CreateManual(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := RefreshAuto(ctx, d, uid, repo.AllEntityKinds); err != nil {
		t.Fatal(err)
	}
	if _, err := kb.Get(ctx, uid, m.ID); err != nil {
		t.Fatalf("manual 条目不应被刷新删除: %v", err)
	}

	// 来源行清空后刷新该 kind：对应 auto 实体消失（宠物全 dismissed）。
	if _, err := db.Exec("UPDATE person_pet SET status='dismissed'"); err != nil {
		t.Fatal(err)
	}
	if err := RefreshAuto(ctx, d, uid, []string{repo.EntityKindPet}); err != nil {
		t.Fatal(err)
	}
	after, _ := kb.ListEnabled(ctx, uid)
	for _, e := range after {
		if e.Kind == repo.EntityKindPet {
			t.Fatal("宠物来源清空后 auto 实体应消失")
		}
	}
}

func strPtr(s string) *string { return &s }
