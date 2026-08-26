// stage_extract_test 验证 extract stage 的端到端行为：
// 聚合 → LLM 抽取（fake）→ 闸门 → Topic 归属 → 单事务提交（幂等）。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// fakeExtractLLM 固定响应（1 条 todo 候选建议新主题 + 1 条同名已有主题的候选）
type fakeExtractLLM struct{ calls int }

func (f *fakeExtractLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	f.calls++
	return provider.ChatResponse{Content: `{"candidates":[
	  {"type":"event","title":"给 Tom 发邮件","content":"明天需要给 Tom 发邮件确认设计稿",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":true,"todo_due":null,"topics":[{"suggested_name":"工作沟通（抽取fixture）"}],"block_index":1},
	  {"type":"fact","title":"学习 Rust","content":"用户正在学习 Rust 计划三个月内读完一本书",
	   "epistemic_type":"observed","importance":0.7,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topics":[{"suggested_name":"Rust 学习（抽取fixture）"}],"block_index":2}
	]}`, TotalTokens: 500}, nil
}

// newExtractDeps 构造 extract stage 依赖。llm 参数用接口类型，
// 以便复用同一套 fixture（fakeExtractLLM / driftExtractLLM 等）。
func newExtractDeps(t *testing.T, llm provider.LLMProvider) StageDeps {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	return StageDeps{
		Sessions:     &repo.SessionRepo{DB: db},
		Transcripts:  &repo.TranscriptRepo{DB: db},
		Speakers:     &repo.SpeakerRepo{DB: db},
		DB:           db,
		Memories:     &repo.MemoryRepo{DB: db},
		Todos:        &repo.TodoRepo{DB: db},
		Topics:       &repo.TopicRepo{DB: db},
		MemoryTopics: &repo.MemoryTopicRepo{DB: db},
		TodoTopics:   &repo.TodoTopicRepo{DB: db},
		LLM:          llm,
		LLMModel:     "fake-model",
		Prompt:       "测试 system prompt",
		// PromptVersion 与生产对齐（cmd/zhiwei-server/main.go 用 extraction_v2.md），
		// 避免 trace 里记的版本与线上不一致（评审 M3）。
		PromptVersion: "extraction_v2",
		ExtractWindow: 10,
		Gate:          memory.GateConfig{MinConf: 0.6, TodoConf: 0.85},
	}
}

// setupExtractFixture 预置 session + transcript + 两个 segments + 一个已有同名 topic
func setupExtractFixture(t *testing.T, d *StageDeps) (sid ids.ID, rustTopic *repo.Topic) {
	t.Helper()
	ctx := context.Background()

	// 预清理：测试库可能残留历史运行的同名 active/suggested 行（脏库重跑），
	// 它们会让按名查找命中旧行、破坏本测试的合并断言，先统一置 dismissed。
	if _, err := d.Topics.DB.ExecContext(ctx, `
UPDATE topic SET status='dismissed'
WHERE user_id = 1 AND name IN (?, ?) AND status IN ('active','suggested')`,
		"Rust 学习（抽取fixture）", "工作沟通（抽取fixture）"); err != nil {
		t.Fatal(err)
	}

	// 预清理：dismiss 其他 session 残留的同名 open todo（给 Tom 发邮件）。
	// T3 落库去重按归一化标题跨 session 比对，残留的 open todo 会让本 session 候选
	// 被跳过、破坏既有断言（todo 数=1）；与上面 topic 预清理同理。
	if _, err := d.Todos.DB.ExecContext(ctx, `
UPDATE todo SET status='dismissed'
WHERE user_id = 1 AND title = ? AND status IN ('suggested','confirmed')`,
		"给 Tom 发邮件"); err != nil {
		t.Fatal(err)
	}

	// 预清理：dismiss 其他 session 残留的同名 active memory
	// （给 Tom 发邮件 / 学习 Rust / 给 Tom 发邮件(改名)）。
	// D1 佐证去重按归一化标题跨 session 比对 active memory，残留的 active memory 会让
	// 本 session 候选被佐证跳过、破坏既有断言（memory 数=2）；与上面 topic/todo 预清理同理。
	// 「给 Tom 发邮件(改名)」专供 RerunPreservesUserLinks 漂移重跑：残留的改名 memory
	// 会让漂移候选被佐证跳过、找不到「给 Tom 发邮件(改名)」memory。
	if _, err := d.Memories.DB.ExecContext(ctx, `
UPDATE memory SET status='dismissed'
WHERE user_id = 1 AND title IN (?, ?, ?) AND status = 'active'`,
		"给 Tom 发邮件", "学习 Rust", "给 Tom 发邮件(改名)"); err != nil {
		t.Fatal(err)
	}

	// 预置已有 topic：第二条候选的 suggested_topic_name 与之同名 → 验证合并。
	// 名称加「（抽取fixture）」后缀保证全库唯一：repo 包的 TestTopicCRUD
	// 也建「Rust 学习」，共享测试库下同名旧行会让两边断言互相污染。
	rustTopic = &repo.Topic{Name: "Rust 学习（抽取fixture）", Status: "active", CreatedBy: "user"}
	if err := d.Topics.Create(ctx, rustTopic); err != nil {
		t.Fatal(err)
	}

	sid = ids.New()
	if err := d.Sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "x.wav",
		StoragePath: "/tmp/x.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := d.Transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	conf := 0.95
	// 两段同说话人但间隔 >30s（blockGapMS 阈值）→ 强制切成两个块：
	// 块 1 = Tom 邮件（todo 候选），块 2 = Rust 学习（抽取fixture）（fact 候选），
	// 与 fake LLM 响应里的 block_index 1/2 对应。
	if err := d.Transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1",
			Text: "明天记得给 Tom 发邮件确认设计稿", StartMS: 0, EndMS: 2000, Confidence: &conf},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "1",
			Text: "对了，我最近在学 Rust，打算三个月内读完那本书", StartMS: 40000, EndMS: 42000, Confidence: &conf},
	}); err != nil {
		t.Fatal(err)
	}
	return sid, rustTopic
}

func TestStageExtractCommit(t *testing.T) {
	llm := &fakeExtractLLM{}
	d := newExtractDeps(t, llm)
	sid, rustTopic := setupExtractFixture(t, &d)
	ctx := context.Background()

	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sid, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sid); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// memory：2 条落库，todo 候选来自块 1
	mems, err := d.Memories.ListBySession(ctx, sid)
	if err != nil || len(mems) != 2 {
		t.Fatalf("memories = %d err=%v", len(mems), err)
	}
	var todoMem *repo.MemoryRow
	for i := range mems {
		if mems[i].Title == "给 Tom 发邮件" {
			todoMem = &mems[i]
		}
	}
	if todoMem == nil {
		t.Fatal("未找到 todo 来源 memory")
	}
	if len(todoMem.TranscriptSegmentIDs) != 1 {
		t.Fatalf("segment_ids = %v", todoMem.TranscriptSegmentIDs)
	}

	// todo：confidence 0.9 >= 0.85 → confirmed；继承来源 memory 的 topic（工作沟通（抽取fixture））
	todos, err := d.Todos.ListBySession(ctx, sid)
	if err != nil || len(todos) != 1 {
		t.Fatalf("todos = %d err=%v", len(todos), err)
	}
	if todos[0].Status != "confirmed" {
		t.Fatalf("todo status = %s, want confirmed", todos[0].Status)
	}
	if todos[0].SourceMemoryID == nil || *todos[0].SourceMemoryID != todoMem.ID {
		t.Fatalf("source_memory_id = %v", todos[0].SourceMemoryID)
	}

	// topic：「工作沟通（抽取fixture）」新建为 suggested；「Rust 学习（抽取fixture）」合并到已有
	workTopic, err := d.Topics.FindActiveByName(ctx, 1, "工作沟通（抽取fixture）")
	if err != nil || workTopic == nil {
		t.Fatalf("工作沟通（抽取fixture） topic 未创建: %v %v", workTopic, err)
	}
	if workTopic.Status != "suggested" || workTopic.CreatedBy != "ai" {
		t.Fatalf("工作沟通（抽取fixture） = %+v", workTopic)
	}
	var rustMem *repo.MemoryRow
	for i := range mems {
		if mems[i].Title == "学习 Rust" {
			rustMem = &mems[i]
		}
	}
	if rustMem == nil {
		t.Fatal("未找到 Rust memory")
	}
	// topic 归属走关联表：rustMem.Topics 应含已有 rustTopic
	foundRustTopic := false
	for _, ti := range rustMem.Topics {
		if ti.ID == rustTopic.ID {
			foundRustTopic = true
		}
	}
	if !foundRustTopic {
		t.Fatalf("Rust memory 未挂已有 topic: %+v", rustMem.Topics)
	}
	foundWorkTopic := false
	for _, ti := range todos[0].Topics {
		if ti.ID == workTopic.ID {
			foundWorkTopic = true
		}
	}
	if !foundWorkTopic {
		t.Fatalf("todo topic = %+v, want 工作沟通（抽取fixture）", todos[0].Topics)
	}

	// 多对多关联表
	memLinks, err := d.MemoryTopics.ListByMemoryIDs(ctx, []ids.ID{todoMem.ID, rustMem.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(memLinks[todoMem.ID]) != 1 || memLinks[todoMem.ID][0].Source != "ai" {
		t.Fatalf("todoMem 关联 = %+v", memLinks[todoMem.ID])
	}
	if len(memLinks[rustMem.ID]) != 1 || memLinks[rustMem.ID][0].Source != "ai" {
		t.Fatalf("rustMem 关联 = %+v", memLinks[rustMem.ID])
	}
	todoLinks, err := d.TodoTopics.ListByTodoIDs(ctx, []ids.ID{todos[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(todoLinks[todos[0].ID]) != 1 || todoLinks[todos[0].ID][0].Source != "ai" {
		t.Fatalf("todo 关联 = %+v", todoLinks[todos[0].ID])
	}

	// trace 已记录
	if j.Trace == nil || len(*j.Trace) == 0 {
		t.Fatal("job.trace 未写入")
	}
	// extract:llm 条目应带 token 用量 / 窗口数 / prompt 版本（spec §3.3/§3.5）
	var entries []repo.TraceEntry
	if err := json.Unmarshal(*j.Trace, &entries); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Stage == "extract:llm" {
			found = true
			if e.Tokens != 500 || e.Windows != 1 || e.PromptVersion == "" {
				t.Fatalf("extract:llm trace = %+v, want Tokens=500 Windows=1 PromptVersion 非空", e)
			}
		}
	}
	if !found {
		t.Fatalf("trace 缺少 extract:llm 条目: %+v", entries)
	}
}

func TestStageExtractIdempotent(t *testing.T) {
	llm := &fakeExtractLLM{}
	d := newExtractDeps(t, llm)
	sid, _ := setupExtractFixture(t, &d)
	ctx := context.Background()

	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sid, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sid); err != nil {
		t.Fatal(err)
	}
	if err := handler(ctx, j, sid); err != nil { // 重跑
		t.Fatal(err)
	}
	mems, _ := d.Memories.ListBySession(ctx, sid)
	if len(mems) != 2 {
		t.Fatalf("重跑后 memories = %d, want 2（幂等）", len(mems))
	}
	todos, _ := d.Todos.ListBySession(ctx, sid)
	if len(todos) != 1 {
		t.Fatalf("重跑后 todos = %d, want 1（幂等）", len(todos))
	}

	// 重跑后关联表无重复行（每条 memory/todo 恰好 1 条 ai 关联）
	memRows, _ := d.Memories.ListBySession(ctx, sid)
	memIDs := make([]ids.ID, len(memRows))
	for i, mr := range memRows {
		memIDs[i] = mr.ID
	}
	ml, _ := d.MemoryTopics.ListByMemoryIDs(ctx, memIDs)
	for _, id := range memIDs {
		if len(ml[id]) != 1 {
			t.Fatalf("重跑后 memory %s 关联=%d, want 1", id, len(ml[id]))
		}
	}
	todoRows, _ := d.Todos.ListBySession(ctx, sid)
	todoIDs := make([]ids.ID, len(todoRows))
	for i, tr := range todoRows {
		todoIDs[i] = tr.ID
	}
	tl, _ := d.TodoTopics.ListByTodoIDs(ctx, todoIDs)
	for _, id := range todoIDs {
		if len(tl[id]) != 1 {
			t.Fatalf("重跑后 todo %s 关联=%d, want 1", id, len(tl[id]))
		}
	}
}

func TestStageExtractEmptyTranscript(t *testing.T) {
	llm := &fakeExtractLLM{}
	d := newExtractDeps(t, llm)
	ctx := context.Background()

	// 全空文本的会话
	sid2 := ids.New()
	_ = d.Sessions.Create(ctx, &repo.AudioSession{
		ID: sid2, Source: "web_upload", Filename: "e.wav",
		StoragePath: "/tmp/e.wav", Status: "processing",
	})
	tr2 := &repo.Transcript{SessionID: sid2, Language: "zh-CN"}
	_ = d.Transcripts.Create(ctx, tr2)
	_ = d.Transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr2.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "", StartMS: 0, EndMS: 100},
	})

	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sid2, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sid2); err != nil {
		t.Fatalf("空会话 extract 应成功: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("空会话不应调 LLM，calls = %d", llm.calls)
	}
	mems, _ := d.Memories.ListBySession(ctx, sid2)
	if len(mems) != 0 {
		t.Fatalf("空会话 memories = %d", len(mems))
	}
}

// TestStageExtractRerunPreservesUserLinks 验证 spec §6「重跑保留手动关联」headline 行为：
// 重跑按自然键 NaturalKey(segment_ids,title) 恢复 source='user' 手动关联，
// ai 行重建不重复；title 漂移则自然键不再命中 → user 关联不复原。
//
// 覆盖 TestStageExtractIdempotent 未守护的「user 行按自然键恢复」路径：
// commitExtract 删旧前快照 source='user' 行成 map，重建后按同键 INSERT IGNORE 补回。
// 见 stage_extract.go 的 commitExtract（快照→删→重建→重链）。
func TestStageExtractRerunPreservesUserLinks(t *testing.T) {
	llm := &fakeExtractLLM{}
	d := newExtractDeps(t, llm)
	sid, rustTopic := setupExtractFixture(t, &d)
	ctx := context.Background()

	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sid, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sid); err != nil {
		t.Fatalf("首次 extract: %v", err)
	}

	// 找到 todo 来源 memory（title=="给 Tom 发邮件"）与其 todo
	mems, _ := d.Memories.ListBySession(ctx, sid)
	todos, _ := d.Todos.ListBySession(ctx, sid)
	var todoMem1 *repo.MemoryRow
	for i := range mems {
		if mems[i].Title == "给 Tom 发邮件" {
			todoMem1 = &mems[i]
		}
	}
	if todoMem1 == nil {
		t.Fatal("未找到 todo 来源 memory（给 Tom 发邮件）")
	}
	if len(todos) != 1 {
		t.Fatalf("首次后 todos = %d, want 1", len(todos))
	}

	// 手动加 user 关联：rustTopic 与该 memory 的 ai topic「工作沟通（抽取fixture）」不同，
	// 便于在重跑后的关联里区分 ai 行与 user 行。
	// 自然键 = NaturalKey(segment_ids, title)；todo 与其 source memory 共享
	// segment_ids+title，故 memory 与 todo 的 user 关联用同一键恢复。
	if err := d.MemoryTopics.AddLink(ctx, todoMem1.ID, rustTopic.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.TodoTopics.AddLink(ctx, todos[0].ID, rustTopic.ID); err != nil {
		t.Fatal(err)
	}

	// ---- 重跑（同一 fake LLM → 同 title → 自然键匹配）----
	if err := handler(ctx, j, sid); err != nil {
		t.Fatalf("重跑 extract: %v", err)
	}

	// 重跑后找新的 todo 来源 memory（title 仍「给 Tom 发邮件」，但 ID 已换）
	mems2, _ := d.Memories.ListBySession(ctx, sid)
	todos2, _ := d.Todos.ListBySession(ctx, sid)
	var newTodoMem *repo.MemoryRow
	for i := range mems2 {
		if mems2[i].Title == "给 Tom 发邮件" {
			newTodoMem = &mems2[i]
		}
	}
	if newTodoMem == nil {
		t.Fatal("重跑后未找到新的 todo 来源 memory")
	}
	if len(todos2) != 1 {
		t.Fatalf("重跑后 todos = %d, want 1（幂等）", len(todos2))
	}

	// 新 todo 来源 memory 的关联恰好 2 条：1 ai（工作沟通）+ 1 user（rustTopic）——
	// 证明 user 行按自然键恢复、ai 行重建不重复。
	memLinks, err := d.MemoryTopics.ListByMemoryIDs(ctx, []ids.ID{newTodoMem.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(memLinks[newTodoMem.ID]); got != 2 {
		t.Fatalf("重跑后 memory 关联 = %d, want 2（1 ai + 1 user）", got)
	}
	memUserCnt, memRustHit := 0, false
	for _, ti := range memLinks[newTodoMem.ID] {
		if ti.Source == "user" {
			memUserCnt++
			if ti.ID == rustTopic.ID {
				memRustHit = true
			}
		}
	}
	if memUserCnt != 1 || !memRustHit {
		t.Fatalf("重跑后 memory user 关联 = %d rustHit=%v, want user=1 且命中 rustTopic", memUserCnt, memRustHit)
	}

	// todo 的关联同理 2 条（1 ai + 1 user）
	todoLinks, err := d.TodoTopics.ListByTodoIDs(ctx, []ids.ID{todos2[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(todoLinks[todos2[0].ID]); got != 2 {
		t.Fatalf("重跑后 todo 关联 = %d, want 2（1 ai + 1 user）", got)
	}
	todoUserCnt, todoRustHit := 0, false
	for _, ti := range todoLinks[todos2[0].ID] {
		if ti.Source == "user" {
			todoUserCnt++
			if ti.ID == rustTopic.ID {
				todoRustHit = true
			}
		}
	}
	if todoUserCnt != 1 || !todoRustHit {
		t.Fatalf("重跑后 todo user 关联 = %d rustHit=%v, want user=1 且命中 rustTopic", todoUserCnt, todoRustHit)
	}

	// ---- title 漂移不复原 ----
	// 用独立 fake LLM（driftExtractLLM）与独立 session/fixture，避免污染上面的断言。
	// 第 1 次返回原 title「给 Tom 发邮件」，加一条 user 关联；第 ≥2 次把 todo 候选
	// title 改成「给 Tom 发邮件(改名)」→ 自然键不再命中旧 user 关联 → 新 memory user=0。
	driftLLM := &driftExtractLLM{}
	dDrift := newExtractDeps(t, driftLLM)
	sid2, rustTopic2 := setupExtractFixture(t, &dDrift)
	h2 := BuildStages(dDrift)["extract"]
	j2 := &repo.Job{SessionID: sid2, Stage: "extract", Status: "running"}
	if err := h2(ctx, j2, sid2); err != nil {
		t.Fatalf("漂移首次 extract: %v", err)
	}
	// 漂移首次后的「给 Tom 发邮件」memory，手动加 user 关联到 rustTopic2
	memsDrift, _ := dDrift.Memories.ListBySession(ctx, sid2)
	var driftMem1 *repo.MemoryRow
	for i := range memsDrift {
		if memsDrift[i].Title == "给 Tom 发邮件" {
			driftMem1 = &memsDrift[i]
		}
	}
	if driftMem1 == nil {
		t.Fatal("漂移首次后未找到「给 Tom 发邮件」memory")
	}
	if err := dDrift.MemoryTopics.AddLink(ctx, driftMem1.ID, rustTopic2.ID); err != nil {
		t.Fatal(err)
	}
	// 重跑 → 第 2 次 Chat 返回 title「给 Tom 发邮件(改名)」
	if err := h2(ctx, j2, sid2); err != nil {
		t.Fatalf("漂移重跑 extract: %v", err)
	}
	// 新 memory 的 title 是改名后的，user 关联应为 0（自然键不再命中，只有 ai）
	memsDrift2, _ := dDrift.Memories.ListBySession(ctx, sid2)
	var driftMem2 *repo.MemoryRow
	for i := range memsDrift2 {
		if memsDrift2[i].Title == "给 Tom 发邮件(改名)" {
			driftMem2 = &memsDrift2[i]
		}
	}
	if driftMem2 == nil {
		t.Fatal("漂移重跑后未找到「给 Tom 发邮件(改名)」memory")
	}
	driftLinks, err := dDrift.MemoryTopics.ListByMemoryIDs(ctx, []ids.ID{driftMem2.ID})
	if err != nil {
		t.Fatal(err)
	}
	driftUserCnt := 0
	for _, ti := range driftLinks[driftMem2.ID] {
		if ti.Source == "user" {
			driftUserCnt++
		}
	}
	if driftUserCnt != 0 {
		t.Fatalf("title 漂移后新 memory user 关联 = %d, want 0（自然键不再命中）", driftUserCnt)
	}
}

// driftExtractLLM 专供 title 漂移测试：第 1 次返回原 title，第 ≥2 次把 todo 候选
// title 改成「给 Tom 发邮件(改名)」，使自然键 NaturalKey(segment_ids,title) 不再命中
// 旧 user 关联。独立于 fakeExtractLLM，避免污染 TestStageExtractCommit/Idempotent。
type driftExtractLLM struct{ call int }

func (d *driftExtractLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	d.call++
	// 第 1 次原 title；第 2 次起漂移成「(改名)」
	title := "给 Tom 发邮件"
	if d.call >= 2 {
		title = "给 Tom 发邮件(改名)"
	}
	// 候选 1（todo）的 title 用变量注入；候选 2（Rust）固定不变。
	resp := fmt.Sprintf(`{"candidates":[
	  {"type":"event","title":%q,"content":"明天需要给 Tom 发邮件确认设计稿",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":true,"todo_due":null,"topics":[{"suggested_name":"工作沟通（抽取fixture）"}],"block_index":1},
	  {"type":"fact","title":"学习 Rust","content":"用户正在学习 Rust 计划三个月内读完一本书",
	   "epistemic_type":"observed","importance":0.7,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topics":[{"suggested_name":"Rust 学习（抽取fixture）"}],"block_index":2}
	]}`, title)
	return provider.ChatResponse{Content: resp, TotalTokens: 500}, nil
}

// TestStageExtractDedupTodoByTitle 验证 T3：落库去重——新 suggested todo 若归一化
// 标题命中该用户已有未关闭（suggested+confirmed）todo 则不插入。
// commitExtract 重跑会删本 session todo 再重建，所以去重比对对象必须是不同 session
// 的已有 open todo（此处预置 session A 的 confirmed todo）。
// 去重只挡 todo，不挡 memory（memory 仍正常落库）。
func TestStageExtractDedupTodoByTitle(t *testing.T) {
	llm := &fakeExtractLLM{}
	d := newExtractDeps(t, llm)
	ctx := context.Background()

	// session B：被抽取的会话（setupExtractFixture 内含预清理同名 open todo）
	sidB, _ := setupExtractFixture(t, &d)

	// 独立 session A：预置一条 confirmed todo「给 Tom 发邮件」。
	// 先建 session 与 memory，再建 todo（source_memory_id 指向 session A 的 memory，
	// 使其不被 session B 的 DeleteBySessionExt 删除）。
	sidA := ids.New()
	if err := d.Sessions.Create(ctx, &repo.AudioSession{
		ID: sidA, Source: "web_upload", Filename: "a.wav",
		StoragePath: "/tmp/a.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	memA := &repo.Memory{
		Type: "event", Title: "预置 todo 来源 memory", Content: "session A 预置",
		EpistemicType: "observed", Importance: 0.5, Confidence: 0.9,
		SessionID: sidA, Status: "active",
	}
	if err := d.Memories.InsertExt(ctx, d.DB, []*repo.Memory{memA}); err != nil {
		t.Fatal(err)
	}
	preTodo := &repo.Todo{
		UserID: 1, Title: "给 Tom 发邮件", SourceMemoryID: &memA.ID,
		Status: "confirmed", Confidence: 0.9,
	}
	if err := d.Todos.InsertExt(ctx, d.DB, []*repo.Todo{preTodo}); err != nil {
		t.Fatal(err)
	}
	// 清理：预置 todo 跨 session 存在，会经 ListOpenTitles 影响后续测试，
	// 测试结束恢复干净状态。
	t.Cleanup(func() {
		_, _ = d.Todos.DB.ExecContext(ctx, `DELETE FROM todo WHERE id = ?`, preTodo.ID.Int64())
		_, _ = d.Memories.DB.ExecContext(ctx, `DELETE FROM memory WHERE id = ?`, memA.ID.Int64())
	})

	// 跑 extract（session B）——fake LLM 产出「给 Tom 发邮件」(todo) + 「学习 Rust」(非 todo)
	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sidB, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sidB); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// 断言：session B 的 todo 数 = 0
	//（「给 Tom 发邮件」归一后 = "给tom发邮件"，撞 session A 的 confirmed todo → 跳过）
	todos, _ := d.Todos.ListBySession(ctx, sidB)
	if len(todos) != 0 {
		t.Fatalf("session B todos = %d, want 0（归一化标题命中已有未关闭 todo，跳过）", len(todos))
	}
	// 断言：session B 的 memory 数 = 2（去重只挡 todo，不挡 memory）
	mems, _ := d.Memories.ListBySession(ctx, sidB)
	if len(mems) != 2 {
		t.Fatalf("session B memories = %d, want 2（去重不挡 memory）", len(mems))
	}
}

// fakeCorroborateLLM 专供 D1 佐证去重测试：产出 1 条候选「学 Rust」(is_todo，标题归一后
// ="学rust"，与预置 active memory「学Rust」撞) + 建议新主题「Rust 进阶（佐证fixture）」。
// 独立于 fakeExtractLLM，避免污染其它 extract 测试。
type fakeCorroborateLLM struct{}

func (f *fakeCorroborateLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{Content: `{"candidates":[
	  {"type":"fact","title":"学 Rust","content":"用户在学 Rust 打算三个月读完一本书",
	   "epistemic_type":"observed","importance":0.7,"confidence":0.9,
	   "is_todo":true,"todo_due":null,"topics":[{"suggested_name":"Rust 进阶（佐证fixture）"}],"block_index":1}
	]}`, TotalTokens: 500}, nil
}

// TestStageExtractMemoryCorroboration 验证 D1：预置 active memory「学Rust」(confidence 0.80)，
// 新 session 抽取候选「学 Rust」(归一后同为 学rust) → 不增 memory 行、旧 memory confidence=0.85、
// 旧 memory 获候选 topic 关联；且佐证候选(is_todo)不产 todo（todo 守卫）。
// 必须用不同 session 预置 old memory（本 session 旧 memory 已在 tx 内被 DeleteBySessionExt 删）。
func TestStageExtractMemoryCorroboration(t *testing.T) {
	llm := &fakeCorroborateLLM{}
	d := newExtractDeps(t, llm)
	sidB, _ := setupExtractFixture(t, &d) // session B：被抽取会话（含预清理）
	ctx := context.Background()

	// 预清理：脏库重跑时残留的 active「学Rust」记忆 / open「学 Rust」todo /
	// 「Rust 进阶（佐证fixture）」topic 会让断言不稳，先统一 dismiss/delete。
	if _, err := d.Memories.DB.ExecContext(ctx,
		`UPDATE memory SET status='dismissed' WHERE user_id=1 AND title='学Rust' AND status='active'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Todos.DB.ExecContext(ctx,
		`UPDATE todo SET status='dismissed' WHERE user_id=1 AND title='学 Rust' AND status IN ('suggested','confirmed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Topics.DB.ExecContext(ctx,
		`UPDATE topic SET status='dismissed' WHERE user_id=1 AND name='Rust 进阶（佐证fixture）' AND status IN ('active','suggested')`); err != nil {
		t.Fatal(err)
	}

	// 独立 session A：预置 active memory「学Rust」(confidence 0.80)，不挂 topic。
	// 必须用不同 session：本 session 旧 memory 已在 tx 内 DeleteBySessionExt 删，tx 内读不到。
	sidA := ids.New()
	if err := d.Sessions.Create(ctx, &repo.AudioSession{
		ID: sidA, Source: "web_upload", Filename: "corr.wav",
		StoragePath: "/tmp/corr.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	oldMem := &repo.Memory{
		Type: "fact", Title: "学Rust", Content: "用户在学 Rust",
		EpistemicType: "observed", Importance: 0.7, Confidence: 0.80,
		SessionID: sidA, Status: "active",
	}
	if err := d.Memories.InsertExt(ctx, d.DB, []*repo.Memory{oldMem}); err != nil {
		t.Fatal(err)
	}

	// 跑 extract（session B）——候选「学 Rust」命中 old memory「学Rust」(归一 = 学rust)
	handler := BuildStages(d)["extract"]
	j := &repo.Job{SessionID: sidB, Stage: "extract", Status: "running"}
	if err := handler(ctx, j, sidB); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// 断言 1：session B 无新 memory 行（候选被佐证跳过，未插）
	mems, _ := d.Memories.ListBySession(ctx, sidB)
	if len(mems) != 0 {
		t.Fatalf("session B memories = %d, want 0（候选佐证并入 old memory，不增行）", len(mems))
	}
	// 断言 2：old memory confidence 0.80 → 0.85（佐证 +0.05）
	got, _ := d.Memories.Get(ctx, oldMem.ID)
	if math.Abs(got.Confidence-0.85) > 0.001 {
		t.Fatalf("old memory confidence = %v, want 0.85", got.Confidence)
	}
	// 断言 3：old memory 获候选的 topic 关联（Rust 进阶（佐证fixture））
	links, _ := d.MemoryTopics.ListByMemoryIDs(ctx, []ids.ID{oldMem.ID})
	hitTopic := false
	for _, ti := range links[oldMem.ID] {
		if ti.Name == "Rust 进阶（佐证fixture）" {
			hitTopic = true
		}
	}
	if !hitTopic {
		t.Fatalf("old memory topics = %+v, want 含 Rust 进阶（佐证fixture）", links[oldMem.ID])
	}
	// 断言 4：佐证候选(is_todo)不产 todo（守卫 memories[i]==nil → continue）
	todos, _ := d.Todos.ListBySession(ctx, sidB)
	if len(todos) != 0 {
		t.Fatalf("session B todos = %d, want 0（佐证跳过的候选不产 todo）", len(todos))
	}
}
