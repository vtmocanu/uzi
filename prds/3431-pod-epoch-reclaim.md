# PRD #3431: Delete settled attempt trees at a verified pod epoch

**Status**: Draft (reviewed together with PRD A). This is PRD B of two for #3329's disposal. PRD A (#3430, `prds/3430-pod-epoch-lock-shadow.md`) ships the epoch lock, the controller handoff and a shadow verdict listing what would be deleted. This PRD turns that verdict into deletion.

**Blocked by:**
- PRD A merged, and its live shadow acceptance passed (verdicts and would-delete lists checked against real rolls; caps measured);
- #2852 slice C, the exclusion phases #3329's dispatch gate names. It also touches `runner.ts` and `run-quiescence.ts`.

## Problem

Settled attempt trees fill Docker workers' data volumes (#3329). PRD A shows, at each pod epoch, which trees could be deleted under a verified writer-exclusion proof, but it deletes none of them.

## Outcome

At an `eligible` pod epoch (PRD A's verdict, under EX), the new pod deletes exactly the trees on its would-delete list, through the existing discard core (ledger `retired`), and keeps every other tree. Only after that does it open claim admission.

The proof and its owner-accepted residual are PRD A's, unchanged. The proof covers the immediate predecessor, any intervening pods and concurrent workers. It does **not** exclude every historical writer: an older pod's process may survive its own termination and still write after a later evidenced epoch, and the owner accepted that residual on 2026-10-24.

Acceptance examples:
1. PRD A's acceptance example 1, with the three settled trees deleted (ledger `retired`) and the held tree kept. Only then is admission opened.
2. One discard fails with a settled `retained` outcome (for example `EACCES`). That tree is kept, the rest continue, and the verdict is `partial`.
3. A discard's outcome is uncertain at the deadline. The worker does not open admission. It exits non-zero; the one-shot stays consumed, so the restarted process does not reclaim.

## Out of scope

The same exclusions as PRD A, except deletion itself.

## Modules and seams (additions to PRD A)

### Deletion

- For each tree on the would-delete list, the worker re-evaluates the authority under EX: holds through `listRecoveryHolds`, lifecycle from the ownership probe, and the `evaluateRetained` disposition (`local_discard` / `local_protected_discard`). It also re-checks the pod-entry record before each tree.
- It deletes in place through `discardRetainedSource` (`agent/src/git.ts:8652`) with:
  - `isLivePath` returning false (nothing is live before admission);
  - `confirm` set to the inspect digest;
  - a **reclaim `quiesce`**: a process scan plus the epoch verdict.

  It never calls `teardownDocker`, which issues `DELETE /containers` (`run-quiescence.ts:2259`), and never reuses `withRetainedSourceBoundary`.
- The discard appends `retired` as operator discard does. There is no new API, migration or ledger state.

### Lock lifetime across destructive children

- Every child process the pass spawns that can mutate the tree, refs, retention records or ledger receives the lock descriptor, so the lock is released only when that child exits. That covers the uid-split `rm` (`git.ts:3785`, stdio `:3809`) and git children such as ref and worktree updates.
- The descriptor is passed through one spawn-option hook in the discard core, scoped to the reclaim pass's async context with AsyncLocalStorage, which GitCache already uses. Run and SDK spawns never inherit it, because any holder of that open file description could `LOCK_UN` it.
- A child left without the descriptor needs a code citation in the PR showing that it cannot mutate protected state.
- **Unsettled** means any of:
  - an `unknown` outcome from `discardRetainedSource` (catch at `:8738`; `incomplete` return at `:8747`);
  - a latch `stopped()` result;
  - `RunnerDeleteUnsettledError` (`:3825`);
  - a non-empty `unsettledDeletes`.

  A settled `retained` outcome is a kept tree and does not count. While anything is unsettled, the lock is never converted or released and admission never opens.
- After every discard has settled, S closes the descriptor and takes SH (bounded) with admission still closed. Only then does it open admission.

### Bounds and verdict

- PRD A's caps, as measured by its shadow acceptance, now also cover settlement.
- On SIGTERM or deadline, no new tree starts and in-flight discards are joined within a short budget. Otherwise the process exits non-zero.
- The verdict counts trees deleted, not would-delete, and reports bytes freed as `measured`, `partial` or `unknown`.

### Records

- **ADR-1783.** Pod-epoch exclusion becomes the writer-exclusion proof for settled trees on Docker-wired hosted workers. It covers the immediate predecessor, intervening pods and concurrent workers only, with the owner-accepted older-pod residual. It does not claim that every historical writer is excluded, and #1858's prohibition stays in force everywhere else.
- **ADR-1786.** Add the narrow automatic-disposal exception that #3329's maintainer decision allows.
- **ADR-3153.** "Automatic retention deletion stays off" gains the epoch exception.
- **`specs/human.md` and `docs/run-recovery.md`.** Update the #1858/#1860 lines and "Retained sources with no open hold", then run `task docs:sync`.
- **#3329's trigger.** Replace it with M0 Q7's text.

## Testing decisions

Worker tests, Linux two-process where the lock is involved:
- the deletion authority, one test per refusal;
- ledger `retired`;
- a partial failure;
- an uncertain outcome at the deadline, which exits;
- S SIGKILLed while an `rm` child holding the descriptor is alive, after which T stays blocked until that child exits;
- run spawns never inherit the descriptor;
- a fake daemon proves the deletion pass still sends only GET requests;
- the #1858 pending-create control (`agent/test/clone-attempt-paths.test.ts`) keeps its sentinel-preservation assertion.

## Milestones (one run, one PR)

- **M1: Delete at a verified epoch.** Everything above. Blocked by: PRD A's shadow acceptance and #2852 C.

### Delivery, acceptance and closure

The PR references #3329 and #1860 and closes neither. A linked `acceptance` issue, owned by the user, covers a reclaiming roll: trees deleted versus kept, bytes freed, and no held tree touched. #3329 and #1860 close after this acceptance and #3329's amended acceptance pass.

## Decision Log

| Date | Decision | Why | Rejected |
|---|---|---|---|
| 2026-10-24 | Deletion as its own PRD after PRD A's shadow acceptance and #2852 C. | Owner-approved split. The shadow data verifies that epochs actually qualify, and it measures the caps. | Shipping deletion with the lock in one PR. |
| 2026-10-24 | Pass the lock descriptor to every mutating child; unsettled work holds the lock. | libuv opens descriptors close-on-exec, and a helper can outlive the parent. | Assuming inheritance; exempting git metadata children. |
| 2026-10-24 | A reclaim `quiesce` that never calls `teardownDocker`. | `teardownDocker` deletes containers, and reclaim sends the daemon only GET requests. | Reusing `withRetainedSourceBoundary`. |
| 2026-10-24 | Re-evaluate each tree's authority under EX immediately before deleting it. | The shadow list was computed earlier in the same pass. Re-reading keeps hold currency tied to "under EX, before admission". | Trusting the earlier list. |
