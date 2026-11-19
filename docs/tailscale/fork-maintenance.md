# Fork maintenance cost

The integration is four commits on top of upstream `main` at f2ed52347:

| # | Commit | Change |
|---|---|---|
| 1 | c3efacb8a | threat model |
| 2 | 1eb29ba75 | discovery |
| 3 | 2054aecae | authorization of new devices |
| 4 | 27fd20d9b | listening on the tailnet only |

This report measures the cost of carrying them as a fork. Each commit was
reapplied to each of the last three releases: v2.1.5, v2.1.6 and v2.1.7.

Upstream `main` has not moved since f2ed52347, so rebasing the fork onto
`main` costs nothing today. All three releases are older than the base. They
are what someone who wants the integration on a released version has to build.

## Method

1. `script/tailscale-fork-cost.sh f2ed52347 27fd20d9b v2.1.5 v2.1.6 v2.1.7`
   cherry-picks the commits onto each tag, each in its own worktree.
   - It ignores recorded resolutions (rerere), so every run starts cold.
   - When `go.mod` and `go.sum` conflict, it regenerates them with the
     dependency added back.
   - It stops at the first code conflict and leaves the tree there.
2. Those remaining conflicts were resolved by hand, as described below.
3. Each resolved tree was built with `go run build.go assets` and
   `go build ./...`, and then ran two suites:
   - the full unit suite with the race detector,
     `go test -race ./lib/... ./cmd/...`;
   - the end-to-end tailnet suite, `go test -tags tailscale_e2e ./test/tailscale`.

## Results

| Release | Released | Upstream commits from release to base | Code conflict hunks | go.mod and go.sum | Lines resolved by hand | Build | Unit tests with `-race` | End-to-end |
|---|---|---|---|---|---|---|---|---|
| v2.1.7 | 2026-10-21 | 8 | 3 | clean | 13 | ok | pass (64 s) | pass (350 s) |
| v2.1.6 | 2026-10-06 | 22 | 3 | regenerated | 13 | ok | pass (62 s) | pass (347 s) |
| v2.1.5 | 2026-09-08 | 35 | 3 | regenerated | 13 | ok | pass (96 s) | pass (375 s) |

The code conflict hunks are the same on every release. There are two in
`lib/discover`, one each in `cache.go` and `manager.go`, both in commit 2.
There is one in `lib/model/model.go`, in commit 3. Commits 1 and 4 applied
cleanly everywhere.

On every release, the end-to-end suite disconnected a device within about two
seconds of each of these events:

| Release | Grant revoked | Node expelled | Key expired |
|---|---|---|---|
| v2.1.7 | 1.8 s | 2.3 s | 2.2 s |
| v2.1.6 | 1.8 s | 2.3 s | 2.0 s |
| v2.1.5 | 2.1 s | 2.3 s | 2.3 s |

The script's own output stops at the first code conflict, in commit 2, so it
does not see the conflict in commit 3:

| Release | Upstream commits from release to base | go.mod | Code conflicts | Code conflict hunks | Build | Unit tests | End-to-end | Seconds |
|---|---|---|---|---|---|---|---|---|
| v2.1.5 | 35 | regenerated | lib/discover/cache.go lib/discover/manager.go | 2 | skipped | skipped | skipped | 1 |
| v2.1.6 | 22 | regenerated | lib/discover/cache.go lib/discover/manager.go | 2 | skipped | skipped | skipped | 1 |
| v2.1.7 | 8 | clean | lib/discover/cache.go lib/discover/manager.go | 2 | skipped | skipped | skipped | 1 |

The series adds or changes 34 files. Conflicts hit five of them on v2.1.5 and
v2.1.6, and three on v2.1.7. Everything else applied cleanly on every release:

- every new file, among them 3,137 lines of Go code and tests in
  `lib/tailscale`, the finder and the end-to-end harness;
- 16 of the 19 existing Go files, including the 306 lines of code added to
  `lib/connections`.

Git reported every conflict. The build and both test suites found no other
problem.

## The conflicts

### 1. `lib/discover`: the cache refactor (#10956)

The fork's base is #10956 itself. That change moved result caching out of the
manager's `Lookup` and into `cachedFinder` in `cache.go`. So the series puts
its "no cache writes when both cache times are zero" rule in `cache.go`. The
releases do not have that code yet, and the hunk conflicts in `cache.go` and
`manager.go`.

The resolution keeps the release's `cache.go` and `manager.go` and adds two
things:

- in `NewManager`, the `tailnet: tailnet,` entry that the conflicting hunk
  adds to the initializer, which is one line (without it, Tailscale discovery
  never registers);
- the same rule in the release's inline lookup, which is nine lines:

```go
		// Perform the actual lookup and cache the result. With both cache
		// times zero caching is disabled, and a stored result would only go
		// stale in Cache().
		if finder.cacheTime == 0 && finder.negCacheTime == 0 {
			if addrs, err := finder.Lookup(ctx, deviceID); err == nil {
				addresses = append(addresses, addrs...)
			}
			continue
		}
```

### 2. `lib/model`: skipping ignored folders in auto-accept (a4a2da477)

The series narrows the condition that a4a2da477 (fixes #10935) added to the
auto-accept loop. That commit came after v2.1.7, so the releases have no
condition to narrow. The resolution keeps the release's loop and adds only the
allowlist check, which is three lines:

```go
				if !current.TailnetAllowsFolder(folder.ID) {
					continue
				}
```

### 3. `go.mod` and `go.sum`: dependency drift (v2.1.5 and v2.1.6)

Between v2.1.6 and the base, upstream updated its dependencies: 37 lines were
added to `go.mod` and 29 removed. v2.1.5 has the same `go.mod` as v2.1.6.
v2.1.7 has the same `go.mod` as the base, so it does not conflict here.

The script regenerates both files without any hand editing. It takes the
release's `go.mod` and runs:

```sh
go get tailscale.com@v1.98.1 github.com/go-json-experiment/json@v0.0.0-20260214004413-d219187c3433
go mod tidy
```

## What it means

- **The cost does not grow with the size of the patch.** All 3,137 lines of
  new Go code and tests applied cleanly to all three releases. The 13 lines
  resolved by hand sit in two hooks, the discovery manager and the auto-accept
  loop. In both places, upstream had changed the same lines between the
  release and the base.
- **Upstream churn on the hook sites is low.** In the twelve months up to the
  base (November 2026), upstream changed `lib/model/model.go` in 8 commits.
  The other hook files changed in 0 to 3 commits each:
  - `lib/connections/service.go`, `lib/config/optionsconfiguration.go` and
    `lib/syncthing/syncthing.go`: 3 each;
  - `lib/discover/cache.go` and `lib/config/config.go`: 2 each;
  - `lib/discover/manager.go` and `lib/config/deviceconfiguration.go`: 1 each;
  - `lib/connections/structs.go`: none.

  `go.mod` changed in 14 commits. Expect a few lines to resolve by hand when a
  change touches one of these hooks, and a mechanical `go.mod` regeneration
  whenever upstream updates dependencies.
- **The largest exposure is `lib/connections/service.go`, with 276 added
  lines.** It did not conflict here. It is still the hook most likely to
  conflict as upstream changes the connection pipeline.
- **Both code conflicts are backport conflicts.** The releases predate two
  changes that the fork's base already contains, so these conflicts do not
  recur while the fork follows `main`.
- **One hook could go upstream on its own.** The zero-cache-time rule in
  discovery does not depend on Tailscale. Upstreaming it would shrink the
  discovery hook to the finder registration.

## Reproducing

```sh
script/tailscale-fork-cost.sh f2ed52347 27fd20d9b v2.1.5 v2.1.6 v2.1.7
```

The script prints its table, as in the second table above, and the directory with its worktrees and
logs. To finish a release:

1. In its worktree, resolve the conflicts as described above.
2. Run `git cherry-pick --continue`.
3. Cherry-pick the remaining commits.
4. Build and test as in the method.

Remove the worktrees with `git worktree remove --force` when done. Set `E2E=1`
to run the end-to-end suite on releases that apply cleanly. It needs Docker and
the images that the end-to-end section of [README.md](README.md) lists.
