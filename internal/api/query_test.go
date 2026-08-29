package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/auth"
	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// newAuthedRouter 返回预装「登录态注入」中间件的测试路由（api 包测试共用）。
// 阶段1 起，A 类 handler（memory/todo/topic/query）会 auth.UserID(ctx) 取登录用户，
// 无登录态直接 401。测试 fixture 默认以 user_id=1 落库，故这里统一把请求上下文注入
// owner=1，使经此路由的请求带上登录用户 1，行为与多租户接线前保持一致。
func newAuthedRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), 1)))
		})
	})
	return r
}

// setupQueryAPI 构造挂载了查询路由的测试 handler。
// Sprint 2：详情需附带 memories/todos，因此注入两个新 repo。
func setupQueryAPI(t *testing.T, s *repo.SessionRepo, j *repo.JobRepo,
	tr *repo.TranscriptRepo, m *repo.MemoryRepo, td *repo.TodoRepo) http.Handler {
	t.Helper()
	_ = ids.Init(1)
	r := newAuthedRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: s, Jobs: j, Transcripts: tr, Memories: m, Todos: td,
	})
	return r
}

func TestSessionsAndDetail(t *testing.T) {
	_ = ids.InitForTest() // 幂等初始化，避免依赖其它测试先跑
	db, err := repo.NewDB(repotest.DSN(t))
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
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
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
	// 该 speaker 全程 active 且不绑定任何 person，会残留在共享 zhiwei_test 库里。repo 包
	// （字典序在本包之后）TestPersonLifecycle 跑 EnsurePersonBootstrap 时，会把每个未绑定的
	// active speaker 物化成同名 active person——于是凭空多出一个 id 更小的「张三」person，令
	// 该用例的 FindByName(张三) 命中错误行。收尾删掉这个 speaker，堵住物化来源。
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), sp.ID) })
	if err := transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", sp.ID); err != nil {
		t.Fatal(err)
	}

	r := newAuthedRouter()
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

// TestGetSessionAcousticFields 验证详情接口返回 audioscene stage 落库的声学环境
// （transcript 的 4 个环境列）+ 说话人整体情绪状态（speaker_session_state，行级 user_id 过滤）。
func TestGetSessionAcousticFields(t *testing.T) {
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	speakerStates := &repo.SpeakerSessionStateRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "scene.wav",
		StoragePath: "/tmp/scene.wav", Status: "completed",
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
			Text: "今天开会", StartMS: 0, EndMS: 1000, Confidence: &conf},
	}); err != nil {
		t.Fatal(err)
	}
	// 写会话级声学环境（audioscene stage 用 SetAcoustic 落库）
	bg := json.RawMessage(`["键盘","空调"]`)
	if err := transcripts.SetAcoustic(ctx, tr.ID, "室内会议室", &bg, "无", "专注讨论"); err != nil {
		t.Fatal(err)
	}
	// 写说话人整体情绪（user_id=1 与测试登录态一致；另写 user_id=2 一行验证行级过滤）
	if err := speakerStates.InsertBatch(ctx, []repo.SpeakerSessionState{
		{UserID: 1, TranscriptID: tr.ID, SessionID: sid, SpeakerLabel: "1",
			Emotion: "平静", MicroEmotion: "专注", MentalState: "投入", Confidence: 0.85},
		{UserID: 2, TranscriptID: tr.ID, SessionID: sid, SpeakerLabel: "2",
			Emotion: "焦虑", Confidence: 0.6},
	}); err != nil {
		t.Fatal(err)
	}

	r := newAuthedRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		SpeakerStates: speakerStates,
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}

	var detail struct {
		AcousticScene    string          `json:"acoustic_scene"`
		BackgroundSounds json.RawMessage `json:"background_sounds"`
		WeatherCues      string          `json:"weather_cues"`
		OverallMood      string          `json:"overall_mood"`
		SpeakerStates    []struct {
			SpeakerLabel string `json:"speaker_label"`
			Emotion      string `json:"emotion"`
		} `json:"speaker_states"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("json: %v", err)
	}
	if detail.AcousticScene != "室内会议室" {
		t.Errorf("acoustic_scene=%q, want 室内会议室", detail.AcousticScene)
	}
	if detail.WeatherCues != "无" || detail.OverallMood != "专注讨论" {
		t.Errorf("weather/mood 未返回: %+v", detail)
	}
	if len(detail.BackgroundSounds) == 0 || !strings.Contains(string(detail.BackgroundSounds), "键盘") {
		t.Errorf("background_sounds 未返回: %s", string(detail.BackgroundSounds))
	}
	// 行级过滤：登录用户 1 只应看到自己的 1 行（user_id=2 的不返回）
	if len(detail.SpeakerStates) != 1 {
		t.Fatalf("speaker_states 应 1 行（行级 user_id 过滤）, got %d: %+v", len(detail.SpeakerStates), detail.SpeakerStates)
	}
	if detail.SpeakerStates[0].Emotion != "平静" {
		t.Errorf("speaker_states[0].emotion=%q, want 平静", detail.SpeakerStates[0].Emotion)
	}
}

// TestGetSessionSpeakerStateName 验证「在场情绪」按已解析 speaker_id 回显正式名：
// 声纹匹配把说话人解析成登记名（如 Allen）后，speaker_session_state 仍只存原始 label
// （speaker_0），API 必须像 segments 那样用 spMap 把 speaker_id 解析成 name 一并返回，
// 前端才能显示「Allen: 困惑」而非「speaker_0」。speaker_id 为空的情绪行回退到 label 名。
func TestGetSessionSpeakerStateName(t *testing.T) {
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}
	speakerStates := &repo.SpeakerSessionStateRepo{DB: db}

	// 登记一个已解析说话人 Allen
	allen := &repo.Speaker{Name: "Allen", Source: "enrolled"}
	if err := speakers.Create(ctx, allen); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), allen.ID) })

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "emo.wav",
		StoragePath: "/tmp/emo.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	// 两段：段1归属 Allen（speaker_id 已解析），段2未解析（speaker_label=2，无 speaker_id）
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "今天开会", StartMS: 0, EndMS: 1000},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "在的", StartMS: 1000, EndMS: 2000},
	}); err != nil {
		t.Fatal(err)
	}
	// 段1 归属到 Allen（模拟 speaker stage 声纹匹配后 SetSegmentSpeaker）
	if err := transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", allen.ID); err != nil {
		t.Fatal(err)
	}
	// 写两行情绪：行1 speaker_id=Allen（解析后回填），行2 无 speaker_id（未解析）
	if err := speakerStates.InsertBatch(ctx, []repo.SpeakerSessionState{
		{UserID: 1, TranscriptID: tr.ID, SessionID: sid, SpeakerLabel: "1", SpeakerID: &allen.ID,
			Emotion: "困惑", Confidence: 0.8},
		{UserID: 1, TranscriptID: tr.ID, SessionID: sid, SpeakerLabel: "2",
			Emotion: "平静", Confidence: 0.7},
	}); err != nil {
		t.Fatal(err)
	}

	r := newAuthedRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Transcripts: transcripts, Speakers: speakers,
		SpeakerStates: speakerStates,
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		SpeakerStates []struct {
			SpeakerLabel string `json:"speaker_label"`
			SpeakerName  string `json:"speaker_name"`
			Emotion      string `json:"emotion"`
		} `json:"speaker_states"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(detail.SpeakerStates) != 2 {
		t.Fatalf("speaker_states 应 2 行, got %d: %+v", len(detail.SpeakerStates), detail.SpeakerStates)
	}
	// 关键断言：speaker_id 解析过的行须回显正式名 Allen，而非原始 label
	if detail.SpeakerStates[0].SpeakerName != "Allen" {
		t.Errorf("speaker_states[0].speaker_name=%q, want Allen（已解析 speaker_id 须回显正式名）", detail.SpeakerStates[0].SpeakerName)
	}
	// 未解析行回退到「说话人 2」
	if detail.SpeakerStates[1].SpeakerName != "说话人 2" {
		t.Errorf("speaker_states[1].speaker_name=%q, want 说话人 2（speaker_id 为空回退 label）", detail.SpeakerStates[1].SpeakerName)
	}
}

// TestGetSessionNameCandidates 验证详情接口 speakers[] 富化名字候选：
// 随机名说话人带倒序候选（张总 0.82 在首、evidence 透传），真名说话人为空数组。
// speakers[] 由 ListSpeakersForTranscript 按本 transcript 段归属聚合，天然作用域到本会话，
// 不受脏库其它说话人干扰，因此无需清表。
func TestGetSessionNameCandidates(t *testing.T) {
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
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
	candidates := &repo.SpeakerNameCandidateRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "cand.wav",
		StoragePath: "/tmp/cand.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	conf := 0.9
	// 两段：段1(label "1")归随机名说话人、段2(label "2")归真名说话人——
	// 均需有段归属才会出现在 speakers[]（ListSpeakersForTranscript 只聚合有段的说话人）。
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1",
			Text: "你好", StartMS: 0, EndMS: 1000, Confidence: &conf},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "2",
			Text: "在的", StartMS: 1000, EndMS: 2000, Confidence: &conf},
	}); err != nil {
		t.Fatal(err)
	}
	randSp := &repo.Speaker{Name: "说话人ab3x9", Source: "auto"}
	if err := speakers.Create(ctx, randSp); err != nil {
		t.Fatal(err)
	}
	namedSp := &repo.Speaker{Name: "张三", Source: "enrolled"}
	if err := speakers.Create(ctx, namedSp); err != nil {
		t.Fatal(err)
	}
	// 收尾删掉这个「张三」speaker：repo 包的 TestPersonLifecycle 会经
	// EnsurePersonBootstrap 把未绑定的 active 同名 speaker 物化成 person，
	// 残留会让其 FindByName(张三) 命中错误行（跨包共享 zhiwei_test 库）。
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), namedSp.ID) })
	if err := transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", randSp.ID); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.SetSegmentSpeaker(ctx, tr.ID, "2", namedSp.ID); err != nil {
		t.Fatal(err)
	}
	// 仅给随机名说话人上候选（真名说话人无候选 → 应为空数组）
	_ = candidates.Upsert(ctx, randSp.ID, "张总", 0.82, "对方称呼张总", sid)
	_ = candidates.Upsert(ctx, randSp.ID, "张明", 0.4, "", sid)

	r := newAuthedRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers,
		SpeakerNameCandidates: candidates,
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Speakers []struct {
			SpeakerID      string `json:"speaker_id"`
			Name           string `json:"name"`
			NameCandidates []struct {
				Name       string  `json:"name"`
				Confidence float64 `json:"confidence"`
				Evidence   string  `json:"evidence"`
			} `json:"name_candidates"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	// 按 speaker_id 定位两个说话人（避免依赖返回顺序）
	var rand, named *struct {
		SpeakerID      string `json:"speaker_id"`
		Name           string `json:"name"`
		NameCandidates []struct {
			Name       string  `json:"name"`
			Confidence float64 `json:"confidence"`
			Evidence   string  `json:"evidence"`
		} `json:"name_candidates"`
	}
	for i := range detail.Speakers {
		switch detail.Speakers[i].SpeakerID {
		case randSp.ID.String():
			rand = &detail.Speakers[i]
		case namedSp.ID.String():
			named = &detail.Speakers[i]
		}
	}
	if rand == nil || named == nil {
		t.Fatalf("speakers 列表缺随机名/真名说话人: %+v", detail.Speakers)
	}
	// 随机名说话人：恰 2 条候选、倒序（张总 0.82 在首）、evidence 透传
	if len(rand.NameCandidates) != 2 {
		t.Fatalf("随机名说话人应 2 条候选，实际 %d: %+v", len(rand.NameCandidates), rand.NameCandidates)
	}
	if rand.NameCandidates[0].Name != "张总" || rand.NameCandidates[0].Confidence != 0.82 {
		t.Fatalf("候选应倒序（张总 0.82 在首），实际 %+v", rand.NameCandidates)
	}
	if rand.NameCandidates[0].Evidence != "对方称呼张总" {
		t.Fatalf("evidence 未透传，实际 %q", rand.NameCandidates[0].Evidence)
	}
	if rand.NameCandidates[1].Name != "张明" {
		t.Fatalf("次候选应为张明，实际 %+v", rand.NameCandidates)
	}
	// 真名说话人：空数组
	if len(named.NameCandidates) != 0 {
		t.Fatalf("真名说话人应无候选，实际 %d: %+v", len(named.NameCandidates), named.NameCandidates)
	}
}

// TestGetSessionProfileChangeCurrentName 验证 timeline「涉及的画像变更」对人物变更行
// 富化现名（方案 C：审计快照保留原文「老保一家」、另附 person_current_name 供前端标注
// 「现名：X」并做跳转链接）。人物行 person_current_name 始终反映 person 表现名；
// 非人物行（attribute/event 等）不富化（null）。
func TestGetSessionProfileChangeCurrentName(t *testing.T) {
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
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
	persons := &repo.PersonRepo{DB: db}
	changeLogs := &repo.PersonChangeLogRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "pc.wav",
		StoragePath: "/tmp/pc.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	// 人物：先以「老保一家」建 pending（模拟 LLM 抽取粘连名），再改名「老保」
	p := &repo.Person{DisplayName: "老保一家", Source: "llm", Status: "pending"}
	if err := persons.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DELETE FROM person WHERE id = ?", p.ID.Int64()) })
	if err := persons.Update(ctx, p.ID, "老保", nil, nil); err != nil {
		t.Fatal(err)
	}
	// 该录音触发的审计：person create（快照旧名）+ attribute create（非人物行）
	_ = changeLogs.Create(ctx, &repo.PersonChangeLog{
		PersonID: p.ID, EntityKind: "person", ChangeType: "create", ChangedBy: "llm",
		NewValue: strPtr(`"老保一家"`), SessionID: &sid, Note: strPtr("LLM 抽取自动新建人物，待确认"),
	})
	_ = changeLogs.Create(ctx, &repo.PersonChangeLog{
		PersonID: p.ID, EntityKind: "attribute", ChangeType: "create", ChangedBy: "llm",
		AttrKey: strPtr("city"), NewValue: strPtr(`"上海"`), SessionID: &sid,
	})

	r := newAuthedRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers,
		ChangeLogs: changeLogs, Persons: persons,
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		ProfileChanges []struct {
			EntityKind       string  `json:"entity_kind"`
			NewValue         *string `json:"new_value"`
			PersonCurrentName *string `json:"person_current_name"`
		} `json:"profile_changes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	var personRow, attrRow *struct {
		EntityKind       string  `json:"entity_kind"`
		NewValue         *string `json:"new_value"`
		PersonCurrentName *string `json:"person_current_name"`
	}
	for i := range detail.ProfileChanges {
		switch detail.ProfileChanges[i].EntityKind {
		case "person":
			personRow = &detail.ProfileChanges[i]
		case "attribute":
			attrRow = &detail.ProfileChanges[i]
		}
	}
	if personRow == nil || attrRow == nil {
		t.Fatalf("profile_changes 缺 person/attribute 行: %+v", detail.ProfileChanges)
	}
	// 审计快照保留原文
	if personRow.NewValue == nil || !strings.Contains(*personRow.NewValue, "老保一家") {
		t.Errorf("审计快照应保留抽取时原名，实际 %v", personRow.NewValue)
	}
	// 人物行富化现名
	if personRow.PersonCurrentName == nil || *personRow.PersonCurrentName != "老保" {
		t.Errorf("人物行应富化现名「老保」，实际 %v", personRow.PersonCurrentName)
	}
	// 非人物行不富化
	if attrRow.PersonCurrentName != nil {
		t.Errorf("非人物行不应富化现名，实际 %v", *attrRow.PersonCurrentName)
	}
}

func strPtr(s string) *string { return &s }

// ServeAudio 流式返回原始音频文件，支持点击播放
func TestServeAudio(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
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
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
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

	r := newAuthedRouter()
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

// TestListSessionsVoiceTop 验证 timeline 列表「整段声纹」富化（2026-08-26 需求）：
//   - 单人会话（1 个 ASR 标签）→ basis=whole，top3 由全部段向量均值算出；
//   - 多人会话 → basis=longest，用时长最长一段的向量；
//   - 判定走两级规则（voiceprint.Matched）：top1≥0.8 强命中 / ≥0.72 且领先 0.06 弱命中。
//
// 场景构造：库中甲=e1、乙=0.6e1+0.8e2（与 e1 余弦 0.6）、丙=e3（正交）。
func TestListSessionsVoiceTop(t *testing.T) {
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
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

	// 共享库隔离：清历史说话人（one-hot 向量残留会干扰 top-3 精确断言，同 TestGetSessionVoiceMatches）
	for _, q := range []string{`DELETE FROM speaker_name_candidate`, `DELETE FROM speaker`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	e1, e3 := make([]float32, 256), make([]float32, 256)
	e1[0], e3[2] = 1, 1
	ab := make([]float32, 256)
	ab[0], ab[1] = 0.6, 0.8 // 已归一，与 e1 的余弦 = 0.6
	ji := &repo.Speaker{Name: "甲", Source: "enrolled", Embedding: float32BlobAPI(e1), SampleCount: 1}
	yi := &repo.Speaker{Name: "乙", Source: "enrolled", Embedding: float32BlobAPI(ab), SampleCount: 1}
	bing := &repo.Speaker{Name: "丙", Source: "enrolled", Embedding: float32BlobAPI(e3), SampleCount: 1}
	for _, sp := range []*repo.Speaker{ji, yi, bing} {
		if err := speakers.Create(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, sp := range []*repo.Speaker{ji, yi, bing} {
			_ = speakers.Delete(context.Background(), sp.ID)
		}
	})

	// 会话一（单人）：两段同为 label "1"，向量均 e1 → 整段均值=e1 → top3=[甲1.0, 乙0.6, 丙0]
	sid1 := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid1, Source: "web_upload", Filename: "single.wav",
		StoragePath: "/tmp/single.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tr1 := &repo.Transcript{SessionID: sid1, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr1); err != nil {
		t.Fatal(err)
	}
	segs1 := []repo.TranscriptSegment{
		{TranscriptID: tr1.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "你好", StartMS: 0, EndMS: 2000},
		{TranscriptID: tr1.ID, SequenceNo: 2, SpeakerLabel: "1", Text: "在的", StartMS: 2100, EndMS: 4000},
	}
	if err := transcripts.InsertSegments(ctx, segs1); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.SaveSegmentEmbeddings(ctx, tr1.ID, map[ids.ID][]byte{
		segs1[0].ID: float32BlobAPI(e1), segs1[1].ID: float32BlobAPI(e1),
	}); err != nil {
		t.Fatal(err)
	}

	// 会话二（多人）：label "1" 短段向量 e1、label "2" 长段（时长最长）向量 e3
	// → basis=longest、用 e3 → top3=[丙1.0, 甲0, 乙0] 强命中丙
	sid2 := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid2, Source: "web_upload", Filename: "multi.wav",
		StoragePath: "/tmp/multi.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tr2 := &repo.Transcript{SessionID: sid2, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr2); err != nil {
		t.Fatal(err)
	}
	segs2 := []repo.TranscriptSegment{
		{TranscriptID: tr2.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "短", StartMS: 0, EndMS: 1000},
		{TranscriptID: tr2.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "最长段", StartMS: 2000, EndMS: 9000},
	}
	if err := transcripts.InsertSegments(ctx, segs2); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.SaveSegmentEmbeddings(ctx, tr2.ID, map[ids.ID][]byte{
		segs2[0].ID: float32BlobAPI(e1), segs2[1].ID: float32BlobAPI(e3),
	}); err != nil {
		t.Fatal(err)
	}

	r := newAuthedRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers,
		VoiceprintThreshold: 0.8,
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Sessions []struct {
			ID       string `json:"id"`
			VoiceTop *struct {
				Basis       string `json:"basis"`
				Matched     bool   `json:"matched"`
				Rule        string `json:"rule"`
				SpeakerName string `json:"speaker_name"`
				Matches     []struct {
					Name       string  `json:"name"`
					Similarity float64 `json:"similarity"`
				} `json:"matches"`
			} `json:"voice_top"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v %s", err, rec.Body.String())
	}
	byID := map[string]*struct {
		Basis       string `json:"basis"`
		Matched     bool   `json:"matched"`
		Rule        string `json:"rule"`
		SpeakerName string `json:"speaker_name"`
		Matches     []struct {
			Name       string  `json:"name"`
			Similarity float64 `json:"similarity"`
		} `json:"matches"`
	}{}
	for _, s := range resp.Sessions {
		if s.VoiceTop != nil {
			byID[s.ID] = s.VoiceTop
		}
	}

	// 单人会话：basis=whole，top1=甲/1.0 强命中
	vt1 := byID[sid1.String()]
	if vt1 == nil {
		t.Fatalf("单人会话缺 voice_top: %s", rec.Body.String())
	}
	if vt1.Basis != "whole" {
		t.Fatalf("单人 basis 应 whole，实际 %s", vt1.Basis)
	}
	if len(vt1.Matches) != 3 || vt1.Matches[0].Name != "甲" || math.Abs(vt1.Matches[0].Similarity-1) > 1e-6 {
		t.Fatalf("单人 top3 应 [甲1.0,...]，实际 %+v", vt1.Matches)
	}
	if math.Abs(vt1.Matches[1].Similarity-0.6) > 1e-6 || vt1.Matches[1].Name != "乙" {
		t.Fatalf("top2 应 乙/0.6，实际 %+v", vt1.Matches[1])
	}
	if !vt1.Matched || vt1.Rule != "strong" || vt1.SpeakerName != "甲" {
		t.Fatalf("单人应强命中甲（strong），实际 matched=%v rule=%s name=%s", vt1.Matched, vt1.Rule, vt1.SpeakerName)
	}

	// 多人会话：basis=longest，用最长段向量 e3 → 强命中丙
	vt2 := byID[sid2.String()]
	if vt2 == nil {
		t.Fatalf("多人会话缺 voice_top: %s", rec.Body.String())
	}
	if vt2.Basis != "longest" {
		t.Fatalf("多人 basis 应 longest，实际 %s", vt2.Basis)
	}
	if len(vt2.Matches) == 0 || vt2.Matches[0].Name != "丙" || math.Abs(vt2.Matches[0].Similarity-1) > 1e-6 {
		t.Fatalf("多人 top1 应 丙/1.0（最长段向量），实际 %+v", vt2.Matches)
	}
	if !vt2.Matched || vt2.Rule != "strong" || vt2.SpeakerName != "丙" {
		t.Fatalf("多人应强命中丙，实际 matched=%v rule=%s name=%s", vt2.Matched, vt2.Rule, vt2.SpeakerName)
	}

	// 清理：删两测试会话的段/转写/会话（共享库非自隔离；repo 无 Transcript.Delete，用原生 SQL）
	t.Cleanup(func() {
		cctx := context.Background()
		for _, sid := range []ids.ID{sid1, sid2} {
			_, _ = db.ExecContext(cctx, `DELETE FROM transcript_segment WHERE transcript_id IN (SELECT id FROM transcript WHERE session_id = ?)`, sid.Int64())
			_, _ = db.ExecContext(cctx, `DELETE FROM transcript WHERE session_id = ?`, sid.Int64())
			_, _ = db.ExecContext(cctx, `DELETE FROM audio_session WHERE id = ?`, sid.Int64())
		}
	})
}

// TestGetSessionVoiceMatches 验证详情接口 segments[] 附「段级声纹相似度 top-3」：
// 用 speaker stage 落库的逐段向量与全库声纹（灾备 BLOB）算余弦取前三。
// 用途：一句话可能混多个人——段级 top-1 不是归属说话人即该段可能被切错/归错。
func TestGetSessionVoiceMatches(t *testing.T) {
	_ = ids.InitForTest()
	db, err := repo.NewDB(repotest.DSN(t))
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

	// 共享库隔离：清掉历史遗留说话人——既有用例会残留 one-hot 向量的自动登记
	// 说话人，与本测试向量完全同向（相似度 1.0）会干扰 top-3 精确断言。
	for _, q := range []string{`DELETE FROM speaker_name_candidate`, `DELETE FROM speaker`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	// 三个说话人：甲 = e1（段的归属者）、乙 = 0.6e1+0.8e2 归一（与 e1 余弦 0.6）、
	// 丙 = e3（与 e1 正交）→ 段向量取 e1 时，voice_matches 应为 [甲 1.0, 乙 0.6, 丙 0]。
	e1, e3 := make([]float32, 256), make([]float32, 256)
	e1[0], e3[2] = 1, 1
	ab := make([]float32, 256)
	ab[0], ab[1] = 0.6, 0.8 // 已归一（0.36+0.64=1），与 e1 的余弦 = 0.6
	ji := &repo.Speaker{Name: "甲", Source: "enrolled", Embedding: float32BlobAPI(e1), SampleCount: 1}
	yi := &repo.Speaker{Name: "乙", Source: "enrolled", Embedding: float32BlobAPI(ab), SampleCount: 1}
	bing := &repo.Speaker{Name: "丙", Source: "enrolled", Embedding: float32BlobAPI(e3), SampleCount: 1}
	for _, sp := range []*repo.Speaker{ji, yi, bing} {
		if err := speakers.Create(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, sp := range []*repo.Speaker{ji, yi, bing} {
			_ = speakers.Delete(context.Background(), sp.ID)
		}
	})

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "vm.wav",
		StoragePath: "/tmp/vm.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	segs := []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "你好", StartMS: 0, EndMS: 1000},
	}
	if err := transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	// 归属甲 + 段声纹向量落库（模拟 speaker stage 的产物）
	if err := transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", ji.ID); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.SaveSegmentEmbeddings(ctx, tr.ID,
		map[ids.ID][]byte{segs[0].ID: float32BlobAPI(e1)}); err != nil {
		t.Fatal(err)
	}

	r := newAuthedRouter()
	RegisterQuery(r, &QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers,
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Segments []struct {
			Text         string `json:"text"`
			VoiceMatches []struct {
				Name       string  `json:"name"`
				Similarity float64 `json:"similarity"`
			} `json:"voice_matches"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(detail.Segments) != 1 {
		t.Fatalf("应 1 段，实际 %d", len(detail.Segments))
	}
	ms := detail.Segments[0].VoiceMatches
	if len(ms) != 3 {
		t.Fatalf("段应有 top-3 相似声纹，实际 %d: %+v", len(ms), ms)
	}
	if ms[0].Name != "甲" || math.Abs(ms[0].Similarity-1) > 1e-6 {
		t.Fatalf("top1 应为 甲/1.0（归属者自相似），实际 %s/%.4f", ms[0].Name, ms[0].Similarity)
	}
	if ms[1].Name != "乙" || math.Abs(ms[1].Similarity-0.6) > 1e-6 {
		t.Fatalf("top2 应为 乙/0.6，实际 %s/%.4f", ms[1].Name, ms[1].Similarity)
	}
	if ms[2].Name != "丙" || ms[2].Similarity != 0 {
		t.Fatalf("top3 应为 丙/0，实际 %s/%.4f", ms[2].Name, ms[2].Similarity)
	}
}

// TestReextractReidentifyBlockedWhileProcessing 验证防重入闸（2026-08-26 需求）：
// 会话当前 job 处于 pending/running 时，重新提取/重新识别一律 409——避免重复排队、
// 以及新旧 job 竞写同一 session 数据（如 reidentify 清空 speaker_id 时旧 speaker
// stage 正在回填）。job done/failed 时不拦截（正常重跑路径）。
func TestReextractReidentifyBlockedWhileProcessing(t *testing.T) {
	r, sid, sessions := buildEnrichedSession(t)
	ctx := context.Background()
	// 收尾清 job：本测试建的 pending job 若残留共享库，pipeline 包 pool 测试的
	// ClaimNext（全局领最老 pending）会抢跑它并拖超时（-p 1 保留的已知根因之一）。
	t.Cleanup(func() {
		_, _ = sessions.DB.ExecContext(context.Background(),
			`DELETE FROM pipeline_job WHERE session_id = ?`, sid.Int64())
	})

	mkJob := func(status string) {
		jr := &repo.JobRepo{DB: sessions.DB}
		j := &repo.Job{SessionID: sid, Stage: "speaker", Status: status}
		if err := jr.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		_ = sessions.SetJobID(ctx, sid, j.ID)
	}
	jr := &repo.JobRepo{DB: sessions.DB}

	// pending → 两个端点都 409
	mkJob("pending")
	for _, path := range []string{"/reextract", "/reidentify"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+path, nil))
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: pending 应 409, got %d %s", path, rec.Code, rec.Body.String())
		}
	}
	// running → 409
	if j, _ := jr.Get(ctx, mustJobID(t, sessions, sid)); j != nil {
		j.Status = "running"
		_ = jr.Save(ctx, j)
	} else {
		t.Fatal("job 未建立")
	}
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+"/reidentify", nil))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("running 应 409, got %d %s", rec2.Code, rec2.Body.String())
	}
	// done → 放行（200，建新 job）
	if j, _ := jr.Get(ctx, mustJobID(t, sessions, sid)); j != nil {
		j.Status = "done"
		_ = jr.Save(ctx, j)
	}
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+"/reextract", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("done 应放行, got %d %s", rec3.Code, rec3.Body.String())
	}
}

// TestGetSessionCorrectedMarker 详情返回被纠正段的 corrected_from + corrected_from_name（原历史人名，
// 即便它已不在本会话说话人列表里，也从 Speakers 兜底解析）。
func TestGetSessionCorrectedMarker(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}

	ghost := &repo.Speaker{Name: "铉晔", Source: "auto"}
	real := &repo.Speaker{Name: "说话人real", Source: "auto"}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	if err := speakers.Create(ctx, real); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), ghost.ID); _ = speakers.Delete(context.Background(), real.ID) })

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "a.wav", StoragePath: "/tmp/a.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tc := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tc); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "2", Text: "幽灵段", StartMS: 0, EndMS: 1000},
		{TranscriptID: tc.ID, SequenceNo: 2, SpeakerLabel: "1", Text: "普通段", StartMS: 1000, EndMS: 2000},
	}); err != nil {
		t.Fatal(err)
	}
	// 先归到 ghost，再纠正给 real（写 corrected_from=ghost）
	if err := transcripts.SetSegmentSpeaker(ctx, tc.ID, "2", ghost.ID); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.CorrectSegmentSpeaker(ctx, tc.ID, "2", ghost.ID, real.ID); err != nil {
		t.Fatal(err)
	}
	// 第二段直接归到 real，不经纠正——用于验证未纠正段不输出 corrected_from* 键（omitempty 契约）
	if err := transcripts.SetSegmentSpeaker(ctx, tc.ID, "1", real.ID); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), 1)))
		})
	})
	RegisterQuery(r, &QueryHandler{Sessions: sessions, Transcripts: transcripts, Speakers: speakers})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Segments []struct {
			CorrectedFrom     string `json:"corrected_from"`
			CorrectedFromName string `json:"corrected_from_name"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Segments) != 2 {
		t.Fatalf("应有 2 段，实际 %d", len(resp.Segments))
	}
	// 段按 sequence_no 排序，seq1（幽灵段）是被纠正的那一段——以非空 corrected_from 定位
	var corrected *struct {
		CorrectedFrom     string `json:"corrected_from"`
		CorrectedFromName string `json:"corrected_from_name"`
	}
	for i := range resp.Segments {
		if resp.Segments[i].CorrectedFrom != "" {
			corrected = &resp.Segments[i]
			break
		}
	}
	if corrected == nil {
		t.Fatalf("未找到被纠正段（corrected_from 全空）: %s", rec.Body.String())
	}
	if corrected.CorrectedFrom != ghost.ID.String() {
		t.Fatalf("corrected_from 应为铉晔 id，实际 %q", corrected.CorrectedFrom)
	}
	if corrected.CorrectedFromName != "铉晔" {
		t.Fatalf("corrected_from_name 应为铉晔，实际 %q", corrected.CorrectedFromName)
	}
	// omitempty 契约：只有 1 段被纠正，raw body 中 "corrected_from" 恰好出现 2 次
	// （该段的 corrected_from + corrected_from_name），未纠正段不输出这两个键。
	if n := strings.Count(rec.Body.String(), "corrected_from"); n != 2 {
		t.Fatalf("未纠正段不应输出 corrected_from* 键，期望恰好 2 处（仅纠正段），实际 %d", n)
	}
}

// mustJobID 取 session 当前指向的 job id（测试 helper）。
func mustJobID(t *testing.T, sessions *repo.SessionRepo, sid ids.ID) ids.ID {
	t.Helper()
	s, err := sessions.Get(context.Background(), 1, sid) // 多用户签名：fixture 默认 owner=1
	if err != nil || s == nil || s.JobID == nil {
		t.Fatalf("session/job 缺失: %v", err)
	}
	return *s.JobID
}

// TestGetSessionCorrectedReasonShort 详情返回过短并入段的 corrected_reason=short（corrected_from 为空）。
func TestGetSessionCorrectedReasonShort(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}
	target := &repo.Speaker{Name: "说话人target", Source: "auto"}
	if err := speakers.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), target.ID) })
	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "a.wav", StoragePath: "/tmp/a.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tc := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tc); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "noise", Text: "嗯。", StartMS: 0, EndMS: 400},
	}); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.MergeShortGroup(ctx, tc.ID, "noise", target.ID); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUserID(req.Context(), 1)))
		})
	})
	RegisterQuery(r, &QueryHandler{Sessions: sessions, Transcripts: transcripts, Speakers: speakers})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Segments []struct {
			CorrectedReason string `json:"corrected_reason"`
			CorrectedFrom   string `json:"corrected_from"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Segments) != 1 {
		t.Fatalf("应 1 段，实际 %d", len(resp.Segments))
	}
	if resp.Segments[0].CorrectedReason != "short" {
		t.Fatalf("corrected_reason 应为 short，实际 %q", resp.Segments[0].CorrectedReason)
	}
	if resp.Segments[0].CorrectedFrom != "" {
		t.Fatalf("short 段 corrected_from 应为空，实际 %q", resp.Segments[0].CorrectedFrom)
	}
}
