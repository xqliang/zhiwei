// stage_profile_test 验证 profile stage 的接线：BuildStages 注册 profile handler，
// handler 调用 Service.ExtractSession 跑通并写 trace；nil service 报错；
// ExtractSession 运行时失败（F3）非致命——记 trace（含「非致命」）后返回 nil 放行。
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// fakeLLM 按序返回预置响应（每次 Chat 弹出一条）。pipeline 包已有 fakeExtractLLM
// 等 fake，但它们产出 memory extract 的 candidates 响应；profile stage 需要 facts
// 响应，形状不同不可复用，故照 profile 包 extractor_test.go 的 fakeLLM 样式在本包另立。
type fakeLLM struct {
	resps []string
	err   error // 非 nil 时 Chat 直接返回该错误（模拟 LLM 超时/抖动，供 F3 非致命化用例用）
}

func (f *fakeLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	if f.err != nil {
		return provider.ChatResponse{}, f.err
	}
	if len(f.resps) == 0 {
		return provider.ChatResponse{}, nil
	}
	r := f.resps[0]
	f.resps = f.resps[1:]
	return provider.ChatResponse{Content: r, TotalTokens: 42}, nil
}

var _ provider.LLMProvider = (*fakeLLM)(nil)

func TestStageProfile(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	speakers := &repo.SpeakerRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	svc := &profile.Service{
		DB:       db,
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Speakers: speakers,
		Persons: persons, Attributes: &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db}, ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
		LLM: &fakeLLM{resps: []string{`{"facts":[
			{"plane":"attribute","subject":{"kind":"self"},"attr_key":"occupation","value":"工程师",
			 "confidence":0.9,"epistemic_type":"observed","block_index":1}
		]}`}},
		Model: "test", Prompt: "sys", PromptVersion: "profile_extraction_v3",
		Window: 10, Gate: profile.GateConfig{AutoConf: 0.75},
	}
	if err := repo.EnsurePersonBootstrap(ctx, persons, speakers); err != nil {
		t.Fatal(err)
	}

	// 本用例经 ExtractSession 往共享 owner（user_id=1）写了 occupation=工程师 active 行 +
	// 审计。pipeline 按字典序在 profile 包之前跑，且两包共用同一 zhiwei_test 库；若不清理，
	// profile 包 TestExtractSession（写 occupation=后端开发工程师，高置信不同值）会撞见这条
	// 现值而被闸门判为冲突→pending，断言「2 条 active」失败。收尾删掉 owner 的 occupation
	// 属性 + 审计，恢复干净基线（模式参照 profile/extract_session_test.go）。提前用 t.Cleanup
	// 注册，保证任一断言 t.Fatal 提前退出时也会清理。
	t.Cleanup(func() {
		cctx := context.Background()
		if o, err := persons.GetOwner(cctx, 1); err == nil && o != nil {
			oid := o.ID.Int64()
			_, _ = db.ExecContext(cctx, `DELETE FROM person_attribute WHERE person_id = ? AND attr_key = 'occupation'`, oid)
			_, _ = db.ExecContext(cctx, `DELETE FROM person_change_log WHERE person_id = ?`, oid)
		}
	})

	// 最小 session 夹具。audio_session.id 非自增，须显式赋雪花 ID（SessionRepo.Create
	// 不生成 ID）；不赋值会插入 id=0，共享测试库重跑即 PRIMARY 冲突。
	sess := &repo.AudioSession{ID: ids.New(), Source: "web_upload", Filename: "t.wav", StoragePath: "/tmp/t.wav", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sess.ID, Language: "zh-CN"}
	if err := svc.Transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := svc.Transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "我", Text: "我是一名后端工程师", StartMS: 0, EndMS: 3000},
	}); err != nil {
		t.Fatal(err)
	}

	// BuildStages 注册了 profile；handler 跑通并写 trace
	stages := BuildStages(StageDeps{Profile: svc})
	h, ok := stages["profile"]
	if !ok {
		t.Fatal("profile stage 未注册")
	}
	j := &repo.Job{}
	if err := h(ctx, j, sess.ID); err != nil {
		t.Fatal(err)
	}
	// trace 有 profile 条目
	var entries []repo.TraceEntry
	if err := json.Unmarshal(*j.Trace, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Stage != "profile" || entries[0].PromptVersion != "profile_extraction_v3" {
		t.Fatalf("trace 错误: %+v", entries)
	}
}

func TestStageProfileNilService(t *testing.T) {
	h := stageProfile(StageDeps{})
	if err := h(context.Background(), &repo.Job{}, ids.New()); err == nil {
		t.Fatal("nil service 应报错")
	}
}

// TestStageProfileNonFatal 验证 F3（spec §13）：ExtractSession 运行时失败（此处模拟
// LLM 超时/抖动）时，profile handler 不把整个 session 置 failed——而是记一条含「非致命」
// 的 trace 后返回 nil 放行。transcript/memory 已落库完好，画像只是增强数据，可事后从
// 历史回填端点重跑（ApplyFacts 幂等）。
func TestStageProfileNonFatal(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 失败场景：LLM 直接返回错误 → ExtractSession 在 ex.Extract（读段/聚合/名单之后）
	// 处失败，返回「LLM 抽取: ...」错误。这正是 F3 要放行的场景：session/transcript
	// 已就绪且完好，只有画像抽取这一步因 LLM 抖动挂了。
	svc := &profile.Service{
		DB:       db,
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Speakers: &repo.SpeakerRepo{DB: db},
		Persons: &repo.PersonRepo{DB: db}, Attributes: &repo.PersonAttributeRepo{DB: db},
		Relationships: &repo.PersonRelationshipRepo{DB: db}, ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
		LLM:    &fakeLLM{err: errors.New("模拟 LLM 超时")},
		Model:  "test", Prompt: "sys", PromptVersion: "profile_extraction_v3",
		Window: 10, Gate: profile.GateConfig{AutoConf: 0.75},
	}

	// 最小 session+transcript+segment 夹具（fresh 雪花 ID，共享测试库重跑无 PRIMARY 冲突）。
	// 失败早于 ApplyFacts（LLM 步就挂了），不写任何 person 行，故无需清理共享库 owner 数据。
	sess := &repo.AudioSession{ID: ids.New(), Source: "web_upload", Filename: "t.wav", StoragePath: "/tmp/t.wav", Status: "completed"}
	if err := svc.Sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sess.ID, Language: "zh-CN"}
	if err := svc.Transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := svc.Transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "我", Text: "我是一名后端工程师", StartMS: 0, EndMS: 3000},
	}); err != nil {
		t.Fatal(err)
	}

	h := stageProfile(StageDeps{Profile: svc})
	j := &repo.Job{}
	// 断言 1：handler 返回 nil（非致命——pool 不会据此把 session 置 failed）。
	if err := h(ctx, j, sess.ID); err != nil {
		t.Fatalf("profile 抽取失败应非致命（handler 返回 nil），却返回: %v", err)
	}
	// 断言 2：trace 记了一条 profile 条目，Error 字段含「非致命」（供事后排查与回填提示）。
	if j.Trace == nil {
		t.Fatal("应写入非致命 trace，但 j.Trace 为 nil")
	}
	var entries []repo.TraceEntry
	if err := json.Unmarshal(*j.Trace, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Stage != "profile" {
		t.Fatalf("期望 1 条 profile trace，得: %+v", entries)
	}
	if !strings.Contains(entries[0].Error, "非致命") {
		t.Fatalf("trace Error 应含「非致命」，得: %q", entries[0].Error)
	}
}
