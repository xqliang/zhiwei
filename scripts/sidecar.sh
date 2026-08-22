#!/usr/bin/env bash
# 声纹 sidecar 后台启停：FastAPI(uvicorn) 承载 WeSpeaker 提向量 + FAISS 1:N。
# 复用 services/voiceprint/.venv（见 Makefile sidecar-start 注释建 venv）。
# PID 文件 data/voiceprint.pid，日志 data/voiceprint.log。
set -e
PIDF=data/voiceprint.pid
LOG=data/voiceprint.log
VENV=services/voiceprint/.venv/bin/python
# venv 不存在时给出明确提示，避免静默用错 python
if [ ! -f "$VENV" ]; then
  echo "错误: 未找到 $VENV，先建 venv："
  echo "  python3.12 -m venv services/voiceprint/.venv"
  echo "  services/voiceprint/.venv/bin/pip install -r services/voiceprint/requirements.txt"
  exit 1
fi
# stubs 前置到 PYTHONPATH：遮蔽 wespeaker 的 SSL/whisper/diarization 可选前端
# (resnet34 走 fbank 前端用不到，但被 import 期 eager 引用)。详见 _stubs/ 注释。
export PYTHONPATH="services/voiceprint/_stubs${PYTHONPATH:+:$PYTHONPATH}"
CMD="$VENV -m uvicorn services.voiceprint.app:app --host 127.0.0.1 --port 8010"
case "${1:-status}" in
  start)
    mkdir -p data
    if [ -f "$PIDF" ] && kill -0 "$(cat "$PIDF")" 2>/dev/null; then
      echo "sidecar 已在运行 (pid $(cat "$PIDF"))"; exit 0
    fi
    nohup $CMD >"$LOG" 2>&1 &
    echo $! >"$PIDF"
    echo "sidecar started (pid $!), 日志 $LOG"
    ;;
  stop)
    if [ -f "$PIDF" ] && kill -0 "$(cat "$PIDF")" 2>/dev/null; then
      kill "$(cat "$PIDF")"
      echo "sidecar stopped (pid $(cat "$PIDF"))"
    else
      echo "sidecar 未运行"
    fi
    rm -f "$PIDF"
    ;;
  restart)
    "$0" stop || true
    "$0" start
    ;;
  status)
    if [ -f "$PIDF" ] && kill -0 "$(cat "$PIDF")" 2>/dev/null; then
      echo "running (pid $(cat "$PIDF"))"
    else
      echo "stopped"
    fi
    ;;
  *)
    echo "用法: $0 {start|stop|restart|status}"; exit 1
    ;;
esac
