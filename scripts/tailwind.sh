#!/bin/sh
# Downloads the pinned Tailwind CSS standalone CLI (verifying its sha256),
# caches it under .cache/, and runs it with the given arguments.
set -eu

VERSION=v4.3.3

os=$(uname -s)
arch=$(uname -m)
case "$os/$arch" in
Linux/x86_64 | Linux/amd64)
	asset=tailwindcss-linux-x64
	sum=dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a
	;;
Linux/aarch64 | Linux/arm64)
	asset=tailwindcss-linux-arm64
	sum=55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195
	;;
Darwin/arm64)
	asset=tailwindcss-macos-arm64
	sum=cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d
	;;
Darwin/x86_64)
	asset=tailwindcss-macos-x64
	sum=7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1
	;;
*)
	echo "tailwind.sh: unsupported platform $os/$arch" >&2
	exit 1
	;;
esac

root=$(cd "$(dirname "$0")/.." && pwd)
dir="$root/.cache/tailwind/$VERSION"
bin="$dir/$asset"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

if [ ! -x "$bin" ] || [ "$(sha256 "$bin")" != "$sum" ]; then
	mkdir -p "$dir"
	tmp="$bin.download"
	echo "tailwind.sh: downloading $asset $VERSION" >&2
	curl -fsSL -o "$tmp" "https://github.com/tailwindlabs/tailwindcss/releases/download/$VERSION/$asset"
	got=$(sha256 "$tmp")
	if [ "$got" != "$sum" ]; then
		rm -f "$tmp"
		echo "tailwind.sh: sha256 mismatch for $asset: got $got, want $sum" >&2
		exit 1
	fi
	chmod +x "$tmp"
	mv "$tmp" "$bin"
fi

exec "$bin" "$@"
