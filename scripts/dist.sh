#!/bin/sh
# Builds release archives for every supported platform into ./dist:
#   throne-connect_<version>_<os>_<arch>.tar.gz (.zip for Windows) and checksums.txt.
# Usage: scripts/dist.sh <version> [os/arch ...]   (default: all targets below)
set -eu

version=${1:?usage: scripts/dist.sh <version> [os/arch ...]}
shift
# os/arch[/goarm]; xray-core publishes a build for each of these.
targets=${*:-"
  linux/amd64 linux/arm64 linux/386 linux/arm/7 linux/arm/6 linux/riscv64
  darwin/amd64 darwin/arm64
  windows/amd64 windows/arm64 windows/386
  freebsd/amd64 freebsd/arm64
"}

root=$(cd "$(dirname "$0")/.." && pwd)
out=$root/dist
rm -rf "$out"
mkdir -p "$out"

for t in $targets; do
  os=${t%%/*}
  rest=${t#*/}
  arch=${rest%%/*}
  goarm=
  [ "$rest" != "$arch" ] && goarm=${rest#*/}
  label=$arch
  [ -n "$goarm" ] && label=${arch}v$goarm
  name=throne-connect_${version}_${os}_${label}
  dir=$out/$name
  exe=throne-connect
  [ "$os" = windows ] && exe=throne-connect.exe

  echo "building $name"
  mkdir -p "$dir"
  (cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=$goarm \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$dir/$exe" ./cmd/throne-connect)
  cp "$root/README.md" "$root/LICENSE" "$dir/"
  case $os in
    windows) cp "$root/scripts/get-xray.ps1" "$root/deploy/install-windows.ps1" "$dir/" ;;
    linux) cp "$root/scripts/get-xray.sh" "$root/deploy/throne-connect.service" "$dir/" ;;
    *) cp "$root/scripts/get-xray.sh" "$dir/" ;;
  esac

  if [ "$os" = windows ]; then
    (cd "$out" && zip -qr "$name.zip" "$name")
  else
    tar -C "$out" -czf "$out/$name.tar.gz" "$name"
  fi
  rm -rf "$dir"
done

cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum -- *.tar.gz *.zip >checksums.txt
else
  shasum -a 256 -- *.tar.gz *.zip >checksums.txt
fi
ls -1 "$out"
