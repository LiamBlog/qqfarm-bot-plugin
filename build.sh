#!/usr/bin/env bash
# 构建 Go 版插件安装包：静态二进制 + 清单，无运行时依赖。
# 用法：bash build.sh [版本号] [amd64|arm64|all]（默认 3.5.0 all）
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${1:-3.6.0}"
TARGET="${2:-all}"
case "$TARGET" in
  amd64|arm64) ARCHES=("$TARGET") ;;
  all) ARCHES=(amd64 arm64) ;;
  *) echo "未知架构：$TARGET（可选 amd64 / arm64 / all）"; exit 1 ;;
esac

OUT="dist"
mkdir -p "$OUT/archive" bin

# 历史安装包归档保留
for f in "$OUT"/*.tar.gz "$OUT"/*.sha256 "$OUT"/*.zip; do
  [ -e "$f" ] || continue
  mv -f "$f" "$OUT/archive/" 2>/dev/null || true
done

for ARCH in "${ARCHES[@]}"; do
  NAME="qqfarm-bot-ipc-plugin-linux-${ARCH}-${VERSION}"
  PKG="$OUT/package-${ARCH}"

  echo "[1/4] 编译 ${ARCH}"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" -o "bin/qqfarm-bot-${ARCH}" .
  ls -lh "bin/qqfarm-bot-${ARCH}"

  echo "[2/4] 组装包目录（${ARCH}）"
  rm -rf "$PKG"; mkdir -p "$PKG/bin"
  cp "bin/qqfarm-bot-${ARCH}" "$PKG/bin/qqfarm-bot"
  cat > "$PKG/mengka-plugin.json" <<JSON
{
  "schema": 3,
  "framework_api": "3.0.0",
  "plugin_id": "qqfarm-bot",
  "name": "农场桥接助手",
  "version": "${VERSION}",
  "min_framework": "3.0.0",
  "author": "mengka-bot",
  "notes": "将登录账号的 QQ 农场 Code 自动推送到 qq-farm-bot 面板（Go 单二进制版，linux-${ARCH}）",
  "runtime": {
    "entry": "bin/qqfarm-bot",
    "args": [],
    "transport": "native-ipc-v1",
    "api_version": "1",
    "admin": true,
    "actions": ["get_bot_list", "get_qq_classic_farm_code"],
    "events": ["bot_online", "bot_offline"]
  }
}
JSON

  echo "[3/4] 冒烟：版本自检（${ARCH}）"
  if [ "$ARCH" = "$(go env GOARCH)" ]; then
    "$PKG/bin/qqfarm-bot" --version
  else
    echo "  跳过（宿主架构 $(go env GOARCH) 无法直接运行 ${ARCH}）"
  fi

  echo "[4/4] 打包 ${NAME}"
  tar -czf "$OUT/${NAME}.tar.gz" -C "$PKG" mengka-plugin.json bin
  SHA=$(sha256sum "$OUT/${NAME}.tar.gz" | awk '{print $1}')
  echo "$SHA  ${NAME}.tar.gz" > "$OUT/${NAME}.tar.gz.sha256"
  SIZE=$(du -h "$OUT/${NAME}.tar.gz" | cut -f1)
  echo "  ${NAME}.tar.gz（${SIZE}）SHA256：${SHA}"
done

echo "全部完成，产物见 $OUT/"
