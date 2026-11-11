# ADR-2445: Separate custody admission capacity from total open holds

**Status**: Accepted (approved #2445 M1 policy; amends ADR-1296 admission accounting and ADR-1751 admission references; amended 2026-11-11 by #4162, see the last amendment)
**Date**: 2026-10-07
**Issue**: [vtmocanu/uzi#2445](https://github.com/vtmocanu/uzi/issues/2445)

## Context

A claim opens a generation-scoped custody hold before execution. Counting healthy live protection as unresolved recovery capacity can block new work while the owner has no recovery decision to make. The hold still protects the source and must remain open. [ADR-1296](1296-durable-run-recovery.md) records the original total-open-hold policy; its admission accounting is superseded here, with custody durability and cleanup safeguards retained.

## Decision

### D1: Fixed limit, separate counts

`workersvc.custodyHoldLimit` stays fixed at 8. `open_holds` remains the owner's total `state = 'open'` count. `admission_counted_holds` is that total minus at most one qualifying hold per run. New code-run admission compares the latter to 8. This is owner-scoped accounting, without fleet percentages or a new configuration knob. Hold-free Plan and Code checker children (`kind = 'cross_check'`, both harness families) bypass owner custody admission independently of the continuation bound; judges and jobs gain no exemption. See [ADR-1296's checker amendment](1296-durable-run-recovery.md#amendment-2026-10-15--report-only-checker-admission).

### D2: Exact healthy live claim evidence

`fn_custody_admission_count(owner_id, heartbeat_cutoff)` is the canonical SQL accounting function. It considers owner-scoped open holds, qualifies them first, then deterministically excludes one per run ordered by `created_at`, then `id`. Qualification requires:

- An existing run with the exact hold `run_id` and owner, and `live_run_id = runs.id`.
- An unreleased claim (`claim_released_at IS NULL`) and `hold.generation = runs.claim_generation`.
- `live_worker_id = runs.worker_id`, with an existing worker belonging to the same owner.
- A non-null worker heartbeat `>= heartbeat_cutoff`, inclusively. Callers derive the cutoff from their observation time minus the configured `WorkerHeartbeatStale` duration.
- Run status in the closed allowlist `claimed`, `running`, `awaiting_approval`, `awaiting_input`, `awaiting_followup`.
- No owner decision according to the canonical custody classifier.

Old or future generations, null or missing identity evidence, owner/run/worker mismatches, stale or null heartbeats, released claims, terminal runs and statuses outside the allowlist remain counted. Additional qualifying holds on the same run remain counted too. Unknown evidence does not free capacity.

`fn_custody_attention` retains discarded/released precedence, then classifies an OPEN hold at `recovery_wait` with cause `worker_requeue_exhausted`, `worker_memory_pressure` (approved #2396 extension, 2026-10-15), `retained_recovery_exhausted` (the #3495 amendment in [ADR-1197](1197-transient-recovery-park.md)), `retained_recovery_adoption_failed` (its #3527 amendment), or `retained_continuation_unwitnessed` (migration 00404, [amendment below](#amendment-2026-11-04--unwitnessed-continuation-parks-3859)) as `needs_action` when its latest capture failed, otherwise `source_only`, before considering available unguarded archives or captures in progress. This narrow cause-specific exception preserves custody even with a downloadable archive or a latest preparing/uploading capture. For retained-recovery exhaustion it conservatively protects both timed and NULL-stamp parks for the whole `recovery_wait`; after timer promotion or owner Resume, existing archive/final-disposition proof rules apply again. Adoption-failure and unwitnessed-continuation holds have the same protection throughout their owner-only wait. Other holds retain available unguarded archive, capture in progress, `needs_action`, nonterminal run, then `source_only` precedence. Archive availability remains independent of attention; it does not prove latest-work coverage or automatically release these causes' custody. `fn_is_decision_attention` identifies `needs_action` and `source_only`; those decision holds remain counted. Eligibility does not use `attention == 'active'`: that label alone proves neither exact live identity nor heartbeat freshness, and qualifying non-decision holds can have other attention labels. The shared `recovery_custody_hold_facts` view supplies listing/decision facts using that classifier. SQL is the sole policy authority; API and clients consume its facts rather than reclassifying attention.

### D3: Continuation keeps its total-count bound

[ADR-1751](1751-continuation-custody-admission.md)'s continuation exemption is retained. A queued run with `claim_generation >= 1` bypasses owner admission while it has 1 through 7 of its own owner-scoped open holds. This per-run bound uses the total open count, not the discounted admission count. At 8 or more own open holds, the exemption is lost and the ordinary owner admission gate applies. This prevents never-started reclaims from creating an unbounded chain of exempt generations.

### D4: Accounting changes no custody disposition

Discounting a hold releases or discards nothing. Custody release still requires its existing proof or explicit owner disposition. Prune, teardown, worker deletion and release safeguards continue to inspect any relevant open holds, including those excluded from admission capacity. A capacity discount is not proof of archival durability or permission to delete local source.

### D5: Shared capacity contract and compatibility

Claim admission, queued-run health reasons, `blocked_runs`, owner-at-limit health checks and Slack episode crossing/re-arm use admission accounting with the heartbeat cutoff. `blocked_runs` retains the continuation exemption. CLI, web, health and Slack capacity displays must use `admission_counted_holds` against `custody_hold_limit` and show `open_holds` separately as total custody; decision counts retain their existing semantics.

The current-server owner aggregate emits numeric `admission_counted_holds`, including zero, alongside `open_holds`, `custody_hold_limit`, `decision_needed` and `blocked_runs`. A client decoding an older API that omits the new field falls back to `open_holds`; an explicit zero must remain zero. The CLI decoder implements that presence-based fallback.

### D6: Statement snapshot, not a strict ceiling

The SQL function is `STABLE` and the claim gate observes its statement snapshot. Admission queries evaluate the accounting function once per statement for the relevant owner and reuse that result for capacity and blocked-run facts (the aggregate uses a materialized CTE). It adds no owner serialization. Concurrent claims can observe available capacity together, and a previously discounted hold can become counted when its heartbeat goes stale. Counts can therefore exceed 8; this policy is an admission gate, not a transactional ceiling on custody or admission-counted holds.

## Delivery boundary and validation

The SQL/API/CLI/health/Slack contract and web rendering share the capacity/total split. No-action UX copy and tone belong to issue #2444 and are outside this decision's scope. The accounting tests establish admission behavior; full-stack acceptance remains the responsibility of the existing lifecycle phase.

Phase 72's seeded holds have null `live_run_id` and `live_worker_id`, so none of those seeds qualifies for the discount. Its aggregate assertions distinguish total and admission counts at the wedge and after a healthy claim. The phase remains a wedge/disposition lifecycle check, not coverage of the full classifier matrix, concurrent admission or late heartbeat staleness.

## Addendum: per-hold admission membership (#2970)

`fn_custody_admission_members` is a read-only per-hold projection that lets an owner see which holds admission counts and why. `fn_custody_admission_count` remains the canonical accounting function and was not changed. Agreement between the two is guarded by parity tests (the mirrors in `custody_admission_livedb_test.go` plus a per-reason test). The owner listing reads the hold list, membership and aggregate in one repeatable-read read-only snapshot with one heartbeat cutoff. Clients never infer membership and treat an absent `counts_toward_admission` as unknown, never as not counted. The policy remains SQL-only; this adds visibility, not authority or a change to accounting.

## Amendment 2026-10-20 — configurable custody admission limit (#3150)

[Issue #3150](https://github.com/vtmocanu/uzi/issues/3150) makes the owner admission limit an admin setting. D1's "stays fixed at 8" and its "without ... a new configuration knob" no longer hold. Everything else in D1 to D6, and the accounting, is unchanged.

**Why.** A fixed 8 blocked legitimate multi-run owners whose only safe alternatives were irreversible discards of held work or waiting for counted holds to stop counting. An admin can now size the gate to the deployment.

**Decision.**

- **D1.** The limit is the admin setting `custody_hold_limit` (`api/internal/settings/settings_custody.go`), default 8, a whole number from 1 to 1000. There is no zero-as-disable value, and the write-time validator rejects zero, negatives, non-integers, signed forms and values above the ceiling. It is one deployment-wide value applied separately to every owner; the compile-time constant is gone.
- **D3.** The continuation bound is 1 through limit-1 of the run's own owner-scoped open holds, using the configured limit; at the limit the exemption is lost.
- **D5.** Capacity displays compare `admission_counted_holds` to the configured `custody_hold_limit`. Claim admission, the continuation bound, queued-run health reasons, `blocked_runs`, the recovery aggregate, the `custody.holds` health check and Slack episode crossing/re-arm each read the setting once per claim, request or pass.
- **D6.** "Counts can therefore exceed 8" now reads "exceed the configured limit"; the policy is still an admission gate, not a transactional ceiling.
- **Fail-closed reads.** The accessor never substitutes 8 and never treats a bad value as zero. With no usable validated value (a read that fails with no cached snapshot, or a hand-edited invalid stored row), new run-lane claims are refused, queued runs show a "setting could not be read" pause reason, the owner hold listing returns an error, and `custody.holds` reports unknown. A failed refresh while a good value is cached keeps serving that value, except right after a save on that replica or right after boot, when the read fails. The dedicated cross-check claim lane is unaffected because checkers are exempt; legacy checker children claimed through the ordinary run lane wait that cycle.
- **Lowering is non-destructive.** Lowering the limit blocks only applicable future claims. It never cancels runs and never releases or discards a hold.
- **D4 is unchanged.** Prune, teardown and release safeguards still see every open hold, and the checker exemption and the statement-snapshot overshoot are unchanged.

A change takes effect without a restart: the saving replica applies it on its next read and other replicas within one settings cache TTL (`SETTINGS_CACHE_TTL`, default 5s) once their refresh succeeds. See [Admin settings](../docs/admin-settings.md#custody-hold-limit).

## Amendment 2026-11-04 — unwitnessed continuation parks (#3859)

Migration 00404 adds `retained_continuation_unwitnessed` to D2's cause-specific `fn_custody_attention` exception. [ADR-1751's 2026-11-04 amendment](1751-continuation-custody-admission.md#amendment-2026-11-04--witnessed-continuation-departures-are-charged-3859) parks a retained continuation there when it still shows presence but no departure witness after its bounded witness window: the claim is released while custody and sources are kept for the owner, with no retry timer. An OPEN hold of a run in that park classifies as `needs_action` after a failed latest capture, otherwise `source_only`, before archive readiness or capture progress, so a downloadable archive never hides the owner decision. Those holds stay admission-counted. After owner Resume or Cancel, the ordinary precedence and release proofs apply again. Why: an unwitnessed park cannot show where the latest work is, so it gets the same conservative custody as the other owner-held parks (specs/human.md Feature #3859). D3's continuation bound is unchanged and remains the backstop the ADR-1751 amendment relies on.

## Amendment 2026-11-11 — admission units and a hard per-run bound (#4162)

[Issue #4162](https://github.com/vtmocanu/uzi/issues/4162) changes what admission capacity counts and makes D3's per-run bound a hard gate. Migration 00424 (`api/internal/store/migrations/00424_custody_admission_units.sql`; the number may be renumbered at merge) implements it in SQL. D4 is unchanged, and so are D2's exclusion of at most one healthy live current-generation hold per run, the checker exemption and the Amendment 2026-10-20 settings reads.

**Revised earlier decision.** The 2026-10-30 maintainer decision on [#2969](https://github.com/vtmocanu/uzi/issues/2969) kept counting earlier generations of a run. This amendment revises it for admission capacity only. Per-hold release by proof stays #2969's goal; the predecessor FINAL pass is still disabled until #4001.

**Why (evidence observed 2026-11-10).** One run's counted disk parks produced generations 1 to 5 on one worker in about 2 hours, within [ADR-1809](1809-per-run-cache-bounds.md)'s `UZI_RUN_DISK_PARK_MAX` default of 3. With a second run's re-claim loop (#4158), the owner's counted holds reached the limit of 8 and unrelated runs stayed queued, although the work was one run on one worker.

**Decision.**

- **A-D1 (revises D1: count definition).** `admission_counted_holds` now counts admission **units**: after D2's exclusion, the remaining open holds that are group-eligible count **once** per (run, original worker); every other remaining hold counts singly. `units = groups + singletons`. A hold is group-eligible when its live run and live worker equal its own run and original worker (non-null); the run and worker exist and belong to the hold owner; the hold's repo equals the run's repo; and `1 <= generation <= runs.claim_generation`. Heartbeat freshness plays no part, so a dead worker's group is still one unit. The field name is unchanged and now means units; total `open_holds` is unchanged. Examples at limit 8: a running run with a healthy generation 5 and open generations 1 to 4 on the same worker is 1 unit; the same run cancelled is 1; only the D2-excluded healthy current hold is 0; generations 1 to 3 on worker A and 4 to 5 on worker B, none healthy, is 2.
- **A-D2 (extends D2: members listing).** `fn_custody_admission_members` reports one representative per group (oldest `created_at`, then `id`) as counted; the other members read `same_run_group` and are not counted. The addendum's parity guarantees apply to units.
- **A-D3 (revises D3: hard per-run bound).** A queued run whose own open holds (every open hold, counted or not) are at or above `custody_hold_limit` is not claimable, whatever the owner's unit count. The per-run arm is ANDed with the owner arm, no longer ORed as in [ADR-1751](1751-continuation-custody-admission.md#d2a-the-exemption-is-bounded-per-run). It is enforced in `ClaimRun`, the queued-run health reason and `blocked_runs`, which includes such a run even when its owner is below the limit. Owner Resume does not bypass it: the run is refused again and no further claim or hold is created. The owner frees it by discarding or settling that run's own holds. The park `ParkFrozenQueuedCustodyExhausted` no longer requires the owner count; every other guard on it is unchanged.
- **A-D4 (revises D5: reader contract).** Owner-level signals (the `custody.holds` health check, Slack custody episodes and their clearing, the board alert) report units. A hard-bound run below the owner limit gets per-run wording ("this run has reached its own custody hold limit; retained holds still protect its work; resolve or discard this run's own retained holds to let it start") and a per-run Slack nudge, with no owner-capacity claim. Clients keep reading `admission_counted_holds` against `custody_hold_limit` and `open_holds` as total custody.
- **A-D5 (D4 is unchanged).** Every hold stays open until released by proof or discarded. Teardown, prune, roll, recycle and release safeguards read open holds directly. Grouping releases nothing and is not proof of archival durability.
- **A-D6 (revises D6: wording).** Admission is evaluated over the claim statement's snapshot. It is an admission gate, not a transactional ceiling, and this amendment states no numerical overshoot bound.

**Rollout and rollback.** The chart runs the api as a hard singleton (replicas plus `Recreate`, `deploy/chart/templates/api-deployment.yaml`) and `store.Migrate` runs before serving (`api/cmd/server/main.go`), so old claim code stops before grouped accounting is active. An old api image's `ClaimRun` against grouped accounting would admit a run at its own limit. Before starting an old image, stop the upgraded api and run `scripts/custody-admission-units-rollback.sql`: it restores the previous function bodies and removes goose row 424. It is non-destructive (it raises admission pressure and changes no hold). Do not assume an image rollback runs the migration Down.
