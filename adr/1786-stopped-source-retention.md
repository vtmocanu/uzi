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
- **Canonical clone (non-Docker worker):** `GitCache.markStoppedCanonical` (amended 2026-10-12,
  maintainer decision 1) does three things in one bare-lock section, in this order, after every
  validation has passed (the only write that can precede them is retiring this run's own stale
  markers, those whose path is verifiably gone and unnamed): it persists the key's attempt mode
  (`uzi-attemptmode.<key>.enabled`, the existing mechanism used after retained recovery) and
  reads it back; it writes a bounded bare-config marker, `uzi-stopped.<runId>.entry` (JSON with
  `version`, `runId`, `generation`, `branch`, `key`, `clonePath`, `journalDigest`, `reason`,
  `at`), bound at write time to the sha256 of the exact plain journal, and reads it back; and
  it clears that exact plain journal, only while its raw value is still the one the digest was
  computed from. The marker, not the journal, now attributes and protects the kept source. The
  marker grants no capture, adoption or release authority. Later claims of the issue (a newer
  generation of the same run, or a different run) see attempt mode and seed a fresh attempt
  path, so the kept clone is untouched, attributable and listed by inspect (marker state
  `detached`). Discovery heals a half-done detachment (a bound plain journal plus a valid
  marker whose digest matches) by persisting the mode and clearing that journal. The claim
  re-reads attempt mode right before seeding and retries once on a fresh path. The run's FINAL
  inventory counts marker-kept paths, so a missing one makes that run's inventory unknown until
  discard. Discard clears a journal only when it still names that source, never a successor's.
- A malformed value, a value over 4 KiB or more than 64 values per run yields `unknown`:
  nothing is written or truncated and the source stays. See Fail-closed cases below for the
  full list.
- Neither path calls `/state`, publishes, archives, finalizes, touches credentials or writes
  `refs/uzi-runner/*`. `RunRunner.retainStoppedSource` first latches
  `preserveRecoveryClone` before any await, then runs own-mode quiescence with a forced
  process scan (killing only attributed processes and containers, never a successor), then
  records retention whatever the proof returned. `preserveSession` is not newly set, so
  existing HOME cleanup still runs.

### Coverage: when a stopped clone may still be torn down

`GitCache.verifyStoppedSourceCovered` runs under the bare lock, with the flight's execution
tail excluding same-run successors, and is evaluated at the exit's cleanup, not when a
capture was taken (the recovery handler evaluates it once, at its own exit; see
Recovery-handler exits below). It returns `covered` only if all of these hold:

- the journal (and, for a canonical clone, the digest-bound marker) still names exactly this
  source, and the path is a real directory with a real `.git` directory;
- `HEAD` equals the head the capture verified, and the clone is clean (no dirty tracked and no
  untracked non-ignored bytes);
- `HEAD` is anchored now: the tracking tip this run and generation owns, an owed pin of this
  run, or a commit already on the bare's default branch (a flight that never committed past
  its seed base);
- every local ref of the clone, in any namespace, points at a commit reachable from `HEAD` or
  from a ref of the bare; there is no stash entry; and no linked worktree exists;
- a capture-mode proof was quiescent before and after the reads;
- there is no populated submodule: `status` runs with `--ignore-submodules=none`, and a
  `.git/modules` store or an index gitlink whose path holds anything but nothing or an empty
  directory is `not_covered`, including under `ignore=all` (set in `.gitmodules` or config),
  since the parent proof says nothing about a submodule's own files, commits or refs;
- the final process proof reaped no writer: a writer reaped by the "after" scan existed after
  the byte checks ran, so they do not cover what it wrote. The residual is a writer that
  writes and exits between the status read and that proof;
- the stopped-source proof's Docker half was `not_wired` or `docker_unconfirmed`. A failed
  Docker teardown (`docker_error`) skips the coverage check and keeps the clone, because the
  proof scans processes only.

Status reads on a kept clone run with `GIT_OPTIONAL_LOCKS=0`, so they never rewrite its
index stat cache.

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
  `partial` or `unknown`, never 0. It lists attempt-ledger sources, canonical sources with a
  `uzi-stopped` marker (marker state `bound` while the plain journal is pending, `detached`
  normally, `mismatch` otherwise), and (maintainer decision 2, 2026-10-12) canonical clones
  kept under their plain journal with no marker (marker state `none`, journal state `plain`,
  consumer `run_state:not_known_locally`). A marker-less record is hidden only by evidence
  that names its path: a valid marker by its parsed `clonePath`, a malformed marker whose text
  carries the path not followed by a clone-key character (so a marker for `issue-290` never
  hides `issue-29`; a malformed marker naming `issue-29.attempt-…` likewise hides the plain
  `issue-29` record, failing closed), the leftover's own run overflowing the marker cap, or a
  protected record that owns the path.
- **`discard`** requires the descriptor digest from inspect (`--confirm`). It closes the claim
  gate under a distinct `retained_ops` owner, waits at most 5 s for in-flight claims to
  drain, requires `Worker.isIdle()`, and holds the run's reservation through disposal. It never
  cancels a run or stops the worker. On a Docker-wired worker discard itself runs the Docker
  teardown, which force-removes containers that bind-mount the clone (with their anonymous
  volumes), and is refused unless that teardown reported `docker_unconfirmed`; a `docker_error`
  or any other state refuses the destructive request, since a container may still mount the
  clone. `--generation none` is accepted for canonical and `gx`
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
  `owner_hold_discard <id>`, `resolve_local_record`, `local_discard` /
  `local_protected_discard` (only when the server read succeeded and lists no hold, no local
  record is unresolved, and every source is exclusively attributable), or `refused` naming the
  consumers. A plain-journal canonical record confirmed for discard is deleted with the same
  digest confirmation and three-fact reporting (server holds, local records, terminal
  protection), and its plain journal is cleared (only while its raw value is unchanged and it
  still names exactly that source), so a later resume seeds fresh.
- **Actively parked, and its gap.** An open hold of the run means the run is still parked, so
  `owner_hold_discard` refuses and deletes nothing: this is the actively-parked refusal. A
  local record or a live flight also refuses. The worker has no authoritative parked signal and
  makes no new network call, so it cannot tell more. A hold can be released (claim released,
  capture available) or never opened (`paused` and `limit_wait` parks, recovery-disabled
  workers) while the run is still parked. Such a source is therefore reclaimable by explicit
  operator confirmation and loses every byte not already in a capture, published checkpoint or
  the tracking ref.
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

`handleRecoveryExhausted` no longer clears `preserveRecoveryClone` at a capture or at an exit
site. A site only marks release as eligible (`markReleaseEligible`) or keeps the clone
(`keepClone`); the handler's `finally` runs `settleCloneRelease` once, after the last
possible writer on every return path, and that is the only place the clone can be released.
It calls `retainStoppedSource` with the capture's `capturedHead`, and the clone goes only
when `verifyStoppedSourceCovered` returns `covered` then. A remembered capture result, a
memo without a `capturedHead` (including the degraded `{ verified: false }` stub) and a
check that throws are all `not_covered`: the clone is kept.

The exit decides the representation of a source that is not covered:

- **Stops this generation** (a `stop` confirm, an ownership read showing another
  generation, the terminal-disk recheck or the pre-handler precheck showing another
  generation, a stale park report, plus the M1 stale and fenced arms): the stopped-source
  record, as in M1 (attempt ledger `reclaimed` and the journal cleared; canonical
  `uzi-stopped` marker).
- **Park exits** (`recovery_wait`, a live non-terminal status, shutdown) and **terminal
  exits** (a terminal status or a terminal-disk recheck showing a terminal status): the
  existing ADR-1197 plain-journal posture. The clone and journal are kept and no marker or
  `reclaimed` entry is written, so a same-worker resume recaptures bytes written after the
  capture, and a later run's existing terminal-orphan handling applies.

Two exits keep their earlier shape. An owner-authorized cancel (a non-credential cancel
ACK) keeps its normal cleanup, since the owner chose to discard; only the
credential-deferred captured cancel is revalidated. A `predecessorCapture` flight is
released in place (ledger abandoned, journal cleared; no filesystem operation on the
clone), so it is not revalidated; the clone is removed later by the retention sweep.

Park and terminal exits that keep the clone outright (`credentialHeld`, an unverified
credential terminal, `receipt_failed`, `ServerWallParkedError`, a confirm `gone` without
a capture) run no own-mode proof beyond the existing entry kill, as before; only exits
that become release-eligible run the `stopped_source` proof, once, at the handler's
cleanup. An interruption propagating out of the handler (shutdown, pause, limit, a codex
deferral, any throw) skips that cleanup check; the preserve flag stays as the
interrupting arm set it (shutdown, pause and limit keep the clone; an owner cancel takes
its normal cancel cleanup; a claim fence or stale claim goes to the caller's stopped-source
arm; a codex deferral or any other throw keeps the clone as latched), and the caller handles the interruption.

Claims without a claim generation (pre-generation APIs) can never be proven covered, so
their park and terminal exits keep the clone, and a same-worker reclaim of such a run
stops silently in retained recovery. Current APIs always send a generation.

An aggregate inspect lists attempt clones through their ledger entry. A canonical clone kept
by a park or terminal exit under its plain journal is also listed (marker state `none`,
consumer `run_state:not_known_locally`) and can be discarded through the channel once no hold,
local record or live flight uses it; the same run's resume on that worker still recaptures it,
and the next run's terminal-orphan handling still applies.

Park timing, report count and order, retry timing, `recovery_wait` returning `true` for a
credential-deferred run, and the accepted cross-worker durability limit are unchanged per
ADR-1197. A local-only park retains its original clone; that is source retention, not a
promise another worker can resume it.

### Fail-closed cases

Every case keeps the source. What a later run sees depends on where the failure sits:

- **Refused with `worker_residue_blocked`** (`RetainedStoppedSourceError`, only when marker
  evidence names the path or the run):
  - malformed or overflowing marker evidence naming the path or the run
    (`assertNoStoppedMarker`, `refuseMarkedPaths`);
  - a bound journal whose digest no longer matches the marker (state `mismatch`);
  - separately, the fresh seed's existing seed-mode quiescence finding unattributed processes
    in the key's footprint (the #1783 residue rule), which this amendment does not change.
- **Nothing recorded.** Writing the attempt mode or the marker fails, so `markStoppedCanonical`
  returns `unknown` with no marker. The source is kept under its plain journal and routes as
  before: the same run's next claim enters ordinary retained recovery
  (`recoverRetainedClaim`), a different run goes through the existing foreign-journal handling
  (`ForeignCaptureBlockedError`, then terminal-orphan handling), and inspect lists it as a
  marker-less plain-journal leftover. (Malformed or overflowing marker evidence of the run is
  the refused case above, not this one.)
- **Journal no longer names the source.** When the active journal is absent or already names
  another source when the stop is recorded, nothing is written and that journal keeps its own
  routing. The kept clone is then not under any journal or marker and inspect does not list it;
  the next claim never reuses the canonical path once the key is in attempt mode, but nothing
  attributes the clone (a narrow race: custody-held flights return before this call). If the
  journal was absent, no attempt mode was written either, so the next canonical claim on that
  key frees the path as before #1786; the journal is written before any model runs, so such a
  clone holds only its fresh seed. This is the one case where "kept" holds only at stop time. A
  stale marker already naming the path is the `mismatch` case above when another run's journal
  names it.
- **Corrupt attempt mode or failed heal.** A malformed attempt-mode value, or a failed write
  during the discovery heal, fails the run closed with "Retained recovery blocked: corrupt
  journal or unsafe local recovery storage" (the default `fail_origin`, not
  `worker_residue_blocked`). The source is kept.
- **Latched residue quarantine.** `markStoppedCanonical` still persists the attempt mode,
  writes the marker and answers `retained`, but keeps the journal (the latch check in
  `clearBoundJournal`). A quarantined worker claims nothing until its container restarts; the
  next discovery then finishes the detachment.

## Alternatives rejected

- **Keep the stale-teardown convention (#1247).** A successor's clone does not contain the
  predecessor's unfetched or uncommitted bytes.
- **A new artifact store with its own retention/GC.** Duplicates bytes the clone already holds.
- **Reuse `transferRestorePointToTrustedBare`.** It advances shared branch tracking, which a
  stopped claim has no authority to do.
- **Mark the attempt `abandoned`, TTL deletion or FIFO eviction.** A cap cannot authorize
  deleting the last unverified copy.
- **Keep the plain journal on a canonical clone and refuse every later run of the issue**
  (the earlier shape of this ADR). Rejected by maintainer decision 1 (2026-10-12): a worker
  with no Docker host would fail each later run of the issue once until an operator discards
  the clone. Persisting attempt mode and clearing the journal lets later runs proceed on a
  fresh path while the marker keeps the clone attributable.
- **Read park state from the server to decide discard.** Rejected by maintainer decision 2
  (2026-10-12): it adds a network call to a worker-local operator path, and the answer is
  still not authoritative for `paused` and `limit_wait` parks. Open-hold refusal plus explicit
  digest confirmation is used instead.
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
- A key whose canonical clone was kept stays in attempt mode on that worker permanently, so
  later claims of the issue start a fresh model session; the work carries over through the
  tracking ref and checkpoints. While a run's marker exists, that run's later stops are never
  judged covered. A dirty kept clone keeps the run's hold, so its route is `owner_hold_discard`.
- **Not shipped:** teardown of attributed containers is not proven by tests or hosted
  acceptance (tracked in #2747). The teardown code ships (`teardownDocker` force-removes
  containers mounting the clone and answers `docker_unconfirmed` only after two clean
  listings), and the coverage proof scans processes and, on Docker-wired workers, uses that
  answer; no test evidence on a real Docker host shows a container is actually gone.
- Operator procedure: `docs/run-recovery.md`, "When a run is stopped by a stale or fenced
  claim" and "Retained sources with no open hold (worker-local inspect and discard)".
