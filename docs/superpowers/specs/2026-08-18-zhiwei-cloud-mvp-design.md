# 知微云端 MVP 设计文档

- 日期：2026-08-18
- 状态：已确认
- 上游文档：`product/知微 Zhiwei——AI 全时生活记忆与个人智能体产品需求与技术方案 V1.0.md`、`architecture/Zhiwei_AI_Agent_Memory_Architecture_v1.0.md`
- 本文档范围：知微的**第一个子项目**——云端 MVP 闭环。硬件、Flutter App、Topic/Project/Risk/Person、多用户体系均不在本期范围。

---

## 1. 目标与非目标

### 1.1 目标

打通架构文档第 63 节定义的「第一条完整闭环」，并附加两项最贴近用户价值的功能：

```text
音频录入（Web 录音 / 文件上传）
  → 火山 Ark ASR（doubao-seed-asr-2-0，含说话人分离）
  → Memory 抽取（LLM 结构化抽取 + 质量闸门）
  → MySQL 入库 + Embedding
  → 混合检索
  → Agent 问答（带证据引用）
  → Todo 提取 / Daily Review
```

用户在浏览器里录一段话或上传一个音频文件，几分钟后能在时间线看到带说话人标签的转写和提取出的记忆卡片，能向知微提问并得到带引用的回答，每天晚上自动生成当日回顾。

### 1.2 非目标（明确的下一期内容）

- 硬件接入（BLE、VAD、Opus 分段）
- Flutter App、消息推送
- Person / Topic / Project / Risk / 声纹库 / Entity Resolution
- Memory Consolidation、用户纠错学习、多用户认证
- AI 漫画、Weekly Review、Personal Insight

---

## 2. 已确认的关键决策

| 决策点 | 结论 | 理由 |
|---|---|---|
| 首期范围 | 云端 MVP 闭环 + Todo + Daily Review | 架构文档第 63 节推荐的首条链路，能完整验证产品核心价值「它居然记得」 |
| 技术栈 | Go 模块化单体（单二进制） | 目录即未来微服务边界；本地开发零重依赖；与架构文档 Go 目标态一致 |
| 数据库 | MySQL 8（docker-compose） | 与架构文档目标态一致 |
| 异步编排 | DB 任务表 + 进程内 Worker 池 | 零额外基础设施，重启可续跑，任务表即 trace 雏形；未来可平滑升级 Asynq/MQ |
| 音频入口 | Web 录音 + 文件上传 + REST API | 硬件缺席期的替代入口，浏览器 MediaRecorder 可模拟全天候采集 |
| 用户体系 | 单用户免登录（内置 user_id=1） | 无真实多用户需求；数据模型保留 user_id 便于扩展 |
| AI 服务 | 全部走火山 Ark，共用 ARK_API_KEY | 用户已有凭证；业务侧经 Provider 接口抽象，不绑定火山 |

### 2.1 模型选型（均经火山 Ark，Bearer 鉴权）

| 能力 | 模型 | 用途 |
|---|---|---|
| ASR | `doubao-seed-asr-2-0` | 流式识别 + 说话人分离 |
| LLM（Tier 1） | `doubao-seed-1.6-flash` | Memory 抽取、查询解析等低成本高频任务 |
| LLM（Tier 2） | `doubao-seed-1.6` | Agent 问答、Daily Review |
| Embedding | `doubao-embedding-large` | Memory 向量化 |

> 注：ASR 具体报文格式以火山官方文档 + Sprint 0 Spike 实测为准（官方文档页为 JS 渲染，无法离线确认）。

---

## 3. 总体架构

### 3.1 系统形态

单个 Go 二进制 `zhiwei-server`，内置：

- HTTP API（chi router）+ Web 静态页托管
- 异步处理 Worker 池（pipeline 状态机）
- 定时任务（Daily Review，robfig/cron）

Docker Compose 只拉起 MySQL 与 zhiwei-server（镜像内置 ffmpeg 用于音频转码）。

### 3.2 处理链路

```text
[同步] Web 录音/文件上传 → POST /api/audio
        → 音频落盘 data/ + audio_session 入库
        → 创建 pipeline_job（stage=asr, status=pending）→ 返回 session_id/job_id

[异步 Worker 池] 状态机依次执行：
  ① asr      ffmpeg 按需转码 → doubao-seed-asr-2-0 流式识别（开说话人分离）
             → 落 transcript + transcript_segment
  ② segment  连续同 speaker 片段聚合为对话块；
             无有效文字的会话直接标记完成（低价值不进抽取）
  ③ extract  每个对话块送 doubao-seed-1.6-flash，输出 JSON 候选：
             type/title/content/epistemic_type/importance/confidence/is_todo/todo_due
  ④ quality  纯规则闸门（不调模型）：
             confidence < 0.6 → 丢弃
             todo 且 confidence < 0.85 → 降级为 suggested（「要不要加入 Todo？」）
  ⑤ commit   memory 批量入库 + 批量 embedding + todo 入库，单事务提交
```

每日 22:00 cron 触发 Daily Review 生成（也可手动触发）。

### 3.3 失败处理与可观测性

- 每个 stage 独立重试：网络/模型类错误指数退避重试 3 次，之后 job 进 `failed`，Web 可一键重跑（`POST /api/jobs/{id}/retry`，attempt 归零重入队列）
- 服务重启扫描 `pending/running` job 继续执行，任务不丢
- job.trace 记录各阶段耗时、模型名、prompt 版本、token 用量、错误——对应架构文档 §55 的可观测性要求
- Provider 超时：ASR 120s / LLM 60s / Embedding 30s

### 3.4 代码结构

```text
zhiwei-glm53/
├── cmd/zhiwei-server/main.go      # 入口：装配各模块
├── internal/
│   ├── api/                       # HTTP handler（REST + 静态页）
│   ├── pipeline/                  # job 状态机 + worker 池 + 各 stage
│   ├── provider/                  # Ark ASR / LLM / Embedding 客户端（接口抽象）
│   ├── memory/                    # memory 领域逻辑（抽取编排、质量闸门、检索）
│   ├── todo/                      # todo 领域逻辑
│   ├── review/                    # daily review 生成
│   ├── agent/                     # agent chat：检索→上下文组装→LLM→引用校验
│   ├── repo/                      # MySQL DAO（sqlx）
│   └── config/                    # 配置（env 读取）
├── web/                           # 前端（Vue 3 CDN 单页，无构建步骤）
├── prompts/                       # prompt 模板（extraction_v1.md / review_daily_v1.md / agent_v1.md / query_parse_v1.md）
├── migrations/                    # SQL 迁移（golang-migrate）
├── deploy/docker-compose.yml      # MySQL + zhiwei-server
├── data/                          # 音频文件存储（gitignore）
└── docs/superpowers/specs/
```

---

## 4. 数据模型

8 张表，全部保留 `user_id`（MVP 默认 1）。DDL 细节实现时按下列字段落地：

```text
audio_session(id, user_id, source[web_upload|web_record], filename,
  storage_path, duration_ms, mime, status[uploaded|processing|completed|failed],
  created_at, updated_at)

pipeline_job(id, session_id, stage[asr|segment|extract|quality|commit|done],
  status[pending|running|failed|done], attempt, last_error, trace JSON,
  created_at, updated_at)

transcript(id, user_id, session_id, language, full_text, confidence, created_at)

transcript_segment(id, transcript_id, sequence_no, speaker_label,
  text, start_ms, end_ms, confidence, created_at)

memory(id, user_id, type[event|fact|decision|idea|problem|preference],
  title, content, epistemic_type[observed|inferred|suggested],
  importance DECIMAL(5,4), confidence DECIMAL(5,4),
  session_id,                    -- provenance：来源会话
  transcript_segment_ids JSON,   -- provenance：具体说话片段
  event_at, status[active|superseded|dismissed],
  embedding BLOB, version INT DEFAULT 1,
  created_at, updated_at)

todo(id, user_id, title, source_memory_id,
  status[suggested|confirmed|done|dismissed], due_at, confidence,
  created_at, updated_at)

daily_review(id, user_id, review_date, content JSON,
  status[pending|ready|failed], created_at)

agent_message(id, user_id, role, content, citations JSON, created_at)
```

### 4.1 与架构文档的差异

- 不建独立 `memory_source` 表：provenance 简化为 `session_id + transcript_segment_ids`（MVP 一对一来源够用，多来源时再拆表）
- 不建 `memory_version` / `user_correction`（纠错学习下一期），`version` 字段占位
- Speaker 仅保留 ASR 标签（说话人 1/2/3），Person 实体与声纹库下一期
- embedding 与 memory 同库同表（BLOB），进程内暴力余弦检索（个人规模 <10 万条，毫秒级）

---

## 5. AI Provider 层

业务代码只依赖接口，不绑定火山：

```go
type ASRProvider interface {
    Transcribe(ctx context.Context, audioPath string) ([]TranscriptPiece, error)
}
type LLMProvider interface {
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) // 支持 JSON 结构化输出
}
type EmbeddingProvider interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```

- 鉴权：`ARK_API_KEY` 环境变量，HTTP Bearer / WebSocket header
- 重试：网络类错误 3 次指数退避；调用结果记入 job.trace
- `TranscriptPiece{SpeakerLabel, Text, StartMs, EndMs, Confidence}`

---

## 6. 检索与 Agent 问答

### 6.1 混合检索（架构文档 §22-24 的简化版）

```text
用户查询
  → LLM（flash）解析：语义 query + 时间范围 + 关键词
  → 并行召回：
      ① 向量余弦（进程内，top 50）
      ② MySQL 关键词匹配（top 50）
      ③ 时间过滤（event_at 落在解析出的范围）
  → 合并去重
  → 评分：0.5*语义 + 0.2*关键词 + 0.15*时间接近度 + 0.15*importance
  → top 10 进入上下文
```

### 6.2 Agent 问答流程

```text
POST /api/agent/chat
  → 检索（6.1）
  → 上下文组装：system prompt（人设 + 引用要求）
    + 最近 3 轮对话 + 检索到的 memory（含 id/时间/说话人标签）
  → doubao-seed-1.6，要求 JSON 输出 {answer, citations:[{memory_id, reason}]}
  → citation 校验：剔除指向不存在 memory_id 的引用（防幻觉）
  → 落 agent_message，返回
```

前端在回答下方展示证据引用，点击展开对应 transcript 原文。

### 6.3 Daily Review

- 每日 22:00 cron / 手动触发
- 输入：当日 memory + todo 变化 + 对话概况
- 输出结构化 JSON：`headline / highlights / todos / decisions / insights / tomorrow`
- 「明天建议」只引用当天 `confirmed` 未完成 todo，不凭空生成

---

## 7. API 设计

```text
POST   /api/audio              上传音频（multipart）→ {session_id, job_id}
GET    /api/sessions           会话列表（含处理状态）
GET    /api/sessions/{id}      详情：转写全文 + 分段（说话人标签）+ 关联 memory/todo
POST   /api/jobs/{id}/retry    失败任务重跑

GET    /api/memories           列表（时间/类型过滤）
PATCH  /api/memories/{id}      修正内容（version+1）或 dismiss
GET    /api/todos              列表
PATCH  /api/todos/{id}         confirmed / done / dismissed

POST   /api/agent/chat         问答 → {answer, citations}
GET    /api/reviews/today      今日回顾（无则触发生成）
POST   /api/reviews/generate   手动生成

GET    /api/health             健康检查
```

## 8. Web 界面

Vue 3（CDN 引入）单页应用，无构建步骤。四个标签页：

1. **时间线**：会话列表 → 转写（说话人分色）→ memory 卡片 / todo 卡片
2. **录音**：MediaRecorder 录音 + 文件拖拽上传，轮询 job 进度
3. **问知微**：聊天界面，回答带可点击证据引用
4. **今日**：Daily Review 展示

---

## 9. 测试策略

| 层级 | 内容 | 是否进 CI |
|---|---|---|
| 单元测试（重点） | pipeline 状态机、quality 闸门规则、检索评分与合并、citation 校验 | 是（纯逻辑，无外部依赖） |
| Provider mock 测试 | 接口 mock 下的编排逻辑 | 是 |
| Ark 真实调用 Spike | Sprint 0 用真实 key 验证 ASR WebSocket 协议、LLM/Embedding 报文 | 否（`make spike` 手动执行，避免 CI 烧钱） |
| 端到端冒烟 | 固定测试音频走完整 pipeline，断言产出 memory | 否（`make e2e`，需本地 MySQL + key） |

迁移由 golang-migrate 管理，开发期可 up/down 重置。

---

## 10. Sprint 拆分（供实现计划参考）

1. **Sprint 0：Spike + 骨架**——Ark 三接口真实调用验证（尤其 ASR 协议）；Go 项目骨架、docker-compose、迁移框架跑通
2. **Sprint 1：音频 + ASR pipeline**——上传 API、任务表 + worker 池、ASR/segment stage、时间线页（转写展示）
3. **Sprint 2：Memory 抽取 + Todo**——extract/quality/commit stage、memory/todo API 与卡片 UI、dismiss/confirm 交互
4. **Sprint 3：检索 + Agent**——embedding、混合检索、agent chat、证据引用展开
5. **Sprint 4：Daily Review + 打磨**——cron、Review 页、失败重跑 UI、e2e 冒烟

每个 Sprint 结束时系统处于可运行、可演示状态。

---

## 11. 风险与开放问题

| 风险 | 应对 |
|---|---|
| `doubao-seed-asr-2-0` 的 Ark 调用协议未实测（官方文档 JS 渲染无法离线确认，说话人分离输出格式待验证） | Sprint 0 首个任务即 Spike；ASR 经 Provider 接口隔离，最坏情况换火山语音技术 API 或本地模型，业务代码不动 |
| 浏览器录音格式（webm/opus）ASR 兼容性 | ffmpeg 统一转码 wav 16k mono 再送 ASR |
| Memory 抽取质量（垃圾卡片 / 漏抽） | quality 闸门阈值可配置；prompt 版本化，迭代不改代码 |
| MySQL 全文检索中文分词效果一般 | MVP 接受 LIKE + ngram；检索主力是向量，关键词仅辅助 |
