package review

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestMain 初始化雪花 ID（集成测试造数据 ids.New() 会用）。与 repo/agent 测试包一致。
func TestMain(m *testing.M) {
	_ = ids.Init(1)
	os.Exit(m.Run())
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	return dsn
}

// newGenWithFake 用真实 repo（独立库）+ 注入的 fakeLLM 装配 Generator。
func newGenWithFake(t *testing.T, f *fakeLLM) *Generator {
	t.Helper()
	db, err := repo.NewDB(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	return &Generator{
		LLM: f, Model: "test-model",
		DailyPrompt: "SYS-DAILY", WeeklyPrompt: "SYS-WEEKLY", TopicStatusPrompt: "SYS-TOPIC",
		Reviews: &repo.ReviewRepo{DB: db}, TopicStatuses: &repo.TopicStatusRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Todos: &repo.TodoRepo{DB: db}, Topics: &repo.TopicRepo{DB: db},
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
	}
}

func TestDailyPersistReady(t *testing.T) {
	f := &fakeLLM{Reply: `{"headline":"当天要点","highlights":["h1"]}`}
	g := newGenWithFake(t, f)
	ctx := context.Background()
	day := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC) // 远期日期避开真实数据
	t.Cleanup(func() { _ = g.Reviews.UpsertDaily(ctx, reviewUserID, day, nil, "pending") })

	// 种一条当天记忆，验证被汇聚（不强绑断言其入 prompt，主要验证落库链路）
	sid := ids.New()
	ev := day.Add(10 * time.Hour)
	_ = g.Memories.InsertExt(ctx, g.Memories.DB, []*repo.Memory{{
		Type: "event", Title: "集成测试记忆", Content: "xxx", EpistemicType: "observed",
		SessionID: &sid, Status: "active", EventAt: &ev, Confidence: 0.9,
	}})
	t.Cleanup(func() { _ = g.Memories.DeleteBySessionExt(context.Background(), g.Memories.DB, sid) })

	row, err := g.Daily(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Status != "ready" || row.Content == nil {
		t.Fatalf("日报应 ready 且有 content: %+v", row)
	}

	// 幂等重生成：再调一次仍 ready，且 GetDaily 只一行（UpsertDaily 覆盖）
	if _, err := g.Daily(ctx, day); err != nil {
		t.Fatal(err)
	}
	got, _ := g.Reviews.GetDaily(ctx, reviewUserID, day)
	if got == nil || got.Status != "ready" {
		t.Errorf("重生成后应仍 ready: %+v", got)
	}
}

func TestDailyLLMFailMarksFailed(t *testing.T) {
	g := newGenWithFake(t, &fakeLLM{Reply: "模型没给 JSON"}) // 解析失败
	ctx := context.Background()
	day := time.Date(2030, 1, 3, 0, 0, 0, 0, time.UTC)
	t.Cleanup(func() { _ = g.Reviews.UpsertDaily(ctx, reviewUserID, day, nil, "pending") })
	if _, err := g.Daily(ctx, day); err == nil {
		t.Error("解析失败应上抛 error")
	}
	got, _ := g.Reviews.GetDaily(ctx, reviewUserID, day)
	if got == nil || got.Status != "failed" {
		t.Errorf("失败应落 status=failed: %+v", got)
	}
}

func TestWeeklyPersistReady(t *testing.T) {
	f := &fakeLLM{Reply: `{"headline":"本周总结","by_topic":[{"topic":"工作","progress":0.5,"key_events":["e1"],"open_todos":[],"risks":[]}],"trends":[{"metric":"每日记忆数","series":[1,0,0,0,0,0,0]}],"risks":[],"next_week":["x"]}`}
	g := newGenWithFake(t, f)
	ctx := context.Background()
	ws := time.Date(2030, 1, 7, 0, 0, 0, 0, time.UTC) // 2030-01-07 是周一
	we := ws.AddDate(0, 0, 6)
	t.Cleanup(func() { _ = g.Reviews.UpsertWeekly(ctx, reviewUserID, ws, we, nil, "pending") })

	row, err := g.Weekly(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Status != "ready" || row.Content == nil {
		t.Fatalf("周报应 ready 且有 content: %+v", row)
	}
	var wc WeeklyContent
	if err := json.Unmarshal(*row.Content, &wc); err != nil || wc.Headline != "本周总结" {
		t.Errorf("周报 content 异常: %+v (err=%v)", wc, err)
	}
}

func TestTopicStatusPersist(t *testing.T) {
	f := &fakeLLM{Reply: `{"summary":"进行中","progress":0.4,"milestones":["m1"],"open_todos":[],"risks":[{"desc":"缺资料","severity":"medium"}],"blockers":[]}`}
	g := newGenWithFake(t, f)
	ctx := context.Background()

	tp := &repo.Topic{Name: "集成测试话题", Status: "active", CreatedBy: "user"}
	if err := g.Topics.Create(ctx, tp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = g.Topics.Delete(context.Background(), tp.ID)
		_, _ = g.TopicStatuses.DB.ExecContext(context.Background(), `DELETE FROM topic_status WHERE topic_id = ?`, tp.ID.Int64())
	})

	row, err := g.TopicStatus(ctx, tp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Content == nil {
		t.Fatalf("话题状态应有快照: %+v", row)
	}
	var tc TopicStatusContent
	if err := json.Unmarshal(*row.Content, &tc); err != nil || tc.Progress != 0.4 || len(tc.Risks) != 1 {
		t.Errorf("话题状态 content 异常: %+v (err=%v)", tc, err)
	}

	// 失败路径：解析失败不插新行、直接上抛
	g.LLM = &fakeLLM{Reply: "没有 JSON"}
	if _, err := g.TopicStatus(ctx, tp.ID); err == nil {
		t.Error("解析失败应上抛")
	}
}
