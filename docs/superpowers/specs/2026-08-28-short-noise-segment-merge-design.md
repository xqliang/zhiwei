# 过短噪声段自动并入 + 段时长显示（Short-Segment Merge + Duration Display）

- 日期：2026-08-28
- 分支：`feat/voiceprint-short-merge`（worktree `.claude/worktrees/voiceprint-short-merge`，基于 main `459e364`）
- 关联：本特性扩展已合入 main 的「幽灵历史声纹纠正」（`corrected_from_speaker_id` + `correctPhantomHistoricalMatches`，见 `2026-08-27-phantom-historical-voiceprint-correction-design.md`）
- 相关代码：`internal/pipeline/stage_speaker.go`、`internal/repo/transcript.go`、`internal/api/query.go`、`web/index.html`

## 1. 背景与问题（Badcase）

同一条录音里，ASR diarization 把一句 ~1.5s 的「嗯。」（实为噪声/语气词）单独切成了第 3 个说话人 `说话人1trdl`。当前 speaker stage 会为它自动登记一个新声纹（因段 <3s 无干净段，走 `g.rep` 聚合兜底登记），于是详情页显示「声纹 ≈ 说话人1trdl 1.00」——它匹配自己那条从噪声建出来的声纹。这条噪声声纹既不该成为独立说话人，也污染跨 session 的 1:N 库（下次录音可能误命中）。

已合入的「幽灵历史声纹纠正」只处理**命中历史库**的组；`说话人1trdl` 是**本次新登记**的组，被那趟明确排除。故需新增对「过短新组」的处理。

段级 top-3 相似度（详情页）：`说话人1trdl 1.00 · 说话人rmxl3 0.69 · 铉晔 0.65`——排除它自己后，最近的在场说话人是 `说话人rmxl3` 0.69。

## 2. 目标

- **A. 过短噪声段并入最近在场说话人**：一个未命中历史库、总时长 <3s 的新组，不再登记为独立说话人，而是把它的段整组改判给本录音里最匹配的在场说话人，并标记「已修改」。
- **B. 标记统一**：两类自动纠正（幽灵历史声纹 / 过短并入）用统一的 `corrected_reason` 标记，前端徽章统一触发。
- **C. 段时长显示**：详情页每段在「声纹 ≈」行前显示「时长 X.XX s」。

## 3. 非目标

- 不改动「幽灵历史声纹纠正」的既有触发逻辑（只在其写入路径上补写 `corrected_reason='phantom'`）。
- 不把过短组的噪声声纹加入 1:N 库（明确不登记）。
- 不处理「过短但命中了历史库」的组——它命中了真人即复用真人，不属于本特性。

## 4. 触发与判定

- **过短组**：一个 ASR 组，**未命中历史库**（`matched[i]==false`）且其**总时长 < `minCleanSegMS`（3000ms）**。总时长 = 组内各有效段（已成功提向、进入 `segVecs`）的 `end_ms-start_ms` 之和。
- 命中历史库的组不参与（命中即真人复用）；总时长 ≥3s 的新组照常登记（不并入）。

## 5. 处理流程（改 `runSpeakerStage`）

### 5.1 Pass 1（切片提向聚合）——补记时长
`groupRep` 增 `durMS int64`（组内 `segVecs` 段时长之和），构建 `reps` 时算出。

### 5.2 Pass 2（登记/回填）——过短组「不建人」
第二趟对未命中组登记新声纹前，先判 `durMS < minCleanSegMS`：
- **是（过短组）**：**跳过登记**——不 `Speakers.Create`、不 `Voiceprint.Add`、不落样本行、不 `SetSegmentSpeaker`（段留 NULL）。记 `deferred[i]=true`，`resolvedID[i]` 留零值（无效）。
- **否**：照现有逻辑登记新声纹并回填。
- 命中组照现有逻辑复用。

段级声纹向量 `segEmbeds` 仍照常落库（过短段也保留，供详情页「声纹 ≈」+ 时长显示）。

### 5.3 Pass 3（纠正 pass）——先幽灵、后过短并入
在现有 `correctPhantomHistoricalMatches` 之后，新增过短并入逻辑（可放同一函数内的第二段，或新函数 `mergeShortGroups`）：

对每个 `deferred[i]`（过短组）：
- 候选目标 = 所有**非 deferred** 的组 j（命中历史库的 + 本次已登记的新人；**排除其他过短组**）。它们的样本向量集合 `samples[j]` 复用现有构造（命中→`loadSpeakerSampleVecs`；新登记→登记向量 clean/rep）。
- `score_j` = max over（过短组 i 的 `segVecs`）of `segMaxScore(seg, samples[j])`（详情页同口径 max 余弦）。
- 取 `argmax_j score_j` = 目标 T。**无阈值**：噪声句总要归给对话中某人。
- 用新 repo 方法把组 i 的 label 段改判给 T 并标记：`speaker_id=T, corrected_reason='short', corrected_from_speaker_id=NULL`（仅 `speaker_id IS NULL` 的段，即本组未回填的段）。
- **边界**：若无任何非 deferred 候选（全部组都过短）→ 退回照常登记组 i（`Speakers.Create`+`Add`+回填），并 `log`，保证段有归属、库不至于空。

先算全部并入判定再统一应用（与幽灵纠正一致，避免相互影响）。

## 6. 数据模型与标记（迁移 000021）

`transcript_segment` 加列 `corrected_reason VARCHAR(16) NULL`：

```sql
-- up (000021)
ALTER TABLE transcript_segment
  ADD COLUMN corrected_reason VARCHAR(16) NULL COMMENT
  '自动纠正原因：phantom=幽灵历史声纹改判(配 corrected_from_speaker_id) | short=过短噪声段并入最近在场说话人(corrected_from 为 NULL)；NULL=未纠正';
-- down
ALTER TABLE transcript_segment DROP COLUMN corrected_reason;
```

- `repo.TranscriptSegment` 增 `CorrectedReason *string`（`db:"corrected_reason" json:"corrected_reason,omitempty"`）。
- **幽灵路径**：`CorrectSegmentSpeaker` 的 UPDATE 顺带 `corrected_reason='phantom'`（统一）。
- **过短路径**：新方法 `MergeShortGroup(ctx, transcriptID, label, toID)`：
  ```sql
  UPDATE transcript_segment SET speaker_id=?, corrected_reason='short', corrected_from_speaker_id=NULL
  WHERE transcript_id=? AND speaker_label=? AND speaker_id IS NULL
  ```
  （仅改本组未回填的段；`corrected_from` 显式置 NULL 表示无原判定说话人。）
- **清标记**：`SetSegmentSpeakerByID`、`ClearSegmentSpeakers`、`ReassignSpeakerSegments`、`ReassignSpeakerInTranscript` 的 SET 追加 `corrected_reason = NULL`（与现有 `corrected_from_speaker_id = NULL` 并列）。

> ⚠️ 迁移号：main 现有最高 000020（000017 本组、000018 person_pet、000019 agent_config、000020 conversation_title），故本特性取 **000021**。

## 7. API（`internal/api/query.go`）

- `segmentView` 增 `CorrectedReason string json:"corrected_reason,omitempty"`（由 `*string` 取值）。
- `GetSession`：填充 `corrected_reason`；`corrected_from`/`corrected_from_name` 逻辑不变（phantom 才有）。

## 8. 前端（`web/index.html`）

- **徽章触发统一**：`v-if="sg.corrected_reason || sg.corrected_from"`（兼容旧 phantom 行）。
- **tooltip 按原因**：
  - `short` → 「过短段自动并入最近说话人（声纹自动纠正）」
  - 否则（phantom）→ 沿用「原判定：<corrected_from_name>（声纹自动纠正）」，名空时退回「声纹自动纠正（原说话人已不可用）」。
- **段时长（Part C）**：在「声纹 ≈」行最前面加「时长 {{ (segDurMs(sg)/1000).toFixed(2) }} s ·」，同灰度样式。仅在该行（`v-if voice_matches.length`）出现——与现状一致，存量无逐段向量的会话不显示。

## 9. 边界与取舍

- 全部组过短 → 退回照常登记（不并入），保证有归属。
- 过短并入无阈值：极低相似度也并给最近在场说话人（噪声句本就无「正确」归属，跟最近的走）。
- 过短组的段仍保留逐段向量与「声纹 ≈」展示（含它自己那条 1.00 的段向量——但不建 speaker、不入库，故不会作为库条目被他人命中）。
- 迁移 000021 若与其他未合分支再撞号，合 main 前按当时 main 最高号重排。

## 10. 测试

- **stage**（有状态 fake `libVoiceprint`）：
  - `TestStageSpeakerMergesShortGroupIntoNearest`：3 组，其一为 1.5s 噪声新组，最近在场说话人胜出 → 段改判给它、`corrected_reason='short'`、**未** `Speakers.Create` 该组、**未** `Voiceprint.Add`（`fv.added` 不含它）。
  - `TestStageSpeakerLongNewGroupStillRegisters`：≥3s 新组照常登记、不并入、无 `corrected_reason`。
  - `TestStageSpeakerAllShortFallbackRegisters`：全部组过短 → 退回登记、有归属、无 `short` 标记。
  - 回归：既有 phantom 测试仍绿；phantom 改判后 `corrected_reason='phantom'`。
- **repo**：`MergeShortGroup` 写 `short`+置空 corrected_from；`CorrectSegmentSpeaker` 写 `phantom`；四条清标记路径清 `corrected_reason`。
- **api**：`GetSession` 返回 `corrected_reason`（short/phantom）。
- **前端**：无自动化测试，静态核对 + `/run` 眼看（徽章、tooltip、时长）。

## 11. 涉及改动清单

1. `migrations/000021_segment_corrected_reason.{up,down}.sql`
2. `internal/repo/transcript.go`：`CorrectedReason` 字段；`MergeShortGroup`；`CorrectSegmentSpeaker` 补 `phantom`；四条清标记路径补 `corrected_reason=NULL`。
3. `internal/pipeline/stage_speaker.go`：`groupRep.durMS`；pass 2 过短组跳过登记；pass 3 过短并入（含全过短退回登记的边界）。
4. `internal/api/query.go`：`segmentView.CorrectedReason` + 填充。
5. `web/index.html`：徽章触发统一 + tooltip 分原因 + 段时长显示。
6. 对应测试。
