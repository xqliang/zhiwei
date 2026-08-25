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

// TestParseDateOrTodayLocation 验证 I2：「今天」(time.Now) 与显式日期解析后落在同一时区
//（均为 time.Local），避免与 dayRange 组合时切出错位窗口。纯单元（无需 DB）。
func TestParseDateOrTodayLocation(t *testing.T) {
	now, ok := parseDateOrToday("")
	if !ok {
		t.Fatal("空日期应 ok")
	}
	explicit, ok := parseDateOrToday("2026-08-25")
	if !ok {
		t.Fatal("合法显式日期应 ok")
	}
	if now.Location() != time.Local {
		t.Errorf("time.Now() 应在 time.Local, got %s", now.Location())
	}
	if explicit.Location() != time.Local {
		t.Errorf("显式日期应用 time.Local 解析, got %s", explicit.Location())
	}
	if now.Location() != explicit.Location() {
		t.Errorf("两者时区应一致: now=%s explicit=%s", now.Location(), explicit.Location())
	}
}

// TestGetTopicStatusNotFound404 验证 M4：不存在的话题 → 404（而非默认 502）。
// 需要独立库（无 DSN 自动跳过）。
func TestGetTopicStatusNotFound404(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置")
	}
	db, err := repo.NewDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	gen := &review.Generator{
		LLM: stubLLM{reply: `{"summary":"x"}`}, Model: "m", TopicStatusPrompt: "S",
		Reviews: &repo.ReviewRepo{DB: db}, TopicStatuses: &repo.TopicStatusRepo{DB: db},
		Memories: &repo.MemoryRepo{DB: db}, Todos: &repo.TodoRepo{DB: db}, Topics: &repo.TopicRepo{DB: db},
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
	}
	r := chi.NewRouter()
	RegisterReviews(r, gen)
	// 用一个几乎不可能存在的话题 id（雪花新 id）
	req := httptest.NewRequest(http.MethodGet, "/api/topics/"+ids.New().String()+"/status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在话题应 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}
