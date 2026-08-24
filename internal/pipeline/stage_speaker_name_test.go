// stage_speaker_name_test 验证 speakername stage：纯函数（isAutoName /
// ParseNameCandidates）单测无需 DB；runSpeakerNameStage 集成测试见下（需 TEST_MYSQL_DSN）。
package pipeline

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

func TestIsAutoName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"说话人ab3x9", true},    // 自动登记随机名（rand5 产物）
		{"说话人zzzzz", true},    // 全字母也命中
		{"张三", false},         // 已确认真名
		{"说话人 1", false},      // 显示回退（带空格），从不落库，不该命中
		{"说话人ab3x", false},    // 4 位，非 rand5 形态
		{"说话人AB3X9", false},   // 大写，rand5 只产小写
		{"说话人ab3x9额外", false}, // 后缀多余
	}
	for _, c := range cases {
		if got := isAutoName(c.name); got != c.want {
			t.Errorf("isAutoName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseNameCandidates(t *testing.T) {
	// 正常：含围栏废话、多候选乱序 → 剥壳解析 + 按置信度倒排
	raw := "好的，以下是结果：\n```json\n{\"speakers\":[{\"ref\":\"待识别人物A\",\"candidates\":[{\"name\":\"张明\",\"confidence\":0.4,\"evidence\":\"自称我姓张\"},{\"name\":\"张总\",\"confidence\":0.82,\"evidence\":\"对方称呼张总\"}]},{\"ref\":\"待识别人物B\",\"candidates\":[]}]}\n```"
	got, err := ParseNameCandidates(raw)
	if err != nil {
		t.Fatal(err)
	}
	cands := got["待识别人物A"]
	if len(cands) != 2 {
		t.Fatalf("A 应 2 候选，实际 %d", len(cands))
	}
	if cands[0].Name != "张总" || cands[0].Confidence != 0.82 {
		t.Fatalf("倒序首位应为 张总/0.82，实际 %s/%.2f", cands[0].Name, cands[0].Confidence)
	}
	if cands[0].Evidence != "对方称呼张总" {
		t.Fatalf("evidence 应透传『对方称呼张总』，实际 %q", cands[0].Evidence)
	}
	if len(got["待识别人物B"]) != 0 {
		t.Fatalf("B 应 0 候选")
	}

	// 置信度越界 clamp 到 [0,1]；空名候选丢弃
	got2, err := ParseNameCandidates(`{"speakers":[{"ref":"X","candidates":[{"name":"a","confidence":1.5},{"name":"","confidence":0.9},{"name":"b","confidence":-0.1}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	c2 := got2["X"]
	if len(c2) != 2 {
		t.Fatalf("空名丢弃后应 2 条，实际 %d", len(c2))
	}
	if c2[0].Confidence != 1 || c2[1].Confidence != 0 {
		t.Fatalf("clamp 失败: %.2f %.2f", c2[0].Confidence, c2[1].Confidence)
	}

	// 纯空白名（trim 后为空）也应丢弃
	got3, err := ParseNameCandidates(`{"speakers":[{"ref":"Y","candidates":[{"name":"  ","confidence":0.9},{"name":"c","confidence":0.5}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got3["Y"]) != 1 || got3["Y"][0].Name != "c" {
		t.Fatalf("纯空白名应丢弃，仅留 c，实际 %+v", got3["Y"])
	}

	// 空输出契约：prompt 承诺无待识别人物时输出 {"speakers":[]} → 非 nil 空 map、无 error
	got4, err := ParseNameCandidates(`{"speakers":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got4 == nil {
		t.Fatal("空 speakers 应返回非 nil 空 map")
	}
	if len(got4) != 0 {
		t.Fatalf("空 speakers 应返回空 map，实际 %d 项", len(got4))
	}

	// 彻底非法 JSON → error（stage 走重试）
	if _, err := ParseNameCandidates(`完全不是 JSON`); err == nil {
		t.Fatal("非法 JSON 应返回 error")
	}
}

// fakeNameLLM 可配置响应的 LLM fake（记录调用次数，验证「无待识别不调 LLM」）。
type fakeNameLLM struct {
	calls int
	resp  string
}

func (f *fakeNameLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	f.calls++
	return provider.ChatResponse{Content: f.resp, TotalTokens: 100}, nil
}

// seedNameStage 建 session + transcript + 2 段 + 2 个 speaker（一个随机名/一个真名），
// 段通过 SetSegmentSpeaker 按 label 回填 speaker_id（InsertSegments 不写 speaker_id 列）。
// 返回 (transcripts, speakers, candidates, sid, tr, randSp, namedSp)。
func seedNameStage(t *testing.T, randName, namedName string) (*repo.TranscriptRepo, *repo.SpeakerRepo, *repo.SpeakerNameCandidateRepo, ids.ID, *repo.Transcript, *repo.Speaker, *repo.Speaker) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}
	candidates := &repo.SpeakerNameCandidateRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "talk.wav",
		StoragePath: "/tmp/talk.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "张总，您看这个方案怎么样", StartMS: 0, EndMS: 3000},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "我觉得可以，按这个来", StartMS: 3200, EndMS: 6000},
	}); err != nil {
		t.Fatal(err)
	}
	randSp := &repo.Speaker{Name: randName, Source: "auto"}
	if err := speakers.Create(ctx, randSp); err != nil {
		t.Fatal(err)
	}
	namedSp := &repo.Speaker{Name: namedName, Source: "enrolled"}
	if err := speakers.Create(ctx, namedSp); err != nil {
		t.Fatal(err)
	}
	// label 1 → 随机名说话人（待识别）；label 2 → 真名说话人
	_ = transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", randSp.ID)
	_ = transcripts.SetSegmentSpeaker(ctx, tr.ID, "2", namedSp.ID)
	return transcripts, speakers, candidates, sid, tr, randSp, namedSp
}

// newNameDeps 组 StageDeps（只填 speakername 用到的字段）。
func newNameDeps(sessions *repo.SessionRepo, transcripts *repo.TranscriptRepo,
	speakers *repo.SpeakerRepo, candidates *repo.SpeakerNameCandidateRepo, llm provider.LLMProvider) StageDeps {
	return StageDeps{
		Sessions: sessions, Transcripts: transcripts, Speakers: speakers,
		SpeakerNameCandidates: candidates, LLM: llm, LLMModel: "fake-model",
		NameInferPrompt: "测试 prompt", NameInferWindowMin: 10, NameInferMaxSegments: 400,
	}
}

func TestStageSpeakerNameInfersAndUpserts(t *testing.T) {
	transcripts, speakers, candidates, sid, tr, randSp, _ := seedNameStage(t, "说话人ab3x9", "李明")
	llm := &fakeNameLLM{resp: `{"speakers":[{"ref":"待识别人物A","candidates":[
		{"name":"张总","confidence":0.82,"evidence":"对方说『张总，您看这个方案』"},
		{"name":"张明","confidence":0.4,"evidence":"上下文推断"}]}]}`}
	d := newNameDeps(&repo.SessionRepo{DB: transcripts.DB}, transcripts, speakers, candidates, llm)
	if err := runSpeakerNameStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("应恰好 1 次 LLM 调用（批处理），实际 %d", llm.calls)
	}
	list, _ := candidates.ListBySpeakers(context.Background(), []ids.ID{randSp.ID})
	if len(list) != 2 || list[0].Name != "张总" || list[0].Confidence != 0.82 {
		t.Fatalf("候选应 2 条且倒序（张总 0.82 在首），实际 %+v", list)
	}
	if list[0].SpeakerID != randSp.ID {
		t.Fatalf("候选应归属随机名说话人 %s，实际 %s", randSp.ID, list[0].SpeakerID)
	}
	// 幂等：重跑（置信度更低）不增行、置信度不降
	llm2 := &fakeNameLLM{resp: `{"speakers":[{"ref":"待识别人物A","candidates":[
		{"name":"张总","confidence":0.5,"evidence":"重跑证据"}]}]}`}
	d2 := newNameDeps(&repo.SessionRepo{DB: transcripts.DB}, transcripts, speakers, candidates, llm2)
	if err := runSpeakerNameStage(context.Background(), d2, sid, tr); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	list, _ = candidates.ListBySpeakers(context.Background(), []ids.ID{randSp.ID})
	if len(list) != 2 || list[0].Confidence != 0.82 {
		t.Fatalf("重跑后应仍 2 条、置信度保留 0.82，实际 %+v", list)
	}
}

func TestStageSpeakerNameNoopWithoutPending(t *testing.T) {
	// 全部说话人已确认真名 → 不调 LLM、不写候选
	transcripts, speakers, candidates, sid, tr, randSp, namedSp := seedNameStage(t, "已改名的人", "李明")
	// 把随机名 speaker 改名（模拟用户已认领）
	_ = speakers.UpdateName(context.Background(), randSp.ID, "王五")
	llm := &fakeNameLLM{resp: `{"speakers":[]}`}
	d := newNameDeps(&repo.SessionRepo{DB: transcripts.DB}, transcripts, speakers, candidates, llm)
	if err := runSpeakerNameStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("无待识别说话人不应调 LLM，实际 %d 次", llm.calls)
	}
	list, _ := candidates.ListBySpeakers(context.Background(), []ids.ID{randSp.ID, namedSp.ID})
	if len(list) != 0 {
		t.Fatalf("不应产生候选，实际 %d 条", len(list))
	}
}

func TestStageSpeakerNameIgnoresUnknownRef(t *testing.T) {
	// LLM 返回未分配的 ref（编造占位符）→ 忽略不落库
	transcripts, speakers, candidates, sid, tr, randSp, _ := seedNameStage(t, "说话人ab3x9", "李明")
	llm := &fakeNameLLM{resp: `{"speakers":[{"ref":"待识别人物Z","candidates":[
		{"name":"编造","confidence":0.9,"evidence":""}]}]}`}
	d := newNameDeps(&repo.SessionRepo{DB: transcripts.DB}, transcripts, speakers, candidates, llm)
	if err := runSpeakerNameStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	list, _ := candidates.ListBySpeakers(context.Background(), []ids.ID{randSp.ID})
	if len(list) != 0 {
		t.Fatalf("未知 ref 的候选应忽略，实际 %d 条", len(list))
	}
}
