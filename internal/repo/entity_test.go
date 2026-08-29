package repo

import (
	"context"
	"testing"

	"zhiwei/internal/repotest"
)

// TestEntityKBRepo 集成测试：ReplaceAuto 重建 auto（删旧+落新、manual 不动）、
// manual CRUD、ListEnabled 过滤、CountByKind 统计。
func TestEntityKBRepo(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &EntityKBRepo{DB: db}
	const uid int64 = 1
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid) })
	_, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid)

	// 1) auto 批量落库 + ListEnabled 读回（enabled 默认 true）。
	auto1 := []Entity{
		{UserID: uid, Canonical: "张梦瑜", Kind: EntityKindPerson, Source: EntitySourceAuto, Pinyin: sp("zhang meng yu"), SourceRef: sp("person:1")},
		{UserID: uid, Canonical: "阿黄", Kind: EntityKindPet, Source: EntitySourceAuto, Pinyin: sp("a huang")},
	}
	if err := r.ReplaceAuto(ctx, uid, EntityKindPerson, auto1[:1]); err != nil {
		t.Fatalf("ReplaceAuto person: %v", err)
	}
	if err := r.ReplaceAuto(ctx, uid, EntityKindPet, auto1[1:]); err != nil {
		t.Fatalf("ReplaceAuto pet: %v", err)
	}
	got, err := r.ListEnabled(ctx, uid)
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条实体, got %d", len(got))
	}

	// 2) 再 ReplaceAuto(person)：旧 person auto 全删重建（张梦瑜→王芳），pet 不受影响。
	auto2 := []Entity{{UserID: uid, Canonical: "王芳", Kind: EntityKindPerson, Source: EntitySourceAuto, Pinyin: sp("wang fang")}}
	if err := r.ReplaceAuto(ctx, uid, EntityKindPerson, auto2); err != nil {
		t.Fatalf("ReplaceAuto 重建: %v", err)
	}
	got, _ = r.ListEnabled(ctx, uid)
	if len(got) != 2 {
		t.Fatalf("重建后应仍是 2 条（person 换 1 条 + pet 1 条）, got %d", len(got))
	}
	names := map[string]bool{}
	for _, e := range got {
		names[e.Canonical] = true
	}
	if names["张梦瑜"] || !names["王芳"] || !names["阿黄"] {
		t.Fatalf("重建结果不对: %v", names)
	}

	// 3) manual CRUD：创建/读回/改名/禁用/删除；auto 条目不能被 UpdateManual 改。
	m := &Entity{UserID: uid, Canonical: "天枢项目", Kind: EntityKindCustom, Source: EntitySourceManual, Pinyin: sp("tian shu xiang mu"), Note: sp("内部代号 TS")}
	if err := r.CreateManual(ctx, m); err != nil {
		t.Fatalf("CreateManual: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("CreateManual 应回填 id")
	}
	full, err := r.Get(ctx, uid, m.ID)
	if err != nil || full.Canonical != "天枢项目" || full.Source != EntitySourceManual {
		t.Fatalf("Get: %v %+v", err, full)
	}
	if err := r.UpdateManual(ctx, uid, m.ID, "天璇项目", "内部代号 TX", sp("tian xuan xiang mu"), nil); err != nil {
		t.Fatalf("UpdateManual: %v", err)
	}
	if full, _ = r.Get(ctx, uid, m.ID); full.Canonical != "天璇项目" {
		t.Fatalf("改名后应读回天璇项目: %+v", full)
	}
	// 改名同时重算的 pinyin 应一并落库（否则召回按旧拼音失配）。
	if full.Pinyin == nil || *full.Pinyin != "tian xuan xiang mu" {
		t.Fatalf("改名后 pinyin 应更新: %+v", full.Pinyin)
	}
	// auto 条目被 UpdateManual → sql.ErrNoRows 语义（应报错而非静默成功）。
	autoID := got[0].ID
	if err := r.UpdateManual(ctx, uid, autoID, "xxx", "", nil, nil); err == nil {
		t.Fatal("UpdateManual 不应能改 auto 条目")
	}
	if err := r.SetEnabled(ctx, uid, m.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if list, _ := r.ListEnabled(ctx, uid); len(list) != 2 {
		t.Fatalf("禁用后 ListEnabled 应只剩 2 条, got %d", len(list))
	}
	if err := r.Delete(ctx, uid, m.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(ctx, uid, m.ID); err == nil {
		t.Fatal("删除后 Get 应报错")
	}

	// 4) CountByKind：按 kind 统计 enabled 条数（设置页汇总用）。
	counts, err := r.CountByKind(ctx, uid)
	if err != nil {
		t.Fatalf("CountByKind: %v", err)
	}
	if counts[EntityKindPerson] != 1 || counts[EntityKindPet] != 1 {
		t.Fatalf("计数不对: %v", counts)
	}
}

// TestEntitySettingsRepo：默认值（无行时零值+默认）、Upsert 读回。
func TestEntitySettingsRepo(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &EntitySettingsRepo{DB: db}
	const uid int64 = 2 // 与 TestEntityKBRepo 隔离
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM entity_settings WHERE user_id = ?", uid) })
	_, _ = db.Exec("DELETE FROM entity_settings WHERE user_id = ?", uid)

	// 1) 无行 → 默认值（enabled=true、threshold=0.8、auto_sources=全量 kinds）。
	s, err := r.Get(ctx, uid)
	if err != nil {
		t.Fatalf("Get 默认: %v", err)
	}
	if !s.CorrectionEnabled || s.ConfidenceThreshold != 0.8 {
		t.Fatalf("默认值不对: %+v", s)
	}
	if len(s.AutoSources) != 6 {
		t.Fatalf("默认 auto_sources 应为 6 种 kind: %v", s.AutoSources)
	}

	// 2) Upsert 后读回。
	if err := r.Upsert(ctx, uid, false, 0.9, []string{"person", "pet"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	s, _ = r.Get(ctx, uid)
	if s.CorrectionEnabled || s.ConfidenceThreshold != 0.9 || len(s.AutoSources) != 2 {
		t.Fatalf("读回不符: %+v", s)
	}
	// 3) 阈值越界被拒绝。
	if err := r.Upsert(ctx, uid, true, 1.5, nil); err == nil {
		t.Fatal("阈值 1.5 应被拒绝")
	}
	// 4) auto_sources 含 custom 被拒绝（custom 没有自动入库来源）。
	if err := r.Upsert(ctx, uid, true, 0.8, []string{"person", "custom"}); err == nil {
		t.Fatal("auto_sources 含 custom 应被拒绝")
	}
}

// TestEntityKBReplaceAutoInvariants 覆盖 ReplaceAuto 的两条关键不变量 + 空 list 语义：
// (1) 与同名 manual 碰撞时保留 manual；(2) auto 禁用态跨刷新保留；(3) 空 list 清空该 kind auto。
func TestEntityKBReplaceAutoInvariants(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &EntityKBRepo{DB: db}
	const uid int64 = 3 // 与其它用例隔离
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid) })
	_, _ = db.Exec("DELETE FROM entity_kb WHERE user_id = ?", uid)

	// findByCanonical 按 canonical 在 List(全部) 里定位（刷新后 id 会变，不能按 id 找）。
	findByCanonical := func(kind, canonical string) *Entity {
		list, err := r.List(ctx, uid, kind)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for i := range list {
			if list[i].Canonical == canonical {
				return &list[i]
			}
		}
		return nil
	}

	// ---- 不变量 1：manual/auto 同名碰撞，manual 幸存 ----
	m := &Entity{UserID: uid, Canonical: "王芳", Kind: EntityKindPerson, Source: EntitySourceManual, Note: sp("我姐")}
	if err := r.CreateManual(ctx, m); err != nil {
		t.Fatalf("CreateManual 王芳: %v", err)
	}
	// 流水线也从来源里派生出同名的「王芳/person」→ ReplaceAuto 不应报错（INSERT IGNORE 撞键跳过）。
	if err := r.ReplaceAuto(ctx, uid, EntityKindPerson, []Entity{{Canonical: "王芳", Pinyin: sp("wang fang")}}); err != nil {
		t.Fatalf("ReplaceAuto 同名不应报错: %v", err)
	}
	rows, _ := r.List(ctx, uid, EntityKindPerson)
	if len(rows) != 1 {
		t.Fatalf("同名碰撞后应仍只有 1 条王芳/person, got %d", len(rows))
	}
	if wf := findByCanonical(EntityKindPerson, "王芳"); wf == nil || wf.Source != EntitySourceManual || wf.Note == nil || *wf.Note != "我姐" {
		t.Fatalf("碰撞后应保留 manual 行（source=manual、note 不变）: %+v", wf)
	}

	// ---- 不变量 2：auto 禁用态跨刷新保留 ----
	if err := r.ReplaceAuto(ctx, uid, EntityKindPet, []Entity{{Canonical: "张三", Pinyin: sp("zhang san")}}); err != nil {
		t.Fatalf("ReplaceAuto 张三: %v", err)
	}
	zs := findByCanonical(EntityKindPet, "张三")
	if zs == nil {
		t.Fatal("张三应存在")
	}
	if err := r.SetEnabled(ctx, uid, zs.ID, false); err != nil {
		t.Fatalf("SetEnabled 禁用张三: %v", err)
	}
	// 再次刷新同一 kind：id 会变，但禁用态应按 canonical 回放保留。
	if err := r.ReplaceAuto(ctx, uid, EntityKindPet, []Entity{{Canonical: "张三", Pinyin: sp("zhang san")}}); err != nil {
		t.Fatalf("ReplaceAuto 张三 重建: %v", err)
	}
	zs2 := findByCanonical(EntityKindPet, "张三")
	if zs2 == nil || zs2.Enabled {
		t.Fatalf("刷新后张三禁用态应保留（enabled=false）: %+v", zs2)
	}

	// ---- 不变量 3：空 list 清空该 kind 的 auto，manual 不动 ----
	if err := r.ReplaceAuto(ctx, uid, EntityKindPet, nil); err != nil {
		t.Fatalf("ReplaceAuto pet 空: %v", err)
	}
	if findByCanonical(EntityKindPet, "张三") != nil {
		t.Fatal("空 list 刷新后 pet auto 应被清空")
	}
	if wf := findByCanonical(EntityKindPerson, "王芳"); wf == nil {
		t.Fatal("清空 pet 不应影响 person 的 manual 王芳")
	}
}

// sp 字符串转指针的测试辅助（pinyin/note/source_ref 是 *string 列）。
func sp(s string) *string { return &s }
