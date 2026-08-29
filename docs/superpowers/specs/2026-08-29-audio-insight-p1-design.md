# P1 音频场景与情绪理解（A+B）实现设计

- 日期：2026-08-29
- 总纲：`docs/superpowers/specs/2026-08-29-conversation-insight-roadmap-design.md`（本文件是其 **P1** 子项的实现 spec）
- 分支/worktree：`feat/audio-insight`
- 可行性：已 spike 实测（见 memory [[audio-understanding-and-image-gen-config]]）——`stepaudio-2.5-chat` 稳定吐结构化情绪/声学场景 JSON。

## 1. 目标与范围

在 ASR 转写之后，用一次 `stepaudio-2.5-chat` 音频理解调用，产出并落库：
- **会话级**（A 声学环境）：`acoustic_scene`（室内/车内/餐厅/地铁/户外…）、`background_sounds[]`（键盘/车流/鸟叫/宠物/风雨/金属…）、`weather_cues`、`overall_mood`。
- **说话人级**（B 情绪，**整段一次按人归因**，已与用户确认的粒度）：每位说话人一条 `emotion` / `micro_emotion` / `mental_state` / `confidence`。
- 前端在会话详情**最小可视化**：显示会话环境 + 每位说话人情绪；转写段按其说话人关联展示情绪（满足"情绪关联到语音条目"）。

**不在 P1**：人物级情绪汇总（C）、报告深度增强（D）、漫画（E）；逐段独立切片情绪（成本，用户已否）。

## 2. 数据模型（迁移 `000025_audio_insight`）

> 迁移号：main 最新 `000024_agent_skill`，本特性用 **000025**。合并前复查 main 最新号（并行分支撞号坑，见 [[zhiwei-db-per-feature-convention]]）。

**(a) `transcript` 加会话级环境列**（1:1，自然挂在 transcript 上）：
```sql
ALTER TABLE transcript ADD COLUMN acoustic_scene    VARCHAR(32)  NOT NULL DEFAULT '' AFTER confidence;
ALTER TABLE transcript ADD COLUMN background_sounds  JSON        NULL              AFTER acoustic_scene;
ALTER TABLE transcript ADD COLUMN weather_cues       VARCHAR(32)  NOT NULL DEFAULT '' AFTER background_sounds;
ALTER TABLE transcript ADD COLUMN overall_mood       VARCHAR(128) NOT NULL DEFAULT '' AFTER weather_cues;
```
（`background_sounds` 用 `*json.RawMessage` 映射，NULL→nil，对齐 job.go 惯例。）

**(b) 新表 `speaker_session_state`**（每会话每说话人一条情绪状态；独立表因是可选/可失败的富化，不污染热表 transcript_segment）：
```sql
CREATE TABLE speaker_session_state (
  id            BIGINT PRIMARY KEY,
  user_id       BIGINT NOT NULL DEFAULT 1,
  transcript_id BIGINT NOT NULL,
  session_id    BIGINT NOT NULL,
  speaker_label VARCHAR(32)  NOT NULL DEFAULT '',   -- ASR 标签（"1"/"2"…）
  speaker_id    BIGINT NULL,                        -- 已识别说话人（speakername 后回填，可空）
  emotion       VARCHAR(32)  NOT NULL DEFAULT '',
  micro_emotion VARCHAR(64)  NOT NULL DEFAULT '',
  mental_state  VARCHAR(64)  NOT NULL DEFAULT '',
  confidence    DOUBLE NOT NULL DEFAULT 0,
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_sss_session (session_id),
  KEY idx_sss_transcript (transcript_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```
`.down.sql`：DROP TABLE speaker_session_state + 4 个 DROP COLUMN。

**repo**：
- `Transcript` 结构体加 4 字段（`AcousticScene/BackgroundSounds *json.RawMessage/WeatherCues/OverallMood`）+ `TranscriptRepo.SetAcoustic(ctx, transcriptID, ...)` 更新方法。
- 新 `SpeakerSessionStateRepo`：`InsertBatch(ctx, []SpeakerSessionState)`、`ListBySession(ctx, userID, sessionID)`（带 user_id 过滤，IDOR）。

## 3. Provider：音频理解

新 `provider.AudioInsight` 接口（业务只依赖接口，实现可换）：
```go
type AudioInsight struct { SessionMood string; AcousticScene string; BackgroundSounds []string; WeatherCues string; Speakers []SpeakerInsight }
type SpeakerInsight struct { Label, Emotion, MicroEmotion, MentalState string; Confidence float64 }
type AudioInsightProvider interface {
    Analyze(ctx context.Context, audioPath string, speakerLabels []string) (AudioInsight, error)
}
```
实现 `StepAudioInsight`（`internal/provider/audio_insight.go`）：
- 调 OpenAI 兼容 `/chat/completions`，`model=stepaudio-2.5-chat`，base+key 来自配置（默认走**代理**：`STEPFUN_ASR_FILE_API_KEY` + `https://api.c.ibasemind.com/v1`——主账号 STEPFUN_API_KEY 已欠费 402）。
- content = `[{type:text, text:<硬约束JSON提示，含已知说话人标签>}, {type:input_audio, input_audio:{data:<base64>, format:"wav"}}]`。
- system 提示强制"只输出 JSON、不要 markdown"（spike 实测这样才稳定可解析）。
- 解析容错：模型偶尔裹 ```json 代码块 → 先剥壳再 `json.Unmarshal`；失败返回 error（stage 据此降级）。

## 4. Stage：`audioscene`

**位置**：`asr → segment → speaker → speakername → audioscene(新) → extract [→ profile]`。放在 `speakername` 后——此时 `transcript_segment.speaker_id` 已回填，可把每位说话人的情绪归因到已识别的人（`speaker_label → speaker_id` 映射从 segments 聚合）。

**逻辑**（`stageAudioScene(d)`）：
1. 开关：`d.AudioInsightEnabled` 为 false → 直接返回 nil（跳过）。
2. `d.AudioInsight == nil`（未装配）→ 返回 nil（兼容旧装配/测试）。
3. 取 session + transcript；`wav := transcodeToWAV(d.DataDir, sessionID, s.StoragePath)`（**幂等复用** ASR 已转码产物）。
4. 从 segments 收集去重的 `speaker_label` 列表 + `label→speaker_id` 映射。
5. 分析（分块编排在 stage 层，provider 保持"一个音频文件 → 一个 insight"）：时长 ≤ `ChunkSec` → 单次 `Analyze(wav,labels)`；否则按 §5 切成多块 WAV、逐块 `Analyze`、按 §5 合并策略合并成会话级 + 每说话人结果。**任一块或整体失败 → 记日志 return nil（降级，不阻断流水线）**（部分块成功则用已成功块的合并结果）。
6. 落库：`Transcripts.SetAcoustic(...)` 写会话级环境；`SpeakerSessionStateRepo.InsertBatch(...)` 写每人情绪（`speaker_id` 用第 4 步映射回填，未识别为 NULL）。
7. user_id：后台流水线暂 user-1（对齐现有各 stage 的 `Get(ctx,1,...)` 现状）。

**降级契约**：本 stage 任何失败（音频取不到/模型报错/解析失败）都只记日志、返回 nil，**绝不让 job 失败**——转写与其余 stage 照常完成，富化信号缺失即空。

## 5. 音频入参与长录音分块

- 默认复用本地转码 WAV（16k mono s16）base64 送模型（spike 已证 base64 可行）。
- **≤10 分钟**：整段单次调用。
- **>10 分钟：分块识别（用户定）**——按 ~10 分钟切成多块，逐块各调一次 `Analyze`，再合并结果：
  1. **切点选择**：在每个 10 分钟边界的**前后 1 分钟窗口内找静音段**（`ffmpeg silencedetect`，如 `-30dB / ≥0.3s`），在静音中点下刀，避免切断正在说的话。
  2. **无静音兜底**：该窗口内一直在说话、找不到静音 → 在固定 10 分钟处切，但**下一块起点前移 2s**（与上一块尾部 2s 重叠），避免边界字词/情绪丢失。
  3. 每块导出为独立 WAV（ffmpeg `-ss/-t` 切片），逐块 base64 送模型。
  4. **合并策略**（多块 → 落库的每会话每说话人一条 + 会话级一条）：
     - 会话级环境：`acoustic_scene`/`weather_cues`/`overall_mood` 取跨块**最高置信/众数**；`background_sounds` 取**并集去重**。
     - 每说话人情绪：跨块按说话人聚合——`emotion`/`mental_state` 取该人最高置信块的值，`micro_emotion` 并集去重（限长），`confidence` 取均值。
  - 分块是**长度约束的技术处理**（适配模型时长/体积上限），落库粒度仍为每会话每说话人（不改数据模型）；chunk 级时间分辨率留作后续（C/D）增强。
- **配置**：`ZW_AUDIO_INSIGHT_CHUNK_SEC`（默认 600=10min）、静音搜索窗口 ±60s、重叠 2s、静音阈值（内置默认，可后续外提）。
- **plan 阶段须验证**：`stepaudio-2.5-chat` 是否接受 `input_audio` 传 URL（接受则每块上传 TOS 用 URL、免 base64 体积；需把 TOS uploader 引入 StageDeps）。spike 只验了 base64；URL 待验。优先级：URL 可行 → 用 URL；否则 base64（分块后每块 ≤10min 已受控）。

## 6. 配置（`internal/config`）

- `AudioInsightEnabled bool`（`ZW_AUDIO_INSIGHT_ENABLED`，默认 `true`，可全局关）。
- `AudioInsightModel string`（`ZW_AUDIO_INSIGHT_MODEL`，默认 `stepaudio-2.5-chat`）。
- `AudioInsightBase string`（`ZW_AUDIO_INSIGHT_BASE`，默认 `https://api.c.ibasemind.com/v1`）。
- `AudioInsightAPIKey string`（默认取 `STEPFUN_ASR_FILE_API_KEY`；为空则不装配 provider → stage no-op）。
- `AudioInsightChunkSec int`（`ZW_AUDIO_INSIGHT_CHUNK_SEC`，默认 600=10min；超此长度分块，见 §5）。
- main.go 装配：仅当 enabled 且 key 非空时构造 `StepAudioInsight` 注入 StageDeps；并把 `audioscene` 加进 `stagesList`（`speakername` 之后、`extract` 之前）。

## 7. API + 前端（最小可视化）

- **API**：会话详情 `GET /api/sessions/{id}` 响应补 `acoustic_scene/background_sounds/weather_cues/overall_mood`（来自 transcript）+ `speaker_states[]`（来自 speaker_session_state，带 user_id 过滤）。
- **前端**（会话详情页，timeline tab）：
  - 顶部一行"环境徽章"：场景 + 背景音 chips + 天气 + 整体氛围（有值才显）。
  - 每位说话人名旁 / 转写段按 `speaker_label` 关联显示情绪 chip（emotion + hover 显 micro_emotion/mental_state）。
  - 隐私：情绪/精神状态仅 owner 可见（P1 后台单用户，天然满足；多用户随全局隔离）。
- 改完 `make hash-web`。

## 8. 测试

- **provider**（`audio_insight_test.go`）：用 fake HTTP / 桩响应验证 JSON 解析（含裹 ```json 代码块的容错）、错误路径返回 error。
- **stage**（`stage_audioscene_test.go`）：fake `AudioInsightProvider` 注入——验证落库（transcript 环境列 + speaker_session_state 每人一条 + speaker_id 归因映射）、开关关闭时跳过、provider 报错时降级不阻断（job 成功）、provider nil 时 no-op。用 `repotest.DSN`。
- **repo**：`SetAcoustic` / `SpeakerSessionStateRepo.InsertBatch/ListBySession`（含越权过滤）。
- **config**：新增 5 个配置项默认值 + 覆盖。

## 9. 已定决策（本 spec 拍死，不留 TBD）

| 项 | 决策 |
|----|------|
| 情绪粒度 | 整段一次调用、**按说话人归因**（用户已确认）；>10min 分块识别后合并回每说话人 |
| 长录音 | >10min 按 ~10min 分块（切点在 ±1min 窗口找静音；无静音则固定切+下块前移 2s 重叠），逐块识别再合并（用户定） |
| 触发 | 随管线自动跑，`ZW_AUDIO_INSIGHT_ENABLED` 默认开可关 |
| 通道 | 代理 key `STEPFUN_ASR_FILE_API_KEY` + ibasemind base（主账号欠费） |
| stage 位置 | `speakername` 之后（情绪能归因到已识别说话人） |
| 会话环境落地 | 扩 `transcript` 4 列 |
| 每人情绪落地 | 新表 `speaker_session_state`（不污染热表 transcript_segment） |
| 段→情绪 | 前端按 `speaker_label` 关联（不在段行落冗余情绪列） |
| 失败降级 | 只记日志、return nil、绝不阻断 job |
| 隐私 | 情绪/精神状态仅 owner 可见 |

## 10. 待 plan 阶段验证/决定（非阻塞，但需在 plan 里落实）

- `stepaudio-2.5-chat` 是否接受 `input_audio` URL（决定 URL vs 压缩 base64）。
- 长录音分块的具体实现（`ffmpeg silencedetect` 在 ±1min 窗口找静音切点、无静音则固定切+2s 重叠、`-ss/-t` 切片导出各块 WAV）。
- 真实录音（非合成）上的情绪/环境质量抽验（spike 用的是 TTS 样本）。

## 11. 风险

- **成本/时延**：每会话 1~N 次音频大模型调用（N=按 10min 分块数，短录音=1）——由开关 + 降级 + 按说话人（非逐段）+ 10min 分块上限控制。
- **质量未真验**：合成样本验证过链路，真录音质量待抽验（plan）。
- **长音频**：base64 体积——靠 URL 或压缩 + 上限兜底。
- **迁移撞号**：000025，合并前复查 main。
