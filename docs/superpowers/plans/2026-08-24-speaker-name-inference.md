# 说话人名字推断（speakername stage）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 声纹识别新建/命中随机名说话人时，用 LLM 从跨录音最近 10 分钟对话推断真实称呼，作为带置信度的候选名供用户确认采纳。

**Architecture:** 新增独立 `speakername` stage（`asr → segment → speaker → speakername → extract`），单次批处理 LLM 调用覆盖本 session 全部待识别说话人；候选存新表 `speaker_name_candidate`（跨 session upsert 累积），API 富化到 speaker 列表，前端展示「名称 + 置信度数值」并支持采纳/忽略。规格见 `docs/superpowers/specs/2026-08-24-speaker-name-inference-design.md`。

**Tech Stack:** Go（chi + sqlx + MySQL 迁移）、Ark LLM（复用现有 `LLMFastModel`）、Vue 3 CDN 单页（本地 vendor）。

**约定（全计划适用）：**
- 集成测试需 MySQL：先 `make compose-up`，再 `make test-integration`（自动重建 zhiwei_test 库 + 迁移 + 串行跑）。纯函数测试 `go test ./internal/pipeline/ -run <名>` 无需 DB。
- 本项目所有测试文件所在包已有 `TestMain` 初始化雪花 ID（`internal/pipeline/main_test.go`、`internal/repo/main_test.go`），无需重复调用 `ids.Init`。
- 提交信息末尾加：`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`。

---

### Task 1: 迁移 000005_speaker_name_candidate

**Files:**
- Create: `migrations/000005_speaker_name_candidate.up.sql`
- Create: `migrations/000005_speaker_name_candidate.down.sql`

- [ ] **Step 1: 写 up 迁移**

`migrations/000005_speaker_name_candidate.up.sql`：

```sql
-- 说话人名字候选（speakername stage 用 LLM 从对话上下文推断的称呼建议）。
-- 一个说话人 N 行候选；uk_speaker_name 唯一键支撑跨 session upsert 累积
-- （confidence 取 GREATEST、证据留最新），见 repo.Upsert。
-- 仅作建议：不改 speaker.name；用户采纳（改名）后整组删除。
CREATE TABLE speaker_name_candidate (
  id                BIGINT PRIMARY KEY,
  speaker_id        BIGINT NOT NULL,                       -- 归属说话人（speaker.id）
  name              VARCHAR(128) NOT NULL,                 -- 候选名（张总/王哥/张三…）
  confidence        DOUBLE NOT NULL DEFAULT 0,             -- 置信度 [0,1]，展示排序键
  evidence          VARCHAR(512) NULL,                     -- 依据：简短引用 + 时间点
  source_session_id BIGINT NULL,                           -- 最近一次产生该候选的会话
  created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_speaker_name (speaker_id, name),
  KEY idx_speaker (speaker_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

- [ ] **Step 2: 写 down 迁移**

`migrations/000005_speaker_name_candidate.down.sql`：

```sql
DROP TABLE speaker_name_candidate;
```

- [ ] **Step 3: 验证迁移可执行**

Run: `make compose-up && make init-testdb`
Expected: `init-testdb` 无报错退出（golang-migrate 对 zhiwei_test 跑完 000001-000005）。

- [ ] **Step 4: Commit**

```bash
git add migrations/000005_speaker_name_candidate.up.sql migrations/000005_speaker_name_candidate.down.sql
git commit -m "feat(migration): speaker_name_candidate 表（说话人名字候选）"
```

---

### Task 2: SpeakerNameCandidateRepo

**Files:**
- Create: `internal/repo/speaker_name_candidate.go`
- Create: `internal/repo/speaker_name_candidate_test.go`

- [ ] **Step 1: 写失败测试**

`internal/repo/speaker_name_candidate_test.go`：

```go
package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

// seedCandidate 准备一个 speaker（随机名，模拟自动登记），返回其 ID。
func seedCandidate(t *testing.T, name string) ids.ID {
	t.Helper()
	speakers := &SpeakerRepo{DB: testDB(t)}
	sp := &Speaker{Name: name, Source: "auto"}
	if err := speakers.Create(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	return sp.ID
}

func TestCandidateUpsertAndList(t *testing.T) {
	db := testDB(t)
	r := &SpeakerNameCandidateRepo{DB: db}
	ctx := context.Background()
	sid := seedCandidate(t, "说话人ab3x9")

	// 初次插入两个候选（第二行故意低置信度，验证倒序）
	if err := r.Upsert(ctx, sid, "张总", 0.82, "对方在 15:03:12 说『张总，您看这个方案』", 1001); err != nil {
		t.Fatal(err)
	}
	if err := r.Upsert(ctx, sid, "张明", 0.4, "自称『我姓张』", 1001); err != nil {
		t.Fatal(err)
	}
	list, err := r.ListBySpeakers(ctx, []ids.ID{sid})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 条候选，实际 %d", len(list))
	}
	if list[0].Name != "张总" || list[0].Confidence != 0.82 {
		t.Fatalf("倒序首位应为 张总/0.82，实际 %s/%.2f", list[0].Name, list[0].Confidence)
	}
	if list[0].Evidence != "对方在 15:03:12 说『张总，您看这个方案』" {
		t.Fatalf("evidence=%s", list[0].Evidence)
	}
}

func TestCandidateUpsertAccumulatesMaxConfidence(t *testing.T) {
	db := testDB(t)
	r := &SpeakerNameCandidateRepo{DB: db}
	ctx := context.Background()
	sid := seedCandidate(t, "说话人cd4e0")

	// 同名候选跨 session 复现：第二次置信度更低 → 保留最高置信、证据取最新
	if err := r.Upsert(ctx, sid, "张总", 0.82, "旧证据", 1001); err != nil {
		t.Fatal(err)
	}
	if err := r.Upsert(ctx, sid, "张总", 0.5, "新证据", 1002); err != nil {
		t.Fatal(err)
	}
	list, _ := r.ListBySpeakers(ctx, []ids.ID{sid})
	if len(list) != 1 {
		t.Fatalf("同名 upsert 后仍应 1 行，实际 %d", len(list))
	}
	if list[0].Confidence != 0.82 {
		t.Fatalf("应保留最高置信 0.82，实际 %.2f", list[0].Confidence)
	}
	if list[0].Evidence != "新证据" {
		t.Fatalf("证据应取最新，实际 %s", list[0].Evidence)
	}
	// 反向：第二次更高 → 抬升
	if err := r.Upsert(ctx, sid, "张总", 0.95, "更强证据", 1003); err != nil {
		t.Fatal(err)
	}
	list, _ = r.ListBySpeakers(ctx, []ids.ID{sid})
	if list[0].Confidence != 0.95 {
		t.Fatalf("置信度应抬升到 0.95，实际 %.2f", list[0].Confidence)
	}
}

func TestCandidateDelete(t *testing.T) {
	db := testDB(t)
	r := &SpeakerNameCandidateRepo{DB: db}
	ctx := context.Background()
	sid := seedCandidate(t, "说话人ef5a1")
	other := seedCandidate(t, "说话人gh6b2")
	_ = r.Upsert(ctx, sid, "张总", 0.8, "", 1001)
	_ = r.Upsert(ctx, sid, "张明", 0.4, "", 1001)
	_ = r.Upsert(ctx, other, "李哥", 0.7, "", 1001)

	// 删单个候选（前端「忽略」）
	if err := r.DeleteOne(ctx, sid, "张明"); err != nil {
		t.Fatal(err)
	}
	list, _ := r.ListBySpeakers(ctx, []ids.ID{sid})
	if len(list) != 1 || list[0].Name != "张总" {
		t.Fatalf("删单条后应剩 张总，实际 %+v", list)
	}
	// 幂等：删不存在的候选不报错
	if err := r.DeleteOne(ctx, sid, "不存在"); err != nil {
		t.Fatalf("删不存在候选应幂等: %v", err)
	}
	// 按说话人清空（改名采纳后调用），不影响他人
	if err := r.DeleteBySpeaker(ctx, sid); err != nil {
		t.Fatal(err)
	}
	list, _ = r.ListBySpeakers(ctx, []ids.ID{sid, other})
	if len(list) != 1 || list[0].SpeakerID != other {
		t.Fatalf("清空后应只剩 other 的候选，实际 %+v", list)
	}
}
```

注意：`testDB(t)` 是本任务 Step 3 新增的 helper（见下），若 `internal/repo/` 已有等价 helper（查 `db_test.go`），复用之并改测试调用名。

- [ ] **Step 2: 跑测试确认失败**

Run: `make test-integration 2>&1 | grep -A5 TestCandidate || go test ./internal/repo/ -run TestCandidate`
Expected: FAIL / 编译错误 `undefined: SpeakerNameCandidateRepo`。

- [ ] **Step 3: 实现 repo**

先确认 `internal/repo/db_test.go` 是否已有测试 DB helper（`TestDSN` 之外）。若无，在 `internal/repo/speaker_name_candidate_test.go` 顶部补：

```go
// testDB 返回测试库连接（无 TEST_MYSQL_DSN 时 skip）。
func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := NewDB(TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	return db
}
```

（需 import `"github.com/jmoiron/sqlx"`；若已有 helper 则直接用现有名。）

`internal/repo/speaker_name_candidate.go`：

```go
package repo

import (
	"context"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// SpeakerNameCandidate 说话人名字候选：speakername stage 用 LLM 从对话上下文
// 推断的称呼建议（名称+置信度+证据）。仅作建议不改 speaker.name；
// 用户采纳（改名）后 DeleteBySpeaker 清空整组。
type SpeakerNameCandidate struct {
	ID              ids.ID    `db:"id" json:"id"`
	SpeakerID       ids.ID    `db:"speaker_id" json:"speaker_id"`
	Name            string    `db:"name" json:"name"`
	Confidence      float64   `db:"confidence" json:"confidence"`
	Evidence        string    `db:"evidence" json:"evidence"`
	SourceSessionID *ids.ID   `db:"source_session_id" json:"source_session_id,omitempty"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
}

type SpeakerNameCandidateRepo struct{ DB *sqlx.DB }

// Upsert 插入或更新候选（跨 session 累积）：命中 (speaker_id,name) 唯一键时
// 置信度取两者最高（多段录音复现=更强信号，不因低质量录音被拉低）、
// 证据与来源会话取最新。ON DUPLICATE KEY 单行原子写，并发安全。
// sourceSessionID 传 0 时存 NULL。
func (r *SpeakerNameCandidateRepo) Upsert(ctx context.Context, speakerID ids.ID, name string, confidence float64, evidence string, sourceSessionID ids.ID) error {
	var src interface{}
	if sourceSessionID != 0 {
		src = sourceSessionID.Int64()
	}
	_, err := r.DB.ExecContext(ctx, `
INSERT INTO speaker_name_candidate (id, speaker_id, name, confidence, evidence, source_session_id)
VALUES (?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  confidence = GREATEST(confidence, VALUES(confidence)),
  evidence = VALUES(evidence),
  source_session_id = VALUES(source_session_id)`,
		ids.New().Int64(), speakerID.Int64(), name, confidence, evidence, src)
	return err
}

// ListBySpeakers 批量取若干说话人的全部候选，按置信度倒序（次键 id 正序保稳定）。
// 说话人面板/名册富化用：一次查询避免逐 speaker N+1。speakerIDs 为空返回空。
func (r *SpeakerNameCandidateRepo) ListBySpeakers(ctx context.Context, speakerIDs []ids.ID) ([]SpeakerNameCandidate, error) {
	if len(speakerIDs) == 0 {
		return nil, nil
	}
	int64s := make([]int64, len(speakerIDs))
	for i, id := range speakerIDs {
		int64s[i] = id.Int64()
	}
	q, args, err := sqlx.In(`
SELECT id, speaker_id, name, confidence, COALESCE(evidence, '') AS evidence,
       source_session_id, created_at, updated_at
FROM speaker_name_candidate
WHERE speaker_id IN (?)
ORDER BY confidence DESC, id ASC`, int64s)
	if err != nil {
		return nil, err
	}
	var list []SpeakerNameCandidate
	err = r.DB.SelectContext(ctx, &list, q, args...)
	return list, err
}

// DeleteBySpeaker 清空某说话人全部候选（用户采纳候选改名后调用：
// 名字已确认、不再是随机名，后续也不再重跑推断）。
func (r *SpeakerNameCandidateRepo) DeleteBySpeaker(ctx context.Context, speakerID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM speaker_name_candidate WHERE speaker_id = ?`, speakerID.Int64())
	return err
}

// DeleteOne 删除单条候选（前端「忽略」按钮）。幂等：不存在也不报错。
func (r *SpeakerNameCandidateRepo) DeleteOne(ctx context.Context, speakerID ids.ID, name string) error {
	_, err := r.DB.ExecContext(ctx,
		`DELETE FROM speaker_name_candidate WHERE speaker_id = ? AND name = ?`,
		speakerID.Int64(), name)
	return err
}
```

（import 需补 `"time"`。）

- [ ] **Step 4: 跑测试确认通过**

Run: `make test-integration 2>&1 | tail -5`（或 `go test ./internal/repo/ -run TestCandidate -v`，需 `TEST_MYSQL_DSN` 指向测试库）
Expected: 三个 TestCandidate* 全 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/speaker_name_candidate.go internal/repo/speaker_name_candidate_test.go
git commit -m "feat(repo): speaker_name_candidate DAO（upsert 累积/批量读/清空/删单条）"
```

---

### Task 3: TranscriptRepo.ListSegmentsInWallClockWindow

**Files:**
- Modify: `internal/repo/transcript.go`（文件末尾追加）
- Modify: `internal/repo/transcript_test.go`（文件末尾追加测试）

- [ ] **Step 1: 写失败测试**

在 `internal/repo/transcript_test.go` 末尾追加：

```go
// TestListSegmentsInWallClockWindow 验证跨 session 墙钟窗口查询：
// 段的墙钟时间 = session.created_at + start_ms；窗口外的 session 不入选；
// DESC+LIMIT 裁剪保留最近；结果按墙钟正序返回。
func TestListSegmentsInWallClockWindow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sessions := &SessionRepo{DB: db}
	transcripts := &TranscriptRepo{DB: db}

	// session A：窗口外（30 分钟前创建）——即便文本命中也不该入选
	sa := &AudioSession{Source: "web_upload", Filename: "old.wav", StoragePath: "/tmp/old.wav", Status: "completed"}
	sa.CreatedAt = time.Now().Add(-30 * time.Minute)
	if err := sessions.Create(ctx, sa); err != nil {
		t.Fatal(err)
	}
	// 手动改 created_at（Create 用的 DB 默认值）：直接 UPDATE 保证窗口判定用目标时间
	if _, err := db.ExecContext(ctx, `UPDATE audio_session SET created_at = ? WHERE id = ?`, sa.CreatedAt, sa.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	tra := &Transcript{SessionID: sa.ID, Language: "zh-CN"}
	_ = transcripts.Create(ctx, tra)
	_ = transcripts.InsertSegments(ctx, []TranscriptSegment{
		{TranscriptID: tra.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "老录音内容", StartMS: 0, EndMS: 1000},
	})

	// session B：紧邻当前录音之前 5 分钟创建（窗口内）
	sb := &AudioSession{Source: "web_upload", Filename: "prev.wav", StoragePath: "/tmp/prev.wav", Status: "completed"}
	sb.CreatedAt = time.Now().Add(-5 * time.Minute)
	if err := sessions.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE audio_session SET created_at = ? WHERE id = ?`, sb.CreatedAt, sb.ID.Int64()); err != nil {
		t.Fatal(err)
	}
	trb := &Transcript{SessionID: sb.ID, Language: "zh-CN"}
	_ = transcripts.Create(ctx, trb)
	_ = transcripts.InsertSegments(ctx, []TranscriptSegment{
		{TranscriptID: trb.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "前一段录音", StartMS: 0, EndMS: 2000},
	})

	// session C：当前录音（now 创建）
	sc := &AudioSession{Source: "web_upload", Filename: "cur.wav", StoragePath: "/tmp/cur.wav", Status: "processing"}
	if err := sessions.Create(ctx, sc); err != nil {
		t.Fatal(err)
	}
	trc := &Transcript{SessionID: sc.ID, Language: "zh-CN"}
	_ = transcripts.Create(ctx, trc)
	_ = transcripts.InsertSegments(ctx, []TranscriptSegment{
		{TranscriptID: trc.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "当前录音开头", StartMS: 0, EndMS: 1000},
		{TranscriptID: trc.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "当前录音后段", StartMS: 60000, EndMS: 61000},
	})

	// 窗口 = [now-10min, now+2min]（当前录音时长按 70s 计）
	got, err := transcripts.ListSegmentsInWallClockWindow(ctx, 1,
		time.Now().Add(-10*time.Minute), time.Now().Add(2*time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("窗口内应 3 段（B 的 1 段 + C 的 2 段），实际 %d: %+v", len(got), got)
	}
	// 正序：B 的段在 C 之前；C 内按 start_ms
	if got[0].Text != "前一段录音" || got[1].Text != "当前录音开头" || got[2].Text != "当前录音后段" {
		t.Fatalf("顺序错误: %s / %s / %s", got[0].Text, got[1].Text, got[2].Text)
	}
	// 墙钟时间正确性：C 第 2 段 = sc.created_at + 60s
	want := sc.CreatedAt.Add(60 * time.Second)
	if diff := got[2].WallTime.Sub(want); diff < -time.Second || diff > time.Second {
		t.Fatalf("wall_time 应≈ created_at+60s，差 %v", diff)
	}
	// LIMIT 裁剪保留最近：上限 2 → C 的两段（最新）
	got2, _ := transcripts.ListSegmentsInWallClockWindow(ctx, 1,
		time.Now().Add(-10*time.Minute), time.Now().Add(2*time.Minute), 2)
	if len(got2) != 2 || got2[0].Text != "当前录音开头" {
		t.Fatalf("裁剪应保留最近的 2 段，实际 %+v", got2)
	}
}
```

（若 `AudioSession.Create` 不支持预设 `CreatedAt`——看 `session.go` 的 Insert 列——上面已用 UPDATE 兜底，两种都写上不冲突。import 需补 `"time"`，若测试文件已有则不重复。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/repo/ -run TestListSegmentsInWallClockWindow -v`（需 `TEST_MYSQL_DSN`）
Expected: 编译错误 `undefined: TranscriptRepo.ListSegmentsInWallClockWindow`。

- [ ] **Step 3: 实现**

在 `internal/repo/transcript.go` 末尾追加：

```go
// WallClockSegment 跨 session 墙钟时间窗口内的一条发言（speakername stage 上下文用）。
// WallTime = session.created_at + start_ms，由 SQL 计算返回。
// SpeakerName 经 LEFT JOIN speaker 取（已确认真名/随机名原样；NULL 段为 nil）。
type WallClockSegment struct {
	SegmentID   ids.ID    `db:"segment_id"`
	SessionID   ids.ID    `db:"session_id"`
	SpeakerID   *ids.ID   `db:"speaker_id"`
	SpeakerName *string   `db:"speaker_name"`
	Text        string    `db:"text"`
	StartMS     int64     `db:"start_ms"`
	EndMS       int64     `db:"end_ms"`
	WallTime    time.Time `db:"wall_time"`
}

// ListSegmentsInWallClockWindow 跨 session 取墙钟时间落在 [from,to] 的全部段，
// 按墙钟**正序**返回；limit 超限时保留**最近**的（靠近 to 的）——当前录音的段
// 是窗口内最新的，天然优先保留。user 维度过滤。
// 实现：SQL DESC + LIMIT 取最近 N，Go 侧反转回正序。
// speakername stage 用它拼「当前录音全文 + 前 N 分钟跨录音对话」上下文。
func (r *TranscriptRepo) ListSegmentsInWallClockWindow(ctx context.Context, userID int64, from, to time.Time, limit int) ([]WallClockSegment, error) {
	if limit <= 0 {
		limit = 400
	}
	var desc []WallClockSegment
	err := r.DB.SelectContext(ctx, &desc, `
SELECT seg.id AS segment_id, tr.session_id, seg.speaker_id, sp.name AS speaker_name,
       seg.text, seg.start_ms, seg.end_ms,
       (s.created_at + INTERVAL seg.start_ms * 1000 MICROSECOND) AS wall_time
FROM transcript_segment seg
JOIN transcript tr      ON tr.id = seg.transcript_id
JOIN audio_session s    ON s.id = tr.session_id
LEFT JOIN speaker sp    ON sp.id = seg.speaker_id
WHERE tr.user_id = ?
  AND (s.created_at + INTERVAL seg.start_ms * 1000 MICROSECOND) BETWEEN ? AND ?
ORDER BY wall_time DESC
LIMIT ?`, userID, from, to, limit)
	if err != nil {
		return nil, err
	}
	// DESC → 正序（原地反转，避免再分配）
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	return desc, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/repo/ -run TestListSegmentsInWallClockWindow -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/repo/transcript.go internal/repo/transcript_test.go
git commit -m "feat(repo): 跨 session 墙钟窗口段查询（speakername 上下文用）"
```

---

### Task 4: prompt 文件 + isAutoName + ParseNameCandidates（纯函数）

**Files:**
- Create: `prompts/speaker_naming_v1.md`
- Create: `internal/pipeline/stage_speaker_name.go`
- Create: `internal/pipeline/stage_speaker_name_test.go`

- [ ] **Step 1: 写 prompt 文件**

`prompts/speaker_naming_v1.md`：

```markdown
# 说话人名字推断 prompt（版本：speaker_naming_v1）

你是个人 AI 记忆助手「知微」的说话人名字推断器。输入是一段时间窗口内的对话转写，每条发言带说话人标注（真名、「待识别人物A/B…」占位符、或「未知」）。你的任务：为每个「待识别人物X」推断真实称呼，输出带置信度的候选名。

## 判定规则（核心，只允许两种情形记名字）

1. 被当面称呼：其他人在**对其说话的话轮**里用了称呼语（常在句首，如「张总，您看这个方案」「李哥，帮我看下」）→ 名字记给**被称呼方**（待识别人物本人），不是说话方。
2. 自我介绍：待识别人物自述「我是李明」「我姓王，叫我王工就行」→ 记给**说话方本人**。

## 禁止：第三人误判

只作为**谈论对象**出现、并未参与对话轮次的名字（两人在谈论一个缺席的第三人，如「昨天王总来找我谈了下」——王总没说话也没被称呼），**不得**当作任何待识别人物的候选。
判定信号：称呼语出现在**指向某在场说话人的话轮**里（后接问候/请求/提问、对方随后应答）vs 出现在**描述缺席者的叙述内容**里。
多人场景中无法确定称呼指向哪位在场者时，降低置信度。拿不准是在场者还是第三人时，宁可不给。

## 置信度

- 0.8 以上：当面直呼且指向明确（应答关系清晰）
- 0.4~0.7：仅姓氏/称谓（如「王哥」）、或指向略有歧义
- 0.4 以下：间接推断（如同段提到姓氏+职务）
无可靠候选时给空数组，不要编造。

## 输出格式

只输出 JSON，不要任何其他文字和代码围栏。无任何待识别人物时输出 {"speakers":[]}。

{"speakers":[
  {"ref":"待识别人物A","candidates":[
    {"name":"张总","confidence":0.82,"evidence":"对方在 15:03:12 说『张总，您看这个方案』"}
  ]},
  {"ref":"待识别人物B","candidates":[]}
]}

- ref 必须原样使用输入里给出的占位符（待识别人物A/B…）
- candidates 按 confidence 从高到低排列
- evidence 是简短依据：引用原话片段 + 出现时间点（用输入里标注的时间），不超过 60 字
```

- [ ] **Step 2: 写失败测试（纯函数，无需 DB）**

`internal/pipeline/stage_speaker_name_test.go`：

```go
// stage_speaker_name_test 验证 speakername stage：纯函数（isAutoName /
// ParseNameCandidates）单测无需 DB；runSpeakerNameStage 集成测试见下（需 TEST_MYSQL_DSN）。
package pipeline

import (
	"testing"
)

func TestIsAutoName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"说话人ab3x9", true},   // 自动登记随机名（rand5 产物）
		{"说话人zzzzz", true},   // 全字母也命中
		{"张三", false},         // 已确认真名
		{"说话人 1", false},     // 显示回退（带空格），从不落库，不该命中
		{"说话人ab3x", false},   // 4 位，非 rand5 形态
		{"说话人AB3X9", false},  // 大写，rand5 只产小写
		{"说话人ab3x9额外", false}, // 后缀多余
	}
	for _, c := range cases {
		if got := isAutoName(c.name); got != c.want {
			t.Errorf("isAutoName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseNameCandidates(t *testing.T) {
	// 正常：含围栏废话、多候选乱序 → 剥壳解析 + 按置信度倒排
	raw := "好的，以下是结果：\n```json\n{\"speakers\":[{\"ref\":\"待识别人物A\",\"candidates\":[{\"name\":\"张明\",\"confidence\":0.4,\"evidence\":\"自称我姓张\"},{\"name\":\"张总\",\"confidence\":0.82,\"evidence\":\"对方称呼张总\"}]},{\"ref\":\"待识别人物B\",\"candidates\":[]}]}\n```"
	got, err := ParseNameCandidates(raw)
	if err != nil {
		t.Fatal(err)
	}
	cands := got["待识别人物A"]
	if len(cands) != 2 {
		t.Fatalf("A 应 2 候选，实际 %d", len(cands))
	}
	if cands[0].Name != "张总" || cands[0].Confidence != 0.82 {
		t.Fatalf("倒序首位应为 张总/0.82，实际 %s/%.2f", cands[0].Name, cands[0].Confidence)
	}
	if len(got["待识别人物B"]) != 0 {
		t.Fatalf("B 应 0 候选")
	}

	// 置信度越界 clamp 到 [0,1]；空名候选丢弃
	got2, err := ParseNameCandidates(`{"speakers":[{"ref":"X","candidates":[{"name":"a","confidence":1.5},{"name":"","confidence":0.9},{"name":"b","confidence":-0.1}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	c2 := got2["X"]
	if len(c2) != 2 {
		t.Fatalf("空名丢弃后应 2 条，实际 %d", len(c2))
	}
	if c2[0].Confidence != 1 || c2[1].Confidence != 0 {
		t.Fatalf("clamp 失败: %.2f %.2f", c2[0].Confidence, c2[1].Confidence)
	}

	// 彻底非法 JSON → error（stage 走重试）
	if _, err := ParseNameCandidates(`完全不是 JSON`); err == nil {
		t.Fatal("非法 JSON 应返回 error")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/pipeline/ -run 'TestIsAutoName|TestParseNameCandidates' -v`
Expected: 编译错误 `undefined: isAutoName`。

- [ ] **Step 4: 实现纯函数部分**

`internal/pipeline/stage_speaker_name.go`（本任务只写常量/类型/纯函数，stage 主体 Task 5 加）：

```go
// stage_speaker_name 实现 speakername stage：对「名字仍是自动随机名」的说话人，
// 用 LLM 从跨录音墙钟窗口（当前录音全文 + 前 N 分钟）的对话转写推断真实称呼，
// 候选（名称+置信度+证据）写入 speaker_name_candidate。仅作建议，不改名。
// 设计见 docs/superpowers/specs/2026-08-24-speaker-name-inference-design.md。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// autoNamePattern 自动登记的默认名：说话人 + 5 位 [a-z0-9]（stage_speaker.go rand5 产物）。
// 用户改名后不再命中 → 不再重复推断（比 source=auto 判定准：source 不随改名变）。
// 注意与显示回退「说话人 N」（带空格）区分——那个从不落库为 speaker.name。
var autoNamePattern = regexp.MustCompile(`^说话人[a-z0-9]{5}$`)

// isAutoName 判断说话人名是否仍是自动登记的随机名（= 待识别）。
func isAutoName(name string) bool { return autoNamePattern.MatchString(name) }

// nameCandidate LLM 输出的一条候选名。
type nameCandidate struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
}

// nameInferResult LLM 输出整体：每个待识别人物（ref=占位符）一组候选。
type nameInferResult struct {
	Speakers []struct {
		Ref        string          `json:"ref"`
		Candidates []nameCandidate `json:"candidates"`
	} `json:"speakers"`
}

// ParseNameCandidates 解析 LLM 输出为 ref→候选列表（纯函数，可单测）。
// 容错同 memory.ParseCandidates：截取首 { 到末 }，剥掉围栏与前后废话；
// 彻底非法 JSON 返回 error（由 stage 走重试）。
// 候选内清洗：空名丢弃、置信度 clamp 到 [0,1]、按置信度倒排。
func ParseNameCandidates(raw string) (map[string][]nameCandidate, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out nameInferResult
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("名字推断结果解析失败: %w", err)
	}
	m := make(map[string][]nameCandidate, len(out.Speakers))
	for _, sp := range out.Speakers {
		cands := make([]nameCandidate, 0, len(sp.Candidates))
		for _, c := range sp.Candidates {
			if strings.TrimSpace(c.Name) == "" {
				continue // 无名候选丢弃
			}
			c.Name = strings.TrimSpace(c.Name)
			c.Confidence = clampConfidence(c.Confidence)
			cands = append(cands, c)
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].Confidence > cands[j].Confidence })
		m[sp.Ref] = cands
	}
	return m, nil
}

// clampConfidence 置信度越界归位 [0,1]。
func clampConfidence(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
```

（`context`/`ids`/`provider`/`repo`/`time` 的 import 在 Task 5 使用时已存在，此时 Go 会报 unused import——**临时保留会编译失败**。处理：本步骤先注释掉未用 import，Task 5 再放开；或直接把 Task 5 Step 1 的 stage 主体与本任务合并执行后再跑测试。**推荐做法**：本任务先只写 `regexp`/`encoding/json`/`fmt`/`sort`/`strings` 五个 import，Task 5 追加代码时补齐其余。）

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/pipeline/ -run 'TestIsAutoName|TestParseNameCandidates' -v`
Expected: 两个测试 PASS。

- [ ] **Step 6: Commit**

```bash
git add prompts/speaker_naming_v1.md internal/pipeline/stage_speaker_name.go internal/pipeline/stage_speaker_name_test.go
git commit -m "feat(speakername): 名字推断 prompt + isAutoName/ParseNameCandidates 纯函数"
```

---

### Task 5: runSpeakerNameStage 主体 + 装配进 BuildStages

**Files:**
- Modify: `internal/pipeline/stage_speaker_name.go`（追加 stage 主体）
- Modify: `internal/pipeline/stage_asr.go`（StageDeps 加字段；BuildStages 加 speakername）
- Modify: `internal/pipeline/stage_speaker_name_test.go`（追加集成测试）

- [ ] **Step 1: 写失败测试（集成，需 TEST_MYSQL_DSN；无则 skip）**

在 `internal/pipeline/stage_speaker_name_test.go` 追加（import 补 `"context"`, `"fmt"`, `"zhiwei/internal/ids"`, `"zhiwei/internal/provider"`, `"zhiwei/internal/repo"`, `"time"`）：

```go
// fakeNameLLM 可配置响应的 LLM fake（记录调用次数，验证「无待识别不调 LLM」）。
type fakeNameLLM struct {
	calls int
	resp  string
}

func (f *fakeNameLLM) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	f.calls++
	return provider.ChatResponse{Content: f.resp, TotalTokens: 100}, nil
}

// seedNameStage 建 session + transcript + 2 段 + 2 个 speaker（一个随机名/一个真名），
// 段通过 SetSegmentSpeaker 按 label 回填 speaker_id（InsertSegments 不写 speaker_id 列）。
// 返回 (deps 可复用的 repos, sid, tr, randSp 随机名 speaker, namedSp 真名 speaker)。
func seedNameStage(t *testing.T, randName, namedName string) (*repo.SessionRepo, *repo.TranscriptRepo, *repo.SpeakerRepo, *repo.SpeakerNameCandidateRepo, ids.ID, *repo.Transcript, *repo.Speaker, *repo.Speaker) {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}
	candidates := &repo.SpeakerNameCandidateRepo{DB: db}

	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "talk.wav",
		StoragePath: "/tmp/talk.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "张总，您看这个方案怎么样", StartMS: 0, EndMS: 3000},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "我觉得可以，按这个来", StartMS: 3200, EndMS: 6000},
	}); err != nil {
		t.Fatal(err)
	}
	randSp := &repo.Speaker{Name: randName, Source: "auto"}
	if err := speakers.Create(ctx, randSp); err != nil {
		t.Fatal(err)
	}
	namedSp := &repo.Speaker{Name: namedName, Source: "enrolled"}
	if err := speakers.Create(ctx, namedSp); err != nil {
		t.Fatal(err)
	}
	// label 1 → 随机名说话人（待识别）；label 2 → 真名说话人
	_ = transcripts.SetSegmentSpeaker(ctx, tr.ID, "1", randSp.ID)
	_ = transcripts.SetSegmentSpeaker(ctx, tr.ID, "2", namedSp.ID)
	return sessions, transcripts, speakers, candidates, sid, tr, randSp, namedSp
}

// newNameDeps 组 StageDeps（只填 speakername 用到的字段）。
func newNameDeps(sessions *repo.SessionRepo, transcripts *repo.TranscriptRepo,
	speakers *repo.SpeakerRepo, candidates *repo.SpeakerNameCandidateRepo, llm provider.LLMProvider) StageDeps {
	return StageDeps{
		Sessions: sessions, Transcripts: transcripts, Speakers: speakers,
		SpeakerNameCandidates: candidates, LLM: llm, LLMModel: "fake-model",
		NameInferPrompt: "测试 prompt", NameInferWindowMin: 10, NameInferMaxSegments: 400,
	}
}

func TestStageSpeakerNameInfersAndUpserts(t *testing.T) {
	_, transcripts, speakers, candidates, sid, tr, randSp, _ := seedNameStage(t, "说话人ab3x9", "李明")
	llm := &fakeNameLLM{resp: `{"speakers":[{"ref":"待识别人物A","candidates":[
		{"name":"张总","confidence":0.82,"evidence":"对方说『张总，您看这个方案』"},
		{"name":"张明","confidence":0.4,"evidence":"上下文推断"}]}]}`}
	d := newNameDeps(nil, transcripts, speakers, candidates, llm)
	// Sessions 需要非 nil（stage 读 session 拿 created_at）
	db := transcripts.DB
	d.Sessions = &repo.SessionRepo{DB: db}
	if err := runSpeakerNameStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("应恰好 1 次 LLM 调用（批处理），实际 %d", llm.calls)
	}
	list, _ := candidates.ListBySpeakers(context.Background(), []ids.ID{randSp.ID})
	if len(list) != 2 || list[0].Name != "张总" || list[0].Confidence != 0.82 {
		t.Fatalf("候选应 2 条且倒序（张总 0.82 在首），实际 %+v", list)
	}
	if list[0].SpeakerID != randSp.ID {
		t.Fatalf("候选应归属随机名说话人 %s，实际 %s", randSp.ID, list[0].SpeakerID)
	}
	// 幂等：重跑（置信度更低）不增行、置信度不降
	llm2 := &fakeNameLLM{resp: `{"speakers":[{"ref":"待识别人物A","candidates":[
		{"name":"张总","confidence":0.5,"evidence":"重跑证据"}]}]}`}
	d2 := newNameDeps(nil, transcripts, speakers, candidates, llm2)
	d2.Sessions = &repo.SessionRepo{DB: db}
	if err := runSpeakerNameStage(context.Background(), d2, sid, tr); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	list, _ = candidates.ListBySpeakers(context.Background(), []ids.ID{randSp.ID})
	if len(list) != 2 || list[0].Confidence != 0.82 {
		t.Fatalf("重跑后应仍 2 条、置信度保留 0.82，实际 %+v", list)
	}
}

func TestStageSpeakerNameNoopWithoutPending(t *testing.T) {
	// 全部说话人已确认真名 → 不调 LLM、不写候选
	_, transcripts, speakers, candidates, sid, tr, randSp, namedSp := seedNameStage(t, "已改名的人", "李明")
	// 把随机名 speaker 改名（模拟用户已认领）——直接 UpdateName
	_ = speakers.UpdateName(context.Background(), randSp.ID, "王五")
	llm := &fakeNameLLM{resp: `{"speakers":[]}`}
	d := newNameDeps(&repo.SessionRepo{DB: transcripts.DB}, transcripts, speakers, candidates, llm)
	if err := runSpeakerNameStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("无待识别说话人不应调 LLM，实际 %d 次", llm.calls)
	}
	list, _ := candidates.ListBySpeakers(context.Background(), []ids.ID{randSp.ID, namedSp.ID})
	if len(list) != 0 {
		t.Fatalf("不应产生候选，实际 %d 条", len(list))
	}
}

func TestStageSpeakerNameIgnoresUnknownRef(t *testing.T) {
	// LLM 返回未分配的 ref（编造占位符）→ 忽略不落库
	_, transcripts, speakers, candidates, sid, tr, randSp, _ := seedNameStage(t, "说话人ab3x9", "李明")
	llm := &fakeNameLLM{resp: `{"speakers":[{"ref":"待识别人物Z","candidates":[
		{"name":"编造","confidence":0.9,"evidence":""}]}]}`}
	d := newNameDeps(&repo.SessionRepo{DB: transcripts.DB}, transcripts, speakers, candidates, llm)
	if err := runSpeakerNameStage(context.Background(), d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	list, _ := candidates.ListBySpeakers(context.Background(), []ids.ID{randSp.ID})
	if len(list) != 0 {
		t.Fatalf("未知 ref 的候选应忽略，实际 %d 条", len(list))
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/pipeline/ -run TestStageSpeakerName -v`
Expected: 编译错误（`runSpeakerNameStage` undefined、StageDeps 无 `NameInferPrompt` 等字段）。

- [ ] **Step 3: 实现 stage 主体**

`internal/pipeline/stage_speaker_name.go` 追加（放开 Task 4 注释掉的 import：`context`、`ids`、`provider`、`repo`、`time`）：

```go
// runSpeakerNameStage 是 speakername stage 的可测核心（避开 pool），由 stageSpeakerName 包装。
//
// 流程：本 session 段解析到的说话人中筛「名字仍是随机名」= 待识别 T
// → 取跨录音墙钟窗口上下文（当前录音全文 + 前 W 分钟）→ 待识别者分配占位符
// → 单次 LLM 调用（批处理：T 共享同一上下文，模型可跨说话人联合推理）
// → ref 映射回 speaker_id，逐候选 upsert（GREATEST 累积置信度，幂等）。
// LLM/候选 repo 未装配时 no-op（兼容旧装配/纯 ASR 测试）。
func runSpeakerNameStage(ctx context.Context, d StageDeps, sessionID ids.ID, tr *repo.Transcript) error {
	if d.LLM == nil || d.SpeakerNameCandidates == nil {
		return nil // 依赖未装配（测试/降级）→ no-op
	}
	s, err := d.Sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("读 session: %w", err)
	}
	segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return fmt.Errorf("读 segments: %w", err)
	}
	if len(segs) == 0 {
		return nil
	}
	// 1) 待识别集合 T：本 session 段解析到的说话人中名字仍是随机名者。
	//    按 session 内首次出现顺序分配占位符（A/B…，说话人数 ≤26 足够）。
	speakerList, err := d.Speakers.List(ctx)
	if err != nil {
		return fmt.Errorf("读 speakers: %w", err)
	}
	nameByID := make(map[ids.ID]string, len(speakerList))
	for _, sp := range speakerList {
		nameByID[sp.ID] = sp.Name
	}
	pending := map[ids.ID]bool{}
	var order []ids.ID
	var durationMS int64 // 本录音时长 = 段最大 end_ms（上下文窗口上界偏移）
	for _, sg := range segs {
		if sg.EndMS > durationMS {
			durationMS = sg.EndMS
		}
		if sg.SpeakerID == nil || pending[*sg.SpeakerID] {
			continue
		}
		if isAutoName(nameByID[*sg.SpeakerID]) {
			pending[*sg.SpeakerID] = true
			order = append(order, *sg.SpeakerID)
		}
	}
	if len(pending) == 0 {
		return nil // 无待识别 → 不调 LLM（省 token）
	}
	refOf := make(map[ids.ID]string, len(order))
	refToID := make(map[string]ids.ID, len(order))
	for i, spID := range order {
		ref := fmt.Sprintf("待识别人物%c", 'A'+i)
		refOf[spID] = ref
		refToID[ref] = spID
	}

	// 2) 上下文：墙钟窗口 [S.created_at − W, S.created_at + 本录音时长]。
	//    DESC+LIMIT 裁剪保留最近——本 session 段是窗口内最新的，天然优先保留。
	windowMin := d.NameInferWindowMin
	if windowMin <= 0 {
		windowMin = 10
	}
	maxSegs := d.NameInferMaxSegments
	if maxSegs <= 0 {
		maxSegs = 400
	}
	from := s.CreatedAt.Add(-time.Duration(windowMin) * time.Minute)
	to := s.CreatedAt.Add(time.Duration(durationMS) * time.Millisecond)
	ctxSegs, err := d.Transcripts.ListSegmentsInWallClockWindow(ctx, s.UserID, from, to, maxSegs)
	if err != nil {
		return fmt.Errorf("读上下文段: %w", err)
	}

	// 3) 组 user message：待识别清单 + 对话（时间|说话人 token|文本）。
	//    token 稳定可指认：待识别→占位符；已确认→真名；随机名非本 session 者按原随机名
	//    （区别于占位符，模型不会为其产候选——prompt 只认清单内的 ref）；未解析→未知。
	var sb strings.Builder
	sb.WriteString("待识别人物清单（只为清单内的占位符推断名字）：\n")
	for _, spID := range order {
		fmt.Fprintf(&sb, "- %s\n", refOf[spID])
	}
	sb.WriteString("\n对话转写（格式：时间|说话人|文本，按时间正序）：\n")
	for _, cs := range ctxSegs {
		token := "未知"
		if cs.SpeakerID != nil {
			if ref, ok := refOf[*cs.SpeakerID]; ok {
				token = ref
			} else if cs.SpeakerName != nil && *cs.SpeakerName != "" {
				token = *cs.SpeakerName // 已确认真名 或 非本 session 的随机名（原样区分）
			}
		}
		fmt.Fprintf(&sb, "%s|%s|%s\n", cs.WallTime.Format("15:04:05"), token, cs.Text)
	}

	// 4) 单次 LLM 调用（批处理）+ 解析
	resp, err := d.LLM.Chat(ctx, provider.ChatRequest{
		Model:  d.LLMModel,
		System: d.NameInferPrompt,
		User:   sb.String(),
	})
	if err != nil {
		return fmt.Errorf("LLM 调用: %w", err)
	}
	parsed, err := ParseNameCandidates(resp.Content)
	if err != nil {
		return fmt.Errorf("解析名字候选: %w", err)
	}
	// 5) ref → speaker_id 回填候选；未知 ref（模型编造的占位符）忽略
	for ref, cands := range parsed {
		spID, ok := refToID[ref]
		if !ok {
			continue
		}
		for _, c := range cands {
			if err := d.SpeakerNameCandidates.Upsert(ctx, spID, c.Name, c.Confidence, c.Evidence, sessionID); err != nil {
				return fmt.Errorf("写候选 %q: %w", c.Name, err)
			}
		}
	}
	return nil
}

// stageSpeakerName 是 pool 用的 Handler 包装。
func stageSpeakerName(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读 transcript: %w", err)
		}
		return runSpeakerNameStage(ctx, d, sessionID, tr)
	}
}
```

- [ ] **Step 4: StageDeps 加字段 + BuildStages 注册**

`internal/pipeline/stage_asr.go` 的 `StageDeps` 结构体，在 `// ---- speaker stage ----` 字段块后追加：

```go
	// ---- speakername stage（名字推断）----
	NameInferPrompt       string                         // prompts/speaker_naming_v1.md 内容（system prompt）
	SpeakerNameCandidates *repo.SpeakerNameCandidateRepo // 候选名存取（nil = no-op，兼容旧装配）
	NameInferWindowMin    int                            // 上下文回看窗口（分钟），0 = 默认 10
	NameInferMaxSegments  int                            // 上下文段数上限，0 = 默认 400
```

`BuildStages`（同文件）改为：

```go
func BuildStages(d StageDeps) map[string]Handler {
	return map[string]Handler{
		"asr":         stageASR(d),
		"segment":     stageSegment(d),
		"speaker":     stageSpeaker(d),
		"speakername": stageSpeakerName(d),
		"extract":     stageExtract(d),
	}
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/pipeline/ -run 'TestStageSpeakerName|TestIsAutoName|TestParseNameCandidates' -v`（需 `TEST_MYSQL_DSN`；stage 测试无 DSN 时 skip 属正常）
Expected: 全 PASS（含既有 speaker/extract 测试不回归：`go build ./...` 后跑 `make test`）。

- [ ] **Step 6: Commit**

```bash
git add internal/pipeline/stage_speaker_name.go internal/pipeline/stage_speaker_name_test.go internal/pipeline/stage_asr.go
git commit -m "feat(speakername): runSpeakerNameStage 批处理推断 + BuildStages 注册"
```

---

### Task 6: config + main.go 装配（Flow 加 speakername）

**Files:**
- Modify: `internal/config/config.go`
- Modify: `cmd/zhiwei-server/main.go`

- [ ] **Step 1: config 加两项**

`internal/config/config.go` 的 `Config` 结构体，`EnrollMinDurationMS` 字段后追加：

```go
	NameInferWindowMin   int // 名字推断上下文回看窗口（分钟，ZW_NAME_INFER_WINDOW_MIN，默认 10）
	NameInferMaxSegments int // 名字推断上下文段数上限（ZW_NAME_INFER_MAX_SEGMENTS，默认 400）
```

`Load()` 返回值里，`EnrollMinDurationMS: ...` 行后追加：

```go
		NameInferWindowMin:   getenvInt("ZW_NAME_INFER_WINDOW_MIN", 10),
		NameInferMaxSegments: getenvInt("ZW_NAME_INFER_MAX_SEGMENTS", 400),
```

- [ ] **Step 2: main.go 装配**

`cmd/zhiwei-server/main.go`：

1. 顶部 `promptPath` 常量后加：

```go
// nameInferPromptPath 说话人名字推断 prompt（speakername stage 用，版本号见文件名）。
const nameInferPromptPath = "prompts/speaker_naming_v1.md"
```

2. `memoryConsolidateBytes` 读取块后加：

```go
	// 说话人名字推断 prompt（版本化文件，speakername stage 用）
	nameInferBytes, err := os.ReadFile(nameInferPromptPath)
	if err != nil {
		log.Fatal("读取名字推断 prompt 失败: ", err)
	}
```

3. `speakers := &repo.SpeakerRepo{DB: db}` 行后加：

```go
	nameCandidates := &repo.SpeakerNameCandidateRepo{DB: db}
```

4. `pipeline.BuildStages(pipeline.StageDeps{...})` 的 `VoiceprintThreshold: cfg.VoiceprintThreshold,` 行后加：

```go
		NameInferPrompt:       string(nameInferBytes),
		SpeakerNameCandidates: nameCandidates,
		NameInferWindowMin:    cfg.NameInferWindowMin,
		NameInferMaxSegments:  cfg.NameInferMaxSegments,
```

5. Flow 行改为：

```go
	flow := pipeline.Flow{Stages: []string{"asr", "segment", "speaker", "speakername", "extract"}}
```

6. `api.RegisterSpeaker(r, &api.SpeakerHandler{...})` 的 `VoiceprintThreshold: cfg.VoiceprintThreshold,` 行后加：

```go
		SpeakerNameCandidates: nameCandidates,
```

7. `api.RegisterQuery(r, &api.QueryHandler{...})` 的 `Speakers: speakers,` 行后加：

```go
		SpeakerNameCandidates: nameCandidates,
```

- [ ] **Step 3: 编译验证**

Run: `go build ./...`
Expected: 无输出（成功）。`NameInferWindowMin` 等 4 处 wiring 就位后编译即过（SpeakerNameCandidates 字段 Task 7 才加，**此处会编译失败**——因此本 Task 与 Task 7 的 Step 3 一起跑编译，或本步骤先只做 1/2/3/4/5（pipeline 侧），API 注入（6/7 两处）放到 Task 7。**推荐**：本 Task 只加到 Flow（第 5 点），API 两处注入挪到 Task 7 Step 3 后统一编译）。

- [ ] **Step 4: Commit**

```bash
git add internal/config/config.go cmd/zhiwei-server/main.go
git commit -m "feat(speakername): config + Flow 装配（speaker stage 后插入 speakername）"
```

---

### Task 7: API 富化（候选随 speaker 返回 + 改名清候选 + 忽略端点）

**Files:**
- Modify: `internal/api/speaker.go`
- Modify: `internal/api/query.go`
- Modify: `internal/api/speaker_test.go`（追加测试）

- [ ] **Step 1: 写失败测试**

在 `internal/api/speaker_test.go` 追加（import 补 `"strings"` 若缺）：

```go
// TestSpeakerListWithCandidates 名册接口富化候选名：随机名说话人带 name_candidates
//（倒排 + 置信度数值），已确认真名者带空数组。
func TestSpeakerListWithCandidates(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.Init(1)
	speakers := &repo.SpeakerRepo{DB: db}
	candidates := &repo.SpeakerNameCandidateRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		SpeakerNameCandidates: candidates,
	})
	ctx := context.Background()
	randSp := &repo.Speaker{Name: "说话人ab3x9", Source: "auto"}
	_ = speakers.Create(ctx, randSp)
	namedSp := &repo.Speaker{Name: "张三", Source: "enrolled"}
	_ = speakers.Create(ctx, namedSp)
	_ = candidates.Upsert(ctx, randSp.ID, "张总", 0.82, "对方称呼张总", 1001)
	_ = candidates.Upsert(ctx, randSp.ID, "张明", 0.4, "", 1001)

	req := httptest.NewRequest(http.MethodGet, "/api/speakers", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Speakers []struct {
			Name            string `json:"name"`
			NameCandidates  []struct {
				Name       string  `json:"name"`
				Confidence float64 `json:"confidence"`
			} `json:"name_candidates"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// List 按 id 倒序（近建在前）：namedSp 后建在前
	if len(out.Speakers) != 2 {
		t.Fatalf("应 2 个说话人，实际 %d", len(out.Speakers))
	}
	if out.Speakers[0].Name != "张三" || len(out.Speakers[0].NameCandidates) != 0 {
		t.Fatalf("真名说话人应无候选: %+v", out.Speakers[0])
	}
	cands := out.Speakers[1].NameCandidates
	if len(cands) != 2 || cands[0].Name != "张总" || cands[0].Confidence != 0.82 {
		t.Fatalf("随机名说话人应带倒序候选（张总 0.82 在首），实际 %+v", cands)
	}
}

// TestSpeakerRenameClearsCandidates 改名（=用户采纳候选或手动命名）后清空该说话人候选。
func TestSpeakerRenameClearsCandidates(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.Init(1)
	speakers := &repo.SpeakerRepo{DB: db}
	candidates := &repo.SpeakerNameCandidateRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		SpeakerNameCandidates: candidates,
	})
	ctx := context.Background()
	sp := &repo.Speaker{Name: "说话人ab3x9", Source: "auto"}
	_ = speakers.Create(ctx, sp)
	_ = candidates.Upsert(ctx, sp.ID, "张总", 0.82, "", 1001)
	_ = candidates.Upsert(ctx, sp.ID, "张明", 0.4, "", 1001)

	req := httptest.NewRequest(http.MethodPatch, "/api/speakers/"+sp.ID.String(),
		bytes.NewBufferString(`{"name":"张总"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("rename code %d body %s", rec.Code, rec.Body.String())
	}
	list, _ := candidates.ListBySpeakers(ctx, []ids.ID{sp.ID})
	if len(list) != 0 {
		t.Fatalf("改名后候选应清空，实际 %d 条", len(list))
	}
}

// TestSpeakerDeleteNameCandidate 忽略单个候选端点：删该行、幂等、缺 name 400。
func TestSpeakerDeleteNameCandidate(t *testing.T) {
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = ids.Init(1)
	speakers := &repo.SpeakerRepo{DB: db}
	candidates := &repo.SpeakerNameCandidateRepo{DB: db}
	r := chi.NewRouter()
	RegisterSpeaker(r, &SpeakerHandler{
		Speakers: speakers, Transcripts: &repo.TranscriptRepo{DB: db},
		Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir(),
		SpeakerNameCandidates: candidates,
	})
	ctx := context.Background()
	sp := &repo.Speaker{Name: "说话人ab3x9", Source: "auto"}
	_ = speakers.Create(ctx, sp)
	_ = candidates.Upsert(ctx, sp.ID, "张总", 0.82, "", 1001)

	// 正常忽略
	req := httptest.NewRequest(http.MethodDelete,
		"/api/speakers/"+sp.ID.String()+"/name-candidates?name="+urlQueryEscape("张总"), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	list, _ := candidates.ListBySpeakers(ctx, []ids.ID{sp.ID})
	if len(list) != 0 {
		t.Fatalf("忽略后应无候选，实际 %d", len(list))
	}
	// 幂等：再删一次 204
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req)
	if rec2.Code != 204 {
		t.Fatalf("重复忽略应幂等 204，实际 %d", rec2.Code)
	}
	// 缺 name → 400
	req3 := httptest.NewRequest(http.MethodDelete, "/api/speakers/"+sp.ID.String()+"/name-candidates", nil)
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)
	if rec3.Code != 400 {
		t.Fatalf("缺 name 应 400，实际 %d", rec3.Code)
	}
}
```

（`urlQueryEscape` 用 `net/url.QueryEscape`；直接在调用处写 `url.QueryEscape("张总")`，import `"net/url"`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run 'TestSpeakerListWithCandidates|TestSpeakerRenameClearsCandidates|TestSpeakerDeleteNameCandidate' -v`
Expected: 编译错误 `SpeakerHandler` 无 `SpeakerNameCandidates` 字段。

- [ ] **Step 3: 实现 speaker.go 改动**

`internal/api/speaker.go`：

1. `SpeakerHandler` 结构体加字段：

```go
	SpeakerNameCandidates *repo.SpeakerNameCandidateRepo // 名字候选 repo（nil = 不富化/不清理，兼容旧装配）
```

2. 文件顶部（RegisterSpeaker 之前）加视图类型与富化 helper：

```go
// NameCandidateView 前端展示的候选名：名称 + 置信度数值（硬性要求：用户确认时
// 必须能看到名称和置信度值）+ 依据。倒排。
type NameCandidateView struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence,omitempty"`
}

// speakerWithCandidates speaker + 候选名列表（名册/面板富化视图）。
type speakerWithCandidates struct {
	repo.Speaker
	NameCandidates []NameCandidateView `json:"name_candidates"`
}

// attachCandidates 为说话人列表批量附候选名（一次查询避免 N+1）。
// repo 未装配时返回全空候选；查询失败降级为空候选（富化仅影响建议展示，不阻断列表）。
func (h *SpeakerHandler) attachCandidates(ctx context.Context, list []repo.Speaker) []speakerWithCandidates {
	out := make([]speakerWithCandidates, len(list))
	spIDs := make([]ids.ID, len(list))
	idx := make(map[ids.ID]int, len(list))
	for i, sp := range list {
		out[i] = speakerWithCandidates{Speaker: sp, NameCandidates: []NameCandidateView{}}
		spIDs[i] = sp.ID
		idx[sp.ID] = i
	}
	if h.SpeakerNameCandidates == nil || len(list) == 0 {
		return out
	}
	cands, err := h.SpeakerNameCandidates.ListBySpeakers(ctx, spIDs)
	if err != nil {
		return out // 降级：无候选展示
	}
	for _, c := range cands {
		if i, ok := idx[c.SpeakerID]; ok {
			out[i].NameCandidates = append(out[i].NameCandidates, NameCandidateView{
				Name: c.Name, Confidence: c.Confidence, Evidence: c.Evidence,
			})
		}
	}
	return out
}
```

3. `List` handler 改为：

```go
// List 全部 active 说话人（管理页/换人下拉用）。随机名说话人附 LLM 推断的候选名（倒排）。
func (h *SpeakerHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.Speakers.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"speakers": h.attachCandidates(r.Context(), list)})
}
```

4. `RegisterSpeaker` 加路由（`r.Delete("/api/speakers/{id}", h.Delete)` 行后）：

```go
	r.Delete("/api/speakers/{id}/name-candidates", h.DeleteNameCandidate) // 忽略单个候选名（建议区 ✕）
```

5. `Rename` 改名成功后清候选（`h.Speakers.UpdateName` 成功分支内、`writeJSON` 之前）：

```go
	// 改名 = 用户已确认称呼（采纳候选或手动命名）：清空候选——名字不再是随机名，
	// 后续也不再重跑推断。清空失败不回滚改名（候选残留仅影响建议展示，前端对
	// 非随机名说话人本就不显示建议区），log 便于排查。
	if h.SpeakerNameCandidates != nil {
		if err := h.SpeakerNameCandidates.DeleteBySpeaker(r.Context(), id); err != nil {
			log.Printf("[speaker] 改名后清候选失败 speaker=%s: %v", id, err)
		}
	}
```

（import 补 `"log"`。）

6. 文件末尾加忽略端点：

```go
// DeleteNameCandidate 忽略单个候选名（前端建议区 ✕ 按钮）。
// ?name= 指定候选名；幂等（不存在也 204）。repo 未装配 501。
func (h *SpeakerHandler) DeleteNameCandidate(w http.ResponseWriter, r *http.Request) {
	if h.SpeakerNameCandidates == nil {
		http.Error(w, "候选名功能未装配", http.StatusNotImplemented)
		return
	}
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "缺少 name", http.StatusBadRequest)
		return
	}
	if err := h.SpeakerNameCandidates.DeleteOne(r.Context(), id, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: 实现 query.go 改动（GetSession 富化）**

`internal/api/query.go`：

1. `QueryHandler` 加字段：

```go
	SpeakerNameCandidates *repo.SpeakerNameCandidateRepo // speakername stage：详情 speakers 附候选名
```

2. `GetSession` 中，`sis, _ := h.Transcripts.ListSpeakersForTranscript(...)` 之后、构建 `views` 之前，把 `sis` 转成带候选的视图并替换 `resp["speakers"] = sis`：

```go
		// sis 富化候选名（随机名说话人展示「建议名字」区）；repo 未装配则空候选
		type speakerWithCands struct {
			repo.SpeakerInSegment
			NameCandidates []NameCandidateView `json:"name_candidates"`
		}
		sisView := make([]speakerWithCands, len(sis))
		spIDs := make([]ids.ID, len(sis))
		for i := range sis {
			sisView[i] = speakerWithCands{SpeakerInSegment: sis[i], NameCandidates: []NameCandidateView{}}
			spIDs[i] = sis[i].SpeakerID
		}
		if h.SpeakerNameCandidates != nil {
			if cands, err := h.SpeakerNameCandidates.ListBySpeakers(r.Context(), spIDs); err == nil {
				idx := make(map[ids.ID]int, len(sisView))
				for i := range sisView {
					idx[sisView[i].SpeakerID] = i
				}
				for _, c := range cands {
					if i, ok := idx[c.SpeakerID]; ok {
						sisView[i].NameCandidates = append(sisView[i].NameCandidates,
							NameCandidateView{Name: c.Name, Confidence: c.Confidence, Evidence: c.Evidence})
					}
				}
			} // 查询失败降级为空候选，不阻断详情
		}
```

然后把原来的 `resp["speakers"] = sis` 改为 `resp["speakers"] = sisView`。

（`NameCandidateView` 定义在 speaker.go，同包复用，勿重复定义。）

- [ ] **Step 5: 补 Task 6 挪过来的 main.go 两处 API 注入 + 编译**

`cmd/zhiwei-server/main.go` 的 `RegisterSpeaker` / `RegisterQuery` 两个 handler 构造处加 `SpeakerNameCandidates: nameCandidates,`（Task 6 Step 2 的第 6/7 点）。

Run: `go build ./... && go vet ./...`
Expected: 无输出。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/api/ -run 'TestSpeaker' -v`（需 `TEST_MYSQL_DSN`）
Expected: 新旧 TestSpeaker* 全 PASS（无 DSN 时 skip 属正常；最终以 `make test-integration` 全绿为准）。

- [ ] **Step 7: Commit**

```bash
git add internal/api/speaker.go internal/api/query.go internal/api/speaker_test.go cmd/zhiwei-server/main.go
git commit -m "feat(api): speaker 列表附名字候选 + 改名清候选 + 忽略候选端点"
```

---

### Task 8: 前端（建议名字区：名称+置信度，采纳/忽略）

**Files:**
- Modify: `web/app.js`
- Modify: `web/index.html`

- [ ] **Step 1: app.js 加状态与操作函数**

在 `web/app.js` 说话人区域（`loadAllSpeakers` 定义附近，约 851 行后）追加：

```js
    // ---------- 建议名字（speakername stage 的 LLM 候选：名称+置信度数值，用户点选确认） ----------
    // 与后端 stage_speaker_name.go 的 autoNamePattern 保持一致：
    // 只有名字仍是自动随机名（说话人+5位[a-z0-9]）的说话人才展示建议区——
    // 用户改过名（含采纳过候选）后不再打扰。
    const AUTO_NAME_RE = /^说话人[a-z0-9]{5}$/;
    function hasNameCandidates(sp) {
      return AUTO_NAME_RE.test(sp.name) && (sp.name_candidates || []).length > 0;
    }
    // 采纳候选：把候选名写为说话人正式名。复用改名 PATCH（后端改名成功即清空全部候选）。
    // sp 兼容两种来源：时间线面板 detail.speakers（speaker_id 字段）/ 声纹 tab allSpeakers（id 字段）。
    async function acceptNameCandidate(sp, cand) {
      const id = sp.speaker_id || sp.id;
      try {
        await api('PATCH', '/api/speakers/' + id, { name: cand.name });
        if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
        await loadAllSpeakers();
      } catch (e) { showError(e); }
    }
    // 忽略单个候选：删该行（后端幂等）。成功后刷新两处列表。
    async function dismissNameCandidate(sp, cand) {
      const id = sp.speaker_id || sp.id;
      try {
        await api('DELETE', '/api/speakers/' + id + '/name-candidates?name=' + encodeURIComponent(cand.name));
        if (detail.value && detail.value.session) await reloadSession(detail.value.session.id);
        await loadAllSpeakers();
      } catch (e) { showError(e); }
    }
```

（注意这些函数需定义在 `setup()` 返回对象使用之前，且函数名要加进 setup 的 return——查 app.js 末尾 return 语句块，把 `hasNameCandidates`、`acceptNameCandidate`、`dismissNameCandidate` 加进返回对象，否则模板访问不到。）

- [ ] **Step 2: index.html 时间线说话人面板加建议区**

`web/index.html` 说话人面板（约 278-288 行，`speaker-row` div 结束后、「改名行」注释之前）插入：

```html
          <!-- 建议名字：speakername stage 用 LLM 从对话推断的候选（名称+置信度数值，倒排）。
               仅随机名说话人展示；✓ 采纳（改名+清候选）· ✕ 忽略该候选；悬浮看依据。 -->
          <div v-if="(detail.speakers||[]).some(hasNameCandidates)" style="margin:0 0 8px; display:flex; flex-direction:column; gap:4px">
            <div v-for="sp in detail.speakers.filter(hasNameCandidates)" :key="'cand-'+sp.speaker_id"
                 style="display:flex; align-items:center; gap:6px; flex-wrap:wrap">
              <span class="muted" style="font-size:var(--fs-xs)">建议名字（{{ sp.name }}）</span>
              <span v-for="c in sp.name_candidates" :key="c.name" class="chip"
                    style="cursor:default" :title="c.evidence || '无依据记录'">
                {{ c.name }} · {{ c.confidence.toFixed(2) }}
                <button class="chip-x" @click.stop="acceptNameCandidate(sp, c)" title="采纳为正式名">✓</button>
                <button class="chip-x" @click.stop="dismissNameCandidate(sp, c)" title="忽略该候选">✕</button>
              </span>
            </div>
          </div>
```

- [ ] **Step 3: index.html 声纹 tab 卡片加建议区**

声纹 tab 说话人卡片（约 540-560 行，名字行 `<div class="kv">` 内 template 结束后、「关联录音/删除」按钮所在的 div 之前，即 kv 块的下方）插入：

```html
      <!-- 建议名字：随机名声纹的 LLM 候选（名称+置信度数值，倒排）；采纳后名字即确认、候选清空 -->
      <div v-if="hasNameCandidates(sp)" style="display:flex; align-items:center; gap:6px; flex-wrap:wrap; margin:6px 0">
        <span class="muted" style="font-size:var(--fs-xs)">建议名字</span>
        <span v-for="c in sp.name_candidates" :key="c.name" class="chip"
              style="cursor:default" :title="c.evidence || '无依据记录'">
          {{ c.name }} · {{ c.confidence.toFixed(2) }}
          <button class="chip-x" @click.stop="acceptNameCandidate(sp, c)" title="采纳为正式名">✓</button>
          <button class="chip-x" @click.stop="dismissNameCandidate(sp, c)" title="忽略该候选">✕</button>
        </span>
      </div>
```

（此处 `sp` 来自 `v-for="(sp, idx) in allSpeakers"`，对象带 `id` 字段——`acceptNameCandidate` 的 `sp.speaker_id || sp.id` 已兼容。）

- [ ] **Step 4: 手动验证**

Run: `make compose-up && make sidecar-start`（如需）+ `set -a; source .env; set +a; make dev`，浏览器开 `http://localhost:8080`：
1. 上传一段含「当面称呼（如 张总您看…）+ 谈论第三人」的双人对话录音（可用 `testdata/` 已有音频或现录）。
2. 等处理完成 → 时间线展开 → 说话人面板出现「建议名字（说话人xxxxx）：张总 · 0.xx ✓ ✕」chips。
3. 点 ✓ → 名字变为「张总」、建议区消失；声纹 tab 同步。
4. 点 ✕ → 该候选消失，其余保留。
5. 悬浮候选 chip → title 显示 evidence。

Expected: 全部符合。若 LLM 未给出候选（对话里确实没称呼），建议区不出现——属正常。

- [ ] **Step 5: Commit**

```bash
git add web/app.js web/index.html
git commit -m "feat(web): 说话人建议名字区（名称+置信度，采纳/忽略，evidence 悬浮）"
```

---

### Task 9: 全量回归 + README + spike 验证

**Files:**
- Modify: `README.md`

- [ ] **Step 1: 全量测试**

Run: `make test && make test-integration`
Expected: 全绿（无 DB 时集成测试 skip 正常；`test-integration` 需 MySQL 容器已起）。

- [ ] **Step 2: README 补配置说明**

`README.md` 环境变量列表（`ZW_ENROLL_MIN_DURATION_MS` 相关行附近，若 README 已列声纹相关变量则跟在其后）加：

```markdown
  - `ZW_NAME_INFER_WINDOW_MIN`（说话人名字推断回看窗口，分钟，默认 `10`）
  - `ZW_NAME_INFER_MAX_SEGMENTS`（名字推断上下文段数上限，默认 `400`）
```

API 一览表 `GET/PATCH/DELETE /api/speakers` 行后补一行：

```markdown
DELETE /api/speakers/{id}/name-candidates?name=…   忽略单个建议名字候选
```

- [ ] **Step 3: 真 LLM spike（手动，不进 CI）**

```bash
make spike-llm   # 现有 spike，先确认 Ark LLM 连通
```

然后按 Task 8 Step 4 的手动流程跑一段真实对话录音，重点核对：
- 称呼归属正确（「张总」给被称呼方而非说话方）；
- 谈论第三人（「昨天王总来找我」）不产生「王总」候选；
- 多候选倒排、置信度数值合理（当面称呼 0.8+）。

Expected: 人工核对通过。发现 prompt 判定不稳时调 `prompts/speaker_naming_v1.md` 措辞（版本号规则同 extraction_v*：改动大时升 v2 并同步 `main.go` 的 `nameInferPromptPath`）。

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs(readme): 名字推断配置与 API 说明"
```

---

## 自检记录（writing-plans Self-Review）

1. **Spec 覆盖**：§3.1 资格正则→Task 4；§3.2 墙钟窗口→Task 3+5；§3.3 token→Task 5 Step 3；§4 prompt→Task 4 Step 1；§5 迁移/累积→Task 1+2；§6 stage→Task 5；§7 API/前端（含置信度数值硬性要求）→Task 7+8；§8 装配/配置→Task 5+6；§9 测试→各 Task + Task 9 spike。无缺口。
2. **占位符扫描**：无 TBD/TODO；所有代码步骤含完整代码。Task 4 Step 4 的 import 坑已在文中显式说明处理方式。
3. **类型一致性**：`Upsert(ctx, speakerID, name, confidence, evidence, sourceSessionID ids.ID)` 签名在 Task 2/5/7 三处使用一致；`NameCandidateView` 仅定义一次（api/speaker.go），query.go 复用；StageDeps 字段名与 main.go 注入一致；前端 `sp.speaker_id || sp.id` 兼容两处来源。
