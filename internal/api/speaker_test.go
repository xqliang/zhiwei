package api

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
