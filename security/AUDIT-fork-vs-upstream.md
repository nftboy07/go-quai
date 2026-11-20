# go-quai fork audit: verdict is an honest kill (no exploitable fork-specific bug)

**Target:** github.com/nftboy07/go-quai
**Upstream:** github.com/dominant-strategies/go-quai (`go.mod` module path `github.com/dominant-strategies/go-quai`)
**Verdict:** HONEST KILL. The fork has no code of its own. Every commit in it is an upstream commit, so it contains no planted or introduced code to hunt.

## Method

1. Found the upstream from `go.mod`, then fetched every upstream branch and tag.
2. Compared the fork's default branch with the upstream release it claims to be (`Cut v0.22.0 Release`).
3. Fetched **all** fork refs (150 branches + tags) and counted commits that are reachable from any fork ref but from no upstream ref.

## Results

| Check | Result |
| --- | --- |
| fork `main` HEAD | `d36178f3a6509954e6debcbf52c5ea179840d96a` |
| upstream `v0.22.0` commit | `d36178f3a6509954e6debcbf52c5ea179840d96a` (same SHA) |
| `git diff --stat main v0.22.0` | empty (byte-identical tree) |
| Fork commits (all branches + tags) not in upstream | **0** |
| Fork branch heads missing from upstream | 0 of 150 |

Because the SHAs match, the trees, file contents and history all match too. Branches with names that suggest a bug, like `free-money`, `bug`, `super-secret-galena` and `no-fees`, are upstream's own development branches with identical SHAs (for example, `origin/free-money` = `upstream/free-money` = `4b5d662`).

## Reproduce (PoC for the verdict)

```
$ ./security/verify-fork-identity.sh
fork main            : d36178f3a6509954e6debcbf52c5ea179840d96a
upstream v0.22.0 commit : d36178f3a6509954e6debcbf52c5ea179840d96a
tree diff main vs v0.22.0: <empty>
fork refs: 209  upstream refs: 288
commits in fork (all branches+tags) not in upstream: 0
RESULT: IDENTICAL - fork adds no code beyond upstream
```

## Why there is nothing to submit

- No fork-specific code exists, so there is no planted bug to find.
- Any bug in this code is an upstream Quai bug in an old release (v0.22.0, 2023-10-21; upstream is now at v0.57.x). Bugs found here would mostly be bugs that later releases already fixed and disclosed, so they would not be new findings for this fork. They also belong to Dominant Strategies' own disclosure process, not to this repo.
- The fork's `SECURITY.md` is go-ethereum boilerplate that points to the Ethereum Foundation bounty. That is the wrong venue for Quai, and the file is unchanged from upstream.

## Venue

None. There is no finding to submit.
