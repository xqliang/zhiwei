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
  - `STEPFUN_ASR_FILE_API_KEY`（ASR 用，`ZW_ASR_PROVIDER=file` 默认方案，可放 `.env`）
  - `STEPFUN_API_KEY`（`ZW_ASR_PROVIDER=realtime` 时用，可放 `.env`）
  - `ZW_STEPFUN_ASR_BASE`（File ASR 前缀，默认 `https://api.c.ibasemind.com/v1`；生产设 `https://api.stepfun.com/v1`）
  - `TOS_ACCESS_KEY` / `TOS_SECRET_KEY`（`ZW_ASR_PROVIDER=file` 时必填，火山引擎 TOS 对象存储，文件 ASR 需上传音频换公网 URL；可放 `.env`）
  - `ZW_ASR_PROVIDER`（`file` 默认｜`realtime`；file 走 StepFun 异步文件 ASR 原生 diarization+ms 时间戳更准，realtime 走 WSS + prompt diarization 免 TOS）
  - `ZW_VOICEPRINT_SIDECAR_URL`（声纹 sidecar 地址，默认 `http://127.0.0.1:8010`）
  - `ZW_VOICEPRINT_THRESHOLD`（1:N 余弦匹配阈值，默认 `0.5`，需用真实录音 benchmark 实调）
  - `ZW_PROFILE_AUTO_CONFIDENCE`（画像 LLM 抽取自动写入的置信阈值，默认 `0.75`）
  - `ZW_PROFILE_EXTRACT_ENABLED`（是否启用画像抽取流水线阶段，默认 `true`）
  - `ZW_PROFILE_EXTRACT_WINDOW`（画像抽取窗口大小，默认 `10`）

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
GET/POST         /api/persons                        人物名册（含 pending 计数）/ 新建
GET/PATCH/DELETE /api/persons/{id}                   详情（分组属性+关系+最近互动）/ 改名·换绑声纹 / 归档
POST             /api/persons/{id}/attributes        手动加属性（source=manual, conf=1.0）
PATCH/DELETE     /api/persons/{id}/attributes/{aid}  手动改值（supersede）/ 删除
POST             /api/persons/{id}/relationships     手动加关系（配偶/子女/上下游/组织…）
DELETE           /api/persons/{id}/relationships/{rid}
GET              /api/persons/{id}/history           修改历史（?entity_kind=&attr_key= 过滤）
GET              /api/profile/pending                确认队列（属性/关系/人物 pending 并集）
POST             /api/profile/pending/{kind}/{id}/confirm|dismiss   确认/放弃
POST             /api/profile/extract                画像抽取/回填（可带 session_id；默认最近 50 个 completed）
```

## 项目结构

```text
cmd/zhiwei-server/   服务入口（HTTP API + 异步 worker 同进程）
internal/api/        REST handler
internal/pipeline/   任务状态机 + worker 池 + 各处理阶段
internal/profile/    用户画像（人物系统）领域逻辑：属性目录/抽取/闸门/确认队列
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
  → [worker 池] profile 阶段：对话块 → LLM 抽取画像事实 → 置信闸门落库
    （高置信直接生效，低置信/冲突进确认队列 /api/profile/pending）
  → session 置 completed，时间线可查看
```

每个阶段失败自动重试 3 次，超限进 `failed` 可在页面上重跑；服务重启会恢复中断的任务。

## 已知限制与下一步

- ASR 用 StepFun `stepaudio-2.5-realtime`（火山 `doubao-seed-asr-2-0` 凭证待开通，开通后新增 Provider 替换即可，业务代码不动）
- ASR 无逐段时间戳，说话人切分依赖模型输出的 `[说话人N]` 前缀
- Sprint 2（Memory 抽取 / Todo / Topic）、Sprint 3（检索 / Agent 问答）、Sprint 4（Daily Review）见实现计划系列
