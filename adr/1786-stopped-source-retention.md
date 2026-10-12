# ADR-1786: A stopped generation's source is kept until preservation is proven or an operator disposes it

**Status**: Accepted
**Date**: 2026-10-12
**Issue**: [vtmocanu/uzi#1786](https://github.com/vtmocanu/uzi/issues/1786)
**Related**: ADR-1197 (`adr/1197-transient-recovery-park.md`), ADR-1783 (`adr/1783-run-quiescence-and-attempt-clone-paths.md`); spec `prds/1786-preserve-worker-only-work.md`

## Context

A worker must stop acting when its claim is fenced or superseded. That does not prove the
stopped clone's unfetched commits, dirty tracked files or untracked files survive elsewhere.
The stale-claim, claim-fence and `RunningAckTerminalError` arms of `executeClaim`, and the
clears in `handleRecoveryExhausted`, followed the "#1247 convention": the new claim has its own
clone, so tear the old one down. A successor clone does not contain the predecessor's
unfetched or uncommitted bytes, so that convention deleted the last copy.

## Decision

### Invariant

A stopped generation's exact local source is kept until one of two things happens:

- complete preservation is proven **at cleanup time**, or
- an operator explicitly disposes of it.

None of the following is proof: a successor's claim, a released hold (including a successor's
release of a predecessor hold), a terminal run status, or a capture result remembered from
earlier in the exit. Guarded FINAL over bare roots does not authorize disposal either.

### Representation (no new artifact store)

The artifact is the existing clone; nothing is copied.

- **Attempt clone (Docker-wired worker):** `GitCache.retainStoppedAttempt` appends ledger
  state `reclaimed`, which the ledger already treats as never deleted and never counted toward
  the abandoned cap. It then clears the active journal, but only when that journal is plain
  and names exactly this source. A same-run successor therefore finds no retained journal,
  seeds a fresh attempt and is neither adopted into nor failed by the old clone. An episode
  journal (`recovery` or `retainedSources`) is left untouched; it is already protected.
- **Canonical clone (non-Docker worker):** `GitCache.markStoppedCanonical` writes a bounded
  bare-config marker, `uzi-stopped.<runId>.entry` (JSON with `version`, `runId`, `generation`,
  `branch`, `key`, `clonePath`, `journalDigest`, `reason`, `at`), bound to the sha256 of the
  exact canonical journal, and keeps the plain journal. The marker grants no capture,
  adoption or release authority; it only refuses reuse. Discovery and seeding throw
  `RetainedStoppedSourceError` (a `RunResidueBlockedError`, reason `worker_residue_blocked`)
  instead of adopting the stale tip. Because the canonical path is shared per issue, any later
  run of that issue on that worker fails once with that error until the source is discarded.
- A malformed value, a value over 4 KiB, more than 64 values per run, a persistence failure or
  a latched residue quarantine yields `unknown`: nothing is written or truncated and the source
  stays.
- Neither path calls `/state`, publishes, archives, finalizes, touches credentials or writes
  `refs/uzi-runner/*`. `RunRunner.retainStoppedSource` first latches
  `preserveRecoveryClone` before any await, then runs own-mode quiescence with a forced
  process scan (killing only attributed processes and containers, never a successor), then
  records retention whatever the proof returned. `preserveSession` is not newly set, so
  existing HOME cleanup still runs.

### Coverage: when a stopped clone may still be torn down

`GitCache.verifyStoppedSourceCovered` runs under the bare lock, with the flight's execution
tail excluding same-run successors, and is evaluated when the exit cleans up, not when a
capture was taken. It returns `covered` only if all of these hold:

- the journal (and, for a canonical clone, the digest-bound marker) still names exactly this
  source, and the path is a real directory with a real `.git` directory;
- `HEAD` equals the head the capture verified, and the clone is clean (no dirty tracked and no
  untracked non-ignored bytes);
- `HEAD` is anchored now: the tracking tip this run and generation owns, an owed pin of this
  run, or a commit already on the bare's default branch (a flight that never committed past
  its seed base);
- every local ref of the clone, in any namespace, points at a commit reachable from `HEAD` or
  from a ref of the bare; there is no stash entry; and no linked worktree exists;
- a capture-mode proof was quiescent before and after the reads.

Anything else (unknown, changed, unanchored, unreadable, blocked) is `not_covered` and the
source is retained. The accepted residuals are stated in `docs/run-recovery.md`: gitignored
files are not covered by any capture; commits reachable only from the reflog (for example
after `git reset --hard`) are not counted as source; and a bare ref that justified coverage
can later move (a forge force-push, pin settlement). Refs that exist only because the seed
created them are not treated as worker-only work.

### Operator channel

Retention needs an explicit reclaim path, including when no server hold exists.

- **Transport.** A Unix socket at `<dataDir>/ops/retained-source.sock` (0600, in a worker-owned
  0700 directory), served by the live Worker. No TCP listener, no API endpoint. Setup failure
  is logged and non-fatal. Requests are NDJSON, at most 8 KiB in and 64 KiB out, with one
  destructive request at a time (a concurrent one gets `busy`).
- **`inspect`** is read-only and content-free, under one 5 s budget; a budget hit reports
  `partial` or `unknown`, never 0.
- **`discard`** requires the descriptor digest from inspect (`--confirm`). It closes the claim
  gate under a distinct `retained_ops` owner, waits at most 5 s for in-flight claims to
  drain, requires `Worker.isIdle()`, and holds the run's reservation through disposal. It never
  cancels a run or stops the worker. `--generation none` is accepted for canonical and `gx`
  sources only when run and path identity are positively established.
- **Server holds are read before the bare lock.** The server hold state comes from
  `client.listRecoveryHolds` and is never inferred from local files. The read happens inside
  the run reservation but before any bare lock, so no network call runs under the lock. It
  is current because the claim CTE is the only production insertion of a hold: with the gate
  closed, claims drained, the worker idle and the reservation held, nothing on this worker
  can open a hold between the read and the deletion.
- **Local custody records are independent consumers.** `recovery/<runId>` records,
  `recovery-settlement/<runId>` records and physical terminal protection are each named and
  resolved through their own existing path. `inspect` reports one disposition:
  `owner_hold_discard <id>`, `resolve_local_record`, `local_protected_discard` (only when
  the server read succeeded and lists no hold, no local record is unresolved, and every
  source is exclusively attributable), or `refused` naming the consumers.
- **One bare lock.** The protected discard runs through a private locked core of
  `discardRetainedRecovery` (`discardRetainedRecoveryUnderLock`); the public wrapper is
  `withLock` plus that core. The lock is taken once and held from the descriptor snapshot
  through deletion and metadata retirement, so no writer runs between physical disposal and
  metadata retirement.
- **Deletion** reuses `deleteRetainedArtifact`; a timeout is never success. Metadata is
  retired only after every path is verified gone. Results are `removed`, `retained`,
  `missing` or `unknown`.
- **Client.** `agent/src/ops/retained-source.mjs` imports only `node:` builtins and is run by
  plain node as `/usr/bin/env -i PATH=... /usr/local/bin/node /app/src/ops/retained-source.mjs`,
  every binary named by an absolute root-owned path (`env -i` removes `NODE_OPTIONS`,
  `NODE_PATH` and tsx cache influence).
- **Trust limit.** On single-UID workers the model shares the worker uid and can reach the
  socket and modify `/app`; the channel adds no capability beyond that uid's existing write
  access to `/data/runner` and is not a model boundary. A Bash-only guardrail deny list is
  defense in depth; the uid split is the boundary.

### Recovery-handler exits (M2)

Each site in `handleRecoveryExhausted` that cleared `preserveRecoveryClone` on `captured`,
`cancelCaptured` or a cached `capture` instead calls `verifyStoppedSourceCovered` with the
capture's `capturedHead` and clears the flag only on `covered`; otherwise it calls
`retainStoppedSource`. A memo without a `capturedHead`, including the degraded
`{ verified: false }` stub, is `not_covered`. Parks are unchanged per ADR-1197: park timing,
report count and order, retry timing, `recovery_wait` returning `true` for a
credential-deferred run, and the accepted cross-worker durability limit. A local-only park
retains its original clone; that is source retention, not a promise another worker can resume
it.

## Alternatives rejected

- **Keep the stale-teardown convention (#1247).** A successor's clone does not contain the
  predecessor's unfetched or uncommitted bytes.
- **A new artifact store with its own retention/GC.** Duplicates bytes the clone already holds.
- **Reuse `transferRestorePointToTrustedBare`.** It advances shared branch tracking, which a
  stopped claim has no authority to do.
- **Mark the attempt `abandoned`, TTL deletion or FIFO eviction.** A cap cannot authorize
  deleting the last unverified copy.
- **Keep the active journal for attempt clones.** It routes the next same-run claim into
  `recoverRetainedClaim`, which adopts or fails the run; clearing it lets a successor seed a
  fresh attempt.
- **Refuse discard without a generation (generation-less discard refusal).** Canonical and
  `gx` clones have none; `--generation none` with positive identity covers them. Generation 0
  is never substituted.
- **Run the client with tsx.** A tsx cache directory is runner-plantable; plain node under
  `env -i` with absolute paths is not.
- **Infer hold state from local files.** `custodyHeld` says only that local protection
  exists; an empty server list cannot distinguish absent from released, and inspect says so.
- **A nested bare lock** (take the lock in the operator discard, then call the public
  wrapper). `withLock` is not reentrant; a second acquisition deadlocks, and releasing
  between deletion and retirement opens a race.
- **Trust cached capture success.** It is a single-instant ref comparison; the clone can change
  before the exit.
- **Per-ref `merge-base` as coverage.** Replaced by "every local ref reachable from `HEAD` or a
  bare ref", which also catches a stash, tags and a linked worktree.
- **Treat seed-created refs as worker-only work.** They exist in the bare already and would
  retain every clone.
- **Durable-before-park retry loop and archive attempts.** Out of scope (#1790); ADR-1197 park
  semantics are kept.
- **An admin API discard delivered by heartbeat, or stopping or replacing the pod to get an
  idle point.** Larger contract surface, and replacing the pod destroys the `emptyDir` source.

## Consequences

- Retention raises ephemeral-storage pressure; existing disk admission still sees the real
  bytes, and there is no TTL or automatic reclaim.
- Local retention is not a checkpoint, archive or server hold. Same-pod process restart keeps
  the clone; pod replacement loses an `emptyDir` clone while the ledger or marker survives
  (reported missing, never empty); PVC teardown loses both.
- Operator procedure: `docs/run-recovery.md`, "When a run is stopped by a stale or fenced
  claim" and "Retained sources with no open hold (worker-local inspect and discard)".
