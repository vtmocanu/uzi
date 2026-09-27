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
   worker-marked spawns. It kills what is in scope, rescans until empty, and
   reports one of three states: `quiescent`, `survivors`, or `unverified`.
   `unverified` (an unattributable process, a helper timeout, a malformed answer)
   and `survivors` both **fail closed**: the worker never treats "could not prove
   it" as "proved it."
3. **A Docker Engine API teardown** force-removes every container with a bind
   mount under the clone, then lists until two consecutive listings are clean. Its
   results are `not_wired` (no daemon configured), `docker_unconfirmed`, or
   `docker_error`. **It never reports `quiescent`**: a `create` the daemon already
   accepted can still complete after the last listing, so no Docker result may
   ever be read as proof of quiescence.
4. **Gating by site**, all fail-closed on a non-quiescent verdict:
   - the limit, wall-clock and completion-hold parks leave the park standing, with
     no credentialed publish;
   - an owner pause that cannot prove quiescence reports `pause_failed` and the
     run keeps running (Decision 8) rather than parking on unproven ground;
   - finalize fails the run with the typed `fail_origin` `worker_residue_blocked`;
   - terminal retire keeps the clone rather than removing it;
   - graceful shutdown, milestone checkpoint, and restore-point capture are all
     gated the same way.
5. Re-proofs run after any runner-clone git operation, since git itself can run
   repo-configured code (filters, textconv) that spawns processes.
6. Every worker-invoked runner-clone git call sets `GIT_NO_LAZY_FETCH=1`; the
   worker-marked subset additionally sets `GIT_ALLOW_PROTOCOL` to a value naming no
   real protocol, so git refuses every transport. Together these close a
   promisor/uploadpack exec path that could otherwise inherit the worker mark and
   run arbitrary repo-configured code with it.
7. uzi's own e2e scripts use `--mount type=bind` instead of the older `-v` bind
   syntax, so a bind's exact source and target are always explicit.

### M2 — attempt-unique clone paths on Docker-wired workers

8. **Each execution attempt seeds a fresh path**,
   `<runnerRoot>/<repoDir>/<key>.attempt-<attemptId>`
   (`agent/src/attempt-path.ts`), and an exposed attempt path is never reused by a
   later attempt. **This is the isolation guarantee itself** — a later attempt
   never inherits an earlier attempt's residue, escaped process, or bind-mounted
   container, because it never touches that path at all. M1's sweep and M3's
   quarantine are recovery for what is left behind, not what makes isolation hold.
9. **The `.attempt-` separator**, not `@` or another delimiter: the attempt id
   (`<UTC timestamp>-<claim generation>-<16 hex>`) is itself hyphen-separated, and
   the basename grammar needs a separator that cannot appear inside a git branch
   component (which the key is derived from) while staying unambiguous under a
   greedy-key regex — the LAST `.attempt-` followed by a complete, anchored id is
   always the true separator, so key and id parse back losslessly even if a key
   were ever to contain the literal string `.attempt-` followed by other text.
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
13. **Retention is bounded**: 3 abandoned attempts per key, and 5 quarantined
    residue entries per key (`agent/src/git.ts`,
    `RETAINED_ABANDONED_PER_KEY` / `RETAINED_RESIDUE_PER_KEY`). Deletes run as the
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
    uid. On `EACCES`/`EPERM`/`ENOTEMPTY` it renames the residue, within the SAME
    parent directory, to `.uzi-residue-<key>.residue-<uuid>` and keeps it there —
    it is never deleted by this path. The same-parent constraint is not
    stylistic: moving a root-owned directory to a DIFFERENT parent needs write
    permission on that directory's own `..`, which a root-owned subtree does not
    grant the runner uid; renaming within the same parent needs only write
    permission on the parent the runner uid already owns.
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

## Disclosed scope reductions

- A container that binds nothing under a clone path is not covered by the Docker
  teardown; it is out of scope by construction, not missed.
- Deliberate env-marker forgery or cwd-based evasion is explicitly out of scope.
  This work addresses the ACCIDENTAL escape path (a detached shell, a stray
  background job); it is not a defense against an agent deliberately trying to
  survive its own reaper.
- Every pause now kills the attempt's background processes and removes its
  clone-bound containers — this is new coverage, not a pre-existing behavior.
- Residue quarantine (M3) covers Docker-wired workers only; unwired workers are
  not swept for the same class of residue.
- The ledger rewrite (decision 10/14) needs hard links on the data volume; a
  volume that does not support them is a deployment constraint on this design,
  not a design gap.
- Legacy journaled root-owned canonical retire (residue that predates M3) is a
  follow-up, not retrofitted by this work.

## Invariants future code must respect

- Never treat a Docker teardown result as proof of quiescence; only the process
  reaper's `quiescent` state is proof, and only when paired with a clean Docker
  listing where a daemon is wired.
- Never mark `UZI_WORKER_SPAWN` on anything other than a worker-authored,
  fixed-argv spawn. Marking a repo- or agent-invoked command breaks the
  attribution the reaper depends on to distinguish worker infrastructure from
  agent activity.
- Never introduce a second attempt-path grammar or separator; parse and mint
  paths only through `agent/src/attempt-path.ts`'s helpers, so `cloneKeyOf`,
  `parseAttemptPath` and `formatResidueName` stay the single source of truth for
  every consumer (the reaper, the retention sweep, `uzi-watcher`'s recovery
  scripts).
- Never delete a `reclaimed` ledger entry's clone via the retention sweep, and
  never delete anything as the worker uid when the runner uid can do it instead.
- Never follow a symlink or non-directory found at a canonical clone path during
  reseed; quarantine it unopened.
- Keep the two quarantine directories semantically distinct: journaled retirees
  go to `runner-quarantine`, unjournaled residue goes to `.uzi-residue-*`.
  Merging them would erase the diagnostic signal that a `.uzi-residue-*` entry
  carries.

## Consequences

The worker can now prove a clone is safe to touch before every destructive or
credentialed operation, and fails closed rather than guessing when it cannot.
Attempt-unique clone paths mean an escaped process or a mis-owned directory from
one attempt can never contaminate a later attempt's work, at the cost of
same-cwd session resume on Docker-wired workers (Option A). Unremovable residue
is quarantined rather than left to wedge the worker or silently deleted. The
durable fix for Option A's continuity loss (Option B, a per-attempt Docker API
mediation proxy) remains open work.
