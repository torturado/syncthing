#!/usr/bin/env bash
# Copyright (C) 2026 The Syncthing Authors.
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at https://mozilla.org/MPL/2.0/.

# Measures the cost of carrying the Tailscale integration as a fork: it
# reapplies a commit range to each release tag, resolves go.mod and go.sum
# conflicts by adding the dependency again, and reports any code conflict, the
# build, the unit tests and, with E2E=1, the end-to-end tailnet test.
# Conflicted trees are kept for manual resolution. Recorded
# conflict resolutions (rerere) are not used, so every run starts cold.
#
# Usage: script/tailscale-fork-cost.sh <base> <head> <tag>...
# Example: script/tailscale-fork-cost.sh f2ed52347 27fd20d9b v2.1.5 v2.1.6 v2.1.7

set -euo pipefail

base=$1
head=$2
shift 2

repo=$(git rev-parse --show-toplevel)
commits=$(git -C "$repo" rev-list --reverse "$base..$head")
if [ -z "$commits" ]; then
	echo "no commits in $base..$head" >&2
	exit 1
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/tailscale-fork-cost.XXXXXX")
tailscaleVersion=$(git -C "$repo" show "$head:go.mod" | awk '$1 == "tailscale.com" { print $2 }')
jsonVersion=$(git -C "$repo" show "$head:go.mod" | awk '$1 == "github.com/go-json-experiment/json" { print $2 }')

pick() {
	git -C "$tree" -c rerere.enabled=false cherry-pick "$@"
}

echo "| Release | Upstream commits from release to base | go.mod | Code conflicts | Code conflict hunks | Build | Unit tests | End-to-end | Seconds |"
echo "|---|---|---|---|---|---|---|---|---|"

for tag in "$@"; do
	tree="$work/$tag"
	git -C "$repo" worktree add -q --detach "$tree" "$tag"
	start=$(date +%s)
	behind=$(git -C "$repo" rev-list --count "$tag..$base")
	dependencies=clean
	conflicts=""
	hunks=0
	for commit in $commits; do
		if pick -x "$commit" >/dev/null 2>&1; then
			continue
		fi
		unmerged=$(git -C "$tree" diff --name-only --diff-filter=U)
		if grep -q -x -e go.mod -e go.sum <<<"$unmerged"; then
			# The dependency lines conflict: start from the release's go.mod
			# and add the dependency again.
			git -C "$tree" checkout -q --ours go.mod go.sum
			if ! (cd "$tree" && go get "tailscale.com@$tailscaleVersion" "github.com/go-json-experiment/json@$jsonVersion" && go mod tidy) >"$work/$tag-go-mod.log" 2>&1; then
				echo "$tag: regenerating go.mod failed, see $work/$tag-go-mod.log and $tree" >&2
				exit 1
			fi
			git -C "$tree" add go.mod go.sum
			dependencies=regenerated
		fi
		other=$(grep -v -x -e go.mod -e go.sum <<<"$unmerged" || true)
		if [ -n "$other" ]; then
			for f in $other; do
				hunks=$((hunks + $(grep -c '^<<<<<<<' "$tree/$f" || true)))
			done
			conflicts=$(paste -sd ' ' <<<"$other")
			break
		fi
		GIT_EDITOR=true pick --continue >/dev/null
	done

	build=skipped tests=skipped e2e=skipped
	if [ -z "$conflicts" ]; then
		if (cd "$tree" && go run build.go assets >/dev/null 2>&1 && go build ./... >/dev/null 2>&1); then
			build=ok
			if (cd "$tree" && go test -race -count=1 ./lib/... ./cmd/... >"$work/$tag-test.log" 2>&1); then tests=ok; else tests=FAIL; fi
			if [ "${E2E:-0}" = 1 ] && ! command -v docker >/dev/null; then
				# The suite skips itself without Docker, which would read as a pass.
				e2e="no docker"
			elif [ "${E2E:-0}" = 1 ]; then
				if (cd "$tree" && go test -tags tailscale_e2e -count=1 -timeout 30m ./test/tailscale >"$work/$tag-e2e.log" 2>&1); then e2e=ok; else e2e=FAIL; fi
			fi
		else
			build=FAIL
		fi
	fi
	echo "| $tag | $behind | $dependencies | ${conflicts:-none} | $hunks | $build | $tests | $e2e | $(($(date +%s) - start)) |"
done

echo
echo "Trees and logs: $work (remove each tree with git worktree remove --force)"
