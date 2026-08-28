# 过短噪声段并入 + 段时长显示 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 未命中历史库、总时长<3s 的新说话人组不再登记为独立声纹，而是整组并入本录音里最匹配的在场说话人并标记「已修改」；两类自动纠正（幽灵/过短）用统一的 `corrected_reason` 标记；详情页每段显示时长。

**Architecture:** 在 speaker stage 的两趟检索/登记之后，pass2 对「过短未命中组」跳过登记（不建 speaker/不入 FAISS），pass3 在幽灵纠正后新增一趟把这些组按 max 余弦（详情页同口径）并入最近的非过短在场说话人。标记落新列 `corrected_reason`（phantom/short）。

**Tech Stack:** Go（`internal/pipeline`/`internal/api`/`internal/repo`）+ MySQL 迁移 + 前端 Vue（`web/index.html`）。测试用有状态 fake `libVoiceprint` + `repotest.DSN`。

**运行环境：** worktree `.claude/worktrees/voiceprint-short-merge`（分支 `feat/voiceprint-short-merge`，基于 main `459e364`，已含幽灵声纹代码）。`testdata/` 已复制进 worktree。测试 DSN：`TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei?parseTime=true&charset=utf8mb4&multiStatements=true"`（库名会被 repotest 按包替换）。**若测试报 `Unknown column corrected_reason`：`docker exec zhiwei-mvp-mysql` 里 `DROP DATABASE zhiwei_test_<pkg>` 让其按本分支迁移重建**（撞号/半迁移 hazard，见记忆）。**验证编译一律 `go build ./...`/`go vet ./...`，勿信 IDE 跨-worktree 假诊断。**

---

## File Structure

- `migrations/000021_segment_corrected_reason.{up,down}.sql` — 新建：加 `corrected_reason` 列。
- `internal/repo/transcript.go` — `TranscriptSegment.CorrectedReason`；新增 `MergeShortGroup`；`CorrectSegmentSpeaker` 补 `corrected_reason='phantom'`；4 条清标记路径补 `corrected_reason=NULL`。
- `internal/repo/transcript_test.go` — 加 `TestMergeShortGroupAndReasonClearing`。
- `internal/pipeline/stage_speaker.go` — `groupRep.durMS`；pass2 `hasTarget`/`deferred`；抽 `buildGroupSamples`；`correctPhantomHistoricalMatches` 加 `deferred`+`samples` 参数并跳过 deferred 目标；新增 `mergeShortGroups`。
- `internal/pipeline/stage_speaker_test.go` — 抽参数化 `seedSpeakerStageSegs`；把默认 `seedSpeakerStage` 的 seq3 改为 ≥3s；加 3 个短并入测试。
- `internal/api/query.go` — `segmentView.CorrectedReason` + 填充。
- `internal/api/query_test.go` — 加/扩 `corrected_reason` 断言。
- `web/index.html` — 徽章触发统一 + tooltip 分原因 + 段时长显示。

---

## Task 1: 迁移 000021 + repo 标记

**Files:**
- Create: `migrations/000021_segment_corrected_reason.up.sql`, `...down.sql`
- Modify: `internal/repo/transcript.go`
- Test: `internal/repo/transcript_test.go`

- [ ] **Step 1: 失败测试** — 追加到 `internal/repo/transcript_test.go`（复用文件已有 imports）：

```go
// TestMergeShortGroupAndReasonClearing 覆盖过短并入的标记写入 + corrected_reason 清理：
// MergeShortGroup 把某 label 未回填段并入目标并写 corrected_reason='short'(corrected_from 为 NULL)；
// CorrectSegmentSpeaker 写 'phantom'；手动换人/整人改判/重新识别清 corrected_reason。
func TestMergeShortGroupAndReasonClearing(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr := &TranscriptRepo{DB: db}
	speakers := &SpeakerRepo{DB: db}
	target := &Speaker{Name: "说话人target", Source: "auto"}
	ghost := &Speaker{Name: "铉晔", Source: "auto"}
	if err := speakers.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := speakers.Create(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = speakers.Delete(context.Background(), target.ID); _ = speakers.Delete(context.Background(), ghost.ID) })

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
	if err := tr.InsertSegments(ctx, []TranscriptSegment{
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "noise", Text: "嗯。", StartMS: 0, EndMS: 400},
	}); err != nil {
		t.Fatal(err)
	}

	// 过短并入：label "noise" 未回填段 → 目标 target，reason=short，corrected_from 置空
	if err := tr.MergeShortGroup(ctx, tc.ID, "noise", target.ID); err != nil {
		t.Fatalf("MergeShortGroup: %v", err)
	}
	got, _ := tr.ListSegments(ctx, tc.ID)
	if got[0].SpeakerID == nil || *got[0].SpeakerID != target.ID {
		t.Fatalf("应并入 target，实际 %+v", got[0].SpeakerID)
	}
	if got[0].CorrectedReason == nil || *got[0].CorrectedReason != "short" {
		t.Fatalf("应 corrected_reason=short，实际 %+v", got[0].CorrectedReason)
	}
	if got[0].CorrectedFromSpeakerID != nil {
		t.Fatalf("short 并入 corrected_from 应为 NULL，实际 %+v", got[0].CorrectedFromSpeakerID)
	}

	// 手动换人 → 清 corrected_reason
	if err := tr.SetSegmentSpeakerByID(ctx, tc.ID, got[0].ID, ghost.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.ListSegments(ctx, tc.ID)
	if got[0].CorrectedReason != nil {
		t.Fatalf("手动换人后应清 corrected_reason，实际 %+v", got[0].CorrectedReason)
	}

	// CorrectSegmentSpeaker 写 phantom（segment 当前 speaker_id=ghost）
	if err := tr.CorrectSegmentSpeaker(ctx, tc.ID, "noise", ghost.ID, target.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.ListSegments(ctx, tc.ID)
	if got[0].CorrectedReason == nil || *got[0].CorrectedReason != "phantom" {
		t.Fatalf("CorrectSegmentSpeaker 应写 phantom，实际 %+v", got[0].CorrectedReason)
	}
	// 重新识别清两者
	if err := tr.ClearSegmentSpeakers(ctx, tc.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.ListSegments(ctx, tc.ID)
	if got[0].SpeakerID != nil || got[0].CorrectedReason != nil || got[0].CorrectedFromSpeakerID != nil {
		t.Fatalf("重新识别应清空 speaker_id/corrected_reason/corrected_from，实际 %+v/%+v/%+v",
			got[0].SpeakerID, got[0].CorrectedReason, got[0].CorrectedFromSpeakerID)
	}
}
```

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/repo/ -run TestMergeShortGroupAndReasonClearing -v` → 编译错误（`CorrectedReason`/`MergeShortGroup` undefined）。

- [ ] **Step 3: 迁移 up** — `migrations/000021_segment_corrected_reason.up.sql`：

```sql
-- 统一自动纠正标记（2026-08-28 需求）：区分两类自动改判。
-- phantom=幽灵历史声纹改判(配 corrected_from_speaker_id) | short=过短噪声段并入最近在场说话人(corrected_from 为 NULL)。
ALTER TABLE transcript_segment
  ADD COLUMN corrected_reason VARCHAR(16) NULL
  COMMENT '自动纠正原因 phantom|short；NULL=未纠正';
```

- [ ] **Step 4: 迁移 down** — `migrations/000021_segment_corrected_reason.down.sql`：

```sql
ALTER TABLE transcript_segment DROP COLUMN corrected_reason;
```

- [ ] **Step 5: `TranscriptSegment` 加字段** — 在 `internal/repo/transcript.go` 的 `CorrectedFromSpeakerID` 字段之后：

```go
	// CorrectedReason 自动纠正原因（000021 迁移加列）：'phantom'=幽灵历史声纹改判（配 CorrectedFromSpeakerID）；
	// 'short'=过短噪声段并入最近在场说话人（CorrectedFromSpeakerID 为 NULL）。nil=未纠正。
	// 与 speaker_id 一同被手动换人/整人改判/重新识别清空。
	CorrectedReason *string `db:"corrected_reason" json:"corrected_reason,omitempty"`
```

- [ ] **Step 6: `CorrectSegmentSpeaker` 补写 phantom** — 改其 SQL：

```go
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = ?, corrected_reason = 'phantom'
		 WHERE transcript_id = ? AND speaker_label = ? AND speaker_id = ?`,
		toID.Int64(), fromID.Int64(), transcriptID.Int64(), speakerLabel, fromID.Int64())
```

- [ ] **Step 7: 新增 `MergeShortGroup`** — 在 `CorrectSegmentSpeaker` 之后：

```go
// MergeShortGroup 过短噪声段并入（2026-08-28 需求）：把本 transcript 内某 speaker_label 下
// **尚未回填**（speaker_id IS NULL）的段整组并入目标在场说话人 toID，并标记 corrected_reason='short'
// （无原判定说话人，corrected_from_speaker_id 显式置 NULL）。这类组因总时长<3s 未登记独立声纹，
// 其段在 speaker stage pass2 被留 NULL、pass3 并入。带 transcript_id 作用域；单条 UPDATE 原子写。
func (r *TranscriptRepo) MergeShortGroup(ctx context.Context, transcriptID ids.ID, speakerLabel string, toID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_reason = 'short', corrected_from_speaker_id = NULL
		 WHERE transcript_id = ? AND speaker_label = ? AND speaker_id IS NULL`,
		toID.Int64(), transcriptID.Int64(), speakerLabel)
	return err
}
```

- [ ] **Step 8: 4 条清标记路径补 `corrected_reason = NULL`** — 在这 4 个方法的 SET 里，`corrected_from_speaker_id = NULL` 后追加 `, corrected_reason = NULL`：

`ClearSegmentSpeakers`:
```go
		`UPDATE transcript_segment SET speaker_id = NULL, corrected_from_speaker_id = NULL, corrected_reason = NULL WHERE transcript_id = ?`, transcriptID.Int64())
```
`SetSegmentSpeakerByID`:
```go
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = NULL, corrected_reason = NULL WHERE id = ? AND transcript_id = ?`,
		speakerID.Int64(), segID.Int64(), transcriptID.Int64())
```
`ReassignSpeakerSegments`:
```go
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = NULL, corrected_reason = NULL WHERE transcript_id = ? AND speaker_id = ?`,
		toID.Int64(), transcriptID.Int64(), fromID.Int64())
```
`ReassignSpeakerInTranscript`:
```go
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = NULL, corrected_reason = NULL WHERE transcript_id = ? AND speaker_id = ?`,
		toID.Int64(), transcriptID.Int64(), fromID.Int64())
```

- [ ] **Step 9: 跑测试确认通过** — `go test ./internal/repo/ -run TestMergeShortGroupAndReasonClearing -v` → PASS。（若报 `Unknown column corrected_reason`，DROP `zhiwei_test_repo` 重建后再跑。）

- [ ] **Step 10: 提交**

```bash
git add migrations/000021_segment_corrected_reason.up.sql migrations/000021_segment_corrected_reason.down.sql internal/repo/transcript.go internal/repo/transcript_test.go
git commit -m "feat(repo): corrected_reason 列(phantom/short) + MergeShortGroup + 清标记统一"
```

---

## Task 2: stage 过短并入 pass

**Files:**
- Modify: `internal/pipeline/stage_speaker.go`
- Test: `internal/pipeline/stage_speaker_test.go`

- [ ] **Step 1: 重构 seeder + 写失败测试** — 在 `internal/pipeline/stage_speaker_test.go`：

(1a) 把现有 `seedSpeakerStage` 重构为委托一个参数化 seeder，并**把默认 seq3 时长改为 ≥3s**（原 3800-4200=400ms 在新规则下会被判过短并入，破坏既有多说话人测试）。将 `func seedSpeakerStage(t *testing.T) (...)` 内部构造 `segs` 的部分替换为调用新函数，默认段改成：

```go
// seedSpeakerStageSegs 建 session+transcript+指定段并复制切片源 wav；返回 (sid, tr, dataDir, transcripts, speakers)。
// 供需要自定义段时长的测试（如过短并入）复用。
func seedSpeakerStageSegs(t *testing.T, segs []repo.TranscriptSegment) (ids.ID, *repo.Transcript, string, *repo.TranscriptRepo, *repo.SpeakerRepo) {
	t.Helper()
	requireFFmpeg(t)
	db, err := repo.NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}
	speakers := &repo.SpeakerRepo{DB: db}
	sid := ids.New()
	if err := sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "speech.wav",
		StoragePath: "../../testdata/speech.wav", Status: "processing",
	}); err != nil {
		t.Fatal(err)
	}
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	if err := transcripts.Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	for i := range segs {
		segs[i].TranscriptID = tr.ID
	}
	if err := transcripts.InsertSegments(ctx, segs); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	transcodedDir := filepath.Join(dataDir, "transcoded")
	if err := os.MkdirAll(transcodedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open("../../testdata/speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(transcodedDir, sid.String()+".wav"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	return sid, tr, dataDir, transcripts, speakers
}

// seedSpeakerStage 默认三段（seq1,2=label"1" 共 3.5s；seq3=label"2" 3.1s——两组都 ≥3s，
// 不触发过短并入，保持既有多说话人测试语义）。
func seedSpeakerStage(t *testing.T) (ids.ID, *repo.Transcript, string, *repo.TranscriptRepo, *repo.SpeakerRepo) {
	return seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "1", Text: "明天发邮件", StartMS: 0, EndMS: 2000},
		{SequenceNo: 2, SpeakerLabel: "1", Text: "确认会议", StartMS: 2100, EndMS: 3600},
		{SequenceNo: 3, SpeakerLabel: "2", Text: "好的", StartMS: 3800, EndMS: 6900},
	})
}
```

（注意：删除原 `seedSpeakerStage` 里内联的 db/session/transcript/segs/wav 构造，改为上面两函数；`io`/`os`/`filepath` imports 已在文件中。seq3 EndMS 3800→6900 使 label"2"=3100ms 非过短。）

(1b) 追加 3 个测试：

```go
// TestStageSpeakerMergesShortGroupIntoNearest 过短噪声组并入最近在场说话人：
// label"A"(seq1,2)=真人 4s → 空库登记新声纹；label"B"(seq3)=0.4s 噪声 → 不登记、并入 A、corrected_reason=short。
func TestStageSpeakerMergesShortGroupIntoNearest(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "正常说话一", StartMS: 0, EndMS: 2000},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "正常说话二", StartMS: 2100, EndMS: 4100},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "嗯。", StartMS: 4200, EndMS: 4600}, // 0.4s 噪声
	})
	vReal := make([]float32, 256)
	vReal[0] = 1
	vNoise := make([]float32, 256)
	vNoise[0] = 0.69
	vNoise[1] = float32(math.Sqrt(1 - 0.69*0.69)) // 与 vReal 余弦 0.69（示例数值）
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vReal, 2: vReal, 3: vNoise}} // 空历史库
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	bySeq := map[int]repo.TranscriptSegment{}
	for _, s := range segs {
		bySeq[s.SequenceNo] = s
	}
	realID := bySeq[1].SpeakerID
	if realID == nil {
		t.Fatal("真人组未回填")
	}
	seg3 := bySeq[3]
	if seg3.SpeakerID == nil || *seg3.SpeakerID != *realID {
		t.Fatalf("过短段应并入真人 %v，实际 %+v", *realID, seg3.SpeakerID)
	}
	if seg3.CorrectedReason == nil || *seg3.CorrectedReason != "short" {
		t.Fatalf("过短段应 corrected_reason=short，实际 %+v", seg3.CorrectedReason)
	}
	if seg3.CorrectedFromSpeakerID != nil {
		t.Fatalf("过短并入 corrected_from 应为 NULL，实际 %+v", seg3.CorrectedFromSpeakerID)
	}
	if len(fv.added) != 1 {
		t.Fatalf("过短组不应登记声纹，应只登记真人 1 个，实际 %d", len(fv.added))
	}
}

// TestStageSpeakerLongNewGroupStillRegisters ≥3s 新组照常登记、不并入、无 corrected_reason。
func TestStageSpeakerLongNewGroupStillRegisters(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "长段一", StartMS: 0, EndMS: 2000},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "长段二", StartMS: 2100, EndMS: 4100},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "也很长的一段独立说话", StartMS: 4200, EndMS: 7500}, // 3.3s
	})
	vA := make([]float32, 256)
	vA[0] = 1
	vB := make([]float32, 256)
	vB[1] = 1 // 与 A 正交，明显不同人
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vA, 3: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.CorrectedReason != nil {
			t.Fatalf("非过短组不应有 corrected_reason，seq%d=%+v", s.SequenceNo, s.CorrectedReason)
		}
	}
	if len(fv.added) != 2 {
		t.Fatalf("两个 ≥3s 新组应各登记 1 个，实际 %d", len(fv.added))
	}
}

// TestStageSpeakerAllShortFallbackRegisters 全部组过短 → 无并入目标 → 退回照常登记（有归属、无 short 标记）。
func TestStageSpeakerAllShortFallbackRegisters(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "嗯", StartMS: 0, EndMS: 500},
		{SequenceNo: 2, SpeakerLabel: "B", Text: "啊", StartMS: 600, EndMS: 1000},
	})
	vA := make([]float32, 256)
	vA[0] = 1
	vB := make([]float32, 256)
	vB[1] = 1
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SpeakerID == nil {
			t.Fatalf("全过短退回登记后每段应有归属，seq%d 仍 NULL", s.SequenceNo)
		}
		if s.CorrectedReason != nil {
			t.Fatalf("全过短退回登记不应打 short 标记，seq%d=%+v", s.SequenceNo, s.CorrectedReason)
		}
	}
	if len(fv.added) != 2 {
		t.Fatalf("全过短退回：两组各登记 1 个，实际 %d", len(fv.added))
	}
}
```

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/pipeline/ -run 'TestStageSpeakerMergesShort|TestStageSpeakerLongNew|TestStageSpeakerAllShort' -v` → `MergesShort` 断言失败（过短组仍被登记为独立说话人、无 short 标记）。

- [ ] **Step 3: `groupRep` 加 `durMS`** — 在 struct 里 `segVecs` 之后：

```go
	// durMS 组内 segVecs 段时长之和（ms）——过短并入判定：<minCleanSegMS 视为过短噪声组。
	durMS int64
```

在 `reps = append(reps, groupRep{...})` 处补算并赋值。把 append 前改为：

```go
		var durMS int64
		for _, sv := range svs {
			durMS += sv.seg.EndMS - sv.seg.StartMS
		}
		reps = append(reps, groupRep{
			label: label, rep: aggregateEmbeddings(vecs), vecN: len(vecs),
			clean:   pickCleanSegVec(svs, segs, label),
			segVecs: svs,
			durMS:   durMS,
		})
```

- [ ] **Step 4: pass2 —— `hasTarget` + `deferred`（过短组跳过登记）** — 把第二趟循环整块（`resolvedID := make(...)` 到该循环结束）替换为：

```go
	// 预判是否存在可作「过短并入」目标的组（命中历史库 or 非过短新组）。
	// 全部组都过短时不缓起——退回照常登记，保证段有归属、库不空。
	hasTarget := false
	for i, g := range reps {
		if matched[i] || g.durMS >= minCleanSegMS {
			hasTarget = true
			break
		}
	}
	// 第二趟：命中的复用；非过短未命中的登记新声纹；过短未命中的缓起(deferred)——不登记、段留 NULL，pass3 并入。
	resolvedID := make([]ids.ID, len(reps)) // 每组最终 speaker（deferred 组留零值，不作目标）
	deferred := make([]bool, len(reps))
	for i, g := range reps {
		if !matched[i] && hasTarget && g.durMS < minCleanSegMS {
			deferred[i] = true // 过短噪声组：不建 speaker/不入 FAISS，pass3 并入最近在场说话人
			continue
		}
		var speakerID ids.ID
		if matched[i] {
			speakerID = matchedID[i]
		} else {
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
```

- [ ] **Step 5: pass3 —— 共享 samples + 幽灵 + 过短并入** — 把原来的幽灵调用段（`margin := ...` 到 `return nil`）替换为：

```go
	// 3) 纠正 pass：先幽灵历史声纹纠正，再过短段并入。两者共享「各在场说话人样本向量」。
	samples := buildGroupSamples(ctx, d, reps, matched, resolvedID)
	margin := d.VoiceprintCorrectMargin
	if margin == 0 {
		margin = defaultCorrectMargin
	}
	if err := correctPhantomHistoricalMatches(ctx, d, tr, reps, matched, deferred, resolvedID, samples, margin); err != nil {
		return err
	}
	if err := mergeShortGroups(ctx, d, tr, reps, deferred, resolvedID, samples); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 6: 抽 `buildGroupSamples` + 改 `correctPhantomHistoricalMatches` 签名 + 新增 `mergeShortGroups`** — 在 `correctPhantomHistoricalMatches` 附近：

(6a) 新增 helper（供两趟共用）：

```go
// buildGroupSamples 为每组构造「该说话人的样本向量集合」（详情页同口径打分用）：
// 命中历史库 → 库内多条样本(回退聚合代表)；其余(新登记/deferred) → 登记向量(clean 优先，否则 rep)。
// deferred 组的样本不会被用作并入目标（调用方按 deferred 跳过），此处一并构造无害。
func buildGroupSamples(ctx context.Context, d StageDeps, reps []groupRep, matched []bool, resolvedID []ids.ID) [][][]float32 {
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
	return samples
}
```

(6b) 改 `correctPhantomHistoricalMatches`：签名加 `deferred []bool` 与 `samples [][][]float32`，删除其内部的 samples 构造块（改用传入的 `samples`），并在目标 `j` 循环里跳过 deferred 组。新签名与关键改动：

```go
func correctPhantomHistoricalMatches(ctx context.Context, d StageDeps, tr *repo.Transcript,
	reps []groupRep, matched, deferred []bool, resolvedID []ids.ID, samples [][][]float32, margin float64) error {
	if len(reps) < 2 {
		return nil
	}
	// （删除原内部 samples := make(...) 构造循环，改用参数 samples）
	type fix struct {
		label    string
		from, to ids.ID
	}
	var fixes []fix
	for i, g := range reps {
		if !matched[i] || len(g.segVecs) == 0 || len(samples[i]) == 0 {
			continue
		}
		self := 0.0
		for _, sv := range g.segVecs {
			if s := segMaxScore(sv.vec, samples[i]); s > self {
				self = s
			}
		}
		bestScore, bestJ := -1.0, -1
		for j := range reps {
			if j == i || deferred[j] || resolvedID[j] == resolvedID[i] {
				continue // 跳过自己、过短缓起组(无有效 speaker)、解析到同一 speaker 的组
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
		if bestJ >= 0 && bestScore > self+margin+correctScoreEps {
			fixes = append(fixes, fix{label: g.label, from: resolvedID[i], to: resolvedID[bestJ]})
		}
	}
	for _, f := range fixes {
		if err := d.Transcripts.CorrectSegmentSpeaker(ctx, tr.ID, f.label, f.from, f.to); err != nil {
			log.Printf("[speaker] 幽灵历史声纹纠正失败 label=%s from=%s to=%s: %v", f.label, f.from, f.to, err)
		}
	}
	return nil
}
```

(6c) 新增 `mergeShortGroups`：

```go
// mergeShortGroups 过短噪声段并入（2026-08-28 需求）：把 pass2 缓起(deferred)的过短组
// 整组并入本录音里最匹配的「非过短在场说话人」（max 余弦，详情页同口径），无阈值——噪声句总要
// 归给对话中某人。目标候选排除其他 deferred 组。best-effort：失败仅 log（段已在 pass2 留 NULL，
// 并入失败则维持 NULL，不致命）。hasTarget 已保证存在至少一个非 deferred 候选。
func mergeShortGroups(ctx context.Context, d StageDeps, tr *repo.Transcript,
	reps []groupRep, deferred []bool, resolvedID []ids.ID, samples [][][]float32) error {
	type fix struct {
		label string
		to    ids.ID
	}
	var fixes []fix
	for i, g := range reps {
		if !deferred[i] || len(g.segVecs) == 0 {
			continue
		}
		bestScore, bestJ := -1.0, -1
		for j := range reps {
			if j == i || deferred[j] || len(samples[j]) == 0 {
				continue // 目标须是非过短、有样本的在场说话人
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
		if bestJ >= 0 {
			fixes = append(fixes, fix{label: g.label, to: resolvedID[bestJ]})
		}
	}
	for _, f := range fixes {
		if err := d.Transcripts.MergeShortGroup(ctx, tr.ID, f.label, f.to); err != nil {
			log.Printf("[speaker] 过短段并入失败 label=%s to=%s: %v", f.label, f.to, err)
		}
	}
	return nil
}
```

- [ ] **Step 7: 跑新测试** — `go test ./internal/pipeline/ -run 'TestStageSpeakerMergesShort|TestStageSpeakerLongNew|TestStageSpeakerAllShort' -v` → 3 个 PASS。

- [ ] **Step 8: 全 pipeline 回归 + 修既有** — `go test ./internal/pipeline/ -v`。**预期既有 phantom / 首次多人 / 干净段 等测试仍全绿**（seq3 已改 ≥3s，无意外过短组）。若有测试因 seed 时长变化失败，核对其意图并把其中「本应是独立说话人」的组调回 ≥3s（不要改动测试意图，只修被 seed 时长影响的断言）。

- [ ] **Step 9: `go build ./...`** → clean。

- [ ] **Step 10: 提交**

```bash
git add internal/pipeline/stage_speaker.go internal/pipeline/stage_speaker_test.go
git commit -m "feat(pipeline): 过短噪声组并入最近在场说话人 pass（不建人+corrected_reason=short）"
```

---

## Task 3: 详情 API 下发 corrected_reason

**Files:**
- Modify: `internal/api/query.go`
- Test: `internal/api/query_test.go`

- [ ] **Step 1: 失败测试** — 追加到 `internal/api/query_test.go`（复用 `TestGetSessionCorrectedMarker` 的脚手架风格）：

```go
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
```

- [ ] **Step 2: 跑确认失败** — `go test ./internal/api/ -run TestGetSessionCorrectedReasonShort -v` → `corrected_reason` 空。

- [ ] **Step 3: `segmentView` 加字段** — 在 `CorrectedFromName` 之后：

```go
	// CorrectedReason 非空=该段被自动纠正；'phantom'=幽灵历史声纹改判(配 CorrectedFrom)；
	// 'short'=过短噪声段并入最近在场说话人(CorrectedFrom 为空)。前端据此渲染徽章 + tooltip。
	CorrectedReason string `json:"corrected_reason,omitempty"`
```

- [ ] **Step 4: `GetSession` 填充** — 在现有 `if sg.CorrectedFromSpeakerID != nil { ... }` 块之后（同一逐段循环内）加：

```go
			if sg.CorrectedReason != nil {
				views[i].CorrectedReason = *sg.CorrectedReason
			}
```

- [ ] **Step 5: 跑通过** — `go test ./internal/api/ -run TestGetSessionCorrectedReasonShort -v` → PASS。（必要时 DROP `zhiwei_test_api` 重建。）

- [ ] **Step 6: api 回归** — `go test ./internal/api/ -v`（含既有 `TestGetSessionCorrectedMarker`）→ PASS。

- [ ] **Step 7: 提交**

```bash
git add internal/api/query.go internal/api/query_test.go
git commit -m "feat(api): 详情段下发 corrected_reason(phantom/short)"
```

---

## Task 4: 前端徽章统一 + tooltip 分原因 + 段时长

**Files:**
- Modify: `web/index.html`

> 无自动化前端测试；静态核对 + `/run` 眼看。段对象由 `GET /api/sessions/{id}` 原样存入 `detail.segments`，`sg.corrected_reason` 直接可用（同 `sg.voice_matches`）。`web/index.html` 直接被服务（`app.js` 有指纹副本，本任务不改 app.js，故指纹不变）。

- [ ] **Step 1: 徽章触发统一 + tooltip 分原因** — 把现有徽章 `<span v-if="sg.corrected_from" class="corrected-badge" :title="...">已修改</span>` 替换为：

```html
            <span v-if="sg.corrected_reason || sg.corrected_from" class="corrected-badge"
                  :title="sg.corrected_reason === 'short' ? '过短段自动并入最近说话人（声纹自动纠正）' : (sg.corrected_from_name ? ('原判定：' + sg.corrected_from_name + '（声纹自动纠正）') : '声纹自动纠正（原说话人已不可用）')">已修改</span>
```

- [ ] **Step 2: 段时长显示（Part C）** — 在「声纹 ≈」那一行（`<div v-if="(sg.voice_matches||[]).length" ...>`）里，`<span>声纹 ≈</span>` **之前**插入时长 span：

```html
                <span>时长 {{ (segDurMs(sg)/1000).toFixed(2) }} s ·</span>
```

（同容器同灰度；`segDurMs(sg)` 前端已有，见「录入」按钮 title。）

- [ ] **Step 3: 静态核对** — `grep -n "corrected_reason\|时长 {{" web/index.html` 确认字段名与 API json 标签一致；确认未改指纹 js；HTML 标签配平。

- [ ] **Step 4: `/run` 眼看（建议）** — 打开一个含过短并入段的会话：过短段显示「已修改」徽章 + tooltip「过短段自动并入…」；每段「声纹 ≈」前显示「时长 X.XX s ·」；幽灵段徽章 tooltip 仍是「原判定：X」；手动换人后徽章消失。

- [ ] **Step 5: 提交**

```bash
git add web/index.html
git commit -m "feat(web): 徽章按 corrected_reason 统一触发+分原因 tooltip + 段时长显示"
```

---

## Self-Review 结论（作者已核对）

- **Spec 覆盖**：触发/不建人/并入(§4-5)→Task 2；标记统一+000021(§6)→Task 1；API(§7)→Task 3；前端+时长(§8,Part C)→Task 4；边界全过短(§9)→Task 2 `hasTarget` 退回登记 + 测试。
- **类型/命名一致**：`corrected_reason` 列、`CorrectedReason *string`(repo)/`string`(api json)、`MergeShortGroup`、`buildGroupSamples`、`mergeShortGroups`、`correctPhantomHistoricalMatches` 新签名(+deferred,+samples) 在各任务间统一。
- **关键正确性**：phantom 目标循环跳过 `deferred[j]`（deferred 组 resolvedID=0 不可作目标）；过短并入 best-effort；seed seq3 改 ≥3s 以免既有多说话人测试被新规则误伤；打分口径 `segMaxScore`（同详情页）。
- **无占位符**：各步含真实代码/命令/预期。
- **迁移**：000021（main 现最高 000020）；合 main 前若再撞号按当时最高重排。
