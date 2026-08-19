// stage_extract_test 验证 extract stage 的端到端行为：
// 聚合 → LLM 抽取（fake）→ 闸门 → Topic 归属 → 单事务提交（幂等）。
package pipeline

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/memory"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// fakeExtractLLM 固定响应（1 条 todo 候选建议新主题 + 1 条同名已有主题的候选）
type fakeExtractLLM struct{ calls int }

func (f *fakeExtractLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	f.calls++
	return provider.ChatResponse{Content: `{"candidates":[
	  {"type":"event","title":"给 Tom 发邮件","content":"明天需要给 Tom 发邮件确认设计稿",
	   "epistemic_type":"observed","importance":0.6,"confidence":0.9,
	   "is_todo":true,"todo_due":null,"topic_id":null,"suggested_topic_name":"工作沟通（抽取fixture）","block_index":1},
	  {"type":"fact","title":"学习 Rust","content":"用户正在学习 Rust 计划三个月内读完一本书",
	   "epistemic_type":"observed","importance":0.7,"confidence":0.9,
	   "is_todo":false,"todo_due":null,"topic_id":null,"suggested_topic_name":"Rust 学习（抽取fixture）","block_index":2}
	]}`, TotalTokens: 500}, nil
}

func newExtractDeps(t *testing.T, llm *fakeExtractLLM) StageDeps {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	return StageDeps{
		Sessions:      &repo.SessionRepo{DB: db},
		Transcripts:   &repo.TranscriptRepo{DB: db},
		DB:            db,
		Memories:      &repo.MemoryRepo{DB: db},
		Todos:         &repo.TodoRepo{DB: db},
		Topics:        &repo.TopicRepo{DB: db},
		LLM:           llm,
		LLMModel:      "fake-model",
		Prompt:        "测试 system prompt",
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
	if rustMem == nil || rustMem.TopicID == nil || *rustMem.TopicID != rustTopic.ID {
		t.Fatalf("Rust memory 未挂已有 topic: %+v", rustMem)
	}
	if todos[0].TopicID == nil || *todos[0].TopicID != workTopic.ID {
		t.Fatalf("todo topic = %v, want 工作沟通（抽取fixture）", todos[0].TopicID)
	}

	// trace 已记录
	if j.Trace == nil || len(*j.Trace) == 0 {
		t.Fatal("job.trace 未写入")
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
