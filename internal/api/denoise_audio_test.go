package api

// denoise_audio_test.go：timeline 播放降噪预览两端点——GET /audio?denoised=1（有产物
// 200 / 无产物 404）与 POST /denoise-audio（按需生成、幂等）。Denoiser 用 fake。
import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/auth"
	"zhiwei/internal/ids"
	"zhiwei/internal/pipeline"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// fakeAPIDenoiser api 测试用降噪桩：dst 落盘占位文件（ServeAudio 靠 Stat 判存在）。
type fakeAPIDenoiser struct{ calls int }

func (f *fakeAPIDenoiser) Denoise(_ context.Context, _, dst string, _ float64) error {
	f.calls++
	return os.WriteFile(dst, []byte("denoised-wav"), 0o644)
}

func setupDenoiseAudioAPI(t *testing.T) (http.Handler, *repo.SessionRepo, string, *fakeAPIDenoiser, ids.ID) {
	t.Helper()
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	asrSettings := &repo.AsrSettingsRepo{DB: db}
	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, UserID: 1, Source: "web_upload", Filename: "a.wav",
		StoragePath: "/tmp/a.wav", Status: "completed", Mime: "audio/webm",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM audio_session WHERE id = ?", sid.Int64()) })

	dataDir := t.TempDir()
	dn := &fakeAPIDenoiser{}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), 1)))
		})
	})
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, DataDir: dataDir, AsrSettings: asrSettings, Denoise: dn,
	})
	return r, sessions, dataDir, dn, sid
}

// TestDenoiseAudioPreviewAndServe：POST 按需生成（幂等：二次不重跑）→ GET ?denoised=1
// 200 播产物；未生成过的另一 session → GET 404、原始 GET 照常 200（原上传文件）。
func TestDenoiseAudioPreviewAndServe(t *testing.T) {
	r, sessions, dataDir, dn, sid := setupDenoiseAudioAPI(t)
	ctx := context.Background()

	// 前置：transcoded wav 存在（POST 的生成源）；原始上传文件存在（默认 GET 的源）
	tDir := filepath.Join(dataDir, "transcoded")
	if err := os.MkdirAll(tDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tDir, sid.String()+".wav"), []byte("orig-wav"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/tmp/a.wav", []byte("orig-upload"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove("/tmp/a.wav") })

	// 1) 未生成 → GET ?denoised=1 404；默认 GET 200（原始上传）
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String()+"/audio?denoised=1", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未生成时 ?denoised 应 404，code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String()+"/audio", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("默认 GET 应 200，code=%d", rec.Code)
	}

	// 2) POST 生成 → generated=true；产物落盘；二次 POST 幂等（generated=false 不重跑）
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+"/denoise-audio", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"generated":true`) {
		t.Fatalf("POST 首次应 200+generated=true，code=%d body=%s", rec.Code, rec.Body.String())
	}
	if dn.calls != 1 {
		t.Fatalf("应调一次降噪，实际 %d", dn.calls)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+"/denoise-audio", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"generated":false`) {
		t.Fatalf("POST 二次应 200+generated=false（幂等），code=%d body=%s", rec.Code, rec.Body.String())
	}
	if dn.calls != 1 {
		t.Fatalf("幂等：不应重复降噪，实际 %d 次", dn.calls)
	}

	// 3) 生成后 → GET ?denoised=1 200 且内容为降噪产物
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String()+"/audio?denoised=1", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "denoised-wav" {
		t.Fatalf("?denoised 应 200 且播降噪产物，code=%d body=%q", rec.Code, rec.Body.String())
	}
	_ = sessions
	_ = ctx
}

// TestDenoiseAudioNotAssembled：Denoise 未装配 → POST 503（不 500）。
func TestDenoiseAudioNotAssembled(t *testing.T) {
	_, sessions, _, _, sid := setupDenoiseAudioAPI(t)
	// 重新挂一个未装配 Denoise/DataDir 的路由（Sessions 有——鉴权与归属校验正常走）
	r2 := chi.NewRouter()
	r2.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), 1)))
		})
	})
	RegisterQuery(r2, &QueryHandler{Sessions: sessions})
	rec := httptest.NewRecorder()
	r2.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+"/denoise-audio", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503，code=%d", rec.Code)
	}
}

var _ pipeline.Denoiser = (*fakeAPIDenoiser)(nil)
