package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// mcpText 取工具结果的文本载荷（proposal JSON）。
func mcpText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("空工具结果")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("非 TextContent: %T", res.Content[0])
	}
	return tc.Text
}

// p2dDeps 构造写-提议闸门测试用的 MCPDeps + ProposalDeps（同一 DB）。
func p2dDeps(t *testing.T) (MCPDeps, ProposalDeps) {
	t.Helper()
	db, err := repo.NewDB(orchDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	mem := &repo.MemoryRepo{DB: db}
	top := &repo.TopicRepo{DB: db}
	tod := &repo.TodoRepo{DB: db}
	tt := &repo.TodoTopicRepo{DB: db}
	pr := &repo.AgentProposalRepo{DB: db}
	md := MCPDeps{Memory: mem, Topic: top, Todo: tod, Proposals: pr}
	pd := ProposalDeps{DB: db, Proposals: pr, Memories: mem, Topics: top, Todos: tod, TodoTopics: tt}
	return md, pd
}

// postProposal 经真 HTTP 路由 confirm/dismiss 一条提议, 返回状态码与响应体解析出的提议。
func postProposal(t *testing.T, pd ProposalDeps, id ids.ID, action string) (int, repo.AgentProposal) {
	t.Helper()
	r := chi.NewRouter()
	RegisterProposals(r, pd)
	req := httptest.NewRequest("POST", "/api/agent/proposals/"+id.String()+"/"+action, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var p repo.AgentProposal
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p
}

func seedMemory(t *testing.T, mem *repo.MemoryRepo, title, content string) *repo.Memory {
	t.Helper()
	m := &repo.Memory{
		Type: "fact", Title: title, Content: content, EpistemicType: "observed",
		Confidence: 0.8, Status: "active", TranscriptSegmentIDs: ids.List{},
	}
	if err := mem.InsertExt(t.Context(), mem.DB, []*repo.Memory{m}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = mem.DB.Exec("DELETE FROM memory WHERE id = ?", m.ID.Int64()) })
	return m
}

func cleanupProposal(t *testing.T, pd ProposalDeps, id ids.ID) {
	t.Cleanup(func() { _, _ = pd.DB.Exec("DELETE FROM agent_proposal WHERE id = ?", id.Int64()) })
}

// TestProposeMemoryEditNoMutation 锁定 §8 根防线：propose_* 只建 pending 提议, 绝不改领域行。
func TestProposeMemoryEditNoMutation(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	m := seedMemory(t, md.Memory, "原标题NM", "原内容NM")
	base, err := md.Memory.Get(ctx, m.ID) // 读库基线（version 由 DB 默认赋值, 非内存 m 的零值）
	if err != nil {
		t.Fatal(err)
	}

	res, _, err := proposeMemoryEditHandler(md)(ctx, nil, proposeMemoryEditArgs{
		MemoryID: m.ID.String(), NewTitle: "新标题NM", Rationale: "更清晰",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	if err := json.Unmarshal([]byte(mcpText(t, res)), &p); err != nil {
		t.Fatalf("解析提议: %v", err)
	}
	cleanupProposal(t, pd, p.ID)
	if p.Status != "pending" || p.Kind != "memory_update" || p.TargetID == nil || *p.TargetID != m.ID {
		t.Fatalf("提议异常: %+v", p)
	}
	// 关键：memory 未被改动
	got, err := md.Memory.Get(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != base.Title || got.Version != base.Version {
		t.Errorf("propose 不应改 memory, got title=%q version=%d (基线 title=%q version=%d)", got.Title, got.Version, base.Title, base.Version)
	}
}

// TestConfirmMemoryUpdateApplyOnce 锁定 §8：确认才落库(version+1, applied+applied_ref); 重复确认幂等不重复应用。
func TestConfirmMemoryUpdateApplyOnce(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	m := seedMemory(t, md.Memory, "旧标题AO", "旧内容AO")
	before, _ := md.Memory.Get(ctx, m.ID)

	res, _, err := proposeMemoryEditHandler(md)(ctx, nil, proposeMemoryEditArgs{
		MemoryID: m.ID.String(), NewTitle: "新标题AO", NewContent: "新内容AO",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)

	code, p1 := postProposal(t, pd, p.ID, "confirm")
	if code != http.StatusOK || p1.Status != "applied" {
		t.Fatalf("confirm code=%d status=%s", code, p1.Status)
	}
	got, _ := md.Memory.Get(ctx, m.ID)
	if got.Title != "新标题AO" || got.Content != "新内容AO" {
		t.Errorf("confirm 未落库: title=%q content=%q", got.Title, got.Content)
	}
	if got.Version != before.Version+1 {
		t.Errorf("version 应 +1: before=%d after=%d", before.Version, got.Version)
	}
	if p1.AppliedRef == nil || *p1.AppliedRef != m.ID {
		t.Errorf("applied_ref 应指向 memory: %+v", p1.AppliedRef)
	}
	verAfter := got.Version

	// 重复确认：apply-once → 幂等, 不再改 version
	code2, p2 := postProposal(t, pd, p.ID, "confirm")
	if code2 != http.StatusOK || p2.Status != "applied" {
		t.Fatalf("2nd confirm code=%d status=%s", code2, p2.Status)
	}
	got2, _ := md.Memory.Get(ctx, m.ID)
	if got2.Version != verAfter {
		t.Errorf("重复确认不应再改 version: %d → %d", verAfter, got2.Version)
	}
}

// TestDismissProposal 锁定放弃：目标行不变, 提议置 dismissed。
func TestDismissProposal(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	m := seedMemory(t, md.Memory, "放弃标题DZ", "放弃内容DZ")

	res, _, err := proposeMemoryEditHandler(md)(ctx, nil, proposeMemoryEditArgs{MemoryID: m.ID.String(), NewTitle: "不该生效DZ"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)

	code, p1 := postProposal(t, pd, p.ID, "dismiss")
	if code != http.StatusOK || p1.Status != "dismissed" {
		t.Fatalf("dismiss code=%d status=%s", code, p1.Status)
	}
	got, _ := md.Memory.Get(ctx, m.ID)
	if got.Title != "放弃标题DZ" {
		t.Errorf("放弃不应改 memory: title=%q", got.Title)
	}
}

// TestConfirmTodoCreate 锁定 todo_create：确认后 todo 表新增一行, applied_ref 指向新待办。
func TestConfirmTodoCreate(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()

	res, _, err := proposeTodoCreateHandler(md)(ctx, nil, proposeTodoCreateArgs{
		Title: "P2D新待办TC", Rationale: "用户要求",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)
	if p.Kind != "todo_create" || p.TargetID != nil {
		t.Fatalf("todo_create 提议异常(应无 target_id): %+v", p)
	}

	code, p1 := postProposal(t, pd, p.ID, "confirm")
	if code != http.StatusOK || p1.Status != "applied" || p1.AppliedRef == nil {
		t.Fatalf("confirm code=%d status=%s ref=%v", code, p1.Status, p1.AppliedRef)
	}
	t.Cleanup(func() { _, _ = pd.DB.Exec("DELETE FROM todo WHERE id = ?", p1.AppliedRef.Int64()) })
	td, err := md.Todo.Get(ctx, *p1.AppliedRef)
	if err != nil {
		t.Fatalf("新 todo 应存在: %v", err)
	}
	if td.Title != "P2D新待办TC" || td.Status != "confirmed" {
		t.Errorf("新 todo 异常: title=%q status=%q", td.Title, td.Status)
	}
}

// TestConfirmTodoStatusIllegalTransition 锁定 I1：闸门确认也须守 todo 状态机——
// dismissed→confirmed 非法, 确认应失败且 todo 不变、提议仍 pending（不被静默 applied）。
func TestConfirmTodoStatusIllegalTransition(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	td := &repo.Todo{Title: "已放弃待办IT", Status: "dismissed", Confidence: 1} // 终态
	if err := pd.Todos.InsertExt(ctx, pd.DB, []*repo.Todo{td}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pd.DB.Exec("DELETE FROM todo WHERE id = ?", td.ID.Int64()) })

	res, _, err := proposeTodoStatusHandler(md)(ctx, nil, proposeTodoStatusArgs{
		TodoID: td.ID.String(), NewStatus: "confirmed",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)

	code, _ := postProposal(t, pd, p.ID, "confirm")
	if code == http.StatusOK {
		t.Fatalf("dismissed→confirmed 非法流转不应确认成功(code=%d)", code)
	}
	got, _ := md.Todo.Get(ctx, td.ID)
	if got.Status != "dismissed" {
		t.Errorf("非法确认不应改 todo, status=%q", got.Status)
	}
	after, _ := pd.Proposals.Get(ctx, p.ID)
	if after.Status != "pending" {
		t.Errorf("非法确认后提议应仍 pending, got %q", after.Status)
	}
}
