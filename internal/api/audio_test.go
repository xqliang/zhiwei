package api

import (
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	if err := ids.InitForTest(); err != nil {
		t.Fatal(err)
	}
	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	// 上传 handler 现要求登录态（评审 I3）：用注入 uid=1 的测试路由，
	// 使 Upload 能取到登录用户并写 user_id。
	r := newAuthedRouter()
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
}
