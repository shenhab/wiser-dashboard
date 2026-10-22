#!/bin/sh
# Builds wiser-dashboard for the common platforms into dist/.
# With matter/node_modules installed (cd matter && npm ci), each build also
# embeds the Matter bridge for its platform, as the releases do.
set -e
cd "$(dirname "$0")"
version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
mkdir -p dist
# Each build swaps in its platform's bridge; put back this machine's afterwards.
if [ -f matterbin/wiser-matter ]; then
	cp matterbin/wiser-matter matterbin/.own
	trap 'mv matterbin/.own matterbin/wiser-matter' EXIT
fi
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64; do
	os=${target%/*}
	arch=${target#*/}
	out="dist/wiser-dashboard-$os-$arch"
	[ "$os" = windows ] && out="$out.exe"

	# Bun's names for the platform; it has no 32-bit ARM build.
	bun_target=""
	case "$os/$arch" in
	*/amd64) bun_target="bun-${os}-x64-baseline" ;;
	*/arm64) bun_target="bun-${os}-arm64" ;;
	esac
	tags=""
	if [ -n "$bun_target" ] && [ -d matter/node_modules ]; then
		(cd matter && npx bun build --compile --target="$bun_target" src/main.ts --outfile "../matterbin/bridge-$os-$arch" >/dev/null)
		cp "matterbin/bridge-$os-$arch$([ "$os" = windows ] && echo .exe)" matterbin/wiser-matter
		tags="-tags=matter"
	fi

	CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=7 go build $tags -trimpath -ldflags="-s -w -X main.version=$version" -o "$out" .
	echo "built $out${tags:+ (with Matter)}"
done
