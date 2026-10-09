# ADR-2169: Dedicated worker cross-check lane

**Status**: Accepted (implementation); hosted acceptance pending
**Date**: 2026-10-08
**Issue**: [vtmocanu/uzi#2169](https://github.com/vtmocanu/uzi/issues/2169)
**Extends**: [PRD #2169](../prds/2169-cross-check-lane.md), D1–D8, and [ADR-2149](./2149-cross-check.md). The PRD stays in `prds/` until hosted acceptance completes.

## Context

A waiting lead retains its run slot. With a run cap of one, placing its checker on the same run lane requires another worker or pod. Raising run capacity also admits more leads and adds shared memory pressure. Dedicated capacity lets a compatible worker check its own lead.

## Decisions

### Separate pool, shared claim machinery

The agent has independent run, cross-check and chat pools. `WORKER_CROSS_CHECK_SLOTS` defaults to 1 and accepts 0–16; invalid agent values fall back to 1. New agent registration always sends `max_cross_check_slots`, including zero; only a positive setting advertises `cross_check_lane_v1` and polls `?lane=cross_check`. Registration and controller configuration reject out-of-range values.

The lane selects children through `ClaimRun`, then uses the existing claim assembly and exact-generation finish paths (`api/internal/workersvc/service.go`, `claim_recovery.go`). Docker eligibility, capability/model checks, snapshot/overflow exclusion, claim generation, credential epochs/revocation and custody remain at their existing boundaries.

The transaction locks and re-reads the worker in a **separate statement before ClaimRun** (`GetWorkerForUpdate`, also in `ephemeral_lease.go`). Under READ COMMITTED the later statement sees occupancy committed by a competing claimant while the lock waited. A worker lock inside the selection statement would retain that statement's earlier snapshot and could over-admit. Dedicated lane paths explicitly set READ COMMITTED.

`fn_cross_check_child_eligible` (migration 00313) centralizes child placement for claim, health and provisioning, with other fences at each caller. Plan-stage run-lane fallback requires both a NULL slot cap and no lane capability. Explicit zero, positive slots without capability, or capability without slots cannot silently use fallback. Legacy children still consume run slots.

### Placement and lifetime

Creation pins a generation-zero child to the lead's `worker_id`. Selection orders own-first, then priority and creation time, with same-user fallback after `WORKER_AFFINITY_GRACE` (default 2m). Cordoning admits a worker's pinned child; maintenance fencing and local quarantine remain authoritative.

An ephemeral worker admits its bound parent's child or the child it was provisioned for. Its parent's lease binding survives checker claims even through legacy fallback. Active children block lease entry, teardown and reaping. An eligible own lane with a free slot suppresses an extra pod; full or unsupported capacity retains existing capability-gap/saturation policy and provisioning caps.

### Durable lane history: additional column beyond the plan

`runs.cross_check_lane` records active dedicated occupancy. Queuing must clear that bool, including when an older API writes the row. The bool alone therefore loses the lane selection needed when recovery restores the **same claim generation**.

Migration 00313 adds `cross_check_lane_generation bigint` beyond the originally planned occupancy bool. Its trigger retains the historical generation through queuing, restores dedicated accounting on same-generation recovery independently of current worker advertisements, and clears/replaces history when the generation changes. This mandatory recovery fix prevents a recovered checker being charged to the run lane. A generation-zero placement pin remains a placement hint, not proof of prior custody.

Run load excludes dedicated occupancy and includes legacy run-slot children. The active-snapshot live allowance adds valid advertised lane slots to run cap + 2; the absolute entry ceiling still applies (`active_snapshot.go`).

### Operator controls and visibility

Hosted chart `workers.crossCheckSlots` defaults to 1 and renders quoted `UZI_WORKER_CROSS_CHECK_SLOTS` even at zero. The controller strictly validates 0–16 and relays it through `RenderConfig` to `WORKER_CROSS_CHECK_SLOTS`. Changes alter the desired spec hash and use existing rolls, without increasing preset resources. Compose exposes the worker variable directly.

Owner/admin DTOs separate `active_runs` / `max_concurrent_runs` from `active_cross_checks` / `max_cross_check_slots`. Web shows `1/1 runs` and `1/1 cross-checks`; CLI uses `RUN SLOTS` and `CROSS-CHECKS`. Draining includes checker occupancy, including active checks after cap re-registration to unknown/disabled. Fleet run-slot labels retain run-lane meaning. Checker children remain hidden from the Runs list.

## Proof limits and acceptance

The shipped family direction is Claude lead → Codex plan checker. Codex-lead checking (#2460) and Code cross-check remain outside this implementation.

The existing checker exposes read-only tools and required checkout-rooted Landlock confinement with private writable grants. This does not prove network isolation or eliminate same-uid process residuals. Hosted uid splitting depends on the configured profile. Lead and checker share pod memory/CPU and OOM risk; default-one headroom is unmeasured. See [worker setup](../docs/worker-setup.md#cross-check-slots) and ADR-2149 for confinement limits.

The [PRD validation record](../prds/2169-cross-check-lane.md#validation-provenance) records lead-observed gates, LiveDB, mutation and offline-render evidence. These prove the exercised boundaries, not hosted deployment, host uid containment or model/network behavior. Hosted acceptance may follow merge under D2 and remains unchecked.

## Rollback

Drain active checker children before reverting lane-aware services/workers. Take and retain a database backup before migration and before destructive rollback, including worker cap data and lane history accumulated after migration if it must be recovered.

Migration 00313's DBA Down path drops the eligibility function, trigger and all three columns, irreversibly deleting stored caps, occupancy and generation history. Down removes schema; it does **not** restore deleted data. Restore required values from a retained backup with an explicit recovery plan. A mixed-image forward rollout instead keeps the narrowly negotiated legacy plan-stage fallback.
