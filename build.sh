#!/usr/bin/env bash
# 构建 Go 版插件安装包：静态二进制 + 清单。
# 用法：bash build.sh [版本号] [目标]
#   目标：all（linux amd64+arm64）｜amd64｜arm64｜windows｜windows-arm64｜linux
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${1:-3.6.2}"
TARGET="${2:-all}"
case "$TARGET" in
  all|linux)       TARGETS=("linux/amd64" "linux/arm64") ;;
  amd64)           TARGETS=("linux/amd64") ;;
  arm64)           TARGETS=("linux/arm64") ;;
  windows)         TARGETS=("windows/amd64") ;;
  windows-arm64)   TARGETS=("windows/arm64") ;;
  *) echo "未知目标：$TARGET（可选 all / linux / amd64 / arm64 / windows / windows-arm64）"; exit 1 ;;
esac

OUT="dist"
mkdir -p "$OUT/archive" bin

# 历史安装包归档保留（当前版本的多平台产物保留在 dist 根目录）
for f in "$OUT"/*.tar.gz "$OUT"/*.sha256 "$OUT"/*.zip; do
  [ -e "$f" ] || continue
  case "$f" in
    *-"${VERSION}.tar.gz"|*"-${VERSION}.tar.gz.sha256") continue ;;
  esac
  mv -f "$f" "$OUT/archive/" 2>/dev/null || true
done

for SPEC in "${TARGETS[@]}"; do
  GOOSV="${SPEC%/*}"
  GOARCHV="${SPEC#*/}"
  NAME="qqfarm-bot-ipc-plugin-${GOOSV}-${GOARCHV}-${VERSION}"
  PKG="$OUT/package-${GOOSV}-${GOARCHV}"
  BIN="qqfarm-bot"
  [ "$GOOSV" = "windows" ] && BIN="qqfarm-bot.exe"

  echo "[1/4] 编译 ${GOOSV}/${GOARCHV}"
  CGO_ENABLED=0 GOOS="$GOOSV" GOARCH="$GOARCHV" go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" -o "bin/${BIN%.exe}-${GOOSV}-${GOARCHV}${BIN#qqfarm-bot}" .
  ls -lh "bin/${BIN%.exe}-${GOOSV}-${GOARCHV}${BIN#qqfarm-bot}"

  echo "[2/4] 组装包目录（${GOOSV}/${GOARCHV}）"
  rm -rf "$PKG"; mkdir -p "$PKG/bin"
  cp "bin/${BIN%.exe}-${GOOSV}-${GOARCHV}${BIN#qqfarm-bot}" "$PKG/bin/$BIN"
  cat > "$PKG/mengka-plugin.json" <<JSON
{
  "schema": 3,
  "framework_api": "3.0.0",
  "plugin_id": "qqfarm-bot",
  "name": "农场桥接助手",
  "version": "${VERSION}",
  "min_framework": "3.0.0",
  "author": "mengka-bot",
  "notes": "将登录账号的 QQ 农场 Code 自动推送到 qq-farm-bot 面板（Go 单二进制版，${GOOSV}-${GOARCHV}）",
  "runtime": {
    "entry": "bin/${BIN}",
    "args": [],
    "transport": "native-ipc-v1",
    "api_version": "1",
    "admin": true,
    "actions": ["get_bot_list", "get_qq_classic_farm_code"],
    "events": ["bot_online", "bot_offline"]
  }
}
JSON

  echo "[3/4] 冒烟：版本自检（${GOOSV}/${GOARCHV}）"
  if [ "$GOOSV" = "linux" ] && [ "$GOARCHV" = "$(go env GOARCH)" ]; then
    "$PKG/bin/$BIN" --version
  else
    echo "  跳过（宿主 $(go env GOOS)/$(go env GOARCH)，无法直接运行 ${GOOSV}/${GOARCHV}）"
  fi

  echo "[4/4] 打包 ${NAME}"
  tar -czf "$OUT/${NAME}.tar.gz" -C "$PKG" mengka-plugin.json bin
  SHA=$(sha256sum "$OUT/${NAME}.tar.gz" | awk '{print $1}')
  echo "$SHA  ${NAME}.tar.gz" > "$OUT/${NAME}.tar.gz.sha256"
  SIZE=$(du -h "$OUT/${NAME}.tar.gz" | cut -f1)
  echo "  ${NAME}.tar.gz（${SIZE}）SHA256：${SHA}"
done

echo "全部完成，产物见 $OUT/"
