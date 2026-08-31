package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
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
func (fakeVoiceprintAPI) IDs(_ context.Context) ([]ids.ID, error)            { return nil, nil }
func (fakeVoiceprintAPI) Search(_ context.Context, _ []float32) (voiceprint.SearchResult, error) {
	return voiceprint.SearchResult{}, nil
}

var _ voiceprint.Client = fakeVoiceprintAPI{}

func setupSpeakerAPI(t *testing.T) (http.Handler, *repo.SpeakerRepo, *repo.TranscriptRepo, string) {
	t.Helper()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.InitForTest(); err != nil {
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
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.InitForTest()
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

// TestSpeakerListWithPersons 名册接口富化人物绑定：已绑人物的声纹带 person_id/person_name
// （名册「跳人物」入口），未绑者为空字段。Persons 未装配（nil）时降级不填充不报错。
func TestSpeakerListWithPersons(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.InitForTest()
	speakers := &repo.SpeakerRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		Persons: persons,
	})
	ctx := context.Background()
	bound := &repo.Speaker{Name: "绑定声纹测试", Source: "enrolled"}
	free := &repo.Speaker{Name: "未绑定声纹测试", Source: "enrolled"}
	_ = speakers.Create(ctx, bound)
	_ = speakers.Create(ctx, free)
	// 绑定人物 + 收尾清理（person 行残留会污染 EnsurePersonBootstrap 类用例的 FindByName）
	p := &repo.Person{DisplayName: "绑定人物测试", SpeakerID: &bound.ID}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = persons.SetStatus(context.Background(), p.ID, "dismissed")
		_ = speakers.Delete(context.Background(), bound.ID)
		_ = speakers.Delete(context.Background(), free.ID)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/speakers", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Speakers []struct {
			Name       string  `json:"name"`
			PersonID   *string `json:"person_id"`
			PersonName string  `json:"person_name"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byName := map[string]int{}
	for i, s := range out.Speakers {
		byName[s.Name] = i
	}
	i, ok := byName["绑定声纹测试"]
	if !ok {
		t.Fatalf("缺绑定声纹: %+v", out.Speakers)
	}
	if out.Speakers[i].PersonID == nil || *out.Speakers[i].PersonID != p.ID.String() ||
		out.Speakers[i].PersonName != "绑定人物测试" {
		t.Fatalf("绑定声纹应带 person_id/person_name: %+v", out.Speakers[i])
	}
	if j, ok := byName["未绑定声纹测试"]; !ok || out.Speakers[j].PersonID != nil || out.Speakers[j].PersonName != "" {
		t.Fatalf("未绑定声纹应无人物字段: %+v", out.Speakers)
	}
}

// TestSpeakerRenameSyncsPerson 绑定不变式的反向：已绑人物的声纹改名 → 人物名连带改
// （走 profile.Service，保审计 + 同事务回写声纹名）。未装配 Persons/Service 的旧装配不受影响。
func TestSpeakerRenameSyncsPerson(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.InitForTest()
	speakers := &repo.SpeakerRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	svc := &profile.Service{
		DB: db, Persons: persons, Speakers: speakers,
		Attributes: &repo.PersonAttributeRepo{DB: db},
		ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
	}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		Persons: persons, Service: svc,
	})
	ctx := context.Background()
	sp := &repo.Speaker{Name: "绑定声纹改名测试", Source: "enrolled"}
	_ = speakers.Create(ctx, sp)
	p := &repo.Person{DisplayName: "旧人物名", SpeakerID: &sp.ID}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = persons.SetStatus(context.Background(), p.ID, "dismissed")
		_ = speakers.Delete(context.Background(), sp.ID)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/speakers/"+sp.ID.String(),
		strings.NewReader(`{"name":"新人物名"}`)))
	if rec.Code != 200 {
		t.Fatalf("声纹改名失败: %d %s", rec.Code, rec.Body.String())
	}
	gotSp, _ := speakers.Get(ctx, sp.ID)
	gotP, _ := persons.Get(ctx, 1, p.ID)
	if gotSp.Name != "新人物名" || gotP.DisplayName != "新人物名" {
		t.Fatalf("声纹改名应联动人物：speaker=%q person=%q", gotSp.Name, gotP.DisplayName)
	}
}

// TestSpeakerSetPerson 声纹侧人物关联端点（转移语义）：关联（声纹名同步）/ 换绑（原持有人
// 被清，不撞唯一键）/ 解绑（声纹名保留）/ 目标人物已绑其他声纹 409。
func TestSpeakerSetPerson(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.InitForTest()
	speakers := &repo.SpeakerRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	svc := &profile.Service{
		DB: db, Persons: persons, Speakers: speakers,
		ChangeLogs: &repo.PersonChangeLogRepo{DB: db},
	}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		Persons: persons, Service: svc,
	})
	ctx := context.Background()
	sp := &repo.Speaker{Name: "转移测试声纹", Source: "enrolled"}
	_ = speakers.Create(ctx, sp)
	otherSp := &repo.Speaker{Name: "另一条声纹", Source: "enrolled"}
	_ = speakers.Create(ctx, otherSp)
	pa := &repo.Person{DisplayName: "持有人甲"}
	pb := &repo.Person{DisplayName: "目标人物乙"}
	pc := &repo.Person{DisplayName: "占用者丙", SpeakerID: &otherSp.ID}
	for _, p := range []*repo.Person{pa, pb, pc} {
		if err := persons.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = speakers.Delete(context.Background(), sp.ID)
		_ = speakers.Delete(context.Background(), otherSp.ID)
		for _, p := range []*repo.Person{pa, pb, pc} {
			_ = persons.SetStatus(context.Background(), p.ID, "dismissed")
		}
	})

	patch := func(sid ids.ID, personID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/speakers/"+sid.String()+"/person",
			strings.NewReader(`{"person_id":"`+personID+`"}`)))
		return rec
	}

	// ① 关联：sp → 甲，声纹名同步为人物名
	if rec := patch(sp.ID, pa.ID.String()); rec.Code != 200 {
		t.Fatalf("关联失败: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := speakers.Get(ctx, sp.ID); got.Name != "持有人甲" {
		t.Fatalf("关联后声纹名应同步为「持有人甲」，得: %q", got.Name)
	}

	// ② 换绑：sp → 乙（转移语义，原持有人甲被清，不撞 speaker_id 唯一键）
	if rec := patch(sp.ID, pb.ID.String()); rec.Code != 200 {
		t.Fatalf("换绑失败: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := persons.Get(ctx, 1, pa.ID); got.SpeakerID != nil {
		t.Fatalf("换绑后原持有人甲应解绑，得: %v", got.SpeakerID)
	}
	if got, _ := persons.Get(ctx, 1, pb.ID); got.SpeakerID == nil || *got.SpeakerID != sp.ID {
		t.Fatalf("换绑后乙应持有声纹，得: %v", got.SpeakerID)
	}
	if got, _ := speakers.Get(ctx, sp.ID); got.Name != "目标人物乙" {
		t.Fatalf("换绑后声纹名应同步为「目标人物乙」，得: %q", got.Name)
	}

	// ③ 解绑：person_id 空串，声纹名保留
	if rec := patch(sp.ID, ""); rec.Code != 200 {
		t.Fatalf("解绑失败: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := persons.Get(ctx, 1, pb.ID); got.SpeakerID != nil {
		t.Fatalf("解绑后乙不应再持有声纹，得: %v", got.SpeakerID)
	}
	if got, _ := speakers.Get(ctx, sp.ID); got.Name != "目标人物乙" {
		t.Fatalf("解绑后声纹名应保留「目标人物乙」，得: %q", got.Name)
	}

	// ④ 冲突：目标人物丙已绑另一条声纹 → 409
	if rec := patch(sp.ID, pc.ID.String()); rec.Code != 409 {
		t.Fatalf("占用冲突应 409: %d %s", rec.Code, rec.Body.String())
	}
}

// TestSpeakerRenameClearsCandidates 改名（=用户采纳候选或手动命名）后清空该说话人候选。
func TestSpeakerRenameClearsCandidates(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.InitForTest()
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
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.InitForTest()
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

// TestSpeakerReassignAll timeline 说话人 chip「切换声纹」：识别错时把本会话内
// 源说话人的全部段一键改判给目标声纹。验证：改判段数、段归属、transcript 作用域
// （另一会话同源说话人的段不受波及）、目标声纹不存在 404、缺参 400。
func TestSpeakerReassignAll(t *testing.T) {
	r, speakers, transcripts, dir := setupSpeakerAPI(t)
	ctx := context.Background()

	// 会话 1：3 段——label 1 两段归 A（随机名，识别错的）、label 2 一段归 B（正确声纹）
	sid, tc, _ := seedEnrollSession(t, transcripts, dir, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "甲一", StartMS: 0, EndMS: 2000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "甲二", StartMS: 2100, EndMS: 4000},
		{SequenceNo: 3, SpeakerLabel: "2", Text: "乙", StartMS: 4100, EndMS: 6000},
	})
	a := &repo.Speaker{Name: "说话人aaaa1", Source: "auto"}
	if err := speakers.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := &repo.Speaker{Name: "李四", Source: "enrolled"}
	if err := speakers.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	// 收尾清理两个测试说话人，防跨包物化污染（对齐既有用例的 cleanup 模式）
	t.Cleanup(func() {
		_ = speakers.Delete(context.Background(), a.ID)
		_ = speakers.Delete(context.Background(), b.ID)
	})
	_ = transcripts.SetSegmentSpeaker(ctx, tc.ID, "1", a.ID)
	_ = transcripts.SetSegmentSpeaker(ctx, tc.ID, "2", b.ID)

	// 会话 2：同源说话人 A 的一段——reassign 按 transcript 作用域，不应跨会话波及
	_, tc2, _ := seedEnrollSession(t, transcripts, dir, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "甲-另会话", StartMS: 0, EndMS: 2000},
	})
	_ = transcripts.SetSegmentSpeaker(ctx, tc2.ID, "1", a.ID)

	// 正常切换：A 的 2 段 → B
	body := fmt.Sprintf(`{"from_speaker_id":"%s","to_speaker_id":"%s"}`, a.ID, b.ID)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+"/speakers/reassign", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("reassign code %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Updated int `json:"updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Updated != 2 {
		t.Fatalf("应改判 2 段，实际 %d", out.Updated)
	}
	// 会话 1 的段全部归 B
	got, _ := transcripts.ListSegments(ctx, tc.ID)
	for _, sg := range got {
		if sg.SpeakerID == nil || *sg.SpeakerID != b.ID {
			t.Fatalf("会话 1 段 %d 应全部改判到 B，实际 %+v", sg.SequenceNo, sg.SpeakerID)
		}
	}
	// 会话 2 的段仍归 A（作用域隔离）
	got2, _ := transcripts.ListSegments(ctx, tc2.ID)
	if len(got2) != 1 || got2[0].SpeakerID == nil || *got2[0].SpeakerID != a.ID {
		t.Fatalf("会话 2 的段不应被波及，实际 %+v", got2)
	}

	// 目标声纹不存在 → 404
	bad := fmt.Sprintf(`{"from_speaker_id":"%s","to_speaker_id":"123"}`, a.ID)
	req2 := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+"/speakers/reassign", bytes.NewBufferString(bad))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != 404 {
		t.Fatalf("目标声纹不存在应 404，实际 %d", rec2.Code)
	}

	// 缺 to_speaker_id → 400
	req3 := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+"/speakers/reassign",
		bytes.NewBufferString(`{"from_speaker_id":"`+a.ID.String()+`"}`))
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)
	if rec3.Code != 400 {
		t.Fatalf("缺 to_speaker_id 应 400，实际 %d", rec3.Code)
	}
}

// TestSpeakerDeleteUnbindsPerson 验证删声纹会解绑关联人物（person 是独立实体，仅清 speaker_id 外键）：
// 此前删声纹只清 transcript_segment.speaker_id，漏了 person.speaker_id，致人物详情仍显示
// 「已关联声纹 / 换绑」指向已删声纹（用户反馈）。
func TestSpeakerDeleteUnbindsPerson(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.InitForTest(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	speakers := &repo.SpeakerRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	sp := &repo.Speaker{Name: "说话人xx", Source: "auto"}
	if err := speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	p := &repo.Person{UserID: 1, DisplayName: "说话人xx", SpeakerID: &sp.ID, Source: "auto"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{Speakers: speakers, Voiceprint: fakeVoiceprintAPI{}, Persons: persons})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/speakers/"+sp.ID.String(), nil))
	if rec.Code != 204 {
		t.Fatalf("delete code %d", rec.Code)
	}

	// 人物仍在，但 speaker_id 已解绑（nil）
	got, err := persons.Get(ctx, 1, p.ID)
	if err != nil {
		t.Fatalf("人物应仍在: %v", err)
	}
	if got.SpeakerID != nil {
		t.Errorf("删声纹后人物应解绑 speaker_id=nil, got %v", *got.SpeakerID)
	}
}

// TestDeleteSpeakerCascade 删声纹时级联处理关联人物（Persons+ChangeLogs 都装配才启用）：
//   ① 关联的是「未编辑过的 LLM 自动人物」→ 随声纹一并软删（status=dismissed），声纹删成功（204）；
//   ② 关联的是「编辑过 / 手动创建的人物」→ 声纹照常删除（200, ok=true, prompts 非空），
//      人物保留，待确认提示随响应带回由前端提示用户。
func TestDeleteSpeakerCascade(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.InitForTest(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	speakers := &repo.SpeakerRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	changeLogs := &repo.PersonChangeLogRepo{DB: db}

	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		Persons: persons, ChangeLogs: changeLogs,
	})

	del := func(spID ids.ID) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/speakers/"+spID.String(), nil))
		return rec
	}

	// ── ① 未编辑的 LLM 人物：级联 dismiss + 声纹删成功 ──
	sp1 := &repo.Speaker{Name: "级联测试甲x", Source: "auto"}
	if err := speakers.Create(ctx, sp1); err != nil {
		t.Fatal(err)
	}
	p1 := &repo.Person{UserID: 1, DisplayName: "级联测试甲x", SpeakerID: &sp1.ID, Source: "llm"}
	if err := persons.Create(ctx, p1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persons.SetStatus(context.Background(), p1.ID, "dismissed") })

	if rec := del(sp1.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("未编辑 LLM 人物：应删成功 204，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := speakers.Get(ctx, sp1.ID); err == nil {
		t.Fatal("声纹应已被删除")
	}
	if got, _ := persons.Get(ctx, 1, p1.ID); got == nil || got.Status != "dismissed" {
		t.Fatalf("未编辑 LLM 人物应被级联 dismiss，实际 %+v", got)
	}

	// ── ② 编辑过的 LLM 人物：声纹照常删除 + 返回待确认提示（人物保留） ──
	sp2 := &repo.Speaker{Name: "级联测试乙x", Source: "auto"}
	if err := speakers.Create(ctx, sp2); err != nil {
		t.Fatal(err)
	}
	p2 := &repo.Person{UserID: 1, DisplayName: "级联测试乙x", SpeakerID: &sp2.ID, Source: "llm"}
	if err := persons.Create(ctx, p2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = persons.SetStatus(context.Background(), p2.ID, "dismissed")
		_ = speakers.Delete(context.Background(), sp2.ID)
	})
	// 追加一条非 create 的审计 → 视为「被编辑过」
	if err := changeLogs.Create(ctx, &repo.PersonChangeLog{
		UserID: 1, PersonID: p2.ID, EntityKind: "person", ChangeType: "update", ChangedBy: "user",
	}); err != nil {
		t.Fatal(err)
	}

	rec := del(sp2.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("编辑过的人物：应返回确认提示 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK      bool `json:"ok"`
		Prompts []struct {
			PersonID string `json:"person_id"`
			Name     string `json:"name"`
			Reason   string `json:"reason"`
		} `json:"prompts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("声纹已删成功时 ok 应为 true，实际 %+v", out)
	}
	if len(out.Prompts) != 1 || out.Prompts[0].PersonID != p2.ID.String() {
		t.Fatalf("应返回 p2 的确认提示，实际 %+v", out.Prompts)
	}
	// 声纹照常删除；人物保留待用户确认
	if _, err := speakers.Get(ctx, sp2.ID); err == nil {
		t.Fatal("声纹应已被删除（不因人物确认而中止）")
	}
	if got, _ := persons.Get(ctx, 1, p2.ID); got == nil || got.Status == "dismissed" {
		t.Fatalf("有确认提示时人物不应被 dismiss，实际 %+v", got)
	}
}
