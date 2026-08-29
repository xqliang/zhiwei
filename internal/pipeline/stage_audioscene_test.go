package pipeline

import (
	"context"
	"errors"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// sampleWAVPath 主仓库 testdata 的真实样本（worktree 内 testdata 未跟踪、不存在，用绝对路径供 ffmpeg 转码）。
const sampleWAVPath = "/Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/testdata/speech20s.wav"

// fakeAI 是测试用 AudioInsightProvider。
type fakeAI struct {
	out provider.AudioInsight
	err error
}

func (f *fakeAI) Analyze(_ context.Context, _ string, _ []string) (provider.AudioInsight, error) {
	return f.out, f.err
}

// 关闭 / 未装配：no-op 返回 nil，不落库。
func TestStageAudioSceneDisabledSkips(t *testing.T) {
	h := stageAudioScene(StageDeps{AudioInsightEnabled: false})
	if err := h(context.Background(), nil, ids.New()); err != nil {
		t.Errorf("关闭时应 no-op 返回 nil, got %v", err)
	}
	h2 := stageAudioScene(StageDeps{AudioInsightEnabled: true, AudioInsight: nil})
	if err := h2(context.Background(), nil, ids.New()); err != nil {
		t.Errorf("未装配 provider 时应 no-op 返回 nil, got %v", err)
	}
}

// 无 transcript 的会话 → 降级返回 nil（不阻断 job）。纯逻辑，不需 ffmpeg。
func TestStageAudioSceneDegradesNoTranscript(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sessions := &repo.SessionRepo{DB: db}
	states := &repo.SpeakerSessionStateRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "y.wav", Status: "done",
	}); err != nil {
		t.Fatal(err)
	}
	d := StageDeps{
		Sessions: sessions, Transcripts: &repo.TranscriptRepo{DB: db}, SpeakerStates: states,
		DataDir: t.TempDir(), AudioInsightEnabled: true,
		AudioInsight: &fakeAI{err: errors.New("boom")},
	}
	if err := stageAudioScene(d)(ctx, nil, sid); err != nil {
		t.Errorf("无 transcript 应降级返回 nil, got %v", err)
	}
	// 确认没落任何情绪行
	rows, _ := states.ListBySession(ctx, 1, sid)
	if len(rows) != 0 {
		t.Errorf("降级不应落库, got %d 行", len(rows))
	}
}

// 正常落库：transcript 环境列写入 + speaker_session_state 每人一条 + speaker_id 归因映射。
// 依赖 ffmpeg（transcode + 切片），无 ffmpeg 则跳过。
func TestStageAudioScenePersist(t *testing.T) {
	requireFFmpeg(t)
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	states := &repo.SpeakerSessionStateRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}

	sid := ids.New()
	// StoragePath 指向真实样本（ffmpeg 可转码）；用绝对路径（worktree 内 testdata 未跟踪、不存在）。
	samplePath := sampleWAVPath
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "speech20s.wav",
		StoragePath: samplePath, DurationMS: 20000, Status: "done",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	// 建两个 speaker 并插段（label 1/2 → speaker_id 归因）
	sp1 := &repo.Speaker{UserID: 1, Name: "甲"}
	_ = speakers.Create(ctx, sp1)
	sp2 := &repo.Speaker{UserID: 1, Name: "乙"}
	_ = speakers.Create(ctx, sp2)
	conf := 0.9
	segs := []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "hi", StartMS: 0, EndMS: 1000, Confidence: &conf},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "hello", StartMS: 1000, EndMS: 2000, Confidence: &conf},
	}
	if err := transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	// 回填 speaker_id（InsertSegments 不含该列，照生产用 SetSegmentSpeaker 按 label 批量回填）
	if err := transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", sp1.ID); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.SetSegmentSpeaker(ctx, tr.ID, "2", sp2.ID); err != nil {
		t.Fatal(err)
	}

	d := StageDeps{
		Sessions: sessions, Transcripts: transcripts, SpeakerStates: states,
		DataDir: t.TempDir(), AudioInsightEnabled: true, AudioInsightChunkSec: 600,
		AudioInsight: &fakeAI{out: provider.AudioInsight{
			AcousticScene: "室内", WeatherCues: "无", OverallMood: "专注",
			Speakers: []provider.SpeakerInsight{
				{Label: "1", Emotion: "平静", Confidence: 0.8},
				{Label: "2", Emotion: "焦虑", Confidence: 0.6},
			},
		}},
	}
	if err := stageAudioScene(d)(ctx, nil, sid); err != nil {
		t.Fatalf("stage 应成功: %v", err)
	}
	// 会话级环境落库
	got, err := transcripts.GetBySession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got.AcousticScene != "室内" || got.OverallMood != "专注" {
		t.Errorf("环境列未写入: %+v", got)
	}
	// 每人情绪落库 + speaker_id 归因
	rows, err := states.ListBySession(ctx, 1, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应 2 行, got %d", len(rows))
	}
	byLabel := map[string]repo.SpeakerSessionState{}
	for _, r := range rows {
		byLabel[r.SpeakerLabel] = r
	}
	if byLabel["1"].SpeakerID == nil || *byLabel["1"].SpeakerID != sp1.ID {
		t.Errorf("label1 应归因到 sp1: %+v", byLabel["1"])
	}
	if byLabel["2"].Emotion != "焦虑" {
		t.Errorf("label2 情绪=焦虑, got %q", byLabel["2"].Emotion)
	}
}

// TestStageAudioSceneSkipsDeadSpeakerID 验证 audioscene 回填 speaker_id 前校验存在性：
// 段的 speaker_label 映射到的 speaker 已被删除/不存在（幽灵纠正 pass 创建又弃用的孤儿声纹），
// 此时不得把该孤儿 id 写进 speaker_session_state.speaker_id（否则前端按 id 关联名字必然失败、
// 只能回退显示原始 label）。应回退：按当前段的稳定映射反查正确 speaker；查不到则留 NULL。
func TestStageAudioSceneSkipsDeadSpeakerID(t *testing.T) {
	requireFFmpeg(t)
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	states := &repo.SpeakerSessionStateRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}

	sid := ids.New()
	samplePath := sampleWAVPath
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "speech20s.wav",
		StoragePath: samplePath, DurationMS: 20000, Status: "done",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	// 真实说话人 real（label 2）；label 1 故意指向一个「不存在」的孤儿 id（模拟幽灵纠正弃用）。
	real := &repo.Speaker{UserID: 1, Name: "真人"}
	_ = speakers.Create(ctx, real)
	ghost := ids.New() // 不 Create → DB 里不存在
	conf := 0.9
	segs := []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "hi", StartMS: 0, EndMS: 1000, Confidence: &conf},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "hello", StartMS: 1000, EndMS: 2000, Confidence: &conf},
	}
	if err := transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	// label1 → ghost（孤儿），label2 → real
	_ = transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", ghost)
	_ = transcripts.SetSegmentSpeaker(ctx, tr.ID, "2", real.ID)

	d := StageDeps{
		Sessions: sessions, Transcripts: transcripts, SpeakerStates: states, Speakers: speakers,
		DataDir: t.TempDir(), AudioInsightEnabled: true, AudioInsightChunkSec: 600,
		AudioInsight: &fakeAI{out: provider.AudioInsight{
			AcousticScene: "室内", WeatherCues: "无", OverallMood: "专注",
			Speakers: []provider.SpeakerInsight{
				{Label: "1", Emotion: "平静", Confidence: 0.8},
				{Label: "2", Emotion: "焦虑", Confidence: 0.6},
			},
		}},
	}
	if err := stageAudioScene(d)(ctx, nil, sid); err != nil {
		t.Fatalf("stage 应成功: %v", err)
	}
	rows, err := states.ListBySession(ctx, 1, sid)
	if err != nil {
		t.Fatal(err)
	}
	byLabel := map[string]repo.SpeakerSessionState{}
	for _, r := range rows {
		byLabel[r.SpeakerLabel] = r
	}
	// 关键断言：label1 的孤儿 id 不得落库（speaker_id 须为 nil）
	if byLabel["1"].SpeakerID != nil {
		t.Errorf("label1 映射到不存在的孤儿 speaker，speaker_id 应留空, got %s（会把脏 id 写进库）", byLabel["1"].SpeakerID.String())
	}
	// label2 正常归因不受影响
	if byLabel["2"].SpeakerID == nil || *byLabel["2"].SpeakerID != real.ID {
		t.Errorf("label2 应仍归因到 real: %+v", byLabel["2"])
	}
}

// silencedetect 输出解析。
func TestParseSilenceBounds(t *testing.T) {
	log := "[silencedetect @ 0x1] silence_start: 12.34\n[silencedetect @ 0x1] silence_end: 13.56 | x\n[silencedetect @ 0x1] silence_start: 40.0\n[silencedetect @ 0x1] silence_end: 41.5\n"
	rs := parseSilenceBounds(log)
	if len(rs) != 2 {
		t.Fatalf("应 2 段静音, got %d", len(rs))
	}
	if rs[0].startSec != 12.34 || rs[0].endSec != 13.56 {
		t.Errorf("第一段静音异常: %+v", rs[0])
	}
	if rs[1].startSec != 40.0 {
		t.Errorf("第二段静音 start 异常: %+v", rs[1])
	}
}
