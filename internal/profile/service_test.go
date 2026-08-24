package profile

import (
	"context"
	"os"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// fakeLLM 已在 extractor_test.go 定义（同包共享），此处不重复定义。

// TestMain 统一初始化雪花 ID 节点：本包此前的测试（extractor/gate/fact/catalog）
// 都不生成主键，故无需 ids.Init；service 测试要落库（EnsurePersonBootstrap、
// ids.New() 造 session），必须先设好 snowflake 节点，否则 node 为 nil 会 panic。
// TestMain 必须定义在 _test.go 里才会被 test 框架调用（一个包仅一个）。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}

// newTestService 建好 Service 并跑 bootstrap（owner「我」必备）。
// Memories/Speakers 必须给：ApplyFacts 读 session memories，speaker 归属解析查名册。
func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		DB: db, Persons: &repo.PersonRepo{DB: db},
		Memories:      &repo.MemoryRepo{DB: db},
		Speakers:      &repo.SpeakerRepo{DB: db},
		Attributes:    &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db},
		ChangeLogs:    &repo.PersonChangeLogRepo{DB: db},
		Gate:          GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(context.Background(), svc.Persons, &repo.SpeakerRepo{DB: db}); err != nil {
		t.Fatal(err)
	}
	return svc
}

func ownerID(t *testing.T, svc *Service) ids.ID {
	t.Helper()
	o, err := svc.Persons.GetOwner(context.Background(), 1)
	if err != nil || o == nil {
		t.Fatalf("owner 缺失: %v %v", o, err)
	}
	return o.ID
}

func TestApplyFactsGatePaths(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	oid := ownerID(t, svc)
	sess := ids.New()

	facts := []Fact{
		// ① 无现值高置信 observed → active
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "工程师", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ② 无现值低置信 → pending
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "personality",
			Value: "内向", Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ③ 列表低置信 → pending
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "hobbies",
			Value: "游泳", Confidence: 0.6, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ④ 关系：mentioned:Alice 高置信 → active + 自动建 pending 人物 Alice
		{Plane: "relationship", Subject: Subject{Kind: "self"},
			Related: Subject{Kind: "mentioned", Name: "Alice"}, RelationType: "配偶",
			Label: "老婆", Confidence: 0.9, EpistemicType: "observed", SegmentIDs: []ids.ID{1}},
		// ⑤ 关系指代 subject：属性挂到 owner 的配偶（=上一步新建的 Alice）身上
		{Plane: "attribute", Subject: Subject{Kind: "relation", Relation: "配偶"},
			AttrKey: "occupation", Value: "医生", Confidence: 0.9, EpistemicType: "observed",
			SegmentIDs: []ids.ID{1}},
	}
	st, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	// ①④⑤ active；②③ pending
	if st.Active != 3 || st.Pending != 2 || st.Skipped != 0 {
		t.Fatalf("统计错误: %+v", st)
	}

	// 校验：occupation=工程师 active；personality/hobbies pending
	oa, _ := svc.Attributes.FindActiveByKey(ctx, oid, "occupation")
	if oa == nil || oa.ValueText != "工程师" || oa.Source != "llm" || oa.SessionID == nil || *oa.SessionID != sess {
		t.Fatalf("occupation active 行错误: %+v", oa)
	}
	pa, _ := svc.Attributes.FindActiveByKey(ctx, oid, "personality")
	if pa != nil {
		t.Fatalf("低置信不应 active: %+v", pa)
	}
	// Alice：pending 人物 + 配偶关系 active + occupation active
	alice, _ := svc.Persons.FindByName(ctx, 1, "Alice")
	if alice == nil || alice.Status != "pending" || alice.Source != "llm" {
		t.Fatalf("Alice 人物错误: %+v", alice)
	}
	rel, err := svc.Relationships.FindActiveByTypeExt(ctx, svc.DB, oid, "配偶", &alice.ID)
	if err != nil || rel == nil {
		t.Fatalf("配偶关系未建立: %v %v", rel, err)
	}
	ao, _ := svc.Attributes.FindActiveByKey(ctx, alice.ID, "occupation")
	if ao == nil || ao.ValueText != "医生" {
		t.Fatalf("Alice 职业错误: %+v", ao)
	}
	// 审计：owner 侧至少 create(attribute×3) 条目 + person create(Alice) + relationship create
	logs, _ := svc.ChangeLogs.ListByPerson(ctx, oid, "attribute", "")
	if len(logs) < 3 {
		t.Fatalf("owner 属性审计不足: %d", len(logs))
	}

	// 幂等：同 session 重跑全部 skip
	st2, err := svc.ApplyFacts(ctx, sess, 1, facts)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Skipped != len(facts) || st2.Active != 0 || st2.Pending != 0 || st2.Reaffirmed != 0 {
		t.Fatalf("重跑应全部 skip: %+v", st2)
	}
	// Alice 不被重复创建
	if a2, _ := svc.Persons.FindByName(ctx, 1, "Alice"); a2.ID != alice.ID {
		t.Fatal("Alice 被重复创建")
	}

	// 冲突：另一 session 说 occupation=教师（高置信）→ pending + supersedes 指向现值
	sess2 := ids.New()
	st3, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "教师", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st3.Pending != 1 || st3.Conflicts != 1 {
		t.Fatalf("冲突统计错误: %+v", st3)
	}
	// 佐证：sess2 重申 occupation=工程师（active 现值）→ reaffirm
	st4, err := svc.ApplyFacts(ctx, sess2, 1, []Fact{
		{Plane: "attribute", Subject: Subject{Kind: "self"}, AttrKey: "occupation",
			Value: "工程师", Confidence: 0.9, EpistemicType: "observed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st4.Reaffirmed != 1 {
		t.Fatalf("同值重申应 reaffirm: %+v", st4)
	}
}
