# ADR-1783: Confirmed run quiescence and attempt-unique clone paths

**Status**: Accepted and implemented (M1-M3)
**Date**: 2026-09-27
**Related**: GitHub issue vtmocanu/uzi#1783

## Context

On the docker lane a run-owned background `docker run -v` outlived its run's park.
The container recreated the clone path as `root:root`, which bricked later runs on
that worker with `EACCES`. `killAgentTree` SIGKILLs only the CLI process groups the
SDK executor itself recorded; it confirms nothing. Claude's CLI tool shell is spawned
detached (its own process group), and anything an agent backgrounds with `setsid`
escapes it the same way — this is inferred from the SDK's own copy of the spawn code
(`agent/src/sdk-spawn.ts`), not reproduced against the deployed CLI. A container is
owned by the Docker daemon, not by any process tree at all, so no process-level kill
ever reaches it. Any of these can keep writing inside a clone, or keep a bind-mounted
container alive, while the worker fetches the branch back, publishes a checkpoint, or
removes the tree.

## Decision

### M1 — confirmed quiescence before every destructive or credentialed clone operation

1. **Every process the agent starts under this run's clone carries a per-attempt
   marker**: `UZI_RUN_ATTEMPT` (`<runId>:<attemptId>`), `UZI_RUN_CLONE`, and
   `UZI_RUN_CLONE_KEY` (`agent/src/sdk-env.ts`, `agent/src/worker-spawn-mark.ts`).
   Separately, `UZI_WORKER_SPAWN` marks a runner-uid process the WORKER itself
   spawned with fixed argv (never a repo- or agent-authored spawn). The two marks
   are disjoint and serve opposite purposes: the attempt marker says "this process
   belongs to this run's clone," the worker mark says "this process is not the
   agent's, do not attribute it to the agent's activity." Only a worker-authored,
   fixed-argv spawn may ever carry `UZI_WORKER_SPAWN`; marking a repo-controlled or
   agent-invoked command would let that command claim worker provenance it does not
   have.
2. **A runner-uid process reaper** (`agent/src/run-quiescence.ts`) scans the process
   table, uid-filtered (only the runner uid), scoped by key/cwd (a process is in
   scope iff its cwd lies, by whole path components, within a target path, or it
   carries both a well-formed attempt marker and a `UZI_RUN_CLONE_KEY` equal to the
   target key — the key alone never scopes a process, or a forged key with no
   marker could wedge every seed of that key), excludes itself, and excludes
   worker-marked spawns. A process carrying this attempt's marker or a
   terminal attempt's marker is killed in every mode; another live attempt's
   process is a `live_attempt_conflict` survivor in every mode, never
   signalled; an unmarked in-scope process is killed only in `own` mode (the
   run's own teardown) and is a survivor in `seed`/`capture` mode, where
   nothing positively attributes it. It rescans until nothing
   is left to kill and nothing is unattributable, and reports one of three
   states: `quiescent`, `survivors`, or `unverified`. `unverified` (an
   unattributable process, a helper timeout, a malformed answer) and `survivors` both **fail closed**: the
   worker never treats "could not prove it" as "proved it."
3. **A Docker Engine API teardown** force-removes every container with a bind
   mount under the clone, together with its anonymous volumes (`v=1`, #2236;
   named volumes survive), then lists until two consecutive listings are clean. Its
   results are `not_wired` (no daemon configured), `docker_unconfirmed`, or
   `docker_error`. **It never reports `quiescent`**: a `create` the daemon already
   accepted can still complete after the last listing, so no Docker result may
   ever be read as proof of quiescence. Because it never reports `quiescent`,
   none of its three outcomes ever blocks anything — a Docker result is
   best-effort cleanup, logged either way, not a gate. The whole teardown is
   bounded by an overall budget (15 s by default): the remaining budget is
   checked before every list and delete request, each individual request is
   capped by whichever is smaller — its own timeout or the time left — with an
   absolute wall-clock deadline so a trickling or stalled response cannot
   outlive the budget, and once the budget is spent the teardown returns
   `docker_error` rather than starting another request.
4. **Gating by site**, all fail-closed on a non-quiescent verdict:
   - the limit, wall-clock and completion-hold parks leave the park standing, with
     no credentialed publish;
   - an owner pause that cannot prove quiescence reports `pause_failed` and the
     run keeps running, under the existing pause rule (PRD #1190 D8: no
     durable checkpoint, no park) rather than
     parking on unproven ground. If capture may have drained a Codex epoch before
     pause failure or withdrawal, continuation first recreates it from saved
     session state; a vault lock on recreation takes ADR-1766 credential deferral;
   - finalize fails the run with the typed `fail_origin` `worker_residue_blocked`;
   - terminal retire keeps the clone rather than removing it;
   - graceful shutdown, milestone checkpoint, and restore-point capture are all
     gated the same way.
5. Re-proofs run after any runner-clone git operation, since git itself can run
   repo-configured code (filters, textconv) that spawns processes.
6. Every worker-invoked runner-clone git call sets `GIT_NO_LAZY_FETCH=1`; the
   worker-marked subset additionally sets `GIT_ALLOW_PROTOCOL` to a value naming
   no real protocol, on every worker-marked git subcommand except `clone` (which
   needs a transport to run at all), so those calls refuse every other
   transport. Together these close a promisor/uploadpack exec path that could
   otherwise inherit the worker mark and run arbitrary repo-configured code with
   it.
7. uzi's own e2e scripts use `--mount type=bind` instead of the older `-v` bind
   syntax for every real mount; the Decision-3 sidecar-isolation fixture
   (`e2e/phases/49-docker-sidecar.sh`) deliberately keeps `-v` in its attack
   matrix as a negative probe, proving a sidecar container cannot read worker
   secrets through it.

### Amendment 2026-10-19 — #3123: done-checkpoint settlement

The reaped Codex done-checkpoint path with `executor.safety` starts its absolute
60 s soft / 90 s hard allowance before gate admission and carries the remaining
time through queue admission, reconciliation, quiescence and action. The existing
fresh advisory snapshot request has an independent allowance. Neither a soft
stop nor a forfeited queued callback proves quiescence or ownership. Owned work
must settle before a sibling sink proceeds, and the Runner still requires exact
positive producing-context, generation and committed-receipt proof.

Shutdown stops active and queued done attempts across runs and blocks fresh
snapshot admission. The actual shutdown sink retains
[#3101's typed credential exception](1766-codex-vault-lock-park.md#amendment-3101-actual-graceful-shutdown-durability)
and #3109's genuine installer-before-credential reconciliation within the original
absolute grace (15 s default). Local content tests cover promptly settled
predecessors and verify the latest receiver content within that grace. They are
not hosted/shared-worker acceptance. Awaited storage settlement is unbounded,
and unsafe cleanup may consume the grace and fail closed with source/custody
retained. This changes no process proof, clone retirement or deletion authority;
one private stage may remain per soft-stopped attempt without sweep or adoption
authority. See [ADR-1597's residual boundaries](1597-midturn-checkpoint-durability.md#amendment-2026-10-19--3123-completion-budgets-and-safe-stops).

### M2 — attempt-unique clone paths on Docker-wired workers

8. **Each execution attempt seeds a fresh path**,
   `<runnerRoot>/<repoDir>/<key>.attempt-<attemptId>`
   (`agent/src/attempt-path.ts`), and an exposed attempt path is never reused by a
   later attempt. **This is the isolation guarantee itself** — a later attempt
   never inherits an earlier attempt's residue, escaped process, or bind-mounted
   container, because it never touches that path at all. M1's process sweep and M3's
   quarantine are recovery for what is left behind, not what makes isolation hold.
9. **The `.attempt-` separator**, not `@` or another delimiter: `git
   check-ref-format` accepts `.attempt-` inside a ref, so the choice is not about
   what git's grammar forbids. It is chosen because it uses only
   `[A-Za-z0-9.-]` and carries no
   special meaning to git, npm, Go or nix build tooling the way `@` does (a
   version-pin or scope marker in several of those). The attempt id
   (`<UTC timestamp>-<claim generation>-<16 hex>`) is itself hyphen-separated, so
   the basename grammar still needs to stay unambiguous under a greedy-key
   regex — the parser is anchored and takes the LAST `.attempt-` followed by a
   complete, strictly-formatted id as the true separator, so key and id parse
   back losslessly even if a key happens to contain the literal string
   `.attempt-` followed by other text.
10. A bare-config ledger, `uzi-attempts.<branch>.entry`, records every attempt's
    state: `live`, `abandoned`, `retired`, or `reclaimed`. `reclaimed` (a foreign
    attempt released without capture) is never disposable — it is not eligible for
    the former retention sweep's delete. Since #1858, automatic retention
    deletion is disabled for disposable-kind attempts too.
11. The recovery journal carries the attempt id, so a journal entry and a ledger
    entry can be cross-checked for the same attempt.
12. Journal cases are classified by identity (C'/B'/A'). Historically, C' capture
    established predecessor-scoped quiescence before in-place path release — the
    predecessor was never removed out from under a proof that had not run.
    Current stopped/parked predecessor policy keeps the exact source after
    successful recapture; recapture grants no deletion authority
    ([ADR-1786](1786-stopped-source-retention.md)) (AI-synced 2026-10-18).
13. **Automatic retention reclaim is disabled on every worker (Refs #1858).**
    Disposable-kind attempts (abandoned and crash-window live orphans), shared
    own-key residue from canonical and attempt entries, and orphaned skills are
    retained. The former 3-attempt / 5-residue caps and any-orphan cleanup trigger
    are diagnostic thresholds only; they grant no deletion authority. The original
    Docker-only sweep and #1859 shared residue sweep are superseded history below.
14. The ledger is compacted per seed, but identities for present paths remain.
    Compaction is not a bound on identity metadata or retained bytes. As inventory
    grows, existing per-seed process/teardown scope over non-live paths and ledger
    compaction work also grow; no cap skips safety checks.

### Option A — the accepted resume trade-off

15. **On a Docker-wired worker, a resume or re-claim always starts a NEW model
    session** (a fresh Claude SDK session or a fresh Codex thread), because a
    session is keyed by HOME *and* cwd, and every attempt runs at a fresh cwd by
    construction (decision 8). The earlier session's transcript can never resolve
    from the new attempt path. Continuity of WORK, not of conversation, is what
    survives: the tracking ref, the recovery journal, and captured checkpoints
    carry the prior attempt's committed state forward; only the model's own
    memory of the conversation is lost. Ordinary unwired resumes (no Docker
    daemon configured), for keys that never entered retained-source recovery,
    keep same-path continuity. Retained-source recovery uses a fresh attempt
    path and model session even on unwired workers (see the #2512 amendment).
    The reviewers chose this trade-off under the user's delegation: it is a real, disclosed cost
    of the isolation guarantee, not an oversight.

### Option B — the deferred durable follow-up

16. The reviewers named, but did **not** build, a per-attempt Docker API mediation
    proxy that would prove zero container exposure across an attempt boundary and
    so restore safe in-place resume without losing session continuity. This is
    future work, not shipped behavior; do not read decision 15 as permanent.

### M3 — residue quarantine at the canonical clone path

17. **The canonical reseed proves quiescence first**, then deletes as the runner
    uid. Whatever remains after a failed or timed-out-and-settled runner-uid
    delete (the delete reports only an exit code under the uid split, not the
    errno that left something behind) is renamed by the WORKER, in-process
    (`fs.rename` in `quarantineCanonical`), within the SAME parent directory, to
    `.uzi-residue-<key>.residue-<uuid>`, and kept there — it is never deleted by
    this path. The same-parent constraint is not stylistic: `<runnerRoot>/<repoDir>`
    is worker-owned `2775` and not sticky, so the worker may rename any entry
    inside it, including a root-owned one, to another name in that SAME
    directory. Moving a root-owned directory to a DIFFERENT parent instead needs
    write permission on the moved directory itself, for the rename to rewrite
    its `..` entry — permission a root-owned directory denies
    the worker; staying in the same parent needs write permission only on the
    parent, which the worker already has.
18. A symlink or non-directory planted at the canonical path is quarantined
    without ever being followed or opened — a planted link cannot be used to
    redirect the reseed's proof or its delete elsewhere.
19. A run that cannot free the canonical path fails with the typed `fail_origin`
    `worker_residue_blocked`, newly introduced by this work.
20. **Two quarantine conventions coexist, and they are not interchangeable**:
    `<dataDir>/runner-quarantine` holds retired attempts the journal already knows
    about, while `.uzi-residue-*` beside the clones holds UNJOURNALED residue —
    something the reaper could not account for by any recorded identity. Treat a
    `.uzi-residue-*` entry as evidence of an escape this ADR's guarantees did not
    fully close, not as routine bookkeeping.
21. A required reseed has no fail-open filesystem fallback: if the options that
    would let the worker recover are unavailable, seeding fails rather than
    silently accepting a compromised path.
22. The opt-in root fixture `task test:clone-quiescence`
    (`e2e/clone-quiescence/`) exercises the reaper inside the real worker image,
    through its root-start entrypoint and the production uid split, because a
    host or single-uid run cannot reproduce the split and a run wholly as root
    passes vacuously.
23. **An unsettled runner-uid delete blocks the path it was walking, not just the
    caller that started it** (`unsettledDeletes`, `assertNoUnsettledDelete`,
    `runRunnerUidDelete` in `agent/src/git.ts`). A group kill sent to a timed-out
    `rm` still waits for the child's `'close'` event before the delete settles —
    a failed kill (e.g. `ESRCH` racing an already-exiting process) does not
    shortcut that wait. When `'close'` has not arrived within the bounded wait
    after the kill, the delete is left `unsettled` for that resolved path: every
    LATER free or reseed attempt against the same path on this worker blocks
    immediately (`CloneResidueBlockedError`) until `'close'` finally arrives or
    the worker restarts, because an `rm` that may still be walking the tree must
    never be handed a fresh clone or a quarantine rename underneath it. From the
    operator's side this shows up as the same key repeatedly failing with
    `worker_residue_blocked`, not as one isolated failure. The former retention sweep
    also honoured this record; #1858 disables that sweep's deletions entirely.
    The worker log records the path entering (`runner-uid delete recorded as
    unsettled`) and leaving (`left the unsettled record`) the record.

## Disclosed scope reductions

- A container that binds nothing under a clone path is not covered by the Docker
  teardown; it is out of scope by construction, not missed.
- Deliberate env-marker forgery or cwd-based evasion is explicitly out of scope.
  This work addresses the ACCIDENTAL escape path (a detached shell, a stray
  background job); it is not a defense against an agent deliberately trying to
  survive its own reaper.
- A Claude/stub pause that reaches its Linux proof boundary reaps attributed
  background processes. Codex owner-pause capture uses A's held supervisor capture
  (contract below). Clone-bound Docker teardown remains best-effort cleanup,
  not writer-exclusion proof.
- **M3 canonical-path quarantine (`freeCanonicalClonePath`/`quarantineCanonical`
  in `agent/src/git.ts`) runs on EVERY worker, wired or not**: `attemptSeedOptions`
  returns `undefined` when the worker has no Docker endpoint, so
  `runnerCloneForBranch` takes the canonical (non-attempt) path, and that path
  always reseeds through the M3 quarantine, not just on Docker-wired workers.
  **Historical retention scope, superseded:** the original
  `sweepRetainedArtifacts` ran only from Docker-wired attempt seeding, leaving
  unwired residue unswept. #1859 then shared own-key residue reclamation between
  canonical and attempt seeding. #1858 stops automatic retention deletion from
  both entries on every worker, including attempts and orphaned skills. Unbounded
  accumulation is now the explicit retention policy, not an unwired-only gap.
- The ledger rewrite (decision 10/14) needs hard links on the data volume; a
  volume that does not support them is a deployment constraint on this design,
  not a design gap. The compaction that removes obsolete ledger entries
  (`compactAttemptLedger`) is itself best-effort: it catches its own failure,
  logs "attempt ledger compaction failed; the ledger is left as it was", and
  leaves the ledger uncompacted rather than failing the run. A volume without
  hard link support therefore degrades to unbounded ledger growth over time,
  not a hard failure.
- Legacy journaled root-owned canonical retire (residue that predates M3) is a
  follow-up, not retrofitted by this work.

## Invariants future code must respect

- **Where a seed or capture sweep uses the process reaper, its `quiescent`
  result is the process proof; a non-quiescent result blocks that operation.**
  Codex's held supervisor capture is a separate proof path, described below.
  `teardownDocker` never returns `quiescent`, ever — its three
  states are `not_wired`, `docker_unconfirmed`, and `docker_error` — so there
  is no Docker result to pair a process proof with. Every Docker outcome is
  treated as best-effort cleanup, never as proof: `docker_unconfirmed` is
  logged and does not block, and `docker_error` is logged and does not block
  either. Fail-closed is the process half's rule alone — a surviving or
  unattributable process (`survivors` or `unverified`) blocks the sink at every
  site that gates on quiescence; a non-quiescent Docker result blocks nothing.
  The reaper's scan itself is skipped, not just relaxed, on a Codex run's own
  `mode: "own"` proof (`runner.ts`'s `processes: (!executor.safety || mode !==
  "own") && process.platform === "linux"`); a `seed`/`capture` sweep runs the
  scan regardless of executor (those paths have no supervisor behind them),
  and so does a Claude/stub run's own-mode proof.

  Own-mode Codex sinks routed end to end through `reapForSink` or
  `withCodexBoundaryOnly` retain their credentialed supervisor boundary: limit
  park, actual graceful-shutdown durability, milestone checkpoint, pre-settle
  reap and finalize.

  **Slice A contract, Refs #2852 (AI-synced 2026-10-18):**
  A separate credential-free GitCache capture callback covers owner pause and
  credential-free recovery, memory-pressure and retained-source captures. It
  holds safety serialization, the epoch fence and closed admission through source
  reads, WIP commit, fetch-back, verification, authorized publication and full
  supervised-child settlement. A successfully settled, intentionally poisoned
  registry admits this narrow capture lane with no provider turn, normal callback,
  admission reopen, credential reconciliation, refresh or release, PAT,
  credential-bearing environment or overlay. Incomplete settlement grants no
  capture authority. #3132's clean cooperative normal checkpoint stops do not
  replace this intentional poison. Normal boundary entry refuses poisoned or
  disposed registries before queue side effects and checks again after the queue.
  A previously queued checkpoint's expiry does not add poison to either state:
  active capture retains its revision and full supervised-child settlement, and
  exact completed evidence survives disposal. Healthy queue expiry still fails
  closed; no deadline is extended. (AI-synced 2026-10-18)
  The capture context mints neither a credentialed nor a
  completion permit; absence of that permit does not mean absence of a held
  capture boundary. [ADR-1106](1106-codex-harness.md#process-root-ownership)
  records the canonical empty helper/askpass launch policy and separate
  credential-presence check for the trusted worker identity `worker_pat`.
  Codex JS dependency installation and Devbox install/shellenv retain their normal
  identity and command/environment contract under registered setup supervisor roots.
  Their fulfilled result grants no process proof without full clean root settlement;
  the private setup binding does not widen the Git-only capture lane or model/credential
  admission. The isolated fixed read-only PATH probe exception is recorded in ADR-1106.
  Executor-owned intent/plan factory, auth, consume, root cleanup and POST remain
  joined past delivery abandonment. Independent registry cleanup faults refuse capture,
  healthy credentials and recreation while preserving primary errors separately;
  other advice factories are unchanged. Authority expires at its original absolute
  deadline, but owned cleanup stays joined under the fence without a new budget.

  GitCache's capture request issuer is module-private and applies its full canonical
  policy. A recipient holding only the capture context cannot choose a validator or
  obtain a generic spawner. Shared pure policy validation cannot register requests;
  the complete policy is checked synchronously again at actual launch. Private
  synthesis bindings, including exact invocation and dynamic environment ownership,
  are described in [ADR-1106](1106-codex-harness.md#process-root-ownership).

  Pre-start predecessor binding uses one fresh, unregistered `newRunAttempt` per
  `runFreeCapture` acquisition, with the current run/generation, exact source path
  and empty recorded roots. It never reuses the source `attemptId`. During the
  owned callback, Runner sets/restores `activeCaptureAttempt`; `quiesceRun` reads
  `activeCaptureAttempt ?? flight.attempt`. The same authenticated object supplies
  the inside clone proof and post-runner-Git reproof. In `recoverRetainedClaim`,
  the initial predecessor quiescence proof also acquires `runFreeCapture`, then
  releases it before #3059's `normalizeRetainedSourceObjects`, which retains its
  independent existing supervision outside A's Git-only scope. `ensureClone`
  remains outside free capture. A later acquisition covers resumed-tip verification,
  direct publication or capture through `recordRecoveryCapture`; this does not
  expand the capture allowlist. (AI-synced 2026-10-18)
  Mode `capture`, exact source targets,
  live-owner conflicts and clone proof/reproof remain required. The executor
  receives only its immutable marker/key/path tuple through the existing optional
  fifth capture argument; its acquisition lifetime is specified in
  [ADR-1106](1106-codex-harness.md#process-root-ownership).
  This fresh process marker is not a recovery-record capture ID (C5b). It changes
  no source identity, normal `flight.attempt`/`attemptId`/`runnerClone`, live registry,
  journal, ledger, successor or deletion authority. C5 remains executing-flight
  `executionTails` ownership; flightless maintenance is unchanged.
  Refs #2852 (AI-synced 2026-10-18).

  **H/C amendment, Refs #2852 (AI-synced 2026-10-18):** ordinary scoped
  helpers retain legacy JSON without `reportDeadline` and unbounded kill-mode
  procfs reads. Genuine `reportBudget` includes the floored deadline and selects
  no-kill report observation, with no new read bounds. Parent operations share
  the minimum absolute scope/report/helper/ambient deadline. Fixed temporary
  creation, existing Node/tsx helper invocation and exact temporary removal use
  registered supervisor roots. Safety refuses reaper entry with pending launches,
  callbacks or unreaped capture children, and ordinary child admission during the
  exclusive pipeline. It joins admitted startup/full completion before releasing
  that pipeline and the safety queue. Raw scoped fallback and mismatched
  attempt/clone-key/target bindings refuse; late/nonzero/overflow/cancel-cleanup
  failure is unverified. Outside executing-flight scope the legacy route remains,
  without new resource waits or budget renewal.

  Human decision C1 adds `HeldRunCommandCache.drained` as a narrow reader of the
  executor's existing `completedCaptures`, alongside epoch teardown. An intentionally
  poisoned registry counts as drained for cache settlement only when it is
  `disposed` and `acceptsCompletedCapture` accepts its exact recorded proof.
  Additional poison/revision, changed roots/launches/callbacks, other poison,
  the sticky unclean flag or open cache roots retain the cache. This prevents
  multi-GiB caches remaining after verified pause/gate capture and terminal
  settlement; it creates no proof, general clean status, admission or
  credential-release authority. Disk-pressure behavior is unchanged. Both free
  capture and reaper roots bypass `cacheForLaunch` / `track`; ordinary cached
  roots retain existing accounting, with no new decorated-environment exception.

  For pending Codex memory capture, the same `executionTails` promise is installed
  synchronously before factory work and owns the original preservation operation
  through capture callback completion and cleanup. The public observer can return
  after its configured bounded wait while that tail still reserves the run. Only after actual settlement may identity-checked deletion release it;
  an old owner cannot delete a queued successor's replacement tail. Same-run local
  duplicates and maintenance respect this ordinary execution reservation, with no
  `inventoryReservations` exemption. An indefinitely pending callback or cleanup
  can retain the reservation indefinitely; capacity cost and occurrence rates are
  unknown. The original deadline still expires command authority. ACK uncertainty
  grants neither recapture nor success; original receipt, custody and late-ACK
  successor-isolation rules remain unchanged. Flightless maintenance outside an
  executing flight is unchanged. Finalization classifies Codex using the same safety
  markers as free capture.

  Save credential-free session state before capture may drain the epoch. After
  failed or withdrawn pause, and before human or automatic changed-milestone gate
  revision continues the model, recreate a possibly drained provider through the
  existing epoch path. Check newer cancellation, pause and vault-lock state before
  credentials or model execution; recreation-time vault lock takes
  [ADR-1766's deferral](1766-codex-vault-lock-park.md#d3-settle-before-capture-and-no-completion-authority-while-locked).
  Keep the prior session generation until provider adoption. `settleForCapture`
  intentionally poisons even a healthy registry; N1's non-poisoning alternative is
  not adopted. An accepted owner pause reached through `OrdinaryOwnerPaused` or
  `requestParkRun`'s `retainPause` continuation acquires a fresh `runFreeCapture`
  scope for source HEAD, generation pin and guarded inventory after the earlier
  scope expired. Refused fresh proof performs no source operations and retains the
  exact parked clone, session, journal and custody; an uncertain ACK grants no
  recapture or new pause attempt. Refs #2852 (AI-synced 2026-10-18).
  The approval path still skips persistence from the old drained HOME. This does
  not make an open approval gate parkable or change retained recovery's fresh
  path/session policy. Retained-source A coverage ends after `recordRecoveryCapture`;
  fresh successor creation from the durable bare pin is outside capture. It reads
  predecessor lstat metadata, not old clone bytes; ordinary seeding trust is unchanged.

  The healthy credentialed wall park, completion hold and recovery/credential-switch
  captures still have the pre-publish status/WIP/fetch-back gap; wrapping publish
  alone does not close it. That is slice B, **unshipped** in A. Terminal paths
  already have executor disposal; the remaining gap is snapshot coverage before
  writer-exclusion proof, not a blanket absence of disposal. Strengthened terminal
  process proof is slice C, **unshipped**. `CodexExecutor` has no `killAgentTree`
  implementation, so the direct optional call is not Codex process proof.
  Neither the held capture nor a process scan proves Docker writer exclusion;
  accepted-but-invisible daemon creates (#1860) and Claude residuals remain.

  A accounts for **R1857-01 and the credential-free half of R1857-05**, using
  shared R1857-07/08 controls. It does not complete #2852 or claim B/C coverage.
  Per-attempt cgroups (#1862) remain unshipped, not an available proof. #1858
  still forbids automatic retention reclaim. Preservation authority is not
  deletion authority: #2746's custody SETTLE and physical CLEANUP stay separate.
  #3031 is independent, not a prerequisite; A changes no model-free coverage,
  producers, ledger, custody or settlement protocol. Whichever lands second
  rebases for coexistence. #2969's live-predecessor policy and #2884's
  plan-rejection producer/discard policy retain their ownership. A adds no runtime
  maintenance-root/HMAC read, write or scan, IDs, classifier or budget.
  A shutdown label alone grants no exception or capture
  authority; the existing typed graceful-shutdown authority remains separate
  from execution cancellation, as recorded under ADR-1766's #3101 amendment.

- Never mark `UZI_WORKER_SPAWN` on anything other than a worker-authored,
  fixed-argv spawn. Marking a repo- or agent-invoked command breaks the
  attribution the reaper depends on to distinguish worker infrastructure from
  agent activity.
- Never introduce a second attempt-path grammar or separator inside the agent:
  parse and mint paths only through `agent/src/attempt-path.ts`'s helpers
  (`cloneKeyOf`, `parseAttemptPath`, `formatResidueName`) and mint attempt ids
  only through `mintAttemptId` (`agent/src/run-quiescence.ts`) — they are the
  single source of truth for every in-process consumer, including the reaper and
  retention diagnostics. `uzi-watcher`'s recovery shell scripts
  (`.agents/skills/uzi-watcher/scripts/backup-runs.sh`) are outside that
  boundary: they cannot import TypeScript, so they parse the same
  `<stem>.attempt-<id>` grammar themselves (`ATTEMPT_RE`, basename matching) — a
  second, necessarily duplicated implementation. Keep it byte-for-byte in sync
  with `attempt-path.ts`'s grammar when either changes.
- Automatic retention deletion is disabled, including for `reclaimed` clones.
  For unchanged cleanup paths, never delete anything as the worker uid when the
  runner uid can do it instead.
- Never follow a symlink or non-directory found at a canonical clone path during
  reseed; quarantine it unopened.
- Keep the two quarantine directories semantically distinct: journaled retirees
  go to `runner-quarantine`, unjournaled residue goes to `.uzi-residue-*`.
  Merging them would erase the diagnostic signal that a `.uzi-residue-*` entry
  carries.
- Never treat a failed group-kill as a settled delete: wait for the child's
  `'close'` regardless of whether the kill itself succeeded, and never free or
  reseed a path an unsettled delete may still be walking (decision 23).

## Consequences

Process proof remains limited to the boundary that actually runs: the Linux
reaper where enabled, the existing credentialed Codex supervisor boundary, or
A's callback-held credential-free GitCache capture (contract above).
The held capture covers its action and supervised children; it does not close
B's healthy credentialed pre-publish gaps or strengthen C's terminal release
proof. Claude/stub process handling is unchanged. This says nothing about a Docker
container — the Docker teardown that runs alongside it is best-effort cleanup,
not proof, so a container that still binds the clone path can, in principle,
outlive the checks that gate on process quiescence alone. Attempt-unique clone
paths mean an escaped process or a mis-owned directory from one attempt can
never contaminate a later attempt's work, at the cost of same-cwd session
resume on Docker-wired workers (Option A). Unremovable residue is quarantined
rather than left to wedge the worker or silently deleted. The durable fix for
Option A's continuity loss (Option B, a per-attempt Docker API mediation proxy)
remains open work.

## Reconciliation with PRD #1809 (worker disk safety, #1826)

The branch merged origin/main at `1d4ba878113fc113a7d52778054e83a46b53e269`
(merge commit `d82789ad`), which carries PRD #1809's bounded run caches,
data-volume-full parks and reclaim. Where the two touch the same sink or seed
path:

- **Limit park.** PRD #1809 D8 moved the park's durability publish into a
  `parkSink` closure that `handleLimitReached` runs *before* the `limit_wait`
  report. This ADR's first proof (through `reapForSink` with `flight`) and its
  re-proof after the WIP marker (`quiesceOrBlockSink(flight, executor, "park",
  { processOnly: true })`) run inside that closure. A blocked proof publishes
  nothing, and the report then carries no `checkpoint_contains_latest`. The
  park itself still stands. Because the sink now also runs on a park the server
  refuses, both settle legs are gated on the sink's result
  (`!parkResidueBlocked`), not only on the pre-report reap.
- **Mid-run `data_volume_full` park.** PRD #1809 D4's park runs
  `executor.reapAttributedProcesses?.()` (`run-procs.ts`) and drops the run-home
  caches (Claude/stub runs only; a Codex run skips that early drop) before
  `captureRecoveryRestorePoint`. That reap is cleanup, never
  proof: the capture still opens with this ADR's `recovery_capture` quiescence
  proof. A blocked proof means no WIP marker, no fetch-back, no publish and no
  settle or release. The clone is kept (`preserveRecoveryClone`) and custody
  stays with the run (`agent/test/run-quiescence-disk-park.test.ts`). The cache
  drop touches only the run's home directory, never the clone, so it may run
  ahead of the proof.
- **Finalize.** PRD #1809 added `reapAttributedProcesses` to the finalize
  security reap after the implement loop. The finalize quiescence proof
  (`site: "finalize"`) still runs after it and still gates the push.
- **Worker spawn marks.** PRD #1809 added runner-uid helper spawns in
  `agent/src/rmtree.ts` (`runHelper`, the run-cache and run-home measure
  helpers, and `runAgentHelper`, which `run-procs.ts` uses). Each runs a fixed
  worker-authored `node -e` script with a PATH-only environment, so each now
  carries `workerSpawnEnv(...)`. Without the mark, the reaper would count an
  in-scope helper as an unmarked survivor in `seed`/`capture` mode and fail the
  run as `worker_residue_blocked` by mistake. `agent/test/runner-spawn-sites.test.ts`
  inventories them.
- **Migration and vocabulary.** The fail_origin widening was renumbered to
  `00263_run_fail_origin_worker_residue_blocked.sql`, above main's
  `00261_run_data_volume_full.sql` and `00262_worker_run_disk.sql`. Its Up adds
  `worker_residue_blocked` to 00261's eighteen values, making nineteen. Its Down
  restores exactly those eighteen. `worker_residue_blocked` is the only one of the
  three newest values (with main's `gate_presentation_refused` and `data_volume_full`) that
  a worker may report. All three are never judged, and none is human-landable.

## Worker-wide lockout by an unreadable same-uid process

Found by CI, not the uzi worker: the GitHub Actions runner that ran this
branch's own test suite had a same-uid process (a setgid `ssh-agent`) whose
`/proc/<pid>/environ` and `cwd` are unreadable to a non-root reader by
construction; the uzi worker itself did not have one. Before #1854, `scanOnce`
(decision 2 above) treated any runner-uid process whose env or cwd it could not
read as `unreadable_unattributed`, UNLESS ancestry (its own pid/pgid/sid, or its
ppid chain) POSITIVELY ties it — pid and recorded start time both matching —
to a recorded root of another live attempt or a worker-launched runner-uid
root, in which case it was skipped entirely rather than reported. An
unreadable process not positively attributed this way made the whole scan
`unverified`. Because `unverified` fails closed everywhere the scan runs, a
single such process anywhere on the worker — an
ssh-agent an unrelated tool started, not necessarily this run's own — is a
**worker-wide lockout** for as long as it lived, at every site the scan
covers. Canonical reseed and predecessor/orphan capture run `seed`/`capture`
mode, which scans regardless of executor. Claude and stub `own` mode sites
also scan. Codex `own` mode sites skip the process scan and use their own
supervisor boundary at credentialed sinks; the separate Codex gap remains as
described in the Invariants section. Under the revised decision below, only
concurrent-claim and single-uid cases can still lock out these scanning sites
solely because of an unattributed unreadable process. The fresh-attempt seed
exception remains because seeding a fresh path moves or frees nothing.

**Decision revised for #1854.** Under the uid split, an unreadable,
unattributed runner-uid process is killed in `own`, `seed` and `capture` mode
only when no other claim is executing and no other live attempt is recorded.
The worker snapshots its execution registry before the helper scan; the helper
rechecks the candidate's `/proc/<pid>/stat` start time immediately before
SIGKILL, then rescans to prove it is gone. A changed or unreadable start time
vetoes the signal. A pid recycled between that final read and the signal remains
a kernel race; this check narrows it but cannot eliminate it without pidfd.
An unconfirmed kill still fails closed. With another claim
or attempt in flight, or on a single-uid worker, the process stays
`unverified`. A recorded live root and its descendants remain exempt only
when the root's pid and start time both match. The scanner excludes its own
descendants; an unreadable descendant without a readable attempt marker cannot
be chosen as a kill target.

This permission relies on every runner-uid process under the split being
agent-controlled code of a current or finished claim, a recorded worker root
or descendant, a readable worker-marked fixed-argv spawn that runs no repo
code, or the scanner itself. Agent-controlled code includes dependency
installs, provisioning builds, self-improve checks, stub git and runner-clone
git that can run configured code. A future worker spawn that can become
non-dumpable must be recorded as a root. The permission kills a process and
requires its exit; it never clears one as unrelated merely because ancestry
is absent. The accepted admission race is that another claim can start after
the worker's snapshot and before the helper signals.

**Operator visibility.** The `unreadable_unattributed` entry's world-readable
`/proc/<pid>/stat` still names the process, so the failure reason and the
worker log name the pid and its (sanitized, quoted) comm, e.g.:

```
runner-uid pid 4242 "ssh-agent" could not be attributed (env/cwd unreadable)
```

and `describeProcesses` adds `attribution: "could not be attributed"` to that
process's log fields. This remains the operator action when the solitary
uid-split permission does not apply or the kill cannot be confirmed.

**A bounded wait, not an unbounded retry, on a blocked recovery capture.**
`handleRecoveryExhausted`'s capture retry used to retry a blocked quiescence
proof forever — this is what could hang a test run driving the same code
path. It is now bounded: after `RECOVERY_CAPTURE_BLOCKED_ATTEMPTS` (5)
CONSECUTIVE captures whose proof blocked, the run fails with the typed
`fail_origin` `worker_residue_blocked`, the clone and session kept for
inspection. That bound's wall time is typically about 30 s at the defaults
(5 reap deadlines of 5 s plus 4 backoffs; about 40 s on the vault-locked
park, whose backoff doubles) — not a strict ceiling: a `survivors` proof
with nothing left to kill returns at once, so the bound can be reached in as
little as ~4 s, while a proof that also runs the best-effort Docker teardown
(budget 15 s, on a Docker-wired worker) or the runner-uid helper (10 s
timeout) to its own budget can take longer than the roughly-30 s figure —
a `survivors` proof with an unconfirmed kill still runs the full 5 s reap
deadline like an `unverified` one, it just reports a count rather than a
pid. Any capture outcome that is not a
blocked proof resets the count. Shutdown and cancellation still take
precedence over this bound: the loop's top routes a shutdown to the retained
posture and a cancel to the cancel report before the bound is ever checked,
so neither is starved by it. A vault-locked (Codex credential-deferral) park
that hits the bound additionally keeps the custody hold, as its other
given-up paths do. This changes decision 4's restore-point-capture gating
above only in degree: the run still fails `worker_residue_blocked` with the
clone kept, it now does so after a bounded wait instead of an unbounded one.

**Test seam.** Production always scans the real `/proc` with real `SIGKILL`
and the 5 s deadline. A narrower view can be installed only in-process, only
by the programmatic, test-only `setQuiescenceViewForTests` — never by an
environment variable or config read, which would be a fail-open knob on
production code (see the `QuiescenceView` docstring in
`agent/src/run-quiescence.ts`). The runner-uid helper honours a `view` only
when it arrives on the stdin request the worker itself wrote to the child it
spawned, so a test view never crosses a trust boundary the worker does not
already own. Every agent test now installs an empty fake proc root by default
(`agent/test/setup/hermetic-proc.ts`, preloaded and re-imported by the test
harness), so no test's outcome depends on the host's live process table; the
real-procfs `ssh-agent` integration tests
(`agent/test/run-quiescence-procfs-ssh-agent.test.ts`) deliberately clear that
default and pin this fail-closed behaviour against the real proc root on
Linux CI, the same shape that produced the original incident.

The durable fix for this whole class, a per-attempt cgroup v2 boundary, is
not built here; it is Follow-up 5 in *Follow-ups and accepted risks* below.

## Root-only acceptance at the final head

(recorded after the final acceptance run, following the #1826 merge)

**This is mounted-source validation**, not a fresh image build. It covers the
code up to `34b6b6c8`, the final code commit when it ran; `agent/src` has
changed since (see *Not re-run after rework 3* at the end of this section). The merge `d82789ad` (origin/main
`1d4ba878113fc113a7d52778054e83a46b53e269`) and `34b6b6c8` both change
`agent/src`, so acceptance was re-run over that code. The commits up to
`8249dec9` changed only this ADR (`git diff --stat 34b6b6c8 8249dec9 -- agent/src
e2e` is empty).

The shared Docker daemon had 10.2G free of its fixed 29.4G pool, which is not
enough for a fresh build of the ~8.75GB worker base image. (Earlier fresh builds
on this branch failed with "no space left on device".) The run therefore used
an existing worker base image, with this tree's `agent/src` and `e2e/` mounted
into it.

Command: `CQ_SKIP_BUILD=1 CQ_IMAGE=uzi-agent-codex-git-trust:base
CQ_MOUNT_SRC=1 CQ_TIMEOUT=900 task test:clone-quiescence` → exit 0.

- Image ID `sha256:5c04540739e3cfd6fc43aa13620bb888593783a77eb505058a055ac7b879d788`.
- Build SHA (`org.opencontainers.image.revision`) `3692a3801e4ca11759f34b41d40f0acc8a4e097f`.
- Mounted source SHA: `34b6b6c8` (`agent/src` via `CQ_MOUNT_SRC`,
  `fixture.test.ts`: `const SRC = process.env.CQ_SRC ?? "/app/src";`; `e2e/`
  bind-mounted read-only).

Image inputs outside the mounted `agent/src` and `e2e/`, build SHA against the
accepted code commit `34b6b6c8`: `git diff --stat 3692a3801e4ca11759f34b41d40f0acc8a4e097f
34b6b6c8 -- agent/templates agent/package.json agent/package-lock.json
agent/bin agent/codex agent/devbox-global agent/tsconfig.json` is empty. That
covers each of the following:

- the Dockerfile itself, including its digest-pinned `FROM node:24-alpine@sha256:…`
  base layers;
- the entrypoint (`agent/templates/entrypoint.sh` → `/usr/local/sbin/uzi-entrypoint`);
- `setpriv` (installed by the Dockerfile's own `apk add`, with its util-linux
  `--reuid` assertion);
- the Codex supervisor, fileop and command-sandbox binaries (built from
  `agent/codex/supervisor`);
- `agent/devbox-global`, `agent/bin` and the package manifests.

The one remaining input, `COPY . /opt/uzi-src`, differs but is not read by the
fixture.

PASS lines (tests 7, pass 7, fail 0, cancelled 0, skipped 0):

```
✔ runs as the worker uid under the split, not as root or single-uid
✔ A-healthy: a clean clone is quiescent through the real setpriv helper
✔ A-healthy: an attributed runner-uid straggler is reaped by the helper; a sibling key's is not
✔ A-foreign-uids: worker, root and runner-cmd processes in scope are ignored, never unverified
✔ M3: the planted residue is root:root drwxr-sr-x with agent/src and gate-log.loop, and the worker cannot delete it
✔ M3: a CROSS-parent rename of the residue fails EACCES, which is why the quarantine stays in the same parent
✔ M3: the canonical reseed renames it within the same parent, keeps it, and completes
```

### Not re-run after rework 3 (the hermetic-test and bounded-capture rework)

`agent/src` changed after `8249dec9`: the two pr_summary spawn-site fixes
(`8fbaf2a5`, `27bd3789`), two merges of origin/main (`7675a2b1`, `61f4c8e4`), and
rework 3 (`run-quiescence.ts`: the test-only process-table view, the pid and comm
in the operator reason; `runner.ts`: the bounded recovery-capture retry). The
acceptance above therefore no longer covers the final code commit. `task test:clone-quiescence` was **not re-run** for it: the shared Docker
daemon no longer held any image built from `agent/templates/base/Dockerfile`
(neither the `uzi-agent-codex-git-trust:base` used above nor the script's own
default tag), so the mounted-source fallback had nothing to mount into, and with
10.2G free of the fixed 29.4G pool a fresh ~8.75GB build was not attempted. Run
it again once a worker base image is available.

What rework 3 was checked with instead: the agent unit suite (the hermetic view
installed for every test file), `task gate:agent`, `gate:api`, `gate:web` and
the `gate:repo` targets; and the maintainer's reproduction, `node:24-bookworm` as
the non-root `node` user with a setgid, same-uid `ssh-agent` running for the
whole of `task test:agent`, run with `--init`. There, the real-procfs ssh-agent
tests all took the unreadable branch and passed, naming the agent's pid. The one
remaining failure, `pr-size.test.ts`'s `--source` lookup, fails the same way at
origin/main in that image (its git 2.39 lacks `check-attr --source`). Without
`--init`, the container's PID 1 reaps no orphans, and seven process-group tests
fail on zombies.

## Follow-ups and accepted risks

The original follow-ups are recorded below with their later disposition where known.

- **Behaviour change: a resume or re-claim on a Docker-wired worker starts a
  new model session** (Option A above). This is an accepted, disclosed
  trade-off of the isolation guarantee, not a bug, but it is a real change from
  prior behavior worth calling out explicitly to anyone debugging a resumed
  run's apparent memory loss.
- **Follow-up 1 — the "container binding nothing under a clone path" scope
  reduction** (see *Disclosed scope reductions* above) is worth its own
  tracking issue: the Docker teardown only ever targets containers with a bind
  mount under the clone, so a container that touches the clone some other way
  is never found or torn down by this work.
- **Follow-up 2 — Option B, a per-attempt Docker API mediation proxy.** The
  reviewers named but did not build a proxy that would prove zero container
  exposure across an attempt boundary and so restore safe in-place session
  resume without losing conversational continuity (decision 16). This remains
  open work, tracked separately as #1860.
- **Follow-up 3 — unsafe automatic retention deletion disabled (Refs #1858).**
  The former sweep could delete an older attempt or residue after process-only
  proof despite unconfirmed Docker teardown. Disabling it removes that deletion
  path; it does not establish writer exclusion or provide safe automatic reclaim.
  #1860 separately owns daemon mediation. The unchanged `beforeFree` /
  `freeCanonicalClonePath` and quarantine flow still has a **process-proof /
  daemon pending-create residual**: a create already accepted by the daemon can
  complete after the process proof and clean listings. #2852's terminal/Docker
  writer-exclusion remainder is not fixed. Currently unwired status is not durable
  proof that a path was never exposed to a daemon.
- **Follow-up 4 — #2852 remains partial under the slice A contract (AI-synced 2026-10-18).** A holds owner
  pause and credential-free recovery, memory-pressure and retained-source capture
  through supervised-child settlement, including the narrow lane of an intentionally
  poisoned, successfully settled registry. It restores neither normal admission
  nor credentialed authority. Failed/withdrawn pause after possible drain and
  changed-milestone gate revision recreate a saved-session provider epoch before
  continuing; vault lock on recreation takes credential deferral. Slice B's healthy
  wall/hold/recovery/switch pre-publish closure and slice C's strengthened terminal
  proof remain unshipped. Existing credentialed permits remain required at their
  sinks; A's capture context is separate and supplies no structural completion permit.
  Owned summary/setup/auth cleanup remains joined after authority or delivery expiry,
  with independent cleanup faults refusing capture and credential continuation.
  Existing terminal executor disposal does
  not close the gap between snapshot coverage and writer-exclusion proof. A provides no daemon mediation,
  per-attempt cgroup proof or safe automatic retention reclaim. Refs #2852.
- **Follow-up 5 — a per-attempt cgroup v2 boundary.** Proper per-run process
  ownership would resolve the remaining concurrent-claim and single-uid
  lockout. This direction is parked in #1862.

**Accepted risks:**

- **The attempt ledger's compaction depends on hard links.** Rewriting the
  ledger atomically (decisions 10/14) publishes the edited config by
  hard-linking a temp file to `config.lock` before renaming it into place
  (`agent/src/git.ts`, `rewriteLedgerAtomically`) — git's own lockfile
  protocol. A data volume that does not support hard links cannot run this
  compaction; that is a deployment constraint on this design, not a design gap.
- **A crash between the hard-link and the rename can leave a stale
  `config.lock`.** This is the same window git's own config writes have, not a
  new one this work introduces: any failure before the rename leaves the bare
  `config` file untouched, but a process killed at exactly that instant leaves
  `config.lock` behind, which would block a later git config write against the
  same bare until the stale lock is cleared.

## Docker-wired Codex worker: attempt seeds are also self-contained

On a Docker-wired worker running Codex, the attempt-path seed introduced by
this work is made self-contained the same way the canonical seed already was
(#1769 materialization): after every ref/checkpoint step, and still under the
bare's lock, the clone is dissociated from the bare because the Codex command
sandbox does not grant the bare and git inside the sandbox cannot follow the
alternate. This applies to both the canonical seed and, since the merge that
reconciled #1783 with #1769, the per-attempt seed as well (see
`runnerCloneForBranch`'s docstring in `agent/src/git.ts`).

## Amendment 2026-10-08 — #2512: retained recovery on unwired workers

The same-path exception in decision 15 applies to ordinary unwired keys
that never entered retained-source recovery. A retained-source claim uses
a fresh attempt path and model session regardless of Docker wiring.
Recovery must not run a model in the predecessor while its source and
evidence remain necessary for custody. This extends the fresh-path trade-off
to retained recovery without changing ordinary unwired continuity.

Discovery precedes forge refresh and disk preflight. Recovery reserves a
durable source-bound budget of 3 total iterations (blocked and nonblocked
together), with a five-minute deadline from the first reservation and the
existing exponential retry cap of 16 times its base. Reservations survive
crash, reclaim and successor handoff; reset requires a trusted, successfully
settled real model turn. External restarts require a genuinely fenced
successor, not reuse of a discovered ready successor as execution authority.

A permanent blocker or exhaustion ends `failed` with `keepCustody`, no
automatic reclaim and no failed-run Resume. In this retained-recovery path,
`worker_residue_blocked` denotes actual quiescence failure; archive size,
prerequisite, source, clock, preservation and adoption blockers have their
own accurate reasons. Healthy provider/empty-turn parks and the owner
worker-recovery-exhaustion hold keep their existing budgets.

An actual verified thin bundle under the cap can support local adoption
and model execution with locally available, verified prerequisites despite
unknown remote publication. A nonempty prerequisite list is not itself a
blocker; unavailable/unverifiable needed prerequisites or a still-oversized
fallback are. This local proof grants neither independent recovery, remote
durability nor custody release. Predecessor sources, pins, journals and
descriptors remain until existing verified final disposition or explicit
discard; guarded archive-release conditions are unchanged.

Healthy unknown publication stays in the same generation with the tip,
owed roots and publication time gate retained; confirmed durability requires
positive proof. No API/schema, hold-rebinding, custody-cap or #2486 behavior
change is included, and [ADR-1751](1751-continuation-custody-admission.md)'s
soft cap is unchanged. Local storage loss remains a limit: a failed
source-only run requires operator recovery from retained storage, and
export still requires a manifest-bound available archive. There is no new
download API or failed-run Resume. Downgrade during pending recovery is
unsupported because older workers may drop its recovery fields.

See the matching [ADR-1197 amendment](1197-transient-recovery-park.md#amendment-2026-10-08--2512-source-bound-retained-recovery)
and [user recovery guide](../docs/run-recovery-wait.md#bounded-retained-source-recovery).

## Amendment 2026-10-09 — #2512: terminal retained custody

A terminal run's retained recovery journal must not monopolize a branch's active
slot. After exact terminal ownership validation, the worker validates the full
journal and every source identity, then proves capture-mode quiescence over the
primary, retained predecessors, recovery source and recorded successor. This
also applies to canonical sources on unwired workers.

Detachment compares the full snapshot again after the proof, including recovery
counters and siblings. Under the bare lock it persists and reads back a
worker-owned config record, `uzi-retained.<runId>.journal`, containing
`{version:1,branch,key,journal}`, with the complete original journal. It marks
every attempt source reclaimed and clears only the matching active slot.
Interruption at write, readback, ledger append or clear retains active evidence
or both records for a bounded, idempotent retry. Malformed, duplicate,
conflicting or unattributed evidence refuses detachment and inventory.

Protected records preserve custody evidence, source paths and recovery pins;
they grant no seed, discovery or executor authority. Inventory and ledger
compaction include their sources; the former retention sweep also protected them.
#1858 disables automatic retention deletion. Backup validates the exact run's
full protected descriptor and ledger identities before selecting its current
clone or a retained predecessor, even after a newer run owns the active slot;
completed and cancelled runs with protected sources remain eligible. Inconclusive
evidence fails the backup cycle without replacing the previous latest backup.
The backup remains a selected-source snapshot, not an atomic archive of all
siblings, and detachment grants no remote durability or server custody release.

The per-key attempt-mode marker is persisted before releasing the active slot.
Later runs use fresh attempt paths and model sessions even without Docker;
a canonical source needs no invented attempt id or on-disk marker. Explicit
owner discard addresses the exact protected run, deletes only its validated
pins and descriptor, leaves source bytes intact, and preserves any newer active
journal and the persistent attempt-mode marker.

## Amendment 2026-10-09 — #2512: verification resource limit

**Historical cap policy:** the 1 GiB limit below is superseded by
[ADR-1197's Amendment 2026-10-11 — #2743](1197-transient-recovery-park.md#amendment-2026-10-11--2743).
The current delivered decoded-history cap is worker-configurable through
`UZI_RECOVERY_DECODED_LIMIT_BYTES`, default 8589934592 bytes (8 GiB), with a fixed
ceiling of 68719476736 bytes (64 GiB). The linked amendment records the reported
offline measurement, startup validation, cumulative repeated-read accounting,
restart behavior and deployment exposure boundary. Full reachable-history hash
verification, prerequisites, custody, cancellation, deadlines and archive
quotas remain unchanged. The cap is not a RAM limit: streaming content buffers
are independent of it, metadata listings retain the 64 MiB `GIT_MAX_BUFFER`
output cap, and Git-internal delta decompression and sibling-worker exhaustion
remain the accepted #2512 residual risk.

Human review authorizes a 1 GiB delivered decoded-history limit for reachable
object integrity verification, even for a thin archive below 64 MiB. The
worker verifies hashes under the existing deadline, records a visible blocker
in `uzi run recovery` and its log on overflow, and retains source and custody
on overflow, timeout, corruption or interruption. This limit is separate from
archive size and does not make a thin archive independently recoverable.

The 1 GiB budget counts delivered object contents cumulatively within a
recovery operation, including repeated verification reads. The delivered-byte
cap does **not** bound Git-internal delta decompression memory. The deadline limits only duration, and the shared worker cgroup
does not isolate this verifier from sibling runs. The resulting residual
resource-exhaustion risk is deferred scope, as authorized by the human
review on 2026-10-09 (#2512).

## Amendment 2026-10-14 — #1859: shared per-key residue retention

**Historical implementation record — superseded by #1858 below.** The entire
quoted description records #1859's delivered sweep, not current deletion policy.

> The Docker-only sweep and unwired accepted-gap paragraph in *Disclosed scope
> reductions* records the original implementation. Current code closes that
> residue-reclamation gap without changing ordinary unwired canonical continuity
> or the recovery-enabled fresh-attempt behavior recorded above.
>
> In `agent/src/git.ts`, `runnerCloneForBranch` calls the shared `sweepResidue`
> under the bare lock **after successful canonical free and before the new seed**.
> The attempt path calls the same helper after seed/live registration.
> Abandoned-attempt and orphaned-skills cleanup remains exclusive to the attempt
> path; canonical reseeding adds residue cleanup, not those phases.
>
> The helper considers direct siblings of the same clone key that match the exact
> parser grammar `.uzi-residue-<key>.residue-<uuid>` (`<uuid>` is a lowercase v4
> UUID). Five entries is a best-effort retention target, not a disk bound. It sorts
> the original candidates by their own lstat mtime, then basename, and considers
> only the original oldest excess slice. A refused excess entry stays; no newer
> entry is substituted to compensate. Parent validation and complete enumeration
> precede reclamation. A private fixed 4096-direct-entry budget bounds this
> enumeration, not storage; overflow skips all residue reclamation for that sweep.
> The budget is not a setting. With no excess, the helper returns without a
> redundant journal discovery.
>
> Expensive residue candidate processing is separately capped at 16 attempts per
> sweep, counting refusals. A per-clone-key in-process cursor rotates through the
> original oldest excess slice, so a persistently refused prefix cannot starve
> later excess. The cursor records ordering position only, never safety evidence;
> every attempt repeats the fresh checks below. The newest five remain outside
> the candidate slice. Cursor storage is a 128-key LRU, cleared when no excess
> remains. Process restart or LRU eviction starts that key from the oldest again.
> This is a work-count bound, not a hard elapsed-time bound: a started operation
> still settles under its existing safety/deletion deadlines before returning.
>
> Before deletion, fresh readable journal discovery protects a candidate or any
> descendant named by recovery evidence, and the live registry protects live paths.
> Deletion requires an affirmative scoped seed-process verdict, with journal,
> live and quarantine checks repeated around the proof and before deletion.
> An active or newly latched unreadable-unattributed worker quarantine keeps all
> residue entries. False, absent or unverified proof and generic thrown proof keep
> the candidate with bounded diagnostics; classified control/recovery
> interruptions propagate. Fresh-attempt seeding's unreadable-process relaxation
> never authorizes retention deletion. Off Linux, an absent process verdict keeps
> entries rather than inventing proof. See [ADR-2213](2213-worker-residue-quarantine.md#amendment-2026-10-14--1859-residue-retention-veto).
>
> Canonical retention in `agent/src/runner.ts` forwards the existing configured
> best-effort Docker handling. Unwired workers gain no daemon obligation.
> Attempt retention preserves its existing `processOnly: true` posture.
> **Follow-up 3 remains an inherited risk**: this change introduces no confirmed
> container verdict and does not redesign Docker teardown. Existing runner-uid,
> path/name validation and deletion deadlines remain in place. A possibly alive
> rm stops further cleanup; a file or symlink residue entry is unlinked itself,
> without following its target or recursively deleting through it.
>
> Safety can leave more than five entries. This adds no hard disk bound,
> background timer or custody change. At delivery, the Linux
> `task test:clone-quiescence` image build timed out before actual cases; real
> procfs/uid verification from that leg remains incomplete, pending maintainer
> verification.

## Amendment 2026-10-18 — #1858: retain artifacts, diagnose accumulation

Refs #1858: retention-only, non-closing delivery. `diagnoseRetainedArtifacts`
replaces automatic sweep deletion of disposable-kind attempts (abandoned and
crash-window live orphans), shared own-key residue at both canonical and attempt
entries, and orphaned skills on every worker. All those artifacts remain; there
is no safe automatic reclaim mechanism. No process scan, daemon listing (including
repeated clean listings), time, age, count or disk pressure authorizes retention
deletion. Current unwired status does not prove absence of prior daemon exposure.

The read-only helper validates the direct parent and observes at most 4096 direct
siblings, without a recursive byte walk. Observed counts are lower bounds with
completeness; overflow or read failure gives partial/unknown evidence, never zero
by inference. Existing predicates fail closed. A true `custodyHeld` result means
protected-or-unreadable, so attempt classification is partial without a second
probe. There is no retention proof, deletion, rename or retired-ledger append.

Known excess alone (>3 attempts, >5 residue, or >0 orphaned skills) can emit
`runner clone retention: automatic reclaim disabled`, with a sanitized, truncated
key and observed counts/completeness. Warnings are limited to one per hour per full
canonical key in a bounded 128-key LRU; restart or eviction may warn again.
Diagnostic memory and enumeration are bounded; retained bytes, identity metadata
and the safety inventory are not. Ledger compaction keeps identities for present
paths. Existing process/teardown scope over non-live paths and compaction work
grow with inventory; no cap skips safety checks.

Normal cleanup, `beforeFree` / `freeCanonicalClonePath`, quarantine, capture and
operator discard contracts remain unchanged. The process-proof / daemon
pending-create residual remains; #1860 is separate daemon mediation, and #2852's
terminal/Docker writer-exclusion remainder is not fixed. This delivers no proxy,
marker, ledger, API, schema, configuration flag or new infrastructure.

Attributed attempts and paired skills may use the existing guarded
[retained-source discard](../docs/run-recovery.md#retained-sources-with-no-open-hold-worker-local-inspect-and-discard).
Shared residue and orphaned skills have no supported generic reclaim route.
Server custody discard alone is not local writer-exclusion proof.
