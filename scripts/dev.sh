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

  # 4) 后台启动：必须在当前 shell 用 & 结束才能拿到 $!（子 shell 里拿不到）
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
