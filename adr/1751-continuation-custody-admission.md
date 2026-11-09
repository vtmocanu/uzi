# ADR-1751: A run resuming its own custody is exempt from custody-count admission

**Status**: Accepted (issue #1751, amends PRD #1296 D2/D4); admission accounting amended by [ADR-2445](2445-custody-admission-accounting.md); D2a's uncharged reset qualified by the [2026-11-04 amendment](#amendment-2026-11-04--witnessed-continuation-departures-are-charged-3859)
**Date**: 2026-09-26
**Deciders**: Vlad Mocanu (maintainer approval of the admission-policy change) + agent team

## Decision (summary)

**Continuation may create another generation hold, but is exempt from custody-count admission.**

A queued run with `claim_generation >= 1` that still has at least one owner-scoped `open` custody hold for that exact run, and fewer than the limit of its own, is admitted by `ClaimRun` even when its owner's `admission_counted_holds` is at or above the custody hold limit (`workersvc.custodyHoldLimit`, 8). A fresh run (`claim_generation = 0`), a previously claimed run whose holds are all settled, or a run that itself holds the limit's worth of open holds uses the ordinary owner admission gate. ADR-2445 separates that gate's admission count from total open custody; this exemption's per-run bound still uses total open holds.

## Context

PRD #1296 D2/D4 pause code-run admission for an owner once they hold `custodyHoldLimit` unresolved (`state = 'open'`) custody holds. Each claim of a code-publishing run on a recovery-capable worker opens a hold for the new generation, and the older generation's hold stays open until it is proven settled (#1346, #1582). When one event interrupts several in-flight runs (a node disk-pressure eviction took two busy hosted workers on 2026-09-26), the runs that re-claim first double their holds and can push the owner to the cap. The runs still waiting to resume are then refused admission for **their own** work: the admission count included the candidate's own hold, and it stayed queued with the custody-limit health reason until the owner discarded an unrelated hold by hand.

## The decisions

### D1: The exemption is the only change to the claim

Only the custody-cap clause of `ClaimRun` (`api/internal/store/queries/runtime.sql`) gains the `claim_generation >= 1 AND EXISTS (own open hold) AND own open holds < limit` disjunct (D2a). `status = 'queued'`, affinity and draining-claimant rules, `fn_worker_can_claim`, the completion-interlock and Codex clauses, and the released-incarnation fence are unchanged, so cancelled, completed and paused runs stay ineligible and no worker can claim anything it could not claim before. The exemption releases nothing: every hold, and every last copy of unpublished work, is kept (PRD #1296 D2 — no release without proof).

Rejected: subtracting the candidate's own holds from the owner count. It fixes the observed total of exactly 8, but not the case where unrelated holds alone reach the cap.

### D2: Health and the owner aggregate read the same predicate

`GetCustodyAdmissionForRun` returns the owner's total `open_holds`, `admission_counted_holds` and the same `continuation_exempt` expression. Under ADR-2445, claim and health compare the admission count to the limit, and `GetCustodyAggregateForOwner.blocked_runs` excludes exempt queued runs. Owner-at-limit health and Slack episode crossing/re-arm also use admission accounting with the configured heartbeat cutoff. The continuation bound remains the exact run's total owner-scoped open-hold count.

### D2a: The exemption is bounded per run

A worker that claims a run and never reports `running` has the run swept back to `queued` (`SweepClaimedNeverStarted`) without a requeue charge and without settling the new hold. Unbounded, the exemption would let that loop add one hold per sweep forever, where the owner cap used to stop it. So the exemption holds only while the run's own open-hold count is below `custodyHoldLimit`; once a single run holds the limit's worth, it faces the owner cap again and surfaces the custody-limit reason. The same bound is in all three predicates.

Exact rule: a queued run with `claim_generation >= 1` bypasses owner-count admission only while it has 1 to 7 owner-scoped open holds of its own; at 8 or more it is admitted exactly like a fresh run. Remedy for a run blocked at its own bound: settle or recover its older holds where possible (the capture/publication release, #1582, or D4), or, after assessing the work each one protects, explicitly discard them through the owner-discard path (#1342 / #1318). No separate constant and no "started executing" distinction: the complexity is not worth a case this rare. Accepted 2026-09-26 by the maintainer's delegates (the landing session and its Codex peer), as a narrowing of the maintainer's original "always resume" decision.

### D3: No strict ceiling

The owner hold count can exceed the limit (by design, for continuations), and #1318 already records that concurrent claims can overshoot it. ADR-2445's `STABLE` accounting function uses a statement snapshot without owner serialization; concurrent claims and later heartbeat staleness can also put admission-counted holds above the limit. The limit remains an admission gate for new work, not a transactional ceiling.

### D4: Live same-worker predecessor settlement (second milestone)

`POST /api/worker/runs/{id}/recovery-holds/{holdID}/settle-live` releases an older-generation hold while the successor is still live. It is a variant beside the completed-run path (#1582), which keeps its `completed` predicate.

- **Proof.** The api derives the target itself: `refs/uzi-checkpoints/<checkpointBranch(kind, run, issue)>` (read via the new forge `RefHead`) for issue, self_improve and prompt runs (the worker does not yet request it for prompt; deferred, #4116), or `runs.branch` for task runs, the only kind whose branch is set at creation. It reads the head once and requires the published, source and adopted SHAs to be ancestors; recovery-capture source binding applies as in #1582. A `published: false` publish never lands a SHA there, so it can never prove anything. Unknown, erroring or missing answers retain.
- **Same worker only.** The hold's `original_worker_id` must be the caller. Cross-worker predecessors stay held.
- **Fencing.** The release is one guarded UPDATE that re-asserts the live status allowlist, `claim_generation`, `worker_id`, an unreleased claim, the open hold, and the target coordinates captured before the forge call (`runs.branch`, plus kind and issue for a checkpoint). It stamps `release_evidence = 'live_ancestry'` with the audit columns and `release_target` (migration 00255, draft number).
- **Idempotency before eligibility.** A hold already released under the identical identity is answered `released` with no forge call and no write, whatever the run's state now. The completed `/settle` recognises a matching `live_ancestry` release the same way, without comparing its pushed head, so a lost acknowledgement never strands the worker's journal and pins.
- **Durability backstop.** The checkpoint ref is deleted when the run ends, so a checkpoint proof is time-limited. The release is still safe because it is same-worker only and the successor's own claim-time hold stays open; its recovery archive treats only the fresh default-branch tip as public, so checkpoint-only commits stay in custody.
- **Worker.** A confirmed publish records a live leg (its exact request, MAC-covered) on the hold's settlement record and sends it in the background. Every read-modify-write on a settlement record runs under a queued per-hold lock and re-reads first; a sent leg is never replaced and survives completion, supersede and newer-generation adoption until answered; the candidate and recovery evidence (source/adopted/pushed pins, the predecessor's recovery pin and journal) is removed only after an exact `released` (except the transient predecessor recovery pin, which may be removed after a durable archive upload and authenticated local `uploaded` write; separate source/adopted/pushed settlement pins and both journals remain until release), under the lock, with the settlement record last; only the leg's own `published` pin is dropped when a leg is cleared or superseded unsent. Each live request is bounded by 10 s, which is the most it can delay a same-hold completion step.
- **Rollback.** A worker older than this change reads a settlement record that carries a live leg as a MAC mismatch and refuses it (fail-closed): those holds wait for the owner.

## Consequences

- An owner's open-hold count can grow past the limit while interrupted runs resume, bounded by D2a to the limit per run. Old generations are still cleaned up by capture/publication release, the completed-run ancestry settlement (#1582), and the live same-worker settlement (D4).
- A run whose every hold is settled is treated like a fresh run at the limit.

## Amendment 2026-10-20 — the limit is configurable (#3150)

The limit this ADR names as the constant `workersvc.custodyHoldLimit` (8) is now the admin setting `custody_hold_limit`, default 8, 1 to 1000. The "1 to 7 ... at 8" rule above is the default-8 case: a previously claimed queued run bypasses owner admission while it has 1 through limit-1 of its own open holds and loses that exemption at the limit. The rationale, the three-predicate bound and the remedies are unchanged. See the [ADR-2445 amendment](2445-custody-admission-accounting.md#amendment-2026-10-20--configurable-custody-admission-limit-3150).

## Amendment 2026-11-04 — witnessed continuation departures are charged (#3859)

D2a's "without a requeue charge" no longer holds for every claimed-never-started run. A retained continuation (a claimed run with an owner-scoped `open` hold of an earlier generation) whose first running report is refused could otherwise loop: uncharged reset, exempt reclaim, another generation hold, until the per-run bound, with no requeue accounting and no owner signal. Migration 00404 and the claimed-never-started sweep now split that cohort, in one transaction locking workers, then runs, then cross-checks.

- **Departure witness.** An accepted heartbeat or claim snapshot (never the register snapshot) that omits the run at its current generation stamps a departure, but only after an earlier witnessed listing: a prior snapshot row for the run at that generation, or registration evidence that the worker's first running report records (written even when the guard refuses the report; feature `run_registration_evidence_v1`). The stamp is bound to claim generation, register nonce and snapshot epoch; the sweep accepts it only for the worker's current nonce, an epoch not above the accepted one, and no active-snapshot row at that generation.
- **Charged arm.** A witnessed continuation past the claim grace is requeued with `requeue_count + 1`, worker affinity and the claim kept, and the Codex claim capability revoked. Once the episode's `RUN_MAX_REQUEUES` budget is spent it parks at `recovery_wait` / `worker_requeue_exhausted` with the classifier's evidence and the claim released; it is never failed, and the existing owner Resume/Cancel applies.
- **Bounded witness wait.** A continuation with presence under the worker's current registration (registration evidence at the current nonce whose epoch is at or ahead of the worker's accepted snapshot epoch, i.e. still pending, or a non-pending active-snapshot row at its generation, on an online worker) and no departure stamp has its reset deferred until `claimed_at + 2 × ClaimGrace`. Past that it parks at the new server-only cause `recovery_wait` / `retained_continuation_unwitnessed`: claim released, Codex capability revoked, custody, sources and journals untouched, no retry timer, `requeue_count` unchanged. The evidence records which presence held, the epochs and the deadline. Only owner Resume or Cancel leaves it. Resume starts a new episode that keeps `worker_id` and the released incarnation, so ClaimRun's released-incarnation fence (PRD #1497 D19) admits only that worker after its own re-registration, or a different worker once the original worker's claim affinity lapses or it goes stale; a re-registration is not a quiescence proof, source admission stays fail-closed and a repeated refusal is bounded again in the new episode; [ADR-2445](2445-custody-admission-accounting.md) D2's attention exception protects its custody. Migration 00404 Down refuses while any run is parked there.
- **Everything else** (fresh runs, runs with no earlier-generation open hold, cross-check children, Codex policy holds, and a run carrying a pending server-stamped stop such as an owner cancel taken with a live poller) keeps D2a's uncharged reset. A pending stop must stay claimable so the reclaimed flight observes the original cancel input; a charge or park would release the claim and leave the cancel undelivered. The exemption covers every non-empty `stop_kind`, while the worker acts on it at the held-predecessor seam only for a deliverable owner cancel that #3860's `heldSeamSyncSafe` admits; any other pending stop (auto-stop, interactive stop, or a cancel the seam cannot act on) keeps the uncharged loop and is bounded only by D2a. A cancel stamped just after a sweep transaction parked the run (the live-poller check is not taken under the row lock) lands on the parked run and needs a second owner cancel. Narrowing the exemption to the deliverable-cancel predicate is deferred.
- **D2a stays the backstop.** The per-run custody bound still stops the loop where neither arm can act: a worker predating registration evidence that never lists the run, a re-registration between refusals (the nonce no longer matches), and a custody limit below the requeue budget, and a pending stop the held seam does not act on. The run then waits `queued` at the custody-limit reason.

**Why.** Serves the specs/human.md Feature #3859 requirement: repeated refusal of a retained continuation reaches a bounded, truthful nonterminal outcome without fabricating departure or discarding custody.

**Rejected.** Deferring for as long as presence holds: an unbounded wait with no owner signal. Charging or parking as departed without a witness: it would label a departure the server never observed.
