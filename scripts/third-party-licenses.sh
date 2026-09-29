#!/bin/bash
# Prints the license texts of the code in a binary that is not this
# repository's own: the Go standard library and every module the package
# imports on the published platforms, from the versions go.mod selects.
# name=file arguments add further texts, for code that is embedded rather
# than compiled. A module without a license file is an error, so that a
# release never ships an incomplete list.
#
# Usage: scripts/third-party-licenses.sh <package> [name=file ...]
set -euo pipefail

PLATFORMS=(linux/amd64 linux/arm64)

die() {
	echo "third-party-licenses: $*" >&2
	exit 1
}

# section prints one license file under a heading.
section() {
	local name="$1" file="$2"
	printf '%s\n' "================================================================================"
	printf '%s\n' "$name"
	printf '%s\n' "${file##*/}"
	printf '%s\n\n' "================================================================================"
	cat "$file"
	printf '\n'
}

main() {
	[ $# -ge 1 ] || die "usage: third-party-licenses.sh <package> [name=file ...]"
	local pkg="$1"
	shift
	cd "$(dirname "$0")/.."
	# The versions in go.mod, not a local workspace.
	export GOWORK=off

	local modules platform
	modules="$(for platform in "${PLATFORMS[@]}"; do
		GOOS="${platform%/*}" GOARCH="${platform#*/}" go list -deps \
			-f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}' "$pkg"
	done | sort -u)"

	echo "Third-party software in this program, and its licenses."
	echo
	section "Go standard library, $(go env GOVERSION)" "$(go env GOROOT)/LICENSE"

	local path version dir files file
	while read -r path version; do
		[ -n "$path" ] || continue
		dir="$(go list -m -f '{{.Dir}}' "$path")"
		[ -n "$dir" ] || die "module $path $version is not downloaded; run go mod download"
		files="$(find "$dir" -maxdepth 1 -type f | grep -iE '/(licen[cs]e|copying|notice)[^/]*$' | sort || true)"
		[ -n "$files" ] || die "module $path $version has no license file"
		while IFS= read -r file; do
			section "$path $version" "$file"
		done <<<"$files"
	done <<<"$modules"

	local extra
	for extra in "$@"; do
		[ -f "${extra#*=}" ] || die "${extra#*=} does not exist"
		section "${extra%%=*}" "${extra#*=}"
	done
}

main "$@"
