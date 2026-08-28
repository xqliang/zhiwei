# 逐段声纹改判（Per-Segment Voiceprint Reattribution）

- 日期：2026-08-28
- 分支：`feat/voiceprint-segment-reattribute`（基于 main `c6159d4`）
- 关联：扩展已合入 main 的声纹自动纠正体系（`corrected_reason` 列 + `corrected_from_speaker_id`；phantom 幽灵历史声纹、short 过短并入两趟）。本特性新增第三种纠正：段级。

## 1. 背景与问题

详情页某段（例「Yeah Yeah.」5.82s）归属 `说话人8glpi`，但其段级 top-3 声纹相似度为 `说话人k8ra7 0.80 · 铉晔 0.70 · 文生 0.65`——归属人 8glpi 甚至不在 top-3（对它的相似度 <0.65），而**另一个在场说话人 k8ra7 明显更像（0.80）**。UI 已有 hint（加粗 + tooltip「与他人相似度 ≥0.72：这句可能属于此人，可换人/切换声纹」），但需人工操作。本特性把该判据**自动化**。

现有两趟纠正（phantom / short）都是**组级**且各有特定触发，不覆盖「某一段的声纹明显更像另一个在场说话人」这种**段级**错分。

## 2. 目标

对每一段：若**另一个在场说话人**的段级声纹相似度 `≥ 0.72` 且比**当前归属说话人**的相似度**领先 ≥ 0.06**，则把该段自动改判给那个说话人并标记「已修改」。

## 3. 非目标

- 不改 phantom / short 两趟既有逻辑（本特性是并列的第三趟）。
- 不改判给**不在场**的库条目（历史库里没参与本录音的人，如 铉晔/文生，即便 top-N 露脸也不作候选）。
- 不新增迁移/API/前端改动（复用现有 `corrected_reason` 标记与详情 tooltip 分支）。

## 4. 触发与判定

阈值直接复用现有 `voiceprint.SoftMin = 0.72` 与 `voiceprint.GapMin = 0.06`（与 1:N 弱命中同参数，语义一致：明显更像且有区分度）。

对每一段 `seg`（用其**最终归属**后的 speaker_id = `S`）：
- `cur` = `segMaxScore(seg.embedding, samples[S])`（对 S 的多条样本取最大余弦，详情页同口径）
- 遍历**其他在场说话人** `Y`（≠S），`bestOther` = max `segMaxScore(seg.embedding, samples[Y])`，`bestID` = 取得 max 的 Y
- 若 `bestID != S` 且 `bestOther ≥ SoftMin` 且 `bestOther − cur ≥ GapMin` → 改判 `seg` 给 `bestID`。

> 实现注（与代码对齐）：领先判据实际用**严格大于 + float32 容差** `bestOther − cur > GapMin + correctScoreEps`（`correctScoreEps=1e-6`），与 pass3 幽灵纠正同一容差纪律——pass4 与 pass3 会对**同一段**重算同一 float32 内积并共享边界，若这里用 `≥` 会在「恰好 = GapMin」的浮点舍入（如 0.06000001）上悄悄推翻 pass3 的「严格大于才纠正」判定（`TestStageSpeakerCorrectionMarginBoundary` 即守护此点）。`voiceprint.Matched` 用 `≥` 无需容差，因其比较的是 sidecar 下发的距离、非本地重算。见 `internal/pipeline/stage_speaker.go`。

**在场说话人**：本录音各段当前非空 `speaker_id` 的去重集合。

## 5. 实现（新增 pass4，自包含）

在 `runSpeakerStage` 的 `mergeShortGroups` 之后、`return nil` 之前，调用 `correctSegmentsByVoiceprint(ctx, d, tr)`。该函数**自包含**（不依赖 pass1-3 的内存 reps/samples 模型，避免耦合已被组级纠正改动过的归属）：

1. `segs, _ := d.Transcripts.ListSegments(ctx, tr.ID)`（拿最终归属 + 逐段 embedding，pass1 已落库 `segEmbeds`）。
2. present 说话人 = `segs` 里非空 `speaker_id` 去重；对每个 `loadSpeakerSampleVecs(ctx, d, spID)` 建 `samples map[ids.ID][][]float32`（命中历史库→多样本；新登记→登记向量；缺失→聚合代表兜底）。
3. 逐段（有 embedding 且有归属）算 `cur` / `bestOther` / `bestID`；满足判据的收集为 `fix{segID, from:S, to:bestID}`。**先算完全部、再统一应用**（与 phantom/short 一致，避免链式影响本趟内其他段的判定基准）。
4. 逐条 `d.Transcripts.ReattributeSegmentByVoiceprint(ctx, tr.ID, segID, from, to)`；best-effort（失败仅 `log`，不致命——段已有归属）。

跳过：present 说话人 <2（无可比对象）、段无 embedding、段无归属（NULL）。归属人 `samples[S]` 为空时 `cur=0`（该段声纹不可取，若他人 ≥0.72 则允许改判——与「提向失败」同等降级）。

## 6. 标记与各层改动

- **无迁移**：复用现有 `transcript_segment.corrected_reason` + `corrected_from_speaker_id`。新增 reason 取值 **`'mismatch'`**（段级声纹与归属不符、改判到更像的在场说话人）；`corrected_from` = 改判前的说话人（前端 tooltip 显示「原判定：X」）。
- **repo**：新增 `ReattributeSegmentByVoiceprint(ctx, transcriptID, segID, fromID, toID ids.ID) error`：
  ```sql
  UPDATE transcript_segment SET speaker_id = ?, corrected_from_speaker_id = ?, corrected_reason = 'mismatch'
  WHERE id = ? AND transcript_id = ? AND speaker_id = ?
  ```
  args `toID, fromID, segID, transcriptID, fromID`。`AND speaker_id = fromID` 护栏：只在段仍归属改判前说话人时生效（防并发/重复改判）。清标记四条路径（换人/整人/重识别）已对**任意** `corrected_reason` 清空，无需改。
- **API**：无需改（`corrected_reason` 已下发；`corrected_from`/`corrected_from_name` 逻辑不变）。
- **前端**：无需改。现有 tooltip 分支 `reason==='short' ? 过短并入 : (corrected_from_name ? '原判定：X（声纹自动纠正）' : '声纹自动纠正（原说话人已不可用）')`——`'mismatch'` 落到「原判定：X」分支；改判前说话人在场，名字必经 `spMap` 解析。徽章按 `corrected_reason` 触发已覆盖。

## 7. 边界与取舍

- 只在整段（重）识别时跑：`runSpeakerStage` 在 `len(reps)==0`（幂等 reextract 全组已解析）时提前 return，pass4 不跑；reidentify 清空后重跑，pass4 覆盖全段。与 phantom/short 一致。
- 与 phantom/short 顺序：pass4 最后、最细粒度，基于最终归属再评估。组级已选「最近/最像」，段级通常找不到再领先 0.06 的他人，实测不来回翻；单趟不迭代。
- **对「信任 ASR 分组」的段级放宽**：仅当另一在场声纹明显更像（≥0.72 且领先 ≥0.06）才改，且只改到在场的人——有意、受控。
- present <2 或段无向量/归属人无样本 → 跳过或按降级处理（见 §5）。

## 8. 测试

- **stage**（有状态 fake `libVoiceprint`）：
  - `TestStageSpeakerReattributesSegmentToBetterPresentSpeaker`：两在场说话人 A、B；某段归属 A 但对 B 相似度 ≥0.72 且领先 ≥0.06 → 改判 B、`corrected_reason='mismatch'`、`corrected_from=A`。
  - `TestStageSpeakerReattributeKeepsWhenLeadBelowGap`：领先 <0.06 → 不动、无标记。
  - `TestStageSpeakerReattributeKeepsWhenBelowSoftMin`：best_other <0.72 → 不动。
- **repo**：`TestReattributeSegmentByVoiceprint`：写 `mismatch`+`corrected_from`；`AND speaker_id=fromID` 护栏（from 不匹配则不动）。
- 回归：既有 phantom/short/repo/api 测试全绿。

## 9. 涉及改动清单

1. `internal/repo/transcript.go`：新增 `ReattributeSegmentByVoiceprint`。
2. `internal/repo/transcript_test.go`：`TestReattributeSegmentByVoiceprint`。
3. `internal/pipeline/stage_speaker.go`：`correctSegmentsByVoiceprint` + 在 `runSpeakerStage` 末尾调用。
4. `internal/pipeline/stage_speaker_test.go`：3 个段级改判测试。
（无迁移、无 API、无前端。）
