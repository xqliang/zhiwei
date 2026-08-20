#!/usr/bin/env bash
# 给 web/app.js 加内容 hash 文件名做缓存破除（无构建方案）。
# 生成 web/app.<hash>.js 并改写 web/index.html 的 <script src>，清理旧 hash 副本。
# 由 make build / dev-restart 自动调用；编辑 app.js 后重跑（或 make dev-restart）即可让浏览器拉到新版本。
# 说明：web/app.*.js 不匹配 web/app.js（后者只有一个点），故源文件不会被删/被 gitignore。
set -euo pipefail

SRC=web/app.js
[ -f "$SRC" ] || { echo "hash-web: 缺 $SRC" >&2; exit 1; }

# 内容 hash（前 8 位 hex）；兼容 macOS shasum 与 Linux sha256sum
if command -v shasum >/dev/null 2>&1; then
  HASH=$(shasum -a 256 "$SRC" | cut -c1-8)
else
  HASH=$(sha256sum "$SRC" | cut -c1-8)
fi
DST="web/app.${HASH}.js"

# 清理旧 hash 副本（! -name 'app.js' 保险性地排除源文件）
find web -maxdepth 1 -name 'app.*.js' ! -name 'app.js' -delete

cp "$SRC" "$DST"

# 改写 index.html 的 src：/app/app.js 或 /app/app.<旧hash>.js → /app/app.<新hash>.js
sed -E -i.bak "s|/app/app(\.[a-f0-9]+)?\.js|/app/app.${HASH}.js|g" web/index.html
rm -f web/index.html.bak

echo "hash-web: $DST (hash=$HASH)"
