#!/bin/sh
# build.sh — 交叉编译 Go 后端并调用 fnpack 打包 fpk（支持 x86 / arm 双架构，一次出两个包）
#
# 用法：
#   sh build.sh            # 默认：同时打出 x86 与 arm 两个 fpk（等价于 all）
#   sh build.sh amd64      # 只打 x86 版  (manifest platform=x86)
#   sh build.sh arm64      # 只打 arm 版  (manifest platform=arm)
#   sh build.sh all        # 同默认
#
# 前置：
#   - Go 工具链（交叉编译纯标准库，无需 cgo）
#   - fnpack 在 PATH 或环境变量 FNPACK 指向它
#   - 编译 usbip 需要 Linux 构建机（fpk/cmd/build-usbip.sh 会装 musl 工具链静态编译内核 userspace）
#
# 多架构策略（与飞牛官方惯例一致：分架构打包，而非把多架构二进制塞进同一个 fpk）：
#   分别把对应架构的 UPS-Helper 放进 fpk/app/UPS-Helper、对应架构 usbip 放进 fpk/cmd/usbip，
#   再用 sed 切换 manifest 的 platform，fnpack 各打一次，产物以 -x86 / -arm64 后缀区分，落到 dist/。
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SVC="$HERE/service"
PKG="$HERE/fpk"
BUILD_TMP="$HERE/.build"
DIST="$HERE/dist"
mkdir -p "$BUILD_TMP" "$DIST"

# fnpack 解析：PATH 优先，其次仓库根下的 fnpack
FNPACK="${FNPACK:-}"
if [ -z "$FNPACK" ]; then
  if command -v fnpack >/dev/null 2>&1; then
    FNPACK="fnpack"
  elif [ -x "$HERE/fnpack" ]; then
    FNPACK="$HERE/fnpack"
  else
    echo "错误：未找到 fnpack（请放入 PATH 或设置 FNPACK 环境变量）" >&2
    exit 1
  fi
fi

VERSION="$(grep -E '^version=' "$PKG/manifest" | head -1 | cut -d= -f2)"
[ -n "$VERSION" ] || VERSION="1.0.0"

TARGET="${1:-amd64}"
case "$TARGET" in
  all|"") ARCHS="amd64 arm64" ;;
  amd64) ARCHS="amd64" ;;
  arm64) ARCHS="arm64" ;;
  *) echo "错误：未知目标 '$TARGET'（amd64 / arm64 / all）" >&2; exit 1 ;;
esac

# 1) 先把所有需要的 Go 后端交叉编译出来（纯标准库，macOS/Linux 均可）
for a in $ARCHS; do
  echo "==> 交叉编译 Go 后端 GOOS=linux GOARCH=$a -> $BUILD_TMP/UPS-Helper-$a"
  ( cd "$SVC" && CGO_ENABLED=0 GOOS=linux GOARCH="$a" go build -trimpath -ldflags="-s -w" -o "$BUILD_TMP/UPS-Helper-$a" . )
done

# 2) 每个架构：放二进制 + 编译 usbip + 切 platform + fnpack build + 重命名
for a in $ARCHS; do
  case "$a" in
    amd64) PLATFORM=x86 ;;
    arm64) PLATFORM=arm ;;
  esac
  echo ""
  echo "==> 打包 $a (manifest platform=$PLATFORM)"

  # usbip 是 C 程序，需 Linux 静态编译；非 Linux 时做能力降级
  if [ "$(uname -s)" != "Linux" ]; then
    if [ ! -f "$PKG/cmd/usbip" ]; then
      echo "警告：非 Linux 且无 fpk/cmd/usbip，跳过 $a 版（usbip 需在 Linux 用 build-usbip.sh 编译）"
      continue
    fi
    if [ "$a" = "arm64" ]; then
      echo "警告：非 Linux 时无法编译 aarch64 版 usbip，跳过 arm64 版（请到 Linux 构建机执行 build.sh arm64）"
      continue
    fi
    echo "    注意：当前非 Linux，沿用已有 x86_64 版 fpk/cmd/usbip 打 amd64 包"
  else
    ( cd "$PKG/cmd" && sh build-usbip.sh "$a" )
  fi

  cp "$BUILD_TMP/UPS-Helper-$a" "$PKG/app/UPS-Helper"
  chmod +x "$PKG/app/UPS-Helper"
  # cmd/main 保持为仓库内的 shell 生命周期脚本（不复制 Go 二进制进来）：
  # 它仅把 start/stop/status 转发给 app/UPS-Helper，避免包内重复 7.29MB 二进制使
  # fpk 体积翻倍（6.89MB）。CGI 由 app/ui/index.cgi 经 -cgi 子命令处理，互不影响。

  sed -i.bak "s/^platform=.*/platform=$PLATFORM/" "$PKG/manifest" && rm -f "$PKG/manifest.bak"

  rm -f "$PKG"/*.fpk
  ( cd "$PKG" && "$FNPACK" build )
  for f in "$PKG"/*.fpk; do
    [ -e "$f" ] || continue
    case "$a" in
      amd64) SUFFIX="-x86" ;;
      arm64) SUFFIX="-arm64" ;;
    esac
    mv -f "$f" "$DIST/UPS-Helper-$VERSION$SUFFIX.fpk"
  done
  echo "    -> $DIST/UPS-Helper-$VERSION$SUFFIX.fpk"
done

# 3) 还原为 x86 默认状态，并清理临时产物
cp "$BUILD_TMP/UPS-Helper-amd64" "$PKG/app/UPS-Helper" 2>/dev/null || true
sed -i.bak "s/^platform=.*/platform=x86/" "$PKG/manifest" && rm -f "$PKG/manifest.bak"
rm -rf "$BUILD_TMP"
echo ""
echo "完成："
ls -la "$DIST"/UPS-Helper-*.fpk 2>/dev/null || true
