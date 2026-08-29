# P2 人物情绪汇总（C）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 新增 `emotionprofile` 管线 stage，把 P1 落库的逐说话人情绪（`speaker_session_state`）聚合成人物级情绪，写入 `PersonMetric`（emotion，valence 数值）时序平面。

**Architecture:** audioscene 之后、extract 之前插入确定性聚合 stage（不调 LLM）：读会话的 speaker_session_state → 每个已识别说话人（speaker_id→person）的类别情绪映射 valence → 写 PersonMetric(emotion, source=auto, measured_at=会话时间)。跳过未识别、幂等防 stage 重跑重复、开关控制、失败降级。

**Tech Stack:** Go + chi + sqlx + MySQL(golang-migrate) + 现有管线框架。无新迁移、无 LLM、无前端改动（复用现有情绪曲线）。

**规格：** `docs/superpowers/specs/2026-08-29-emotion-profile-p2-design.md`。

**测试约定：** repo/stage 集成测试 `repotest.DSN(t)`（未设 TEST_MYSQL_DSN 则 skip）；stage 测试照 `stage_audioscene_test.go` 模式（fake 依赖注入 + `BuildStages(StageDeps{...})`）；`ids.ID` 用 `.Int64()`/`.String()`。**无新迁移**（复用 PersonMetric + speaker_session_state）。每任务末尾提交；Go 改动后 `go build ./... && go vet ./...`。MySQL 在 127.0.0.1:3307，DSN=`zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true`。worktree 内 testdata 未跟踪（用绝对路径 `/Users/jyxc-dz-0100360/work/fun/zhiwei-water18-0822/testdata/`）。

---

### Task 1: 情绪→valence 映射（纯函数）

**Files:** Create `internal/profile/emotion_valence.go` / `emotion_valence_test.go`

- [ ] **Step 1: 写失败测试**

`internal/profile/emotion_valence_test.go`：
```go
package profile

import (
	"math"
	"testing"
)

func TestEmotionToValence(t *testing.T) {
	// 正价情绪 > 0
	if v := EmotionToValence("喜悦"); v <= 0 { t.Errorf("喜悦 应 >0, got %v", v) }
	if v := EmotionToValence(" 开心 "); v <= 0 { t.Errorf("去空格后 开心 应 >0, got %v", v) }
	// 负价情绪 < 0
	if v := EmotionToValence("愤怒"); v >= 0 { t.Errorf("愤怒 应 <0, got %v", v) }
	if v := EmotionToValence("焦虑"); v >= 0 { t.Errorf("焦虑 应 <0, got %v", v) }
	// 中性
	if v := EmotionToValence("平静"); math.Abs(v) > 0.5 { t.Errorf("平静 应接近中性, got %v", v) }
	// 未收录 → 0（中性回落）
	if v := EmotionToValence("某种未知情绪"); v != 0 { t.Errorf("未收录应回落 0, got %v", v) }
	if v := EmotionToValence(""); v != 0 { t.Errorf("空应回落 0, got %v", v) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go vet ./internal/profile/`。Expected: `EmotionToValence` 未定义。

- [ ] **Step 3: 实现 emotion_valence.go**

`internal/profile/emotion_valence.go`：
```go
package profile

import "strings"

// EmotionValence 类别情绪 → 效价 −1..1（PersonMetric.emotion 的 value_num，spec §4）。
// 覆盖 audioscene 常见输出；值为经验设定，未收录回落 0（中性），不报错。
var EmotionValence = map[string]float64{
	"喜悦": 0.8, "开心": 0.8, "兴奋": 0.9, "满足": 0.6, "平静": 0.2, "中性": 0.0,
	"疲惫": -0.4, "焦虑": -0.6, "紧张": -0.5, "愤怒": -0.9, "悲伤": -0.8,
	"沮丧": -0.7, "无聊": -0.2, "困惑": -0.3,
}

// EmotionToValence 类别情绪 → 效价；去首尾空格，未收录回落 0（中性）。
func EmotionToValence(emotion string) float64 {
	if v, ok := EmotionValence[strings.TrimSpace(emotion)]; ok {
		return v
	}
	return 0.0
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/profile/ -run TestEmotionToValence -v -count=1`。Expected: PASS。

- [ ] **Step 5: 提交**
```bash
git add internal/profile/emotion_valence.go internal/profile/emotion_valence_test.go
git commit -m "feat(profile): 情绪→valence 映射表(类别情绪→-1..1, 未收录回落0)"
```

---

### Task 2: stage emotionprofile + StageDeps 字段 + BuildStages 注册

**Files:** Create `internal/pipeline/stage_emotionprofile.go` / `stage_emotionprofile_test.go`；Modify `internal/pipeline/stage_asr.go`（StageDeps 加字段 + BuildStages 注册）

- [ ] **Step 1: 写失败测试**

`internal/pipeline/stage_emotionprofile_test.go`（照 stage_audioscene_test 模式，用 fake 依赖——但本 stage 依赖都是真 repo，用 repotest 真实库）：
```go
package pipeline

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/repotest"
)

// 关闭 / 缺依赖：no-op 返回 nil。
func TestStageEmotionProfileDisabledSkips(t *testing.T) {
	h := stageEmotionProfile(StageDeps{EmotionProfileEnabled: false})
	if err := h(context.Background(), nil, ids.New()); err != nil {
		t.Errorf("关闭时应 no-op 返回 nil, got %v", err)
	}
	h2 := stageEmotionProfile(StageDeps{EmotionProfileEnabled: true, PersonMetrics: nil})
	if err := h2(context.Background(), nil, ids.New()); err != nil {
		t.Errorf("缺 PersonMetrics 应 no-op 返回 nil, got %v", err)
	}
}

// 落库：speaker_session_state → PersonMetric(emotion)，valence 映射对、source=auto、measured_at=会话时间。
func TestStageEmotionProfileWritesMetrics(t *testing.T) {
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sessions := &repo.SessionRepo{DB: db}
	persons := &repo.PersonRepo{DB: db}
	metrics := &repo.PersonMetricRepo{DB: db}
	states := &repo.SpeakerSessionStateRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}

	// 建 session
	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{ID: sid, Source: "web_upload", Filename: "x.wav", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	sess, _ := sessions.Get(ctx, 1, sid)

	// 建 speaker + 绑定 person（GetBySpeaker 依赖 person.speaker_id）
	sp := &repo.Speaker{UserID: 1, Name: "甲"}
	_ = speakers.Create(ctx, sp)
	p := &repo.Person{UserID: 1, DisplayName: "甲", SpeakerID: &sp.ID}
	_ = persons.Create(ctx, p)

	// 未绑定 person 的 speaker（应跳过）
	spOrphan := &repo.Speaker{UserID: 1, Name: "乙"}
	_ = speakers.Create(ctx, spOrphan)

	// speaker_session_state：甲(喜悦→绑定person) + 乙(焦虑→未绑定跳过)
	tid := ids.New()
	_ = states.InsertBatch(ctx, []repo.SpeakerSessionState{
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "1", SpeakerID: &sp.ID, Emotion: "喜悦", Confidence: 0.8},
		{UserID: 1, TranscriptID: tid, SessionID: sid, SpeakerLabel: "2", SpeakerID: &spOrphan.ID, Emotion: "焦虑", Confidence: 0.6},
	})

	d := StageDeps{
		Sessions: sessions, Persons: persons, PersonMetrics: metrics, SpeakerStates: states,
		EmotionProfileEnabled: true,
	}
	if err := stageEmotionProfile(d)(ctx, nil, sid); err != nil {
		t.Fatalf("stage 应成功: %v", err)
	}

	// 甲的 emotion 落库（valence=喜悦=0.8）
	rows, err := metrics.ListByPerson(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found *repo.PersonMetric
	for i := range rows {
		if rows[i].MetricKey == "emotion" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("甲的 emotion metric 未写入")
	}
	if found.ValueNum == nil || *found.ValueNum <= 0 {
		t.Errorf("喜悦 valence 应 >0, got %v", found.ValueNum)
	}
	if found.ValueText == nil || *found.ValueText != "喜悦" {
		t.Errorf("value_text 应=喜悦, got %v", found.ValueText)
	}
	if found.Source != "auto" {
		t.Errorf("source 应=auto, got %q", found.Source)
	}
	if !found.MeasuredAt.Equal(sess.CreatedAt) {
		t.Errorf("measured_at 应=会话 created_at, got %v want %v", found.MeasuredAt, sess.CreatedAt)
	}

	// 幂等：重跑不重复写
	if err := stageEmotionProfile(d)(ctx, nil, sid); err != nil {
		t.Fatal(err)
	}
	rows2, _ := metrics.ListByPerson(ctx, p.ID)
	cnt := 0
	for _, r := range rows2 {
		if r.MetricKey == "emotion" {
			cnt++
		}
	}
	if cnt != 1 {
		t.Errorf("幂等:重跑后应仍 1 条 emotion, got %d", cnt)
	}
}
```
（说明：`PersonRepo.Create`、`AudioSession` 字段、`SpeakerSessionState` 字段以实际 repo 为准；若某 Create 签名不同，照实际调整。`found.MeasuredAt.Equal(sess.CreatedAt)` 若时间精度有截断（DATETIME(3) vs time.Time），可改为比较到秒或去掉该断言——plan 阶段实现者据实测调整。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go vet ./internal/pipeline/`。Expected: `stageEmotionProfile` / StageDeps 新字段 未定义。

- [ ] **Step 3: StageDeps 加字段 + BuildStages 注册**

`internal/pipeline/stage_asr.go` 的 StageDeps 结构体末尾（audioscene 字段后）加：
```go
	// ---- emotionprofile stage（P2 人物情绪汇总）----
	PersonMetrics         *repo.PersonMetricRepo
	Persons               *repo.PersonRepo
	EmotionProfileEnabled bool
```
`BuildStages` map 里，`"audioscene": stageAudioScene(d),` 之后加：
```go
		"emotionprofile": stageEmotionProfile(d),
```

- [ ] **Step 4: 实现 stage_emotionprofile.go**

`internal/pipeline/stage_emotionprofile.go`：
```go
// stage_emotionprofile 实现 emotionprofile stage（P2 人物情绪汇总，spec §5）。
// 确定性聚合（不调 LLM）：读本会话 speaker_session_state，把每个已识别说话人
// （speaker_id→person）的类别情绪映射 valence，写 PersonMetric(emotion, source=auto)。
// 跳过未识别（speaker_id 空/找不到 person）；幂等防 stage 重跑重复；全程降级不阻断。
package pipeline

import (
	"log"

	"zhiwei/internal/ids"
	"zhiwei/internal/profile"
	"zhiwei/internal/repo"
)

func stageEmotionProfile(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
		if !d.EmotionProfileEnabled || d.PersonMetrics == nil || d.Persons == nil || d.SpeakerStates == nil {
			return nil // 开关关闭或缺依赖：no-op
		}
		s, err := d.Sessions.Get(ctx, 1, sessionID)
		if err != nil {
			log.Printf("[emotionprofile] 读 session 失败(降级): %v", err)
			return nil
		}
		states, err := d.SpeakerStates.ListBySession(ctx, 1, sessionID)
		if err != nil {
			log.Printf("[emotionprofile] 读说话人情绪失败(降级): %v", err)
			return nil
		}
		if len(states) == 0 {
			return nil
		}
		for i := range states {
			st := &states[i]
			if st.SpeakerID == nil {
				continue // 未识别说话人：跳过
			}
			person, err := d.Persons.GetBySpeaker(ctx, *st.SpeakerID)
			if err != nil {
				continue // 找不到绑定 person：跳过（未建档/未关联）
			}
			// 幂等：同 person+emotion+measured_at(会话时间) 已写过则跳过（防 stage 重跑重复）。
			// FindByPointExt 自然键含 measured_at+值；LLM 抽取用 source=extract 且 measured_at 不同，不冲突。
			valence := profile.EmotionToValence(st.Emotion)
			vn, vt := valence, st.Emotion
			ex, err := d.PersonMetrics.FindByPointExt(ctx, d.PersonMetrics, person.ID, "emotion", s.CreatedAt, &vn, &vt)
			if err == nil && ex != nil {
				continue // 已写过：幂等跳过
			}
			row := &repo.PersonMetric{
				UserID: 1, PersonID: person.ID, MetricKey: "emotion",
				ValueNum: &vn, ValueText: &vt,
				MeasuredAt:    s.CreatedAt,
				Confidence:    st.Confidence,
				EpistemicType: "observed",
				Source:        "auto",
				Status:        "active",
				TranscriptSegmentIDs: ids.List{},
			}
			if err := d.PersonMetrics.Create(ctx, row); err != nil {
				log.Printf("[emotionprofile] 写 emotion metric 失败(person=%s, 跳过): %v", person.ID, err)
				continue
			}
		}
		return nil
	}
}
```
> **注意**：`FindByPointExt(ctx, q QueryerContext, ...)` 第一参是 `QueryerContext`（`*sqlx.DB` 或 `*sqlx.Tx`）。`d.PersonMetrics` 是 `*repo.PersonMetricRepo`，不是 QueryerContext。需传 DB：给 StageDeps 已可直接用 `d.PersonMetrics` 的 DB，或 StageDeps 加 `DB *sqlx.DB`。**最简**：直接复用 PersonMetricRepo 里已注入的 DB。看 PersonMetricRepo 结构是 `struct{ DB *sqlx.DB }`，但字段私有不可外取。解决：StageDeps 已有 `DB *sqlx.DB`（extract stage 用），复用 `d.DB` 作 QueryerContext 传给 FindByPointExt。即 `FindByPointExt(ctx, d.DB, ...)`。**plan 阶段实现者据 StageDeps 实际字段确认 d.DB 存在并用它。** 若 FindByPointExt 不按 source 区分（只按 measured_at+值），而本会话 measured_at 唯一，已足够幂等；LLM 的 extract 行 measured_at 是记忆 event_at（通常≠会话 created_at），不冲突。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/pipeline/ -run 'TestStageEmotionProfile' -v -count=1`（DSN 已设）。Expected: PASS。（时间精度断言据实测调整。）

- [ ] **Step 6: 全包 build + 提交**

Run: `go build ./... && go vet ./...`
```bash
git add internal/pipeline/stage_emotionprofile.go internal/pipeline/stage_emotionprofile_test.go internal/pipeline/stage_asr.go
git commit -m "feat(pipeline): emotionprofile stage(逐说话人情绪聚合写 PersonMetric)"
```

---

### Task 3: 配置 + main.go 装配

**Files:** Modify `internal/config/config.go` / `config_test.go`；`cmd/zhiwei-server/main.go`

- [ ] **Step 1: config 加字段 + 默认 + 测试**

`internal/config/config.go` Config 结构体加：
```go
	EmotionProfileEnabled bool // ZW_EMOTION_PROFILE_ENABLED：是否启用人物情绪汇总阶段（默认 true）
```
Load() 的 `return &Config{...}` 加：
```go
		EmotionProfileEnabled: getenvBool("ZW_EMOTION_PROFILE_ENABLED", true),
```
`config_test.go` 加断言（找现有默认值测试）：
```go
	if c.EmotionProfileEnabled != true { t.Errorf("EmotionProfileEnabled 默认应 true") }
```

- [ ] **Step 2: main.go 装配**

读 `cmd/zhiwei-server/main.go` 找三处：
- (a) 构造 `pipeline.StageDeps{...}`（含 SpeakerStates 的 audioscene 分组）——在其后加 PersonMetrics/Persons/EmotionProfileEnabled。
- (b) `stagesList := []string{...}`（含 "audioscene"）——在 "audioscene" 后、"extract" 前插入 "emotionprofile"。

(a) 在 audioscene 分组后加：
```go
		// ---- emotionprofile stage（P2 人物情绪汇总）----
		PersonMetrics:         &repo.PersonMetricRepo{DB: db},
		Persons:               persons,
		EmotionProfileEnabled: cfg.EmotionProfileEnabled,
```
（确认 `db`、`persons` 在该作用域可见——StageDeps 构造处必有 db；persons 若未构造则找现成的 person repo，照现有 SpeakerStates: `&repo.X{DB: db}` 风格。）
(b) stagesList：
```go
	stagesList := []string{"asr", "segment", "speaker", "speakername", "audioscene", "emotionprofile", "extract"}
```
（若原是 append profile 形式，照原样在 audioscene 后、extract 前加 "emotionprofile"。）

- [ ] **Step 3: build + 提交**

Run: `go build ./... && go vet ./... && go test ./internal/config/ -count=1`
```bash
git add internal/config/config.go internal/config/config_test.go cmd/zhiwei-server/main.go
git commit -m "feat(config): EmotionProfileEnabled 配置 + main 装配 emotionprofile stage"
```

---

### Task 4: 全量回归

- [ ] **Step 1: 全量 build/vet + 相关包测试**

Run: `go build ./... && go vet ./...`；`TEST_MYSQL_DSN=... go test ./internal/profile/ ./internal/pipeline/ ./internal/config/ -run 'TestEmotionToValence|TestStageEmotionProfile|TestEmotionProfile' -count=1`。Expected: 全 PASS。

- [ ] **Step 2: 真录音冒烟（best-effort）**

若有 ffmpeg + 真实录音：跑完整管线（asr→…→audioscene→emotionprofile），确认 person 的 emotion metric 落库。无真录音则靠 Task 2 集成测试覆盖。

---

## Self-Review 结果

**Spec 覆盖：** §4 映射→Task1；§5 stage→Task2；§6 配置装配→Task3；§8 测试→Task1/2/4；§9 决策全覆盖。§7 前端=零改动（复用情绪曲线）→无 Task，符合。✅

**占位符：** Task2 的 FindByPointExt 用 `d.DB` 是**显式标注的实现注意**（QueryerContext 类型），非模糊 TODO。时间精度断言标了"据实测调整"。其余代码完整。

**类型一致性：** `EmotionToValence`(profile) Task1 定义、Task2 引用；`stageEmotionProfile`/StageDeps 新字段 Task2 定义、Task3 装配；`SpeakerSessionState`/`PersonMetric`/`Person`/`Speaker` 字段以现有 repo 为准。

**关键正确性点：** ①幂等靠 FindByPointExt 自然键（measured_at=会话时间唯一）+ LLM 的 extract 行 measured_at 不同不冲突；②source=auto 与 LLM 的 extract 区分；③跳过未识别不造占位 person；④降级不阻断。

**无新迁移** → 无撞号风险。
