# 幽灵历史声纹纠正 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** ASR 过度切分出的「幽灵历史声纹」组，若在自己名下的段上被同录音另一在场说话人（max 相似度口径）匹配得更好，则整组自动改判给那个人，并给受影响转写条目打持久化「已修改」标记。

**Architecture:** 在 `runSpeakerStage` 检索/登记/回填之后新增一趟纯内存「纠正 pass」——仅历史命中组参与，用与详情页同口径（对样本取最大余弦）打分，`max_Y score_Y > self_H + margin`（默认 0.06）时改判。标记落 `transcript_segment.corrected_from_speaker_id` 列（存被顶掉的原历史说话人 id），手动换人/重新识别时清空。API 详情返回该字段 + 原说话人名，前端渲染徽章。

**Tech Stack:** Go（`internal/pipeline`、`internal/api`、`internal/repo`）+ MySQL 迁移（golang-migrate）+ 前端 Vue（`web/app.js`/`web/index.html`）。测试用现有有状态 fake `libVoiceprint` + `repotest.DSN` 按包隔离库。

**运行前置：** 本分支调试用自己的临时库（见 memory `db-per-feature-convention`），勿动共享 `zhiwei` 库；集成测试自动用 `repotest.DSN` 的隔离库并自动跑迁移。stage 测试需要 `ffmpeg` + `testdata/speech.wav`（现有约定）。

---

## File Structure

- `migrations/000017_segment_speaker_correction.{up,down}.sql` — **新建**：`transcript_segment` 加 `corrected_from_speaker_id` 列。
- `internal/repo/transcript.go` — **改**：`TranscriptSegment` 加字段；新增 `CorrectSegmentSpeaker`；`SetSegmentSpeakerByID`/`ReassignSpeakerSegments`/`ReassignSpeakerInTranscript`/`ClearSegmentSpeakers` 的 SET 加清标记。
- `internal/repo/transcript_test.go` — **改**：加 `TestCorrectSegmentSpeakerAndClearOnManual`。
- `internal/pipeline/stage_asr.go` — **改**：`StageDeps` 加 `VoiceprintCorrectMargin float64`。
- `internal/pipeline/stage_speaker.go` — **改**：`groupRep` 加 `segVecs`；主流程收集 `resolvedID`；新增纠正 pass + 打分/解码 helper。
- `internal/pipeline/stage_speaker_test.go` — **改**：加 4 个纠正 pass 测试。
- `internal/config/config.go` — **改**：加 `VoiceprintCorrectMargin` + env 读取。
- `cmd/zhiwei-server/main.go` — **改**：pipeline `StageDeps` 注入 margin。
- `internal/api/query.go` — **改**：`segmentView` 加 `corrected_from`/`corrected_from_name`；`GetSession` 填充。
- `internal/api/query_test.go` — **改**：加 `TestGetSessionCorrectedMarker`。
- `web/app.js` / `web/index.html` — **改**：转写条目「已修改」徽章。

---

## Task 1: DB 迁移 + repo 标记读写与清理

**Files:**
- Create: `migrations/000017_segment_speaker_correction.up.sql`
- Create: `migrations/000017_segment_speaker_correction.down.sql`
- Modify: `internal/repo/transcript.go`（`TranscriptSegment` 结构、新增 `CorrectSegmentSpeaker`、4 处 SET 加清标记）
- Test: `internal/repo/transcript_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/repo/transcript_test.go` 末尾追加（复用文件已有的 `NewDB`/`repotest` 导入）：

```go
// TestCorrectSegmentSpeakerAndClearOnManual 覆盖幽灵声纹纠正的标记读写 + 手动/重识别清标记：
// CorrectSegmentSpeaker 按 label 整组改判并写 corrected_from；手动换人(SetSegmentSpeakerByID)、
// 整人改判(ReassignSpeakerSegments)、重新识别(ClearSegmentSpeakers)三条路径都应清掉标记。
func TestCorrectSegmentSpeakerAndClearOnManual(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr := &TranscriptRepo{DB: db}
	speakers := &SpeakerRepo{DB: db}

	// 两个说话人：ghost=被顶掉的历史人，real=真正说话人
	ghost := &Speaker{Name: "铉晔", Source: "auto"}
	real := &Speaker{Name: "说话人real", Source: "auto"}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	if err := speakers.Create(ctx, real); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), ghost.ID); _ = speakers.Delete(context.Background(), real.ID) })

	sid := ids.New()
	if err := (&SessionRepo{DB: db}).Create(ctx, &AudioSession{
		ID: sid, Source: "web_upload", Filename: "a.wav", StoragePath: "/tmp/a.wav", Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	tc := &Transcript{SessionID: sid, Language: "zh-CN"}
	if err := tr.Create(ctx, tc); err != nil {
		t.Fatal(err)
	}
	segs := []TranscriptSegment{
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "2", Text: "幽灵段甲", StartMS: 0, EndMS: 1000},
		{TranscriptID: tc.ID, SequenceNo: 2, SpeakerLabel: "2", Text: "幽灵段乙", StartMS: 1000, EndMS: 2000},
	}
	if err := tr.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	// 先把 label "2" 归到 ghost（模拟回填结果）
	if err := tr.SetSegmentSpeaker(ctx, tc.ID, "2", ghost.ID); err != nil {
		t.Fatal(err)
	}

	// 纠正：label "2" 从 ghost 改判给 real，写 corrected_from=ghost
	if err := tr.CorrectSegmentSpeaker(ctx, tc.ID, "2", ghost.ID, real.ID); err != nil {
		t.Fatalf("CorrectSegmentSpeaker: %v", err)
	}
	got, _ := tr.ListSegments(ctx, tc.ID)
	for _, s := range got {
		if s.SpeakerID == nil || *s.SpeakerID != real.ID {
			t.Fatalf("段 %d 应改判给 real，实际 %+v", s.SequenceNo, s.SpeakerID)
		}
		if s.CorrectedFromSpeakerID == nil || *s.CorrectedFromSpeakerID != ghost.ID {
			t.Fatalf("段 %d 应有 corrected_from=ghost，实际 %+v", s.SequenceNo, s.CorrectedFromSpeakerID)
		}
	}

	// 手动单段换人 → 清标记
	if err := tr.SetSegmentSpeakerByID(ctx, tc.ID, got[0].ID, ghost.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.ListSegments(ctx, tc.ID)
	if got[0].CorrectedFromSpeakerID != nil {
		t.Fatalf("手动换人后应清标记，实际 %+v", got[0].CorrectedFromSpeakerID)
	}
	if got[1].CorrectedFromSpeakerID == nil {
		t.Fatalf("未手动改的段标记不应被清")
	}

	// 整人改判 → 清标记
	if _, err := tr.ReassignSpeakerSegments(ctx, tc.ID, real.ID, ghost.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.ListSegments(ctx, tc.ID)
	if got[1].CorrectedFromSpeakerID != nil {
		t.Fatalf("整人改判后应清标记，实际 %+v", got[1].CorrectedFromSpeakerID)
	}

	// 重新纠正后再 ClearSegmentSpeakers → 清标记 + 清 speaker_id
	if err := tr.CorrectSegmentSpeaker(ctx, tc.ID, "2", ghost.ID, real.ID); err != nil {
		t.Fatal(err)
	}
	if err := tr.ClearSegmentSpeakers(ctx, tc.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.ListSegments(ctx, tc.ID)
	for _, s := range got {
		if s.SpeakerID != nil || s.CorrectedFromSpeakerID != nil {
			t.Fatalf("重新识别后 speaker_id 与标记都应清 NULL，实际 %+v / %+v", s.SpeakerID, s.CorrectedFromSpeakerID)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败（编译错误）**

Run: `go test ./internal/repo/ -run TestCorrectSegmentSpeakerAndClearOnManual -v`
Expected: FAIL — 编译错误 `s.CorrectedFromSpeakerID undefined` 和 `tr.CorrectSegmentSpeaker undefined`。

- [ ] **Step 3: 写迁移 up**

`migrations/000017_segment_speaker_correction.up.sql`：

```sql
-- 幽灵历史声纹纠正（2026-08-27 需求）：ASR 过度切分出的组常命中历史库某真人；纠正 pass
-- 把这类组整组改判给同录音里真正解释它的在场说话人，并在段上记录被顶掉的原历史说话人 id。
-- 非 NULL = 该段被自动纠正过 → 前端"已修改"徽章 + 审计 + 手动改回依据。
ALTER TABLE transcript_segment
  ADD COLUMN corrected_from_speaker_id BIGINT NULL
  COMMENT '幽灵历史声纹纠正：被自动顶掉的原历史说话人 id；非 NULL=该段已被自动改判';
```

- [ ] **Step 4: 写迁移 down**

`migrations/000017_segment_speaker_correction.down.sql`：

```sql
ALTER TABLE transcript_segment DROP COLUMN corrected_from_speaker_id;
```

- [ ] **Step 5: `TranscriptSegment` 加字段**

在 `internal/repo/transcript.go` 的 `TranscriptSegment` 结构里，`SpeakerID` 字段之后加（`SELECT *` + sqlx safe 模式要求列有对应字段）：

```go
	// CorrectedFromSpeakerID 幽灵历史声纹纠正（000017 迁移加列）：非 NULL = 该段被 speaker stage
	// 的纠正 pass 自动改判过，值为被顶掉的原历史说话人 id（前端"已修改"徽章 + 审计 + 手动改回依据）。
	// 手动换人 / 整人改判 / 重新识别时清 NULL。存量 / 未纠正段为 NULL。
	CorrectedFromSpeakerID *ids.ID `db:"corrected_from_speaker_id" json:"corrected_from_speaker_id,omitempty"`
```

- [ ] **Step 6: 新增 `CorrectSegmentSpeaker`**

在 `internal/repo/transcript.go` 的 `SetSegmentSpeakerByID` 方法之后加：

```go
// CorrectSegmentSpeaker 幽灵历史声纹纠正：把本 transcript 内某 speaker_label 的全部段
// 从原历史说话人 fromID 改判给 toID，并记录 corrected_from_speaker_id=fromID（前端"已修改"
// 徽章 + 审计）。纠正 pass 以「组=speaker_label」为单位，故按 label 定位；带 transcript_id
// 作用域防跨会话误写；单条 UPDATE 原子写、并发安全。
func (r *TranscriptRepo) CorrectSegmentSpeaker(ctx context.Context, transcriptID ids.ID, speakerLabel string, fromID, toID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = ?
		 WHERE transcript_id = ? AND speaker_label = ?`,
		toID.Int64(), fromID.Int64(), transcriptID.Int64(), speakerLabel)
	return err
}
```

- [ ] **Step 7: 4 处清标记**

在 `internal/repo/transcript.go` 修改以下 4 个方法的 SET 子句（手动/重识别覆盖自动纠正后徽章应消失）：

`SetSegmentSpeakerByID`（约 196-201 行）：
```go
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = NULL WHERE id = ? AND transcript_id = ?`,
		speakerID.Int64(), segID.Int64(), transcriptID.Int64())
```

`ClearSegmentSpeakers`（约 189-192 行）：
```go
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = NULL, corrected_from_speaker_id = NULL WHERE transcript_id = ?`, transcriptID.Int64())
```

`ReassignSpeakerSegments`（约 223-225 行）：
```go
	res, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = NULL WHERE transcript_id = ? AND speaker_id = ?`,
		toID.Int64(), transcriptID.Int64(), fromID.Int64())
```

`ReassignSpeakerInTranscript`（约 240-242 行）：
```go
	res, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = NULL WHERE transcript_id = ? AND speaker_id = ?`,
		toID.Int64(), transcriptID.Int64(), fromID.Int64())
```

- [ ] **Step 8: 运行测试确认通过**

Run: `go test ./internal/repo/ -run TestCorrectSegmentSpeakerAndClearOnManual -v`
Expected: PASS

- [ ] **Step 9: 提交**

```bash
git add migrations/000017_segment_speaker_correction.up.sql migrations/000017_segment_speaker_correction.down.sql internal/repo/transcript.go internal/repo/transcript_test.go
git commit -m "feat(repo): transcript_segment 加 corrected_from_speaker_id + 纠正写入/清理"
```

---

## Task 2: stage 纠正 pass（核心算法）+ margin 配置

**Files:**
- Modify: `internal/pipeline/stage_asr.go`（`StageDeps` 加 `VoiceprintCorrectMargin`）
- Modify: `internal/pipeline/stage_speaker.go`（`groupRep.segVecs`、收集 `resolvedID`、纠正 pass、helper）
- Modify: `internal/config/config.go`、`cmd/zhiwei-server/main.go`（wiring）
- Test: `internal/pipeline/stage_speaker_test.go`

- [ ] **Step 1: 写失败测试（4 个）**

在 `internal/pipeline/stage_speaker_test.go` 末尾追加。这些测试构造的向量满足：`vR`=真人 iux5x 方向（=e0），`vHist`=历史人铉晔（与 vR 相关 cos=0.7），`vP`=幽灵段（cos 到 vHist=0.73、到 vR=0.88）。几何见设计文档第 5 节。

```go
// buildCorrectionVecs 构造纠正 pass 测试用的三组单位向量：
// vR   真人/新登记说话人方向（e0）
// vHist 历史库说话人方向（与 vR 余弦 = corrHistR，模拟两人声纹相关）
// vSeg  某幽灵段向量：cos(vSeg,vR)=simR、cos(vSeg,vHist)=simHist
// 返回三者（256 维，L2 归一）。要求 simR、simHist、corrHistR 几何可解（z²≥0）。
func buildCorrectionVecs(t *testing.T, corrHistR, simR, simHist float64) (vR, vHist, vSeg []float32) {
	t.Helper()
	e := func(idx int, val float64) []float32 { v := make([]float32, 256); v[idx] = float32(val); return v }
	vR = e(0, 1)
	h1 := math.Sqrt(1 - corrHistR*corrHistR)
	vHist = make([]float32, 256)
	vHist[0] = float32(corrHistR)
	vHist[1] = float32(h1)
	// vSeg = x*e0 + y*e1 + z*e2，x=simR；x*corrHistR + y*h1 = simHist
	x := simR
	y := (simHist - x*corrHistR) / h1
	z2 := 1 - x*x - y*y
	if z2 < 0 {
		t.Fatalf("几何不可解: simR=%v simHist=%v corr=%v (z²=%v)", simR, simHist, corrHistR, z2)
	}
	vSeg = make([]float32, 256)
	vSeg[0] = float32(x)
	vSeg[1] = float32(y)
	vSeg[2] = float32(math.Sqrt(z2))
	return vR, vHist, vSeg
}

// TestStageSpeakerCorrectsPhantomHistoricalMatch 幽灵历史声纹纠正主链路：
// label "1"(seq1,2)=真人 → 空历史库中登记为新声纹；label "2"(seq3)=幽灵 → 弱命中历史人铉晔。
// 铉晔在幽灵段上 max=0.73，真人在幽灵段上 max=0.88 > 0.73+0.06 → 整组改判给真人，写 corrected_from=铉晔。
func TestStageSpeakerCorrectsPhantomHistoricalMatch(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vR, vHist, vSeg := buildCorrectionVecs(t, 0.7, 0.88, 0.73)
	ghost := &repo.Speaker{Name: "铉晔", Source: "auto", Embedding: float32Blob(vHist), SampleCount: 1}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), ghost.ID) })
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vR, 2: vR, 3: vSeg},
		entries:  []libEntry{{id: ghost.ID, vec: vHist}}, // 本 run 开始前的历史库
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	bySeq := map[int]repo.TranscriptSegment{}
	for _, s := range segs {
		bySeq[s.SequenceNo] = s
	}
	realID := bySeq[1].SpeakerID // 真人 = seq1 归属
	if realID == nil {
		t.Fatal("seq1 未回填")
	}
	seg3 := bySeq[3]
	if seg3.SpeakerID == nil || *seg3.SpeakerID != *realID {
		t.Fatalf("幽灵段 seq3 应改判给真人 %v，实际 %+v", *realID, seg3.SpeakerID)
	}
	if seg3.CorrectedFromSpeakerID == nil || *seg3.CorrectedFromSpeakerID != ghost.ID {
		t.Fatalf("幽灵段应有 corrected_from=铉晔 %v，实际 %+v", ghost.ID, seg3.CorrectedFromSpeakerID)
	}
	if len(fv.added) != 1 {
		t.Fatalf("应只新登记真人 1 个声纹，实际 %d", len(fv.added))
	}
}

// TestStageSpeakerKeepsHistoricalMatchWhenSelfHighest 历史命中且在自己段上最高 → 不纠正。
// 幽灵段 cos 到铉晔=0.85(强命中)、到真人=0.5：0.5 不 > 0.85+0.06 → 保持铉晔、无标记。
func TestStageSpeakerKeepsHistoricalMatchWhenSelfHighest(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vR, vHist, vSeg := buildCorrectionVecs(t, 0.7, 0.5, 0.85)
	ghost := &repo.Speaker{Name: "铉晔", Source: "auto", Embedding: float32Blob(vHist), SampleCount: 1}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), ghost.ID) })
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vR, 2: vR, 3: vSeg},
		entries:  []libEntry{{id: ghost.ID, vec: vHist}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo == 3 {
			if s.SpeakerID == nil || *s.SpeakerID != ghost.ID {
				t.Fatalf("seq3 应保持铉晔，实际 %+v", s.SpeakerID)
			}
			if s.CorrectedFromSpeakerID != nil {
				t.Fatalf("未纠正段不应有标记，实际 %+v", s.CorrectedFromSpeakerID)
			}
		}
	}
}

// TestStageSpeakerNeverCorrectsAutoRegistered 空历史库 → 两组都是新登记(非历史命中)：
// 即使一组段更像另一组，也不参与纠正（只有历史命中组是候选）。断言无任何 corrected_from。
func TestStageSpeakerNeverCorrectsAutoRegistered(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vA := make([]float32, 256)
	vA[0] = 1
	vB := make([]float32, 256)
	vB[0] = 0.9
	vB[1] = float32(math.Sqrt(1 - 0.81)) // cos(vA,vB)=0.9
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vA, 3: vB}} // 空历史库
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.CorrectedFromSpeakerID != nil {
			t.Fatalf("新登记组不应被纠正，seq%d 却有标记 %+v", s.SequenceNo, s.CorrectedFromSpeakerID)
		}
	}
	if len(fv.added) != 2 {
		t.Fatalf("空库两人应各建 1 个，实际 %d", len(fv.added))
	}
}

// TestStageSpeakerCorrectionMarginBoundary 边界：真人在幽灵段上恰好 = self+margin（严格大于才纠正）→ 不纠正。
// self(铉晔)=0.73，margin=0.06 → 真人=0.79 恰好不触发。
func TestStageSpeakerCorrectionMarginBoundary(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStage(t)
	vR, vHist, vSeg := buildCorrectionVecs(t, 0.7, 0.79, 0.73)
	ghost := &repo.Speaker{Name: "铉晔", Source: "auto", Embedding: float32Blob(vHist), SampleCount: 1}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), ghost.ID) })
	fv := &libVoiceprint{
		vecBySeq: map[int][]float32{1: vR, 2: vR, 3: vSeg},
		entries:  []libEntry{{id: ghost.ID, vec: vHist}},
	}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo == 3 && s.CorrectedFromSpeakerID != nil {
			t.Fatalf("恰好等于 self+margin 不应触发（需严格大于），实际标记 %+v", s.CorrectedFromSpeakerID)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/pipeline/ -run 'TestStageSpeakerCorrects|TestStageSpeakerKeeps|TestStageSpeakerNeverCorrects|TestStageSpeakerCorrectionMargin' -v`
Expected: FAIL — `TestStageSpeakerCorrectsPhantomHistoricalMatch` 断言失败（seq3 仍是铉晔、无 corrected_from），因为纠正 pass 尚未实现。

- [ ] **Step 3: `StageDeps` 加 margin 字段**

在 `internal/pipeline/stage_asr.go` 的 `StageDeps` 里 `VoiceprintThreshold` 字段之后加：

```go
	// VoiceprintCorrectMargin 幽灵历史声纹纠正的领先幅度门槛（ZW_VOICEPRINT_CORRECT_MARGIN）。
	// 0 表示用默认 0.06。仅 speaker stage 的纠正 pass 使用。
	VoiceprintCorrectMargin float64
```

- [ ] **Step 4: `groupRep` 加 `segVecs` 并在主流程收集**

在 `internal/pipeline/stage_speaker.go` 的 `groupRep` 结构里加字段：

```go
		// segVecs 组内各段与其向量（纠正 pass 用：逐段对各在场说话人打分）。
		segVecs []segVec
```

在构建 `reps` 的 `append` 处（约 104-107 行）补上 `segVecs`：

```go
		reps = append(reps, groupRep{
			label: label, rep: aggregateEmbeddings(vecs), vecN: len(vecs),
			clean:   pickCleanSegVec(svs, segs, label),
			segVecs: svs,
		})
```

- [ ] **Step 5: 收集 `resolvedID` 并在回填后调用纠正 pass**

在 `internal/pipeline/stage_speaker.go` 第二趟（回填）循环之前声明 `resolvedID`，循环内记录，循环后调用纠正 pass。把现有第二趟循环（约 143-175 行）替换为：

```go
	// 第二趟：未命中的登记新声纹（此时才 Add，故上面的检索全部只见历史库），命中的复用，回填。
	resolvedID := make([]ids.ID, len(reps)) // 每组最终解析到的 speaker（纠正 pass 用）
	for i, g := range reps {
		var speakerID ids.ID
		if matched[i] {
			speakerID = matchedID[i]
		} else {
			// 自动登记：name=说话人{5位随机串}，向量 BLOB 灾备。
			// 登记向量优先用干净段（pickCleanSegVec 的结果）：混入他人语音的段会污染
			// 聚合向量，新声纹「出厂即脏」；无干净段才退回聚合代表。
			embVec, sampleN := g.rep, g.vecN
			if g.clean != nil {
				embVec, sampleN = g.clean, 1
			}
			sp := &repo.Speaker{Name: "说话人" + rand5(), Source: "auto", Embedding: float32Blob(embVec), SampleCount: sampleN}
			if err := d.Speakers.Create(ctx, sp); err != nil {
				return fmt.Errorf("登记 speaker: %w", err)
			}
			if err := d.Voiceprint.Add(ctx, embVec, sp.ID); err != nil {
				return fmt.Errorf("voiceprint add: %w", err)
			}
			// 样本行落库（多条声纹模型；nil = 旧装配跳过。失败仅 log 不致命——speaker/FAISS
			// 已就绪，样本行缺失只影响后续聚合重算来源，可用启动 bootstrap 兜底补齐）
			if d.SpeakerEmbeddings != nil {
				e := &repo.SpeakerEmbedding{SpeakerID: sp.ID, Embedding: float32Blob(embVec), SampleCount: sampleN, Source: "auto"}
				if err := d.SpeakerEmbeddings.Create(ctx, e); err != nil {
					log.Printf("[speaker] 自动登记后样本行落库失败 speaker=%s: %v", sp.ID, err)
				}
			}
			speakerID = sp.ID
		}
		resolvedID[i] = speakerID
		if err := d.Transcripts.SetSegmentSpeaker(ctx, tr.ID, g.label, speakerID); err != nil {
			return fmt.Errorf("回填 speaker_id: %w", err)
		}
	}

	// 3) 幽灵历史声纹纠正 pass（2026-08-27 需求）：见 correctPhantomHistoricalMatches。
	margin := d.VoiceprintCorrectMargin
	if margin == 0 {
		margin = defaultCorrectMargin
	}
	if err := correctPhantomHistoricalMatches(ctx, d, tr, reps, matched, resolvedID, margin); err != nil {
		return err
	}
	return nil
}
```

（注意：删掉原循环末尾原有的 `return nil`，改由上面新的结尾统一返回。）

- [ ] **Step 6: 实现纠正 pass + helper**

在 `internal/pipeline/stage_speaker.go` 里 `minCleanSegMS` 常量附近加常量，并在 `pickCleanSegVec` 之前（或文件合适位置）加纠正 pass 与 helper：

```go
// defaultCorrectMargin 幽灵历史声纹纠正的默认领先幅度门槛（沿用 voiceprint.GapMin 经验值）。
// max 相似度口径下，真人在幽灵段上需比历史人自身 max 领先该幅度才改判，挡住接近平局的噪声翻转。
const defaultCorrectMargin = 0.06

// correctPhantomHistoricalMatches 幽灵历史声纹纠正（2026-08-27 需求）：
// ASR 过度切分出的幽灵组常命中历史库某真人；若该组名下的段被同录音另一在场说话人
// 匹配得更好（max 相似度口径，与详情页 topVoiceMatchesVec 同口径），判为幽灵、整组改判
// 给那个人，段写 corrected_from。仅**历史命中组**（matched[i]）参与——新登记组的声纹是
// 从自己段建出来的、天生在自己段上最高，不可能被判幽灵。先算全部判定（基于本趟归属快照）、
// 再统一应用，避免链式/互换改判抖动。
func correctPhantomHistoricalMatches(ctx context.Context, d StageDeps, tr *repo.Transcript,
	reps []groupRep, matched []bool, resolvedID []ids.ID, margin float64) error {
	if len(reps) < 2 {
		return nil // 少于两个在场说话人无可比对象
	}
	// 每个在场说话人的样本向量集合：历史命中 → 库内多条样本(回退聚合代表)；新登记 → 本趟登记向量。
	samples := make([][][]float32, len(reps))
	for i, g := range reps {
		if matched[i] {
			samples[i] = loadSpeakerSampleVecs(ctx, d, resolvedID[i])
		} else {
			embVec := g.rep
			if g.clean != nil {
				embVec = g.clean
			}
			samples[i] = [][]float32{embVec}
		}
	}
	type fix struct {
		label    string
		from, to ids.ID
	}
	var fixes []fix
	for i, g := range reps {
		if !matched[i] || len(g.segVecs) == 0 || len(samples[i]) == 0 {
			continue // 仅历史命中组是候选
		}
		// self = 该组段对「历史人自己」的最高相似度（对样本取 max，再对段取 max）
		self := 0.0
		for _, sv := range g.segVecs {
			if s := segMaxScore(sv.vec, samples[i]); s > self {
				self = s
			}
		}
		// 找在场其他说话人里，在本组段上得分最高者
		bestScore, bestJ := -1.0, -1
		for j := range reps {
			if j == i || resolvedID[j] == resolvedID[i] {
				continue // 跳过自己、跳过解析到同一 speaker 的组
			}
			sc := 0.0
			for _, sv := range g.segVecs {
				if s := segMaxScore(sv.vec, samples[j]); s > sc {
					sc = s
				}
			}
			if sc > bestScore {
				bestScore, bestJ = sc, j
			}
		}
		if bestJ >= 0 && bestScore > self+margin {
			fixes = append(fixes, fix{label: g.label, from: resolvedID[i], to: resolvedID[bestJ]})
		}
	}
	for _, f := range fixes {
		if err := d.Transcripts.CorrectSegmentSpeaker(ctx, tr.ID, f.label, f.from, f.to); err != nil {
			return fmt.Errorf("幽灵历史声纹纠正: %w", err)
		}
	}
	return nil
}

// segMaxScore 段向量对某说话人「多条样本取最大余弦」——与详情页 topVoiceMatchesVec 同口径，
// 保证纠正判定与用户在详情页看到的段级相似度数字一致。样本为空返回 0。
func segMaxScore(seg []float32, sampleVecs [][]float32) float64 {
	best := 0.0
	for _, sv := range sampleVecs {
		if s := dotSim(seg, sv); s > best {
			best = s
		}
	}
	return best
}

// dotSim 两个 L2 归一向量的内积（= 余弦）。声纹向量由 sidecar 归一化，与 api.cosine 同实现。
func dotSim(a, b []float32) float64 {
	var s float64
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// loadSpeakerSampleVecs 取说话人的多条样本向量（详情页同口径）；无样本行 / 未装配 repo
// 回退聚合代表（speaker.embedding）。
func loadSpeakerSampleVecs(ctx context.Context, d StageDeps, spID ids.ID) [][]float32 {
	var vecs [][]float32
	if d.SpeakerEmbeddings != nil {
		if es, err := d.SpeakerEmbeddings.ListBySpeaker(ctx, spID); err == nil {
			for _, e := range es {
				if v, ok := decodeEmbeddingPipe(e.Embedding); ok && len(v) == 256 {
					vecs = append(vecs, v)
				}
			}
		}
	}
	if len(vecs) == 0 && d.Speakers != nil {
		if sp, err := d.Speakers.Get(ctx, spID); err == nil {
			if v, ok := decodeEmbeddingPipe(sp.Embedding); ok && len(v) == 256 {
				vecs = append(vecs, v)
			}
		}
	}
	return vecs
}

// decodeEmbeddingPipe []byte(256×float32 LE) → []float32（与 float32Blob 互逆）。
func decodeEmbeddingPipe(blob []byte) ([]float32, bool) {
	if len(blob) == 0 || len(blob)%4 != 0 {
		return nil, false
	}
	v := make([]float32, len(blob)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return v, true
}
```

- [ ] **Step 7: 运行 stage 测试确认通过**

Run: `go test ./internal/pipeline/ -run 'TestStageSpeakerCorrects|TestStageSpeakerKeeps|TestStageSpeakerNeverCorrects|TestStageSpeakerCorrectionMargin' -v`
Expected: PASS（4 个全过）

- [ ] **Step 8: 跑全 pipeline 包回归（确认没破坏既有两趟登记等测试）**

Run: `go test ./internal/pipeline/ -v`
Expected: PASS（含既有 `TestStageSpeakerFirstMultiSpeakerEmptyLibrary`、`TestStageSpeakerHistoricalSingleVoiceprintWeakMatchReuses` 等）

- [ ] **Step 9: 配置 wiring**

`internal/config/config.go` 的 `Config` 结构里 `VoiceprintThreshold` 之后加字段：
```go
	VoiceprintCorrectMargin float64 // 幽灵历史声纹纠正领先幅度门槛，0→默认 0.06
```
并在 `Load`（约 128 行 `VoiceprintThreshold` 之后）加：
```go
		VoiceprintCorrectMargin: getenvFloat("ZW_VOICEPRINT_CORRECT_MARGIN", 0.06),
```

`cmd/zhiwei-server/main.go` 的 pipeline `StageDeps`（约 220-221 行）里补：
```go
		Voiceprint: voiceprintCli, Speakers: speakers, VoiceprintThreshold: cfg.VoiceprintThreshold,
		VoiceprintCorrectMargin: cfg.VoiceprintCorrectMargin,
```

- [ ] **Step 10: 编译确认**

Run: `go build ./...`
Expected: 无错误

- [ ] **Step 11: 提交**

```bash
git add internal/pipeline/stage_speaker.go internal/pipeline/stage_asr.go internal/pipeline/stage_speaker_test.go internal/config/config.go cmd/zhiwei-server/main.go
git commit -m "feat(pipeline): speaker stage 幽灵历史声纹纠正 pass（max 口径 + margin 配置）"
```

---

## Task 3: 详情 API 下发 corrected_from + 原说话人名

**Files:**
- Modify: `internal/api/query.go`（`segmentView` 加字段、`GetSession` 填充）
- Test: `internal/api/query_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/api/query_test.go` 末尾追加（复用文件已有的 `chiInject`/`RegisterQuery` 等模式，参考同文件 `TestGetSessionSpeakerEnrichment`）：

```go
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
	if len(resp.Segments) != 1 {
		t.Fatalf("应有 1 段，实际 %d", len(resp.Segments))
	}
	if resp.Segments[0].CorrectedFrom != ghost.ID.String() {
		t.Fatalf("corrected_from 应为铉晔 id，实际 %q", resp.Segments[0].CorrectedFrom)
	}
	if resp.Segments[0].CorrectedFromName != "铉晔" {
		t.Fatalf("corrected_from_name 应为铉晔，实际 %q", resp.Segments[0].CorrectedFromName)
	}
}
```

（若 `query_test.go` 尚未导入 `encoding/json` / `github.com/go-chi/chi/v5` / `zhiwei/internal/auth`，按文件现有 import 风格补齐——`TestGetSessionSpeakerEnrichment` 已用到 chi router + auth 注入，可直接照搬其 import。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/api/ -run TestGetSessionCorrectedMarker -v`
Expected: FAIL — `corrected_from` 为空（`segmentView` 未含该字段、`GetSession` 未填充）。

- [ ] **Step 3: `segmentView` 加字段**

在 `internal/api/query.go` 的 `segmentView` 结构里 `VoiceMatches` 字段之后加：

```go
	// CorrectedFrom 非空 = 该段被 speaker stage 的幽灵历史声纹纠正 pass 自动改判过；
	// 值为被顶掉的原历史说话人 id，CorrectedFromName 为其显示名（前端"已修改"徽章 + tooltip）。
	CorrectedFrom     string `json:"corrected_from,omitempty"`
	CorrectedFromName string `json:"corrected_from_name,omitempty"`
```

- [ ] **Step 4: `GetSession` 填充**

在 `internal/api/query.go` 的 `GetSession` 逐段循环里，紧接现有 `views[i].Speaker = ...` 归属解析块（约 412-421 行）之后、`VoiceMatches` 计算之前，加：

```go
			// 幽灵历史声纹纠正标记：原历史人可能已不在本会话 speaker 列表（spMap），从 Speakers 兜底解析名字。
			if sg.CorrectedFromSpeakerID != nil {
				views[i].CorrectedFrom = sg.CorrectedFromSpeakerID.String()
				if name, ok := spMap[*sg.CorrectedFromSpeakerID]; ok {
					views[i].CorrectedFromName = name
				} else if h.Speakers != nil {
					if sp, err := h.Speakers.Get(r.Context(), *sg.CorrectedFromSpeakerID); err == nil {
						views[i].CorrectedFromName = sp.Name
					}
				}
			}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/api/ -run TestGetSessionCorrectedMarker -v`
Expected: PASS

- [ ] **Step 6: api 包回归**

Run: `go test ./internal/api/ -v`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add internal/api/query.go internal/api/query_test.go
git commit -m "feat(api): 详情段下发 corrected_from + 原说话人名"
```

---

## Task 4: 前端「已修改」徽章

**Files:**
- Modify: `web/index.html`（转写条目模板加徽章）
- Modify: `web/app.js`（如需，确认段数据已含 `corrected_from`——API 直接下发，通常无需改 JS 逻辑）

> 说明：前端无自动化测试，本任务靠 `/run` 或手动在浏览器核对。徽章为纯展示，数据来自 Task 3 的 API 字段。

- [ ] **Step 1: 定位转写条目模板**

Run: `grep -n "voice_matches\|声纹 ≈\|VoiceMatches\|s.speaker\|segment" web/index.html | head`
先找到详情页渲染单条转写段（含 speaker 标签 chip 与「声纹 ≈ …」相似度行）的 `v-for` 块，确认段对象变量名（下述以 `s` 为例）与相似度行位置。

- [ ] **Step 2: 加徽章模板**

在转写条目里 speaker 标签 chip 附近（与「声纹 ≈ …」同区域），加一个条件徽章。示例（按实际模板变量名/类名微调）：

```html
<span v-if="s.corrected_from"
      class="corrected-badge"
      :title="'原判定：' + (s.corrected_from_name || '未知') + '（声纹自动纠正）'">已修改</span>
```

- [ ] **Step 3: 加最小样式**

在 `web/index.html` 现有 `<style>` 里（或既有 chip 样式附近）加：

```css
.corrected-badge {
  display: inline-block;
  margin-left: 6px;
  padding: 0 6px;
  font-size: 11px;
  line-height: 18px;
  color: #8a6d00;
  background: #fff4d6;
  border: 1px solid #f0d98a;
  border-radius: 9px;
  vertical-align: middle;
  cursor: help;
}
```

- [ ] **Step 4: 手动核对**

用 `/run` 启动应用，打开一个含被自动纠正段的会话详情（或用 Task 2 场景造一条），确认：
- 被纠正的转写条目 speaker 标签旁显示灰黄「已修改」徽章；
- 悬停 tooltip 显示「原判定：铉晔（声纹自动纠正）」；
- 用换人下拉手动改判该段后，重拉详情，徽章消失（Task 1 已保证清标记）。

- [ ] **Step 5: 提交**

```bash
git add web/index.html web/app.js
git commit -m "feat(web): 转写条目'已修改'徽章（幽灵历史声纹纠正）"
```

---

## Self-Review 结论（作者已核对）

- **Spec 覆盖**：算法(§5)→Task 2；数据模型/标记(§6)→Task 1；标记生命周期(§7)→Task 1(清标记)+Task 2(纠正写入)；前端(§8)→Task 4；测试(§10)→各任务 TDD；改动清单(§11)→逐项落到 Files。
- **打分口径一致性**：`segMaxScore`(pipeline) 与 `topVoiceMatchesVec`(api) 都是「对样本取最大余弦、内积实现」，保证徽章⇔可见数字一致。
- **类型/命名一致**：新列 `corrected_from_speaker_id`、struct 字段 `CorrectedFromSpeakerID`、repo 方法 `CorrectSegmentSpeaker`、JSON `corrected_from`/`corrected_from_name` 在 Task 1/2/3 间统一。
- **无占位符**：所有步骤含真实代码/命令/预期。
- **已知取舍**（写入设计文档 §9）：仅整段重跑（identify/reidentify）可靠触发；margin 严格大于；不级联。
