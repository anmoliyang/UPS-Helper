#!/bin/sh
# cmd/build-usbip.sh — 在 Linux x86_64 上把 usbip 用户态客户端静态编译为无动态依赖的二进制，
# 产出 ../cmd/usbip（即 fpk 安装后的 $TRIM_APPDEST/usbip），由 cmd/main、install_init 调用。
#
# 为什么需要它：
#   飞牛 fnOS 不预装 usbip，官方也无免 apt/免 root 的 USB/IP 用户态接口。
#   本项目服务端（internal/usbip）是 Go 纯标准库自实现；但客户端 `usbip attach`
#   必须调用系统 usbip 用户态工具。为零依赖、可合规随 fpk 分发，只能自带一个
#   静态链接的 usbip 二进制。本脚本用 musl 静态编译 Linux 内核树 userspace 工具，
#   产物不依赖 glibc / libudev / libsysfs，可在飞牛 fnOS 直接运行。
#
# 用法（在 Linux x86_64 构建机执行）：
#   sh cmd/build-usbip.sh            # 默认从 kernel.org 拉取与当前内核同主版本的源码
#   KVER=6.1.12 sh cmd/build-usbip.sh# 指定内核源码版本（飞牛 fnOS 多为 6.x）
set -eu

# 架构选择：amd64 (默认) | arm64
#   amd64 -> x86_64-linux-musl   (对应 fpk manifest platform=x86)
#   arm64 -> aarch64-linux-musl  (对应 fpk manifest platform=arm)
ARCH="${1:-amd64}"
case "$ARCH" in
  amd64) HOST=x86_64-linux-musl ;;
  arm64) HOST=aarch64-linux-musl ;;
  *) echo "错误：未知 arch '$ARCH'（仅支持 amd64 / arm64）" >&2; exit 1 ;;
esac

OUT="$(cd "$(dirname "$0")" && pwd)/usbip"
KVER="${KVER:-$(uname -r | cut -d- -f1)}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "==> 目标：$OUT （静态 usbip，内核 userspace 工具，KVER=$KVER）"

# 1) 装 musl 工具链与头文件（apt 仅用于构建机，不影响 fpk 运行时）
if command -v apt-get >/dev/null 2>&1; then
  sudo apt-get update
  sudo apt-get install -y musl-tools linux-libc-dev pkg-config
elif command -v apk >/dev/null 2>&1; then
  sudo apk add musl-dev linux-headers pkgconf
fi
export CC=musl-gcc

# 2) 取内核源码（仅 tools/usb/usbip 需要；用稳定版 tarball 避免完整 clone）
cd "$WORK"
URL="https://cdn.kernel.org/pub/linux/kernel/v${KVER%%.*}.x/linux-${KVER}.tar.xz"
if [ ! -f "linux-${KVER}.tar.xz" ]; then
  echo "==> 下载内核源码 $URL"
  curl -fsSL "$URL" -o "linux-${KVER}.tar.xz"
fi
tar -xf "linux-${KVER}.tar.xz"
SRC="$WORK/linux-${KVER}/tools/usb/usbip"

# 3) 编译 userspace（autotools；--enable-static + 静态链接 musl）
cd "$SRC"
./autogen.sh
./configure --host="$HOST" \
  --prefix="$WORK/install" \
  --disable-shared --enable-static \
  LDFLAGS="-static" CFLAGS="-Os -static"
make -j"$(nproc)"

# 4) 取出 usbip / usbipd 静态二进制（客户端只要 usbip）
"$WORK/linux-${KVER}/tools/usb/usbip/src/usbip" --version || true
cp "$WORK/linux-${KVER}/tools/usb/usbip/src/usbip" "$OUT"
chmod 0755 "$OUT"

echo "==> 完成：$OUT"
file "$OUT"
echo "==> 确认无动态依赖（应显示 'statically linked'）："
ldd "$OUT" 2>&1 || true
