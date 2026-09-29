#!/bin/bash
# Checks that a pushed tag may be released, before anything is built:
#   - the tag is a version, v1.2.3 or v1.2.3-rc.1;
#   - it points at a commit on main;
#   - the CI workflow passed on that commit. A run still in progress is
#     waited for, so a tag pushed right after its commit does not fail.
#
# Usage: scripts/release-preflight.sh <tag>
# Needs GITHUB_REPOSITORY, and GH_TOKEN with read access to Actions.
set -euo pipefail

readonly WORKFLOW="ci.yml"
readonly POLL_SECONDS=30
readonly MAX_POLLS=60

die() {
	echo "release-preflight: $*" >&2
	exit 1
}

main() {
	[ $# -eq 1 ] || die "usage: release-preflight.sh <tag>"
	local tag="$1"
	[[ $tag =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] ||
		die "tag $tag is not a version such as v1.2.3 or v1.2.3-rc.1"
	: "${GITHUB_REPOSITORY:?}"

	local commit
	commit="$(git rev-parse "$tag^{commit}")"
	git fetch --quiet origin main
	git merge-base --is-ancestor "$commit" FETCH_HEAD ||
		die "$tag points at $commit, which is not on main"
	echo "release-preflight: $tag is $commit on main"

	local poll states
	for ((poll = 1; poll <= MAX_POLLS; poll++)); do
		states="$(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/$WORKFLOW/runs?head_sha=$commit&event=push" \
			--jq '.workflow_runs[] | "\(.status) \(.conclusion)"')"
		if grep -qx 'completed success' <<<"$states"; then
			echo "release-preflight: CI passed on $commit"
			return
		fi
		if [ -z "$states" ] || ! grep -qv '^completed ' <<<"$states"; then
			die "CI has not passed on $commit (runs: ${states:-none})"
		fi
		echo "release-preflight: CI is still running on $commit; waiting"
		sleep "$POLL_SECONDS"
	done
	die "CI did not finish on $commit in time"
}

main "$@"
