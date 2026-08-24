# 说话人名字推断（从对话上下文猜真名）· 设计规格

- 日期：2026-08-24
- 状态：待评审
- 相关：`docs/superpowers/specs/2026-08-22-speaker-voiceprint-design.md`（声纹识别，本设计的上游）、`internal/pipeline/stage_speaker.go`（说话人解析）、`internal/pipeline/stage_extract.go`（LLM 抽取范式）

## 1. 目标

当声纹识别（speaker stage）**新建**一个说话人（默认名 `说话人{5位随机串}`），或**匹配到一个仍是随机名**的既有说话人时，用 LLM 从最近的对话转写里推断这个人的真实称呼（如「张总」「王哥」「张三」），作为**带置信度的候选名**关联到该说话人上，供用户一键确认。

具体要求：

1. 只对「名字仍是自动随机名」的说话人跑推断（新建的天然是；匹配到的随机名说话人也是——即用户尚未认领的人）。
2. 上下文取**跨录音最近 10 分钟**（墙钟时间）的对话转写，带说话人标注。
3. 输出多个候选名，各带置信度，**按置信度倒排**。
4. 处理链路里**自动跑**；`重新识别`时重跑。
5. 候选**仅作建议**：不自动改名，保留随机名，用户在界面看到「候选名 + 置信度数值」后点选确认，才写成正式名。
6. **区分在场者与第三人**：两人谈论第三人（缺席者）时，第三人的名字不得当作任一在场说话人的候选。

## 2. 关键决策

### 2.1 独立 stage，而非内联进 speaker stage

新增 `speakername` stage，链路：`asr → segment → speaker → speakername → extract`。

理由：声纹匹配（ML/sidecar）与名字推断（LLM）职责不同、失败原因不同、重试代价不同，拆开各自可测、可独立重试。`重新识别`（`POST /api/sessions/{id}/reidentify`，从 speaker stage 起跑）会自然带着 speakername 一起重跑。`extract` stage 依旧只读**已确认名**（未确认的说话人仍是随机名/回退标签），行为不变——候选名不污染记忆抽取。

**权衡**：多一个 stage 的装配成本。相比内联，收益（隔离、可测、可重跑）值得。

### 2.2 一次 session 一个 LLM 调用（批处理）

对本 session 内**所有**待识别说话人，用**同一段上下文**、**一次** LLM 调用得到全部候选，而不是每个说话人各调一次。

理由：上下文共享，省 token；且让模型能跨说话人联合推理（"A 管 B 叫张总" 同时定位 B 的名字、排除 A）。逐个调用会丢失这种全局视角，还更贵。

### 2.3 候选存独立表，跨 session 累积

候选名存 `speaker_name_candidate` 表（一个说话人 N 行候选），而非塞进 `speaker` 的 JSON 列。理由：需要按置信度排序/查询、跨多段录音累积同一候选的证据、单独忽略某候选——关系表比 JSON 更契合，也与项目现有规范化风格一致。

## 3. 架构与数据流

```
speaker stage 完成（本 session 段已回填 speaker_id）
  → [speakername] ← 新增 stage
      1. 取本 session 段 → 找出「出现在本 session 且 name 仍是随机名」的说话人集合 T（待识别）
         · 无 → 直接 no-op 返回（不调 LLM）
      2. 取上下文：跨录音墙钟窗口 [S.created_at − W, S.created_at + 本录音时长] 的全部段
         （带已知说话人名/待识别占位/未知），按墙钟时间排序
      3. 组 prompt（每个说话人稳定 token，标出哪些是「待识别人物A/B…」）→ 1 次 LLM 调用
      4. 解析 JSON：每个待识别人物 → 候选 [{name, confidence, evidence}]（倒排）
      5. upsert 进 speaker_name_candidate（跨 session 取最高置信、留最新证据）
  → [extract] 记忆/待办抽取（不变）
  → session completed
```

### 3.1 触发资格：什么叫「随机名」

自动登记名由 `stage_speaker.go` 的 `rand5()` 生成，形如 `说话人` + 5 位 `[a-z0-9]`。资格判定用**正则** `^说话人[a-z0-9]{5}$`（`internal/pipeline` 内 `isAutoName(name)`）：

- 命中 → 该说话人「仍是随机名」，属待识别。
- 用正则而非 `source='auto'`：用户可能把某个 `source=auto` 的说话人手动改成了真名（source 不变但 name 已非随机），这类**不该**再被推断打扰。
- 注意区分：`说话人 N`（带空格，如「说话人 1」）是 `internal/api/query.go:speakerLabelName` 的**显示回退**，从不落库为 `speaker.name`，正则（无空格）不会误命中。

### 3.2 上下文窗口（跨录音最近 10 分钟）

- 窗口 = `[S.created_at − W, S.created_at + durationMS]`，`W` 默认 10 分钟（`ZW_NAME_INFER_WINDOW_MIN`），`durationMS` = 本 session 段的 `max(end_ms)`。
- 语义 = **当前录音全文 + 紧邻其前 W 分钟的跨录音对话**。取全文而非严格「最后 10 分钟」，是因为自我介绍/称呼常出现在录音开头，严格尾窗会漏。
- 段的墙钟时间 = `session.created_at + segment.start_ms`。跨 session 拉取时在 SQL 里算，`user_id` 维度过滤。
- 超长上下文按段数上限 `ZW_NAME_INFER_MAX_SEGMENTS`（默认 400）裁剪，**保留最近的**（靠近 S 结束的）。本 session 的段墙钟时间恒 ≥ `S.created_at`，是窗口内最新的一段，DESC+LIMIT 天然优先保留——待识别说话人在本 session 的发言不会被裁掉。
- **已知假设**：墙钟用 `audio_session.created_at`。对「上传历史音频」场景（上传时间 ≠ 实际对话时间），窗口基本只含该 session 自身——可接受（跨录音回看本就是「连续对话被切成多段录音」的场景）。

### 3.3 喂给模型的说话人 token

上下文里每条发言前挂一个**稳定 token**，让模型能指认「是谁说的 / 名字该归给谁」：

| 段的情况 | token |
|---|---|
| `speaker_id` 已解析且**非随机名**（已确认真名） | 真名，如 `李明` |
| `speaker_id` 已解析但**随机名**（待识别，属 T） | `待识别人物A`、`待识别人物B`…（按 speaker_id 去重分配字母） |
| `speaker_id` 为 NULL（提向失败等未解析） | `未知` |

模型只需为 `待识别人物X` 产出候选；已确认名的人是上下文/称呼来源，不产候选。

## 4. 第三人区分（prompt 设计）

`prompts/speaker_naming_v1.md`（system prompt）。核心规则：

只在两种情形给某个**在场说话人**记名字：

1. **被当面称呼**：其他人在**对其说话的话轮**里用了称呼语（常在句首，如「张总，您看这个方案」「李哥，帮我看下」）→ 名字记给**被称呼方**，不是说话方。
2. **自我介绍**：说话人自述「我是李明」「我姓王，叫我王工就行」→ 记给**说话方本人**。

**禁止**：仅作为**谈论对象**出现的名字（谈论一个缺席的第三人，如「昨天王总来找我谈了下」——王总没有参与对话轮次）不得当作任何在场说话人的候选。

判定信号：称呼语位于**指向某在场说话人的话轮**（后接问候/请求/提问，对方随后应答）vs 出现在**描述缺席者的叙述内容**里。多人场景中若无法确定称呼指向哪位在场者，降低置信度或不给。拿不准是在场者还是第三人 → 宁可不给。

**输出格式**（只输出 JSON，无围栏）：

```json
{"speakers":[
  {"ref":"待识别人物A","candidates":[
    {"name":"张总","confidence":0.82,"evidence":"对方在 03:12 说『张总，您看这个方案』"},
    {"name":"张明","confidence":0.4,"evidence":"自称『我姓张』"}
  ]},
  {"ref":"待识别人物B","candidates":[]}
]}
```

- `candidates` 按 `confidence` 倒排；无可靠候选给空数组。
- `confidence` ∈ [0,1]：当面直呼且指向明确、或本人自述完整姓名 0.8+；仅姓氏/称谓（「王哥」但不确定全名）或指向略歧义 0.4~0.8；间接/推断更低。
- `evidence`：简短引用 + 时间点，供用户判断，也便于排查。

## 5. 数据模型（迁移 `000005_speaker_name_candidate`）

```sql
CREATE TABLE speaker_name_candidate (
  id                BIGINT PRIMARY KEY,
  speaker_id        BIGINT NOT NULL,                       -- 归属说话人
  name              VARCHAR(128) NOT NULL,                 -- 候选名（张总/王哥/张三…）
  confidence        DECIMAL(5,4) NOT NULL DEFAULT 0,       -- 置信度 [0,1]，排序键（类型对齐全库 confidence 列）
  evidence          VARCHAR(512) NULL,                     -- 依据：简短引用 + 时间
  source_session_id BIGINT NULL,                           -- 最近一次产生该候选的会话
  created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_speaker_name (speaker_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```
（质量审查修订：去掉冗余的 `idx_speaker`——被 uk 最左前缀覆盖；confidence 用 DECIMAL(5,4) 对齐全库。）down：`DROP TABLE IF EXISTS speaker_name_candidate;`

- **跨 session 累积**：`INSERT … ON DUPLICATE KEY UPDATE confidence=GREATEST(confidence, VALUES(confidence)), evidence=VALUES(evidence), source_session_id=VALUES(source_session_id)`。多段录音复现同一候选名 → 取最高置信、留最新证据。（MVP 用 GREATEST；后续可考虑「多次复现小幅上调」的佐证式增强，参考 extract stage 的 D1。）
- **说话人改名即清空候选**：`speaker` 改名（点选候选或手动改名）后删除该 speaker 的全部候选行——名字已确认、也不再是随机名，不再重跑。删除逻辑挂在改名路径（见 §7）。

## 6. Stage 实现（`internal/pipeline/stage_speaker_name.go`）

核心可测函数 `runSpeakerNameStage(ctx, d, sessionID, tr)`：

1. `segs := d.Transcripts.ListSegments(tr.ID)`；建 `speaker_id → *Speaker` 映射（`d.Speakers.List`）。
2. 求待识别集合 `T` = { seg.SpeakerID | seg 在本 session 且 speaker.name 命中 `isAutoName` }。`T` 空 → 返回 nil。
3. `durationMS = max(seg.EndMS)`；`from = S.CreatedAt.Add(-W)`，`to = S.CreatedAt.Add(durationMS ms)`。
4. `ctxSegs := d.Transcripts.ListSegmentsInWallClockWindow(userID, from, to, maxSegments)`。
5. 给每个 speaker 分配 token（§3.3），组 user message（对话按墙钟排序，格式 `时间|token|文本` + 一份「待识别人物 → token」清单）。
6. `resp := d.LLM.Chat({Model: d.LLMModel, System: d.NameInferPrompt, User: msg})`。
7. `parsed := ParseNameCandidates(resp.Content)`（纯函数，可单测）。
8. 把 `ref（待识别人物X）` 映射回 `speaker_id`，逐候选 `d.SpeakerNameCandidates.Upsert(speakerID, name, confidence, evidence, sessionID)`。

包装成 `stageSpeakerName(d) Handler`，注册进 `BuildStages`。

- **幂等**：候选 upsert（GREATEST 置信），重跑不产生重复行；无待识别说话人 no-op。
- **错误（2026-08-24 质量审查修订：尽力而为语义）**：LLM 调用失败与输出解析失败只记日志 + job.trace（`speakername` 错误条目），**不返回 error、不 fail session**——候选仅建议，不该阻塞后续 extract；说话人仍是随机名时下一段录音会自然重试推断，恢复路径天然存在。DB 类错误（段/候选读写）仍走 pool 现有重试 3 次 → `failed`。不影响已落库转写与说话人归属（本 stage 只写候选表）。
- **trace**：LLM 调用写 `speakername:llm` 条目（model/耗时/tokens，对齐 extract stage 的 `extract:llm`）。
- **成本**：每 session 至多 1 次 LLM 调用，且仅在存在待识别说话人时。数据仍只发给现有 Ark LLM（与 extract 同），无新增外部暴露。

新增 repo 查询 `TranscriptRepo.ListSegmentsInWallClockWindow`（跨 session 墙钟窗口）：

```sql
SELECT seg.id, tr.session_id, s.created_at, seg.start_ms, seg.end_ms,
       seg.speaker_label, seg.speaker_id, sp.name AS speaker_name, seg.text
FROM transcript_segment seg
JOIN transcript tr      ON tr.id = seg.transcript_id
JOIN audio_session s    ON s.id = tr.session_id
LEFT JOIN speaker sp    ON sp.id = seg.speaker_id
WHERE tr.user_id = ?
  AND (s.created_at + INTERVAL seg.start_ms * 1000 MICROSECOND) BETWEEN ? AND ?
ORDER BY (s.created_at + INTERVAL seg.start_ms * 1000 MICROSECOND) DESC
LIMIT ?;               -- DESC + LIMIT 取最近 N，Go 侧反转回正序
```

## 7. API / 前端（建议模式，确认时显示置信度）

### 7.1 API

- **读**：`GET /api/speakers` 与 `GET /api/sessions/{id}` 的 `speakers[]`，每个说话人附
  `name_candidates: [{name, confidence, evidence}]`（按 confidence 倒排；无则空数组）。
  由新 repo `SpeakerNameCandidateRepo.ListBySpeaker` 提供；`SpeakerHandler`/`QueryHandler` 富化。
- **采纳**：复用现有 `PATCH /api/speakers/{id} {name}`（`SpeakerHandler.Rename`），前端把选中候选名传入。
  在 `Rename` 成功后调用 `SpeakerNameCandidates.DeleteBySpeaker(id)` 清空候选。
- **忽略单个候选**：`DELETE /api/speakers/{id}/name-candidates?name=…` → 删该行。
- 不新增手动触发端点（自动跑；`reidentify` 已能重跑本 stage）。

### 7.2 前端（`web/app.js` + `index.html`）

在「声纹」tab 的说话人卡片、以及会话详情的说话人面板：当该说话人是随机名且有候选时，展示「**建议名字**」区：

- 每个候选一行/一 chip：**名称 + 置信度数值**，如 `张总 · 0.82`（倒排）。置信度**必须**以数值显示（用户确认时能看到名称和置信度值——硬性要求）。
- 每个候选带「✓ 采纳」（→ PATCH 改名 + 清候选 + `reloadSession`/刷新名册）与「忽略」（→ DELETE 该候选）。
- `evidence` 以副文本/悬浮/展开呈现，辅助用户判断。
- 已确认名（非随机名）的说话人不展示该区。

沿用现有设计系统（chips / ask-confirm / editable 模式）。

## 8. 装配与配置

- `StageDeps`（`internal/pipeline/stage_asr.go`）新增：
  `NameInferPrompt string`、`SpeakerNameCandidates *repo.SpeakerNameCandidateRepo`、`NameInferWindowMin int`、`NameInferMaxSegments int`。
- `BuildStages` 加 `"speakername": stageSpeakerName(d)`；`main.go` 的 `Flow.Stages` 改为
  `["asr","segment","speaker","speakername","extract"]`。
- `main.go`：读 `prompts/speaker_naming_v1.md`、构造 `SpeakerNameCandidateRepo` 并注入；`SpeakerHandler`/`QueryHandler` 注入新 repo。
- 配置（`internal/config/config.go`）：

| 配置 | 环境变量 | 默认 |
|---|---|---|
| 上下文窗口（分钟） | `ZW_NAME_INFER_WINDOW_MIN` | `10` |
| 上下文段数上限 | `ZW_NAME_INFER_MAX_SEGMENTS` | `400` |

LLM 复用 `LLMFastModel`（`doubao-seed-1-6-flash`）。

## 9. 测试策略（对齐四层）

- **Unit**：
  - `ParseNameCandidates(json)`：正常/空候选/脏 JSON/倒排。
  - `isAutoName`：命中 `说话人ab3x9`、不命中 `张总`/`说话人 1`（带空格）/`enrolled 名`。
  - `runSpeakerNameStage`：注入 fake LLM + fake repo，覆盖
    ① 无待识别说话人 → 不调 LLM、no-op；
    ② 单待识别 + 当面称呼 → 候选正确、归属正确；
    ③ **谈论第三人 → 第三人不入候选**（fake LLM 返回符合 prompt 语义的结果，用例校验 stage 侧映射/落库正确）；
    ④ 多候选按置信度倒排落库；
    ⑤ 跨 session 复现同名 → GREATEST 取最高置信、不重复行。
- **Integration**：迁移 `000005`；`ListSegmentsInWallClockWindow` 真连 MySQL 验墙钟跨 session 窗口 + 上限裁剪；`SpeakerNameCandidateRepo` upsert/list/delete。
- **E2E**：`scripts/e2e.sh` 可选校验处理后随机名说话人带 `name_candidates`（真 LLM，同 spike 不进 CI）。
- **Spike（手动）**：真 Ark LLM 跑一段含「当面称呼 + 自我介绍 + 谈论第三人」的对话，人工核对归属与第三人排除、置信度合理性。

## 10. 已知限制与后续

- 墙钟基于 `created_at`（§3.2 假设）；上传历史音频时跨录音回看意义有限。
- 置信度累积用 GREATEST（MVP）；后续可做「多录音复现→佐证式上调」。
- 多人（>2）场景称呼指向可能有歧义，靠模型 + 低置信兜底，用户确认为最终裁决。
- 候选仅建议、不自动改名（本设计既定）；若后续想要「高置信自动改名」，只需在 stage 末尾按阈值调用 `UpdateName` + 清候选，数据模型无需改。
- 说话人若在 `reidentify` 后被重建为新 speaker_id，旧候选成为孤儿（其 speaker 仍在，属边角情况），可接受。
- 候选只累积不删除：后续 run 收窄候选集时，模型不再支持的旧候选会残留（用户可手动忽略）；后续可做「重跑时整组替换」。
- 「当前录音段天然优先保留」的裁剪保证在**录音重叠**场景不严格成立（前置录音的段墙钟可能晚于当前录音开头，DESC 排序在其前）；需重叠录音 + 10 分钟窗口内 >400 段才触发，实际罕见。
