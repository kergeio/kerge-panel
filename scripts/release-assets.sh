#!/bin/bash
# Builds the files of a panel release into an empty directory: the
# deployment files from deploy/, with the compose files pinning the panel
# image by the digest just pushed, install.sh with the release version
# written in when the repository has one, and checksums.txt over all of
# them. Signing checksums.txt is left to the caller, which holds the key.
#
# Usage: scripts/release-assets.sh <version> <image digest> <out-dir>
#   <version> has no leading "v", for example 0.1.0 or 0.1.0-rc.1;
#   <image digest> is sha256:<64 hex digits>.
set -euo pipefail

readonly IMAGE="ghcr.io/kergeio/kerge"
readonly COMPOSE_FILES=(compose.yml compose.external-proxy.yml)
readonly OTHER_FILES=(Caddyfile Caddyfile.cloudflare cloudflare-ips.txt)

die() {
	echo "release-assets: $*" >&2
	exit 1
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$@"
	else
		shasum -a 256 "$@"
	fi
}

main() {
	[ $# -eq 3 ] || die "usage: release-assets.sh <version> <image digest> <out-dir>"
	local version="$1" digest="$2" out="$3"
	[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] ||
		die "version $version is not of the form 1.2.3 or 1.2.3-rc.1"
	[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || die "$digest is not an image digest"

	mkdir -p "$out"
	out="$(cd "$out" && pwd)"
	[ -z "$(ls -A "$out")" ] || die "$out is not empty"
	cd "$(dirname "$0")/.."

	# The repository refers to the image as $IMAGE:dev; a release pins the
	# image it has just pushed.
	local name
	for name in "${COMPOSE_FILES[@]}"; do
		[ "$(grep -c "^    image: $IMAGE:dev\$" "deploy/$name")" -eq 1 ] ||
			die "deploy/$name must have exactly one 'image: $IMAGE:dev' line"
		sed "s|^    image: $IMAGE:dev\$|    image: $IMAGE@$digest|" "deploy/$name" >"$out/$name"
		if grep -q "$IMAGE:" "$out/$name" || [ "$(grep -c "@$digest\$" "$out/$name")" -ne 1 ]; then
			die "could not pin the image in $name"
		fi
	done
	for name in "${OTHER_FILES[@]}"; do
		cp "deploy/$name" "$out/$name"
	done
	local files=("${COMPOSE_FILES[@]}" "${OTHER_FILES[@]}")
	if [ -f install.sh ]; then
		# The script in the repository carries VERSION="dev"; the released
		# copy installs its own release by default.
		sed "s/^VERSION=\"dev\"\$/VERSION=\"$version\"/" install.sh >"$out/install.sh"
		if [ "$(grep -c '^VERSION=' "$out/install.sh")" -ne 1 ] ||
			! grep -qx "VERSION=\"$version\"" "$out/install.sh"; then
			die "could not write the version into install.sh"
		fi
		files+=(install.sh)
	else
		echo "release-assets: the repository has no install.sh; releasing without it"
	fi

	(cd "$out" && sha256 "${files[@]}" >checksums.txt)
	echo "release-assets: done"
	cat "$out/checksums.txt"
}

main "$@"
