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
  echo "跟随日志 ${LOG_FILE}（Ctrl-C 退出）"
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
