package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestPersonLifecycle(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	speakers := &SpeakerRepo{DB: db}

	// bootstrap：owner 回填 + speaker→person 回填，且幂等
	sp := &Speaker{Name: "回填测试说话人"}
	if err := speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePersonBootstrap(ctx, persons, speakers); err != nil {
		t.Fatal(err)
	}
	owner, err := persons.GetOwner(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || !owner.IsOwner || owner.DisplayName != "我" {
		t.Fatalf("owner 未回填: %+v", owner)
	}
	if err := EnsurePersonBootstrap(ctx, persons, speakers); err != nil {
		t.Fatal(err)
	}
	if o2, _ := persons.GetOwner(ctx, 1); o2 == nil || o2.ID != owner.ID {
		t.Fatal("bootstrap 不幂等：owner 被重复创建")
	}
	linked, err := persons.GetBySpeaker(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linked == nil || linked.DisplayName != sp.Name {
		t.Fatalf("speaker 未回填为 person: %+v", linked)
	}

	// 新建 + 按名查找 + 更新 + 状态
	p := &Person{DisplayName: "张三"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 || p.Status != "active" || p.Source != "manual" {
		t.Fatalf("Create 默认值未兜底: %+v", p)
	}
	found, err := persons.FindByName(ctx, 1, "张三")
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || found.ID != p.ID {
		t.Fatalf("FindByName 未命中: %+v", found)
	}
	// 换绑声纹：绑到一个未被 bootstrap 占用的新声纹。
	// sp 已在 bootstrap 阶段绑给了 linked 人物，而 person.uk_speaker 要求
	// 一个声纹至多绑一个人；若这里仍绑 sp 会触发唯一键冲突，换绑须换到空闲声纹。
	sp2 := &Speaker{Name: "换绑测试声纹"}
	if err := speakers.Create(ctx, sp2); err != nil {
		t.Fatal(err)
	}
	sid := sp2.ID
	if err := persons.Update(ctx, p.ID, "张三丰", &sid, nil); err != nil {
		t.Fatal(err)
	}
	got, err := persons.Get(ctx, 1, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "张三丰" || got.SpeakerID == nil || *got.SpeakerID != sp2.ID {
		t.Fatalf("Update 未生效: %+v", got)
	}
	if err := persons.SetStatus(ctx, p.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if g2, _ := persons.Get(ctx, 1, p.ID); g2.Status != "dismissed" {
		t.Fatalf("SetStatus 未生效: %+v", g2)
	}
}

// TestEnsureOwnerForUser 验证为指定 user 幂等引导 owner「我」：
// 首次创建（is_owner=1、DisplayName="我"、UserID 落在该域），二次为 no-op（仍是同一 owner）。
func TestEnsureOwnerForUser(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}

	// 独占一个大 user_id，避开 user_id=1 存量域；用例结束清理该域全部 person 行。
	const uid int64 = 900000000000000001
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM person WHERE user_id = ?`, uid)
	})

	// 首次：无 owner → 创建。
	if err := EnsureOwnerForUser(ctx, persons, uid); err != nil {
		t.Fatal(err)
	}
	owner, err := persons.GetOwner(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || !owner.IsOwner || owner.DisplayName != "我" || owner.UserID != uid {
		t.Fatalf("owner 未按预期创建: %+v", owner)
	}

	// 二次：幂等 no-op，仍命中同一 owner（未重复创建）。
	if err := EnsureOwnerForUser(ctx, persons, uid); err != nil {
		t.Fatal(err)
	}
	owner2, err := persons.GetOwner(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if owner2 == nil || owner2.ID != owner.ID {
		t.Fatalf("幂等失败：owner 被重复创建 first=%d second=%+v", owner.ID, owner2)
	}
}

func TestPersonListWithPendingAndRecentSessions(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	attrs := &PersonAttributeRepo{DB: db}

	p := &Person{DisplayName: "计数测试人物"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	sess := ids.New()
	// 一条 active、一条 pending：pending 计数应为 1
	if err := attrs.Create(ctx, &PersonAttribute{PersonID: p.ID, AttrKey: "city", ValueText: "上海", Status: "active", SessionID: &sess}); err != nil {
		t.Fatal(err)
	}
	if err := attrs.Create(ctx, &PersonAttribute{PersonID: p.ID, AttrKey: "occupation", ValueText: "医生", Status: "pending", SessionID: &sess}); err != nil {
		t.Fatal(err)
	}

	list, err := persons.ListWithPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var hit *PersonWithPending
	for i := range list {
		if list[i].ID == p.ID {
			hit = &list[i]
		}
	}
	if hit == nil || hit.PendingCount != 1 {
		t.Fatalf("ListWithPending 计数错误: %+v", hit)
	}

	sids, err := persons.RecentSessionIDs(ctx, p.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sids) != 1 || sids[0] != sess {
		t.Fatalf("RecentSessionIDs 错误: %v", sids)
	}
}

// TestPersonRosterExcludesPending 锁定 spec「避免抽取噪声污染名册」（person-profile spec §86）：
// 名册查询（ListWithPending）只返回 active 人物——LLM 抽取新建的 pending 人物只在确认
// 队列出现，确认（pending→active）后才进名册。List（确认队列的人名映射用）不受此约束。
func TestPersonRosterExcludesPending(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM person WHERE display_name LIKE '名册过滤测试%'") })
	ctx := context.Background()
	persons := &PersonRepo{DB: db}

	active := &Person{DisplayName: "名册过滤测试已确认"}
	if err := persons.Create(ctx, active); err != nil {
		t.Fatal(err)
	}
	pending := &Person{DisplayName: "名册过滤测试待确认", Source: "llm", Status: "pending"}
	if err := persons.Create(ctx, pending); err != nil {
		t.Fatal(err)
	}

	// 名册：只有 active 的
	roster, err := persons.ListWithPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var sawActive, sawPending bool
	for _, p := range roster {
		if p.ID == active.ID {
			sawActive = true
		}
		if p.ID == pending.ID {
			sawPending = true
		}
	}
	if !sawActive || sawPending {
		t.Errorf("名册应含 active、不含 pending: sawActive=%v sawPending=%v", sawActive, sawPending)
	}

	// 确认队列人名映射（List）：pending 仍在（要给待确认的人物条目显示名字）
	all, err := persons.List(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var sawPendingInList bool
	for _, p := range all {
		if p.ID == pending.ID {
			sawPendingInList = true
		}
	}
	if !sawPendingInList {
		t.Error("List 应仍含 pending（确认队列人名映射用）")
	}
}

// TestFindByNameOrAliasExt 别名感知解析（2026-08-31）：display_name 命中直返；
// 否则 active aliases 行唯一 owner 才命中——歧义（「亮哥」两人共有）与 pending 别名不猜不算。
func TestFindByNameOrAliasExt(t *testing.T) {
	_ = ids.InitForTest()
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	attrs := &PersonAttributeRepo{DB: db}

	jbg := &Person{UserID: 1, DisplayName: "解保功", Source: "manual"}
	_ = persons.Create(ctx, jbg)
	zl := &Person{UserID: 1, DisplayName: "赵亮", Source: "manual"}
	_ = persons.Create(ctx, zl)
	ql := &Person{UserID: 1, DisplayName: "清亮", Source: "manual"}
	_ = persons.Create(ctx, ql)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM person_attribute WHERE person_id IN (?, ?, ?)`, jbg.ID.Int64(), zl.ID.Int64(), ql.ID.Int64())
		_, _ = db.ExecContext(ctx, `DELETE FROM person WHERE id IN (?, ?, ?)`, jbg.ID.Int64(), zl.ID.Int64(), ql.ID.Int64())
	})
	mkAlias := func(p *Person, val, status string) {
		_ = attrs.Create(ctx, &PersonAttribute{UserID: 1, PersonID: p.ID, AttrKey: "aliases", ValueText: val, Source: "manual", Status: status})
	}
	mkAlias(jbg, "老保", "active")
	mkAlias(zl, "亮哥", "active")
	mkAlias(ql, "亮哥", "active") // 歧义：同一别名两人共有
	mkAlias(ql, "阿亮", "pending") // pending 别名不算数

	cases := []struct {
		name, q string
		want    *ids.ID // 期望命中的 person id（nil=不命中）
	}{
		{"显示名命中（不查别名）", "解保功", &jbg.ID},
		{"别名唯一命中", "老保", &jbg.ID},
		{"无别名不命中", "不存在的人", nil},
		{"歧义别名不猜", "亮哥", nil},
		{"pending 别名不算", "阿亮", nil},
	}
	for _, c := range cases {
		p, err := persons.FindByNameOrAlias(ctx, 1, c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.want == nil {
			if p != nil {
				t.Fatalf("%s: 应不命中，实际 %+v", c.name, p)
			}
			continue
		}
		if p == nil || p.ID != *c.want {
			t.Fatalf("%s: 应命中 #%s，实际 %+v", c.name, *c.want, p)
		}
	}
}
