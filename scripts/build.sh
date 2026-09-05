#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p bin
for target in windows/amd64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    target_os=${target%/*}
    target_arch=${target#*/}
    name="proxy-$target_os-$target_arch"
    if [ "$target_os" = windows ]; then name="$name.exe"; fi
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -o "bin/$name" ./cmd/proxy
done
