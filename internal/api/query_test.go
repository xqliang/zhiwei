package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// setupQueryAPI 构造挂载了查询路由的测试 handler。
// Sprint 2：详情需附带 memories/todos，因此注入两个新 repo。
func setupQueryAPI(t *testing.T, s *repo.SessionRepo, j *repo.JobRepo,
	tr *repo.TranscriptRepo, m *repo.MemoryRepo, td *repo.TodoRepo) http.Handler {
	t.Helper()
	_ = ids.Init(1)
	r := chi.NewRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: s, Jobs: j, Transcripts: tr, Memories: m, Todos: td,
	})
	return r
}

func TestSessionsAndDetail(t *testing.T) {
	_ = ids.Init(1) // 幂等初始化，避免依赖其它测试先跑
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

	// Sprint 2：插入 memory 与 todo，验证详情接口附带返回
	memories := &repo.MemoryRepo{DB: db}
	todos := &repo.TodoRepo{DB: db}
	eventAt := time.Now()
	_ = memories.InsertExt(ctx, db, []*repo.Memory{{
		Type: "event", Title: "装配用例发邮件", Content: "明天记得给 Tom 发邮件确认设计稿",
		EpistemicType: "observed", Confidence: 0.9, SessionID: sid,
		EventAt: &eventAt, Status: "active",
	}})
	memRows, _ := memories.ListBySession(ctx, sid)
	_ = todos.InsertExt(ctx, db, []*repo.Todo{{
		Title: "装配用例给 Tom 发邮件", SourceMemoryID: &memRows[0].ID, Status: "confirmed",
		Confidence: 0.9,
	}})

	handler := setupQueryAPI(t, sessions, jobs, transcripts, memories, todos)

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
	for _, want := range []string{`"segments"`, "明天记得发邮件", "说话人 1",
		`"memories"`, `"todos"`, "装配用例发邮件"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail body 缺少 %s: %s", want, body)
		}
	}
}

// ServeAudio 流式返回原始音频文件，支持点击播放
func TestServeAudio(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	memories := &repo.MemoryRepo{DB: db}
	todos := &repo.TodoRepo{DB: db}

	// 临时音频文件（4 字节 WAV 头足以验证流式返回，无需可播放内容）
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "clip.wav")
	if err := os.WriteFile(audioPath, []byte("RIFFxxxxWAVEfmt "), 0o644); err != nil {
		t.Fatal(err)
	}
	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "clip.wav",
		StoragePath: audioPath, Mime: "audio/wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}

	handler := setupQueryAPI(t, sessions, jobs, transcripts, memories, todos)

	// 正常返回音频内容
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/sessions/"+sid.String()+"/audio", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("audio: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("音频响应体为空")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("Content-Type = %s, want audio/wav", ct)
	}

	// 不存在的 session → 404
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet,
		"/api/sessions/"+ids.New().String()+"/audio", nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", rec2.Code)
	}
}
