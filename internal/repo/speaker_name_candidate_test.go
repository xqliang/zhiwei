package repo

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

// testDB 返回测试库连接（无 TEST_MYSQL_DSN 时 skip）。
func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// seedCandidate 准备一个 speaker（随机名，模拟自动登记），返回其 ID。
// 复用调用方传入的 db，避免每次另开连接池（与既有 repo 测试模式一致）。
func seedCandidate(t *testing.T, db *sqlx.DB, name string) ids.ID {
	t.Helper()
	speakers := &SpeakerRepo{DB: db}
	sp := &Speaker{Name: name, Source: "auto"}
	if err := speakers.Create(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	return sp.ID
}

func TestCandidateUpsertAndList(t *testing.T) {
	db := testDB(t)
	r := &SpeakerNameCandidateRepo{DB: db}
	ctx := context.Background()
	sid := seedCandidate(t, db, "说话人ab3x9")

	// 初次插入两个候选（第二行故意低置信度，验证倒序）
	if err := r.Upsert(ctx, sid, "张总", 0.82, "对方在 15:03:12 说『张总，您看这个方案』", 1001); err != nil {
		t.Fatal(err)
	}
	if err := r.Upsert(ctx, sid, "张明", 0.4, "自称『我姓张』", 1001); err != nil {
		t.Fatal(err)
	}
	list, err := r.ListBySpeakers(ctx, []ids.ID{sid})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 条候选，实际 %d", len(list))
	}
	if list[0].Name != "张总" || list[0].Confidence != 0.82 {
		t.Fatalf("倒序首位应为 张总/0.82，实际 %s/%.2f", list[0].Name, list[0].Confidence)
	}
	if list[0].Evidence != "对方在 15:03:12 说『张总，您看这个方案』" {
		t.Fatalf("evidence=%s", list[0].Evidence)
	}

	// NULL 分支：sourceSessionID 传 0 时应存 NULL，扫描回来 SourceSessionID == nil
	if err := r.Upsert(ctx, sid, "李工", 0.3, "证据", 0); err != nil {
		t.Fatal(err)
	}
	list, err = r.ListBySpeakers(ctx, []ids.ID{sid})
	if err != nil {
		t.Fatal(err)
	}
	var li *SpeakerNameCandidate
	for i := range list {
		if list[i].Name == "李工" {
			li = &list[i]
		}
	}
	if li == nil {
		t.Fatal("未找到候选 李工")
	}
	if li.SourceSessionID != nil {
		t.Fatalf("sourceSessionID=0 应存 NULL，SourceSessionID 应为 nil，实际 %v", *li.SourceSessionID)
	}
}

func TestCandidateUpsertAccumulatesMaxConfidence(t *testing.T) {
	db := testDB(t)
	r := &SpeakerNameCandidateRepo{DB: db}
	ctx := context.Background()
	sid := seedCandidate(t, db, "说话人cd4e0")

	// 同名候选跨 session 复现：第二次置信度更低 → 保留最高置信、证据取最新
	if err := r.Upsert(ctx, sid, "张总", 0.82, "旧证据", 1001); err != nil {
		t.Fatal(err)
	}
	if err := r.Upsert(ctx, sid, "张总", 0.5, "新证据", 1002); err != nil {
		t.Fatal(err)
	}
	list, _ := r.ListBySpeakers(ctx, []ids.ID{sid})
	if len(list) != 1 {
		t.Fatalf("同名 upsert 后仍应 1 行，实际 %d", len(list))
	}
	if list[0].Confidence != 0.82 {
		t.Fatalf("应保留最高置信 0.82，实际 %.2f", list[0].Confidence)
	}
	if list[0].Evidence != "新证据" {
		t.Fatalf("证据应取最新，实际 %s", list[0].Evidence)
	}
	// 反向：第二次更高 → 抬升
	if err := r.Upsert(ctx, sid, "张总", 0.95, "更强证据", 1003); err != nil {
		t.Fatal(err)
	}
	list, _ = r.ListBySpeakers(ctx, []ids.ID{sid})
	if list[0].Confidence != 0.95 {
		t.Fatalf("置信度应抬升到 0.95，实际 %.2f", list[0].Confidence)
	}
}

func TestCandidateDelete(t *testing.T) {
	db := testDB(t)
	r := &SpeakerNameCandidateRepo{DB: db}
	ctx := context.Background()
	sid := seedCandidate(t, db, "说话人ef5a1")
	other := seedCandidate(t, db, "说话人gh6b2")
	_ = r.Upsert(ctx, sid, "张总", 0.8, "", 1001)
	_ = r.Upsert(ctx, sid, "张明", 0.4, "", 1001)
	_ = r.Upsert(ctx, other, "李哥", 0.7, "", 1001)

	// 删单个候选（前端「忽略」）
	if err := r.DeleteOne(ctx, sid, "张明"); err != nil {
		t.Fatal(err)
	}
	list, _ := r.ListBySpeakers(ctx, []ids.ID{sid})
	if len(list) != 1 || list[0].Name != "张总" {
		t.Fatalf("删单条后应剩 张总，实际 %+v", list)
	}
	// 幂等：删不存在的候选不报错
	if err := r.DeleteOne(ctx, sid, "不存在"); err != nil {
		t.Fatalf("删不存在候选应幂等: %v", err)
	}
	// 按说话人清空（改名采纳后调用），不影响他人
	if err := r.DeleteBySpeaker(ctx, sid); err != nil {
		t.Fatal(err)
	}
	list, _ = r.ListBySpeakers(ctx, []ids.ID{sid, other})
	if len(list) != 1 || list[0].SpeakerID != other {
		t.Fatalf("清空后应只剩 other 的候选，实际 %+v", list)
	}
}
