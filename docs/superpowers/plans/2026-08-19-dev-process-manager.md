# 调试进程管理脚本 dev.sh 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 提供后台管理 zhiwei-server 调试进程的脚本 `scripts/dev.sh`（start / stop / restart / status / logs），并集成到 Makefile。

**Architecture:** 单个 Bash 脚本 + PID 文件（`.run/dev.pid`）+ 日志文件（`logs/dev.log`）。进程状态判断统一收敛到 `running_pid` 函数（含 PID 复用防误杀）；服务本身已支持 SIGTERM 优雅退出（`cmd/zhiwei-server/main.go` 的 `signal.NotifyContext`），脚本只需发信号后轮询。

**Tech Stack:** Bash（macOS bash 3.2 兼容，不用关联数组等新特性）、curl、ps、Make。

**关键事实（实现前必读）:**

- 端口由 `ZW_PORT` 环境变量控制，默认 8080（`internal/config/config.go`）
- 服务启动必需 `ARK_API_KEY`（config.Load 报错）和 `STEPFUN_API_KEY`（main.go log.Fatal）——预检两个都查
- 服务**不会自己读 .env**，脚本必须在启动前 `set -a; . .env; set +a`
- 健康检查端点：`GET /api/health`
- 手动验证需要 MySQL 在跑：先 `make compose-up && make migrate-up`

---

### Task 1: .gitignore 增加 .run/ 与 logs/

**Files:**
- Modify: `.gitignore`

- [x] **Step 1: 追加两行**

在 `.gitignore` 末尾追加（现有内容为 `data/`、`bin/`、`*.log`、`.env`，保持不动）：

```gitignore
.run/
logs/
```

- [x] **Step 2: 验证 git 不再追踪这两个目录的文件**

Run: `mkdir -p .run logs && touch .run/dev.pid logs/dev.log && git status --short | grep -E '\.run|logs' ; echo "exit=$?"`
Expected: 无匹配输出且 `exit=1`（git status 里看不到这两个文件）

- [x] **Step 3: Commit**

```bash
git add .gitignore
git commit -m "chore: gitignore 增加 .run/ 与 logs/（dev.sh 运行时目录）"
```

---

### Task 2: dev.sh 脚本骨架 + logs 子命令

**Files:**
- Create: `scripts/dev.sh`

- [x] **Step 1: 创建脚本**

```bash
touch scripts/dev.sh && chmod +x scripts/dev.sh
```

- [x] **Step 2: 写入骨架代码**

写入 `scripts/dev.sh` 完整内容（本任务只实现 usage / 公共函数 / logs；start、stop、restart、status 在后续任务逐个追加，dispatch 也随之逐行追加）：

```bash
#!/usr/bin/env bash
# dev.sh —— zhiwei-server 调试进程管理脚本。
# 用法：scripts/dev.sh {start|stop|restart|status|logs}
#
# 设计要点：
# - PID 文件 .run/dev.pid 记录后台进程号；判断存活时校验进程名，防止 PID 复用误杀
# - 日志统一追加到 logs/dev.log（stdout+stderr）
# - 服务本身响应 SIGTERM 优雅退出，stop 先 SIGTERM 等 5s，超时再 SIGKILL
set -euo pipefail

# 项目根目录（脚本在 scripts/ 下）
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin/zhiwei-server"
PID_FILE="$ROOT/.run/dev.pid"
LOG_FILE="$ROOT/logs/dev.log"
# 与 internal/config 保持一致：ZW_PORT 默认 8080
PORT="${ZW_PORT:-8080}"

usage() {
  cat <<'EOF'
用法: scripts/dev.sh <command>

命令:
  start     预检环境变量 → 编译 → 后台启动 → 等待健康检查通过
  stop      优雅停止（SIGTERM，5s 超时后 SIGKILL）
  restart   stop（若在运行）+ start
  status    查看运行状态 / PID / 端口 / 最近日志
  logs      tail -f 跟随日志
EOF
}

# 输出存活进程的 PID；进程不存在或 PID 文件陈旧时返回非 0（不输出）。
# 陈旧场景：PID 文件存在但进程已死，或 PID 被系统复用给了其他程序。
running_pid() {
  [ -f "$PID_FILE" ] || return 1
  local pid
  pid="$(cat "$PID_FILE" 2>/dev/null)" || return 1
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  # 防 PID 复用误杀：确认该 PID 对应的进程确实是 zhiwei-server
  ps -p "$pid" -o comm= 2>/dev/null | grep -q "zhiwei-server" || return 1
  echo "$pid"
}

cmd_logs() {
  # 日志文件可能还不存在（从未启动过），先 touch 保证 tail 不报错
  mkdir -p "$(dirname "$LOG_FILE")"
  touch "$LOG_FILE"
  echo "跟随日志 $LOG_FILE（Ctrl-C 退出）"
  exec tail -n 100 -f "$LOG_FILE"
}

case "${1:-}" in
  logs)
    cmd_logs
    ;;
  *)
    usage
    exit 1
    ;;
esac
```

- [x] **Step 3: 语法检查**

Run: `bash -n scripts/dev.sh && echo OK`
Expected: `OK`

- [x] **Step 4: 验证 usage 与 logs**

Run: `./scripts/dev.sh` 然后 `./scripts/dev.sh badcmd`（分别观察）
Expected: 两次都打印 usage 且退出码为 1

Run: `timeout 2 ./scripts/dev.sh logs`
Expected: 打印"跟随日志 ..."（timeout 2 结束，退出码 124 是预期）

- [x] **Step 5: Commit**

```bash
git add scripts/dev.sh
git commit -m "feat: dev.sh 脚本骨架（usage/running_pid/logs）"
```

---

### Task 3: start 子命令

**Files:**
- Modify: `scripts/dev.sh`

- [x] **Step 1: 在 `cmd_logs` 函数定义之前插入 `cmd_start`**

注意：后台启动必须在**当前 shell** 用 `&` 结束才能拿到 `$!`（子 shell 里后台启动拿不到），所以最后 `cd "$ROOT"` 切目录后直接 `nohup ... &`：

```bash
cmd_start() {
  # 1) 防重复启动
  local pid
  if pid="$(running_pid)"; then
    echo "zhiwei-server 已在运行 (PID $pid)。如需重启请用: scripts/dev.sh restart"
    exit 0
  fi
  rm -f "$PID_FILE" # 清理陈旧 PID 文件

  # 2) 环境变量预检：服务不自己读 .env，这里统一 source 后校验必需密钥
  if [ -f "$ROOT/.env" ]; then
    set -a
    # shellcheck disable=SC1091
    . "$ROOT/.env"
    set +a
  fi
  if [ -z "${ARK_API_KEY:-}" ]; then
    echo "错误: ARK_API_KEY 未设置（LLM 必需）。请在项目根目录 .env 中配置后重试。" >&2
    exit 1
  fi
  if [ -z "${STEPFUN_API_KEY:-}" ]; then
    echo "错误: STEPFUN_API_KEY 未设置（ASR 必需）。请在项目根目录 .env 中配置后重试。" >&2
    exit 1
  fi

  # 3) 编译
  mkdir -p "$ROOT/.run" "$ROOT/logs" "$(dirname "$BIN")"
  echo "编译 zhiwei-server ..."
  (cd "$ROOT" && go build -o "$BIN" ./cmd/zhiwei-server)

  # 4) 后台启动：必须在当前 shell 用 & 才能拿到 $!；日志追加写入
  echo "---- $(date '+%Y-%m-%d %H:%M:%S') dev.sh start ----" >> "$LOG_FILE"
  cd "$ROOT"
  nohup "$BIN" >> "$LOG_FILE" 2>&1 &
  pid=$!
  echo "$pid" > "$PID_FILE"

  # 5) 健康确认：最多等 5s（10 次 × 0.5s），失败则自动回滚
  printf '等待健康检查 http://localhost:%s/api/health ...' "$PORT"
  local i
  for i in $(seq 1 10); do
    if curl -sf "http://localhost:$PORT/api/health" >/dev/null 2>&1; then
      echo
      echo "启动成功 (PID $pid)"
      echo "日志: $LOG_FILE （scripts/dev.sh logs 跟随）"
      return 0
    fi
    sleep 0.5
    printf '.'
  done
  echo
  echo "健康检查超时，最近 20 行日志：" >&2
  tail -n 20 "$LOG_FILE" >&2
  echo "启动失败，回滚：停止进程并清理 PID 文件" >&2
  kill "$pid" 2>/dev/null || true
  rm -f "$PID_FILE"
  exit 1
}
```

- [x] **Step 2: dispatch 增加 start 分支**

`case "${1:-}" in` 中 `logs)` 之前加入：

```bash
  start)
    cmd_start
    ;;
```

- [x] **Step 3: 语法检查**

Run: `bash -n scripts/dev.sh && echo OK`
Expected: `OK`

- [x] **Step 4: 真实启动验证（前置：make compose-up && make migrate-up）**

Run: `./scripts/dev.sh start && curl -sf http://localhost:8080/api/health && echo`
Expected: `编译 zhiwei-server ...` → `等待健康检查 ...` → `启动成功 (PID xxx)`；health 返回正常 JSON

Run: `kill -0 "$(cat .run/dev.pid)" && echo alive`
Expected: `alive`（后台进程确实存活）

- [x] **Step 5: 防重复启动验证**

Run: `./scripts/dev.sh start`
Expected: `zhiwei-server 已在运行 (PID xxx)。如需重启请用: scripts/dev.sh restart`，退出码 0，原进程不受影响

- [x] **Step 6: 环境变量预检验证**

Run: `mv .env .env.bak && ./scripts/dev.sh start; mv .env.bak .env`
Expected: 报 `错误: ARK_API_KEY 未设置...`，退出码 1，没有新进程被拉起（`.run/dev.pid` 内容不变）

- [x] **Step 7: 清理现场（留给 Task 4 测试）**

先手动 `kill "$(cat .run/dev.pid)" && rm -f .run/dev.pid`，确认 `pgrep -f zhiwei-server` 无输出。

- [x] **Step 8: Commit**

```bash
git add scripts/dev.sh
git commit -m "feat: dev.sh start 子命令（预检/编译/后台启动/健康确认/失败回滚）"
```

---

### Task 4: stop 子命令

**Files:**
- Modify: `scripts/dev.sh`

- [x] **Step 1: 在 `cmd_start` 之后插入 `cmd_stop`**

```bash
cmd_stop() {
  local pid
  if ! pid="$(running_pid)"; then
    # 未在运行（或 PID 文件陈旧）：清理后幂等退出，不算错误
    rm -f "$PID_FILE"
    echo "zhiwei-server 未在运行"
    return 0
  fi
  echo "停止 zhiwei-server (PID $pid) ..."
  kill "$pid" 2>/dev/null || true
  # SIGTERM 后每 0.5s 轮询一次，5s 内退出则优雅完成
  local i
  for i in $(seq 1 10); do
    if ! kill -0 "$pid" 2>/dev/null; then
      rm -f "$PID_FILE"
      echo "已停止"
      return 0
    fi
    sleep 0.5
  done
  echo "优雅终止超时，SIGKILL 强制停止 ..."
  kill -9 "$pid" 2>/dev/null || true
  rm -f "$PID_FILE"
  echo "已停止 (SIGKILL)"
}
```

- [x] **Step 2: dispatch 增加 stop 分支**

`case "${1:-}" in` 中加入（与 start 分支并列）：

```bash
  stop)
    cmd_stop
    ;;
```

- [x] **Step 3: 语法检查**

Run: `bash -n scripts/dev.sh && echo OK`
Expected: `OK`

- [x] **Step 4: 真实停止验证（前置：先 `./scripts/dev.sh start`）**

Run: `./scripts/dev.sh stop && pgrep -f zhiwei-server; echo "pgrep exit=$?"`
Expected: `停止 zhiwei-server (PID xxx) ...` → `已停止`；pgrep 无输出且 `pgrep exit=1`（进程确实没了）；`.run/dev.pid` 不存在

- [x] **Step 5: 幂等验证**

Run: `./scripts/dev.sh stop`
Expected: `zhiwei-server 未在运行`，退出码 0

- [x] **Step 6: 陈旧 PID 文件验证**

Run: `mkdir -p .run && echo 999999 > .run/dev.pid && ./scripts/dev.sh stop`
Expected: `zhiwei-server 未在运行`（PID 999999 不存在/名字不匹配，被当作陈旧文件清理），退出码 0

- [x] **Step 7: Commit**

```bash
git add scripts/dev.sh
git commit -m "feat: dev.sh stop 子命令（SIGTERM 优雅终止 + 超时 SIGKILL + 幂等）"
```

---

### Task 5: restart 与 status 子命令

**Files:**
- Modify: `scripts/dev.sh`

- [x] **Step 1: 在 `cmd_stop` 之后插入两个函数**

```bash
cmd_restart() {
  # 未运行时 stop 幂等跳过，效果等同直接 start
  cmd_stop
  cmd_start
}

cmd_status() {
  local pid
  if pid="$(running_pid)"; then
    echo "zhiwei-server 运行中"
    echo "  PID:   $pid"
    # etime= 进程已运行时长；comm= 进程名
    ps -p "$pid" -o pid=,etime=,comm= | sed 's/^/  /'
    echo "  端口:  $PORT （http://localhost:$PORT）"
    echo "  日志:  $LOG_FILE"
    echo "  最近 5 行日志:"
    tail -n 5 "$LOG_FILE" 2>/dev/null | sed 's/^/    /' || echo "    (无日志)"
  else
    rm -f "$PID_FILE"
    echo "zhiwei-server 未在运行"
  fi
}
```

- [x] **Step 2: dispatch 增加 restart / status 分支**

`case "${1:-}" in` 中加入（与 stop 分支并列）：

```bash
  restart)
    cmd_restart
    ;;
  status)
    cmd_status
    ;;
```

- [x] **Step 3: 语法检查**

Run: `bash -n scripts/dev.sh && echo OK`
Expected: `OK`

- [x] **Step 4: status 验证（未运行 → 运行中）**

Run: `./scripts/dev.sh status`
Expected: `zhiwei-server 未在运行`

Run: `./scripts/dev.sh start && ./scripts/dev.sh status`
Expected: `运行中`，输出 PID、运行时长（etime）、端口、最近 5 行日志

- [x] **Step 5: restart 验证**

Run: `old_pid="$(cat .run/dev.pid)" && ./scripts/dev.sh restart && new_pid="$(cat .run/dev.pid)" && echo "old=$old_pid new=$new_pid" && curl -sf http://localhost:8080/api/health`
Expected: stop + start 流程输出；`old=xxx new=yyy` 且两个 PID 不同；健康检查正常

Run: `./scripts/dev.sh stop`（收尾，不留后台进程）

- [x] **Step 6: Commit**

```bash
git add scripts/dev.sh
git commit -m "feat: dev.sh restart/status 子命令，五命令齐备"
```

---

### Task 6: Makefile 别名 + README 更新

**Files:**
- Modify: `Makefile`（`.PHONY` 行与新增 5 个 target）
- Modify: `README.md`（常用命令表）

- [x] **Step 1: Makefile 增加 5 个 target**

在 `dev: build` 目标之后追加：

```makefile
# 调试进程后台管理（scripts/dev.sh 封装）
dev-start:
	bash scripts/dev.sh start
dev-stop:
	bash scripts/dev.sh stop
dev-restart:
	bash scripts/dev.sh restart
dev-status:
	bash scripts/dev.sh status
dev-logs:
	bash scripts/dev.sh logs
```

`.PHONY` 行同步追加（在行尾补）：

```makefile
.PHONY: build dev dev-start dev-stop dev-restart dev-status dev-logs test test-integration migrate-up migrate-down compose-up compose-down init-testdb spike-llm spike-embed spike-asr e2e
```

- [x] **Step 2: README 常用命令表追加一行**

在 `| make init-testdb | 重建集成测试库 |` 之后追加：

```markdown
| `make dev-start / dev-stop / dev-restart` | 后台启停调试进程（另有 `dev-status` / `dev-logs`） |
```

- [x] **Step 3: 验证 make 别名**

Run: `make dev-start && make dev-status && make dev-stop`
Expected: start → status 显示运行中 → stop 显示已停止；`make dev-status` 再次执行显示未运行

- [x] **Step 4: 全量回归**

Run: `make test`
Expected: 全部 PASS（确认没有破坏任何 Go 代码路径——本任务本不该影响，跑一遍兜底）

- [x] **Step 5: Commit**

```bash
git add Makefile README.md
git commit -m "feat: make dev-start/stop/restart/status/logs 集成 dev.sh"
```

---

## 验收清单（对照 spec 测试计划）

- [x] `dev.sh status`（未运行）→ 显示未运行（Task 5 Step 4）
- [x] `dev.sh start` → 存活、健康检查通过、PID 文件正确（Task 3 Step 4）
- [x] `dev.sh start`（重复）→ 拒绝（Task 3 Step 5）
- [x] `dev.sh restart` → PID 变化、服务恢复（Task 5 Step 5）
- [x] `dev.sh stop` → 进程退出、PID 文件清理；再次 stop 幂等（Task 4 Step 4/5）
- [x] 缺 `ARK_API_KEY` 时 start → 明确报错（Task 3 Step 6）
