# ADR-2445: Separate custody admission capacity from total open holds

**Status**: Accepted (approved #2445 M1 policy; amends ADR-1296 admission accounting and ADR-1751 admission references)
**Date**: 2026-10-07
**Issue**: [vtmocanu/uzi#2445](https://github.com/vtmocanu/uzi/issues/2445)

## Context

A claim opens a generation-scoped custody hold before execution. Counting healthy live protection as unresolved recovery capacity can block new work while the owner has no recovery decision to make. The hold still protects the source and must remain open. [ADR-1296](1296-durable-run-recovery.md) records the original total-open-hold policy; its admission accounting is superseded here, with custody durability and cleanup safeguards retained.

## Decision

### D1: Fixed limit, separate counts

`workersvc.custodyHoldLimit` stays fixed at 8. `open_holds` remains the owner's total `state = 'open'` count. `admission_counted_holds` is that total minus at most one qualifying hold per run. New code-run admission compares the latter to 8. This is owner-scoped accounting, without fleet percentages or a new configuration knob.

### D2: Exact healthy live claim evidence

`fn_custody_admission_count(owner_id, heartbeat_cutoff)` is the canonical SQL accounting function. It considers owner-scoped open holds, qualifies them first, then deterministically excludes one per run ordered by `created_at`, then `id`. Qualification requires:

- An existing run with the exact hold `run_id` and owner, and `live_run_id = runs.id`.
- An unreleased claim (`claim_released_at IS NULL`) and `hold.generation = runs.claim_generation`.
- `live_worker_id = runs.worker_id`, with an existing worker belonging to the same owner.
- A non-null worker heartbeat `>= heartbeat_cutoff`, inclusively. Callers derive the cutoff from their observation time minus the configured `WorkerHeartbeatStale` duration.
- Run status in the closed allowlist `claimed`, `running`, `awaiting_approval`, `awaiting_input`, `awaiting_followup`.
- No owner decision according to the canonical custody classifier.

Old or future generations, null or missing identity evidence, owner/run/worker mismatches, stale or null heartbeats, released claims, terminal runs and statuses outside the allowlist remain counted. Additional qualifying holds on the same run remain counted too. Unknown evidence does not free capacity.

`fn_custody_attention` retains discarded/released precedence, then classifies an OPEN hold for `recovery_wait` / `worker_requeue_exhausted` as `needs_action` when its latest capture failed, otherwise `source_only`, before considering available unguarded archives or captures in progress. This narrow cause-specific exception preserves the owner decision even with a downloadable archive or a latest preparing/uploading capture. Other holds retain available unguarded archive, capture in progress, `needs_action`, nonterminal run, then `source_only` precedence. Archive availability remains independent of attention; it does not prove latest-work coverage or automatically release exhaustion custody. `fn_is_decision_attention` identifies `needs_action` and `source_only`; those decision holds remain counted. Eligibility does not use `attention == 'active'`: that label alone proves neither exact live identity nor heartbeat freshness, and qualifying non-decision holds can have other attention labels. The shared `recovery_custody_hold_facts` view supplies listing/decision facts using that classifier. SQL is the sole policy authority; API and clients consume its facts rather than reclassifying attention.

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
