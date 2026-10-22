#!/bin/sh
# Builds wiser-dashboard for the common platforms into dist/.
set -e
cd "$(dirname "$0")"
version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
mkdir -p dist
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64; do
	os=${target%/*}
	arch=${target#*/}
	out="dist/wiser-dashboard-$os-$arch"
	[ "$os" = windows ] && out="$out.exe"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=7 go build -trimpath -ldflags="-s -w -X main.version=$version" -o "$out" .
	echo "built $out"
done
