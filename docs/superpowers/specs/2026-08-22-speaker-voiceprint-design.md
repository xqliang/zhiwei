# 说话人声纹识别与检索 · 设计规格

- 日期：2026-08-22
- 分支：`3-glm-5.2`（worktree `arena-ecd6d324/3-glm-5.2`）
- 状态：已评审，待出实施计划
- 相关：`docs/superpowers/specs/asr-protocol-notes.md`（ASR 协议）、`docs/superpowers/specs/2026-08-18-zhiwei-cloud-mvp-design.md`（MVP 架构）

## 1. 目标

为知微云端 MVP 增加「根据语音识别说话人」的能力：

1. 支持手动录入声音样本提取声纹，存入 FAISS，可填用户名称，后续做 1:N 检索比对。
2. ASR 返回每个说话人每段的开始/截止时间（秒，3 位小数）+ 转写文本。
3. 用 ffmpeg 按起止时间切出每个说话人每段的音频片段。
4. 用本地 WeSpeaker（resnet34-LM，87MB）提取每段 256 维 float 声纹向量。
5. 从 FAISS 做 1:N 检索；未匹配则自动登记入 FAISS，名称 `说话人{5位随机串}`，用户后续可改名。
6. 时间线页面可点击说话人列表切换（过滤）或新加说话人，并支持改名/换人。

## 2. 关键决策与调研依据

### 2.1 ASR 接口选型（StepFun）

调研 StepFun 全部 ASR 接口（来源：`platform.stepfun.com/docs/llms.txt` 及各 API 参考页），结论：

| 接口 | 入参 | 时间戳 | 说话人分离 |
|---|---|---|---|
| `POST /v1/audio/transcriptions`（同步） | multipart 直传 | ❌ | ❌ |
| `POST /v1/audio/asr/sse`（SSE 流式） | base64 内联（无 URL） | ✅ 逐 delta `start/end_time`(ms) | ❌ |
| `WSS /v1/realtime/asr/stream`（双向流式） | base64 分片 | ✅ 逐词 `start/end`(s) | ❌ |
| **`POST /v1/audio/asr/file/submit` + `/file/query`（异步文件）** | **`audio.url`（公网 URL）** | ✅ 每句 `start/end_time`(ms)+词级 | ✅ `speaker.id`(`spk_1`..) |

只有**异步文件接口**同时提供原生 ms 时间戳 + 说话人分离（`enable_speaker_info=true` + `show_utterances=true`，`speaker.id` 任务内稳定、跨任务不保证，最多 10 说话人）。`ms/1000 = x.xxx 秒`，恰好满足「秒数 3 位小数」要求。采用此接口，弃用现有 `stepaudio-2.5-realtime`（realtime 无原生时间戳/说话人，靠 prompt 猜测不准确）。

### 2.2 公网 URL 机制（TOS）

异步文件接口只接受 `audio.url`（公网 URL），不支持 base64/直传。采用火山引擎 TOS 对象存储：

- 复用 `xy/web/tools/tos-upload.mjs` 同一账号/桶配置：region `cn-shanghai`、bucket `user-growth`、endpoint `tos-cn-shanghai.volces.com`、凭证 `TOS_ACCESS_KEY`/`TOS_SECRET_KEY`。
- key 前缀改为 `zhiwei/`（用户指定）。
- **隐私改进**：xy 脚本用 `public-read`（游戏素材要 CDN 缓存），语音是隐私数据，改为**上传私有 + presigned GET URL（1h TTL）**给 StepFun，识别完成后删除对象。
- Go 侧用官方火山引擎 TOS Go SDK（确切模块路径实现时确认）。

### 2.3 WeSpeaker + FAISS 集成（Python sidecar）

WeSpeaker + FAISS 是 Python 生态，后端是 Go。采用 Python FastAPI sidecar（常驻，模型只加载一次），Go 通过 HTTP 调用。职责切分：sidecar 管 ML + 向量索引，Go 管 MySQL 业务 + 说话人名册。

### 2.4 说话人解析粒度

ASR 原生 diarization 的 `spk_N` 已完成「session 内」的说话人聚类。因此解析粒度 = 按 `spk_N` 标签分组，每组聚一个代表声纹做「跨 session」1:N。WeSpeaker 只负责跨 session 身份匹配，不做 session 内二次聚类。

> **2026-08-22 修订（见 §13.6）**：上段假设 ASR 原生 diarization 准确。realtime prompt 式 diarization 不稳，会把同一人标成不同 `spk_N`；已加 **session 内声纹聚类**兜底合并同人标签（§13.6），不再「不做 session 内二次聚类」。需真实 WeSpeaker 向量才生效。

## 3. 架构与数据流

```
上传/录音 → audio_session / pipeline_job 入库
  → [asr]      ffmpeg→wav16k → 上传 TOS(zhiwei/{sid}.wav, 私有) → presigned GET URL(1h)
                 → StepFun 异步文件 ASR: POST /v1/audio/asr/file/submit
                     model=stepaudio-2.5-asr, show_utterances=true, enable_speaker_info=true,
                     audio.url=presigned, format=wav, channel=1, rate=16000
                 → 轮询 POST /v1/audio/asr/file/query 至完成 → 解析 result[].utterances[]
                     每句 {text, start_time(ms), end_time(ms), speaker.id(spk_N)}
                     → TranscriptPiece{SpeakerLabel:"N"(spk_去前缀), Text, StartMS, EndMS}
                 → 删 TOS 对象(best-effort) → transcript + segments 落库(真实时间戳+说话人标签)
  → [segment]  汇总全文（已有，不动）
  → [speaker]  ← 新增 stage
  → [extract]  记忆/待办抽取（已有）
  → session completed
```

### 3.1 speaker stage 流程（`internal/pipeline/stage_speaker.go`）

```
取 transcript + segments → 按 speaker_label 分组
逐段：从 transcoded/{sid}.wav 用 ffmpeg 按 [start_ms,end_ms] 切到 slices/{sid}/seg-{n}.wav
      → sidecar /embed → 256 维向量（失败则该段跳过，speaker_id 留 NULL）
每组：聚合同组向量 mean + L2 归一 → 代表声纹 rep
      → sidecar /search rep → {speaker_id, distance} 或 空索引 unmatched
         · distance ≥ ZW_VOICEPRINT_THRESHOLD → 命中 X：组内段 speaker_id = X
         · 未命中 → 新建 speaker(name="说话人"+rand5, source=auto, embedding=blob(rep))
                    + sidecar /add(rep, new_id) → 组内段 speaker_id = new_id
回填：UPDATE transcript_segment SET speaker_id=? WHERE transcript_id=? AND speaker_label=?
      （带 transcript_id 作用域，跨会话静默忽略，并发安全原子写）
清理切片文件（best-effort）
```

关键点：
- 切片源用 `transcoded/{sid}.wav`（asr stage 已转的 16k mono s16），命令 `ffmpeg -y -ss {sec} -to {sec} -i {src} -c copy {out}`（PCM 样本级精确）。
- 阈值判定在 Go 侧（config 单一来源），sidecar 只回 top-1 id+distance。
- 已命中说话人**不增量更新向量**（MVP 保持简单）。
- 单段 embed 失败只跳过该段，不拖垮整 stage。

### 3.2 错误处理

- ASR：submit 后轮询 query（2s 间隔，上限 ~5min）；`FAILED`→返回错误；网络/超时→现有 stage 重试 3 次→`failed`→用户重跑。
- TOS：上传/presigned 失败→ASR stage 错误→重试；识别完成后删对象 best-effort。
- sidecar：单段 `/embed` 失败→跳过、`speaker_id` 留 NULL；`/search`/`/add`/`/remove` 失败→stage 错误→重试。
- FAISS 索引：启动加载失败→建空索引 + log；MySQL `speaker.embedding` BLOB 兜底可重建。
- 现有 pool 重试 + `failed`→重跑闭环不变；转写已在 asr stage 落库，speaker stage 失败不丢转写。

## 4. 组件清单

| 组件 | 位置 | 说明 |
|---|---|---|
| ASR Provider 重写 | `internal/provider/asr.go` | 新增 `StepFunFileASR`（submit+poll），替换 `StepFunASR`(realtime)。`TranscriptPiece`/`ASRProvider` 接口不变 |
| ASR 结果解析 | `internal/provider/asr.go` | 纯函数 `parseFileASRResult(json) []TranscriptPiece`（spk_N→N、ms→StartMS/EndMS） |
| TOS 客户端 | `internal/storage/tos.go`（新） | 上传 wav(私有) + presigned GET URL + 删除；官方 TOS Go SDK |
| 声纹 sidecar 客户端 | `internal/voiceprint/client.go`（新） | HTTP 调 sidecar：Embed/Search/Add/Remove |
| Python sidecar | `services/voiceprint/`（新） | WeSpeaker resnet34-LM + FAISS；`/embed /search /add /remove /health` |
| speaker 仓库 | `internal/repo/speaker.go`（新） | speaker 表 CRUD |
| speaker stage | `internal/pipeline/stage_speaker.go`（新） | 切片→提向→聚合→1:N→登记→回填 segment.speaker_id |
| speaker API | `internal/api/speaker.go`（新） | 名册/录入/改名/删除/换人/session 说话人列表 |
| 前端 | `web/app.js` + `index.html` | 说话人面板：chips 过滤/改名/换人/+录入 |
| 配置 | `internal/config/config.go` | TOS 配置、sidecar URL、阈值、切片目录 |
| 迁移 | `migrations/000004_speaker.{up,down}.sql` | speaker 表 + transcript_segment.speaker_id |
| 入口装配 | `cmd/zhiwei-server/main.go` | ASR 换 StepFunFileASR、注入 TOS/voiceprint/speaker 依赖 |
| Makefile | `Makefile` | `sidecar-start/stop`、`spike-voiceprint` |

## 5. 数据模型

### 5.1 迁移 `000004_speaker`

```sql
-- 说话人声纹名册（向量存 FAISS，embedding BLOB 作灾备/可重建索引，与 memory.embedding LONGBLOB 模式一致）
CREATE TABLE speaker (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  name         VARCHAR(128) NOT NULL,                  -- 用户填的名 或 "说话人{5位随机串}"
  source       VARCHAR(8) NOT NULL DEFAULT 'auto',     -- enrolled(手动录入) | auto(自动登记)
  status       VARCHAR(16) NOT NULL DEFAULT 'active',  -- active | dismissed
  embedding    LONGBLOB NULL,                          -- 256×float32 = 1024B
  sample_count INT NOT NULL DEFAULT 0,
  created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

ALTER TABLE transcript_segment ADD COLUMN speaker_id BIGINT NULL AFTER speaker_label;
ALTER TABLE transcript_segment ADD KEY idx_speaker (speaker_id);
```

down：`DROP TABLE speaker` + `ALTER TABLE transcript_segment DROP KEY idx_speaker` + `DROP COLUMN speaker_id`。

- `transcript_segment.speaker_id` = 该段解析到的已登记说话人（NULL=未解析，如切片过短提向失败）。
- `speaker_label` 保留 ASR 的 `spk_N`（session 内分组键 + 未解析时回退显示）。

### 5.2 StageDeps / 装配

- `StageDeps` 增 `Voiceprint voiceprint.Client`、`Speakers *repo.SpeakerRepo`。
- `Flow` stages：`["asr","segment","extract"]` → `["asr","segment","speaker","extract"]`。
- `main.go`：ASR 换 `StepFunFileASR`，加 TOS client、voiceprint client、speaker repo 注入；启动校验 `TOS_ACCESS_KEY`/`TOS_SECRET_KEY` 缺失→fatal。

## 6. Sidecar HTTP 契约与内部设计

### 6.1 Go↔Python 契约

```
POST /embed   {audio_path}            → {vector:[256 floats]}              # L2 归一
POST /search  {vector:[256 floats]}   → {speaker_id, distance} | {matched:false}  # top-1，Go 侧判阈值
POST /add     {vector, speaker_id}    → {ok:true}                         # 幂等：先 remove_ids 再 add_with_ids
POST /remove  {speaker_id}            → {ok:true}                         # remove_ids + 落盘
GET  /health                          → {status, model, n_vectors}
```

向量 L2 归一后的 256 维 float32；FAISS 用 `IndexIDMap2(IndexFlatIP)`，内积 = 余弦相似度；索引文件 `data/voiceprint.index` 每次 add/remove 后落盘，sidecar 启动时加载。MySQL `speaker.embedding` BLOB 作灾备（可重建索引，后续加 `make rebuild-voiceprint`）。

### 6.2 sidecar 内部（`services/voiceprint/`，FastAPI）

- 启动：load WeSpeaker resnet34-LM（一次）+ load `data/voiceprint.index`（无则建空 `IndexIDMap2(IndexFlatIP(256))`）。
- `/embed`：读本地 wav（sidecar 与 Go 同机共享 `data/`，收路径直接读，无需传文件）→ WeSpeaker 提取 → L2 归一。
- `/add`：`threading.Lock` 串行化（FAISS 写非线程安全）+ 落盘。
- WeSpeaker 加载细节（Python API/ONNX）由 `make spike-voiceprint` 先验证，确认 256 维输出后再固化。

## 7. API

`internal/api/speaker.go`，`SpeakerHandler`：

```
GET    /api/speakers                       说话人名册（id/name/source/status/sample_count）— 管理页/换人下拉用
POST   /api/speakers                       录入：multipart{file,name} → 转码 wav16k → sidecar /embed → 登记(source=enrolled,embedding=blob) + /add → 返回 speaker
PATCH  /api/speakers/{id}                  改名 {name}
DELETE /api/speakers/{id}                  sidecar /remove(id) + DB 删 + UPDATE segment SET speaker_id=NULL WHERE speaker_id=id
GET    /api/sessions/{id}/speakers        本 session 解析到的说话人：[{speaker_id,name,source,segment_count,color_index}]
PATCH  /api/sessions/{id}/segments/{segId}/speaker   换人 {speaker_id}（校验 transcript 作用域，跨会话静默忽略）
```

`GetSession` 详情响应增强：每段 segment 附 `speaker_id` + `speaker_name`（解析到用登记名，未解析回退 `说话人 N`）；顶层加 `speakers[]`（面板用）。

## 8. 前端

会话详情展开区，音频播放器下方、转写列表上方插入「说话人」面板。沿用现有设计系统（chips / editable / ask-confirm 模式）。

- **chips**：每个解析到的说话人一个 chip，颜色按其在 session 内序号取调色板（新增 `speakerPalette` ~8 色循环，替代现有 `Math.min(n,3)` 截断）。
- **点击 chip → 切换/过滤**：toggle `speakerFilter`，转写列表只显示该说话人的段（含「全部」chip 复位）。
- **chip 名 inline ✎ 改名**：`PATCH /api/speakers/{id}`（处理自动登记的 `说话人{5位随机串}`）。
- **chip ✕ 删除**：2 步确认（沿用现有 ask/confirm）→ `DELETE`。
- **+ 新加说话人**：弹录入表单（拖拽/选短音频样本 + 填名）→ `POST /api/speakers` → 刷新面板。复用 `#drop` 拖拽样式。
- **段 badge 点击 → 换人下拉**：`<select>` 列全部已登记说话人 +「+ 新加…」，选则 `PATCH` 段 speaker；「+ 新加」打开录入表单，录入完用新 speaker 回填该段。

回退：段无 `speaker_id` → badge 显示 `说话人 N`（ASR 标签回退）、灰色。录入与换人成功后 `reloadSession` 刷新。

## 9. 配置

| 配置 | 环境变量 | 默认 |
|---|---|---|
| TOS AK/SK | `TOS_ACCESS_KEY`/`TOS_SECRET_KEY` | 必填（可放 `.env`） |
| TOS region | `ZW_TOS_REGION` | `cn-shanghai` |
| TOS bucket | `ZW_TOS_BUCKET` | `user-growth` |
| TOS endpoint | `ZW_TOS_ENDPOINT` | `tos-cn-shanghai.volces.com` |
| TOS key 前缀 | `ZW_TOS_KEY_PREFIX` | `zhiwei/` |
| StepFun 文件 ASR base | `ZW_STEPFUN_ASR_BASE` | `https://api.stepfun.com/v1` |
| 声纹 sidecar URL | `ZW_VOICEPRINT_SIDECAR_URL` | `http://127.0.0.1:8010` |
| 匹配阈值 | `ZW_VOICEPRINT_THRESHOLD` | `0.5`（余弦，需 benchmark 实调） |
| 切片目录 | （`DataDir/slices`） | `./data/slices` |

`STEPFUN_API_KEY` 复用（文件 ASR 同一鉴权）。README 补 env 说明。

## 10. 测试策略

对齐项目现有 unit + integration + e2e + spike 四层：

- **Unit**：`parseFileASRResult(JSON)→[]TranscriptPiece`（含 `spk_N`→`N`、ms→StartMS/EndMS）；sidecar client 用 mock HTTP server；`stage_speaker_test.go` 注入 fake VoiceprintClient + fake repo，覆盖 all-match/all-enroll/混合/空索引/embed 失败跳过/聚合；`repo/speaker_test.go` CRUD + 段换人作用域。
- **Integration**：迁移 000004，`-p 1` 串行复用 `init-testdb`。
- **Python pytest（sidecar）**：stub 模型跑 /embed→256 归一、/add→/search 命中、/remove（CI 用 stub，真 WeSpeaker 太重）。
- **E2E**：`scripts/e2e.sh` 扩展，校验处理后 segment 带 `speaker_id`；真 WeSpeaker+TOS 需凭据→手动（同 `spike-asr` 不进 CI）。
- **Spikes（手动）**：`make spike-voiceprint`（加载 WeSpeaker resnet34-LM 验证 256 维）、`make spike-asr` 改为验文件 ASR 端到端（TOS 上传→submit→query）。

## 11. 依赖与上线

- **Go**：火山引擎 TOS Go SDK（确切模块路径实现时确认）。
- **Python（`services/voiceprint/requirements.txt`）**：`fastapi`、`uvicorn`、`faiss-cpu`、`numpy`、`wespeaker`/`onnxruntime`。
- **Makefile**：加 `sidecar-start`/`sidecar-stop`、`spike-voiceprint`。
- **启动顺序**：MySQL → sidecar → server。
- **迁移**：`000004_speaker` up/down；存量 session 的 `speaker_id` 为 NULL（回退显示），无需回填。

## 12. 已知限制与后续

- 匹配阈值 `0.5` 为经验初值，需用真实录音 benchmark 实调（满足「性能优化必须有 benchmark 数据」）。
- 已命中说话人不增量更新向量（多段累积更准），后续可加 re-add 更新 + sample_count 递增。
- 自动登记按 ASR `spk_N` 分组，依赖 ASR diarization 准确率；若 ASR 把同人标成不同 `spk_N`，会重复登记（后续可用 WeSpeaker 二次聚类兜底）。
- TOS presigned URL TTL 1h，长音频（>1h 处理）极端情况下可能过期，重试会重新上传+submit，可接受。
- 录入仅文件上传；录音录入（复用 record tab 的 MediaRecorder）为后续增强。

## 13. 补遗：增量需求（2026-08-22 续，落地后追加）

原 spec/plan 落地后，用户提出 4 项增量需求 + 一个线上问题，均已实现。本节补充记录，与上文不冲突处以上文为准。

### 13.1 ASR 默认切 realtime + diarization prompt（文件 ASR 配额受限）

线上实测：异步文件 ASR `POST /v1/audio/asr/file/submit` 返回 `quota_exceeded`（账号配额耗尽）；加 Step Plan 前缀 `/step_plan/v1/audio/asr/file/submit` 返回 404（文件 ASR 端点不在 Step Plan 下）。故默认 ASR 由文件接口切回 **realtime `stepaudio-2.5-realtime`（Step Plan WSS）+ diarization prompt**：

- 端点 `wss://api.stepfun.com/step_plan/v1/realtime?model=stepaudio-2.5-realtime`，配额可用、与文件 ASR 不冲突。
- 指令让模型按 `[spkN][开始秒-结束秒]说话内容` 模板输出（秒·2 位小数，spk0/spk1 按出场顺序）。
- `provider.ParseTimedSpeakerTranscript` 解析（兼容模型省略时间段第二层方括号，输出 `[spk0]0.00-4.15内容`），spk→`SpeakerLabel`、秒→`StartMS/EndMS`。
- **免 TOS**（base64 pcm 直传 WSS），realtime 模式下 ASR 不需 TOS 凭据。
- config 增 `ASRProvider`（`realtime` 默认｜`file` 可切回，原生 diarization+ms 时间戳更准但受配额限制）。main 按开关装配：realtime 免 TOS；file 需 TOS。
- 权衡：realtime 的时间戳与说话人标签为模型 prompt 生成（非原生），精度依赖模型，不如文件 ASR 准。文件 ASR 配额恢复后可 `ZW_ASR_PROVIDER=file` 切回。

### 13.2 声纹 tab（说话人名册管理页）

新增前端「声纹」tab（nav，紧邻录音），管理全部说话人：列表（色块 chip + 名 + 来源徽标「手动录入/自动登记」+ 样本数）、`＋录入`、行内 ✎ 改名、✕ 删除。复用 `GET/POST/PATCH/DELETE /api/speakers` 与 Task 16 的 `allSpeakers`/`enrollForm`/`submitEnroll`/`startRenameSpeaker`/`commitRenameSpeaker`/`askDeleteSpeaker` 绑定。

### 13.3 每个声纹点开关联录音 + 按时间段播放

- 后端新增 `GET /api/speakers/{id}/segments` → `TranscriptRepo.ListSpeakersBySpeaker` 跨 session 取该说话人片段（JOIN transcript→audio_session），每条含 `session_id/filename/created_at` + 段文本与 `start_ms/end_ms`。音频经 `GET /api/sessions/{session_id}/audio` 播放，不外泄 `storage_path`。
- 前端：声纹卡片可展开 → 按录音分组列片段 → 每片「▶ 播放」按钮。共享 `<audio>` 元素：`playSpeakerSegment` 设 src（跨录音切源时等 `loadedmetadata` 再 seek）、seek 到 `start_ms/1000`、播放，`onVoiceAudioTimeUpdate` 在 `currentTime ≥ end_ms/1000` 时暂停。同录音片段复用已加载音频（只 seek）。

### 13.4 录入支持麦克风直录 + 提示文字照念

录入表单（声纹 tab + 时间线面板）加 `🎤 麦克风录入`：独立 `MediaRecorder`（不与录音 tab 的 `recorder` 冲突），录完产 webm `File` → `enrollForm.file` → 走原 `POST /api/speakers` 路径（后端 ffmpeg 转 wav16k）。表单显示一段样本提示文字（约 15s 自然语速）供照着念，便于录到足够人声。拖拽文件作备选保留。（§12 末「录音录入为后续增强」已完成。）

### 13.5 实现状态

全部落地、构建绿、相关单测/集成测试通过。增量提交：`6e9dfc9`（segments 端点）、`0143bcd`（realtime+prompt ASR）、`d87809f`（声纹 tab）、`39e82dc`（docs）、`3b73b54`（麦克风录入）。评审抓出的 Important 项（reextract 重跑 speaker stage 覆盖手动换人、Enroll 吞 Add 错误）已修+补 `TestStageSpeakerIdempotentSkip`（commit `c2846d7`）。真实 WeSpeaker 加载 / 阈值 benchmark / spikes+e2e 真跑 仍为手动 follow-up。

### 13.6 同人合并（session 内聚类）+ 原始 ASR 详细视图

用户反馈：realtime prompt 式 diarization 把同一人标成多个 `spk_N`（时间线被拆开）。两处修复：

- **session 内声纹聚类**（`stage_speaker.go` `runSpeakerStage` 重构）：原流程「按 spk 分组→每组各自 1:N 登记」会在 ASR 拆错时重复登记同人。改为「逐组切片提向→组代表声纹→**session 内按余弦相似度 ≥ 阈值聚类合并同人 spk 标签**（贪心并查，每 session 说话人数极少 O(n³) 可接受）→每聚类聚代表→跨 session 1:N/登记→回填该聚类所有组段」。加 `cosineVec`（L2 归一向量内积=余弦）。幂等性不变（组内全解析仍跳过）、`SetSegmentSpeaker` 仍只填 NULL。补 `TestStageSpeakerClustersSamePerson`（fake 同向量→2 标签聚成 1 speaker），既有 3 测改用逐段正交 one-hot 向量避免被误聚类。
- **原始 ASR 详细视图**（前端）：`segmentView` 增 `SpeakerLabel`（ASR 原始 spk 标签，未聚类前的模型输出）字段，`GetSession` 填充；转写详情头部加「详细」按钮 toggle `rawAsrView`，展开只读视图逐段显示 `spk{speaker_label}` + `start_ms→end_ms`（`fmtSec` 毫秒→秒·3 位小数）+ 文本，便于排查 diarization 拆分根因。

**重要前置条件**：聚类需真实 WeSpeaker 提取向量才真正生效——当前 `StubEmbedder` 随机/伪向量聚不出（同人会因向量不相似而仍被拆开）。realtime prompt 式 diarization 标签不稳是拆分根因；加载真实 WeSpeaker 后聚类兜底才见效。属手动 follow-up（同 §12/§13.5）。
