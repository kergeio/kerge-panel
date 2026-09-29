#!/bin/sh
# Checks that the Cloudflare examples in deploy/examples/ list exactly the
# ranges of the snapshot in deploy/cloudflare-ips.txt. Offline; the release
# pipeline separately checks the snapshot against Cloudflare's lists.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
deploy="$root/deploy"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

grep -Ev '^[[:space:]]*(#|$)' "$deploy/cloudflare-ips.txt" | sort >"$tmp/snapshot"
if [ ! -s "$tmp/snapshot" ]; then
	echo "check-cloudflare-ips: deploy/cloudflare-ips.txt lists no ranges" >&2
	exit 1
fi

awk '$1 == "set_real_ip_from" { sub(/;$/, "", $2); print $2 }' \
	"$deploy/examples/nginx-cloudflare.conf" | sort >"$tmp/nginx"
awk '$1 == "trusted_proxies" && $2 == "static" { for (i = 3; i <= NF; i++) print $i }' \
	"$deploy/examples/Caddyfile-cloudflare" | sort >"$tmp/caddy"

status=0
for f in nginx caddy; do
	if ! cmp -s "$tmp/snapshot" "$tmp/$f"; then
		echo "check-cloudflare-ips: the $f Cloudflare example does not match deploy/cloudflare-ips.txt:" >&2
		diff "$tmp/snapshot" "$tmp/$f" >&2 || true
		status=1
	fi
done
exit $status
