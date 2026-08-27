package repo

import (
	"context"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

// TestPersonPetQueries 覆盖 pet 平面 repo：Create/Get 往返（含可空列 NULL）、
// FindActiveByNameExt 同名匹配、FindByNaturalKeyExt 同 session 幂等、
// ListPending/CountPendingByPerson、SetStatus、人物级联 dismiss/restore。
func TestPersonPetQueries(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persons := &PersonRepo{DB: db}
	pets := &PersonPetRepo{DB: db}

	// 挂独立人物 + 收尾删净（跨包 ListPending(user=1) 隔离，对齐 activity 测试模式）。
	p := &Person{DisplayName: "宠物测试-甲"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM person_pet WHERE person_id = ?`, p.ID.Int64())
	})
	sess := ids.New()

	// p1：满字段宠物（active）。
	bd := time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC)
	p1 := &PersonPet{
		PersonID: p.ID, Name: "小花", Nickname: strp("花花"), Species: "猫", Breed: strp("布偶"),
		Gender: strp("母"), AgeText: strp("3岁"), Birthday: &bd, Likes: strp("不吃鱼"),
		SessionID: &sess,
	}
	if err := pets.Create(ctx, p1); err != nil {
		t.Fatal(err)
	}
	// p2：极简宠物（仅 name+species，可空列全 NULL），pending。
	p2 := &PersonPet{PersonID: p.ID, Name: "豆豆", Species: "狗", Status: "pending", SessionID: &sess}
	if err := pets.Create(ctx, p2); err != nil {
		t.Fatal(err)
	}

	// 零值兜底：p2 未显式给横切字段。
	if p2.Confidence != 0.8 || p2.Version != 1 || p2.Source != "manual" ||
		p2.EpistemicType != "observed" || p2.Status != "pending" || p2.UserID != 1 {
		t.Fatalf("CreateExt 零值兜底异常: %+v", p2)
	}

	// Get 往返：满字段。
	got1 := mustGetPet(t, pets, p1.ID)
	if got1.Name != "小花" || got1.Species != "猫" {
		t.Fatalf("name/species 回读异常: %+v", got1)
	}
	if got1.Nickname == nil || *got1.Nickname != "花花" || got1.Breed == nil || *got1.Breed != "布偶" ||
		got1.Gender == nil || *got1.Gender != "母" || got1.AgeText == nil || *got1.AgeText != "3岁" ||
		got1.Likes == nil || *got1.Likes != "不吃鱼" {
		t.Fatalf("可空列回读异常: %+v", got1)
	}
	if got1.Birthday == nil || !got1.Birthday.Equal(bd) {
		t.Fatalf("birthday 回读异常: %v (期望 %v)", got1.Birthday, bd)
	}
	// Get 往返：极简行可空列全 NULL。
	got2 := mustGetPet(t, pets, p2.ID)
	if got2.Nickname != nil || got2.Breed != nil || got2.Gender != nil ||
		got2.AgeText != nil || got2.Birthday != nil || got2.Likes != nil {
		t.Fatalf("极简行可空列应全 NULL: %+v", got2)
	}
	// Get 未命中。
	if g, err := pets.Get(ctx, ids.New()); g != nil || err != nil {
		t.Fatalf("Get(不存在) 应 (nil,nil): %+v err=%v", g, err)
	}

	// FindActiveByNameExt：命中 active 的小花；豆豆是 pending 不命中。
	fa, err := pets.FindActiveByNameExt(ctx, db, p.ID, "小花")
	if err != nil {
		t.Fatal(err)
	}
	if fa == nil || fa.ID != p1.ID {
		t.Fatalf("FindActiveByNameExt 未命中 p1: %+v", fa)
	}
	if miss, err := pets.FindActiveByNameExt(ctx, db, p.ID, "豆豆"); err != nil {
		t.Fatal(err)
	} else if miss != nil {
		t.Fatalf("pending 行不应被 FindActiveByNameExt 命中: %+v", miss)
	}

	// FindByNaturalKeyExt：同 session 同名命中（任意 status）；不同名不命中。
	nk, err := pets.FindByNaturalKeyExt(ctx, db, sess, p.ID, "豆豆")
	if err != nil {
		t.Fatal(err)
	}
	if nk == nil || nk.ID != p2.ID {
		t.Fatalf("FindByNaturalKeyExt 未命中 p2: %+v", nk)
	}
	if miss, err := pets.FindByNaturalKeyExt(ctx, db, sess, p.ID, "旺财"); err != nil {
		t.Fatal(err)
	} else if miss != nil {
		t.Fatalf("不同名不应命中: %+v", miss)
	}

	// ListByPerson：2 行。
	list, err := pets.ListByPerson(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListByPerson 应 2 行: %d", len(list))
	}

	// ListPending / CountPendingByPerson：仅 p2。
	pend, err := pets.ListPending(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range pend {
		if x.ID == p2.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("ListPending 未包含 p2")
	}
	if n, err := pets.CountPendingByPerson(ctx, p.ID); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("CountPendingByPerson 应 1: %d", n)
	}

	// SetStatus：p2 → dismissed，pending 计数归零。
	if err := pets.SetStatus(ctx, p2.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if n, err := pets.CountPendingByPerson(ctx, p.ID); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("SetStatus 后 pending 计数应 0: %d", n)
	}

	// 人物级联：dismiss 全部活跃行 → 恢复。
	if n, err := pets.DismissAllByPersonExt(ctx, db, p.ID); err != nil {
		t.Fatal(err)
	} else if n != 1 { // p1 active（p2 已 dismissed 终态不动）
		t.Fatalf("级联 dismiss 应 1 行: %d", n)
	}
	if g := mustGetPet(t, pets, p1.ID); g.Status != "dismissed" || g.PreDismissStatus == nil || *g.PreDismissStatus != "active" {
		t.Fatalf("级联 dismiss 后状态异常: %+v", g)
	}
	if n, err := pets.RestoreArchivedExt(ctx, db, p.ID); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("级联恢复应 1 行: %d", n)
	}
	if g := mustGetPet(t, pets, p1.ID); g.Status != "active" || g.PreDismissStatus != nil {
		t.Fatalf("级联恢复后状态异常: %+v", g)
	}
}

func mustGetPet(t *testing.T, r *PersonPetRepo, id ids.ID) *PersonPet {
	t.Helper()
	x, err := r.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if x == nil {
		t.Fatalf("Get(%v) 应命中却为 nil", id)
	}
	return x
}
