#!/bin/bash
# Checks the agent release that agent-release.env pins before a panel
# release compiles it in: the release must exist, its checksums must carry
# a valid release signature, and its install-agent.sh must match the pinned
# sha256 and install the pinned version. A panel release without a suffix
# must pin an agent release without one.
#
# Usage: scripts/check-agent-release.sh <panel tag>
set -euo pipefail

readonly AGENT_REPO="https://github.com/kergeio/kerge-agent"

die() {
	echo "check-agent-release: $*" >&2
	exit 1
}

# value prints the value of a key that agent-release.env sets exactly once.
value() {
	local key="$1" lines
	lines="$(grep -c "^$key=" agent-release.env || true)"
	[ "$lines" = 1 ] || die "agent-release.env must set $key exactly once"
	sed -n "s/^$key=//p" agent-release.env
}

main() {
	[ $# -eq 1 ] || die "usage: check-agent-release.sh <panel tag>"
	local tag="$1"
	cd "$(dirname "$0")/.."

	local version sum
	version="$(value AGENT_VERSION)"
	sum="$(value AGENT_SCRIPT_SHA256)"
	[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] ||
		die "AGENT_VERSION $version is not of the form 1.2.3 or 1.2.3-rc.1"
	[[ $sum =~ ^[0-9a-f]{64}$ ]] || die "AGENT_SCRIPT_SHA256 is not a sha256"
	if [[ $tag != *-* && $version == *-* ]]; then
		die "release $tag pins the agent pre-release $version; pin a release"
	fi

	local tmp url name
	tmp="$(mktemp -d)"
	# shellcheck disable=SC2064 # expand now: the variable is local
	trap "rm -rf '$tmp'" EXIT
	url="$AGENT_REPO/releases/download/v$version"
	for name in checksums.txt checksums.txt.sig install-agent.sh; do
		curl -fsSL --retry 3 -o "$tmp/$name" "$url/$name" ||
			die "could not download $url/$name; is agent v$version released?"
	done

	./scripts/release-verify.sh "$tmp" install-agent.sh
	local got
	got="$(sha256sum "$tmp/install-agent.sh" | cut -d' ' -f1)"
	[ "$got" = "$sum" ] ||
		die "install-agent.sh of agent v$version has sha256 $got, but AGENT_SCRIPT_SHA256 is $sum"
	grep -qx "VERSION=\"$version\"" "$tmp/install-agent.sh" ||
		die "install-agent.sh of agent v$version does not install version $version"
	echo "check-agent-release: agent v$version is released and matches agent-release.env"
}

main "$@"
