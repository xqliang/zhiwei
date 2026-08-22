#!/usr/bin/env bash
# e2e 冒烟：起依赖 → 起服务 → 上传音频 → 轮询到终态 → 校验转写非空
# 用法: bash scripts/e2e.sh [音频文件]   （默认 testdata/speech.wav，真实语音）
set -euo pipefail

cd "$(dirname "$0")/.."
AUDIO="${1:-testdata/speech.wav}"

echo "==> 启动 MySQL 并迁移"
make compose-up
sleep 3
make migrate-up || true   # 已迁移则跳过

echo "==> 启动声纹 sidecar（说话人解析需要；首次需建 venv 见 Makefile 注释）"
make sidecar-start || echo "  (sidecar 起不来，speaker stage 会失败重试，转写仍可用)"

echo "==> 启动 zhiwei-server"
make build
(set -a; source .env; set +a; ./bin/zhiwei-server & echo $! > /tmp/zhiwei-e2e.pid)
trap 'kill "$(cat /tmp/zhiwei-e2e.pid)" 2>/dev/null || true; make sidecar-stop >/dev/null 2>&1 || true' EXIT
sleep 2

echo "==> 健康检查"
curl -fsS localhost:8080/api/health
echo

echo "==> 上传音频: $AUDIO"
RESP=$(curl -fsS -F "file=@$AUDIO" -F "source=web_upload" localhost:8080/api/audio)
echo "$RESP"
SESSION_ID=$(echo "$RESP" | sed -E 's/.*"session_id":"([0-9]+)".*/\1/')

echo "==> 轮询处理结果（最多 180s）"
for i in $(seq 1 60); do
  DETAIL=$(curl -fsS "localhost:8080/api/sessions/$SESSION_ID")
  STATUS=$(echo "$DETAIL" | python3 -c "import json,sys; d=json.load(sys.stdin); j=d.get('job'); print(j['status'] if j else d['session']['status'])")
  echo "  [$i] status=$STATUS"
  if [ "$STATUS" = "done" ] || [ "$STATUS" = "completed" ]; then
    if ! echo "$DETAIL" | python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get('segments') else 1)"; then
      echo "FAIL: segments 为空"; exit 1
    fi
    # Sprint 2：断言 memory 抽取产出（真实语音才有内容）
    MEMS=$(curl -fsS "localhost:8080/api/memories?limit=5")
    echo "memories: $(echo "$MEMS" | head -c 500)"
    if ! echo "$MEMS" | python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get('memories') else 1)"; then
      echo "FAIL: memories 为空（真实语音不应为空）"; exit 1
    fi
    # 说话人：speaker stage 应解析到说话人（speakers 列表非空）
    SPK=$(curl -fsS "localhost:8080/api/sessions/$SESSION_ID/speakers")
    echo "speakers: $(echo "$SPK" | head -c 500)"
    if ! echo "$SPK" | python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get('speakers') else 1)"; then
      echo "FAIL: speakers 为空（speaker stage 未产出，检查 sidecar 是否运行）"; exit 1
    fi
    echo "PASS: pipeline 跑通，转写/记忆抽取/说话人解析产出正常"
    echo "$DETAIL" | python3 -m json.tool | head -30
    exit 0
  fi
  if [ "$STATUS" = "failed" ]; then
    echo "FAIL: 处理失败"; echo "$DETAIL"; exit 1
  fi
  sleep 3
done
echo "FAIL: 180s 未完成"; exit 1
