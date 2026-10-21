# ADR-2213: Worker-wide residue quarantine

**Status**: Accepted
**Date**: 2026-10-06
**Issue**: [#2213](https://github.com/vtmocanu/uzi/issues/2213)

## Context

[ADR-1783](1783-run-quiescence-and-attempt-clone-paths.md) proves a run's clone
quiescent before a credentialed sink or a destructive cleanup, and the HOME reap
of #1828 extends that to processes carrying the run's HOME. Neither covers a
survivor that nothing ties to the run: a runner-uid process whose environment and
working directory cannot be read (non-dumpable) and that no live attempt owns.

On a single-uid worker the model process, its descendants and the worker share one
uid. Such a process can read `/proc/<pid>/environ` of every child the worker starts
afterwards: the forge token that `gitEnv` puts in a git child's environment, and a
provider credential in a Claude or Codex child. Nothing on that worker can kill or
contain it. Under the uid split the solitary-kill path and the runner-uid boundary
already handle it.

## Decision

The worker latches a **quarantine**. While it is held, the worker starts no new
forge-credentialed git child and no new provider turn, and claims nothing.

### The latch

- It is in memory, process-wide, and has no production release. A container restart
  is the only release.
- **Invariant**: nothing restarts the node process in place. The container runs tini
  as PID 1, then npm, tsx and node, so a node exit ends the container. An
  in-container supervisor that restarted node would clear the latch while the
  unreadable process (a sibling, not a child of node) survived, so adding one
  breaks this decision and must revisit it.
- It is set only on a single-uid Linux worker. Under the uid split the process is
  contained by the uid boundary, and off Linux there is no process table to trust.
  The latch never fires there rather than faking a check.

### Detection and its cause

A worker-wide, credential-free scan (a process-table read with nothing in scope and
nothing signalled) runs on every claim before the clone fetch, for both harnesses
and before the review runner's fetch, and inside the quiescence proofs that scan
processes. A Codex run's own-mode proof normally neither scans nor latches;
`quiesceRun` enables process scanning for an own-mode Codex proof only through the
owner-cancellation-only `forceProcessScan` exception.

The latch cause is narrowed to the verdict reason `unreadable_unattributed`, and
only that reason (an approver decision). A `kill_unconfirmed`, a HOME reap that left
a process, an `unreadable status` entry or a helper failure keeps refusing the work
that needed the proof, exactly as before, but does not latch. Only the
`unreadable_unattributed` finding names a process that may read credentials and
that nothing ties to a run; the others are narrower or inconclusive states.

### What is guaranteed

No new forge-credentialed git child and no new provider turn starts after the latch
is held. Each is checked synchronously at the funnel that spawns or dispatches it,
with nothing awaited between the check and the start:

- git: `gitEnv`, `execScoped`, the tick spawner and the Codex effect-root checks
  refuse a child whose environment carries the forge Authorization header;
- providers: every Claude `queryFn` call site, `defaultQueryFn` and the detached
  spawn belt, `launchCodexRoot`, the Codex transport's login, thread and turn
  requests, and the epoch-start `releaseCodex` call;
- claims: the run and chat claim loops claim nothing while latched.

### Fail typed, do not park

A run that hits the latch fails `worker_residue_blocked`; it is not parked. The api
pins a parked run to a heartbeating worker for up to `WORKER_AFFINITY_CEILING`, so a
park would hold the run on the one worker that cannot serve it. The failed run keeps
its clone, generation hold, recovery pins and journal.

### Nothing is uploaded or released while latched

The recovery upload, release and settle paths are gated at their choke points, and
the terminal clone retire keeps the clone. Two releases are exempt, both because they
involve no source that could be lost and refusing them would strand custody:

- a **completed** run's custody release (`completedRun`), an in-process api call
  that the unlatched flow sends at completion; and
- the pre-clone park's hold release, where no source exists.

A completed run's clone is therefore kept. Attributed sources may use the existing
guarded operator discard; quarantine still refuses that operation while latched.
No automatic retention sweep removes it (see #1858 below).

### Why the ordinary settle is not reused

The ordinary recovery settle starts runner-clone git, which can run configuration
planted in the clone, deletes recovery pins and retires journal records, and the
api auto-releases an open hold as soon as a capture is `available`
(`ListReleasableCustodyHolds`). Reusing it would let a latched worker discard what
the quarantine exists to keep. So the ordinary path stays skipped.

### The additive archival capture

A run refused by the latch (`ResidueQuarantinedError`) instead gets a local, credential-free copy of the committed
work already in the worker bare. It reads only the bare, runs only worker-uid git,
never touches the runner clone, makes no reserve, upload or release call, and never
deletes a pin or changes the journal or the hold:

1. resolve the head H from the bare's tracking ref and anchor it at the create-only
   ref `refs/uzi-archive/<runId>/g<gen>` (the leaf starts with `g`, so it cannot
   collide with the existing 40-hex leaves);
2. bundle H from the bare into a temporary file in a worker-owned 0700 directory,
   under the recovery size cap;
3. read the bundle once into memory (bounded by the size cap), requiring its sha256
   and size to equal the producer's, and verify those bytes: a header gate (v2, or v3
   with only `@object-format=sha1`; one head, equal to H; no prerequisites; pack
   trailer checked) plus a probe-tag proof, an in-memory tag object naming H appended
   to the pack and fed to `git index-pack --strict --check-self-contained-and-connected`
   in a fresh temporary bare repository, computed from the snapshot bytes alone (the
   `GIT_SHALLOW_FILE` and `GIT_GRAFT_FILE` overrides neutralise a planted shallow or
   graft file, and the remaining store state can only lower the verdict), with `list-heads` as a cross-check. The
   proof relies on an internal git option, pinned by tests. SHA-256 repositories get no
   archive (the head read already requires 40-hex);
4. re-read the archive ref (still H);
5. write exactly the verified bytes to a fresh exclusive temp and publish
   `<dataDir>/recovery-archive/<runId>/g<gen>.bundle` (0600) and its manifest by
   rename, all within a 120-second deadline that also stops publication; the reported
   sha256 is the digest of those same bytes.

H and the sha256 are appended to the run's `failure_reason`. They live in the api's
run row, which a same-uid survivor cannot reach, and detect a later tampering with
the local file. The outcome never changes the typed failure. The archive runs only for a
`ResidueQuarantinedError`, i.e. a run stopped because the latch refused its turn,
credentialed git or claim. A run stopped by one of its own blocking quiescence
checks (pre-clone check, finalize proof, canonical reseed, orphan reclaim, predecessor capture, or a
recovery capture after its bounded retries) fails with a plain residue-blocked error
(`RunResidueBlockedError`, or `CloneResidueBlockedError` for the canonical reseed),
usually naming the process (orphan reclaim and the terminal-disk recovery capture
report a fixed text), and gets no archive; detection is a side effect of every
process-scanning check, so which of the two a run hits depends on what stops it, not
on which run detected the process. Checks that do not stop the run when they block
include, for example, the milestone checkpoint, pause, limit or wall park, completion
hold, shutdown requeue, credential-switch capture, the pre-settle
reap and the terminal retire: a run that continues fails at a later step in one of
the two ways; a park or requeue stands and the run resumes on another worker, or here
after the restart; at the terminal retire the run has already reported. Where the capture
cannot complete (no committed work in the bare, verification failure, over the size
cap, deadline) nothing is appended.

## Alternatives rejected

- **Park instead of fail**: see affinity above.
- **Kill the process**: on a single-uid worker nothing can kill or contain it
  (see Context); the solitary-kill path applies only under the uid split.
- **Latch on any non-quiescent verdict**: widens the denial of service to states
  that do not imply an unreadable process.
- **Off-worker archive-only upload**: deferred. It needs an api path that stores a
  capture without the release and auto-release semantics of an ordinary one.

## Visibility

The heartbeat carries `residue_quarantine` while latched, gated by the
`worker_residue_quarantine` protocol feature so an older api sees an unchanged wire.
The api keeps it in an in-memory tracker (cleared by a heartbeat without the member)
and overlays it as `residue_quarantined_at` and `residue_quarantine_cause` on worker
DTOs, a `(quarantined)` suffix in `uzi worker list` and `uzi admin workers`, the TUI
worker view, the web `quarantined` badge (Workers settings page and admin worker lists, cause in its tooltip), and the admin Health warning `fleet.quarantine`.
Operator steps are in [Quarantined worker](../docs/worker-setup.md#quarantined-worker).

## Consequences and residuals

#1659 owns the broader process boundary. This decision guarantees only that no new
credentialed git child and no new provider turn starts once the latch is held.

- **The in-flight turn.** A provider turn running at latch time is not killed. It
  continues, including its tool activity, fetch-tool requests, Claude CLI API calls
  and a Codex token refresh, until its boundary, where the run stops.
- **Point-in-time detection.** A process that becomes non-dumpable after a clean scan
  is seen only at the next scan (the next claim or quiescence proof). Children
  started before then are exposed.
- **`attributedToOtherLive`.** A non-dumpable process linked to another live
  attempt's recorded root is not reported while that attempt lives. This is an
  accepted residual, pinned by a test and owned by #1659.
- **Commits not yet in the bare.** The archive covers only the head already in the
  bare's tracking ref. Newer commits and uncommitted edits exist only in the kept
  clone and are not read, because runner-clone git could run planted configuration.
- **No revocation.** Credentials exposed before detection are not revoked.
- **Forced latches.** Any run can force a latch: a fail-closed denial of service on a
  shared worker, bounded by a restart and attributed in Health.
- **Completed-run clone.** Kept while latched and after, until an existing guarded
  operator discard is authorized; restarting alone does not authorize deletion.
- **Skipped terminal quiesce.** Any run whose failure path already set
  `preserveRecoveryClone` (latched when it fails, or a latch caught before its settle)
  skips the `terminal_retire` quiesce, as residue-blocked runs did before.
- **Ungated Codex credential paths.** Only the epoch-start `releaseCodex` call is
  gated. Not gated: the run-lane per-sink boundary reconcile (subscription
  `refreshCodex`, api_key `releaseCodex`), the app-server refresh bridge
  (`bridge.refresh` to `refreshCodex`) and the advice harness's initial
  `bridge.release`. While latched these can still fetch a provider token into worker
  memory or deliver one to an app-server already running, though no new turn starts
  (launch and login are gated).
- **Visibility gap.** After an api restart a latched worker shows as not quarantined
  until its next heartbeat.
- **Stale badge.** A quarantined badge can show on a worker whose heartbeat went
  stale while `fleet.quarantine` (fresh heartbeats only) reports none, for roughly
  the tracker's 10-minute TTL plus up to one sweep interval after the last heartbeat
  (the prune runs per sweep).

## Amendment 2026-10-14 — #1859: residue retention veto

**Historical implementation record — superseded by #1858 below.** The entire
quoted description records #1859's sweep and veto, not current deletion policy.

> Current canonical reseeding and attempt seeding share per-key residue retention
> in `agent/src/git.ts` (`sweepResidue`). Canonical retention runs under the bare
> lock after successful canonical free and before the new seed; attempt retention
> runs after seed/live registration. Abandoned-attempt and orphaned-skills phases
> remain exclusive to the attempt path. The original ADR-1783 Docker-only sweep
> gap is historical; [its historical-coverage amendment](1783-run-quiescence-and-attempt-clone-paths.md#amendment-2026-10-14--1859-shared-per-key-residue-retention)
> records the shared helper and the inherited Docker Follow-up 3 risk.
>
> Residue deletion requires fresh readable journal evidence (protecting a named
> candidate or descendant), a non-live path and an affirmative scoped seed-process
> verdict. Quarantine is checked before and after proof and synchronously at the
> deletion boundary after awaited validation. An active or newly latched
> `unreadable_unattributed` worker quarantine keeps all residue entries.
> Fresh-attempt seeding's unreadable-process relaxation never authorizes deletion.
> False, absent or unverified process proof and generic thrown proof keep entries
> with bounded diagnostics; classified control/recovery interruptions propagate.
> Off-Linux absence of a process verdict keeps entries. The latch's cause,
> lifetime, credential protections and custody semantics are unchanged.
>
> The best-effort target is five exact parser-valid
> `.uzi-residue-<key>.residue-<uuid>` direct siblings of the same key. Parent
> validation and complete enumeration within a private fixed 4096-direct-entry
> enumeration budget precede reclamation; overflow skips all residue reclamation.
> No excess returns without redundant journal discovery. Selection uses the
> original oldest excess slice by lstat mtime and basename tie-break; refusal
> keeps that entry without deleting a newer substitute. Protected or unproven
> entries can leave more than five, so this is no hard disk bound.
>
> Each sweep attempts at most 16 residue candidates from that oldest excess slice.
> A bounded 128-key LRU cursor rotates past refusals and revisits them on later
> sweeps; it caches no journal, live, process or quarantine verdict. No excess
> clears the cursor, and restart/eviction restarts from the oldest. The newest five
> are never compensating candidates. The attempt cap bounds work, not wall time;
> existing operation/deletion settlement and safety deadlines remain unchanged.
>
> Canonical retention forwards existing configured best-effort Docker handling;
> unwired workers gain no daemon obligation. Attempt retention remains
> `processOnly: true`, with no new container verdict. Existing uid/path/name
> checks and deletion deadlines remain; a possibly alive rm stops further cleanup,
> and file/symlink residue is unlinked as an entry without following its target.
> There is no new background timer or custody change. The opt-in Linux
> `task test:clone-quiescence` image build timed out before actual cases;
> actual Linux uid verification from that leg remains uncompleted.

## Amendment 2026-10-18 — #1858: automatic retention reclaim disabled

Refs #1858 is retention-only, non-closing delivery. Canonical and attempt seeding
now call read-only `diagnoseRetainedArtifacts`, retaining shared own-key residue,
disposable-kind attempts (abandoned and crash-window live orphans), and orphaned
skills on every worker. The former five-residue target, sixteen-candidate deletion
budget and rotating deletion cursor are historical; 3 attempts / 5 residue / any
orphan are diagnostic thresholds only. No safe automatic reclaim exists.

The latch's cause, lifetime, credential and claim guards, archival capture, custody,
normal cleanup and guarded operator discard contracts are unchanged. Retention
diagnostics do not prove quiescence or delete, rename or append retirement state.
No process scan, daemon listing (even repeated clean listings), time, age, count or
disk pressure authorizes retention deletion. Currently unwired status is not
durable proof of no prior daemon exposure.

Diagnostics observe lower-bound counts/completeness within 4096 direct siblings
of a validated parent, without a recursive byte walk. Overflow/read failures yield
partial/unknown evidence; existing protection predicates fail closed and
protected-or-unreadable custody makes classification partial without a second
probe. Known excess (>3 attempts, >5 residue, >0 orphaned skills) can warn
`runner clone retention: automatic reclaim disabled` with a sanitized, truncated
key. The one-hour rate limit uses full canonical keys in a 128-key LRU; restart or
eviction may warn again. These bounds cover diagnostics, not retained bytes,
identity metadata or safety inventory. Compaction retains present-path identities;
existing per-seed process/teardown scope over non-live paths and compaction work
grow with inventory, without caps skipping safety checks.

Unchanged `beforeFree` / `freeCanonicalClonePath` and quarantine retain the
process-proof / daemon pending-create residual. #1860 separately owns daemon
mediation; #2852's terminal/Docker writer-exclusion remainder is not fixed.
Attributed attempts and paired skills may use existing guarded
[retained-source discard](../docs/run-recovery.md#retained-sources-with-no-open-hold-worker-local-inspect-and-discard);
shared residue and orphaned skills have no supported generic reclaim route.
API custody discard alone is not local writer-exclusion proof. No proxy, marker,
ledger, API, schema, configuration flag or new infrastructure is introduced.

## Amendment 2026-10-21 — #3181: the retained credential-switch settlement site

A retained-recovery run's owner token switch now runs an own-mode quiescence proof with
site `retained_credential_switch` before the worker reports the switch (see
[ADR-1247](1247-per-run-token-selection.md#amendment-2026-10-21--3181-a-retained-switch-settles-then-releases-or-holds-for-the-owner)).
Like the predecessor proof it is a process-scanning check, so a verdict listing an
`unreadable_unattributed` process can latch the quarantine through the same detection,
with the site recorded as the latch's site. The latch cause, lifetime and guards are
unchanged. A proof that blocks without latching does not stop the run: it holds the run
for the owner (`credential_switch_settling`, then `credential_switch_unsettled`). The
hold is not an exoneration, because the next claim still runs the fail-closed predecessor
proof.
