# ADR-1809: a run's rebuildable caches stay per run and are bounded, trimmed and dropped through descriptor-pinned agent-uid removal

**Status**: Accepted (issue #1809; implemented in this branch, with the M1/M3 real uid-split fixtures and the hosted-k8s acceptance pending, see the PRD)
**Date**: 2026-09-28
**Deciders**: agent team (issue #1809 milestones and review waves); the maintainer at plan review (the uncounted soft-cap park, trim-not-wipe).
**Related**: PRD #1809 (`prds/1809-worker-disk-safety.md`, the decision log D1-D8 this ADR condenses the lasting parts of); [ADR-837](0837-worker-disk-lifecycle.md) (the pressure recycle this design sits in front of, unchanged); [ADR-1766](1766-codex-vault-lock-park.md) (the typed `recovery_wait` cause precedent `data_volume_full` follows); [ADR-1598](1598-codex-command-storage.md) (Codex's own per-run cache, out of scope here). Serves `specs/human.md` Feature #1809.

## Decision (summary)

A Claude run's Go and npm caches live under its private HOME (`/data/agent-home/<run>`) and stay **per run**. uzi bounds them instead of sharing them: a process-ended park drops a **named list** of cache subtrees; a running run is **trimmed least-recently-used first** at a **proven quiet point** when it exceeds its cap; and a run that stays over the cap, or a volume that crosses a hard threshold mid-turn, is parked. Every removal inside a run HOME or of a run's tree goes through a **descriptor-pinned walk run as the owning agent uid**; that walk is a seam other code must use, not re-implement.

## Context

Measured on 2026-09-27: one uzi-on-uzi run's HOME reached 24G (21G of it `.cache/go-build`) on a 25 GiB data volume, and the run failed on resume with ENOSPC. Nothing bounded the caches while the run lived, the park kept the whole HOME, and the PRD #837 recycle is deferred while a worker is busy (parked runs included). Under the uid split the cache dirs are owned by `runner`/`runner-cmd`, some `0555` or `0700`, so the worker cannot even list them, and a run HOME is `worker:runner 0775`, not sticky: any live `runner` process, the run's own or another run's on the same worker, can rename a component and plant a symlink at any moment.

## The decisions

### D1: per-run caches, not a shared cache (trust boundary)

Runs on one worker may execute different repositories' untrusted code. A shared writable build or module cache would let one run plant an entry another run's build consumes; `GOFLAGS=-modcacherw` fixes permissions, not that boundary. Each run keeps its own caches and uzi bounds them. A shared cache is a possible later optimisation that needs its own trust decision.

### D2: the named cache list

Only `.cache/go-build`, `go/pkg/mod` and `.npm/_cacache` (relative to the run HOME; `RUN_CACHE_SUBTREES` in `agent/src/rmtree.ts`) are ever deleted. The removal helper refuses any other name. Everything else stays, including unknown files, the session transcript and its subagent dir, `.claude.json`, the memory dir and `go/bin`. Adding an entry needs code evidence that its tool refills it on demand.

### D3: drops only on process-ended parks, Claude runs only

The drop runs in the claim's `finally`, only for a Claude run that parked with its executor returned and its process tree reaped (usage limit, owner pause, recovery and disk parks). A gate park (`awaiting_approval`, `awaiting_input`, `awaiting_followup`) keeps a live executor and never reaches it. Codex runs keep their caches on their own per-run volume (ADR-1598). Separately, the worker's periodic reclaim drops the same named caches from any worker-owned HOME of a run the api reports parked with its process ended, whatever its runtime. The drop is best-effort under one deadline and never throws: a failed drop leaves the HOME as it was before this change.

## What outlives this work

### Descriptor-pinned agent-uid removal is the only way to delete inside a run tree while runs are live

`rmHomeSubtree` (a listed cache subtree under a HOME) and `rmTreePinned` (a whole run tree under a parent, used by the running disk reclaim) walk by **pinned directory descriptors**, never by path: every lookup goes through `/proc/self/fd/<dirfd>/<name>`, each directory is pinned with `O_PATH | O_DIRECTORY | O_NOFOLLOW`, the helper refuses to start unless its own pin is the inode the worker pinned, and the final `rmdir` re-checks the inode through the pinned parent. Under the uid split the walk runs as `runner`, then `runner-cmd`, then `runner` (then the worker for worker-owned leftovers), with no path-based worker removal at all; single-uid it runs once as the worker. A symlinked, non-directory or not-worker-owned root is refused. Without Linux `/proc/self/fd` both refuse outright; there is no path fallback. Listings are streamed and every pass is bounded by entries read and by a deadline.

Consequence for other code: anything that deletes or measures inside a run HOME or run tree while other runs' processes may be live must go through these helpers (or the same pinned prelude), never `fs.rm`/`rmHomeTree` by path. Path-based `rmHomeTree` stays only where no foreign writer can be live (boot sweep, a run's own teardown). A root the worker does not own is out of reach by design: legacy runner-owned provision dirs are skipped, not chmodded open.

### The LRU trim order

A trim (never a wipe) evicts until the caches are at or under the low-water mark (`UZI_RUN_CACHE_LOW_WATER`, default 0.6 of the cap), in this order:

1. `.cache/go-build`, oldest mtime first, one unit per `*-a`/`*-d` file or executable-cache directory (Go refreshes an entry's mtime on use); `README`, `trim.txt` and `testexpire.txt` are never touched.
2. Only while still over the low-water mark: `.npm/_cacache` by index entry, oldest `index-v5` bucket first, then only `content-v2` files no remaining bucket references (npm errors on an index entry whose content is gone). Skipped when the index could not be read completely.
3. Only while still over the **cap**: `go/pkg/mod` whole (Go cannot evict single modules from its read-only tree).

An entry whose mtime or type changed between listing and eviction was used meanwhile and is kept.

### The quiet-point proof: HOME/cwd process attribution

The CLI spawns each Bash tool command detached (new session and process group), so "the CLI's process groups are empty" does not prove the run is idle. A process is the run's when its environ holds exactly `HOME=<run HOME>` or its cwd is inside the run's HOME or worktree, found by a bounded scan of the `runner` uid's processes (single-uid: the worker's). A non-dumpable process reads EACCES even for its own uid, so an unreadable process counts only when **stat-linked**: its session or process group is a readable-attributed process's (never the worker's own), or its bounded parent chain reaches one or a run CLI pid. A proc mount with `hidepid`/`subset=`, a truncated scan, or any unknown answer is "not quiet". The same attribution drives the reap before every cache drop.

Residual: a process that changes its HOME and leaves the run's directories is invisible to the scan.

### Counted vs uncounted disk parks

Every disk park is `recovery_wait` with cause `data_volume_full`, and only four paths produce one: a disk-full failure of the clone/fetch, the claim/resume preflight, the soft cache cap and the mid-turn hard stop. Parks that mean the volume actually filled or hit the hard threshold (the first two, and the hard stop) are **counted** toward `UZI_RUN_DISK_PARK_MAX` (default 3; the run then fails with the `data_volume_full` fail origin). The soft-cap park is preventive and carries `disk_park_preventive: true`, so it is **uncounted**: a run that merely grew past its cap is never failed by it. The api accepts the flag only with that cause.

A disk-full write anywhere else after the clone (a build or test mid-turn, a commit) is not classified: it takes the run's normal failure path with its own error. The cap and the hard stop are what keep a run from reaching that point; classify-reclaim-retry covers only the clone/fetch and the claim/resume preflight.

## Consequences

- A resumed or trimmed run rebuilds cold (minutes), accepted against a failed run.
- The hard layer considers every watched Claude run in every phase (#1830). It stops (counted park) only a run whose executor is running and for which the server has acknowledged `running` for a status report newer than its last non-`running` report and newer than any declined or unreadable reply (reports are ordered by when they were sent; a `running` report still awaiting its reply changes nothing), because the api accepts a disk park only from `running` and a park would swallow a pending approval. Any other run (cloning, at the plan gate, in a revision turn, waiting on a question or follow-up, between approval and its first progress report, finalizing) is not stopped: its rebuildable caches are dropped in place, `.npm/_cacache` kept while a JS-deps install may be running, and it keeps its gate. A stop pending when a run is about to report a wait parks it from `running` instead.
  - Accepted hazards (#1830): a run in a revision planning turn, or in finalize (for example self-improve's dependency install and check suites), is not stopped: the api accepts a disk park only from `running`, and a finalizing run has left its executor, so the hard layer never asks it to stop and drops its caches in place instead. Its rebuildable Go caches can disappear under a running tool command at the hard threshold, and the tool rebuilds them; `.npm/_cacache` is kept while an npm install may be running. A clarification or completion question already posted to the run's feed stays visible when a pending stop parks the run instead of entering that wait.
- The PRD #837 recycle stays the last resort, unchanged.
