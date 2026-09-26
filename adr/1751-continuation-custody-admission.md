# ADR-1751: A run resuming its own custody is exempt from custody-count admission

**Status**: Accepted (issue #1751, amends PRD #1296 D2/D4)
**Date**: 2026-09-26
**Deciders**: Vlad Mocanu (maintainer approval of the admission-policy change) + agent team

## Decision (summary)

**Continuation may create another generation hold, but is exempt from custody-count admission.**

A queued run with `claim_generation >= 1` that still has at least one owner-scoped `open` custody hold for that exact run, and fewer than the limit of its own, is admitted by `ClaimRun` even when its owner is at or above the custody hold limit (`workersvc.custodyHoldLimit`, 8). A fresh run (`claim_generation = 0`), a previously claimed run whose holds are all settled, or a run that itself holds the limit's worth of open holds is gated exactly as before.

## Context

PRD #1296 D2/D4 pause code-run admission for an owner once they hold `custodyHoldLimit` unresolved (`state = 'open'`) custody holds. Each claim of a code-publishing run on a recovery-capable worker opens a hold for the new generation, and the older generation's hold stays open until it is proven settled (#1346, #1582). When one event interrupts several in-flight runs (a node disk-pressure eviction took two busy hosted workers on 2026-09-26), the runs that re-claim first double their holds and can push the owner to the cap. The runs still waiting to resume are then refused admission for **their own** work: the admission count included the candidate's own hold, and it stayed queued with the custody-limit health reason until the owner discarded an unrelated hold by hand.

## The decisions

### D1: The exemption is the only change to the claim

Only the custody-cap clause of `ClaimRun` (`api/internal/store/queries/runtime.sql`) gains the `claim_generation >= 1 AND EXISTS (own open hold) AND own open holds < limit` disjunct (D2a). `status = 'queued'`, affinity and draining-claimant rules, `fn_worker_can_claim`, the completion-interlock and Codex clauses, and the released-incarnation fence are unchanged, so cancelled, completed and paused runs stay ineligible and no worker can claim anything it could not claim before. The exemption releases nothing: every hold, and every last copy of unpublished work, is kept (PRD #1296 D2 — no release without proof).

Rejected: subtracting the candidate's own holds from the owner count. It fixes the observed total of exactly 8, but not the case where unrelated holds alone reach the cap.

### D2: Health and the owner aggregate read the same predicate

`GetCustodyAdmissionForRun` returns the owner's open-hold count and the same `continuation_exempt` expression, and the health resolver shows the custody-limit reason only for a run the claim actually blocks. `GetCustodyAggregateForOwner.blocked_runs` excludes exempt queued runs. The owner-at-limit alert and Slack episode (`ListOwnersOverCustodyLimit`) are unchanged: the owner is still at the limit and fresh runs are still blocked.

### D2a: The exemption is bounded per run

A worker that claims a run and never reports `running` has the run swept back to `queued` (`SweepClaimedNeverStarted`) without a requeue charge and without settling the new hold. Unbounded, the exemption would let that loop add one hold per sweep forever, where the owner cap used to stop it. So the exemption holds only while the run's own open-hold count is below `custodyHoldLimit`; once a single run holds the limit's worth, it faces the owner cap again and surfaces the custody-limit reason. The same bound is in all three predicates.

### D3: No strict ceiling

The owner hold count can now exceed the limit (by design, for continuations), and #1318 already records that concurrent claims can overshoot it. The limit is an admission gate for new work, not a transactional ceiling on holds.

## Consequences

- An owner's open-hold count can grow past the limit while interrupted runs resume, bounded by D2a to the limit per run. Old generations are still cleaned up by capture/publication release and the completed-run ancestry settlement (#1582).
- A run whose every hold is settled is treated like a fresh run at the limit.
