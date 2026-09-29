#!/bin/bash
# Runs install.sh against throwaway Debian containers without network, one
# fresh container per case (see scripts/testdata/install-cases.sh). Docker
# inside the container is a stub; the downloads, the signature and checksum
# checks, the Cloudflare range handling, the files and .env are real. Needs
# Docker.
set -euo pipefail

cd "$(dirname "$0")/.."

readonly IMAGE="kerge-panel-install-test"
CASES=(
	helpers
	install
	cloudflare-detect
	cloudflare-fallback
	cloudflare-invalid
	rerun-upgrade
	switch-cloudflare
	external-proxy
	ports-busy
	dns-mismatch
	bad-signature
	bad-checksum
	no-docker
	no-domain
	domain-change
	unreleased-script
	truncated
	release-key
)

command -v docker >/dev/null 2>&1 || {
	echo "test-install: docker is required" >&2
	exit 1
}

docker build -q -t "$IMAGE" - >/dev/null <<'DOCKERFILE'
FROM debian:12-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl iproute2 openssh-client \
 && rm -rf /var/lib/apt/lists/*
DOCKERFILE

failed=0
for name in "${CASES[@]}"; do
	echo "== $name"
	# NET_ADMIN only to add a dummy interface; see give_address.
	if ! docker run --rm --network none --cap-add NET_ADMIN -v "$PWD:/src:ro" "$IMAGE" \
		bash /src/scripts/testdata/install-cases.sh "$name"; then
		echo "FAIL: $name" >&2
		failed=1
	fi
done

if [ "$failed" -ne 0 ]; then
	echo "install.sh: some cases failed" >&2
	exit 1
fi
echo "install.sh: all cases passed"
