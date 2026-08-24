package agent

import (
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
	ms := []*repo.Memory{{Type: "fact", Title: kw, Content: kw, SessionID: ids.New(), Status: "active", Confidence: 0.8}}
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
