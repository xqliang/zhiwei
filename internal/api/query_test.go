package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func setupQueryAPI(t *testing.T, s *repo.SessionRepo, j *repo.JobRepo, tr *repo.TranscriptRepo) http.Handler {
	t.Helper()
	_ = ids.Init(1)
	r := chi.NewRouter()
	RegisterQuery(r, &QueryHandler{Sessions: s, Jobs: j, Transcripts: tr})
	return r
}

func TestSessionsAndDetail(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "a.wav",
		StoragePath: "/tmp/a.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	conf := 0.95
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1",
			Text: "明天记得发邮件", StartMS: 0, EndMS: 1000, Confidence: &conf},
	}); err != nil {
		t.Fatal(err)
	}

	handler := setupQueryAPI(t, sessions, jobs, transcripts)

	// 列表
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Sessions []map[string]any `json:"sessions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Sessions) < 1 {
		t.Fatal("sessions 为空")
	}

	// 详情
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil)
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec2.Code, rec2.Body.String())
	}
	body := rec2.Body.String()
	for _, want := range []string{`"segments"`, "明天记得发邮件", "说话人 1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail body 缺少 %s: %s", want, body)
		}
	}
}
