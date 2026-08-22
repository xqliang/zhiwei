# 知微 Zhiwei · 云端 MVP

AI 全时生活记忆与个人智能体的云端服务。当前为 **Sprint 0-1**：音频上传/录音 → ASR 转写（含说话人切分）→ 时间线展示。

- 产品文档：`product/知微 Zhiwei——AI 全时生活记忆与个人智能体产品需求与技术方案 V1.0.md`
- 架构文档：`architecture/Zhiwei_AI_Agent_Memory_Architecture_v1.0.md`
- 设计文档：`docs/superpowers/specs/2026-08-18-zhiwei-cloud-mvp-design.md`
- 实现计划：`docs/superpowers/plans/2026-08-18-zhiwei-sprint0-1.md`
- ASR 协议实测笔记：`docs/superpowers/specs/asr-protocol-notes.md`

## 环境要求

- Go 1.26+
- Docker（跑 MySQL）
- ffmpeg（音频转码）
- golang-migrate CLI（`brew install golang-migrate`）
- 环境变量：
  - `ARK_API_KEY`（必填，火山方舟，LLM 用）
  - `STEPFUN_API_KEY`（ASR 用，可放 `.env` 文件，已被 gitignore）
  - `TOS_ACCESS_KEY` / `TOS_SECRET_KEY`（`ZW_ASR_PROVIDER=file` 时必填，火山引擎 TOS 对象存储，文件 ASR 需上传音频换公网 URL；可放 `.env`）
  - `ZW_ASR_PROVIDER`（`realtime` 默认｜`file`；realtime 走 Step Plan WSS + diarization prompt 免 TOS，文件 ASR 原生 diarization+ms 时间戳更准但受配额限制）
  - `ZW_VOICEPRINT_SIDECAR_URL`（声纹 sidecar 地址，默认 `http://127.0.0.1:8010`）
  - `ZW_VOICEPRINT_THRESHOLD`（1:N 余弦匹配阈值，默认 `0.5`，需用真实录音 benchmark 实调）

## 快速开始

```bash
# 1. 起 MySQL（本机 3307 端口，容器名 zhiwei-mvp-mysql，避免与其他项目冲突）
make compose-up

# 2. 建表
make migrate-up

# 3. 起声纹 sidecar（说话人识别需要）
#    首次：建 venv + 装依赖（torch/wespeaker 等，需 python3.12）：
bash scripts/setup-voiceprint.sh
#    日常：
make sidecar-start

# 4. 起服务（会读取 .env 里的密钥）
set -a; source .env; set +a
make dev
# 打开 http://localhost:8080 —— 时间线 / 录音 两个标签页
```

> 启动顺序：MySQL → 声纹 sidecar → 服务。sidecar 未起时转写仍可用，但说话人解析 stage 会失败重试（不丢转写）。

## 运行测试

```bash
# 单元测试（无需 MySQL，Provider 全部 mock）
make test

# 集成测试（自动重建 zhiwei_test 库 + 迁移 + 真连 MySQL）
make test-integration

# e2e 冒烟（起服务 + 上传真实语音 + 轮询到转写完成）
make e2e
# 或指定音频：bash scripts/e2e.sh testdata/speech.wav
```

AI 真实调用验证（手动，不进 CI，避免测试烧钱）：

```bash
make spike-llm    # 火山 Ark LLM
make spike-embed  # Ark Embedding（当前账号 403，需控制台开通）
make spike-asr    # StepFun realtime 转写
```

## 常用命令

| 命令 | 说明 |
|---|---|
| `make build` | 编译到 `bin/zhiwei-server` |
| `make compose-up / down` | 启停 MySQL |
| `make migrate-up / down` | 数据库迁移（golang-migrate） |
| `make init-testdb` | 重建集成测试库 |
| `make dev-start / dev-stop / dev-restart` | 后台启停调试进程（另有 `dev-status` / `dev-logs`） |

## API 一览

```text
GET  /api/health            健康检查
POST /api/audio             上传音频（multipart：file + source=web_upload|web_record）
GET  /api/sessions          会话列表（含处理状态）
GET  /api/sessions/{id}     会话详情：转写分段（带说话人标签+解析到的说话人名）+ speakers 列表
POST /api/jobs/{id}/retry   失败任务重跑
GET/PATCH /api/memories      记忆列表（type/topic_id 过滤）/ 修正与忽略
GET/PATCH /api/todos         待办列表 / 状态流转（确认/完成/忽略）
GET/POST/PATCH /api/topics   主题计数列表 / 新建 / 确认/改名/忽略
GET/POST/PATCH/DELETE /api/speakers  说话人名册 / 录入声纹(multipart file+name) / 改名 / 删除
GET  /api/sessions/{id}/speakers     会话内已解析说话人列表（面板用）
PATCH /api/sessions/{id}/segments/{seg}/speaker  单段换人（手动纠正说话人）
```

## 项目结构

```text
cmd/zhiwei-server/   服务入口（HTTP API + 异步 worker 同进程）
internal/api/        REST handler
internal/pipeline/   任务状态机 + worker 池 + 各处理阶段
internal/provider/   AI 能力抽象（ASR / LLM / Embedding，可替换实现）
internal/repo/       MySQL DAO（sqlx）
internal/ids/        雪花 ID（JSON 序列化为字符串）
internal/config/     环境变量配置
migrations/          SQL 迁移（9 张表一次到位）
web/                 Vue 3 单页（本地 vendor，无构建）
scripts/e2e.sh       e2e 冒烟脚本
```

## 处理链路

```text
上传/录音 → data/uploads 落盘 + audio_session/pipeline_job 入库
  → [worker 池] asr 阶段：ffmpeg 转 wav16k → ASR → transcript/segments 落库
  → [worker 池] segment 阶段：汇总全文
  → session 置 completed，时间线可查看
```

每个阶段失败自动重试 3 次，超限进 `failed` 可在页面上重跑；服务重启会恢复中断的任务。

## 已知限制与下一步

- ASR 用 StepFun `stepaudio-2.5-realtime`（火山 `doubao-seed-asr-2-0` 凭证待开通，开通后新增 Provider 替换即可，业务代码不动）
- ASR 无逐段时间戳，说话人切分依赖模型输出的 `[说话人N]` 前缀
- Sprint 2（Memory 抽取 / Todo / Topic）、Sprint 3（检索 / Agent 问答）、Sprint 4（Daily Review）见实现计划系列
