package repo

import (
	"context"
	"testing"
)

func TestPersonLifecycle(t *testing.T) {
	db, err := NewDB(TestDSN(t))
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
	got, err := persons.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "张三丰" || got.SpeakerID == nil || *got.SpeakerID != sp2.ID {
		t.Fatalf("Update 未生效: %+v", got)
	}
	if err := persons.SetStatus(ctx, p.ID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	if g2, _ := persons.Get(ctx, p.ID); g2.Status != "dismissed" {
		t.Fatalf("SetStatus 未生效: %+v", g2)
	}
}
