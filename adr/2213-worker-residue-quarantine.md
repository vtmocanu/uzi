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
processes. A Codex run's own-mode proof does not (`quiesceRun` sets `processes:
(!executor.safety || mode !== "own")`), so it neither scans nor latches.

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

A completed run's clone is therefore kept until cleaned up by hand. No sweep removes
it.

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
hold, shutdown requeue, credential-switch capture, attempt retention, the pre-settle
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
- **Completed-run clone.** Kept forever while latched and after, until cleaned by
  hand.
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
