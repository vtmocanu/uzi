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
   mount under the clone, then lists until two consecutive listings are clean. Its
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
     parking on unproven ground;
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

### M2 — attempt-unique clone paths on Docker-wired workers

8. **Each execution attempt seeds a fresh path**,
   `<runnerRoot>/<repoDir>/<key>.attempt-<attemptId>`
   (`agent/src/attempt-path.ts`), and an exposed attempt path is never reused by a
   later attempt. **This is the isolation guarantee itself** — a later attempt
   never inherits an earlier attempt's residue, escaped process, or bind-mounted
   container, because it never touches that path at all. M1's sweep and M3's
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
    the retention sweep's delete.
11. The recovery journal carries the attempt id, so a journal entry and a ledger
    entry can be cross-checked for the same attempt.
12. Journal cases are classified by identity (C'/B'/A'), and a C' capture proves
    predecessor-scoped quiescence before releasing the predecessor's path in
    place — the predecessor is never removed out from under a proof that has not
    run.
13. **Retention is bounded where the sweep runs**: 3 abandoned attempts per key,
    and 5 quarantined residue entries per key (`agent/src/git.ts`,
    `RETAINED_ABANDONED_PER_KEY` / `RETAINED_RESIDUE_PER_KEY`) — see the disclosed
    scope reduction below: the sweep that enforces these caps runs only on
    Docker-wired workers. Deletes run as the
    **runner uid**, never a worker-uid recursive removal over a tree the runner
    (and, transitively, the agent) can write to. Reclaimed-but-uncaptured attempts
    are excluded from deletion regardless of age or count.
14. The ledger is compacted per seed, so it does not grow without bound across the
    life of a branch.

### Option A — the accepted resume trade-off

15. **On a Docker-wired worker, a resume or re-claim always starts a NEW model
    session** (a fresh Claude SDK session or a fresh Codex thread), because a
    session is keyed by HOME *and* cwd, and every attempt runs at a fresh cwd by
    construction (decision 8). The earlier session's transcript can never resolve
    from the new attempt path. Continuity of WORK, not of conversation, is what
    survives: the tracking ref, the recovery journal, and captured checkpoints
    carry the prior attempt's committed state forward; only the model's own
    memory of the conversation is lost. Unwired workers (no Docker daemon
    configured) keep full same-path resume and are unaffected. The reviewers
    chose this trade-off under the user's delegation: it is a real, disclosed cost
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
    `worker_residue_blocked`, not as one isolated failure. The retention sweep
    honours the same record: a sweep target at or under an unsettled path is
    refused and kept with a warning (the sweep does not fail the run), and the
    worker log records the path entering (`runner-uid delete recorded as
    unsettled`) and leaving (`left the unsettled record`) the record.

## Disclosed scope reductions

- A container that binds nothing under a clone path is not covered by the Docker
  teardown; it is out of scope by construction, not missed.
- Deliberate env-marker forgery or cwd-based evasion is explicitly out of scope.
  This work addresses the ACCIDENTAL escape path (a detached shell, a stray
  background job); it is not a defense against an agent deliberately trying to
  survive its own reaper.
- Every pause now kills the attempt's background processes and removes its
  clone-bound containers — this is new coverage, not a pre-existing behavior.
- **M3 canonical-path quarantine (`freeCanonicalClonePath`/`quarantineCanonical`
  in `agent/src/git.ts`) runs on EVERY worker, wired or not**: `attemptSeedOptions`
  returns `undefined` when the worker has no Docker endpoint, so
  `runnerCloneForBranch` takes the canonical (non-attempt) path, and that path
  always reseeds through the M3 quarantine, not just on Docker-wired workers.
  What is genuinely Docker-only is the retention SWEEP (`sweepRetainedArtifacts`)
  that bounds decision 13's caps and reclaims old residue — it is called only
  from `attemptCloneForBranch`, which itself only runs when the worker is
  Docker-wired. So an unwired worker quarantines residue exactly as a
  Docker-wired one does, but nothing ever caps or sweeps what accumulates there:
  a `.uzi-residue-*` entry on an unwired worker is retained forever. This is an
  accepted gap, not a design intent; treat unbounded residue growth on unwired
  workers as a known follow-up.
- The ledger rewrite (decision 10/14) needs hard links on the data volume; a
  volume that does not support them is a deployment constraint on this design,
  not a design gap. The compaction that bounds the ledger's size
  (`compactAttemptLedger`) is itself best-effort: it catches its own failure,
  logs "attempt ledger compaction failed; the ledger is left as it was", and
  leaves the ledger uncompacted rather than failing the run. A volume without
  hard link support therefore degrades to unbounded ledger growth over time,
  not a hard failure.
- Legacy journaled root-owned canonical retire (residue that predates M3) is a
  follow-up, not retrofitted by this work.

## Invariants future code must respect

- **The process reaper's `quiescent` state is the only proof of quiescence a
  seed or capture sweep can ever get, and where it applies it alone is
  blocking.** `teardownDocker` never returns `quiescent`, ever — its three
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

  **For an own-mode Codex sink that is credentialed through `reapForSink` or
  `withCodexBoundaryOnly`** — the limit park, the graceful-shutdown publish,
  the milestone checkpoint, the pre-settle reap, and finalize — the Codex
  supervisor's own `withBoundary` boundary is what proves the run's processes
  have drained before the sink runs, substituting for the skipped reaper scan.
  **The remaining sinks split three ways, not two.** The pause park
  (credential-free publish, no permit minted at all — per the m4 structural
  rule, a path mints a permit iff it does a PAT-bearing overlay publish or a
  terminal/finalize reap) and terminal retire (no publish at all) call
  `quiesceRun` directly and get no boundary anywhere
  (`codex-executor.ts`: "`parkForPause` does not reap the provider root").
  The wall park and the completion hold instead route their credentialed
  publish through the shared `captureHoldContext`, whose publish runs under
  its own `withCodexBoundaryOnly` — but only the publish: the status read,
  the WIP marker commit and the fetch-back that precede it run with no
  boundary and no process proof. `captureRecoveryRestorePoint` (the
  `recovery_capture` and `credential_switch` sites) has the identical shape:
  a direct `quiesceRun` call, then an unboundaried status read/WIP
  marker/fetch-back, then a `withCodexBoundaryOnly`-wrapped publish — except
  its credential-free branch (`opts.credentialFree`, taken by the Codex
  vault-lock park and its retries): there the executor's
  `settleForCredentialFreeCapture` first poisons and drains the Codex
  registry, and the publish then opens NO Codex boundary. So on a
  Codex run: pause park and terminal retire have no process proof at any
  point; the wall park, the completion hold, and `captureRecoveryRestorePoint`'s
  two sites have no process proof over their pre-publish steps, with the
  supervisor boundary picking up only the publish itself (on the
  credential-free vault-lock capture, the settle drain precedes those steps
  and the publish is unboundaried instead). **In every one of
  these unboundaried steps, `quiesceRun`'s own `executor.killAgentTree?.()`
  call kills nothing for Codex**: `CodexExecutor` deliberately does not
  implement `killAgentTree` ("This class deliberately does NOT implement
  `killAgentTree`"), so the optional call is a no-op there and the only thing
  that ever runs against a Codex run's processes at an unboundaried step is
  the best-effort Docker teardown — a disclosed gap (see *Follow-ups* below),
  not a design intent.
- Never mark `UZI_WORKER_SPAWN` on anything other than a worker-authored,
  fixed-argv spawn. Marking a repo- or agent-invoked command breaks the
  attribution the reaper depends on to distinguish worker infrastructure from
  agent activity.
- Never introduce a second attempt-path grammar or separator inside the agent:
  parse and mint paths only through `agent/src/attempt-path.ts`'s helpers
  (`cloneKeyOf`, `parseAttemptPath`, `formatResidueName`) and mint attempt ids
  only through `mintAttemptId` (`agent/src/run-quiescence.ts`) — they are the
  single source of truth for every in-process consumer, including the reaper and
  the retention sweep. `uzi-watcher`'s recovery shell scripts
  (`.agents/skills/uzi-watcher/scripts/backup-runs.sh`) are outside that
  boundary: they cannot import TypeScript, so they parse the same
  `<stem>.attempt-<id>` grammar themselves (`ATTEMPT_RE`, basename matching) — a
  second, necessarily duplicated implementation. Keep it byte-for-byte in sync
  with `attempt-path.ts`'s grammar when either changes.
- Never delete a `reclaimed` ledger entry's clone via the retention sweep, and
  never delete anything as the worker uid when the runner uid can do it instead.
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

The worker can now prove that a run's OWN processes have stopped before every
destructive or credentialed operation gated on it, and fails closed rather than
guessing when it cannot. This is narrower than proving the clone itself is
safe to touch: the reaper's process scan is gated to run at all only on Linux
workers (`agent/src/runner.ts`'s `process.platform === "linux"` check), and
for a Codex run's own-mode proof the scan is skipped outright. Where that
own-mode sink is credentialed through `reapForSink`/`withCodexBoundaryOnly`
(the limit park, graceful shutdown, the milestone checkpoint, the pre-settle
reap, and finalize), the Codex supervisor's own `withBoundary` boundary
substitutes as the process proof (see the invariant above). The pause park
and terminal retire call `quiesceRun` directly with no boundary anywhere; the
wall park, the completion hold, and `captureRecoveryRestorePoint`'s
`recovery_capture`/`credential_switch` sites route their credentialed publish
through their own `withCodexBoundaryOnly`, so only their pre-publish steps
(status read, WIP marker, fetch-back) run with no process proof. In every one
of these unboundaried steps, `quiesceRun`'s `killAgentTree?.()` call is a
no-op on Codex (`CodexExecutor` deliberately does not implement
`killAgentTree`), so nothing kills a Codex run's processes there and only the
best-effort Docker teardown runs — a disclosed gap, not a design intent (see
*Follow-ups* below). The reaper itself is the proof only for a Claude/stub
run, or for a `seed`/`capture` sweep (which runs regardless of executor). It also says nothing about a Docker
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
  caches before `captureRecoveryRestorePoint`. That reap is cleanup, never
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
  three new values (with `gate_presentation_refused` and `data_volume_full`) that
  a worker may report. All three are never judged, and none is human-landable.

## Root-only acceptance at the final head

(recorded after the final acceptance run)

Final code commit 576521c0 (runner.ts comment + spawn-site test inventory);
later commits change only documentation and code comments. Acceptance ran at
60f164b1 (`git diff --stat 576521c0 60f164b1`: adr, docs, embed mirror,
specs only).

A fresh build (`CQ_BUILD_ARGS="--network host" CQ_IMAGE=cq1783:9ef3a599ddda
CQ_TIMEOUT=2400 task test:clone-quiescence`) failed twice at image extraction
with "no space left on device" on the shared Docker daemon's fixed 29.4G
storage pool (no test ran); so the run used an existing worker base image
with this tree's `agent/src` mounted:

Command: `CQ_SKIP_BUILD=1 CQ_IMAGE=uzi-agent-codex-git-trust:base
CQ_MOUNT_SRC=1 CQ_TIMEOUT=900 task test:clone-quiescence` → exit 0.

Image ID `sha256:5c04540739e3cfd6fc43aa13620bb888593783a77eb505058a055ac7b879d788`,
`org.opencontainers.image.revision` `3692a3801e4ca11759f34b41d40f0acc8a4e097f`.
`git diff --stat 3692a3801e4ca11759f34b41d40f0acc8a4e097f 576521c0 --
agent/templates agent/package.json agent/package-lock.json agent/bin
agent/codex agent/devbox-global agent/tsconfig.json` is empty; the two other
Dockerfile build inputs, `COPY agent/src ./src` (superseded by the `CQ_SRC`
mount) and `COPY . /opt/uzi-src`, differ between those commits but are not
read by the fixture. The fixture (`e2e/`, mounted read-only) and
`agent/src` (mounted via `CQ_MOUNT_SRC`; `fixture.test.ts:40` `const SRC =
process.env.CQ_SRC ?? "/app/src";`) come from the tree at 60f164b1, whose
`agent/src` equals the final code commit 576521c0's (`git diff --stat
576521c0 60f164b1 -- agent/src` is empty), and the commits after 60f164b1
(978cbde5, 312a8fb7, c7e97e4c and the ADR fix that follows them) change only
docs and code comments.

PASS lines (7/7; tests 7, pass 7, fail 0, cancelled 0, skipped 0):

```
✔ runs as the worker uid under the split, not as root or single-uid
✔ A-healthy: a clean clone is quiescent through the real setpriv helper
✔ A-healthy: an attributed runner-uid straggler is reaped by the helper; a sibling key's is not
✔ A-foreign-uids: worker, root and runner-cmd processes in scope are ignored, never unverified
✔ M3: the planted residue is root:root drwxr-sr-x with agent/src and gate-log.loop, and the worker cannot delete it
✔ M3: a CROSS-parent rename of the residue fails EACCES, which is why the quarantine stays in the same parent
✔ M3: the canonical reseed renames it within the same parent, keeps it, and completes
```

## Follow-ups and accepted risks

These are not filed yet; listed here for the maintainer to file after merge.

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
  open work.
- **Follow-up 3 — the retention sweep can delete an older attempt or residue
  path after a process-only check, even when the Docker teardown was
  unconfirmed.** Raised in a bot security review: since a Docker result never
  gates the sweep's deletions (only the process proof does), a sweep-triggered
  delete can remove a path whose Docker containers were never confirmed torn
  down. A follow-up could require a fresh, successful Docker-use check before
  deleting a retained path, rather than relying on the process proof alone.
- **Follow-up 4 — on a Codex run, several sinks have a process-proof gap,
  though not all the same gap.** `handlePausePark` and terminal retire call
  `quiesceRun` directly and get no boundary anywhere: no process proof runs
  at any point, and `quiesceRun`'s `killAgentTree?.()` call is a no-op on
  Codex (the executor deliberately does not implement it), so only the
  best-effort Docker teardown ever touches a Codex run's processes there.
  `enterWallPark`, the completion hold, and `captureRecoveryRestorePoint`'s
  `recovery_capture`/`credential_switch` sites are narrower: their
  credentialed publish is wrapped in its own `withCodexBoundaryOnly`
  (`captureHoldContext`'s publish, and `captureRecoveryRestorePoint`'s own),
  so the supervisor's drain proof does cover the publish itself, but the
  status read, WIP marker commit, and fetch-back that precede it run with no
  boundary and no process proof. The credential-free (vault-lock)
  `recovery_capture` branch is the inverse: the executor's
  `settleForCredentialFreeCapture` drains the Codex registry before those
  steps, and the publish itself opens no boundary. Unlike the limit park, graceful shutdown,
  the milestone checkpoint, the pre-settle reap, and finalize (which get the
  supervisor's `withBoundary` drain proof over their whole credentialed
  region because they are routed through `reapForSink`/`withCodexBoundaryOnly`
  end to end), none of these six sites gets that coverage for their
  pre-publish steps. This is disclosed here for the maintainer to file as its
  own tracking issue, not filed yet.

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
