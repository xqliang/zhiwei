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
		EpistemicType: "observed", Confidence: 0.9, SessionID: &sid,
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

// TestGetSessionSpeakerEnrichment 验证详情接口的说话人富化：
// 段已解析到登记说话人 → segment 带 speaker_id + 登记名（非回退 "说话人 N"），
// 顶层 speakers 列表含该说话人。
func TestGetSessionSpeakerEnrichment(t *testing.T) {
	_ = ids.Init(1)
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
	speakers := &repo.SpeakerRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "spk.wav",
		StoragePath: "/tmp/spk.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	conf := 0.9
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1",
			Text: "你好", StartMS: 0, EndMS: 1000, Confidence: &conf},
	}); err != nil {
		t.Fatal(err)
	}
	// 登记 1 个说话人并回填到 label "1" 的段
	sp := &repo.Speaker{Name: "张三", Source: "enrolled"}
	if err := speakers.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", sp.ID); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers,
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{sp.ID.String(), "张三", `"speakers"`, `"speaker_id"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body 缺少 %s: %s", want, body)
		}
	}
	// 已解析到登记名，不应回退成 "说话人 1"
	if strings.Contains(body, "说话人 1") {
		t.Fatalf("应显示登记名而非回退: %s", body)
	}

	// 解析 speakers 列表，确认含张三 + color_index
	var detail struct {
		Speakers []struct {
			Name       string `json:"name"`
			ColorIndex int    `json:"color_index"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("json: %v", err)
	}
	found := false
	for _, s := range detail.Speakers {
		if s.Name == "张三" {
			found = true
		}
	}
	if !found {
		t.Fatalf("speakers 列表无张三: %+v", detail.Speakers)
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

// buildEnrichedSession 构造 1 session + 1 段转写 + 1 active memory + 1 confirmed todo，
// 供 ListSessions 富化与 DeleteSession 级联测试共用。返回 router+session id+SessionRepo
// （含 DB 句柄供断言）。
func buildEnrichedSession(t *testing.T) (http.Handler, ids.ID, *repo.SessionRepo) {
	_ = ids.Init(1)
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

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "enriched.wav",
		StoragePath: "/tmp/enriched.wav", Status: "completed",
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
			Text: "明天记得发邮件确认设计稿", StartMS: 0, EndMS: 1000, Confidence: &conf},
	}); err != nil {
		t.Fatal(err)
	}
	eventAt := time.Now()
	_ = memories.InsertExt(ctx, db, []*repo.Memory{{
		Type: "event", Title: "富化用例发邮件", Content: "明天记得给 Tom 发邮件",
		EpistemicType: "observed", Confidence: 0.9, SessionID: &sid,
		EventAt: &eventAt, Status: "active",
	}})
	memRows, _ := memories.ListBySession(ctx, sid)
	_ = todos.InsertExt(ctx, db, []*repo.Todo{{
		Title: "富化用例给 Tom 发邮件", SourceMemoryID: &memRows[0].ID, Status: "confirmed", Confidence: 0.9,
	}})

	r := chi.NewRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts, Memories: memories, Todos: todos,
	})
	return r, sid, sessions
}

// TestListSessionsEnriched 验证 ListSessions 富化字段：asr_preview 含转写文本、
// memory_count/todo_count 各 1。按 session id 精确定位，避免脏库其他行干扰。
func TestListSessionsEnriched(t *testing.T) {
	r, sid, _ := buildEnrichedSession(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Sessions []struct {
			ID          string `json:"id"`
			AsrPreview  string `json:"asr_preview"`
			MemoryCount int    `json:"memory_count"`
			TodoCount   int    `json:"todo_count"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	found := false
	for _, s := range resp.Sessions {
		if s.ID == sid.String() {
			found = true
			if !strings.Contains(s.AsrPreview, "明天记得发邮件") {
				t.Fatalf("asr_preview=%s", s.AsrPreview)
			}
			if s.MemoryCount != 1 {
				t.Fatalf("memory_count=%d, want 1", s.MemoryCount)
			}
			if s.TodoCount != 1 {
				t.Fatalf("todo_count=%d, want 1", s.TodoCount)
			}
		}
	}
	if !found {
		t.Fatalf("session %s missing: %s", sid, rec.Body.String())
	}
}

// TestDeleteSession 验证 DELETE session 级联：audio_session/memory/transcript/todo 均删。
func TestDeleteSession(t *testing.T) {
	r, sid, sr := buildEnrichedSession(t)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	// 级联断言：四类行均 0（todo 经 source_memory_id 子查询，memory 删后子查询空→0）
	checks := []struct{ name, sql string }{
		{"audio_session", `SELECT COUNT(*) FROM audio_session WHERE id = ?`},
		{"memory", `SELECT COUNT(*) FROM memory WHERE session_id = ?`},
		{"transcript", `SELECT COUNT(*) FROM transcript WHERE session_id = ?`},
		{"todo", `SELECT COUNT(*) FROM todo WHERE source_memory_id IN (SELECT id FROM memory WHERE session_id = ?)`},
	}
	for _, c := range checks {
		var n int
		if err := sr.DB.GetContext(ctx, &n, c.sql, sid.Int64()); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n != 0 {
			t.Fatalf("%s 残留 %d", c.name, n)
		}
	}
}

// TestPatchTranscript 验证 ASR 就地编辑：PATCH 段文本后，再取详情段文本已更新。
func TestPatchTranscript(t *testing.T) {
	r, sid, _ := buildEnrichedSession(t)

	// GET 详情拿首个 segment id
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Segments []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || len(detail.Segments) == 0 {
		t.Fatalf("detail 解析: %v %s", err, rec.Body.String())
	}
	seg := detail.Segments[0]
	if seg.ID == "" {
		t.Fatalf("segment 缺 id: %s", rec.Body.String())
	}

	// PATCH 改文本
	body, _ := json.Marshal(map[string]any{
		"segments": []map[string]string{{"id": seg.ID, "text": "修正后的转写文本"}},
	})
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodPatch,
		"/api/sessions/"+sid.String()+"/transcript", strings.NewReader(string(body))))
	if rec2.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec2.Code, rec2.Body.String())
	}

	// 再 GET 验证段文本已更新
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if !strings.Contains(rec3.Body.String(), "修正后的转写文本") {
		t.Fatalf("段文本未更新: %s", rec3.Body.String())
	}

	// 不存在的 session → 404
	rec4 := httptest.NewRecorder()
	r.ServeHTTP(rec4, httptest.NewRequest(http.MethodPatch,
		"/api/sessions/"+ids.New().String()+"/transcript", strings.NewReader(`{"segments":[]}`)))
	if rec4.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", rec4.Code)
	}
}

// TestReextract 验证重新提取建 job：成功→job_id 返回 + session 指向新 job（stage=segment, pending）；
// 无转写 session→409；不存在 session→404。测试无 pool 运行，job 保持 pending 可稳定断言。
func TestReextract(t *testing.T) {
	r, sid, sessions := buildEnrichedSession(t)
	ctx := context.Background()

	// 成功：POST reextract
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/sessions/"+sid.String()+"/reextract", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reextract: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		JobID any `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.JobID == nil {
		t.Fatalf("缺 job_id: %v %s", err, rec.Body.String())
	}

	// GET 详情：job.stage=segment、status=pending（无 pool，job 保持 pending）
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	body := rec2.Body.String()
	if !strings.Contains(body, `"stage":"segment"`) {
		t.Fatalf("job.stage 不是 segment: %s", body)
	}
	if !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("job.status 不是 pending: %s", body)
	}

	// 无转写的 session → 409（建一个裸 session，无 transcript）
	bareSID := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: bareSID, Source: "web_upload", Filename: "bare.wav",
		StoragePath: "/tmp/bare.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodPost,
		"/api/sessions/"+bareSID.String()+"/reextract", nil))
	if rec3.Code != http.StatusConflict {
		t.Fatalf("无转写应 409, got %d %s", rec3.Code, rec3.Body.String())
	}

	// 不存在的 session → 404
	rec4 := httptest.NewRecorder()
	r.ServeHTTP(rec4, httptest.NewRequest(http.MethodPost,
		"/api/sessions/"+ids.New().String()+"/reextract", nil))
	if rec4.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", rec4.Code)
	}
}
