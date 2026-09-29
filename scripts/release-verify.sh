#!/bin/bash
# Verifies a signed release directory: checksums.txt.sig must be a
# signature by the release key, and the files must match checksums.txt.
# With file names, only those files are checked and only they need to be
# present; without, every file listed in checksums.txt is.
#
# The release workflow runs it on the panel release it has just signed, so
# a signing key that does not match the published key fails the release,
# and on the agent release the panel pins.
#
# Usage: scripts/release-verify.sh <dir> [file...]
set -euo pipefail

readonly SIGNER="releases@kerge.io"
readonly NAMESPACE="kerge-release"

die() {
	echo "release-verify: $*" >&2
	exit 1
}

main() {
	[ $# -ge 1 ] || die "usage: release-verify.sh <dir> [file...]"
	local dir="$1" pubkey
	shift

	# The key install.sh verifies with, which is the key the agent
	# repository's install-agent.sh carries too.
	pubkey="$(sed -n 's/^RELEASE_PUBKEY="\(.*\)"$/\1/p' "$(dirname "$0")/../install.sh")"
	[ -n "$pubkey" ] || die "install.sh has no RELEASE_PUBKEY"

	local tmp
	tmp="$(mktemp -d)"
	# shellcheck disable=SC2064 # expand now: the variable is local
	trap "rm -rf '$tmp'" EXIT
	printf '%s namespaces="%s" %s\n' "$SIGNER" "$NAMESPACE" "$pubkey" >"$tmp/allowed_signers"
	ssh-keygen -Y verify -f "$tmp/allowed_signers" -I "$SIGNER" -n "$NAMESPACE" \
		-s "$dir/checksums.txt.sig" <"$dir/checksums.txt" ||
		die "checksums.txt.sig is not a signature by the release key"

	if [ $# -eq 0 ]; then
		cp "$dir/checksums.txt" "$tmp/expected"
	else
		: >"$tmp/expected"
		local name
		for name in "$@"; do
			grep -E "^[0-9a-f]{64} [ *]$name\$" "$dir/checksums.txt" >>"$tmp/expected" ||
				die "checksums.txt has no entry for $name"
		done
	fi
	(cd "$dir" && sha256sum -c "$tmp/expected") || die "a file does not match checksums.txt"
	echo "release-verify: the signature and the checksums are valid"
}

main "$@"
