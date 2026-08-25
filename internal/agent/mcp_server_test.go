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
	res, _, err := searchMemoryHandler(d)(ctx, nil, searchMemoryArgs{Query: kw, Limit: 10})
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
	res, _, err := getTodosHandler(d)(ctx, nil, getTodosArgs{})
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
	res, _, err := getTopicsHandler(d)(ctx, nil, getTopicsArgs{})
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
	res, _, err := getTimelineHandler(d)(ctx, nil, getTimelineArgs{Limit: 50})
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
	res, _, err := getTimelineHandler(d)(ctx, nil, getTimelineArgs{SessionID: sess.ID.String()})
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
