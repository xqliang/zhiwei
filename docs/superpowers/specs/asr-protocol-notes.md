# ASR 实测协议笔记（2026-08-18）

## 2026-08-22 更新：改用异步文件 ASR（说话人声纹功能）

说话人识别功能需要「每段时间戳 + 说话人标签」，realtime 协议均无原生时间戳/说话人分离。
调研 StepFun 全部 ASR 接口后（见设计 `2026-08-22-speaker-voiceprint-design.md` §2.1），
**改用异步文件 ASR**（唯一同时提供原生时间戳 + 说话人分离的接口）：

- 端点：`POST https://api.stepfun.com/v1/audio/asr/file/submit` + `POST /v1/audio/asr/file/query`
- 鉴权：`Authorization: Bearer $STEPFUN_API_KEY`
- model：`stepaudio-2.5-asr`
- 入参（submit body）：`{audio:{format:"wav",channel:1,rate:16000,url:<公网URL>}, request:{model_name, show_utterances:true, enable_speaker_info:true}}`
  - **`audio.url` 只接受公网 URL，不支持 base64/直传** → 本项目用火山引擎 TOS 上传音频（私有 + presigned GET URL，1h TTL，识别后删）。
- 轮询 query：`{task_id}` → `status: PENDING|RUNNING|SUCCEEDED|FAILED`，完成后 `result[].utterances[]` 每句含：
  - `text`、`start_time`(ms)、`end_time`(ms) — **原生 ms 级时间戳**（满足"秒数 3 位小数"：ms/1000=x.xxx）
  - `speaker.id` 形如 `spk_1`/`spk_2` — **原生说话人分离**，任务内稳定、跨任务不保证，最多 10 说话人
- 解析：`provider.ParseFileASRResult(raw)` 把 `spk_N` 去前缀得说话人标签 `N`，ms→StartMS/EndMS。

> realtime `stepaudio-2.5-realtime` 的 Provider 代码保留（`StepFunASR`），但主链路装配已切到 `StepFunFileASR`。
> realtime 无原生时间戳/说话人，仅作回退/历史参考。

## 结论：当前可用 ASR = StepFun stepaudio-2.5-realtime（WSS，OpenAI Realtime 协议）

### 火山 Ark（不可用，待开通）

- `ARK_API_KEY` 可调：LLM（`doubao-seed-1-6-flash-250828`、`doubao-seed-2-0-pro-260215` 已验证 200）
- `ARK_API_KEY` **不可调 ASR**：`/api/v3/models`（130 个模型）无任何 ASR 模型；`/api/v3/*` 无 ASR 路由
- 语音技术端点 `openspeech.bytedance.com/api/v3/sauc/bigmodel` 存在且 `volc.bigasr.sauc.duration` 是有效 resource id，但鉴权走语音技术控制台的 APPID+Access Token（401 grant not found）
- `plan/tts/unidirectional`（用户提供的示例）为 TTS 端点，且该 key 无 TTS 授权（转发 Ark 校验 401）
- **待办**：用户在火山控制台开通 doubao-seed-asr-2-0 或语音技术后，新增 ArkASR Provider 替换

### StepFun stepaudio-2.5-realtime（已验证可用）

- 端点：`wss://api.stepfun.com/step_plan/v1/realtime?model=stepaudio-2.5-realtime`
- 鉴权：`Authorization: Bearer $STEPFUN_API_KEY`（.env）
- 协议：OpenAI Realtime 风格 JSON 事件（文本帧）
- 流程：
  1. 连接后收 `session.created`
  2. 发 `session.update`：`modalities=["text"]`、`instructions`=转写指令、`input_audio_format="pcm16"`
  3. 收 `session.updated`
  4. 分块发 `input_audio_buffer.append`（`audio`=base64(pcm16 s16le 16k mono)，100ms≈3200B 一片，片间 sleep 30ms）
  5. `input_audio_buffer.commit` → 收 `input_audio_buffer.committed`
  6. `response.create` → 收 `response.audio_transcript.delta`（增量文本）直到 `response.done`
- 转写指令（关键）：要求"逐字转写引擎，只输出原文，多说话人用 [说话人1] [说话人2] 前缀"
- 实测（macOS say 生成的中文语音）：逐字正确 ✓
- 输入必须是**裸 PCM**（跳过 wav 44 字节头）

### 已知限制

- 无逐段时间戳：整个音频一个响应，segment 按解析 `[说话人N]` 前缀切分，start/end 置 0
- 说话人分离靠指令而非模型原生 diarization，多人准确率待真实录音验证
- 每次转写新建会话（无会话复用），长音频未测（>1 分钟待验证）
- **指令措辞陷阱（已踩坑）**：「你是逐字转写引擎…」角色设定式指令会让模型把指令本身复读成转写结果；
  必须用任务式表述（"你的任务是语音转文字，请把音频里…完整逐字转写出来"）。已固化在 provider。
- 模型偶发行首输出 `[HH:MM]` 时间码前缀，解析时清洗（timePrefix 正则）
