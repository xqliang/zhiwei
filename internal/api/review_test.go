package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/review"
)

func TestMain(m *testing.M) { _ = ids.Init(1); os.Exit(m.Run()) }

type stubLLM struct{ reply string }

func (s stubLLM) Chat(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{Content: s.reply}, nil
}

func TestGetDailyGeneratesWhenMissing(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置")
	}
	db, err := repo.NewDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	gen := &review.Generator{
		LLM: stubLLM{reply: `{"headline":"HTTP 日报"}`}, Model: "m", DailyPrompt: "S",
		Reviews: &repo.ReviewRepo{DB: db}, TopicStatuses: &repo.TopicStatusRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Todos: &repo.TodoRepo{DB: db}, Topics: &repo.TopicRepo{DB: db},
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
	}
	r := chi.NewRouter()
	RegisterReviews(r, gen)
	day, _ := time.Parse("2006-01-02", "2031-02-02")
	t.Cleanup(func() { _ = gen.Reviews.UpsertDaily(context.Background(), 1, day, nil, "pending") })

	req := httptest.NewRequest(http.MethodGet, "/api/reviews/daily?date=2031-02-02", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("状态码 %d, body=%s", rec.Code, rec.Body.String())
	}
	var row repo.DailyReview
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil || row.Status != "ready" {
		t.Errorf("应返回 ready 日报: %s (err=%v)", rec.Body.String(), err)
	}
}

func TestGetDailyBadDate(t *testing.T) {
	gen := &review.Generator{} // 400 在触库前返回，无需 DB
	r := chi.NewRouter()
	RegisterReviews(r, gen)
	req := httptest.NewRequest(http.MethodGet, "/api/reviews/daily?date=notadate", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法日期应 400, got %d", rec.Code)
	}
}
