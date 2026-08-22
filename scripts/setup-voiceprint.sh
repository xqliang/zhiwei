#!/usr/bin/env bash
# 建声纹 sidecar venv + 装依赖（Python 3.12，CPU）。一键复现 spike 验证过的环境。
# 用法：bash scripts/setup-voiceprint.sh
set -euo pipefail
VENV=services/voiceprint/.venv

# 需 python3.12（torch 2.x + 科学栈 wheel 覆盖最好）。缺则提示。
if ! command -v python3.12 >/dev/null 2>&1; then
  echo "错误: 未找到 python3.12（torch/scipy/umap 在 3.12 上 wheel 最稳）。请先装："
  echo "  brew install python@3.12  # 或用 miniforge3 的 python3.12"
  exit 1
fi

echo "=== 建 venv: $VENV ==="
python3.12 -m venv "$VENV"
"$VENV/bin/pip" install -q --upgrade pip

echo "=== 装依赖（requirements.txt）==="
"$VENV/bin/pip" install -r services/voiceprint/requirements.txt

echo "=== 装 wespeaker 本体（--no-deps，避免拖 umap/s3prl 等重/不兼容依赖）==="
"$VENV/bin/pip" install --no-deps git+https://github.com/wenet-e2e/wespeaker.git

echo ""
echo "venv 就绪: $VENV"
echo "首次 load_model('chinese') 会从 ModelScope 下载 CnCeleb resnet34_LM (~31MB) 到 ~/.wespeaker/chinese。"
echo "验证: make spike-voiceprint"
echo "启动 sidecar: make sidecar-start（PYTHONPATH 已前置 _stubs/ 遮蔽可选前端）"
