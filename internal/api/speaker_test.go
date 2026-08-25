package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// fakeVoiceprintAPI 实现 voiceprint.Client（Embed/Add/Remove 真返回，Search 不用）。
type fakeVoiceprintAPI struct{}

func (fakeVoiceprintAPI) Embed(_ context.Context, _ string) ([]float32, error) {
	v := make([]float32, 256)
	for i := range v {
		v[i] = 0.1
	}
	return v, nil
}
func (fakeVoiceprintAPI) Add(_ context.Context, _ []float32, _ ids.ID) error { return nil }
func (fakeVoiceprintAPI) Remove(_ context.Context, _ ids.ID) error           { return nil }
func (fakeVoiceprintAPI) Search(_ context.Context, _ []float32) (ids.ID, float64, bool, error) {
	return 0, 0, false, nil
}

var _ voiceprint.Client = fakeVoiceprintAPI{}

func setupSpeakerAPI(t *testing.T) (http.Handler, *repo.SpeakerRepo, *repo.TranscriptRepo, string) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.Init(1); err != nil {
		t.Fatal(err)
	}
	speakers := &repo.SpeakerRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	dir := t.TempDir()
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{Speakers: speakers, Transcripts: transcripts, Voiceprint: fakeVoiceprintAPI{}, DataDir: dir})
	return r, speakers, transcripts, dir
}

func requireFFmpegAPI(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
}

func TestSpeakerRenameAndDelete(t *testing.T) {
	r, speakers, _, _ := setupSpeakerAPI(t)
	ctx := context.Background()
	sp := &repo.Speaker{Name: "说话人xx", Source: "auto"}
	if err := speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}

	// rename
	req := httptest.NewRequest(http.MethodPatch, "/api/speakers/"+sp.ID.String(), bytes.NewBufferString(`{"name":"张三"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("rename code %d body %s", rec.Code, rec.Body.String())
	}
	got, _ := speakers.Get(ctx, sp.ID)
	if got.Name != "张三" {
		t.Fatalf("name=%s", got.Name)
	}

	// delete
	req2 := httptest.NewRequest(http.MethodDelete, "/api/speakers/"+sp.ID.String(), nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != 204 {
		t.Fatalf("delete code %d", rec2.Code)
	}
	if _, err := speakers.Get(ctx, sp.ID); err == nil {
		t.Fatal("删除后仍可查到")
	}
}

func TestSpeakerEnroll(t *testing.T) {
	requireFFmpegAPI(t)
	r, speakers, _, _ := setupSpeakerAPI(t)
	wav, err := os.Open("../../testdata/speech.wav")
	if err != nil {
		t.Skip("无 testdata/speech.wav")
	}
	defer wav.Close()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "sample.wav")
	if _, err := io.Copy(fw, wav); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("name", "李四")
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/speakers", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("enroll code %d body %s", rec.Code, rec.Body.String())
	}
	// 登记成功后应能查到 enrolled speaker 名"李四"
	list, _ := speakers.List(context.Background())
	found := false
	for _, s := range list {
		if s.Name == "李四" && s.Source == "enrolled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未找到录入的说话人: %+v", list)
	}
}

// TestSpeakerSegments 验证 GET /api/speakers/{id}/segments 返回该说话人跨 session 的片段
// （含 session_id/filename/created_at + 段文本与 start/end ms，供声纹 tab 点开播放）。
func TestSpeakerSegments(t *testing.T) {
	r, speakers, transcripts, _ := setupSpeakerAPI(t)
	ctx := context.Background()
	sp := &repo.Speaker{Name: "王五", Source: "enrolled"}
	if err := speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	sessions := &repo.SessionRepo{DB: transcripts.DB}
	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "mtg.wav",
		StoragePath: "/tmp/mtg.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	conf := 0.9
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{{
		TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1",
		Text: "你好世界", StartMS: 1200, EndMS: 3400, Confidence: &conf,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", sp.ID); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/speakers/"+sp.ID.String()+"/segments", nil))
	if rec.Code != 200 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Segments []repo.SpeakerSegmentOccurrence `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Segments) != 1 {
		t.Fatalf("len=%d", len(resp.Segments))
	}
	got := resp.Segments[0]
	if got.SessionID != sid || got.Text != "你好世界" ||
		got.StartMS != 1200 || got.EndMS != 3400 || got.Filename != "mtg.wav" {
		t.Fatalf("occurrence mismatch: %+v", got)
	}
}

// TestSpeakerListWithCandidates 名册接口富化候选名：随机名说话人带 name_candidates
// （倒排 + 置信度数值），已确认真名者带空数组。
func TestSpeakerListWithCandidates(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.Init(1)
	speakers := &repo.SpeakerRepo{DB: db}
	candidates := &repo.SpeakerNameCandidateRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		SpeakerNameCandidates: candidates,
	})
	// 共享测试库隔离：本用例硬断言「恰好 2 个说话人 + 位置有序」，但同套件先跑的
	// TestSpeakerEnroll/TestSpeakerSegments 会往 speaker 表留数据（无 per-test 清理）。
	// 清空两表保证计数/顺序确定；speaker_name_candidate 无外键约束（见 migration），可独立清空。
	if _, err := db.Exec("DELETE FROM speaker_name_candidate"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM speaker"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	randSp := &repo.Speaker{Name: "说话人ab3x9", Source: "auto"}
	_ = speakers.Create(ctx, randSp)
	namedSp := &repo.Speaker{Name: "张三", Source: "enrolled"}
	_ = speakers.Create(ctx, namedSp)
	// 收尾删掉「张三」：repo 包 TestPersonLifecycle 经 EnsurePersonBootstrap 会把
	// 未绑定 active 同名 speaker 物化成 person，残留令其 FindByName(张三) 命中错误行。
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), namedSp.ID) })
	_ = candidates.Upsert(ctx, randSp.ID, "张总", 0.82, "对方称呼张总", 1001)
	_ = candidates.Upsert(ctx, randSp.ID, "张明", 0.4, "", 1001)

	req := httptest.NewRequest(http.MethodGet, "/api/speakers", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Speakers []struct {
			Name           string `json:"name"`
			NameCandidates []struct {
				Name       string  `json:"name"`
				Confidence float64 `json:"confidence"`
			} `json:"name_candidates"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// List 按 id 倒序（近建在前）：namedSp 后建在前
	if len(out.Speakers) != 2 {
		t.Fatalf("应 2 个说话人，实际 %d", len(out.Speakers))
	}
	if out.Speakers[0].Name != "张三" || len(out.Speakers[0].NameCandidates) != 0 {
		t.Fatalf("真名说话人应无候选: %+v", out.Speakers[0])
	}
	cands := out.Speakers[1].NameCandidates
	if len(cands) != 2 || cands[0].Name != "张总" || cands[0].Confidence != 0.82 {
		t.Fatalf("随机名说话人应带倒序候选（张总 0.82 在首），实际 %+v", cands)
	}
}

// TestSpeakerRenameClearsCandidates 改名（=用户采纳候选或手动命名）后清空该说话人候选。
func TestSpeakerRenameClearsCandidates(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.Init(1)
	speakers := &repo.SpeakerRepo{DB: db}
	candidates := &repo.SpeakerNameCandidateRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		SpeakerNameCandidates: candidates,
	})
	ctx := context.Background()
	sp := &repo.Speaker{Name: "说话人ab3x9", Source: "auto"}
	_ = speakers.Create(ctx, sp)
	_ = candidates.Upsert(ctx, sp.ID, "张总", 0.82, "", 1001)
	_ = candidates.Upsert(ctx, sp.ID, "张明", 0.4, "", 1001)

	req := httptest.NewRequest(http.MethodPatch, "/api/speakers/"+sp.ID.String(),
		bytes.NewBufferString(`{"name":"张总"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("rename code %d body %s", rec.Code, rec.Body.String())
	}
	list, _ := candidates.ListBySpeakers(ctx, []ids.ID{sp.ID})
	if len(list) != 0 {
		t.Fatalf("改名后候选应清空，实际 %d 条", len(list))
	}
}

// TestSpeakerDeleteNameCandidate 忽略单个候选端点：删该行、幂等、缺 name 400。
func TestSpeakerDeleteNameCandidate(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.Init(1)
	speakers := &repo.SpeakerRepo{DB: db}
	candidates := &repo.SpeakerNameCandidateRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		SpeakerNameCandidates: candidates,
	})
	ctx := context.Background()
	sp := &repo.Speaker{Name: "说话人ab3x9", Source: "auto"}
	_ = speakers.Create(ctx, sp)
	_ = candidates.Upsert(ctx, sp.ID, "张总", 0.82, "", 1001)

	// 正常忽略（中文 name 需 URL 编码）
	req := httptest.NewRequest(http.MethodDelete,
		"/api/speakers/"+sp.ID.String()+"/name-candidates?name="+url.QueryEscape("张总"), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	list, _ := candidates.ListBySpeakers(ctx, []ids.ID{sp.ID})
	if len(list) != 0 {
		t.Fatalf("忽略后应无候选，实际 %d", len(list))
	}
	// 幂等：再删一次 204
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req)
	if rec2.Code != 204 {
		t.Fatalf("重复忽略应幂等 204，实际 %d", rec2.Code)
	}
	// 缺 name → 400
	req3 := httptest.NewRequest(http.MethodDelete, "/api/speakers/"+sp.ID.String()+"/name-candidates", nil)
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)
	if rec3.Code != 400 {
		t.Fatalf("缺 name 应 400，实际 %d", rec3.Code)
	}
	// repo 未装配 → 501（另建一个不注入 SpeakerNameCandidates 的 router）
	rNil := chi.NewRouter()
	RegisterSpeaker(rNil, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		// SpeakerNameCandidates 故意留空
	})
	rec4 := httptest.NewRecorder()
	rNil.ServeHTTP(rec4, httptest.NewRequest(http.MethodDelete,
		"/api/speakers/"+sp.ID.String()+"/name-candidates?name="+url.QueryEscape("张总"), nil))
	if rec4.Code != 501 {
		t.Fatalf("repo 未装配应 501，实际 %d", rec4.Code)
	}
	// 非法 id → 400（id 解析在 name 校验之前，故带 name 也应 400）
	rec5 := httptest.NewRecorder()
	r.ServeHTTP(rec5, httptest.NewRequest(http.MethodDelete,
		"/api/speakers/not-a-valid-id/name-candidates?name="+url.QueryEscape("张总"), nil))
	if rec5.Code != 400 {
		t.Fatalf("非法 id 应 400，实际 %d", rec5.Code)
	}
}

// seedEnrollSession 建 session + transcript + 指定段，并把 speech20s.wav 放到
// {dir}/transcoded/{sid}.wav 供 EnrollFromSegment 切片。段的 TranscriptID 由本函数回填。
// 返回 sid + transcript + 落库后（含 id、按 sequence_no 升序）的段。
func seedEnrollSession(t *testing.T, transcripts *repo.TranscriptRepo, dir string, segs []repo.TranscriptSegment) (ids.ID, *repo.Transcript, []repo.TranscriptSegment) {
	t.Helper()
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: transcripts.DB}
	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "m.wav",
		StoragePath: "../../testdata/speech20s.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tc := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tc); err != nil {
		t.Fatal(err)
	}
	for i := range segs {
		segs[i].TranscriptID = tc.ID
	}
	if err := transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	// 放置切片源 wav：{dir}/transcoded/{sid}.wav
	tdir := filepath.Join(dir, "transcoded")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open("../../testdata/speech20s.wav")
	if err != nil {
		t.Skip("无 testdata/speech20s.wav")
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(tdir, sid.String()+".wav"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	saved, err := transcripts.ListSegments(ctx, tc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return sid, tc, saved
}

func enrollFromSegment(t *testing.T, r http.Handler, sid, segID ids.ID, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/sessions/"+sid.String()+"/segments/"+segID.String()+"/enroll",
		bytes.NewBufferString(`{"name":"`+name+`"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestEnrollFromSegmentReassignsSameSpeaker：timeline「用此段录音纹」录入后，该段 + 本会话中
// 与它当前 speaker_id 相同的所有段，都改判到新录入的说话人；其他说话人的段不受影响。
func TestEnrollFromSegmentReassignsSameSpeaker(t *testing.T) {
	requireFFmpegAPI(t)
	r, speakers, transcripts, dir := setupSpeakerAPI(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	spX := &repo.Speaker{Name: "说话人x", Source: "auto"}
	spY := &repo.Speaker{Name: "说话人y", Source: "auto"}
	must(speakers.Create(ctx, spX))
	must(speakers.Create(ctx, spY))

	sid, tc, segs := seedEnrollSession(t, transcripts, dir, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "甲一", StartMS: 0, EndMS: 4000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "甲二", StartMS: 4000, EndMS: 8000},
		{SequenceNo: 3, SpeakerLabel: "2", Text: "乙", StartMS: 8000, EndMS: 12000},
	})
	// 段1、段2 → X；段3 → Y
	must(transcripts.SetSegmentSpeakerByID(ctx, tc.ID, segs[0].ID, spX.ID))
	must(transcripts.SetSegmentSpeakerByID(ctx, tc.ID, segs[1].ID, spX.ID))
	must(transcripts.SetSegmentSpeakerByID(ctx, tc.ID, segs[2].ID, spY.ID))

	if rec := enrollFromSegment(t, r, sid, segs[0].ID, "张三"); rec.Code != 200 {
		t.Fatalf("enroll code %d body %s", rec.Code, rec.Body.String())
	}

	got, _ := transcripts.ListSegments(ctx, tc.ID)
	newID := got[0].SpeakerID
	if newID == nil || *newID == spX.ID || *newID == spY.ID {
		t.Fatalf("段1 应改判为新录入的说话人（非 X/Y）, got %+v", newID)
	}
	if got[1].SpeakerID == nil || *got[1].SpeakerID != *newID {
		t.Fatalf("段2（同属 X）应一并改判到新说话人, got %+v", got[1].SpeakerID)
	}
	if got[2].SpeakerID == nil || *got[2].SpeakerID != spY.ID {
		t.Fatalf("段3（Y）不应受影响, got %+v", got[2].SpeakerID)
	}
	np, err := speakers.Get(ctx, *newID)
	must(err)
	if np.Name != "张三" || np.Source != "enrolled" {
		t.Fatalf("新说话人应为 张三/enrolled, got %s/%s", np.Name, np.Source)
	}
	// 收尾删掉新录入的「张三」：repo 包 TestPersonLifecycle 经 EnsurePersonBootstrap
	// 会把未绑定 active 同名 speaker 物化成 person，残留令其 FindByName(张三) 命中错误行。
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), *newID) })
}

// TestEnrollFromSegmentReassignsByLabelWhenUnresolved：该段尚未识别出说话人(speaker_id 为空)时，
// 退回按 ASR 说话人标签分组——同标签的未解析段一并归到新说话人，其他标签的段不动。
func TestEnrollFromSegmentReassignsByLabelWhenUnresolved(t *testing.T) {
	requireFFmpegAPI(t)
	r, speakers, transcripts, dir := setupSpeakerAPI(t)
	ctx := context.Background()

	sid, tc, segs := seedEnrollSession(t, transcripts, dir, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "甲一", StartMS: 0, EndMS: 4000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "甲二", StartMS: 4000, EndMS: 8000},
		{SequenceNo: 3, SpeakerLabel: "2", Text: "乙", StartMS: 8000, EndMS: 12000},
	})
	// 三段 speaker_id 全为 NULL（未解析）
	if rec := enrollFromSegment(t, r, sid, segs[0].ID, "李四"); rec.Code != 200 {
		t.Fatalf("enroll code %d body %s", rec.Code, rec.Body.String())
	}

	got, _ := transcripts.ListSegments(ctx, tc.ID)
	if got[0].SpeakerID == nil || got[1].SpeakerID == nil || *got[0].SpeakerID != *got[1].SpeakerID {
		t.Fatalf("未解析场景应按 ASR 标签把 label 1 的两段归到同一新说话人, got %+v %+v", got[0].SpeakerID, got[1].SpeakerID)
	}
	if got[2].SpeakerID != nil {
		t.Fatalf("label 2 的段不应被改判, got %+v", got[2].SpeakerID)
	}
	np, err := speakers.Get(ctx, *got[0].SpeakerID)
	if err != nil {
		t.Fatal(err)
	}
	if np.Name != "李四" {
		t.Fatalf("新说话人名应为 李四, got %s", np.Name)
	}
	// 同上：收尾删除新录入说话人，防跨包物化污染（对齐 张三 用例的 cleanup）。
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), np.ID) })
}
