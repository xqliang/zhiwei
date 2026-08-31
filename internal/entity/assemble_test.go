package entity

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// ---- 纯函数单测（无 DB）----

// TestIsPlaceholderName 占位名判定：自动说话人名「说话人+5位」+ 明显占位名（未知/未知同事/未命名）。
// 对所有 kind 生效（修 person 线漏网）；真名不得误伤。
func TestIsPlaceholderName(t *testing.T) {
	hit := []string{"说话人abcde", "说话人1trdl", "说话人7xv6w", "未知", "未知同事", "未命名"}
	for _, n := range hit {
		if !isPlaceholderName(n) {
			t.Errorf("应判为占位名: %q", n)
		}
	}
	miss := []string{"张梦瑜", "李工", "Allen", "Simin", "胡志涛",
		"说话人1", "说话人abcdef", "说话人", "未知的人", "同事"}
	for _, n := range miss {
		if isPlaceholderName(n) {
			t.Errorf("不应判为占位名: %q", n)
		}
	}
}

// TestDedupeAcrossKinds 跨 kind 去重：同名 person 优先于 speaker（与「同名只留一条」语义一致）；
// 不同名各自保留。
func TestDedupeAcrossKinds(t *testing.T) {
	// person 与 speaker 同名 → 只留 person（输入顺序无关）。
	in := []repo.Entity{
		{Canonical: "Allen", Kind: repo.EntityKindSpeaker},
		{Canonical: "Allen", Kind: repo.EntityKindPerson},
	}
	out := dedupeAcrossKinds(in)
	if len(out) != 1 || out[0].Kind != repo.EntityKindPerson {
		t.Fatalf("同名应只留 person: %+v", out)
	}
	// 不同名 → 全保留。
	in2 := []repo.Entity{
		{Canonical: "张梦瑜", Kind: repo.EntityKindPerson},
		{Canonical: "李工", Kind: repo.EntityKindSpeaker},
		{Canonical: "阿黄", Kind: repo.EntityKindPet},
	}
	if out2 := dedupeAcrossKinds(in2); len(out2) != 3 {
		t.Fatalf("不同名应全保留: %+v", out2)
	}
	// 同 kind 内同名也去重。
	in3 := []repo.Entity{
		{Canonical: "梦梦", Kind: repo.EntityKindPerson},
		{Canonical: "梦梦", Kind: repo.EntityKindPerson},
	}
	if out3 := dedupeAcrossKinds(in3); len(out3) != 1 {
		t.Fatalf("同 kind 同名应去重: %+v", out3)
	}
}

// TestMergeWhitelist 纠错白名单合并：auto − disabled + manual（manual 同名覆盖 auto，保真实 id）。
func TestMergeWhitelist(t *testing.T) {
	person := func(c string, id int64) repo.Entity {
		return repo.Entity{ID: ids.ID(id), Canonical: c, Kind: repo.EntityKindPerson}
	}
	// 1) 纯 auto，无禁用 → 原样。
	auto := []repo.Entity{person("张梦瑜", 0), {Canonical: "李工", Kind: repo.EntityKindSpeaker}}
	if got := MergeWhitelist(auto, nil, nil); len(got) != 2 {
		t.Fatalf("纯 auto 应原样: %+v", got)
	}
	// 2) disabled 过滤 auto（大小写不敏感）。
	if got := MergeWhitelist(auto, nil, map[string]bool{"张梦瑜": true}); len(got) != 1 || got[0].Canonical != "李工" {
		t.Fatalf("disabled 应过滤 auto: %+v", got)
	}
	// 大小写不敏感(拉丁名):disabled 用小写键也能禁掉大写 canonical。
	autoLatin := []repo.Entity{{Canonical: "Skynet", Kind: repo.EntityKindProject}}
	if got := MergeWhitelist(autoLatin, nil, map[string]bool{"skynet": true}); len(got) != 0 {
		t.Fatalf("disabled 应大小写不敏感: %+v", got)
	}
	// 3) manual 同名覆盖 auto（保真实 id）。
	manual := []repo.Entity{person("张梦瑜", 5)}
	got := MergeWhitelist(auto, manual, nil)
	if len(got) != 2 {
		t.Fatalf("manual+auto 应去重为 2: %+v", got)
	}
	for _, e := range got {
		if e.Canonical == "张梦瑜" && e.ID != ids.ID(5) {
			t.Fatalf("manual 应覆盖 auto 且保留 id=5: %+v", e)
		}
	}
	// 4) manual 独有 → 保留。
	got4 := MergeWhitelist(auto, []repo.Entity{person("内部代号X", 9)}, nil)
	if len(got4) != 3 {
		t.Fatalf("manual 独有应保留: %+v", got4)
	}
}

// ---- 集成测试：AssembleEntities 实时聚合 ----

// TestAssembleEntities 造 person(同名 speaker)/占位名 speaker/todo/topic 数据,
// 实时聚合应:person 优先去重、占位名过滤、不收集 task、不落库、带拼音。
func TestAssembleEntities(t *testing.T) {
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
		_, _ = db.Exec("DELETE FROM entity_disabled")
		_, _ = db.Exec("DELETE FROM person_attribute")
		_, _ = db.Exec("DELETE FROM person_pet")
		_, _ = db.Exec("DELETE FROM person WHERE user_id = ?", uid)
		_, _ = db.Exec("DELETE FROM speaker")
		_, _ = db.Exec("DELETE FROM todo WHERE user_id = ?", uid)
		_, _ = db.Exec("DELETE FROM topic WHERE user_id = ?", uid)
	})

	// person 张梦瑜；说话人同时有「张梦瑜」(同名→应被 person 去重)、「说话人1trdl」(占位名→滤掉)、
	// 「李工」(保留)；todo「评审Skynet方案」(不应收集)；topic「周末骑行」。
	p := &repo.Person{UserID: uid, DisplayName: "张梦瑜"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"张梦瑜", "说话人1trdl", "李工"} {
		if err := speakers.Create(ctx, &repo.Speaker{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	if err := todos.InsertExt(ctx, db, []*repo.Todo{{UserID: uid, Title: "评审Skynet方案", Status: "suggested"}}); err != nil {
		t.Fatal(err)
	}
	if err := topics.Create(ctx, &repo.Topic{UserID: uid, Name: "周末骑行", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	d := SeedDeps{KB: kb, Persons: persons, Attributes: attrs, Relationships: rels,
		Pets: pets, Speakers: speakers, Todos: todos, Topics: topics}
	list, err := AssembleEntities(ctx, d, uid, repo.AllEntityKinds)
	if err != nil {
		t.Fatalf("AssembleEntities: %v", err)
	}
	got := map[string]string{} // canonical → kind
	for _, e := range list {
		if e.Source != repo.EntitySourceAuto {
			t.Fatalf("应全为 auto: %+v", e)
		}
		if e.Pinyin == nil || *e.Pinyin == "" {
			t.Fatalf("实体应带拼音: %+v", e)
		}
		got[e.Canonical] = e.Kind
	}
	// person 张梦瑜 存在(person 优先,不因 speaker 同名而重复/丢失)
	if got["张梦瑜"] != repo.EntityKindPerson {
		t.Fatalf("张梦瑜 应为 person,实际 kind=%q 全量=%v", got["张梦瑜"], got)
	}
	// 说话人李工 保留
	if got["李工"] != repo.EntityKindSpeaker {
		t.Fatalf("李工 应为 speaker,实际=%v", got)
	}
	// 占位名 说话人1trdl 被过滤
	if _, ok := got["说话人1trdl"]; ok {
		t.Fatalf("占位名不应出现: %v", got)
	}
	// task 不收集
	if _, ok := got["评审Skynet方案"]; ok {
		t.Fatalf("待办不应进实体: %v", got)
	}
	// topic 保留
	if got["周末骑行"] != repo.EntityKindTopic {
		t.Fatalf("周末骑行 应为 topic,实际=%v", got)
	}
	// 张梦瑜 只出现一次(person+speaker 同名去重)
	cnt := 0
	for c := range got {
		if c == "张梦瑜" {
			cnt++
		}
	}
	if cnt != 1 {
		t.Fatalf("张梦瑜 应只出现一次,实际 %d 次: %v", cnt, got)
	}

	// 不落库:entity_kb 不应有 auto 行。
	var n int
	if err := db.GetContext(ctx, &n, `SELECT COUNT(*) FROM entity_kb WHERE user_id=? AND source='auto'`, uid); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("AssembleEntities 不应落库,实际 %d 行 auto", n)
	}
}
