package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
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
// P2 起同时装配画像 repo + profile.Service（供 get_profile/get_person 读工具、
// propose_profile_* 读现值、以及 confirm 时经 ManualAdd*Ext 落库）。
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
	// 画像 repo + Service（manual 路径只用 DB/Attributes/Events/Metrics/ChangeLogs/Persons）
	persons := &repo.PersonRepo{DB: db}
	pattrs := &repo.PersonAttributeRepo{DB: db}
	pevents := &repo.PersonEventRepo{DB: db}
	pmetrics := &repo.PersonMetricRepo{DB: db} // 第 5 平面：时序个人指标（get_metrics 读 + confirm 落库）
	profileSvc := &profile.Service{
		DB: db, Persons: persons, Attributes: pattrs, Events: pevents,
		Metrics:       pmetrics, // confirm profile_metric 时经 ManualAddMetricExt 落库需此 repo
		Relationships: &repo.PersonRelationshipRepo{DB: db},
		ChangeLogs:    &repo.PersonChangeLogRepo{DB: db},
	}
	md := MCPDeps{
		Memory: mem, Topic: top, Todo: tod, Proposals: pr,
		Persons: persons, PersonAttributes: pattrs, PersonEvents: pevents,
		PersonMetrics: pmetrics, // get_metrics 读工具依赖
	}
	pd := ProposalDeps{
		DB: db, Proposals: pr, Memories: mem, Topics: top, Todos: tod, TodoTopics: tt,
		Profile: profileSvc, Persons: persons,
	}
	return md, pd
}

// ensureOwner 保证画像 owner「我」存在：已存在则复用（不清理，属公共 bootstrap 数据）；
// 本用例首次创建则登记 t.Cleanup 删除（只清理自己插入的行，符合共享库约定）。
func ensureOwner(t *testing.T, persons *repo.PersonRepo) *repo.Person {
	t.Helper()
	ctx := t.Context()
	o, err := persons.GetOwner(ctx, toolUserID)
	if err != nil {
		t.Fatal(err)
	}
	if o != nil {
		return o
	}
	np := &repo.Person{DisplayName: "我", IsOwner: true}
	if err := persons.Create(ctx, np); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = persons.DB.Exec("DELETE FROM person WHERE id = ?", np.ID.Int64()) })
	return np
}

// postProposal 经真 HTTP 路由 confirm/dismiss 一条提议, 返回状态码与响应体解析出的提议。
// 注入 uid=1（模拟 authGate；测试里的提议默认归 user 1），使 2B-B 的鉴权 + IDOR 归属校验放行。
func postProposal(t *testing.T, pd ProposalDeps, id ids.ID, action string) (int, repo.AgentProposal) {
	t.Helper()
	return postProposalAs(t, pd, id, action, 1)
}

// postProposalAs 同 postProposal，但可指定注入的 uid（模拟不同登录用户）。C1 端到端隔离用例
// 用它以 uid=2 确认自己的提议、以 uid=1 确认他人的提议断言 404。
func postProposalAs(t *testing.T, pd ProposalDeps, id ids.ID, action string, uid int64) (int, repo.AgentProposal) {
	t.Helper()
	r := chi.NewRouter()
	r.Use(injectUser(uid))
	RegisterProposals(r, pd)
	req := httptest.NewRequest("POST", "/api/agent/proposals/"+id.String()+"/"+action, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var p repo.AgentProposal
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p
}

// listPendingHas 断言某用户的 ListPending 是否含指定提议 id（C1 隔离用例的读侧断言）。
func listPendingHas(t *testing.T, pd ProposalDeps, userID int64, id ids.ID) bool {
	t.Helper()
	rows, err := pd.Proposals.ListPending(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
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

// TestProposalIDORCrossUser 锁定 2B-B 的 IDOR 归属校验：某用户不能 confirm/dismiss 他人的提议。
// 造一条归 user 2 的提议，以 user 1（postProposal 注入的身份）confirm/dismiss → 均 404，
// 且提议仍 pending（越权尝试绝不改其状态、绝不经 applyInTx 落库到他人数据）。
func TestProposalIDORCrossUser(t *testing.T) {
	_, pd := p2dDeps(t)
	ctx := t.Context()

	// 归 user 2 的提议（直接建行，UserID=2 显式，Create 尊重非 0 值）。
	p := &repo.AgentProposal{UserID: 2, Kind: "todo_create", TargetKind: "todo", Rationale: "他人的提议IDOR"}
	if err := pd.Proposals.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	cleanupProposal(t, pd, p.ID)

	// user 1 尝试 confirm / dismiss → 均按「不存在」404（不泄露存在性）。
	if code, _ := postProposal(t, pd, p.ID, "confirm"); code != http.StatusNotFound {
		t.Errorf("跨用户 confirm 应 404, got %d", code)
	}
	if code, _ := postProposal(t, pd, p.ID, "dismiss"); code != http.StatusNotFound {
		t.Errorf("跨用户 dismiss 应 404, got %d", code)
	}
	// 越权尝试后，提议必须仍是 pending（未被处理）。
	got, err := pd.Proposals.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Errorf("越权尝试后提议应仍 pending, got %q", got.Status)
	}
}

// TestProposalUserIsolationEndToEnd 是 C1 的回归护栏（经真实 propose handler，非手工建行）：
// 现有 TestProposalIDORCrossUser 手工 new AgentProposal{UserID:2} 绕过了 handler，无法守护
// 「handler 是否把 UserID 设成发起用户」——而这正是 C1 的根 bug（handler 从不设 UserID，全部误挂
// user 1）。本用例经 proposeTodoCreateHandler(md, 2) 建提议，断言：
//
//	① 提议确实归属 user 2（而非误挂 user 1）；
//	② ListPending(1) 不含它、ListPending(2) 含它（读侧租户隔离）；
//	③ user 1 confirm 它 → 404 且提议仍 pending（跨租户写被挡，不经 applyInTx 落库）；
//	④ user 2（owner）confirm 它 → 200 applied（owner 功能未因 C1 修复受损）。
func TestProposalUserIsolationEndToEnd(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	const otherUID = int64(2)

	// 经真实 handler 以 userID=2 建提议。todo_create 不读任何用户域数据，最干净地隔离 UserID 注入这一点。
	res, _, err := proposeTodoCreateHandler(md, otherUID)(ctx, nil, proposeTodoCreateArgs{
		Title: "user2的待办ISO", Rationale: "隔离回归",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	if err := json.Unmarshal([]byte(mcpText(t, res)), &p); err != nil {
		t.Fatalf("解析提议: %v", err)
	}
	cleanupProposal(t, pd, p.ID)

	// ① 提议归属 user 2（C1 核心：handler 必须把 UserID 设成发起用户，不再误挂 1）
	if p.UserID != otherUID {
		t.Fatalf("提议应归属 user 2, got UserID=%d", p.UserID)
	}

	// ② ListPending 租户隔离
	if listPendingHas(t, pd, 1, p.ID) {
		t.Error("user1 的 ListPending 不应含 user2 的提议（跨租户泄漏）")
	}
	if !listPendingHas(t, pd, otherUID, p.ID) {
		t.Error("user2 的 ListPending 应含自己的提议")
	}

	// ③ 跨用户 confirm：user1 确认 user2 的提议 → 404，且提议仍 pending（未 applied、不落 todo）
	if code, _ := postProposalAs(t, pd, p.ID, "confirm", 1); code != http.StatusNotFound {
		t.Errorf("user1 跨用户 confirm 应 404, got %d", code)
	}
	if after, _ := pd.Proposals.Get(ctx, p.ID); after.Status != "pending" {
		t.Errorf("跨用户 confirm 后提议应仍 pending, got %q", after.Status)
	}

	// ④ owner confirm：user2 确认自己的提议 → 200 applied
	code, p1 := postProposalAs(t, pd, p.ID, "confirm", otherUID)
	if code != http.StatusOK || p1.Status != "applied" {
		t.Fatalf("owner(user2) confirm 应成功, code=%d status=%s", code, p1.Status)
	}
	if p1.AppliedRef != nil { // 清理 confirm 落库的 todo
		t.Cleanup(func() { _, _ = pd.DB.Exec("DELETE FROM todo WHERE id = ?", p1.AppliedRef.Int64()) })
	}
	// 落库的 todo 必须归属 user2（Todo.Get 带 user 过滤：若被误挂 user1 则此处读不到）。
	// 守护 applyInTx 的 todo_create 分支已把新待办 UserID 设为 p.UserID（否则 InsertExt 默认落 1）。
	if p1.AppliedRef == nil {
		t.Fatal("owner confirm 应回填 applied_ref")
	}
	if td, err := md.Todo.Get(ctx, otherUID, *p1.AppliedRef); err != nil || td.Title != "user2的待办ISO" {
		t.Errorf("confirm 落库的 todo 应归属 user2, err=%v td=%+v", err, td)
	}
}

// TestApplyInTxRejectsCrossUserTarget 锁定 C1 纵深防御（applyInTx 复核 target 归属）：即便一条提议
// 被误 attribution——UserID 与其 TargetID 归属不一致（如提议归 user1 却指向 user2 的 memory）——
// confirm 也必须在 applyInTx 内按 p.UserID 复核 target 归属并中止，绝不跨写他人数据。此前 applyInTx
// 用不带 user 过滤的 GetExt 落库，误 attribution 会直接演变成跨用户写（C1 的「条件跨写」一症）。
func TestApplyInTxRejectsCrossUserTarget(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()

	// user 2 拥有的 memory
	m := &repo.Memory{
		UserID: 2, Type: "fact", Title: "user2记忆XW", Content: "user2内容XW",
		EpistemicType: "observed", Confidence: 0.8, Status: "active", TranscriptSegmentIDs: ids.List{},
	}
	if err := md.Memory.InsertExt(ctx, md.Memory.DB, []*repo.Memory{m}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = md.Memory.DB.Exec("DELETE FROM memory WHERE id = ?", m.ID.Int64()) })

	// 误 attribution 的提议：归 user 1，却指向 user 2 的 memory（模拟未来不 gate 的提议源 / 篡改）。
	// 手工建行以精确构造这一非法组合（正常 propose handler 已 gate，产生不了它）。
	p := &repo.AgentProposal{
		UserID: 1, Kind: "memory_update", TargetKind: "memory", TargetID: &m.ID,
		Payload: json.RawMessage(`{"new":{"title":"跨写标题XW"}}`), Rationale: "跨写尝试",
	}
	if err := pd.Proposals.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	cleanupProposal(t, pd, p.ID)

	// user 1 confirm：主 IDOR 校验放行(p.UserID==1==uid)，但 applyInTx 须复核 target 归属 → 中止
	code, _ := postProposalAs(t, pd, p.ID, "confirm", 1)
	if code == http.StatusOK {
		t.Fatalf("误 attribution 提议不应确认成功(会跨写 user2 的 memory), code=%d", code)
	}
	// 关键：user 2 的 memory 未被跨写
	got, err := md.Memory.Get(ctx, 2, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "user2记忆XW" {
		t.Errorf("跨用户 target 不应被写, title=%q", got.Title)
	}
	// 提议仍 pending（未 applied）
	if after, _ := pd.Proposals.Get(ctx, p.ID); after.Status != "pending" {
		t.Errorf("中止后提议应仍 pending, got %q", after.Status)
	}
}

// TestProposeMemoryEditNoMutation 锁定 §8 根防线：propose_* 只建 pending 提议, 绝不改领域行。
func TestProposeMemoryEditNoMutation(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	m := seedMemory(t, md.Memory, "原标题NM", "原内容NM")
	base, err := md.Memory.Get(ctx, 1, m.ID) // 读库基线（version 由 DB 默认赋值, 非内存 m 的零值）
	if err != nil {
		t.Fatal(err)
	}

	res, _, err := proposeMemoryEditHandler(md, toolUserID)(ctx, nil, proposeMemoryEditArgs{
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
	got, err := md.Memory.Get(ctx, 1, m.ID)
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
	before, _ := md.Memory.Get(ctx, 1, m.ID)

	res, _, err := proposeMemoryEditHandler(md, toolUserID)(ctx, nil, proposeMemoryEditArgs{
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
	got, _ := md.Memory.Get(ctx, 1, m.ID)
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
	got2, _ := md.Memory.Get(ctx, 1, m.ID)
	if got2.Version != verAfter {
		t.Errorf("重复确认不应再改 version: %d → %d", verAfter, got2.Version)
	}
}

// TestDismissProposal 锁定放弃：目标行不变, 提议置 dismissed。
func TestDismissProposal(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	m := seedMemory(t, md.Memory, "放弃标题DZ", "放弃内容DZ")

	res, _, err := proposeMemoryEditHandler(md, toolUserID)(ctx, nil, proposeMemoryEditArgs{MemoryID: m.ID.String(), NewTitle: "不该生效DZ"})
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
	got, _ := md.Memory.Get(ctx, 1, m.ID)
	if got.Title != "放弃标题DZ" {
		t.Errorf("放弃不应改 memory: title=%q", got.Title)
	}
}

// TestConfirmTodoCreate 锁定 todo_create：确认后 todo 表新增一行, applied_ref 指向新待办。
func TestConfirmTodoCreate(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()

	res, _, err := proposeTodoCreateHandler(md, toolUserID)(ctx, nil, proposeTodoCreateArgs{
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
	td, err := md.Todo.Get(ctx, 1, *p1.AppliedRef)
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

	res, _, err := proposeTodoStatusHandler(md, toolUserID)(ctx, nil, proposeTodoStatusArgs{
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
	got, _ := md.Todo.Get(ctx, 1, td.ID)
	if got.Status != "dismissed" {
		t.Errorf("非法确认不应改 todo, status=%q", got.Status)
	}
	after, _ := pd.Proposals.Get(ctx, p.ID)
	if after.Status != "pending" {
		t.Errorf("非法确认后提议应仍 pending, got %q", after.Status)
	}
}

// ---- P2 画像工具（读 + propose + confirm 落库）----

func profHasAttr(p profileOut, key, value string) bool {
	for _, a := range p.Attributes {
		if a.Key == key && a.Value == value {
			return true
		}
	}
	return false
}

func profHasEvent(p profileOut, title string) bool {
	for _, e := range p.Events {
		if e.Title == title {
			return true
		}
	}
	return false
}

// TestGetProfileAndPerson 锁定读工具：get_profile 返回 owner 及其 active 属性/事件；
// get_person 命中具名人物 / 不命中返回 {found:false}。
func TestGetProfileAndPerson(t *testing.T) {
	md, _ := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	// seed owner 一条独占属性 + 一条独占事件（用完按 id 精确清理）
	attr := &repo.PersonAttribute{PersonID: owner.ID, AttrKey: "occupation", ValueText: "读工具职业GP",
		ValueType: "text", Status: "active", Source: "manual", Confidence: 1}
	if err := md.PersonAttributes.Create(ctx, attr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = md.PersonAttributes.DB.Exec("DELETE FROM person_attribute WHERE id = ?", attr.ID.Int64())
	})
	ev := &repo.PersonEvent{PersonID: owner.ID, EventType: "里程碑", Title: "读工具事件GP",
		Status: "active", Source: "manual", Confidence: 1, Importance: 1}
	if err := md.PersonEvents.Create(ctx, ev); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = md.PersonEvents.DB.Exec("DELETE FROM person_event WHERE id = ?", ev.ID.Int64()) })

	// get_profile：含 owner + seed 的属性/事件
	res, _, err := getProfileHandler(md, toolUserID)(ctx, nil, getProfileArgs{})
	if err != nil {
		t.Fatalf("get_profile: %v", err)
	}
	var prof profileOut
	if err := json.Unmarshal([]byte(mcpText(t, res)), &prof); err != nil {
		t.Fatalf("解析 get_profile: %v", err)
	}
	if !prof.Found || prof.DisplayName == "" {
		t.Fatalf("get_profile 应返回存在的 owner: %+v", prof)
	}
	if !profHasAttr(prof, "occupation", "读工具职业GP") {
		t.Errorf("get_profile 缺 seed 属性: %+v", prof.Attributes)
	}
	if !profHasEvent(prof, "读工具事件GP") {
		t.Errorf("get_profile 缺 seed 事件: %+v", prof.Events)
	}

	// get_person：建一个具名人物 + 属性，命中
	person := &repo.Person{DisplayName: "画像测试人物GP", IsOwner: false}
	if err := md.Persons.Create(ctx, person); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = md.Persons.DB.Exec("DELETE FROM person WHERE id = ?", person.ID.Int64()) })
	pattr := &repo.PersonAttribute{PersonID: person.ID, AttrKey: "city", ValueText: "上海GP",
		ValueType: "text", Status: "active", Source: "manual", Confidence: 1}
	if err := md.PersonAttributes.Create(ctx, pattr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = md.PersonAttributes.DB.Exec("DELETE FROM person_attribute WHERE id = ?", pattr.ID.Int64())
	})

	res2, _, err := getPersonHandler(md, toolUserID)(ctx, nil, getPersonArgs{Name: "画像测试人物GP"})
	if err != nil {
		t.Fatalf("get_person 命中: %v", err)
	}
	var got profileOut
	if err := json.Unmarshal([]byte(mcpText(t, res2)), &got); err != nil {
		t.Fatalf("解析 get_person: %v", err)
	}
	if !got.Found || got.DisplayName != "画像测试人物GP" || !profHasAttr(got, "city", "上海GP") {
		t.Errorf("get_person 命中结果异常: %+v", got)
	}

	// get_person 不命中 → {found:false}
	res3, _, err := getPersonHandler(md, toolUserID)(ctx, nil, getPersonArgs{Name: "查无此人ZZZ"})
	if err != nil {
		t.Fatalf("get_person 不命中: %v", err)
	}
	var miss map[string]any
	if err := json.Unmarshal([]byte(mcpText(t, res3)), &miss); err != nil {
		t.Fatal(err)
	}
	if f, _ := miss["found"].(bool); f {
		t.Errorf("不存在人物应 found:false, got %+v", miss)
	}
}

// TestProposeProfileAttrNoMutation 锁定 §8 根防线：propose_profile_attr 只建 pending 提议，
// 绝不改 owner 画像；非法 attr_key / event_type / 空 title 报 tool-error。
func TestProposeProfileAttrNoMutation(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	const key = "phone_brand" // 单值、其它用例未用，避免共享库串扰
	seed := &repo.PersonAttribute{PersonID: owner.ID, AttrKey: key, ValueText: "小米NM",
		ValueType: "text", Status: "active", Source: "manual", Confidence: 1}
	if err := md.PersonAttributes.Create(ctx, seed); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = md.PersonAttributes.DB.Exec("DELETE FROM person_attribute WHERE person_id = ? AND attr_key = ?", owner.ID.Int64(), key)
	})

	res, _, err := proposeProfileAttrHandler(md, toolUserID)(ctx, nil, proposeProfileAttrArgs{
		AttrKey: key, Value: "华为NM", Rationale: "换手机了",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	if err := json.Unmarshal([]byte(mcpText(t, res)), &p); err != nil {
		t.Fatalf("解析提议: %v", err)
	}
	cleanupProposal(t, pd, p.ID)
	if p.Status != "pending" || p.Kind != "profile_attr" || p.TargetKind != "profile" || p.TargetID == nil || *p.TargetID != owner.ID {
		t.Fatalf("画像属性提议异常: %+v", p)
	}
	// 关键：owner 属性现值未变（仍是 seed 的小米NM，未 supersede）
	cur, err := md.PersonAttributes.FindActiveByKey(ctx, owner.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	if cur == nil || cur.ValueText != "小米NM" || cur.ID != seed.ID {
		t.Errorf("propose 不应改画像属性, got %+v", cur)
	}
	if old, _ := md.PersonAttributes.Get(ctx, seed.ID); old == nil || old.Status != "active" {
		t.Errorf("seed 行应仍 active（未被 supersede）: %+v", old)
	}

	// 非法 attr_key → tool-error
	if _, _, e := proposeProfileAttrHandler(md, toolUserID)(ctx, nil, proposeProfileAttrArgs{AttrKey: "不存在的键xx", Value: "x"}); e == nil {
		t.Error("非法 attr_key 应报 tool-error")
	}
	// 空 value → tool-error
	if _, _, e := proposeProfileAttrHandler(md, toolUserID)(ctx, nil, proposeProfileAttrArgs{AttrKey: key, Value: "   "}); e == nil {
		t.Error("空 value 应报 tool-error")
	}
	// 非法 event_type → tool-error
	if _, _, e := proposeProfileEventHandler(md, toolUserID)(ctx, nil, proposeProfileEventArgs{EventType: "不存在类型", Title: "x"}); e == nil {
		t.Error("非法 event_type 应报 tool-error")
	}
	// 空 title → tool-error
	if _, _, e := proposeProfileEventHandler(md, toolUserID)(ctx, nil, proposeProfileEventArgs{EventType: "里程碑", Title: "  "}); e == nil {
		t.Error("空 title 应报 tool-error")
	}
}

// TestConfirmProfileAttrApplyOnce 锁定 §8 apply-once：propose_profile_attr → confirm → owner
// 多一条 active 属性、proposal=applied、applied_ref 指向新属性；重复 confirm 幂等不重复叠加。
func TestConfirmProfileAttrApplyOnce(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	const key = "car_brand" // 单值、其它用例未用
	// 先登记清理：confirm 会经 ManualAddAttributeExt 在 owner 上新建该 key 的 active 行 + 审计。
	t.Cleanup(func() {
		_, _ = md.PersonAttributes.DB.Exec("DELETE FROM person_attribute WHERE person_id = ? AND attr_key = ?", owner.ID.Int64(), key)
		_, _ = md.PersonAttributes.DB.Exec("DELETE FROM person_change_log WHERE person_id = ? AND attr_key = ?", owner.ID.Int64(), key)
	})

	res, _, err := proposeProfileAttrHandler(md, toolUserID)(ctx, nil, proposeProfileAttrArgs{AttrKey: key, Value: "特斯拉AO"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)

	code, p1 := postProposal(t, pd, p.ID, "confirm")
	if code != http.StatusOK || p1.Status != "applied" || p1.AppliedRef == nil {
		t.Fatalf("confirm code=%d status=%s ref=%v", code, p1.Status, p1.AppliedRef)
	}
	// owner 多一条 active 属性 = 特斯拉AO，source=manual conf=1.0，applied_ref 指向它
	cur, _ := md.PersonAttributes.FindActiveByKey(ctx, owner.ID, key)
	if cur == nil || cur.ValueText != "特斯拉AO" || cur.Source != "manual" || cur.Confidence != 1.0 {
		t.Fatalf("confirm 未落库画像属性: %+v", cur)
	}
	if *p1.AppliedRef != cur.ID {
		t.Errorf("applied_ref 应指向新属性: ref=%v attr=%v", p1.AppliedRef, cur.ID)
	}

	// 重复 confirm：apply-once → 幂等，不新增 active 行
	code2, p2 := postProposal(t, pd, p.ID, "confirm")
	if code2 != http.StatusOK || p2.Status != "applied" {
		t.Fatalf("2nd confirm code=%d status=%s", code2, p2.Status)
	}
	var activeCnt int
	if err := md.PersonAttributes.DB.GetContext(ctx, &activeCnt,
		`SELECT COUNT(*) FROM person_attribute WHERE person_id = ? AND attr_key = ? AND status = 'active'`,
		owner.ID.Int64(), key); err != nil {
		t.Fatal(err)
	}
	if activeCnt != 1 {
		t.Errorf("重复 confirm 不应叠加, 期望 1 个 active 得 %d", activeCnt)
	}
}

// TestConfirmProfileEvent 锁定：propose_profile_event → confirm → owner 新增一条 active 大事记。
func TestConfirmProfileEvent(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	const title = "确认大事记CE"
	t.Cleanup(func() {
		_, _ = md.PersonEvents.DB.Exec("DELETE FROM person_event WHERE person_id = ? AND title = ?", owner.ID.Int64(), title)
		_, _ = md.PersonEvents.DB.Exec("DELETE FROM person_change_log WHERE person_id = ? AND entity_kind = 'event'", owner.ID.Int64())
	})

	res, _, err := proposeProfileEventHandler(md, toolUserID)(ctx, nil, proposeProfileEventArgs{
		EventType: "旅行", Title: title, OccurredAt: "2026-05-01", Rationale: "去了趟三亚",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)
	if p.Kind != "profile_event" || p.TargetID == nil || *p.TargetID != owner.ID {
		t.Fatalf("大事记提议异常: %+v", p)
	}

	code, p1 := postProposal(t, pd, p.ID, "confirm")
	if code != http.StatusOK || p1.Status != "applied" || p1.AppliedRef == nil {
		t.Fatalf("confirm code=%d status=%s ref=%v", code, p1.Status, p1.AppliedRef)
	}
	ev, err := md.PersonEvents.Get(ctx, *p1.AppliedRef)
	if err != nil || ev == nil {
		t.Fatalf("新事件应存在: %v %+v", err, ev)
	}
	if ev.Title != title || ev.EventType != "旅行" || ev.Status != "active" || ev.Source != "manual" || ev.OccurredAt == nil {
		t.Errorf("新事件字段异常: %+v", ev)
	}
}

// ---- P2 画像关系提议（propose_profile_relationship + confirm 解析或新建关联人）----

// countActiveRelsByLabel 统计带指定 label 的 active 关系条数（用独占 label 精确圈定本用例产生的行，
// 免受共享库里 owner 既有关系串扰）。
func countActiveRelsByLabel(t *testing.T, pd ProposalDeps, label string) int {
	t.Helper()
	var n int
	if err := pd.DB.GetContext(t.Context(), &n,
		`SELECT COUNT(*) FROM person_relationship WHERE label = ? AND status = 'active'`, label); err != nil {
		t.Fatal(err)
	}
	return n
}

// countPersonsByName 统计指定显示名的人物条数（验证 confirm 复用已有人物 / 只新建一个）。
func countPersonsByName(t *testing.T, pd ProposalDeps, name string) int {
	t.Helper()
	var n int
	if err := pd.DB.GetContext(t.Context(), &n,
		`SELECT COUNT(*) FROM person WHERE display_name = ?`, name); err != nil {
		t.Fatal(err)
	}
	return n
}

// cleanupRel 登记关系 + 人物的精确清理：按独占 label 删关系（含其 relationship 审计），
// 按显示名删本用例新建的人物（含其 person 审计）。用 JOIN 定位审计行，只清自己插入的数据。
func cleanupRel(t *testing.T, pd ProposalDeps, label, personName string) {
	t.Cleanup(func() {
		_, _ = pd.DB.Exec(
			`DELETE l FROM person_change_log l JOIN person_relationship r ON l.entity_id = r.id
			 WHERE l.entity_kind = 'relationship' AND r.label = ?`, label)
		_, _ = pd.DB.Exec(`DELETE FROM person_relationship WHERE label = ?`, label)
		if personName != "" {
			_, _ = pd.DB.Exec(
				`DELETE l FROM person_change_log l JOIN person p ON l.person_id = p.id WHERE p.display_name = ?`, personName)
			_, _ = pd.DB.Exec(`DELETE FROM person WHERE display_name = ?`, personName)
		}
	})
}

// TestProposeProfileRelationshipNoMutation 锁定 §8 根防线：propose_profile_relationship 只建
// pending 提议，绝不写 person/relationship 表；非法 relation_type、两名皆空 → tool-error。
func TestProposeProfileRelationshipNoMutation(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	const relatedName = "关系无变更关联人RNM"     // 独占名：propose 绝不应新建它
	const label = "关系无变更标签RNM"            // 独占 label：propose 绝不应产生带此 label 的关系
	cleanupRel(t, pd, label, relatedName) // 兜底清理（即便断言失败提前退出）

	// 前置：该关联人此刻不存在
	if before, _ := md.Persons.FindByName(ctx, toolUserID, relatedName); before != nil {
		t.Fatalf("前置：关联人不应已存在: %+v", before)
	}

	res, _, err := proposeProfileRelationshipHandler(md, toolUserID)(ctx, nil, proposeProfileRelationshipArgs{
		RelationType: "朋友", RelatedPersonName: relatedName, Label: label, Rationale: "老朋友",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	if err := json.Unmarshal([]byte(mcpText(t, res)), &p); err != nil {
		t.Fatalf("解析提议: %v", err)
	}
	cleanupProposal(t, pd, p.ID)
	if p.Status != "pending" || p.Kind != "profile_relationship" || p.TargetKind != "profile" || p.TargetID == nil || *p.TargetID != owner.ID {
		t.Fatalf("关系提议异常: %+v", p)
	}
	// 关键：propose 未新建关联人、未写任何关系
	if after, _ := md.Persons.FindByName(ctx, toolUserID, relatedName); after != nil {
		t.Errorf("propose 不应新建关联人, got %+v", after)
	}
	if n := countActiveRelsByLabel(t, pd, label); n != 0 {
		t.Errorf("propose 不应写关系, 带此 label 的 active 关系应为 0 得 %d", n)
	}

	// 非法 relation_type → tool-error
	if _, _, e := proposeProfileRelationshipHandler(md, toolUserID)(ctx, nil, proposeProfileRelationshipArgs{
		RelationType: "不存在的关系", RelatedPersonName: relatedName,
	}); e == nil {
		t.Error("非法 relation_type 应报 tool-error")
	}
	// related_person_name 与 org_name 皆空 → tool-error
	if _, _, e := proposeProfileRelationshipHandler(md, toolUserID)(ctx, nil, proposeProfileRelationshipArgs{
		RelationType: "朋友",
	}); e == nil {
		t.Error("两名皆空应报 tool-error")
	}
	// 非法 direction → tool-error
	if _, _, e := proposeProfileRelationshipHandler(md, toolUserID)(ctx, nil, proposeProfileRelationshipArgs{
		RelationType: "同事", RelatedPersonName: relatedName, Direction: "sideways",
	}); e == nil {
		t.Error("非法 direction 应报 tool-error")
	}
}

// TestConfirmProfileRelationshipResolveExisting 锁定 D1：related_person_name 命中已有人物 →
// confirm 后 owner 多一条 active 关系指向该人物、proposal=applied、不新建重名人物。
func TestConfirmProfileRelationshipResolveExisting(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	const relatedName = "关系已有关联人RE"
	const label = "关系已有标签RE"
	cleanupRel(t, pd, label, relatedName)
	// 预置已有人物（active）；confirm 应复用它、不新建
	related := &repo.Person{DisplayName: relatedName, IsOwner: false}
	if err := md.Persons.Create(ctx, related); err != nil {
		t.Fatal(err)
	}

	res, _, err := proposeProfileRelationshipHandler(md, toolUserID)(ctx, nil, proposeProfileRelationshipArgs{
		RelationType: "同事", RelatedPersonName: relatedName, Direction: "peer", Label: label,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)

	code, p1 := postProposal(t, pd, p.ID, "confirm")
	if code != http.StatusOK || p1.Status != "applied" || p1.AppliedRef == nil {
		t.Fatalf("confirm code=%d status=%s ref=%v", code, p1.Status, p1.AppliedRef)
	}
	// applied_ref 指向的关系：owner→related、active/manual、指向既有人物、方向 peer
	rel, err := pd.Profile.Relationships.Get(ctx, *p1.AppliedRef)
	if err != nil || rel == nil {
		t.Fatalf("关系应存在: %v %+v", err, rel)
	}
	if rel.PersonID != owner.ID || rel.RelationType != "同事" || rel.Status != "active" || rel.Source != "manual" {
		t.Fatalf("关系字段异常: %+v", rel)
	}
	if rel.RelatedPersonID == nil || *rel.RelatedPersonID != related.ID {
		t.Fatalf("关系应指向既有人物 %d, got %+v", related.ID, rel.RelatedPersonID)
	}
	// 不应新建重名人物（仍只有预置的那 1 个）
	if n := countPersonsByName(t, pd, relatedName); n != 1 {
		t.Errorf("不应新建重名人物, 期望 1 得 %d", n)
	}
}

// TestConfirmProfileRelationshipResolveCreate 锁定 D1：related_person_name 未命中 → confirm 在
// 同事务内新建该人物（active/manual）+ 关系；重复 confirm apply-once（不重复建人/建关系）。
func TestConfirmProfileRelationshipResolveCreate(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	const relatedName = "关系新建关联人RC"
	const label = "关系新建标签RC"
	cleanupRel(t, pd, label, relatedName)
	// 前置：该人此刻不存在
	if before, _ := md.Persons.FindByName(ctx, toolUserID, relatedName); before != nil {
		t.Fatalf("前置：关联人不应已存在: %+v", before)
	}

	res, _, err := proposeProfileRelationshipHandler(md, toolUserID)(ctx, nil, proposeProfileRelationshipArgs{
		RelationType: "朋友", RelatedPersonName: relatedName, Label: label, Rationale: "新朋友",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)

	code, p1 := postProposal(t, pd, p.ID, "confirm")
	if code != http.StatusOK || p1.Status != "applied" || p1.AppliedRef == nil {
		t.Fatalf("confirm code=%d status=%s ref=%v", code, p1.Status, p1.AppliedRef)
	}
	// 新建了关联人（active/manual）
	created, err := md.Persons.FindByName(ctx, toolUserID, relatedName)
	if err != nil || created == nil {
		t.Fatalf("confirm 应新建关联人: %v %+v", err, created)
	}
	if created.Status != "active" || created.Source != "manual" {
		t.Errorf("新建关联人应 active/manual: %+v", created)
	}
	// 关系指向新建的人物
	rel, err := pd.Profile.Relationships.Get(ctx, *p1.AppliedRef)
	if err != nil || rel == nil || rel.PersonID != owner.ID || rel.RelatedPersonID == nil || *rel.RelatedPersonID != created.ID {
		t.Fatalf("关系应指向新建关联人: rel=%+v created=%d", rel, created.ID)
	}

	// 重复 confirm：apply-once → 幂等，不重复建人 / 建关系
	code2, p2 := postProposal(t, pd, p.ID, "confirm")
	if code2 != http.StatusOK || p2.Status != "applied" {
		t.Fatalf("2nd confirm code=%d status=%s", code2, p2.Status)
	}
	if n := countPersonsByName(t, pd, relatedName); n != 1 {
		t.Errorf("重复 confirm 不应重复建人, 期望 1 得 %d", n)
	}
	if n := countActiveRelsByLabel(t, pd, label); n != 1 {
		t.Errorf("重复 confirm 不应重复建关系, 期望 1 得 %d", n)
	}
}

// TestConfirmProfileRelationshipOrgOnly 锁定 D1：仅 org_name（无 related_person_name）→ 组织关系，
// related_person_id 为 NULL、org_name 落库；不新建任何人物。
func TestConfirmProfileRelationshipOrgOnly(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	const orgName = "关系测试组织RO"
	const label = "关系组织标签RO"
	cleanupRel(t, pd, label, "") // 组织关系不涉及新建人物

	res, _, err := proposeProfileRelationshipHandler(md, toolUserID)(ctx, nil, proposeProfileRelationshipArgs{
		RelationType: "组织", OrgName: orgName, Direction: "upstream", Label: label,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)

	code, p1 := postProposal(t, pd, p.ID, "confirm")
	if code != http.StatusOK || p1.Status != "applied" || p1.AppliedRef == nil {
		t.Fatalf("confirm code=%d status=%s ref=%v", code, p1.Status, p1.AppliedRef)
	}
	rel, err := pd.Profile.Relationships.Get(ctx, *p1.AppliedRef)
	if err != nil || rel == nil {
		t.Fatalf("组织关系应存在: %v %+v", err, rel)
	}
	if rel.PersonID != owner.ID || rel.RelationType != "组织" || rel.Status != "active" {
		t.Fatalf("组织关系字段异常: %+v", rel)
	}
	if rel.RelatedPersonID != nil {
		t.Errorf("组织关系 related_person_id 应为 NULL, got %v", rel.RelatedPersonID)
	}
	if rel.OrgName == nil || *rel.OrgName != orgName {
		t.Errorf("组织关系 org_name 未落库: %+v", rel.OrgName)
	}
}

// ---- P2 画像指标提议（propose_profile_metric + confirm 落库；第 5 平面 person_metric）----

// countActiveMetrics 统计某主体某指标键的 active 测点条数。metric 无独占 label 可用，
// 故用 before/after 差值圈定本用例产生的行（免受共享库既有测点串扰）。
func countActiveMetrics(t *testing.T, pd ProposalDeps, personID ids.ID, metricKey string) int {
	t.Helper()
	var n int
	if err := pd.DB.GetContext(t.Context(), &n,
		`SELECT COUNT(*) FROM person_metric WHERE person_id = ? AND metric_key = ? AND status = 'active'`,
		personID.Int64(), metricKey); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestProposeProfileMetricNoMutation 锁定 §8 根防线：propose_profile_metric 只建 pending 提议，
// 绝不写 person_metric；非法 metric_key、数值键缺 value_num、类别键缺 value_text → tool-error。
func TestProposeProfileMetricNoMutation(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	before := countActiveMetrics(t, pd, owner.ID, "weight") // 基线（差值法，免受共享库既有测点串扰）

	vn := 70.0
	res, _, err := proposeProfileMetricHandler(md, toolUserID)(ctx, nil, proposeProfileMetricArgs{
		MetricKey: "weight", ValueNum: &vn, Unit: "kg", Rationale: "量了体重",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	if err := json.Unmarshal([]byte(mcpText(t, res)), &p); err != nil {
		t.Fatalf("解析提议: %v", err)
	}
	cleanupProposal(t, pd, p.ID)
	if p.Status != "pending" || p.Kind != "profile_metric" || p.TargetKind != "profile" || p.TargetID == nil || *p.TargetID != owner.ID {
		t.Fatalf("指标提议异常: %+v", p)
	}
	// 关键：propose 不写 person_metric（weight 测点数未变）
	if after := countActiveMetrics(t, pd, owner.ID, "weight"); after != before {
		t.Errorf("propose 不应写 person_metric, weight 测点 %d → %d", before, after)
	}

	// 非法 metric_key → tool-error
	if _, _, e := proposeProfileMetricHandler(md, toolUserID)(ctx, nil, proposeProfileMetricArgs{MetricKey: "不存在指标xx", ValueNum: &vn}); e == nil {
		t.Error("非法 metric_key 应报 tool-error")
	}
	// 数值型指标(weight)缺 value_num → tool-error
	if _, _, e := proposeProfileMetricHandler(md, toolUserID)(ctx, nil, proposeProfileMetricArgs{MetricKey: "weight"}); e == nil {
		t.Error("数值指标缺 value_num 应报 tool-error")
	}
	// 类别型指标(diet)缺 value_text → tool-error
	if _, _, e := proposeProfileMetricHandler(md, toolUserID)(ctx, nil, proposeProfileMetricArgs{MetricKey: "diet"}); e == nil {
		t.Error("类别指标缺 value_text 应报 tool-error")
	}
}

// TestConfirmProfileMetricApplyOnce 锁定 §8 apply-once：propose_profile_metric → confirm → owner
// 新增一条 active weight 测点(value_num=70)、proposal=applied、applied_ref 指向它；重复 confirm
// 幂等——metric 虽 append-only，但第二次 confirm 因 status!=pending 早返回不再落库，故仍只 1 条。
func TestConfirmProfileMetricApplyOnce(t *testing.T) {
	md, pd := p2dDeps(t)
	ctx := t.Context()
	owner := ensureOwner(t, md.Persons)

	before := countActiveMetrics(t, pd, owner.ID, "weight")

	vn := 70.0
	res, _, err := proposeProfileMetricHandler(md, toolUserID)(ctx, nil, proposeProfileMetricArgs{
		MetricKey: "weight", ValueNum: &vn, Unit: "kg", MeasuredAt: "2026-06-15", Rationale: "体检",
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var p repo.AgentProposal
	_ = json.Unmarshal([]byte(mcpText(t, res)), &p)
	cleanupProposal(t, pd, p.ID)
	if p.Kind != "profile_metric" || p.TargetID == nil || *p.TargetID != owner.ID {
		t.Fatalf("指标提议异常: %+v", p)
	}

	code, p1 := postProposal(t, pd, p.ID, "confirm")
	if code != http.StatusOK || p1.Status != "applied" || p1.AppliedRef == nil {
		t.Fatalf("confirm code=%d status=%s ref=%v", code, p1.Status, p1.AppliedRef)
	}
	// 精确清理本用例落库的测点 + 其审计（append-only 表用 applied_ref 定位唯一行）
	t.Cleanup(func() {
		_, _ = pd.DB.Exec("DELETE FROM person_metric WHERE id = ?", p1.AppliedRef.Int64())
		_, _ = pd.DB.Exec("DELETE FROM person_change_log WHERE entity_kind = 'metric' AND entity_id = ?", p1.AppliedRef.Int64())
	})

	// applied_ref 指向的测点：weight/value_num=70/active/manual，单位 kg，测点时间已落库
	m, err := md.PersonMetrics.Get(ctx, *p1.AppliedRef)
	if err != nil || m == nil {
		t.Fatalf("新测点应存在: %v %+v", err, m)
	}
	if m.PersonID != owner.ID || m.MetricKey != "weight" || m.Status != "active" || m.Source != "manual" {
		t.Fatalf("新测点字段异常: %+v", m)
	}
	if m.ValueNum == nil || *m.ValueNum != 70 {
		t.Errorf("新测点 value_num 应为 70, got %v", m.ValueNum)
	}
	if m.Unit == nil || *m.Unit != "kg" {
		t.Errorf("新测点 unit 应为 kg, got %v", m.Unit)
	}
	if m.MeasuredAt.IsZero() {
		t.Errorf("新测点 measured_at 不应为零值")
	}
	// owner 恰好多一条 active weight 测点
	if after := countActiveMetrics(t, pd, owner.ID, "weight"); after != before+1 {
		t.Fatalf("confirm 应新增 1 条 active weight 测点, %d → %d", before, after)
	}

	// 重复 confirm：apply-once → 幂等，不再追加第二条（Resolve CAS：status!=pending 早返回）
	code2, p2 := postProposal(t, pd, p.ID, "confirm")
	if code2 != http.StatusOK || p2.Status != "applied" {
		t.Fatalf("2nd confirm code=%d status=%s", code2, p2.Status)
	}
	if after := countActiveMetrics(t, pd, owner.ID, "weight"); after != before+1 {
		t.Errorf("重复 confirm 不应再追加测点, 期望 %d 得 %d", before+1, after)
	}
}
