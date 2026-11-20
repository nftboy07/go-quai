#!/usr/bin/env bash
# Reproduces the fork-vs-upstream comparison for nftboy07/go-quai.
# Exits 0 and prints "IDENTICAL" when the fork adds no code beyond upstream.
set -euo pipefail

FORK=https://github.com/nftboy07/go-quai
UPSTREAM=https://github.com/dominant-strategies/go-quai
TAG=v0.22.0

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"
git init -q
git remote add fork "$FORK"
git remote add upstream "$UPSTREAM"

git fetch -q fork '+refs/heads/*:refs/fork/heads/*' '+refs/tags/*:refs/fork/tags/*'
git fetch -q upstream '+refs/heads/*:refs/up/heads/*' '+refs/tags/*:refs/up/tags/*'

fork_main=$(git rev-parse refs/fork/heads/main)
up_tag=$(git rev-parse "refs/up/tags/$TAG^{commit}")
echo "fork main            : $fork_main"
echo "upstream $TAG commit : $up_tag"

tree_diff=$(git diff --stat "$fork_main" "$up_tag")
echo "tree diff main vs $TAG: ${tree_diff:-<empty>}"

fork_refs=$(git for-each-ref --format='%(objectname)' refs/fork | sort -u)
up_refs=$(git for-each-ref --format='%(objectname)' refs/up | sort -u)
echo "fork refs: $(echo "$fork_refs" | wc -l)  upstream refs: $(echo "$up_refs" | wc -l)"

# shellcheck disable=SC2086
unique=$(git rev-list --count $fork_refs --not $up_refs)
echo "commits in fork (all branches+tags) not in upstream: $unique"

if [[ "$fork_main" == "$up_tag" && -z "$tree_diff" && "$unique" == "0" ]]; then
  echo "RESULT: IDENTICAL - fork adds no code beyond upstream"
else
  echo "RESULT: DIFFERS - review fork-only commits"
  exit 1
fi
