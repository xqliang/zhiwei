package repo

import (
	"testing"

	"zhiwei/internal/ids"
)

func TestMemorySearch(t *testing.T) {
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mr := &MemoryRepo{DB: db}
	ctx := t.Context()

	sid := ids.New()
	kw := "量子隧穿实验" // 独特词，避免与库里既有数据碰撞
	ms := []*Memory{
		{Type: "fact", Title: kw + "记录", Content: "今天讨论了" + kw + "的进展", SessionID: sid, Status: "active", Importance: 0.6, Confidence: 0.8},
		{Type: "idea", Title: "无关记忆", Content: "买牛奶", SessionID: sid, Status: "active", Importance: 0.3, Confidence: 0.8},
	}
	if err := mr.InsertExt(ctx, db, ms); err != nil {
		t.Fatalf("InsertExt: %v", err)
	}

	got, err := mr.Search(ctx, 1, kw, "", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var hit bool
	for _, m := range got {
		if m.ID == ms[0].ID {
			hit = true
		}
		if m.ID == ms[1].ID {
			t.Error("Search 命中了不含关键词的记忆")
		}
	}
	if !hit {
		t.Errorf("Search 未命中含关键词 %q 的记忆", kw)
	}

	got2, err := mr.Search(ctx, 1, kw, "idea", 20)
	if err != nil {
		t.Fatalf("Search(type=idea): %v", err)
	}
	for _, m := range got2 {
		if m.ID == ms[0].ID {
			t.Error("type=idea 不应命中 fact 记忆")
		}
	}
}
