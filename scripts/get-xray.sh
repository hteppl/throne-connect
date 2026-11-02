#!/bin/sh
# Downloads the latest xray-core for this machine (Linux, macOS or FreeBSD) into ./bin
# (or the dir given as $1).
set -eu
dest=${1:-bin}
case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=macos ;;
  FreeBSD) os=freebsd ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case $(uname -m) in
  x86_64|amd64) arch=64 ;;
  aarch64|arm64) arch=arm64-v8a ;;
  i?86) arch=32 ;;
  armv7*) arch=arm32-v7a ;;
  armv6*) arch=arm32-v6 ;;
  riscv64) arch=riscv64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
asset=Xray-$os-$arch.zip
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
url=https://github.com/XTLS/Xray-core/releases/latest/download/$asset
echo "downloading $url"
curl -fL --retry 3 -o "$tmp/xray.zip" "$url"
mkdir -p "$dest"
unzip -o -q "$tmp/xray.zip" xray -d "$dest"
chmod +x "$dest/xray"
"$dest/xray" version | head -1
