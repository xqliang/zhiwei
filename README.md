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
  - `ZW_VOICEPRINT_THRESHOLD`（1:N 余弦匹配阈值，默认 `0.8`，需用真实录音 benchmark 实调）
  - `ZW_NAME_INFER_WINDOW_MIN`（说话人名字推断回看窗口，分钟，默认 `10`）
  - `ZW_NAME_INFER_MAX_SEGMENTS`（名字推断上下文段数上限，默认 `400`）
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
# 打开 http://localhost:8080 —— 登录后进入 时间线 / 录音 / 声纹 / 人物 / 主题 / 记忆 / 待办 / 问知微 / 报告
```

> 启动顺序：MySQL → 声纹 sidecar → 服务。sidecar 未起时转写仍可用，但说话人解析 stage 会失败重试（不丢转写）。

## 账号与登录

系统是多用户的，访问需登录（cookie + 服务端 session 鉴权）。**没有硬编码默认密码**——用户名固定播种，口令由你首次启动时引导设置。

- **初始用户**：迁移 `000012_auth` 播种 `id=1`、**用户名 `owner`**、显示名「我」，存量数据（录音/画像等）都归它。其 `password_hash` 初始为空（**空 = 不可登录**）。
- **首次设 owner 口令**：在 `.env`（或环境）里配 `ZW_OWNER_PASSWORD=你的口令`，然后启动服务——若 owner 口令仍为空，服务会用它引导设置（`cmd/zhiwei-server/main.go`）。**一旦设过就不再被覆盖**（改 `ZW_OWNER_PASSWORD` 重启无效）。之后用 **`owner` / 你设的口令** 登录。
- **本地 http 调试**：session cookie 默认带 `Secure`，纯 http 下浏览器不会回传 cookie → 登不进。本地调试设 `ZW_COOKIE_SECURE=false`。
- **会话有效期**：默认 30 天，`ZW_SESSION_TTL_DAYS` 可调。

### 用户管理 CLI

```bash
# 加载 .env（拿到 ZW_MYSQL_DSN 等），下面命令都需要
set -a; . ./.env; set +a

# 新增用户（建 app_user + 为其引导画像 owner「我」根节点）
go run ./cmd/zhiwei-adduser -u alice -p 口令 -n 爱丽丝    # -n 显示名可选，缺省取用户名

# 重置 / 清空口令（仅改 password_hash，不动其它数据）
go run ./cmd/zhiwei-resetpw -u owner -p 新口令           # 重置为新口令
go run ./cmd/zhiwei-resetpw -u owner                     # 省略 -p 即清空口令（禁登；可随后配 ZW_OWNER_PASSWORD 重启重新引导 owner）
```

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
| `go run ./cmd/zhiwei-adduser -u <名> -p <口令> [-n <显示名>]` | 新增登录用户 + 引导其画像 owner |
| `go run ./cmd/zhiwei-resetpw -u <名> [-p <新口令>]` | 重置口令；省略 `-p` 即清空（禁登） |
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
DELETE /api/speakers/{id}/name-candidates?name=…   忽略单个建议名字候选
GET  /api/sessions/{id}/speakers     会话内已解析说话人列表（面板用）
PATCH /api/sessions/{id}/segments/{seg}/speaker  单段换人（手动纠正说话人）
GET/POST         /api/persons                        人物名册（含 pending 计数）/ 新建
GET/PATCH/DELETE /api/persons/{id}                   详情（分组属性+关系+最近互动）/ 改名·换绑声纹 / 归档
POST             /api/persons/{id}/attributes        手动加属性（source=manual, conf=1.0）
PATCH/DELETE     /api/persons/{id}/attributes/{aid}  手动改值（supersede）/ 删除
POST             /api/persons/{id}/relationships     手动加关系（配偶/子女/上下游/组织…）
DELETE           /api/persons/{id}/relationships/{rid}
GET              /api/persons/{id}/history           修改历史（?entity_kind=&attr_key= 过滤）
GET/POST         /api/persons/{id}/events             大事记列表（?status= 过滤）/ 手动新增
DELETE           /api/persons/{id}/events/{eid}       删除事件（软删 dismissed）
GET/POST         /api/persons/{id}/metrics            时序指标（?metric_key=&from=&to= 区间，升序）/ 手动新增
DELETE           /api/persons/{id}/metrics/{mid}      删除测点
GET/POST         /api/persons/{id}/cycles             周期列表（含免责 note）/ 手动新增（生理期/用药等，敏感）
DELETE           /api/persons/{id}/cycles/{cid}       删除周期
GET              /api/profile/pending                确认队列（属性/关系/人物/大事记/指标/周期 pending 并集）
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
  → [worker 池] speaker 阶段：声纹 1:N 解析说话人 → 回填 segment.speaker_id
  → [worker 池] speakername 阶段：LLM 推断说话人名字（尽力而为，失败不阻塞）
  → [worker 池] extract 阶段：LLM 抽取记忆/待办/主题
  → [worker 池] profile 阶段：对话块 → LLM 抽取画像事实 → 置信闸门落库
    （高置信直接生效，低置信/冲突进确认队列 /api/profile/pending）
  → session 置 completed，时间线可查看
```

每个阶段失败自动重试 3 次，超限进 `failed` 可在页面上重跑；服务重启会恢复中断的任务。

## 已知限制与下一步

- ASR 用 StepFun `stepaudio-2.5-realtime`（火山 `doubao-seed-asr-2-0` 凭证待开通，开通后新增 Provider 替换即可，业务代码不动）
- ASR 无逐段时间戳，说话人切分依赖模型输出的 `[说话人N]` 前缀
- Sprint 2（Memory 抽取 / Todo / Topic）、Sprint 3（检索 / Agent 问答）、Sprint 4（Daily Review）见实现计划系列
