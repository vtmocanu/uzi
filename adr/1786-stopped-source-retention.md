# ADR-1786: A stopped generation's source is kept until explicit operator disposal

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

A stopped generation's exact local source is kept until an operator explicitly disposes of it. For
stopped-source exits the coverage check is diagnostic only and never authorizes release. Park
release paths also keep the exact clone and existing journal even when covered. Terminal exits
retain the existing snapshot-based covered release, subject to the remaining writer-exclusion
limitation tracked in [#2852](https://github.com/vtmocanu/uzi/issues/2852) (AI-synced 2026-10-18).

**Slice A contract, Refs #2852 (AI-synced 2026-10-18):** A holds
credential-free GitCache capture and its supervised children, including retained-source
capture, under closed admission and an epoch fence. This preservation boundary grants
no disposal authority: stopped and parked sources remain retained under this ADR.
The held scope ends after `recordRecoveryCapture`; fresh successor creation from the
durable bare pin is outside capture and reads only predecessor lstat metadata, not
old clone bytes. Ordinary seeding trust is unchanged. Refusing every credential-free
capture would strand capturable work in the ephemeral clone rather than the durable
worker bare; successful local capture still proves neither archive availability nor
permission to delete the predecessor.
Existing terminal executor disposal does not close the gap between snapshot coverage
and writer-exclusion proof; strengthened terminal proof (C) and healthy credentialed pre-publish closure (B)
remain unshipped. Docker writer exclusion, including accepted-but-invisible daemon
creates (#1860), and per-attempt cgroups (#1862) are not supplied by A. #1858 grants
no automatic reclaim, and #2746 custody SETTLE remains separate from physical CLEANUP.

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

### Coverage: a diagnostic on stopped and park exits, the release gate on terminal exits

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

The verdict is a snapshot, and its final process scan cannot see a writer that writes and
exits between the status read and the scan; the cleanup's retire (`discard: true`) would then
delete that writer's bytes. Park exits therefore keep the source regardless of this verdict;
the remaining snapshot-based terminal release is an open limitation under #2852:

- **Stopped-source exits** (no handler: the generic `stale_claim`, `claim_fence`,
  `running_ack_terminal` arms and a `terminal_disk_precheck` where the generation moved; or a
  handler exit with `record: "stopped"`: a stop confirm, another generation, a stale park
  report) keep the clone even when `retainStoppedSource` finds it `covered`. The verdict is
  diagnostic only: it is logged ("stopped source: coverage verified (diagnostic only); the
  clone is kept until explicit operator discard") and the retention log lines carry
  `coverage: covered|not_covered|skipped`. The preserve latch stays and the retention record
  is written as for any other stopped source; the clone waits for explicit operator discard.
- **Park release paths** (recovery, provider outage, credential-switch, disk and live
  non-terminal final parks) keep the exact clone and existing journal even when covered.
  Coverage is diagnostic when checked; shutdown still skips the check. No stopped-source
  marker or `reclaimed` entry is added by the park.
- **Terminal exits** (`completed`, `failed`, `cancelled`, with `record: "none"`) retain the
  existing covered release at the handler's exit. The late-writer snapshot residual remains
  open under #2852; retaining parks is a partial fix, not closure of that issue.

Status reads on a kept clone run with `GIT_OPTIONAL_LOCKS=0`, so they never rewrite its
index stat cache.

Anything else (unknown, changed, unanchored, unreadable, blocked) is `not_covered` and the
source is retained. The accepted residuals are stated in `docs/run-recovery.md`: gitignored
files are not covered by any capture; commits reachable only from the reflog (for example
after `git reset --hard`) are not counted as source; and a bare ref that justified coverage
can later move (a forge force-push, pin settlement). Refs that exist only because the seed
created them are not treated as worker-only work.

### Operator channel

Retention needs an explicit reclaim path. A canonical clone kept with no marker is reclaimed only on
positive server evidence that its run is terminal; an absent or released hold is never that evidence
(maintainer decision 3, 2026-10-12).

- **Transport.** A Unix socket at `<dataDir>/ops/retained-source.sock` (0600, in a worker-owned
  0700 directory), served by the live Worker. No TCP listener, no API endpoint. Setup failure
  is logged and non-fatal. Requests are NDJSON, at most 8 KiB in and 64 KiB out, with one
  destructive request at a time (a concurrent one gets `busy`).
- **`inspect`** is read-only and content-free, under one 5 s budget; a budget hit reports
  `partial` or `unknown`, never 0. It lists attempt-ledger sources, canonical sources with a
  `uzi-stopped` marker (marker state `bound` while the plain journal is pending, `detached`
  normally, `mismatch` otherwise), and (maintainer decisions 2 and 3, 2026-10-12) canonical
  clones kept under their plain journal with no marker (marker state `none`, journal state
  `plain`). For a selected source inspect also reads the run's lifecycle through the worker's
  existing ownership probe and reports consumer `run_state:terminal:<status>`,
  `run_state:<status>` for a non-terminal status or `run_state:unknown`, the blocker
  `run_state_not_terminal` or `run_state_unknown`, and `lifecycleRead` (`terminal`,
  `not_terminal` or `unknown`); the aggregate view reads no lifecycle and reports
  `run_state:not_read` with no blocker. A marker-less record is hidden only by evidence
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
- **Actively parked, and the lifecycle evidence for a marker-less source.** An open hold of the
  run means the run is still parked, so `owner_hold_discard` refuses and deletes nothing: this
  is the actively-parked refusal. A local record (`resolve_local_record`) or a live flight also
  refuses. An absent or released hold is not proof the run is terminal: a hold can be released
  (claim released, capture available) or never opened (`paused` and `limit_wait` parks,
  recovery-disabled workers) while the run is still parked. Discard of a marker-less canonical
  source therefore requires positive lifecycle evidence (maintainer decision 3, 2026-10-12): the
  worker's existing ownership probe (`GET /api/worker/runs/{id}/ownership`, read inside the run
  reservation and before the bare lock, alongside the holds read, on the same timeout) must
  report `completed`, `failed` or `cancelled`. A 404, any error, a timeout or a body without a
  string `status` is `unknown` and refuses with `run_state_unknown`; any other status refuses
  with `run_state_not_terminal`. Both blockers are soft, so an open hold or a local record still
  reports its own, more specific disposition. No generation binds the read: a marker-less part
  carries none, and a terminal status is absorbing. This adds no new endpoint. Terminal status
  only gates explicit operator discard; it is still not preservation proof (see the proof rule).
- **Accepted boundary: the probe is worker-scoped.** The ownership read is
  `GetRunOwnedByWorker` (`WHERE id = @id AND worker_id = @worker_id`). Once the run's
  `worker_id` moves off this worker (a requeue after a released claim, or a claim elsewhere),
  the probe reads 404, so the leftover is refused through the channel indefinitely. It stays
  kept, its bytes retained. While no open hold of this worker on the run covers it, the channel
  refuses it: an open hold gives `owner_hold_discard` (the owner's server-side hold discard,
  read in `git.ts` `evaluateRetained`, where the unknown-lifecycle blockers are soft) even
  when the lifecycle read is unknown, and a cross-worker predecessor's hold can stay open
  (`specs/human.md`). Its only automatic reclaim is a later run of that branch's
  terminal-orphan handling on this worker, which moves the clone to `<dataDir>/runner-quarantine/` (retained, never
  deleted automatically; the retention sweep never touches `runner-quarantine/`) and frees
  the canonical path, while a protected source or an attempt clone is kept in place. A read that is not worker-scoped would close this,
  but needs a new API and is an open follow-up, not shipped (PRD #1786, Decision Log,
  maintainer decisions 3 and 4).
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
site. A site marks terminal release as eligible (`markReleaseEligible`), marks a park to
keep (`markParkKeep`, reason `recovery_park_keep`), or keeps the clone outright (`keepClone`).
The handler's `finally` runs `settleCloneRelease` once after the return-path work; this
orders the snapshot after that work but does not exclude an independent late writer.
It calls `retainStoppedSource` with the capture's `capturedHead`. A terminal exit can
release the clone when `verifyStoppedSourceCovered` returns `covered`; on a park or an exit
that stops the generation the verdict is diagnostic and the clone stays (see Coverage above).
A remembered capture result, a memo without a `capturedHead` (including the degraded `{ verified: false }` stub) and a
check that throws are all `not_covered`: the clone is kept.

The exit decides the representation of a kept source:

- **Stops this generation** (kept even when covered; a `stop` confirm, an ownership read showing another
  generation, the terminal-disk recheck or the pre-handler precheck showing another
  generation, a stale park report, plus the M1 stale and fenced arms): the stopped-source
  record, as in M1 (attempt ledger `reclaimed` and the journal cleared; canonical
  `uzi-stopped` marker).
- **Park exits** (`recovery_wait`, provider outage, credential-switch, disk or live
  non-terminal final park, shutdown): the existing ADR-1197 plain-journal posture, even
  when covered. The exact clone and existing journal stay, with no marker or `reclaimed`
  entry. A successful direct credential switch also preserves the clone flag.
- **Terminal exits** (a terminal status or a terminal-disk recheck showing one): covered
  release is unchanged; otherwise the clone and plain journal stay, and a later run's
  existing terminal-orphan handling applies.

On the same worker, the next claim follows `recoverRetainedClaim` to recapture the kept
source onto a fresh attempt path and model session. Failed or unknown recapture keeps the
source. Successful recapture and completion also leave the predecessor retained: neither
recapture nor a new claim is deletion authority. Existing verified-successor retirement
applies only within its exact-source boundary; the authenticated covering FINAL path retires
its exact successor and reanchors the journal to retained predecessors without deleting them. Otherwise disposal requires explicit operator discard. Stopped and fenced
sources retain their operator-only policy.

The cost is storage for the old parked source, including after resume and completion. On an
unwired worker this also replaces ordinary same-path/session continuity with a fresh attempt
and model session; Docker-wired resumes already use fresh attempts and sessions. Capacity
pressure is not deletion authority. No new rename, state, TTL, exclusion or disposal
mechanism is introduced.

Two exits keep their earlier shape. An owner-authorized cancel (a non-credential cancel
ACK) keeps its normal cleanup, since the owner chose to discard; only the
credential-deferred captured cancel is revalidated. The existing terminal release-eligible
`predecessorCapture` exception releases in place (ledger abandoned, journal cleared; no
filesystem operation on the clone), without coverage revalidation; its later retention-sweep
handling is unchanged. A predecessor-capture park instead latches keep and retains custody.

Park and terminal exits that keep the clone outright (`credentialHeld`, an unverified
credential terminal, `receipt_failed`, `ServerWallParkedError`, a confirm `gone` without
a capture) run no own-mode proof beyond the existing entry kill, as before; only exits
that become terminal release-eligible or mark a park keep run the `stopped_source` proof,
once, at the handler's cleanup (an existing retained-custody flight skips coverage).
An interruption propagating out of the handler (shutdown, pause, limit, a codex deferral,
any throw) skips that cleanup check; the preserve flag stays as the
interrupting arm set it (shutdown, pause and limit keep the clone; an owner cancel takes
its normal cancel cleanup; a claim fence or stale claim goes to the caller's stopped-source
arm; a codex deferral or any other throw keeps the clone as latched), and the caller handles the interruption.

Claims without a claim generation (pre-generation APIs) can never be proven covered, so
their park and terminal exits keep the clone, and a same-worker reclaim of such a run
stops silently in retained recovery. Current APIs always send a generation.

An aggregate inspect lists attempt clones through their ledger entry. A canonical clone kept
by a park or terminal exit under its plain journal is also listed (marker state `none`)
and can be discarded through the channel only when the server's
ownership probe reports the run `completed`, `failed` or `cancelled` and no hold, local record
or live flight uses it (see Operator channel); the same run's resume on that worker still recaptures it,
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
  (2026-10-12) on the grounds that it adds a network call to a worker-local operator path and
  is not authoritative for `paused` and `limit_wait` parks. Superseded by maintainer decision 3
  (2026-10-12), which adopts a server read for the marker-less canonical case, reading the run's
  lifecycle (not its park state) through the existing worker-scoped ownership probe. Only a
  terminal status is accepted, an unreadable answer refuses, and the open-hold refusal and
  digest confirmation stay. Decision 2's reasoning that an absent or released hold leaves such a
  source reclaimable by confirmation alone no longer holds.
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

- **Open follow-up (not shipped):** cross-worker reclaim. A marker-less kept canonical clone
  whose run moved off this worker reads 404 on the worker-scoped probe and stays refused
  through the operator channel unless an open hold of this worker on the run gives
  `owner_hold_discard`; its only automatic reclaim is a later run of that branch's
  terminal-orphan handling on this worker (it moves the clone to `<dataDir>/runner-quarantine/`, retained and never
  deleted automatically, and a protected source or an attempt clone is kept in place). Closing it needs a lifecycle read that is not worker-scoped
  (new API). No manual removal procedure is verified or documented as safe.
- Retention raises ephemeral-storage pressure; existing disk admission still sees the real
  bytes, and there is no TTL or automatic reclaim.
- Local retention is not a checkpoint, archive or server hold. Same-pod process restart keeps
  the clone; pod replacement loses an `emptyDir` clone while the ledger or marker survives
  (reported missing, never empty); PVC teardown loses both.
- A key whose canonical clone was kept stays in attempt mode on that worker permanently, so
  later claims of the issue start a fresh model session; the work carries over through the
  tracking ref and checkpoints. A dirty kept clone keeps the run's hold, so its route is
  `owner_hold_discard`.
- **Retention cost of keeping covered stopped clones.** Every stale or fenced stop keeps its
  clone until an operator discards it, even a clean one the bare already holds. Attempt-ledger
  `reclaimed` entries are permanent until discard, so disk use grows per stop. On a worker with
  no Docker host every stopped canonical clone, even one stopped while planning and clean at
  its seed base, persists the key's attempt mode, so later claims of that issue use fresh
  attempt paths and fresh model sessions. Stopped markers count toward the per-run cap of 64;
  past it the record is `unknown` and the clone stays under its plain journal.
- **Deferred (not shipped):** writer exclusion spanning the final byte checks through retirement
  disposal, which would let a covered stopped clone be released again. Park retention avoids
  snapshot-authorized park deletion, but the residual remains on completed, failed and
  cancelled terminal release (#2852), pending real writer exclusion and safe reclaim in the
  Docker lane. Persisting the coverage verdict into inspect
  output is also not done; today it is in the worker log only. Decision: PRD
  `prds/1786-preserve-worker-only-work.md`, Decision 6 (lander and reviewer).
- **Not shipped:** teardown of attributed containers is not proven by tests or hosted
  acceptance (tracked in #2747). The teardown code ships (`teardownDocker` force-removes
  containers mounting the clone and answers `docker_unconfirmed` only after two clean
  listings), and the coverage proof scans processes and, on Docker-wired workers, uses that
  answer; no test evidence on a real Docker host shows a container is actually gone.
- Operator procedure: `docs/run-recovery.md`, "When a run is stopped by a stale or fenced
  claim" and "Retained sources with no open hold (worker-local inspect and discard)".
