# ASR 实体纠错（专有名词后处理纠正）设计

- 日期：2026-08-29
- 分支：`worktree-feat+asr-entity-correction`（工作树，基于 main `b07a45b`）
- 状态：设计已确认，待写实现计划

## 1. 背景与目标

ASR 经常把**专有名词**识别错：员工姓名/花名、项目代号、产品别名、宠物名等。
本项目数据层已沉淀大量实体（`person`、`person_pet`、`speaker`、`topic`、`todo` 及 `person_attribute`/`person_relationship` 里的别名/称呼），
可用于**后置纠错**：ASR 出文本后，用实体知识库把疑似被误识的实体片段纠正回规范名。

**目标**：
- 降低专有名词误识别率。
- **只改实体**，不随意改写整句；保留原始时序（`start_ms`/`end_ms`）与断句（不改 `sequence_no`、不跨段合并）。
- 被纠正处**标识出来**，可回溯、可查看原文→纠正对照。
- 用户可在设置页手动补充 DB 里没有的内部代号/产品别名。

**链路**：`ASR 文本 → 拼音/音素召回候选白名单 → LLM 一程裁决（只改实体、给依据）→ 达阈值改写 + 标记`

## 2. 非目标（v1 明确不做）

- **ROVER / 多路 beam 候选融合**：用户标注为「进阶」，留待二期。当前 ASR Provider 也只输出单路文本，无多 beam 数据可融。
- **独立 NER 模型**：NER 并入 LLM 一程裁决，不单独训练/接入 NER 服务。
- **建议-确认队列**：已定为「直接改写并标记」，不做 `speaker_name_candidate` 式的人工确认队列。
- **跨段改写**：纠正严格限定在单段内，不跨 `sequence_no` 改写或合并段落。

## 3. 现状（关键代码事实）

以下为探索阶段确认的落地事实，实现时直接对接：

- **流水线**：`internal/pipeline/`，stage 顺序 `asr → segment → speaker → speakername → extract → [profile]`
  （`cmd/zhiwei-server/main.go` 的 `stagesList`）。每个 stage 是 `Handler(ctx, *repo.Job, sessionID ids.ID)`，
  在 `BuildStages(d StageDeps) map[string]Handler`（`internal/pipeline/stage_asr.go`）注册。
- **ASR 落库**：`stageASR` 转码 → `ASR.Transcribe` → 写 `transcript` + 逐条 `transcript_segment`。
  文件 ASR（`stepaudio-2.5-asr`）有原生 ms 时间戳 + 说话人标签；`TranscriptPiece{SpeakerLabel, Text, StartMS, EndMS, Confidence}`。
- **转写段模型**：`repo.TranscriptSegment`（`internal/repo/transcript.go`）已有 `CorrectedFromSpeakerID`、`CorrectedReason`（`'phantom'`/`'short'`）列——
  实体纠错复用 `corrected_reason`，新增原因值 `'entity'`。
- **可复用的写方法**：`TranscriptRepo.UpdateSegmentText(transcriptID, segID, text)`、`RecomputeFullText(transcriptID)`
  （`internal/repo/transcript.go`）。改段文本后需重算 `full_text`。
- **LLM 调用**：`provider.LLMProvider.Chat(ctx, provider.ChatRequest{Model, System, User, Temperature})`（`internal/provider/llm.go`），
  Ark doubao；快模型 `cfg.LLMFastModel`（默认 `doubao-seed-1-6-flash-250828`）。
- **LLM 阶段模板**：`speakername` stage（`internal/pipeline/stage_speaker_name.go` + `prompts/speaker_naming_v1.md`）——
  LLM 调用 + `appendTrace` + **best-effort 出错不 fail**。实体纠错 stage 照此实现。
- **实体数据来源**（见 §5）：`person.display_name`、`person_attribute`(aliases/current_projects)、
  `person_relationship.label/org_name`、`person_pet.name/nickname`、`speaker.name`、`speaker_name_candidate.name`、`topic.name`、`todo.title`。
- **设置页模板**：设置页现有「知微人设」（`agent_config`，迁移 000019）与「技能」子区。
  端到端模板 = 迁移 → `internal/repo` → handler（`internal/agent/handlers.go` 模式）→ `main.go` 装配 → `web/app.js` + `web/index.html`。
- **前端无打包器**：`web/app.js`/`web/index.html` 直接 serve；改 `app.js` 后需 `make hash-web` 重新指纹。
- **迁移/库约定**（见 memory）：共享 `zhiwei` 库只由 main 迁移；工作树/分支调试用**临时库** `zhiwei_<feature>`；
  集成测试 `repotest.DSN` 按包隔离 `zhiwei_test_<pkg>`。本工作树临时库 = `zhiwei_asr_entity_correction`。

## 4. 总体架构

新增 pipeline stage **`correct`**，插入 `asr` 与 `segment` 之间：

```
asr → correct → segment → speaker → speakername → extract → [profile]
```

放在 `segment` 之前，使 `segment` 阶段拼接的 `transcript.full_text` 反映纠错结果。

`correct` stage 流程：

1. **刷新实体库**：从现有表 upsert 进 `entity_kb`（重算拼音/音素），手动条目保留不覆盖（§5）。
2. 逐段处理：对每段 `transcript_segment`——
   a. **召回**：滑窗取子串，按拼音/音素相似度召回 Top-K 候选，并集 = 合法实体白名单（§6）。
   b. 白名单为空 → 跳过（省 LLM 调用）。
   c. **LLM 一程裁决**：送「该段 + 前后各 N 段上下文 + 白名单」，输出疑似误识实体的纠正 edits（§7）。
   d. **门控应用**：`confidence ≥ 阈值` 且 `orig` 原样出现在段文本 → 局部子串替换（§8）。
   e. 有改动 → 写回 `text` + `corrected_reason='entity'` + `entity_edits`（§9）。
3. 末尾 `RecomputeFullText`。

**best-effort**：stage 出错只 log、不 fail job（沿用 `speakername`），下游 speaker/extract 仍可基于未纠正文本运行。
**幂等**：二次运行因文本已匹配规范名、召回不到候选而自然不再改动；显式跳过已 `corrected_reason='entity'` 的段亦可（二者取其一，实现时定）。

## 5. 数据模型（迁移 `000025_entity_kb`）

> 迁移号 000025：main 现有最高为 000024（`000022_mcp_server`/`000023_person_change_log_session_idx`/`000024_agent_skill`），
> 000025 无冲突。本工作树运行用临时库 `zhiwei_asr_entity_correction`。

### 5.1 `entity_kb` 表（每用户实体知识库）

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | BIGINT UNSIGNED PK | snowflake（`internal/ids`） |
| `user_id` | BIGINT UNSIGNED NOT NULL | 所属用户 |
| `canonical` | VARCHAR(128) NOT NULL | 规范名（纠正目标） |
| `kind` | VARCHAR(32) NOT NULL | person\|pet\|project\|task\|topic\|speaker\|custom |
| `pinyin` | VARCHAR(256) NULL | 拼音（归一化：小写、无声调、音节空格分隔），CJK 主匹配键 |
| `metaphone` | VARCHAR(64) NULL | double metaphone，拉丁名/代号用 |
| `source` | VARCHAR(16) NOT NULL | auto\|manual |
| `source_ref` | VARCHAR(64) NULL | 来源行标识（审计/追溯，如 `person:123`、`todo:456`） |
| `enabled` | TINYINT(1) NOT NULL DEFAULT 1 | 用户可单条启禁 |
| `note` | VARCHAR(256) NULL | 手动条目备注 |
| `created_at`/`updated_at` | TIMESTAMP | 标准时间戳 |

- 唯一键 `uk_entity_kb (user_id, canonical, kind)`；索引 `idx_entity_kb_user (user_id, enabled)`。

### 5.2 `entity_settings` 表（每用户功能配置）

| 列 | 类型 | 说明 |
|---|---|---|
| `user_id` | BIGINT UNSIGNED PK | |
| `correction_enabled` | TINYINT(1) NOT NULL DEFAULT 1 | 功能总开关 |
| `confidence_threshold` | DECIMAL(3,2) NOT NULL DEFAULT 0.80 | 应用阈值 |
| `auto_sources` | JSON NULL | 自动入库的 kind 列表，如 `["person","pet","project","task","topic","speaker"]` |
| `updated_at` | TIMESTAMP | |

### 5.3 `transcript_segment.entity_edits` 列

```sql
ALTER TABLE transcript_segment
  ADD COLUMN entity_edits JSON NULL
  COMMENT '实体纠错明细 [{orig,corrected,entity_id,canonical,confidence}]';
```

与 `corrected_reason='entity'` 配合：徽章靠 `corrected_reason`，对照明细靠 `entity_edits`。

### 5.4 Repo 方法

- `internal/repo/entity.go`：`EntityKBRepo{ListEnabled(userID)}`、`UpsertAuto(...)`（按自然键 upsert，来源 auto）、
  `CreateManual/Update/Delete`（来源 manual）、`DeleteAutoByKind(userID, kind)`（刷新前清旧 auto 条目，避免来源行删除后残留）。
  `EntitySettingsRepo{Get/Upsert}`。
- `internal/repo/transcript.go`：`ApplyEntityCorrections(transcriptID, segID, newText string, editsJSON []byte) error`
  —— 单条 UPDATE 同时写 `text` + `corrected_reason='entity'` + `entity_edits`。

## 6. 实体库种子来源与同步

`correct` stage 刷新逻辑（每用户）：

1. 按 `auto_sources` 配置的 kind，从现有表读当前行，映射成 `entity_kb`（`source=auto`）：
   - **person**：`person.display_name`（active/pending，排除 owner 自身可选）+ `person_attribute` 中 `attr_key='aliases'` 的 `value_text` + `person_relationship.label`/`org_name` + `speaker.name`。
   - **pet**：`person_pet.name` + `nickname`。
   - **project**：`person_attribute` 中 `attr_key='current_projects'` 的 `value_text`。
   - **task**：`todo.title`（open 优先，避免全量历史噪音——实现时定截断策略）。
   - **topic**：`topic.name`。
2. 刷新前 `DeleteAutoByKind` 清掉该用户旧 auto 条目（防来源行已删导致残留），再批量 upsert 新 auto 条目；重算 `pinyin`/`metaphone`。
3. `source=manual` 条目不动。

**性能**：实体量级几十到几百，每 stage 全量重算拼音开销可忽略（go-pinyin 对短串极快）。

## 7. 召回（拼音/音素 → 候选白名单）

- 拼音：`go-pinyin`（或等价成熟库），归一化为小写、无声调、音节间空格分隔。
- 音素：double metaphone（拉丁名/内部代号，如英文项目名）。
- 对每段文本，取候选子串（CJK 按 2–4 字滑窗；拉丁按词），计算每子串的拼音/音素，
  与实体库对应键做**相似度**匹配（拼音用音节序列编辑距离归一化相似度；阈值默认 ≥0.7），召回 Top-K（默认 K=5）。
- 并集构成**合法实体白名单**（带 `entity_id`/`canonical`/`kind`）。
- 白名单为空 → 跳过该段 LLM 调用。

> 相似度算法与阈值在计划阶段用真实样例 benchmark 校准（全局规范要求"性能优化必须有 benchmark"）。

## 8. LLM 一程裁决

新增 `prompts/asr_correction_v1.md`，仿 `speaker_naming_v1.md` 风格（中文、严格规则、只输出 JSON）：

**系统指令要点**：
- 输入：一段转写（附前后文）+ 合法实体白名单（canonical/kind）。
- 任务：仅当片段明显是白名单某实体的**语音误识**时，输出替换；否则空。
- **硬约束**：`corrected` 必须原样取自白名单 canonical；只能替换白名单实体对应的片段；**不得改写实体以外的任何文字**（标点、语气词、语序、非实体名词全保留）；保留原始断句。
- 拿不准 → 输出空 edits，不要猜。
- 输出 `{"edits":[{"orig":"<段内原片段>","corrected":"<canonical>","entity_id":"<id>","confidence":0.0-1.0,"reason":"<简短依据>"}]}`。

**调用**：`provider.ChatRequest{Model: d.LLMModel, System: prompt, User: 组装的上下文+白名单}`；Temperature 设低（如 0.1）保稳定。
`appendTrace` 记录 `PromptVersion=asr_correction_v1`、Model、Tokens。

## 9. 应用与门控

对 LLM 返回的每个 edit，**双重门控**才应用：

1. `edit.confidence ≥ entity_settings.confidence_threshold`（默认 0.8）。
2. `edit.orig` **原样出现**在段 `text` 中（防幻觉/越界；用首次出现位置做替换）。

均通过 → 在段文本内做**局部子串替换** `orig → corrected`（多处出现则按 LLM 给的定位/或仅替换首次，实现时定；默认替换首次以最小改动）。
应用后 `ApplyEntityCorrections` 写回 `text` + `corrected_reason='entity'` + `entity_edits=该段应用的 edits`。
整段处理完 → 阶段末尾 `RecomputeFullText(transcriptID)`。

**不门控通过**：原样保留，不落地（"不落地即安全"）。

## 10. 标记纠正 + 前端

- **徽章**：复用现有「已修改」徽章（前端已在 `corrected_reason` 上触发）。新增 `'entity'` 原因，tooltip 显示「实体纠错」。
- **对照展示**：转写详情右栏/段级，用 `entity_edits` 展示**原文（删除线）→ 纠正后**，并标注匹配的实体名（canonical）。落实"标识哪里做了纠正"。
- 前端改动落在 `web/app.js`（读 `entity_edits`、渲染对照）+ `web/index.html`（标记样式）；改完 `make hash-web`。

## 11. 设置页 + API

设置页新增「专有名词」子区（仿「知微人设」+「技能」模板，`web/index.html` 设置面板内 + `web/app.js`）：

- **功能开关** + **置信度阈值**（进阶折叠）。
- **自动入库来源**只读汇总（各 kind 条目数）。
- **手动自定义实体**列表：增删改（规范名 + kind + 备注），`enabled` 单条启禁。

**API**（`internal/agent` handler 模式，`reqUserID(r)` 鉴权，`writeJSON`）：
- `GET /api/agent/entity-settings` / `PUT /api/agent/entity-settings`：开关 + 阈值 + auto_sources。
- `GET /api/agent/entities`（分页/按 kind）：手动 + 自动条目列表。
- `POST /api/agent/entities` / `PUT /api/agent/entities/:id` / `DELETE /api/agent/entities/:id`：手动实体 CRUD。

`main.go` 装配 `EntityKBRepo`、`EntitySettingsRepo` 进 handler + `StageDeps`（`correct` stage 用）。

## 12. 配置项（`internal/config` + env）

- `ZW_ENTITY_CORRECT_ENABLED`（默认 true）：功能总开关（env 层；运行时用户开关存 `entity_settings`）。
- `ZW_ENTITY_CORRECT_THRESHOLD`（默认 0.8）：应用置信度阈值默认值。
- `ZW_ENTITY_CORRECT_WINDOW`（默认 2）：上下文前后各 N 段。
- `ZW_ENTITY_CORRECT_TOPK`（默认 5）：召回 Top-K。

## 13. 错误处理 / best-effort / 幂等

- stage 出错（LLM 超时/解析失败/DB 错误）→ log + 返回 nil（不 fail job），与 `speakername` 一致。
- 单段 LLM 失败 → 跳过该段，不影响其他段。
- **幂等**：重复跑 `correct` 不再产生新纠正（文本已匹配 canonical，召回为空）。额外显式跳过 `corrected_reason='entity'` 段（实现时二选一，推荐显式跳过更省）。

## 14. 测试策略

- **拼音/音素单测**：归一化、相似度排序、Top-K 召回正确性（含中文/拉丁混合）。
- **LLM 纠正（fake provider）**：只改白名单实体；低于阈值不改；`orig` 不存在不改；非实体文字/标点不动；输出非法 JSON 时容错。
- **应用/标记**：`text` 更新、`corrected_reason='entity'`、`entity_edits` 内容、`full_text` 重算。
- **实体库种子**：各来源正确入库带拼音；刷新清旧 auto 不误删 manual；手动 CRUD。
- **端到端 stage**：fake ASR + fake LLM，走通 asr→correct→segment。
- 用临时库 `zhiwei_asr_entity_correction`（或 `repotest.DSN` 隔离库）；**勿污染共享 `zhiwei`**。

## 15. 落地文件清单（预排）

- `migrations/000025_entity_kb.{up,down}.sql`
- `internal/repo/entity.go`、`internal/repo/entity_test.go`
- `internal/repo/transcript.go`（+`ApplyEntityCorrections`）、对应测试
- `internal/pinyin/` 或复用库：拼音/音素工具 + 召回（`internal/entity/recall.go`）
- `internal/entity/`：种子刷新 + KB service
- `prompts/asr_correction_v1.md`
- `internal/pipeline/stage_correct.go`、`stage_correct_test.go`
- `internal/pipeline/stage_asr.go`（`StageDeps` + `BuildStages` 注册）
- `cmd/zhiwei-server/main.go`（`stagesList` 插入 `"correct"` + 装配 deps）
- `internal/config/config.go`（新增 env）
- `internal/agent/`（entity 相关 handler）或新 handler 文件 + 路由
- `web/app.js`、`web/index.html`（设置页 + 纠正对照展示）

## 16. 待定 / 风险（计划阶段收敛）

- 拼音相似度算法选型与阈值：需真实 ASR 错误样例 benchmark 校准。
- `todo.title` 种子截断策略（避免全量历史待办噪音）：open 优先 + 数量上限。
- 多处 `orig` 出现时的替换策略（首次 vs 全部）：默认首次，最小改动。
- double metaphone 的 Go 实现选型（成熟开源库优先，避免重复造轮子）。
