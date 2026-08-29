package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
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
		SpeakerStates: &repo.SpeakerSessionStateRepo{DB: db}, Persons: &repo.PersonRepo{DB: db},
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

// TestGatherWeeklyWindowsPastWeek 复现并验证 I1（truncate + mis-window）修复：
// 目标是一个"过去的周"，且库里存在大量比该周更晚的记忆。修复前 gatherWeekly 用
// List(Since:ws, Limit:500) —— 500 被 repo 夹成 50 → 只取最新 50 条（全是远期记忆）→
// inRange 按周窗口全部滤掉 → 窗口内真实记忆被漏掉。修复后按天把 [dayStart,dayEnd)
// 下推到 SQL，窗口内记忆稳定被汇聚、窗口外记忆不出现。需要独立库（无 DSN 自动跳过）。
func TestGatherWeeklyWindowsPastWeek(t *testing.T) {
	g := newGenWithFake(t, &fakeLLM{}) // gatherWeekly 不调 LLM
	ctx := context.Background()

	ws := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC) // 2020-06-01 是周一
	sid := ids.New()
	t.Cleanup(func() { _ = g.Memories.DeleteBySessionExt(context.Background(), g.Memories.DB, sid) })

	inWindow := ws.AddDate(0, 0, 2).Add(10 * time.Hour) // 周三 10:00，落在 [ws, ws+7d)
	const inTitle = "窗口内记忆-2020周"
	batch := []*repo.Memory{{
		Type: "event", Title: inTitle, Content: "x", EpistemicType: "observed",
		SessionID: &sid, Status: "active", EventAt: &inWindow, Confidence: 0.9,
	}}
	// 60 条远期(2035)记忆：均晚于窗口下界 ws 且数量 >50，用于复现旧 bug 的挤占场景
	for i := 0; i < 60; i++ {
		ev := time.Date(2035, 1, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		batch = append(batch, &repo.Memory{
			Type: "event", Title: fmt.Sprintf("远期噪声-%d", i), Content: "y", EpistemicType: "observed",
			SessionID: &sid, Status: "active", EventAt: &ev, Confidence: 0.9,
		})
	}
	if err := g.Memories.InsertExt(ctx, g.Memories.DB, batch); err != nil {
		t.Fatal(err)
	}

	in, err := g.gatherWeekly(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, tl := range in.MemoriesByTopic {
		lines = append(lines, tl.Lines...)
	}
	if !containsStr(lines, inTitle) {
		t.Errorf("窗口内记忆应被汇聚，实际 = %v", lines)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "远期噪声-") {
			t.Errorf("窗口外(远期)记忆不应出现: %s", l)
		}
	}
	if in.DailyMemoryCnt[2] < 1 { // 周三桶应计到窗口内那条
		t.Errorf("DailyMemoryCnt[周三] 应>=1, got %v", in.DailyMemoryCnt)
	}
}

// TestTopicStatusNotFound 验证 M4：不存在的话题返回 sentinel ErrTopicNotFound
//（handler 据此映射 404）。gather 阶段即返回，不触 LLM。需要独立库（无 DSN 自动跳过）。
func TestTopicStatusNotFound(t *testing.T) {
	g := newGenWithFake(t, &fakeLLM{Err: errors.New("话题不存在时不应调用 LLM")})
	if _, err := g.TopicStatus(context.Background(), ids.New()); !errors.Is(err, ErrTopicNotFound) {
		t.Errorf("不存在话题应返回 ErrTopicNotFound, got %v", err)
	}
}

// TestDayRangeStable 验证 I2：dayRange 保持入参时区、切出 [00:00, 次日00:00) 的整天、且幂等。
// 纯单元（无需 DB / DSN）。
func TestDayRangeStable(t *testing.T) {
	d := time.Date(2026, 8, 25, 15, 30, 0, 0, time.Local)
	s, e := dayRange(d)
	if s.Location() != time.Local || e.Location() != time.Local {
		t.Errorf("dayRange 应保持入参时区(Local), got start=%s end=%s", s.Location(), e.Location())
	}
	if s.Hour() != 0 || s.Minute() != 0 || s.Second() != 0 {
		t.Errorf("start 应为当日 00:00, got %s", s)
	}
	if !e.Equal(s.AddDate(0, 0, 1)) {
		t.Errorf("[start,end) 应恰为一整天, start=%s end=%s", s, e)
	}
	if s2, e2 := dayRange(s); !s2.Equal(s) || !e2.Equal(e) {
		t.Errorf("dayRange 应稳定/幂等, got start=%s end=%s", s2, e2)
	}
}

// TestGatherDailyEmotionEnv 验证 P3：gatherDaily 汇聚当天声学环境（transcript）+
// 说话人情绪（speaker_session_state），效价经 EmotionToValence 映射，speaker_id 回显 person 名。
// 造一条当天 session + transcript（SetAcoustic 写场景/氛围）+ 一条情绪状态（绑 person）。
// 需要独立库（无 DSN 自动跳过）。用当天日期——session.created_at 由 DB 取 now，须落在日窗内。
func TestGatherDailyEmotionEnv(t *testing.T) {
	g := newGenWithFake(t, &fakeLLM{}) // gatherDaily 不调 LLM
	ctx := context.Background()
	day := time.Now() // session.created_at=now，dayRange(now) 窗口必含之

	// 说话人 → person：person.speaker_id 与情绪行的 speaker_id 一致，走 GetBySpeaker 回显名。
	speakerID := ids.New()
	person := &repo.Person{DisplayName: "张三", SpeakerID: &speakerID, Status: "active", Source: "manual"}
	if err := g.Persons.Create(ctx, person); err != nil {
		t.Fatal(err)
	}

	sess := &repo.AudioSession{ID: ids.New(), Source: "test", Filename: "emo.wav", Mime: "audio/wav", Status: "done", DurationMS: 1000}
	if err := g.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sess.ID, Language: "zh"}
	if err := g.Transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := g.Transcripts.SetAcoustic(ctx, tr.ID, "室内", nil, "", "轻松"); err != nil {
		t.Fatal(err)
	}
	if err := g.SpeakerStates.InsertBatch(ctx, []repo.SpeakerSessionState{{
		UserID: reviewUserID, TranscriptID: tr.ID, SessionID: sess.ID,
		SpeakerLabel: "1", SpeakerID: &speakerID,
		Emotion: "喜悦", MicroEmotion: "微笑", MentalState: "放松", Confidence: 0.9,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = g.Sessions.DB.ExecContext(bg, `DELETE FROM speaker_session_state WHERE session_id=?`, sess.ID.Int64())
		_, _ = g.Sessions.DB.ExecContext(bg, `DELETE FROM transcript WHERE session_id=?`, sess.ID.Int64())
		_, _ = g.Sessions.DB.ExecContext(bg, `DELETE FROM audio_session WHERE id=?`, sess.ID.Int64())
		_, _ = g.Sessions.DB.ExecContext(bg, `DELETE FROM person WHERE id=?`, person.ID.Int64())
	})

	in, err := g.gatherDaily(ctx, day)
	if err != nil {
		t.Fatal(err)
	}

	// 声学环境：应含一行同时带场景"室内"与氛围"轻松"（"室内·轻松"）。
	var envHit bool
	for _, n := range in.AcousticNotes {
		if strings.Contains(n, "室内") && strings.Contains(n, "轻松") {
			envHit = true
		}
	}
	if !envHit {
		t.Errorf("AcousticNotes 应含场景/氛围行(室内·轻松), 实际 = %v", in.AcousticNotes)
	}

	// 情绪点：应含 speaker=张三、Emotion=喜悦、Valence>0、When 为 HH:MM。
	var emoHit bool
	for _, e := range in.EmotionLines {
		if e.Speaker == "张三" && e.Emotion == "喜悦" {
			emoHit = true
			if e.Valence <= 0 {
				t.Errorf("喜悦效价应>0, got %v", e.Valence)
			}
			if e.MicroMood != "微笑" || e.MentalState != "放松" {
				t.Errorf("微情绪/精神状态未透传: %+v", e)
			}
			if m, _ := regexp.MatchString(`^\d{2}:\d{2}$`, e.When); !m {
				t.Errorf("When 应为 HH:MM, got %q", e.When)
			}
		}
	}
	if !emoHit {
		t.Errorf("EmotionLines 应含张三的喜悦情绪点, 实际 = %+v", in.EmotionLines)
	}
}

// containsStr 是测试小工具：判断切片是否含某字符串。
func containsStr(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
