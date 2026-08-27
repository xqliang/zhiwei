# 幽灵历史声纹纠正（Phantom Historical-Voiceprint Correction）

- 日期：2026-08-27
- 分支：`feat/voiceprint-phantom-correction`
- 相关代码：`internal/pipeline/stage_speaker.go`、`internal/api/query.go`、`internal/api/speaker.go`、`internal/repo/transcript.go`、`web/app.js`、`web/index.html`

## 1. 背景与问题（Badcase）

ASR 原生 diarization 偶尔会**过度切分**：一段实际只有 2 个人的录音被切成 3 个说话人标签。多出来的那个「幽灵」标签，其组代表声纹在跨 session 1:N 检索里**命中了历史库里某个真实的人**（例：铉晔），于是被错误地归到那个人名下。

实测 badcase（详情页每段的段级 top-3 余弦，「声纹 ≈ 名字 分数」）：

| 段 | 文本 | 铉晔 | 说话人iux5x | 文生 |
|----|------|------|------------|------|
| A | 其实的话就是我们家的那个酱油。 | 0.64 | 0.61 | 0.63 |
| B | 我试一下它多有多神 | 0.73 | **0.88** | 0.71 |
| C | 在这个 DJ 工作室里面 | 0.73 | 0.72 | 0.67 |

铉晔（历史库声纹）名下这 3 段，其中段 B 上说话人 iux5x（本次录音新建的声纹）匹配得明显更好（0.88 vs 铉晔 0.73）。实际这 3 段都应属于 iux5x，铉晔是过度切分产生的幽灵。

现有设计原则是「信任 ASR diarization，本 session 内不同标签一律视为不同人，跨 session 才靠 1:N 归并」。本特性是对该原则一个**窄口径的、有护栏的例外**：仅当一个组「命中了历史库」却「在自己名下的段上被另一个在场说话人匹配得更好」时，才判定为幽灵并改判。

## 2. 目标

- 在说话人识别阶段自动识别并纠正这类「幽灵历史声纹」——把幽灵组的全部段整组改判给真正解释它的在场说话人（可以是本次录音新建的声纹）。
- 被自动改判的转写条目持久化一个「已修改」标记，前端渲染徽章，方便人工复核/手动改回。

## 3. 非目标

- 不做通用的「本地按声纹合并任意两个 ASR 标签」——只处理「历史库命中且在自己段上被超过」这一窄情形。
- 不删除、不修改被顶掉的历史说话人（铉晔）的名册与声纹样本（它是别的会话的真实声纹）。
- 不把被改判的段追加进目标说话人（iux5x）的声纹样本——纯改归属，FAISS 索引与样本行都不动。

## 4. 术语与打分口径

- **在场说话人**：本次录音里被解析出的说话人（= 本次 speaker stage 处理的各 ASR 标签组各自解析到的 speaker）。**不含**仅在段级 top-N 里露脸但本会话并不说话的库条目（如上表的「文生」）。
- **段对某说话人的相似度**：复用**详情页同款打分**——对该说话人已登记的**多条样本取最大余弦**（与 `internal/api/query.go` 的 `topVoiceMatchesVec` / `libraryWithEntries` 一致）。这样保证「徽章触发 ⇔ 屏幕上看到的数字支持它」，不会出现算法说改、但可见分数对不上的情况。

## 5. 算法（在 `runSpeakerStage` 末尾新增一趟「纠正 pass」）

在现有「① 逐组切片提向聚合 → ② 全部组先检索、再统一登记未命中的 → 回填 speaker_id」之后，对**本次处理的全部组**做一趟自包含纠正。

只有**命中了历史库声纹**的组 `H` 参与（在现有代码里即 `matched[i]==true`）。自动新建的组（iux5x/ul8zb）不参与——它们的声纹就是从自己的段建出来的，天生在自己段上最高，不可能被判为幽灵。

对每个候选组 `H`：

1. `self_H` = **max** over（H 名下各段）的「该段对历史声纹 H 的相似度」。
   - 例：铉晔 = `max(0.64, 0.73, 0.73)` = **0.73**。
2. 对每个**在场的其他说话人** `Y`（本次其他组解析到的 speaker，含新建的 iux5x/ul8zb）：
   `score_Y` = **max** over（H 名下各段）对 Y 的相似度。
   - 例：iux5x = `max(0.61, 0.88, 0.72)` = **0.88**。
3. 令 `bestY = argmax_Y score_Y`。若 `score_bestY > self_H + margin` → 判定 H 为幽灵：把 H 名下**全部段整组改判**给 `bestY`，这些段写 `corrected_from_speaker_id = H`。
   - 例：`0.88 > 0.73 + 0.06` → 触发，改判给 iux5x。

**margin**：默认 `0.06`（沿用现有 `voiceprint.GapMin` 的经验值），可配 `ZW_VOICEPRINT_CORRECT_MARGIN`。用 max 口径后本 badcase 的差距是 0.15，远大于 margin，稳稳触发；同时 margin 能挡住接近平局时的噪声翻转。

**语义**：只要有某个在场的其他人，在 H 的某个段上比 H 自己在任何段上都匹配得更好，就说明 H 是被 ASR 过度切分出来的幽灵，把它并回那个人。

**一致性护栏**：先对所有候选组算完全部判定（基于本趟开始时的归属快照），再统一应用改判——避免链式改判或两个历史组互相改判导致的抖动。每个 H 独立评估，不迭代、不级联。

### 5.1 打分所需数据在阶段内如何获得

`runSpeakerStage` 本趟已持有：每段的向量（`svs`/`segEmbeds`）、每组的代表向量、每组解析到的 `speakerID`、以及 `matched[i]`（是否历史命中）。

为忠于详情页口径（对样本取 max），纠正 pass 需要**在场说话人的样本向量集合**：

- 对**新建组**（Y=iux5x/ul8zb）：其样本就是本趟登记用的那一条向量（clean 段或聚合代表），阶段内已在手。
- 对**历史命中组**（H=铉晔，以及可能作为 Y 的其他历史命中组）：需要其库内多条样本向量——通过 `d.SpeakerEmbeddings` 按 speaker_id 批量取，解码为 `[][]float32`。

因此在 pass 开头，一次性为「本次全部在场说话人」加载样本向量（新建组用在手向量，历史组从 `d.SpeakerEmbeddings` 取），构造与详情页一致的评分函数。若某历史说话人样本行缺失，退回其 `speaker.embedding` 聚合代表兜底。

## 6. 数据模型与标记

迁移 `000017_segment_speaker_correction`（up/down）：

```sql
-- up
ALTER TABLE transcript_segment
  ADD COLUMN corrected_from_speaker_id BIGINT NULL COMMENT
  '幽灵历史声纹纠正：被自动顶掉的原历史说话人 id；非 NULL = 该段已被自动改判（前端"已修改"徽章 + 审计 + 手动改回依据）';
```

```sql
-- down
ALTER TABLE transcript_segment DROP COLUMN corrected_from_speaker_id;
```

- 非 NULL = 「已修改」，其值 = 被顶掉的历史说话人 id（铉晔），供审计 / tooltip 显示原判定 / 将来撤销。
- `repo.TranscriptSegment` 增字段 `CorrectedFromSpeakerID *ids.ID`。
- 详情 API（`GetSession`）的 `segmentView` 增 `corrected_from`（id 字符串）+ `corrected_from_name`（解析铉晔的名字，即便它已从本会话 chip 列表消失——从 `Speakers` 兜底解析，不依赖本会话 speaker 列表）。

## 7. 标记生命周期

- **重新识别**（`ClearSegmentSpeakers`）：清 `speaker_id` 的同时把 `corrected_from_speaker_id` 一并清 NULL，重新走一遍纠正。
- **手动换人**（`SetSegmentSpeakerByID` 单段 / `ReassignSpeakerSegments` 整人）：一旦用户手动改判，把受影响段的 `corrected_from_speaker_id` 清 NULL——手动结果不再是「自动纠正」，徽章消失。
- **幂等重跑**（Reextract：段已 assigned 则整组跳过）：不重复纠正，已有标记保留。
- **改判的副作用**：铉晔库条目不删不动；不把铉晔的段追加进 iux5x 的样本（FAISS/样本行不动）。改判后铉晔在本会话零段 → 由 `ListSpeakersForTranscript`（按段 speaker_id 派生）自动从说话人 chip 列表消失。

## 8. 前端（`web/app.js` / `web/index.html`）

- 详情页转写条目：当段的 `corrected_from` 非空时，在 speaker 标签旁渲染一个「已修改」小徽章（灰底，`title` tooltip 显示「原判定：铉晔」= `corrected_from_name`）。
- 纯展示；用户仍可用现有换人下拉手动改回（改回后徽章消失，见第 7 节生命周期）。

## 9. 边界与已知取舍

- **只在整段重跑时可靠触发**：纠正需要「本次在场全部组」的信息。identify 与 reidentify（先 `ClearSegmentSpeakers` 再重跑）都会整段重处理，故是主路径。若出现「部分组已 assigned、部分未 assigned」的偏路径（reextract 跳过已解析组），本次 `reps` 覆盖不全，纠正可能不触发——可接受（要么已纠正过、要么用户已手动处理）。
- **margin=0.06 默认**：用 max 口径下本 badcase 差 0.15，稳过。若未来发现真人换环境掉分被误纠，调大 margin。
- **不级联**：单趟评估，H 改判给 Y 后不再回头看 Y 是否又该改判给别人。

## 10. 测试（TDD，用现有有状态 fake `libVoiceprint`）

- `TestStageSpeakerCorrectsPhantomHistoricalMatch`：构造铉晔（历史命中）名下段，使某在场新建说话人 iux5x 在其某段上的 max 相似度 > 铉晔自身 max + margin → 断言整组改判给 iux5x，且段的 `corrected_from` = 铉晔 id。
- `TestStageSpeakerKeepsHistoricalMatchWhenSelfHighest`：历史命中且铉晔在自己段上 max 最高（无人超过）→ 不改判、无标记。
- `TestStageSpeakerNeverCorrectsAutoRegistered`：自动新建组即使分数偏低也不参与纠正（不被判幽灵）。
- `TestStageSpeakerCorrectionMarginBoundary`：`score_bestY` 恰好等于 / 略高于 `self_H + margin` 的边界行为。
- API 测试：`GetSession` 返回 `corrected_from` / `corrected_from_name`；手动换人（单段 & 整人）后 `corrected_from` 被清；reidentify 后标记清空并重算。

## 11. 涉及改动清单

1. `migrations/000017_*.{up,down}.sql`：加列。
2. `internal/repo/transcript.go`：`TranscriptSegment.CorrectedFromSpeakerID`；查询列；`ClearSegmentSpeakers`、`SetSegmentSpeakerByID`、`ReassignSpeakerSegments` 清标记；新增写标记的方法（或在纠正 pass 内用带 `corrected_from` 的 UPDATE）。
3. `internal/pipeline/stage_speaker.go`：纠正 pass + 详情页同款打分（对样本取 max）+ 加载在场说话人样本向量。
4. `internal/api/query.go`：`segmentView` 增 `corrected_from`/`corrected_from_name`；解析原说话人名。
5. `web/app.js` / `web/index.html`：「已修改」徽章。
6. 对应测试文件。
