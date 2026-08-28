# 逐段声纹改判 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 某段的段级声纹若明显更像**另一个在场说话人**（≥0.72 且比当前归属领先 ≥0.06），自动把该段改判给那个人并标记「已修改」(`corrected_reason='mismatch'`)。

**Architecture:** `runSpeakerStage` 在 phantom/short 两趟之后新增自包含的 pass4 `correctSegmentsByVoiceprint`：重列段（最终归属+逐段向量）→ 建在场说话人样本 → 逐段比 `cur`(对归属人) 与 `bestOther`(对其他在场人) → 满足判据的先算完再统一逐段改判。阈值复用 `voiceprint.SoftMin`/`GapMin`。

**Tech Stack:** Go（`internal/repo`、`internal/pipeline`）。**无迁移、无 API、无前端改动**——复用现有 `corrected_reason` 列 + 详情 tooltip 分支。测试用有状态 fake `libVoiceprint` + `repotest.DSN`。

**运行环境：** 分支 `feat/voiceprint-segment-reattribute`（基于 main `c6159d4`，已含 corrected_reason + phantom/short + SoftMin/GapMin）。测试 DSN：`export TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei?parseTime=true&charset=utf8mb4&multiStatements=true"`。stage 测试需 ffmpeg + `testdata/`（主 checkout 已有；此分支非 worktree，同一 checkout 直接可用）。编译验证用 `go build ./...`/`go vet ./...`，忽略 IDE 跨-worktree 假诊断。fake `libVoiceprint.Embed` 按段 SequenceNo 返回向量（不读真实音频），段 durMS 由 DB start/end 决定。

---

## File Structure

- `internal/repo/transcript.go` — 新增 `ReattributeSegmentByVoiceprint`。
- `internal/repo/transcript_test.go` — 新增 `TestReattributeSegmentByVoiceprint`。
- `internal/pipeline/stage_speaker.go` — 新增 `correctSegmentsByVoiceprint` + 在 `runSpeakerStage` 末尾调用。
- `internal/pipeline/stage_speaker_test.go` — 3 个段级改判测试。

---

## Task 1: repo 逐段改判方法

**Files:**
- Modify: `internal/repo/transcript.go`（新增 `ReattributeSegmentByVoiceprint`，放在 `SetSegmentSpeakerByID` 之后 / `MergeShortGroup` 附近）
- Test: `internal/repo/transcript_test.go`

- [ ] **Step 1: 失败测试** — 追加到 `internal/repo/transcript_test.go`：

```go
// TestReattributeSegmentByVoiceprint 逐段声纹改判：写 speaker_id=to + corrected_from=from + reason='mismatch'；
// AND speaker_id=fromID 护栏——from 不匹配当前归属则不动。
func TestReattributeSegmentByVoiceprint(t *testing.T) {
	db, err := NewDB(repotest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr := &TranscriptRepo{DB: db}
	speakers := &SpeakerRepo{DB: db}
	a := &Speaker{Name: "说话人A", Source: "auto"}
	b := &Speaker{Name: "说话人B", Source: "auto"}
	c := &Speaker{Name: "说话人C", Source: "auto"}
	for _, sp := range []*Speaker{a, b, c} {
		if err := speakers.Create(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, sp := range []*Speaker{a, b, c} {
			_ = speakers.Delete(context.Background(), sp.ID)
		}
	})
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
		{TranscriptID: tc.ID, SequenceNo: 1, SpeakerLabel: "A", Text: "一段", StartMS: 0, EndMS: 1000},
	}); err != nil {
		t.Fatal(err)
	}
	seg := mustSeg(t, tr, tc.ID, 1) // 复用本文件已有 helper（Task1-短并入引入）
	if err := tr.SetSegmentSpeaker(ctx, tc.ID, "A", a.ID); err != nil {
		t.Fatal(err)
	}

	// 护栏：from 传 c（非当前归属 a）→ 不动
	if err := tr.ReattributeSegmentByVoiceprint(ctx, tc.ID, seg.ID, c.ID, b.ID); err != nil {
		t.Fatalf("Reattribute(from=c): %v", err)
	}
	got := mustSeg(t, tr, tc.ID, 1)
	if got.SpeakerID == nil || *got.SpeakerID != a.ID || got.CorrectedReason != nil {
		t.Fatalf("from 不匹配应不动，实际 speaker=%+v reason=%+v", got.SpeakerID, got.CorrectedReason)
	}

	// 正常：from=a → 改判 b，reason=mismatch，corrected_from=a
	if err := tr.ReattributeSegmentByVoiceprint(ctx, tc.ID, seg.ID, a.ID, b.ID); err != nil {
		t.Fatalf("Reattribute(from=a): %v", err)
	}
	got = mustSeg(t, tr, tc.ID, 1)
	if got.SpeakerID == nil || *got.SpeakerID != b.ID {
		t.Fatalf("应改判给 b，实际 %+v", got.SpeakerID)
	}
	if got.CorrectedReason == nil || *got.CorrectedReason != "mismatch" {
		t.Fatalf("应 corrected_reason=mismatch，实际 %+v", got.CorrectedReason)
	}
	if got.CorrectedFromSpeakerID == nil || *got.CorrectedFromSpeakerID != a.ID {
		t.Fatalf("应 corrected_from=a，实际 %+v", got.CorrectedFromSpeakerID)
	}

	// 手动换人清标记（复用既有清理路径）
	if err := tr.SetSegmentSpeakerByID(ctx, tc.ID, seg.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	got = mustSeg(t, tr, tc.ID, 1)
	if got.CorrectedReason != nil || got.CorrectedFromSpeakerID != nil {
		t.Fatalf("手动换人后应清 mismatch 标记，实际 reason=%+v from=%+v", got.CorrectedReason, got.CorrectedFromSpeakerID)
	}
}
```

（`mustSeg` helper 已在 `transcript_test.go`（短并入任务引入）。若不存在则加：
```go
func mustSeg(t *testing.T, tr *TranscriptRepo, transcriptID ids.ID, seq int) TranscriptSegment {
	t.Helper()
	segs, err := tr.ListSegments(context.Background(), transcriptID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range segs {
		if s.SequenceNo == seq {
			return s
		}
	}
	t.Fatalf("seg seq %d 未找到", seq)
	return TranscriptSegment{}
}
```
）

- [ ] **Step 2: 跑确认失败** — `go test ./internal/repo/ -run TestReattributeSegmentByVoiceprint -v` → 编译错误（`ReattributeSegmentByVoiceprint` undefined）。

- [ ] **Step 3: 实现方法** — 在 `internal/repo/transcript.go` 的 `SetSegmentSpeakerByID` 之后加：

```go
// ReattributeSegmentByVoiceprint 逐段声纹改判（2026-08-28 需求）：某段的段级声纹明显更像另一个
// 在场说话人（≥SoftMin 且领先当前归属 ≥GapMin）时，把该单段从 fromID 改判给 toID，标记
// corrected_reason='mismatch' + corrected_from_speaker_id=fromID（前端「已修改」徽章 tooltip「原判定：X」）。
// `AND speaker_id = fromID` 护栏：仅在段仍归属改判前说话人时生效（防并发/重复改判误写）；带 transcript_id 作用域。
func (r *TranscriptRepo) ReattributeSegmentByVoiceprint(ctx context.Context, transcriptID, segID, fromID, toID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = ?, corrected_reason = 'mismatch'
		 WHERE id = ? AND transcript_id = ? AND speaker_id = ?`,
		toID.Int64(), fromID.Int64(), segID.Int64(), transcriptID.Int64(), fromID.Int64())
	return err
}
```

- [ ] **Step 4: 跑通过** — `go test ./internal/repo/ -run TestReattributeSegmentByVoiceprint -v` → PASS（`Unknown column` 则 DROP `zhiwei_test_repo` 重建）。`go build ./...` clean。

- [ ] **Step 5: 提交**

```bash
git add internal/repo/transcript.go internal/repo/transcript_test.go
git commit -m "feat(repo): ReattributeSegmentByVoiceprint 逐段声纹改判(mismatch+from 护栏)"
```

---

## Task 2: stage pass4 逐段改判

**Files:**
- Modify: `internal/pipeline/stage_speaker.go`（新增 `correctSegmentsByVoiceprint` + `runSpeakerStage` 末尾调用）
- Test: `internal/pipeline/stage_speaker_test.go`

- [ ] **Step 1: 写 3 个失败测试** — 追加到 `internal/pipeline/stage_speaker_test.go`。三例共用几何：`vA=e0`、`vB=e1`(正交)、label"A" 由干净段 seq1(=vA,3.5s) 登记为 A、label"B" seq3(=vB,3.2s) 登记为 B；被测段 seq2(label"A",0.4s，向量按用例设定) 归属 A 但更像 B。

```go
// mkVec 造单位向量：indices/vals 指定分量（其余 0）。用于段级改判用例的相似度几何。
func mkVec(pairs ...[2]float64) []float32 {
	v := make([]float32, 256)
	for _, p := range pairs {
		v[int(p[0])] = float32(p[1])
	}
	return v
}

// TestStageSpeakerReattributesSegmentToBetterPresentSpeaker 段级改判：seq2 归属 A 但对在场 B 相似 0.85、
// 对 A 仅 0.40（领先 0.45≥0.06 且 0.85≥0.72）→ 改判 B，corrected_reason=mismatch，corrected_from=A。
func TestStageSpeakerReattributesSegmentToBetterPresentSpeaker(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "A 的干净长段", StartMS: 0, EndMS: 3500},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "其实更像 B 的一段", StartMS: 3600, EndMS: 4000},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "B 的干净长段", StartMS: 5000, EndMS: 8200},
	})
	vA := mkVec([2]float64{0, 1})
	vB := mkVec([2]float64{1, 1})
	// seq2：cos 到 vA(e0)=0.40、到 vB(e1)=0.85，z 补足单位长
	vMix := mkVec([2]float64{0, 0.40}, [2]float64{1, 0.85}, [2]float64{2, math.Sqrt(1 - 0.40*0.40 - 0.85*0.85)})
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vMix, 3: vB}} // 空历史库
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	bySeq := map[int]repo.TranscriptSegment{}
	for _, s := range segs {
		bySeq[s.SequenceNo] = s
	}
	aID, bID := bySeq[1].SpeakerID, bySeq[3].SpeakerID
	if aID == nil || bID == nil {
		t.Fatal("A/B 未回填")
	}
	seg2 := bySeq[2]
	if seg2.SpeakerID == nil || *seg2.SpeakerID != *bID {
		t.Fatalf("seq2 应改判给 B %v，实际 %+v", *bID, seg2.SpeakerID)
	}
	if seg2.CorrectedReason == nil || *seg2.CorrectedReason != "mismatch" {
		t.Fatalf("seq2 应 corrected_reason=mismatch，实际 %+v", seg2.CorrectedReason)
	}
	if seg2.CorrectedFromSpeakerID == nil || *seg2.CorrectedFromSpeakerID != *aID {
		t.Fatalf("seq2 应 corrected_from=A %v，实际 %+v", *aID, seg2.CorrectedFromSpeakerID)
	}
	// seq1/seq3 不动
	if bySeq[1].CorrectedReason != nil || bySeq[3].CorrectedReason != nil {
		t.Fatalf("seq1/seq3 不应被改判")
	}
}

// TestStageSpeakerReattributeKeepsWhenLeadBelowGap best_other≥0.72 但领先<0.06 → 不动。
func TestStageSpeakerReattributeKeepsWhenLeadBelowGap(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "A 的干净长段", StartMS: 0, EndMS: 3500},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "临界段", StartMS: 3600, EndMS: 4000},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "B 的干净长段", StartMS: 5000, EndMS: 8200},
	})
	vA := mkVec([2]float64{0, 1})
	vB := mkVec([2]float64{1, 1})
	// cos 到 A=0.68、到 B=0.73（领先 0.05<0.06；0.73≥0.72）
	vMix := mkVec([2]float64{0, 0.68}, [2]float64{1, 0.73}, [2]float64{2, math.Sqrt(1 - 0.68*0.68 - 0.73*0.73)})
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vMix, 3: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo == 2 && s.CorrectedReason != nil {
			t.Fatalf("领先<0.06 不应改判，实际 %+v", s.CorrectedReason)
		}
	}
}

// TestStageSpeakerReattributeKeepsWhenBelowSoftMin best_other<0.72 → 不动。
func TestStageSpeakerReattributeKeepsWhenBelowSoftMin(t *testing.T) {
	ctx := context.Background()
	sid, tr, dataDir, transcripts, speakers := seedSpeakerStageSegs(t, []repo.TranscriptSegment{
		{SequenceNo: 1, SpeakerLabel: "A", Text: "A 的干净长段", StartMS: 0, EndMS: 3500},
		{SequenceNo: 2, SpeakerLabel: "A", Text: "都不太像的段", StartMS: 3600, EndMS: 4000},
		{SequenceNo: 3, SpeakerLabel: "B", Text: "B 的干净长段", StartMS: 5000, EndMS: 8200},
	})
	vA := mkVec([2]float64{0, 1})
	vB := mkVec([2]float64{1, 1})
	// cos 到 A=0.50、到 B=0.70（best_other 0.70<0.72）
	vMix := mkVec([2]float64{0, 0.50}, [2]float64{1, 0.70}, [2]float64{2, math.Sqrt(1 - 0.50*0.50 - 0.70*0.70)})
	fv := &libVoiceprint{vecBySeq: map[int][]float32{1: vA, 2: vMix, 3: vB}}
	d := StageDeps{Transcripts: transcripts, Speakers: speakers, Voiceprint: fv, DataDir: dataDir}
	if err := runSpeakerStage(ctx, d, sid, tr); err != nil {
		t.Fatalf("stage: %v", err)
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SequenceNo == 2 && s.CorrectedReason != nil {
			t.Fatalf("best_other<0.72 不应改判，实际 %+v", s.CorrectedReason)
		}
	}
}
```

- [ ] **Step 2: 跑确认失败** — `go test ./internal/pipeline/ -run 'TestStageSpeakerReattribute' -v` → `...ToBetterPresentSpeaker` 失败（seq2 未改判/无 mismatch）；两个负例此时也「通过」（尚无 pass4，本就不改），实现后须仍通过。

- [ ] **Step 3: `runSpeakerStage` 末尾调用 pass4** — 把 `mergeShortGroups(...)` 调用之后的 `return nil` 段（当前 stage_speaker.go 约 206-209 行）改为：

```go
	if err := mergeShortGroups(ctx, d, tr, reps, deferred, resolvedID, samples); err != nil {
		return err
	}
	// 4) 逐段声纹改判：某段声纹明显更像另一在场说话人时改判该段（见 correctSegmentsByVoiceprint）。
	if err := correctSegmentsByVoiceprint(ctx, d, tr); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 4: 实现 `correctSegmentsByVoiceprint`** — 加到 `internal/pipeline/stage_speaker.go`（`mergeShortGroups` 附近，其他 pass 函数之后）。用现有 `loadSpeakerSampleVecs`、`segMaxScore`、`decodeEmbedding`、`voiceprint.SoftMin/GapMin`（`voiceprint` 已 import）：

```go
// correctSegmentsByVoiceprint 逐段声纹改判（2026-08-28 需求）：ASR 分组内某段的段级声纹若明显更像
// 另一个**在场说话人**（相似度 ≥ voiceprint.SoftMin 且比当前归属领先 ≥ voiceprint.GapMin，与 1:N
// 弱命中同判据），把该单段改判给那个人（corrected_reason='mismatch'）。自包含：重列段拿最终归属+
// 逐段向量，不依赖 pass1-3 的内存模型（归属已被 phantom/short 改动过）。候选仅在场说话人（本录音各段
// 非空 speaker_id 去重）——不改判给历史库里不在场的人。先算完全部再统一应用（避免本趟内相互影响判据）。
func correctSegmentsByVoiceprint(ctx context.Context, d StageDeps, tr *repo.Transcript) error {
	segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return fmt.Errorf("逐段改判读 segments: %w", err)
	}
	// 在场说话人样本向量（按 speaker id 去重加载；命中历史库→多样本，新登记→登记向量，缺失→聚合代表）
	samples := map[ids.ID][][]float32{}
	for _, s := range segs {
		if s.SpeakerID != nil {
			if _, ok := samples[*s.SpeakerID]; !ok {
				samples[*s.SpeakerID] = loadSpeakerSampleVecs(ctx, d, *s.SpeakerID)
			}
		}
	}
	if len(samples) < 2 {
		return nil // 少于两个在场说话人，无可比对象
	}
	type fix struct {
		segID, from, to ids.ID
	}
	var fixes []fix
	for _, s := range segs {
		if s.SpeakerID == nil || len(s.Embedding) == 0 {
			continue // 未归属 / 无逐段向量的段跳过
		}
		vec, ok := decodeEmbedding(s.Embedding)
		if !ok || len(vec) != 256 {
			continue
		}
		assigned := *s.SpeakerID
		cur := segMaxScore(vec, samples[assigned]) // 归属人无样本时为 0（声纹不可取，降级）
		bestOther, bestID, hasBest := 0.0, ids.ID(0), false
		for spID, sv := range samples {
			if spID == assigned {
				continue
			}
			if sc := segMaxScore(vec, sv); !hasBest || sc > bestOther {
				bestOther, bestID, hasBest = sc, spID, true
			}
		}
		if hasBest && bestOther >= voiceprint.SoftMin && bestOther-cur >= voiceprint.GapMin {
			fixes = append(fixes, fix{segID: s.ID, from: assigned, to: bestID})
		}
	}
	for _, f := range fixes {
		if err := d.Transcripts.ReattributeSegmentByVoiceprint(ctx, tr.ID, f.segID, f.from, f.to); err != nil {
			// best-effort：改判失败仅 log 不致命（段已有归属；与 phantom/short 一致，不返回错误以免 job 重试却无法重跑）
			log.Printf("[speaker] 逐段声纹改判失败 seg=%s from=%s to=%s: %v", f.segID, f.from, f.to, err)
		}
	}
	return nil
}
```

- [ ] **Step 5: 跑 3 测试 + 全 pipeline 回归** — 
`go test ./internal/pipeline/ -run 'TestStageSpeakerReattribute' -v` → 3 PASS；
`go test ./internal/pipeline/ -count=1` → 全绿（既有 phantom/short/first-multi/clean-seg 等不受影响：那些用例里没有「某段更像另一在场人且领先 ≥0.06」的构造）。

- [ ] **Step 6: `go build ./...`** clean。

- [ ] **Step 7: 提交**

```bash
git add internal/pipeline/stage_speaker.go internal/pipeline/stage_speaker_test.go
git commit -m "feat(pipeline): pass4 逐段声纹改判（更像在场他人则改判该段，mismatch）"
```

---

## Self-Review 结论（作者已核对）

- **Spec 覆盖**：判据(§4)→Task 2 pass4；实现(§5)→`correctSegmentsByVoiceprint` 自包含重列段；标记 mismatch+from(§6)→Task 1 repo 方法；无迁移/API/前端(§3,6)→计划未含这些改动；边界(§7)→present<2 / 无向量 / 无样本降级 + best-effort。
- **类型/命名一致**：`ReattributeSegmentByVoiceprint`(repo)、`correctSegmentsByVoiceprint`(stage)、reason 值 `'mismatch'`、复用 `voiceprint.SoftMin/GapMin`、`loadSpeakerSampleVecs`/`segMaxScore`/`decodeEmbedding`（均为 stage 现有）在两任务间统一。
- **关键正确性**：`ReattributeSegmentByVoiceprint` 带 `AND speaker_id=fromID` 护栏；pass4 先算完 fixes 再应用；候选仅在场说话人；阈值同 1:N 弱命中。
- **前端无改**：`'mismatch'` 落到详情 tooltip 现有「原判定：X」分支（改判前说话人在场，`spMap` 必能解析名字），徽章按 `corrected_reason` 触发已覆盖。
- **无占位符**：各步含真实代码/命令/预期。
