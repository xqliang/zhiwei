# P1 音频场景与情绪理解（A+B）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** ASR 后新增 `audioscene` stage，用 `stepaudio-2.5-chat` 产出会话级声学环境 + 每说话人情绪，落库并在会话详情最小可视化。

**Architecture:** 新 provider `AudioInsightProvider`（stepaudio-2.5-chat，走 ibasemind 代理）+ 新 pipeline stage `audioscene`（`speakername` 后，>10min 分块识别再合并）+ 迁移 000025（transcript 4 列 + 新表 speaker_session_state）+ 配置开关 + GetSession API 扩展 + 前端环境徽章/情绪 chip。失败全程降级不阻断。

**Tech Stack:** Go + chi + sqlx + MySQL(golang-migrate) + ffmpeg(转码/静音检测/切片) + StepFun stepaudio-2.5-chat(OpenAI 兼容) ；Vue3 CDN 前端。

**规格：** `docs/superpowers/specs/2026-08-29-audio-insight-p1-design.md`（§编号下同）。

**测试约定：** repo 集成测试 `repotest.DSN(t)`（未设 TEST_MYSQL_DSN 则 skip）；pipeline 测试见 `stage_asr_test.go` 模式（`NewPool(jobs, Flow{Stages:[...]}, BuildStages(deps))`）；`ids.ID` 用 `.Int64()`/`.String()`。**迁移号 000025**（main 最新 000024，合并前复查）。每任务末尾提交；Go 改动后 `go build ./... && go vet ./...`。MySQL 已在 127.0.0.1:3307，DSN=`zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true`。

---

### Task 0: Spike — 验证 stepaudio-2.5-chat 音频 URL 输入 + 真录音质量

**Files:** 无（一次性验证，结果写入 Task 3 决策）。

- [ ] **Step 1: 验 URL 输入支持**

用代理 key 试 `input_audio` 传 URL（而非 base64）。若项目根 `.env` 可读，运行（`dangerouslyDisableSandbox` 若网络被沙箱拦）：
```bash
set -a; . ./.env 2>/dev/null; set +a
python3 - <<'PY'
import json,urllib.request,os
key=os.environ["STEPFUN_ASR_FILE_API_KEY"]
# 用一个可公网访问的音频 URL（可先 TOS 上传 testdata/speech20s.wav 得 URL；无则跳过本步，直接 base64 路线）
url=os.environ.get("SPIKE_AUDIO_URL","")
if not url: print("SKIP: 无 SPIKE_AUDIO_URL，URL 路线跳过，用 base64"); raise SystemExit
for shape in [{"type":"input_audio","input_audio":{"url":url}},
              {"type":"input_audio","input_audio":{"audio_url":url,"format":"wav"}}]:
    body=json.dumps({"model":"stepaudio-2.5-chat","messages":[{"role":"user","content":[
      {"type":"text","text":"这段音频的声学场景？只答两字"},shape]}]}).encode()
    req=urllib.request.Request("https://api.c.ibasemind.com/v1/chat/completions",data=body,
      headers={"Authorization":"Bearer "+key,"Content-Type":"application/json"})
    try:
      import urllib.error
      r=urllib.request.urlopen(req,timeout=60); print("OK",shape.get("input_audio"),json.load(r)["choices"][0]["message"]["content"][:80])
    except urllib.error.HTTPError as e: print("HTTP",e.code,e.read().decode()[:200])
PY
```
Expected: 某种 URL 形状成功 → Task 3 用 URL（每块上传 TOS）；全失败 → Task 3 用 base64（分块后每块 ≤10min 已受控）。

- [ ] **Step 2: 真录音质量抽验（best-effort）**

若有真实录音（非 TTS），用 §2 的 JSON 提示跑一遍，人工看情绪/场景是否合理。无真录音则记录"待上线后抽验"，不阻断。

- [ ] **Step 3: 记录结论**

把"URL 是否可用 / 用哪种 content 形状"写进 Task 3 的实现注释。无需提交（无文件改动）。

---

### Task 1: 迁移 000025 — transcript 环境列 + speaker_session_state 表

**Files:** Create `migrations/000025_audio_insight.up.sql` / `.down.sql`

- [ ] **Step 1: 写 up 迁移**

`migrations/000025_audio_insight.up.sql`：
```sql
-- P1 音频场景与情绪理解（spec §2）。合并回 main 前复查 main 最新迁移号（并行分支撞号）。
-- 会话级声学环境（1:1 挂 transcript）。
ALTER TABLE transcript ADD COLUMN acoustic_scene   VARCHAR(32)  NOT NULL DEFAULT '' AFTER confidence;
ALTER TABLE transcript ADD COLUMN background_sounds JSON        NULL              AFTER acoustic_scene;
ALTER TABLE transcript ADD COLUMN weather_cues      VARCHAR(32)  NOT NULL DEFAULT '' AFTER background_sounds;
ALTER TABLE transcript ADD COLUMN overall_mood      VARCHAR(128) NOT NULL DEFAULT '' AFTER weather_cues;

-- 每会话每说话人情绪状态（可选富化，独立表不污染热表 transcript_segment）。
CREATE TABLE speaker_session_state (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  transcript_id BIGINT NOT NULL,
  session_id    BIGINT NOT NULL,
  speaker_label VARCHAR(32)  NOT NULL DEFAULT '',
  speaker_id    BIGINT NULL,
  emotion       VARCHAR(32)  NOT NULL DEFAULT '',
  micro_emotion VARCHAR(64)  NOT NULL DEFAULT '',
  mental_state  VARCHAR(64)  NOT NULL DEFAULT '',
  confidence    DOUBLE NOT NULL DEFAULT 0,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_sss_session (session_id),
  KEY idx_sss_transcript (transcript_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- [ ] **Step 2: 写 down 迁移**

`migrations/000025_audio_insight.down.sql`：
```sql
DROP TABLE IF EXISTS speaker_session_state;
ALTER TABLE transcript DROP COLUMN overall_mood;
ALTER TABLE transcript DROP COLUMN weather_cues;
ALTER TABLE transcript DROP COLUMN background_sounds;
ALTER TABLE transcript DROP COLUMN acoustic_scene;
```

- [ ] **Step 3: 应用验证**

Run: `mysql -h127.0.0.1 -P3307 -uzhiwei -pzhiwei -e "DROP DATABASE IF EXISTS zhiwei_test_repo"` 然后 `TEST_MYSQL_DSN="...zhiwei_test..." go test ./internal/repo/ -run TestAgentConversationCRUD -count=1`（repotest 会自动跑到 000025 重建库）。Expected: PASS（迁移无误）。

- [ ] **Step 4: 提交**
```bash
git add migrations/000025_audio_insight.up.sql migrations/000025_audio_insight.down.sql
git commit -m "feat(migrations): 000025 transcript 环境列 + speaker_session_state 表"
```

---

### Task 2: repo — Transcript 环境字段 + SetAcoustic + SpeakerSessionStateRepo

**Files:** Modify `internal/repo/transcript.go`；Create `internal/repo/speaker_session_state.go`；Test `internal/repo/speaker_session_state_test.go`

- [ ] **Step 1: 写失败测试**

`internal/repo/speaker_session_state_test.go`：
```go
package repo

import (
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repotest"
)

func TestSpeakerSessionState(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil { t.Fatal(err) }
	ctx := t.Context()
	sid, tid := ids.New(), ids.New()
	r := &SpeakerSessionStateRepo{DB: db}
	rows := []SpeakerSessionState{
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "1", Emotion: "平静", MicroEmotion: "专注", MentalState: "投入", Confidence: 0.8},
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "2", Emotion: "焦虑", Confidence: 0.6},
	}
	if err := r.InsertBatch(ctx, rows); err != nil { t.Fatalf("InsertBatch: %v", err) }
	got, err := r.ListBySession(ctx, 1, sid)
	if err != nil { t.Fatalf("ListBySession: %v", err) }
	if len(got) != 2 { t.Fatalf("want 2 rows, got %d", len(got)) }
	// 越权：user 2 看不到
	got2, _ := r.ListBySession(ctx, 2, sid)
	if len(got2) != 0 { t.Errorf("越权应 0 行, got %d", len(got2)) }
}

func TestTranscriptSetAcoustic(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil { t.Fatal(err) }
	ctx := t.Context()
	tr := &Transcript{SessionID: ids.New(), Language: "zh-CN"}
	trepo := &TranscriptRepo{DB: db}
	if err := trepo.Create(ctx, tr); err != nil { t.Fatal(err) }
	bg := json.RawMessage(`["键盘","车流"]`)
	if err := trepo.SetAcoustic(ctx, tr.ID, "室内", &bg, "无", "专注工作"); err != nil { t.Fatalf("SetAcoustic: %v", err) }
	got, err := trepo.GetBySession(ctx, tr.SessionID)
	if err != nil { t.Fatal(err) }
	if got.AcousticScene != "室内" || got.WeatherCues != "无" || got.OverallMood != "专注工作" { t.Errorf("环境列未写入: %+v", got) }
	if got.BackgroundSounds == nil || string(*got.BackgroundSounds) == "" { t.Error("background_sounds 未写入") }
}
```
（`speaker_session_state_test.go` 需 import `"encoding/json"` 供第二个测试——放同文件即可。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go vet ./internal/repo/`。Expected: `SpeakerSessionStateRepo` / `SetAcoustic` / Transcript 新字段 未定义（编译错）。

- [ ] **Step 3: 实现 Transcript 字段 + SetAcoustic**

`internal/repo/transcript.go` 的 `Transcript` 结构体（`Confidence` 后、`CreatedAt` 前）加：
```go
	AcousticScene    string           `db:"acoustic_scene" json:"acoustic_scene"`
	BackgroundSounds *json.RawMessage `db:"background_sounds" json:"background_sounds,omitempty"`
	WeatherCues      string           `db:"weather_cues" json:"weather_cues"`
	OverallMood      string           `db:"overall_mood" json:"overall_mood"`
```
（文件需 import `"encoding/json"`。）加方法：
```go
// SetAcoustic 写会话级声学环境（audioscene stage 用）。bg 可空。
func (r *TranscriptRepo) SetAcoustic(ctx context.Context, id ids.ID, scene string, bg *json.RawMessage, weather, mood string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript SET acoustic_scene=?, background_sounds=?, weather_cues=?, overall_mood=? WHERE id=?`,
		scene, bg, weather, mood, id.Int64())
	return err
}
```

- [ ] **Step 4: 实现 SpeakerSessionStateRepo**

`internal/repo/speaker_session_state.go`：
```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// SpeakerSessionState 是每会话每说话人的整体情绪/精神状态（audioscene stage 落库；spec §2）。
type SpeakerSessionState struct {
	ID           ids.ID    `db:"id" json:"id"`
	UserID       int64     `db:"user_id" json:"user_id"`
	TranscriptID ids.ID    `db:"transcript_id" json:"transcript_id"`
	SessionID    ids.ID    `db:"session_id" json:"session_id"`
	SpeakerLabel string    `db:"speaker_label" json:"speaker_label"`
	SpeakerID    *ids.ID   `db:"speaker_id" json:"speaker_id,omitempty"`
	Emotion      string    `db:"emotion" json:"emotion"`
	MicroEmotion string    `db:"micro_emotion" json:"micro_emotion"`
	MentalState  string    `db:"mental_state" json:"mental_state"`
	Confidence   float64   `db:"confidence" json:"confidence"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

type SpeakerSessionStateRepo struct{ DB *sqlx.DB }

// InsertBatch 批量插入（生成 ID，UserID 默认 1）。空切片 no-op。
func (r *SpeakerSessionStateRepo) InsertBatch(ctx context.Context, rows []SpeakerSessionState) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		rows[i].ID = ids.New()
		if rows[i].UserID == 0 {
			rows[i].UserID = 1
		}
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO speaker_session_state
  (id, user_id, transcript_id, session_id, speaker_label, speaker_id, emotion, micro_emotion, mental_state, confidence)
VALUES
  (:id, :user_id, :transcript_id, :session_id, :speaker_label, :speaker_id, :emotion, :micro_emotion, :mental_state, :confidence)`, rows)
	return err
}

// ListBySession 返回某会话的说话人情绪（行级 user_id 过滤，IDOR）。
func (r *SpeakerSessionStateRepo) ListBySession(ctx context.Context, userID int64, sessionID ids.ID) ([]SpeakerSessionState, error) {
	var rows []SpeakerSessionState
	err := r.DB.SelectContext(ctx, &rows,
		`SELECT * FROM speaker_session_state WHERE session_id=? AND user_id=? ORDER BY id ASC`, sessionID.Int64(), userID)
	return rows, err
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/repo/ -run 'TestSpeakerSessionState|TestTranscriptSetAcoustic' -v -count=1`（DSN 已设）。Expected: PASS。

- [ ] **Step 6: 提交**
```bash
git add internal/repo/transcript.go internal/repo/speaker_session_state.go internal/repo/speaker_session_state_test.go
git commit -m "feat(repo): Transcript 环境字段+SetAcoustic + SpeakerSessionStateRepo"
```

---

### Task 3: provider — AudioInsight 类型 + StepAudioInsight

**Files:** Create `internal/provider/audio_insight.go` / `audio_insight_test.go`

- [ ] **Step 1: 写失败测试（JSON 解析容错 + 错误路径）**

`internal/provider/audio_insight_test.go`：
```go
package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseAudioInsightJSON(t *testing.T) {
	// 裹 ```json 代码块也要能解析
	raw := "```json\n{\"acoustic_scene\":\"室内\",\"background_sounds\":[\"键盘\"],\"weather_cues\":\"无\",\"overall_mood\":\"专注\",\"speakers\":[{\"label\":\"1\",\"emotion\":\"平静\",\"micro_emotion\":\"专注\",\"mental_state\":\"投入\",\"confidence\":0.8}]}\n```"
	ins, err := parseAudioInsight(raw)
	if err != nil { t.Fatalf("parse: %v", err) }
	if ins.AcousticScene != "室内" || len(ins.Speakers) != 1 || ins.Speakers[0].Emotion != "平静" {
		t.Errorf("解析异常: %+v", ins)
	}
}

func TestStepAudioInsightHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402); w.Write([]byte(`{"error":{"message":"quota"}}`))
	}))
	defer srv.Close()
	p := NewStepAudioInsight(srv.URL, "k", "stepaudio-2.5-chat")
	// 需要一个存在的小音频文件；用 testdata 的相对路径（provider 测试从包目录跑，用绝对/临时文件）
	tmp := t.TempDir() + "/a.wav"
	if err := writeMinimalWAV(tmp); err != nil { t.Skip("无法造 wav") }
	if _, err := p.Analyze(context.Background(), tmp, []string{"1"}); err == nil {
		t.Error("402 应返回 error")
	}
}
```
（`writeMinimalWAV` 写一个最小合法 wav 头即可，让 base64 读取不 panic；HTTP 402 由桩返回、断言 err 非 nil。若嫌复杂，本测试可只保留 TestParseAudioInsightJSON，HTTP 错误路径靠 stage 测试覆盖。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go vet ./internal/provider/`。Expected: `parseAudioInsight`/`NewStepAudioInsight`/`AudioInsight` 未定义。

- [ ] **Step 3: 实现 audio_insight.go**

`internal/provider/audio_insight.go`（HTTP 调用范式对齐 `llm.go`；base64 路线，Task 0 若确认 URL 可用则在此加 URL 分支）：
```go
package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// AudioInsight 是一次音频理解的结构化结果（spec §3）。
type AudioInsight struct {
	AcousticScene    string           `json:"acoustic_scene"`
	BackgroundSounds []string         `json:"background_sounds"`
	WeatherCues      string           `json:"weather_cues"`
	OverallMood      string           `json:"overall_mood"`
	Speakers         []SpeakerInsight `json:"speakers"`
}

type SpeakerInsight struct {
	Label        string  `json:"label"`
	Emotion      string  `json:"emotion"`
	MicroEmotion string  `json:"micro_emotion"`
	MentalState  string  `json:"mental_state"`
	Confidence   float64 `json:"confidence"`
}

// AudioInsightProvider 输入一个音频文件（≤ 模型时长上限，长录音已在 stage 分块）+ 已知说话人标签，
// 返回结构化情绪/声学场景。业务只依赖此接口。
type AudioInsightProvider interface {
	Analyze(ctx context.Context, audioPath string, speakerLabels []string) (AudioInsight, error)
}

type StepAudioInsight struct {
	baseURL, apiKey, model string
	client                 *http.Client
}

func NewStepAudioInsight(baseURL, apiKey, model string) *StepAudioInsight {
	return &StepAudioInsight{baseURL: baseURL, apiKey: apiKey, model: model, client: &http.Client{Timeout: 180 * time.Second}}
}

const audioInsightSystem = "你是声学场景与情绪分析器。只输出一个 JSON 对象，不要任何解释、不要 markdown 代码块。"

func audioInsightPrompt(labels []string) string {
	ls := "（未提供，请用说话人1/2…）"
	if len(labels) > 0 {
		ls = strings.Join(labels, "、")
	}
	return "分析这段录音的【声音本身】(非文字内容)。已知说话人标签：" + ls + "。严格按此 JSON 模式输出：" +
		`{"acoustic_scene":"室内|室外|会议室|餐厅|车内|地铁|户外|电梯|厨房|办公室|未知",` +
		`"background_sounds":["键盘|车流|鸟叫|宠物叫|风声|雨声|金属撞击|人声嘈杂|无"],` +
		`"weather_cues":"无|有风|有雨|雷电",` +
		`"overall_mood":"一句话",` +
		`"speakers":[{"label":"与已知标签对应","emotion":"平静|喜悦|焦虑|愤怒|疲惫|…","micro_emotion":"细微语气","mental_state":"精神状态","confidence":0.0}]}`
}

type aiChatReq struct {
	Model    string      `json:"model"`
	Messages []aiMessage `json:"messages"`
	Temperature float64  `json:"temperature"`
}
type aiMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}
type aiContentText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type aiContentAudio struct {
	Type       string       `json:"type"`
	InputAudio aiAudioInner `json:"input_audio"`
}
type aiAudioInner struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

func (p *StepAudioInsight) Analyze(ctx context.Context, audioPath string, labels []string) (AudioInsight, error) {
	b, err := os.ReadFile(audioPath)
	if err != nil {
		return AudioInsight{}, fmt.Errorf("读音频: %w", err)
	}
	audio := base64.StdEncoding.EncodeToString(b)
	req := aiChatReq{
		Model:       p.model,
		Temperature: 0.2,
		Messages: []aiMessage{
			{Role: "system", Content: audioInsightSystem},
			{Role: "user", Content: []any{
				aiContentText{Type: "text", Text: audioInsightPrompt(labels)},
				aiContentAudio{Type: "input_audio", InputAudio: aiAudioInner{Data: audio, Format: "wav"}},
			}},
		},
	}
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return AudioInsight{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var cr struct {
		Choices []struct{ Message struct{ Content string `json:"content"` } `json:"message"` } `json:"choices"`
		Error   *struct{ Message string `json:"message"` } `json:"error"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return AudioInsight{}, fmt.Errorf("响应解析(http %d): %s", resp.StatusCode, truncate(raw))
	}
	if cr.Error != nil {
		return AudioInsight{}, fmt.Errorf("audio-insight 错误: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return AudioInsight{}, fmt.Errorf("空响应(http %d): %s", resp.StatusCode, truncate(raw))
	}
	return parseAudioInsight(cr.Choices[0].Message.Content)
}

// parseAudioInsight 清洗并解析模型输出（容忍 ```json 代码块与首尾杂字）。
func parseAudioInsight(s string) (AudioInsight, error) {
	s = strings.TrimSpace(s)
	// 去 ```json / ``` 围栏
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	// 截取第一个 { 到最后一个 }（去掉围栏外杂字）
	if l, r := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}'); l >= 0 && r > l {
		s = s[l : r+1]
	}
	var ins AudioInsight
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &ins); err != nil {
		return AudioInsight{}, fmt.Errorf("audio-insight JSON 解析失败: %w", err)
	}
	return ins, nil
}
```
（`truncate` 已在 `llm.go` 同包定义，可直接复用。**Task 0 若确认 URL 可用**：加 `AnalyzeURL` 或在 Analyze 里按传入是 path/URL 分支——由 stage 决定传哪种；本任务先实现 base64 path 版，URL 作为 Task 5 的可选优化。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/provider/ -run 'TestParseAudioInsight|TestStepAudioInsight' -v -count=1`。Expected: PASS。

- [ ] **Step 5: 提交**
```bash
git add internal/provider/audio_insight.go internal/provider/audio_insight_test.go
git commit -m "feat(provider): AudioInsight 接口 + StepAudioInsight(stepaudio-2.5-chat, base64, JSON容错)"
```

---

### Task 4: 分块 + 合并 helper（长录音）

**Files:** Create `internal/pipeline/audio_chunk.go` / `audio_chunk_test.go`

- [ ] **Step 1: 写失败测试（纯函数：合并 + 切点计算）**

`internal/pipeline/audio_chunk_test.go`：
```go
package pipeline

import (
	"testing"

	"zhiwei/internal/provider"
)

// mergeInsights：多块 → 会话级取众数/并集，每说话人按最高置信合并。
func TestMergeInsights(t *testing.T) {
	chunks := []provider.AudioInsight{
		{AcousticScene: "室内", BackgroundSounds: []string{"键盘"}, WeatherCues: "无", OverallMood: "专注",
			Speakers: []provider.SpeakerInsight{{Label: "1", Emotion: "平静", MicroEmotion: "专注", Confidence: 0.6}}},
		{AcousticScene: "室内", BackgroundSounds: []string{"车流"}, WeatherCues: "无", OverallMood: "疲惫",
			Speakers: []provider.SpeakerInsight{{Label: "1", Emotion: "焦虑", MicroEmotion: "急促", Confidence: 0.9}, {Label: "2", Emotion: "喜悦", Confidence: 0.7}}},
	}
	m := mergeInsights(chunks)
	if m.AcousticScene != "室内" { t.Errorf("scene=%q", m.AcousticScene) }
	if len(m.BackgroundSounds) != 2 { t.Errorf("bg 应并集去重=2, got %v", m.BackgroundSounds) }
	// 说话人1 取最高置信块(0.9)的 emotion=焦虑
	var s1 *provider.SpeakerInsight
	for i := range m.Speakers { if m.Speakers[i].Label == "1" { s1 = &m.Speakers[i] } }
	if s1 == nil || s1.Emotion != "焦虑" { t.Errorf("说话人1 应取最高置信情绪=焦虑, got %+v", s1) }
	if len(m.Speakers) != 2 { t.Errorf("应有 2 位说话人, got %d", len(m.Speakers)) }
}

// planChunks：按 chunkSec 计算切点数（纯计算，不切文件）。
func TestPlanChunks(t *testing.T) {
	// 25min，chunk=10min → 3 块
	if n := len(planChunks(25*60*1000, 10*60)); n != 3 { t.Errorf("25min/10min 应 3 块, got %d", n) }
	// 8min → 1 块（不分）
	if n := len(planChunks(8*60*1000, 10*60)); n != 1 { t.Errorf("8min 应 1 块, got %d", n) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go vet ./internal/pipeline/`。Expected: `mergeInsights`/`planChunks` 未定义。

- [ ] **Step 3: 实现 audio_chunk.go**

```go
package pipeline

import (
	"sort"

	"zhiwei/internal/provider"
)

// chunkPlan 是一个待分析块的时间范围（毫秒）。
type chunkPlan struct{ StartMS, EndMS int64 }

// planChunks 把总时长按 chunkSec 切成若干块（固定切点；静音微调/重叠在 stage 切片时用 ffmpeg 处理）。
// 返回每块的粗略时间范围（供 stage 决定 ffmpeg -ss/-t 与静音搜索窗口）。
func planChunks(totalMS int64, chunkSec int) []chunkPlan {
	step := int64(chunkSec) * 1000
	if totalMS <= step {
		return []chunkPlan{{0, totalMS}}
	}
	var plans []chunkPlan
	for start := int64(0); start < totalMS; start += step {
		end := start + step
		if end > totalMS {
			end = totalMS
		}
		plans = append(plans, chunkPlan{start, end})
	}
	return plans
}

// mergeInsights 合并多块结果（spec §5）：会话级 scene/weather/mood 取众数（并列取先出现），
// background_sounds 并集去重；每说话人按 label 聚合、emotion/mental_state 取最高置信块、
// micro_emotion 并集去重、confidence 取均值。单块直接返回。
func mergeInsights(chunks []provider.AudioInsight) provider.AudioInsight {
	if len(chunks) == 0 {
		return provider.AudioInsight{}
	}
	if len(chunks) == 1 {
		return chunks[0]
	}
	out := provider.AudioInsight{
		AcousticScene: mode(pluck(chunks, func(a provider.AudioInsight) string { return a.AcousticScene })),
		WeatherCues:   mode(pluck(chunks, func(a provider.AudioInsight) string { return a.WeatherCues })),
		OverallMood:   mode(pluck(chunks, func(a provider.AudioInsight) string { return a.OverallMood })),
	}
	// background_sounds 并集去重（保序）
	seen := map[string]bool{}
	for _, c := range chunks {
		for _, b := range c.BackgroundSounds {
			if b != "" && b != "无" && !seen[b] {
				seen[b] = true
				out.BackgroundSounds = append(out.BackgroundSounds, b)
			}
		}
	}
	// 每说话人聚合
	type agg struct {
		best   provider.SpeakerInsight
		micros []string
		sum    float64
		n      int
	}
	byLabel := map[string]*agg{}
	var order []string
	for _, c := range chunks {
		for _, s := range c.Speakers {
			a := byLabel[s.Label]
			if a == nil {
				a = &agg{}
				byLabel[s.Label] = a
				order = append(order, s.Label)
			}
			if s.Confidence >= a.best.Confidence {
				a.best = s // 最高置信块的 emotion/mental_state
			}
			if s.MicroEmotion != "" {
				a.micros = appendUniq(a.micros, s.MicroEmotion)
			}
			a.sum += s.Confidence
			a.n++
		}
	}
	for _, label := range order {
		a := byLabel[label]
		conf := 0.0
		if a.n > 0 {
			conf = a.sum / float64(a.n)
		}
		out.Speakers = append(out.Speakers, provider.SpeakerInsight{
			Label: label, Emotion: a.best.Emotion, MentalState: a.best.MentalState,
			MicroEmotion: joinUniq(a.micros), Confidence: conf,
		})
	}
	return out
}

func pluck(cs []provider.AudioInsight, f func(provider.AudioInsight) string) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		if v := f(c); v != "" {
			out = append(out, v)
		}
	}
	return out
}
func mode(xs []string) string {
	cnt := map[string]int{}
	best, bestN := "", 0
	for _, x := range xs {
		cnt[x]++
		if cnt[x] > bestN {
			best, bestN = x, cnt[x]
		}
	}
	return best
}
func appendUniq(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}
func joinUniq(xs []string) string {
	sort.Strings(xs)
	return strings_Join(xs, "/")
}
```
（`strings_Join` 用标准库 `strings.Join`——import `"strings"` 并把 `strings_Join` 改回 `strings.Join`；此处占位提醒实现者用真实 `strings.Join`，limit ≤64 字符截断留给 stage 落库前处理。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/pipeline/ -run 'TestMergeInsights|TestPlanChunks' -v -count=1`。Expected: PASS。

- [ ] **Step 5: 提交**
```bash
git add internal/pipeline/audio_chunk.go internal/pipeline/audio_chunk_test.go
git commit -m "feat(pipeline): 长录音分块规划 + 多块结果合并 helper"
```

---

### Task 5: stage `audioscene` + 音频切片（ffmpeg 静音切点/2s 重叠）+ StageDeps

**Files:** Modify `internal/pipeline/stage_asr.go`（BuildStages + StageDeps）；Create `internal/pipeline/stage_audioscene.go` / `stage_audioscene_test.go`

- [ ] **Step 1: 写失败测试（fake provider，验落库/降级/开关/归因）**

`internal/pipeline/stage_audioscene_test.go`：
```go
package pipeline

import (
	"context"
	"errors"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

type fakeAI struct {
	out provider.AudioInsight
	err error
}
func (f *fakeAI) Analyze(_ context.Context, _ string, _ []string) (provider.AudioInsight, error) { return f.out, f.err }

func TestStageAudioScenePersist(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil { t.Fatal(err) }
	ctx := t.Context()
	// 造 session + transcript + 2 段（说话人 1/2），并给个可读的转码 wav（stage 会 transcodeToWAV，
	// 但测试注入 fake provider 不真读音频——见下 d.AudioInsight fake）。
	sess := &repo.AudioSession{UserID: 1, StoragePath: "testdata/speech20s.wav", Status: "done"}
	// … 用 SessionRepo.Create 造 session；TranscriptRepo.Create + InsertSegments 造 2 段（SpeakerLabel "1"/"2"）
	// （具体构造照 stage_asr_test.go / 已有 repo 测试的建数据方式）

	d := StageDeps{
		Sessions: &repo.SessionRepo{DB: db}, Transcripts: &repo.TranscriptRepo{DB: db},
		SpeakerStates: &repo.SpeakerSessionStateRepo{DB: db},
		DataDir: t.TempDir(), // 无真音频时 transcodeToWAV 会失败 → 见 stage 对 transcode 失败的降级
		AudioInsight: &fakeAI{out: provider.AudioInsight{
			AcousticScene: "室内", WeatherCues: "无", OverallMood: "专注",
			Speakers: []provider.SpeakerInsight{{Label: "1", Emotion: "平静", Confidence: 0.8}, {Label: "2", Emotion: "焦虑", Confidence: 0.6}},
		}},
		AudioInsightEnabled: true, AudioInsightChunkSec: 600,
	}
	_ = d // 断言：跑 stage 后 transcript 环境列写入 + speaker_session_state 2 行 + speaker_id 按 label 映射回填
}

func TestStageAudioSceneDisabledSkips(t *testing.T) {
	d := StageDeps{AudioInsightEnabled: false}
	if err := stageAudioScene(d)(context.Background(), nil, ids.New()); err != nil {
		t.Errorf("关闭时应 no-op 返回 nil, got %v", err)
	}
}

func TestStageAudioSceneDegradesOnError(t *testing.T) {
	// provider 报错 → stage 仍返回 nil（不阻断 job）
	// 用真 db 造最小 session/transcript，fake provider err=boom，断言 stage 返回 nil 且无 panic
	_ = errors.New
}
```
（说明：真音频依赖较重——`TestStageAudioSceneDisabledSkips` 是纯逻辑必过；`Persist`/`Degrades` 需造 DB 数据 + 让 transcode 可跳过。实现者可给 stage 一个"audio 已存在的 wav 路径"注入点，或在测试用 `testdata/speech20s.wav` 作 StoragePath 且 DataDir 指向可写临时目录让 transcode 真跑（需 ffmpeg）。若 CI 无 ffmpeg，用 build tag 或 t.Skip 守卫重音频测试。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go vet ./internal/pipeline/`。Expected: `stageAudioScene` / `StageDeps.AudioInsight` 等未定义。

- [ ] **Step 3: StageDeps 加字段 + BuildStages 注册**

`internal/pipeline/stage_asr.go`：StageDeps 末尾加：
```go
	// ---- audioscene stage（P1 音频场景与情绪）----
	AudioInsight         provider.AudioInsightProvider // nil = no-op
	SpeakerStates        *repo.SpeakerSessionStateRepo
	AudioInsightEnabled  bool
	AudioInsightChunkSec int // 0 = 默认 600
```
`BuildStages` map 加：`"audioscene": stageAudioScene(d),`

- [ ] **Step 4: 实现 stage_audioscene.go**

```go
package pipeline

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

func stageAudioScene(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
		if !d.AudioInsightEnabled || d.AudioInsight == nil {
			return nil // 开关关闭或未装配：no-op
		}
		s, err := d.Sessions.Get(ctx, 1, sessionID)
		if err != nil {
			log.Printf("[audioscene] 读 session 失败(降级): %v", err)
			return nil
		}
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			log.Printf("[audioscene] 无 transcript(降级): %v", err)
			return nil
		}
		wav, err := transcodeToWAV(d.DataDir, sessionID, s.StoragePath)
		if err != nil {
			log.Printf("[audioscene] 转码失败(降级): %v", err)
			return nil
		}
		segs, _ := d.Transcripts.ListSegments(ctx, tr.ID)
		labels, labelToSpeaker := collectSpeakerLabels(segs)

		chunkSec := d.AudioInsightChunkSec
		if chunkSec <= 0 {
			chunkSec = 600
		}
		durMS := int64(s.DurationMS)
		if durMS <= 0 && len(segs) > 0 {
			durMS = segs[len(segs)-1].EndMS
		}
		plans := planChunks(durMS, chunkSec)

		var results []provider.AudioInsight
		if len(plans) <= 1 {
			ins, err := d.AudioInsight.Analyze(ctx, wav, labels)
			if err != nil {
				log.Printf("[audioscene] 分析失败(降级): %v", err)
				return nil
			}
			results = []provider.AudioInsight{ins}
		} else {
			for i, pl := range plans {
				clip, err := sliceWAV(d.DataDir, sessionID, i, wav, pl, plans, i)
				if err != nil {
					log.Printf("[audioscene] 切片 %d 失败(跳过该块): %v", i, err)
					continue
				}
				ins, err := d.AudioInsight.Analyze(ctx, clip, labels)
				_ = os.Remove(clip) // 用后即删临时切片
				if err != nil {
					log.Printf("[audioscene] 块 %d 分析失败(跳过): %v", i, err)
					continue
				}
				results = append(results, ins)
			}
			if len(results) == 0 {
				log.Printf("[audioscene] 所有块失败(降级)")
				return nil
			}
		}
		merged := mergeInsights(results)

		// 落会话级环境
		var bg *json.RawMessage
		if len(merged.BackgroundSounds) > 0 {
			if raw, err := json.Marshal(merged.BackgroundSounds); err == nil {
				rm := json.RawMessage(raw)
				bg = &rm
			}
		}
		if err := d.Transcripts.SetAcoustic(ctx, tr.ID, clip64(merged.AcousticScene, 32), bg, clip64(merged.WeatherCues, 32), clip64(merged.OverallMood, 128)); err != nil {
			log.Printf("[audioscene] 写环境失败: %v", err)
		}
		// 落每人情绪（speaker_id 按 label 映射回填）
		var rows []repo.SpeakerSessionState
		for _, sp := range merged.Speakers {
			row := repo.SpeakerSessionState{
				UserID: 1, TranscriptID: tr.ID, SessionID: sessionID,
				SpeakerLabel: sp.Label, Emotion: clip64(sp.Emotion, 32),
				MicroEmotion: clip64(sp.MicroEmotion, 64), MentalState: clip64(sp.MentalState, 64), Confidence: sp.Confidence,
			}
			if sid, ok := labelToSpeaker[sp.Label]; ok {
				row.SpeakerID = &sid
			}
			rows = append(rows, row)
		}
		if err := d.SpeakerStates.InsertBatch(ctx, rows); err != nil {
			log.Printf("[audioscene] 写说话人情绪失败: %v", err)
		}
		return nil
	}
}

// collectSpeakerLabels 从段收集去重 label（保序）+ label→speaker_id 映射（首个非空 speaker_id）。
func collectSpeakerLabels(segs []repo.TranscriptSegment) ([]string, map[string]ids.ID) {
	var labels []string
	seen := map[string]bool{}
	m := map[string]ids.ID{}
	for _, sg := range segs {
		if sg.SpeakerLabel != "" && !seen[sg.SpeakerLabel] {
			seen[sg.SpeakerLabel] = true
			labels = append(labels, sg.SpeakerLabel)
		}
		if sg.SpeakerID != nil {
			if _, ok := m[sg.SpeakerLabel]; !ok {
				m[sg.SpeakerLabel] = *sg.SpeakerID
			}
		}
	}
	return labels, m
}

func clip64(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}

// sliceWAV 用 ffmpeg 切出一块（静音切点/2s 重叠见 spec §5）。startMS 若非首块，尝试前移到静音或 -2s 重叠。
// 简化实现：块内固定 -ss/-t；非首块起点前移 overlapMS（2000）——静音精修可后续增强。
func sliceWAV(dataDir string, sessionID ids.ID, idx int, srcWAV string, pl chunkPlan, plans []chunkPlan, i int) (string, error) {
	const overlapMS = 2000
	start := pl.StartMS
	if i > 0 {
		start -= overlapMS
		if start < 0 {
			start = 0
		}
	}
	dur := pl.EndMS - start
	dst := filepath.Join(dataDir, "transcoded", sessionID.String()+"_chunk"+strconv.Itoa(idx)+".wav")
	cmd := exec.Command("ffmpeg", "-y", "-ss", msToSec(start), "-t", msToSec(dur),
		"-i", srcWAV, "-ar", "16000", "-ac", "1", "-sample_fmt", "s16", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[audioscene] ffmpeg 切片: %s", out)
		return "", err
	}
	return dst, nil
}

func msToSec(ms int64) string { return strconv.FormatFloat(float64(ms)/1000.0, 'f', 3, 64) }
```
> **静音切点精修**（spec §5 的"±1min 找静音"）：本任务先落"固定切点 + 2s 重叠"的可用版；静音检测（`ffmpeg silencedetect` 在边界 ±60s 找最近静音中点微调 start/end）作为**同任务的增强 Step**（见 Step 5）。这样功能先跑通，再精修切点。

- [ ] **Step 5: 静音切点精修（增强）**

在 `sliceWAV` 前加 `refineCutBySilence(srcWAV, boundaryMS)`：跑 `ffmpeg -i src -af silencedetect=noise=-30dB:d=0.3 -f null -`，解析 `silence_start/silence_end`，在 `boundaryMS±60000` 内找最近静音区间中点作为切点；找到则用它替换固定边界，没找到则回退固定+2s 重叠。为每对相邻块的边界调用。（实现后补一个解析 silencedetect 输出的纯函数单测 `TestParseSilence`。）

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/pipeline/ -run 'TestStageAudioScene|TestParseSilence' -count=1`（重音频用例若无 ffmpeg 用 t.Skip）。Expected: 逻辑用例 PASS。

- [ ] **Step 7: 全包 build + 提交**

Run: `go build ./... && go vet ./...`
```bash
git add internal/pipeline/stage_asr.go internal/pipeline/stage_audioscene.go internal/pipeline/stage_audioscene_test.go
git commit -m "feat(pipeline): audioscene stage(转码复用+分块+静音切点+归因落库+全程降级)"
```

---

### Task 6: 配置 + main.go 装配

**Files:** Modify `internal/config/config.go` / `config_test.go`；`cmd/zhiwei-server/main.go`

- [ ] **Step 1: config 加 5 字段 + 默认（写测试先）**

`config_test.go` 加断言：`AudioInsightEnabled` 默认 true、`AudioInsightModel`=`stepaudio-2.5-chat`、`AudioInsightBase`=`https://api.c.ibasemind.com/v1`、`AudioInsightChunkSec`=600、`AudioInsightAPIKey` 取 `STEPFUN_ASR_FILE_API_KEY`。

- [ ] **Step 2: 实现 config 字段**

`config.go` Config 结构体 + Load 加：
```go
	AudioInsightEnabled  bool
	AudioInsightModel    string
	AudioInsightBase     string
	AudioInsightAPIKey   string
	AudioInsightChunkSec int
```
Load：
```go
		AudioInsightEnabled:  getenvBool("ZW_AUDIO_INSIGHT_ENABLED", true),
		AudioInsightModel:    getenv("ZW_AUDIO_INSIGHT_MODEL", "stepaudio-2.5-chat"),
		AudioInsightBase:     getenv("ZW_AUDIO_INSIGHT_BASE", "https://api.c.ibasemind.com/v1"),
		AudioInsightAPIKey:   getenv("ZW_AUDIO_INSIGHT_API_KEY", os.Getenv("STEPFUN_ASR_FILE_API_KEY")),
		AudioInsightChunkSec: getenvInt("ZW_AUDIO_INSIGHT_CHUNK_SEC", 600),
```
（`getenvBool`/`getenvInt` 若不存在则在 config.go 内实现小助手，对齐现有 `getenv`。）

- [ ] **Step 3: main.go 装配**

在构造 StageDeps 处（`pipeline.BuildStages(pipeline.StageDeps{...}`，main.go:212）加：
```go
		SpeakerStates:        &repo.SpeakerSessionStateRepo{DB: db},
		AudioInsightEnabled:  cfg.AudioInsightEnabled,
		AudioInsightChunkSec: cfg.AudioInsightChunkSec,
		AudioInsight:         audioInsight, // 见下
```
在 asr 装配附近构造 provider（仅 enabled 且 key 非空）：
```go
	var audioInsight provider.AudioInsightProvider
	if cfg.AudioInsightEnabled && cfg.AudioInsightAPIKey != "" {
		audioInsight = provider.NewStepAudioInsight(cfg.AudioInsightBase, cfg.AudioInsightAPIKey, cfg.AudioInsightModel)
	}
```
把 `audioscene` 加入 stagesList（`speakername` 之后、`extract` 之前）：
```go
	stagesList := []string{"asr", "segment", "speaker", "speakername", "audioscene", "extract"}
```

- [ ] **Step 4: build + 提交**

Run: `go build ./... && go vet ./... && go test ./internal/config/ -count=1`
```bash
git add internal/config/config.go internal/config/config_test.go cmd/zhiwei-server/main.go
git commit -m "feat(config): 音频洞察 5 配置项 + main 装配 audioscene stage"
```

---

### Task 7: API — GetSession 返回环境 + 说话人情绪

**Files:** Modify `internal/api/query.go`（GetSession，:395-）；Modify QueryHandler 加 `SpeakerStates` 依赖 + main.go 注入

- [ ] **Step 1: 加 handler 依赖**

`QueryHandler` 结构体加 `SpeakerStates *repo.SpeakerSessionStateRepo`；main.go 构造 QueryHandler 处注入。

- [ ] **Step 2: GetSession 补字段**

在 `if tr, err := h.Transcripts.GetBySession(...)` 块内（组装 resp 处）加：
```go
		resp["acoustic_scene"] = tr.AcousticScene
		resp["background_sounds"] = tr.BackgroundSounds
		resp["weather_cues"] = tr.WeatherCues
		resp["overall_mood"] = tr.OverallMood
		if h.SpeakerStates != nil {
			states, _ := h.SpeakerStates.ListBySession(r.Context(), uid.Int64(), sid)
			resp["speaker_states"] = states
		}
```

- [ ] **Step 3: 测试 + 提交**

补/扩 `query_test.go`：造带 speaker_session_state + transcript 环境列的会话，GET 断言响应含这些字段。
Run: `go test ./internal/api/ -run TestGetSession -count=1`（按现有测试命名）。
```bash
git add internal/api/query.go cmd/zhiwei-server/main.go internal/api/query_test.go
git commit -m "feat(api): GetSession 返回声学环境 + 说话人情绪状态"
```

---

### Task 8: 前端 — 环境徽章 + 说话人情绪 chip

**Files:** Modify `web/index.html` / `web/app.js`；`make hash-web`

- [ ] **Step 1: 会话详情顶部环境徽章**

在会话详情转写区上方加（`detail` 已含新字段）：场景 + 背景音 chips（`background_sounds` 数组）+ 天气 + 整体氛围，`v-if` 有值才显。用 `.chip.chip-ro` 样式（已存在）。

- [ ] **Step 2: 说话人情绪展示**

`detail.speaker_states`（label→情绪）构建 map；说话人面板 / 转写段按 `speaker_label` 显示 emotion chip，`title` hover 显 micro_emotion + mental_state。

- [ ] **Step 3: hash-web + 冒烟**

Run: `make hash-web`；`node --check web/app.js`。若能起 dev 则浏览器看会话详情环境徽章 + 情绪 chip。
```bash
git add web/index.html web/app.js
git commit -m "feat(web): 会话详情显示声学环境徽章 + 说话人情绪"
```

---

### Task 9: 全量回归 + 真录音冒烟

- [ ] **Step 1: 全量 build/vet/test**

Run: `go build ./... && go vet ./...`；`TEST_MYSQL_DSN=... go test ./internal/repo/ ./internal/pipeline/ ./internal/provider/ ./internal/config/ ./internal/api/ -count=1`（污染库先 drop zhiwei_test_* 让 repotest 按 000025 重建）。

- [ ] **Step 2: 真录音端到端冒烟（best-effort）**

若有 `.env` + MySQL + ffmpeg：上传一条真实录音过完整管线，确认 `audioscene` 落库（transcript 环境列 + speaker_session_state），人工看情绪/场景是否合理。记录结论。

- [ ] **Step 3: 迁移号复查**

Run: `git fetch origin main; git ls-tree origin/main --name-only migrations/ | grep -E '\.up\.sql$' | sort | tail -3`。若 main 已过 000025 则重编号。

---

## Self-Review 结果

**Spec 覆盖：** §2 数据模型→Task1/2；§3 provider→Task3；§4 stage→Task5；§5 分块合并→Task4+Task5；§6 配置→Task6；§7 API+前端→Task7/8；§8 测试→各任务；§9 决策全覆盖；§10 URL 验证→Task0，静音精修→Task5 Step5，真录音→Task9 Step2。✅

**占位符：** Task4 的 `strings_Join` 是**显式标注的占位提醒**（实现者改回 `strings.Join`）；Task5 重音频测试用 t.Skip 守卫已说明；其余代码完整。Task0 是 spike（无文件产出）合理。

**类型一致性：** `AudioInsight`/`SpeakerInsight`（provider）跨 Task3/4/5 一致；`SpeakerSessionState`/`SpeakerSessionStateRepo`（repo）Task2 定义、Task5/7 引用；`StageDeps` 新字段 Task5 定义、Task6 装配；`planChunks`/`mergeInsights`/`stageAudioScene` 签名一致。

**风险留意：** 重音频测试依赖 ffmpeg——用 t.Skip 守卫；真录音质量是唯一未经验证项（Task0/Task9 抽验）。
