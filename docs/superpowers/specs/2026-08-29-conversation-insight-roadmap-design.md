# 对话洞察增强总纲：情绪 · 声学环境 · 深度总结 · 漫画

- 日期：2026-08-29
- 类型：**总纲/路线图 spec**（多子系统功能的全局图；每个子项另有自己的 spec→plan→实现）
- 分支：main（各子项实现时按需起 worktree）
- 缘起：用户希望「总结功能」不再是干巴巴的罗列，要**有深度、好玩**——抽取细微规律/微情绪/状态；ASR 时识别当时环境（开会/开车/餐厅/地铁/户外…、背景声、天气线索）；给出每人整体情绪/精神状态并**关联到语音条目**；总结据此推断；并插入 5-8 格相关漫画。

## 1. 愿景（North Star）

把「录音 → 转写 → 罗列式总结」升级为「录音 → **多维感知**（说了什么 + 什么情绪 + 什么环境）→ **洞察式叙事总结 + 漫画**」：让报告像一个懂你的观察者写的，而非事项清单。

## 2. 可行性结论（2026-08-29 实测 spike，见 memory [[audio-understanding-and-image-gen-config]]）

两个关键能力**均已实测可行**：

- **音频理解（情绪 + 声学场景）✅**：`stepaudio-2.5-chat` 能真正"听"声音（非转写）——实测输出背景干净度/音色/氛围判断；加强硬 system 提示 + 明确 JSON schema 后**稳定吐可解析 JSON**（speakers[{emotion,micro_emotion,mental_state}] / acoustic_scene / background_sounds[] / weather_cues / overall_mood / confidence）。通道：`STEPFUN_ASR_FILE_API_KEY` + `https://api.c.ibasemind.com/v1`（有额度；主账号 STEPFUN_API_KEY 已欠费 402）。请求走 `/chat/completions` 的 `input_audio{data:base64,format:wav}`。
  - **局限**：spike 用合成 TTS 样本，真实录音质量待验；对**逐段**情绪需决定是整段音频归因还是切片送模型（见 P1 待决）。
- **文生图（漫画）✅**：`doubao-seedream-4-0-250828`（`ARK_API_KEY` + `/api/v3/images/generations`）实测出图，返回 TOS URL。⚠️ 仅此 id 可用，带 `-t2i-` 的旧 id 全 404。

## 3. 现状（调研实证）

- **ASR 管线**（`internal/pipeline`）：stage 顺序 `asr → segment → speaker → speakername → extract [→ profile]`，按名注册 handler 顺序跑（`stage_asr.go:BuildStages` / `main.go:231`）。
- **段数据模型** `repo.TranscriptSegment`（`transcript.go:24`）：有 speaker/text/start_ms/end_ms/embedding/纠正字段，**无情绪、无环境字段**。
- **情绪时序平面已存在**：`PersonMetric`（`person_metric.go`）catalog 含 `emotion` 键（value_num −1..1 / value_text 类别）——C 子项可衔接复用，不另起炉灶。
- **报告子系统** `internal/review`：`gather` 汇聚 repo 数据 → `prompt` → Ark LLM（doubao）→ 结构化 `DailyContent/WeeklyContent` JSON（`types.go`）→ 落库 + 前端手绘 SVG 曲线渲染。当前**纯文本/计数维度**，无情绪/环境/叙事/漫画。

## 4. 拆分：5 个子项

| 子项 | 范围 | 产出 | 依赖 |
|------|------|------|------|
| **A. 声学环境识别** | ASR 后加一路 `stepaudio-2.5-chat`：识别会话的声学场景、背景声、天气线索、整体氛围 | 会话级环境标签 | 管线 |
| **B. 逐条情绪** | 同一次音频理解调用，产出每位说话人/每段的情绪·微情绪·精神状态 | 段级（或说话人级）情绪，关联到语音条目 | 与 A 同源 |
| **C. 人物情绪汇总** | 把 B 的逐段情绪聚合成每人「整体情绪/精神状态」，衔接 `PersonMetric.emotion` 时序 | 人物情绪画像 | B |
| **D. 总结深度增强** | 报告消费 A/B/C：推断细微规律/微情绪/状态，叙事化、少罗列 | 更深的日/周报 | A/B/C |
| **E. 漫画生成** | 从报告叙事派生 5-8 格场景 → Seedream 4.0 出图 → 插入报告 | 报告内漫画 | D（场景来自叙事）|

**A+B 合并为一个子项**：两者同源于一次 `stepaudio-2.5-chat` 调用，拆开会重复调用、翻倍成本。

## 5. 分期与数据流

```
P1  A+B 音频理解地基
      录音 → [asr → segment → speaker → speakername] → (新)audioscene stage
        └ stepaudio-2.5-chat(整段音频 + 说话人时间线) → 结构化 JSON
            → 落库：会话级 acoustic_scene/background_sounds/weather_cues/overall_mood
                    + 段级(或说话人级) emotion/micro_emotion/mental_state
P2  C 人物情绪汇总
      段级情绪 → 按人聚合 → PersonMetric.emotion 时序 + 人物情绪画像字段
P3  D 报告深度增强
      gather 扩充(带 A/B/C 信号) → prompt 改造(要求洞察/规律/叙事) → 结构化+叙事字段
P4  E 漫画
      D 的叙事 → LLM 派生 5-8 格分镜 prompt → Seedream 4.0 批量出图 → 存 URL → 报告渲染
```

依赖链：**A+B → C → D → E**（E 的分镜来自 D 的叙事；C 喂 D 的人物维度）。P1 是一切地基。

## 6. 数据模型草图（细节留各子 spec 定）

- **会话级环境**（A）：`transcript` 或 `session` 加列 `acoustic_scene VARCHAR` / `background_sounds JSON` / `weather_cues VARCHAR` / `overall_mood VARCHAR`；或新表 `session_acoustic`。（P1 待决：扩列 vs 新表）
- **段级情绪**（B）：`transcript_segment` 加 `emotion` / `micro_emotion` / `mental_state` 列；或新表 `segment_emotion`（段:情绪 1:1）。（P1 待决：粒度=逐段 vs 逐说话人-轮次）
- **人物情绪**（C）：复用 `PersonMetric`（metric_key=emotion），可能加 `mental_state` 类别；人物级画像字段另议。
- **报告叙事 + 漫画**（D/E）：`DailyContent/WeeklyContent` 加 `narrative`/`patterns`/`micro_emotions`/`comic[]{scene,image_url,caption}` 字段。

## 7. 每个子项的待决问题（在各自 spec 里解决）

- **P1（A+B）**：逐段情绪粒度（每段送切片 vs 整段音频让模型按说话人/时间归因）；扩列 vs 新表；音频理解 stage 的位置（segment 后 vs speakername 后）；用代理 key 还是给主账号充值；失败降级（音频理解失败不阻断转写主流程）；成本/时延（每会话多一次大模型音频调用）。
- **P2（C）**：情绪聚合算法（均值/众数/加权时间衰减）；与既有 PersonMetric.emotion 写入路径的关系（避免重复/冲突）。
- **P3（D）**：叙事 prompt 如何约束"有洞察但不编造"（只基于落库信号推断）；结构化字段 vs 自由叙事的平衡；报告体积/时延。
- **P4（E）**：分镜风格与一致性（人物形象跨格一致是难点）；出图张数与成本/时延；同步出图 vs 后台异步补图；图片存储（TOS URL 时效 vs 落本地/自有存储）；不适内容防护。

## 8. 横切关注点

- **成本/时延**：A+B 每会话多一次音频大模型调用；E 每报告 5-8 次文生图。需可开关、可降级、优先异步（不阻断转写/报告主流程）。
- **多用户隔离**：所有新表/字段带 user_id，沿用现有 IDOR 行级过滤。
- **隐私**：情绪/精神状态/环境是敏感推断——需明确仅 owner 可见、可关闭；对齐 person-profile 敏感区（健康周期）的处理惯例。
- **降级**：音频理解 / 文生图任一失败，主流程（转写、基础报告）必须照常完成，新信号缺失即空。
- **迁移号**：各子项加迁移前查 main 最新号（并行分支撞号坑，见 [[zhiwei-db-per-feature-convention]]）。

## 9. 下一步

P1（A+B 音频场景与情绪理解）走完整 brainstorming→spec→plan→实现。本总纲提交后即进入 P1 设计。
