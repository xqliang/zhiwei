package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// TestMain 统一初始化雪花 ID 节点（测试造数据 ids.New() 会生成主键）。
// 与 internal/repo、internal/api 等测试包一致：不 Init 时 ids.New() 会 panic。
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

func testDeps(t *testing.T) MCPDeps {
	t.Helper()
	db, err := repo.NewDB(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	return MCPDeps{
		Memory: &repo.MemoryRepo{DB: db}, Session: &repo.SessionRepo{DB: db},
		Transcript: &repo.TranscriptRepo{DB: db}, Topic: &repo.TopicRepo{DB: db},
		Todo: &repo.TodoRepo{DB: db},
	}
}

func firstText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("空工具结果")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("首段非 TextContent: %T", res.Content[0])
	}
	return tc.Text
}

func TestSearchMemoryTool(t *testing.T) {
	d := testDeps(t)
	ctx := t.Context()
	kw := "工具层检索验证词"
	sid := ids.New()
	t.Cleanup(func() { _ = d.Memory.DeleteBySessionExt(context.Background(), d.Memory.DB, sid) })
	ms := []*repo.Memory{{Type: "fact", Title: kw, Content: kw, SessionID: &sid, Status: "active", Confidence: 0.8}}
	if err := d.Memory.InsertExt(ctx, d.Memory.DB, ms); err != nil {
		t.Fatal(err)
	}
	res, _, err := searchMemoryHandler(d, toolUserID)(ctx, nil, searchMemoryArgs{Query: kw, Limit: 10})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var out []memoryOut
	if err := json.Unmarshal([]byte(firstText(t, res)), &out); err != nil {
		t.Fatalf("结果非 JSON 数组: %v", err)
	}
	var hit bool
	for _, m := range out {
		if m.Title == kw {
			hit = true
		}
	}
	if !hit {
		t.Errorf("search_memory 未返回关键词记忆")
	}
}

func TestGetTodosTool(t *testing.T) {
	d := testDeps(t)
	ctx := t.Context()
	res, _, err := getTodosHandler(d, toolUserID)(ctx, nil, getTodosArgs{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var out []todoOut
	if err := json.Unmarshal([]byte(firstText(t, res)), &out); err != nil {
		t.Fatalf("结果非 JSON 数组: %v", err)
	}
}

func TestGetTopicsTool(t *testing.T) {
	d := testDeps(t)
	ctx := t.Context()
	res, _, err := getTopicsHandler(d, toolUserID)(ctx, nil, getTopicsArgs{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var out []topicOut
	if err := json.Unmarshal([]byte(firstText(t, res)), &out); err != nil {
		t.Fatalf("结果非 JSON 数组: %v", err)
	}
}

func TestGetTimelineRecentTool(t *testing.T) {
	d := testDeps(t)
	ctx := t.Context()
	sess := &repo.AudioSession{ID: ids.New(), Source: "web_upload", Filename: "t.wav", DurationMS: 1000, Status: "completed"}
	if err := d.Session.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Session.Delete(context.Background(), sess.ID, nil) })
	res, _, err := getTimelineHandler(d, toolUserID)(ctx, nil, getTimelineArgs{Limit: 50})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var out []sessionOut
	if err := json.Unmarshal([]byte(firstText(t, res)), &out); err != nil {
		t.Fatalf("非 JSON 数组: %v", err)
	}
	var found bool
	for _, s := range out {
		if s.SessionID == sess.ID {
			found = true
		}
	}
	if !found {
		t.Error("最近时间线应含新建会话")
	}
}

func TestGetTimelineBySessionTool(t *testing.T) {
	d := testDeps(t)
	ctx := t.Context()
	sess := &repo.AudioSession{ID: ids.New(), Source: "web_upload", Filename: "t2.wav", DurationMS: 2000, Status: "completed"}
	if err := d.Session.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Session.Delete(context.Background(), sess.ID, nil) })
	tr := &repo.Transcript{SessionID: sess.ID, Language: "zh-CN"}
	if err := d.Transcript.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	segs := []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 0, SpeakerLabel: "张三", Text: "分段验证文本", StartMS: 0, EndMS: 500},
	}
	if err := d.Transcript.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	res, _, err := getTimelineHandler(d, toolUserID)(ctx, nil, getTimelineArgs{SessionID: sess.ID.String()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var out []segmentOut
	if err := json.Unmarshal([]byte(firstText(t, res)), &out); err != nil {
		t.Fatalf("非 JSON 数组: %v", err)
	}
	if len(out) != 1 || out[0].Text != "分段验证文本" || out[0].EndMS != 500 || out[0].Speaker != "张三" {
		t.Errorf("分段结果异常: %+v", out)
	}
}

// TestGetTimelineBySessionCrossUserIDOR 锁定 I1：带 session_id 读转写必须先按 userID 校验会话归属；
// 越权（读他人会话）返回 tool-error，绝不泄漏他人转写分段。此前该分支直接 GetBySession+ListSegments
// 无 user 校验，任何人凭 session_id 即可读他人转写。
func TestGetTimelineBySessionCrossUserIDOR(t *testing.T) {
	d := testDeps(t)
	ctx := t.Context()
	// 会话 + 转写 + 分段全归 user 2
	sess := &repo.AudioSession{ID: ids.New(), UserID: 2, Source: "web_upload", Filename: "u2.wav", DurationMS: 3000, Status: "completed"}
	if err := d.Session.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Session.Delete(context.Background(), sess.ID, nil) })
	tr := &repo.Transcript{SessionID: sess.ID, Language: "zh-CN"}
	if err := d.Transcript.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	segs := []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 0, SpeakerLabel: "机密", Text: "user2的机密转写", StartMS: 0, EndMS: 500},
	}
	if err := d.Transcript.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}

	// user 1 凭 user 2 的 session_id 读 → tool-error，绝不返回分段
	if res, _, err := getTimelineHandler(d, 1)(ctx, nil, getTimelineArgs{SessionID: sess.ID.String()}); err == nil {
		t.Fatalf("越权读他人会话转写应报 tool-error, got res=%v", res)
	}

	// owner(user 2) 读自己的 → 正常返回分段（校验未误伤 owner）
	res2, _, err := getTimelineHandler(d, 2)(ctx, nil, getTimelineArgs{SessionID: sess.ID.String()})
	if err != nil {
		t.Fatalf("owner 读自己会话应成功: %v", err)
	}
	var out []segmentOut
	if err := json.Unmarshal([]byte(firstText(t, res2)), &out); err != nil {
		t.Fatalf("非 JSON 数组: %v", err)
	}
	if len(out) != 1 || out[0].Text != "user2的机密转写" {
		t.Errorf("owner 读自己会话分段异常: %+v", out)
	}
}

// TestMCPToolUserIDInjection 锁定 2B-A：MCP 工具读的是「注入的 userID」的数据，而非写死的
// toolUserID。给 user 1 / user 2 各 seed 一条独占关键词记忆，各自 userID 的 search_memory
// handler 只应看到自己那条、看不到对方——多用户隔离的雏形。（旧版把 userID 写死为 1 时，
// searchMemoryHandler(d, 2) 仍只会查到 user 1 的数据，本用例必然失败，故它对新签名有真实约束力。）
func TestMCPToolUserIDInjection(t *testing.T) {
	d := testDeps(t)
	ctx := t.Context()
	const kw1 = "隔离验证词甲ISO"
	const kw2 = "隔离验证词乙ISO"
	s1, s2 := ids.New(), ids.New()
	// memory.user_id 无外键（仅索引），可直接为 user 2 造数据；InsertExt 尊重显式 UserID（非 0 不改）。
	m1 := &repo.Memory{UserID: 1, Type: "fact", Title: kw1, Content: kw1, SessionID: &s1,
		Status: "active", Confidence: 0.8, TranscriptSegmentIDs: ids.List{}}
	m2 := &repo.Memory{UserID: 2, Type: "fact", Title: kw2, Content: kw2, SessionID: &s2,
		Status: "active", Confidence: 0.8, TranscriptSegmentIDs: ids.List{}}
	if err := d.Memory.InsertExt(ctx, d.Memory.DB, []*repo.Memory{m1, m2}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Memory.DB.Exec("DELETE FROM memory WHERE id IN (?, ?)", m1.ID.Int64(), m2.ID.Int64())
	})

	// seenTitles：以某 userID 构造的 handler 检索「隔离验证词」前缀，返回命中标题集合。
	seenTitles := func(userID int64) map[string]bool {
		res, _, err := searchMemoryHandler(d, userID)(ctx, nil, searchMemoryArgs{Query: "隔离验证词", Limit: 50})
		if err != nil {
			t.Fatalf("search(uid=%d): %v", userID, err)
		}
		var out []memoryOut
		if err := json.Unmarshal([]byte(firstText(t, res)), &out); err != nil {
			t.Fatalf("结果非 JSON 数组: %v", err)
		}
		set := map[string]bool{}
		for _, m := range out {
			set[m.Title] = true
		}
		return set
	}

	if u1 := seenTitles(1); !u1[kw1] || u1[kw2] {
		t.Errorf("userID=1 应只见甲: 见甲=%v 见乙=%v", u1[kw1], u1[kw2])
	}
	if u2 := seenTitles(2); !u2[kw2] || u2[kw1] {
		t.Errorf("userID=2 应只见乙: 见甲=%v 见乙=%v", u2[kw1], u2[kw2])
	}
}
