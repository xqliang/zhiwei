package api

import (
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func setupAPI(t *testing.T) http.Handler {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := ids.Init(1); err != nil {
		t.Fatal(err)
	}
	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	r := chi.NewRouter()
	RegisterAudio(r, sessions, jobs, dir)
	return r
}

func TestUploadAudio(t *testing.T) {
	handler := setupAPI(t)

	var body strings.Builder
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "test.webm")
	_, _ = fw.Write([]byte("fake-audio-bytes"))
	_ = mw.WriteField("source", "web_record")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/audio", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 got %d: %s", rec.Code, rec.Body.String())
	}
	resp := rec.Body.String()
	if !strings.Contains(resp, `"session_id"`) || !strings.Contains(resp, `"job_id"`) {
		t.Fatalf("resp = %s", resp)
	}
	_ = os.ReadDir(filepath.Dir("data"))
}
