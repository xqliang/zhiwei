package pipeline

// denoise_test.go：ASR 前降噪（DeepFilterNet3）stage 集成测试——开关/强度传递、
// 失败降级、幂等复用。Denoiser 用注入 fake（真模型跑不起单测，exec 版由 dev 手测）。
import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// recASR 记录收到的音频路径（断言降噪开关是否换喂了 denoised 文件）。
type recASR struct{ got []string }

func (a *recASR) Transcribe(_ context.Context, path string) ([]provider.TranscriptPiece, error) {
	a.got = append(a.got, path)
	return []provider.TranscriptPiece{{SpeakerLabel: "1", Text: "降噪后转写", StartMS: 0, EndMS: 2000, Confidence: 0.9}}, nil
}

// fakeDenoiser 记录调用参数；fail=true 时返回错误（测降级）。dst 写个占位文件
//（幂等复用靠 Stat 存在判定，fake 须真落盘）。
type fakeDenoiser struct {
	mu    chan struct{} // 串行化 calls 计数（stage 单线程调用，防御性）
	calls []denoiseCall
	fail  bool
}

type denoiseCall struct {
	src, dst string
	atten    float64
}

func (f *fakeDenoiser) Denoise(_ context.Context, src, dst string, atten float64) error {
	<-f.mu
	f.calls = append(f.calls, denoiseCall{src, dst, atten})
	f.mu <- struct{}{}
	if f.fail {
		return os.ErrPermission
	}
	return os.WriteFile(dst, []byte("denoised"), 0o644)
}

func newFakeDenoiser(fail bool) *fakeDenoiser {
	f := &fakeDenoiser{mu: make(chan struct{}, 1), fail: fail}
	f.mu <- struct{}{}
	return f
}

// seedASRDenoiseSession 建 user-1 的 session + 降噪设置行（enabled/atten）。
// 返回 (sid, transcripts, asrSettings, dataDir)。
func seedASRDenoiseSession(t *testing.T, enabled bool, atten float64) (ids.ID, *repo.TranscriptRepo, *repo.AsrSettingsRepo, string) {
	t.Helper()
	requireFFmpeg(t)
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	asrSettings := &repo.AsrSettingsRepo{DB: db}
	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, UserID: 1, Source: "web_upload", Filename: "speech.wav",
		StoragePath: "../../testdata/speech.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	if err := asrSettings.Upsert(ctx, 1, enabled, atten, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM asr_settings WHERE user_id = 1")
		_, _ = db.Exec("DELETE FROM transcript_segment WHERE transcript_id IN (SELECT id FROM transcript WHERE session_id = ?)", sid.Int64())
		_, _ = db.Exec("DELETE FROM transcript WHERE session_id = ?", sid.Int64())
		_, _ = db.Exec("DELETE FROM audio_session WHERE id = ?", sid.Int64())
	})
	// DataDir 指向临时目录（transcode 产物 + denoised 产物都落在里面，测试互不污染）；
	// 源音频用相对路径（StoragePath 相对包目录），故 dataDir 建在包目录下由 t.TempDir 搬不动——
	// 直接用临时目录并把 StoragePath 写绝对路径。
	abs, err := filepath.Abs("../../testdata/speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec("UPDATE audio_session SET storage_path = ? WHERE id = ?", abs, sid.Int64())
	return sid, transcripts, asrSettings, t.TempDir()
}

// TestStageASRDenoiseEnabledAndIdempotent：开关开 → ASR 收到 {sid}.denoised.wav、
// Denoise 收到用户配置的强度；重跑 stage 复用已存在产物（不重复降噪）。
func TestStageASRDenoiseEnabledAndIdempotent(t *testing.T) {
	sid, transcripts, asrSettings, dataDir := seedASRDenoiseSession(t, true, 30)
	asr := &recASR{}
	dn := newFakeDenoiser(false)
	d := StageDeps{
		Sessions: &repo.SessionRepo{DB: transcripts.DB}, Transcripts: transcripts,
		ASR: asr, DataDir: dataDir, AsrSettings: asrSettings, Denoise: dn,
	}
	ctx := context.Background()
	h := stageASR(d)
	if err := h(ctx, &repo.Job{}, sid); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(asr.got) != 1 || !strings.HasSuffix(asr.got[0], ".denoised.wav") {
		t.Fatalf("ASR 应收到 denoised 路径，实际 %v", asr.got)
	}
	if len(dn.calls) != 1 || dn.calls[0].atten != 30 {
		t.Fatalf("Denoise 应以用户强度 30 调一次，实际 %+v", dn.calls)
	}
	if !strings.HasSuffix(dn.calls[0].dst, sid.String()+".denoised.wav") {
		t.Fatalf("降噪产物路径不符: %s", dn.calls[0].dst)
	}
	// 幂等：重跑 stage（transcode 复用 + denoised 存在）→ 不再调 Denoise，ASR 仍喂 denoised
	if err := h(ctx, &repo.Job{}, sid); err != nil {
		t.Fatalf("重跑 stage: %v", err)
	}
	if len(dn.calls) != 1 {
		t.Fatalf("重跑应复用降噪产物（不重复调 Denoise），实际 %d 次", len(dn.calls))
	}
	if len(asr.got) != 2 || !strings.HasSuffix(asr.got[1], ".denoised.wav") {
		t.Fatalf("重跑 ASR 仍应喂 denoised，实际 %v", asr.got)
	}
}

// TestStageASRDenoiseDisabledByDefault：无开关（默认关）→ 不降噪，ASR 吃原始 transcoded wav。
func TestStageASRDenoiseDisabledByDefault(t *testing.T) {
	sid, transcripts, asrSettings, dataDir := seedASRDenoiseSession(t, false, 21)
	asr := &recASR{}
	dn := newFakeDenoiser(false)
	d := StageDeps{
		Sessions: &repo.SessionRepo{DB: transcripts.DB}, Transcripts: transcripts,
		ASR: asr, DataDir: dataDir, AsrSettings: asrSettings, Denoise: dn,
	}
	if err := stageASR(d)(context.Background(), &repo.Job{}, sid); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(dn.calls) != 0 {
		t.Fatalf("开关关时不应降噪，实际 %d 次", len(dn.calls))
	}
	if len(asr.got) != 1 || strings.HasSuffix(asr.got[0], ".denoised.wav") {
		t.Fatalf("ASR 应吃原始 wav，实际 %v", asr.got)
	}
}

// TestStageASRDenoiseFailureFallsBack：降噪失败（如 python 环境缺失）→ 降级用原始
// 音频继续 ASR（尽力而为不 fail session），trace 记录失败原因。
func TestStageASRDenoiseFailureFallsBack(t *testing.T) {
	sid, transcripts, asrSettings, dataDir := seedASRDenoiseSession(t, true, 21)
	asr := &recASR{}
	dn := newFakeDenoiser(true)
	d := StageDeps{
		Sessions: &repo.SessionRepo{DB: transcripts.DB}, Transcripts: transcripts,
		ASR: asr, DataDir: dataDir, AsrSettings: asrSettings, Denoise: dn,
	}
	j := &repo.Job{}
	if err := stageASR(d)(context.Background(), j, sid); err != nil {
		t.Fatalf("降噪失败不应 fail stage: %v", err)
	}
	if len(asr.got) != 1 || strings.HasSuffix(asr.got[0], ".denoised.wav") {
		t.Fatalf("失败应降级喂原始 wav，实际 %v", asr.got)
	}
	if j.Trace == nil || !strings.Contains(string(*j.Trace), "降噪失败") {
		t.Fatalf("trace 应记录降噪失败，实际 %v", j.Trace)
	}
	// 转写仍完成（段落库）
	tr, err := transcripts.GetBySession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	segs, _ := transcripts.ListSegments(context.Background(), tr.ID)
	if len(segs) == 0 {
		t.Fatal("降级路径下转写段应正常落库")
	}
}

// TestVoiceprintWAVForStage 声纹域开关：denoise_voiceprint 开 → 返回降噪产物路径
//（无则生成、幂等）；关 → 原始 wav 不动；降噪失败 → 降级原始 wav。
func TestVoiceprintWAVForStage(t *testing.T) {
	sid, transcripts, asrSettings, dataDir := seedASRDenoiseSession(t, false, 25)
	ctx := context.Background()
	db := transcripts.DB
	// 造 transcoded wav（生成源）
	tDir := filepath.Join(dataDir, "transcoded")
	if err := os.MkdirAll(tDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(tDir, sid.String()+".wav")
	if err := os.WriteFile(orig, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	dn := newFakeDenoiser(false)
	failDN := newFakeDenoiser(true)
	baseD := StageDeps{
		Sessions: &repo.SessionRepo{DB: db}, DataDir: dataDir,
		AsrSettings: asrSettings,
	}

	// 1) 开关关（seed 默认 false）→ 原始路径、不调降噪
	got := voiceprintWAVForStage(ctx, baseD, sid, orig)
	if got != orig || len(dn.calls) != 0 {
		t.Fatalf("开关关应返回原始 wav 且不降噪，got=%v calls=%d", got, len(dn.calls))
	}
	// 2) 开关开 → 生成降噪产物并返回其路径（强度用用户设置 25）
	if err := asrSettings.Upsert(ctx, 1, false, 25, true); err != nil {
		t.Fatal(err)
	}
	d := baseD
	d.Denoise = dn
	got = voiceprintWAVForStage(ctx, d, sid, orig)
	want := filepath.Join(tDir, sid.String()+".denoised.wav")
	if got != want {
		t.Fatalf("开关开应返回降噪产物 %s，实际 %s", want, got)
	}
	if len(dn.calls) != 1 || dn.calls[0].atten != 25 || dn.calls[0].src != orig {
		t.Fatalf("降噪调用不符（强度应 25）: %+v", dn.calls)
	}
	// 3) 幂等：再调一次不重复生成
	got = voiceprintWAVForStage(ctx, d, sid, orig)
	if got != want || len(dn.calls) != 1 {
		t.Fatalf("幂等应复用产物，got=%v calls=%d", got, len(dn.calls))
	}
	// 4) 降噪失败 → 降级原始 wav
	d2 := baseD
	d2.Denoise = failDN
	os.Remove(want)
	got = voiceprintWAVForStage(ctx, d2, sid, orig)
	if got != orig {
		t.Fatalf("失败应降级原始 wav，实际 %s", got)
	}
	// 5) 依赖未装配（Denoise nil）→ 原始 wav（兼容旧装配/测试）
	d3 := baseD
	got = voiceprintWAVForStage(ctx, d3, sid, orig)
	if got != orig {
		t.Fatalf("未装配应原始 wav，实际 %s", got)
	}
}
