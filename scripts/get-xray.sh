#!/bin/sh
# Downloads the latest xray-core for Linux into ./bin (or the dir given as $1).
set -eu
dest=${1:-bin}
case $(uname -m) in
  x86_64) asset=Xray-linux-64.zip ;;
  aarch64|arm64) asset=Xray-linux-arm64-v8a.zip ;;
  i?86) asset=Xray-linux-32.zip ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
url=https://github.com/XTLS/Xray-core/releases/latest/download/$asset
echo "downloading $url"
curl -fL --retry 3 -o "$tmp/xray.zip" "$url"
mkdir -p "$dest"
unzip -o -q "$tmp/xray.zip" xray -d "$dest"
chmod +x "$dest/xray"
"$dest/xray" version | head -1
