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

# 输出监听服务端口的进程行（lsof 一行）；无监听返回空。
# 用途：start 前端口预检——端口被非本脚本管理的进程占用时，新进程会 bind 失败
# 秒退，而健康检查会打到占用进程上「假成功」，必须提前拦下并报出占用者。
port_listener() {
  lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | tail -n +2 | head -n 1
}

cmd_start() {
  # 1) 防重复启动
  local pid
  if pid="$(running_pid)"; then
    echo "zhiwei-server 已在运行 (PID $pid)。如需重启请用: scripts/dev.sh restart"
    exit 0
  fi
  rm -f "$PID_FILE" # 清理陈旧 PID 文件

  # 1.5) 端口预检：走到这里说明本脚本没在管任何进程，端口若被监听就是「野」进程
  #（如手动起的 bin/zhiwei-server）。此时启动必然 bind 失败，且健康检查会打到
  # 野进程上假成功——提前报出占用者让用户处理（kill 或换 ZW_PORT）。
  local listener
  if listener="$(port_listener)"; then
    echo "错误: 端口 $PORT 已被其它进程占用（非本脚本管理，dev-stop 停不掉）:" >&2
    echo "  $listener" >&2
    echo "请先 kill 该进程（kill <PID>），或用 ZW_PORT 环境变量换端口。" >&2
    exit 1
  fi

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
  # ASR key：file provider（默认）用 STEPFUN_ASR_FILE_API_KEY；realtime provider 用 STEPFUN_API_KEY
  if [ -z "${STEPFUN_ASR_FILE_API_KEY:-}" ] && [ -z "${STEPFUN_API_KEY:-}" ]; then
    echo "错误: STEPFUN_ASR_FILE_API_KEY 或 STEPFUN_API_KEY 未设置（ASR 必需）。请在项目根目录 .env 中配置后重试。" >&2
    exit 1
  fi

  # 3) 编译
  mkdir -p "$ROOT/.run" "$ROOT/logs" "$(dirname "$BIN")"
  echo "编译 zhiwei-server ..."
  (cd "$ROOT" && go build -o "$BIN" ./cmd/zhiwei-server)

  # 4) 后台启动：必须在当前 shell 用 & 结束才能拿到 $!（子 shell 里拿不到）
  echo "---- $(date '+%Y-%m-%d %H:%M:%S') dev.sh start ----" >> "$LOG_FILE"
  cd "$ROOT"
  nohup "$BIN" >> "$LOG_FILE" 2>&1 &
  pid=$!
  echo "$pid" > "$PID_FILE"

  # 5) 健康确认：最多等 5s（10 次 × 0.5s），失败则自动回滚。
  #    每轮先校验我们起的进程还活着——bind 失败等启动即崩的场景（预检漏网时），
  #    若端口上有别的进程，curl 会打到旧进程上「假成功」，必须先排除。
  printf '等待健康检查 http://localhost:%s/api/health ...' "$PORT"
  local i
  for i in $(seq 1 10); do
    if ! kill -0 "$pid" 2>/dev/null; then
      echo
      echo "错误: 进程 (PID $pid) 启动后即退出（端口冲突/配置错误等），最近 20 行日志：" >&2
      tail -n 20 "$LOG_FILE" >&2
      rm -f "$PID_FILE"
      exit 1
    fi
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
    echo "  端口:  ${PORT} （http://localhost:${PORT}）"
    echo "  日志:  $LOG_FILE"
    echo "  最近 5 行日志:"
    tail -n 5 "$LOG_FILE" 2>/dev/null | sed 's/^/    /' || echo "    (无日志)"
  else
    rm -f "$PID_FILE"
    echo "zhiwei-server 未在运行"
  fi
}

cmd_logs() {
  # 日志文件可能还不存在（从未启动过），先 touch 保证 tail 不报错
  mkdir -p "$(dirname "$LOG_FILE")"
  touch "$LOG_FILE"
  echo "跟随日志 ${LOG_FILE}（Ctrl-C 退出）"
  exec tail -n 100 -f "$LOG_FILE"
}

case "${1:-}" in
  start)
    cmd_start
    ;;
  stop)
    cmd_stop
    ;;
  restart)
    cmd_restart
    ;;
  status)
    cmd_status
    ;;
  logs)
    cmd_logs
    ;;
  *)
    usage
    exit 1
    ;;
esac
