# ADR-4628: Fresh resume: an owner Resume on a separate hosted worker from durable evidence

**Status**: Implemented (store, server, agent, CLI, web, docs); exact-head CI and live hosted acceptance pending. Gate `UZI_FRESH_RESUME` defaults off.
**Date**: 2026-11-25
**Issue**: [vtmocanu/uzi#4628](https://github.com/vtmocanu/uzi/issues/4628)
**Related**: [ADR-1197](1197-transient-recovery-park.md), [ADR-1296](1296-durable-run-recovery.md), [ADR-1783](1783-run-quiescence-and-attempt-clone-paths.md), [ADR-2009](2009-pending-outcome-identities.md), [ADR-2445](2445-custody-admission-accounting.md), [ADR-4001](4001-command-uid-writer-exclusion.md), [ADR-4476](4476-codex-claim-provenance.md)

## Context

A run held at a worker-exhaustion `recovery_wait` (`worker_requeue_exhausted`, `retained_recovery_exhausted`, `retained_recovery_adoption_failed`, `retained_continuation_unwitnessed`) waits for the one worker whose bare repository holds its source. When that worker is gone or unusable, the owner's only exits were to wait or cancel, although uzi may already hold durable copies of the work: a recovery capture per generation and the broker-owned checkpoint ref.

Each hosted worker has its own data PVC (`dataPVCName` in `controller/internal/kube/render.go`), so a second worker cannot read the first worker's source and has to be seeded from evidence the server holds (owner question Q13).

## Decision

An owner may Resume such a run on a **different persistent hosted worker** (W2), seeded from one piece of durable evidence the server selects and the owner confirms. It is owner-initiated only, off by default (`UZI_FRESH_RESUME`), and never deletes the original worker's (W1) source or custody. User-facing behaviour is in [docs/run-recovery.md](../docs/run-recovery.md#fresh-resume-on-a-separate-hosted-worker).

### Selection rule

- The **evidence generation** `Ge` is the highest claim generation, up to the run's latest `G`, that saved anything: a recovery capture under that generation's custody hold, or a checkpoint tip stamped with it (`runs.checkpoint_tip_generation`, valid only while `checkpoint_tip_generation_tip` equals the tip, so an unstamped rewrite reads as unbound). Generations above `Ge` are *uncovered* and listed to the owner. There is no fallback to an older generation when `Ge` has no usable candidate.
- Candidates at `Ge`: an available, manifest-bound, unexpired capture whose prerequisites the forge proves to be ancestors of the default head (or equal to the stamped tip); the checkpoint ref when the tip is stamped with `Ge`, the run has an open hold and the forge ref still equals the tip.
- **Dominance.** The chosen candidate must be the same commit as, or contain, every other candidate. If none does the result is `ambiguous`. A tip of unknown generation that the chosen candidate cannot be proven to contain makes it `unbound`.
- **Tie-break** among equally dominant candidates: a checkpoint ref beats a capture, then the lowest capture UUID.
- Forge questions are bounded (96 calls, 20 seconds). A question the budget or the forge cannot answer is unproven, never assumed (`prereq_unproven`).

### Authority chain

1. **Owner start.** The selection token (sha256 over run, `G`, `fresh_hold_version`, `Ge`, evidence identity and sha) binds the confirmation to exactly that selection. A change of any input is `stale_selection`.
2. **Idempotency.** Each confirm carries a `request_id`, unique per run (`run_fresh_resumes_run_request_key`). A known (run, request) is resolved *before* eligibility: an active request returns its row, a completed one is `request_completed`. Only that constraint maps to `request_completed`; the per-run pending/claimed unique index never does.
3. **Pending row.** Start inserts a `pending` row and writes no `runs` column; the run stays held. A pending row not claimed within 24 hours is refused by the sweep.
4. **Try-locked fresh claim.** Inside an eligible hosted destination's claim transaction, before the ordinary claim, up to three pending rows of the same owner are examined. Locks that a concurrent holder has (owner headroom, predecessor, run and transfer, capture) are skipped, never waited for.
5. **R1 headroom recheck.** The owner's custody headroom is rechecked at claim time, after every lock. A refusal writes `insufficient_custody_headroom` and persists.
6. **V1 assembly refusals.** When claim assembly cannot deliver (disabled credential, unhonorable override, Codex account hold, pool wait, terminal or transient failure), the claimed transfer is refused and the held run restored instead of the ordinary write.
7. **Claim.** `PrepareFreshResumeClaim` saves the run's pre-claim tuple in the row, then queues the run for W2 with the paused-time credit and clears of `ResumeWorkerRecoveryEpisode`. The row is `claimed` with a 2 hour seed window.
8. **Seeded receipt.** W2 reports its first fenced commit (`destination_attempt_id`, `seeded_generation`, write-once, enforced by a trigger). The receipt carries `current_authority`, judged under the run lock.
9. **L4 under authority.** The destination commits its recovery journal, ledger `live` and fresh journal `ready` in one rename under the bare lock, and starts the model, only when the receipt says it still holds the run's live claim. Every failure before the receipt abandons the transfer and stops nonterminally; the run is never reported `failed` from this window. A prior transfer is marked `superseded` when a later one seeds.

Destination sequence: the pre-L0 inventory exclusion check (`refuseExcludedKey`, under the bare lock), L0 fresh journal `seeding`, L1 ledger `fresh_pending`, L2 seed under the bare lock, L3 receipt, L4 commit. A destination whose bare already holds any local state for the branch abandons the transfer (`local_state_present`) and never seeds on top.

### Two trigger rules

The `a0_guard_fresh_resume` BEFORE UPDATE trigger on `runs` (SQLSTATE `UZ416`) validates only: it never writes another table and never rewrites `NEW`.

- **Before seeding** (a pending or claimed row): a change of `status`, `worker_id` or `claim_generation` raises `fresh resume transfer in progress`. Exceptions: the transaction opted in for the row (`uzi.fresh_resume_id`); Cancel; auto-stop (`failed` with `uzi.fresh_resume_terminal` naming the run); the claimed destination starting (`claimed`/`running` to `running`, worker and generation unchanged); and the `ON DELETE SET NULL` cascade of a deleted worker.
- **After seeding** (a seeded or superseded row, nonterminal run): moving the run to the predecessor, or to a worker holding an open hold older than `seeded_generation`, raises `fresh resume predecessor excluded`. No opt-in bypasses it.

A restore puts back the saved tuple but never `claim_generation` (it stays at the claim's `G'`), `codex_claim_epoch`, `codex_cap_hash` or claim provenance.

### Opt-in gate, lock order, rollback

`UZI_FRESH_RESUME` (default off, set uniformly on every replica) gates start and the preview; the destination's routes for a transfer in flight ignore it and the sweep drains what it no longer admits. Binary rollback needs no pending or claimed row on a nonterminal run, and the schema Down refuses while any pending, claimed, seeded or superseded row is on a nonterminal run ([Configuration](../docs/configuration.md#fresh-resume-rollback)).

Lock order for every path that refuses or restores: owner advisory lock; the saved predecessor worker `FOR KEY SHARE` (`NOWAIT` for a caller that already holds a worker row); the run; the transfer; then the opt-in. A pending row refuses with run then transfer only. The sweep takes its predecessor lock with a 2 second timeout and skips a busy row to the next tick. The destination routes take the caller's worker row, the predecessor `NOWAIT`, the run, then the transfer.

### Failure model

Process or container crash only. The fresh journal, ledger and receipt commit are written so that a crash between steps reconciles to either an abandoned transfer or a committed one. No power-loss durability is claimed: the one config rename under git's `config.lock` adds no fsync beyond the existing rewrite.

## Accepted limits

- **Forge ancestry replaces the pinned prerequisite closure.** The selection proves a capture's prerequisites against the default head instead of using pins ([#3591](https://github.com/vtmocanu/uzi/issues/3591)). The destination stays fail-closed through git's own bundle verify, and abandons the transfer when it fails.
- **An older agent build fails closed for the destination's whole bare.** The new ledger states `fresh_pending` and `fresh_unbound` are unknown to an older parser, which reads them as unknown evidence for the entire bare.
- **After a binary rollback with seeded rows,** the original worker's lane for the run stalls until the run ends; the predecessor stays excluded.
- **V1 accepted side writes.** An assembly refusal still lets `recordRunCredential` and `recordHarnessModelFallbackNote` write; neither is part of the restored tuple.
- **The fence, not snapshot witnesses, governs transfer runs before seeding.** `LockFrozenWorkerSnapshotRuns` excludes runs with a pending or claimed transfer, which keeps them out of [ADR-2009](2009-pending-outcome-identities.md)'s identity writes. #3859's departure stamp still sees them, because the stamp and the upsert take their run lists from `LockWorkerRecoveryParents`, and that stamp is loss evidence the sweep uses (`snapshot_departed`).
- **R1 disclosure.** A healthy seeded run counts one extra admission unit until its new hold is released or the run ends, because W2's hold is added while W1's stays ([ADR-2445](2445-custody-admission-accounting.md)).
- **Third worker (S6).** While W1 and W2 both hold custody, only a third eligible hosted worker can take a repeat fresh resume. After seeding W1 never takes the run again, so an owner with two workers whose W2 is lost adds a worker or cancels.
- **Stale `config.lock`.** The agent never removes one (`FreshConfigLockedError`); an operator removes it after confirming no git process runs, then Resume. Before the seeded receipt (L0 to L2) the seed's catch abandons the transfer and rethrows. At L4 (`commitFreshReceipt`) the receipt is already `seeded`, so the first flight stops nonterminally keeping the seed and custody on the new worker, and after a restart the reconciler re-runs L4 and, on the lock, takes an owner hold (`holdFreshResumeRetained(..., "config_locked")`: `recovery_wait`, `retained_recovery_adoption_failed`) on the new worker.
- **No power-loss durability** (see Failure model).

## Shipped follow-on

- **Inventory source exclusion and `handlePermanentFailure` ordering, shipped** ([#4643](https://github.com/vtmocanu/uzi/issues/4643), [#4655](https://github.com/vtmocanu/uzi/issues/4655)). `GitCache.seedFreshAttempt` calls `refuseExcludedKey` under the held bare lock after the read-only refusal checks and before the first `assertBareClear`, so a same-key retained inventory exclusion refuses (`InventorySourceExclusionRefusal`, passed through untouched) before any L0 or L1 write. `seedFresh` then waits once, outside the lock, through the runner's `awaitInventoryExclusionSettled`, and retries the seed once (the evidence is not provided again). A second refusal abandons through the fenced abandon (reason `inventory_source_exclusion`), marks the attempt `fresh_unbound` only after a closed answer, and the flight stops nonterminally (`FreshReconcileStop`, never `failed`). Cost: an exclusion that outlasts one wait consumes the transfer; the worst-case seed time, counting each attempt's authority wait (which runs before its seed deadline clock starts), is 2 x (`CLONE_AUTHORITY_WAIT_MS` + `RESTORE_SEED_DEADLINE_MS`) + `SEED_EXCLUSION_WAIT_MS`, about 1230 s at today's constants. In `handlePermanentFailure` the fresh pre-seed branch (`stage === "pre_receipt"`) sits after the #4645/#4655 `holdOrWallParkConfirmed` early return, so a confirmed hold or wall park wins; the `freshResumeHold` line stays early. The `policyHold` check that precedes the pre-seed branch is not expected to meet a pre-seed flight. Only a Codex flight writes a `policy_hold` outbox entry (`policyHoldEligible` requires `initiallyCodex`), and the fresh run base checked when the resume is requested refuses a claimed Codex-provenance run and excludes `policy_pending_fresh` and `codex_policy_hold` (migration 00535). The claim never places the transfer on the run's own or released worker. `preparePolicyAdmission` reads only this worker's own outbox after resolving its pending terminals, and `enterPolicyHold` runs only after clone setup, which for a fresh flight is after the receipt. This rests on a run's harness and Codex provenance fields never being cleared after creation; if that ever failed, such a flight would return without a `failed` report but without the pre-seed stop.

## Not shipped

- **Slice 2, automatic resume, is deferred.** [ADR-4001](4001-command-uid-writer-exclusion.md) "Coverage-based release, evaluated and not adopted" concludes no record proves a lossless cutoff, so only a human, shown the cutoff text, decides.
- **Compose destinations** and the **Codex hand-off** ([#4651](https://github.com/vtmocanu/uzi/issues/4651)).

## Consequences

An owner can recover a run whose worker is lost, accepting that edits after the saved evidence may be missing, which the owner is told before confirming. The original worker's data is kept. The cost is one more admission unit, one transfer table, three triggers (`run_fresh_resumes_write_once_trg`, `a0_guard_fresh_resume_trg`, `fresh_resume_after_trg`) and a destination state machine that must stay in step with the ledger readers (ADR-1783).
