#!/bin/sh
# Checks that deploy/cloudflare-ips.txt lists exactly the ranges Cloudflare
# publishes today. The release workflow runs it, so a release never ships a
# stale snapshot; everyday checks stay offline and do not run it.
set -eu

readonly URLS="https://www.cloudflare.com/ips-v4 https://www.cloudflare.com/ips-v6"

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

grep -Ev '^[[:space:]]*(#|$)' "$root/deploy/cloudflare-ips.txt" | sort >"$tmp/snapshot"

: >"$tmp/fetched"
for url in $URLS; do
	curl -fsSL --retry 3 -o "$tmp/list" "$url"
	# The lists may end without a newline.
	awk 'NF { print $1 }' "$tmp/list" >>"$tmp/fetched"
done
sort "$tmp/fetched" >"$tmp/upstream"

if [ ! -s "$tmp/upstream" ]; then
	echo "check-cloudflare-upstream: Cloudflare returned no ranges" >&2
	exit 1
fi
if ! cmp -s "$tmp/snapshot" "$tmp/upstream"; then
	echo "check-cloudflare-upstream: deploy/cloudflare-ips.txt differs from Cloudflare's lists" >&2
	echo "(< snapshot, > Cloudflare); update it and the examples, then tag again:" >&2
	diff "$tmp/snapshot" "$tmp/upstream" >&2 || true
	exit 1
fi
echo "check-cloudflare-upstream: deploy/cloudflare-ips.txt matches Cloudflare's lists"
