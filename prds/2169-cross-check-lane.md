# PRD #2169: Dedicated cross-check slots on workers

**Status**: Implementation complete; hosted acceptance pending. Child 2 of 6 under umbrella #2148 (Cross-check). Built on PRD #2149 (the `cross_check` kind); naming follows its D13/D14. Retained in `prds/` until hosted acceptance completes.

Original planning facts were read at `main` `c1543616`. Implemented behavior below was checked at `3b2ab7412e6baf1cc6833a276112044b5ccc99a9`; the documentation milestone updates that tree. Documentation mirrors were synchronized with `task docs:sync` in the documentation milestone.

## Problem

Before this change, a cross-check child (PRD #2149) competed with leads on the ordinary run lane while its lead retained a slot waiting for it (PRD #2149 D4). Hosted workers default to one run slot (`UZI_WORKER_MAX_CONCURRENT_RUNS`, `controller/internal/config/config.go`), so that worker had no free run slot for its lead's child: the check needed another worker or ephemeral pod, or timed out. A fleet filled with waiting leads could strand its checks. Planned Code cross-checks (PRD #2170) would add another child stage and more pressure.

Raising the run cap does not fix it: new leads fill the extra slots too, a higher cap opens the two intra-user residuals ADR-0042 documents for concurrent runs, and #2127 recorded a hosted worker OOM-killed at its 12Gi limit during one Codex run with the consumer still unknown.

## Outcome

New workers support a separate **cross-check lane**, next to the run lane and the chat lane, with its own cap `WORKER_CROSS_CHECK_SLOTS` (default 1). A lane-aware worker claims plan checkers on that lane, preferring the lead's own worker. A compatible own worker with a free checker slot can check a Claude lead on Codex while the lead holds its run slot. Legacy workers retain narrowly negotiated plan-stage run-slot fallback. A full lane can still queue the checker. The lane is invisible in the Runs list (children are already unlisted, PRD #2149) but visible where capacity is shown.

Hosted acceptance examples (pending maintainer evidence):

1. One persistent worker, run cap 1, cross-check slots 1, runs both families. A sweep run R occupies the run slot and submits its plan. R's child is claimed by the same worker's cross-check lane within one poll; R passes and implements. No other worker is online.
2. Two leads on one worker (run cap 2, cross-check slots 1) submit plans together. One child runs, the other waits for the lane slot, then runs; neither lead's run slot is used by a child.
3. The lead's worker cannot run Codex (it does not advertise `codex_harness_v1`). After `WORKER_AFFINITY_GRACE`, another of the user's workers that can run Codex claims the child on its own cross-check lane.
4. The lead's worker is cordoned for a roll while its lead waits. The cordoned worker still claims that lead's child (it claims nothing else new), so the lead is not stranded by the roll.
5. A lead on an ephemeral worker bound to it: the same pod's cross-check lane claims the lead's child. It never claims another run's child.
6. The workers page shows "1/1 runs" and "1/1 cross-checks" for a worker busy with a lead and its child.

## Out of scope

- Raising run caps or changing pod presets. Lane capacity is measured in acceptance first.
- A durable wait that releases the lead's run slot.
- Placing a child on another user's worker. Workers are per-user, as today.
- Chat-lane changes.

## Modules and seams

### Worker (agent)

- `crossCheckClaimLoop` in `agent/src/worker.ts`, beside `claimLoop` and `chatClaimLoop` and started in the same `Promise.all`, with its own tracked pool (the `chatActive` pattern), shutdown drain, and the shared DinD-prune claim gate (`tryEnterClaim`). It claims only while its pool is below `WORKER_CROSS_CHECK_SLOTS`, and routes every claim to `CrossCheckRunner`; a non-`cross_check` claim on this lane is refused loudly and never executed. `isIdle` counts the new pool.
- Config: `crossCheckSlots: nonNegativeInt(env, "WORKER_CROSS_CHECK_SLOTS", 1, 16)` in `agent/src/config.ts`, max 16; 0 turns the lane off.
- The worker advertises the new protocol capability `cross_check_lane_v1` (registered in `api/internal/capability`) only when the lane is on, and its slot count on register (`max_cross_check_slots`, next to `max_concurrent_runs` in `client.register()`).
- `agent/src/client.ts`: `claimCrossCheck()` posts `/api/worker/runs/claim?lane=cross_check`, 204 = idle.

### Claim (api)

- `WorkerClaim` (`api/internal/handler/worker_protocol.go`) gains `case "cross_check"` in its lane switch; the default case's 400 lists the new lane. `laneWorkerRouteGuard` (`worker_lane_routes.go`) answers an isolated-lane worker 204 on it, as it does for `?lane=chat`.
- `workers.max_cross_check_slots INT NULL`, written at registration, accepted in [0, 16]; out-of-range registration is rejected. New agents send the field even at zero. Only a positive setting advertises `cross_check_lane_v1` and polls; invalid agent config falls back to 1.
- **The lane reuses the run claim and finish machinery.** `ClaimRun` (`runtime.sql`) uses a `@lane` parameter for child selection and records the selected lane's occupancy; the cross-check child retains the existing run-claim boundaries: docker-repo eligibility, the active-snapshot and overflow exclusions, the claim-generation increment and released-fence clearing, credential finishing (`finishRunClaim`), custody admission and the post-claim re-checks. `ClaimChatRun` is deliberately not the model: it skips several of these (it does not increment the claim generation). Each guard the lane inherits gets a lane-side regression test; dropping one needs a written reason in this PRD, not the child being read-only.
- **One eligibility predicate for a child**, defined once and used by `ClaimRun`, `CountOnlineWorkersClaimableForRun`, the health rungs and the provisioning queries alike. A queued `cross_check` child is claimable by:
  - (a) on `@lane = 'cross_check'`: a worker advertising `cross_check_lane_v1`, `cross_check_v1` and the child harness's capability set as `ClaimRun` already requires it, with a free lane slot (`active cross-checks < max_cross_check_slots`), subject to affinity and draining below; or
  - (b) on the run lane, **plan-stage children only**: a worker advertising `cross_check_v1` with NULL `max_cross_check_slots` and no `cross_check_lane_v1` (an older image during a roll), consuming an ordinary run slot exactly as PRD #2149 ships. Explicit zero or incomplete lane negotiation cannot use run-lane fallback. Only `stage = 'plan'` exists today (`00299_plan_cross_check.sql`, `cross_check_claim.go`); the predicate is keyed by stage so PRD #2170 can add the code stage's rules (never through (b), own worker only) without reshaping it.
- **Claim serialization and recovery accounting.** The transaction locks and re-reads the worker in a separate statement before `ClaimRun` under READ COMMITTED so a waiting claimant's selection sees committed occupancy. Migration 00313 stores the durable `cross_check_lane` bool plus an additional `cross_check_lane_generation bigint` beyond the plan: queuing clears the bool through a trigger even for older API writers, while historical generation restores same-generation recovery accounting independently of later worker advertisements. New-generation claims replace history. Valid advertised lane allowance extends the run-cap + 2 live-snapshot budget without changing the absolute ceiling. See [ADR-2169](../adr/2169-cross-check-lane.md).
- **Affinity.** The child is created (PRD #2149's create closure) at claim generation zero with `runs.worker_id` set to the lead's current worker, the column resume affinity already uses on queued runs. A plan-stage child admits that worker, or any eligible worker once `updated_at < now() - WORKER_AFFINITY_GRACE` (the chat lane's grace, 2 min), ordered own-first, then `fn_run_priority`, then `created_at`. The affinity rule is keyed by stage; PRD #2170 adds the code stage's own-worker-only, no-grace rule. ADR-216 spread deferral does not apply on the lane; affinity replaces it.
- **Draining.** The run lane's rule, `NOT @claimant_draining OR worker_id = @worker_id`: a cordoned worker admits children pinned to it, subject to maintenance fences and worker quarantine.
- **Ephemeral.** A non-ephemeral claimant passes; an ephemeral claimant admits only a child whose lead is its `ephemeral_run_id`, or the child it was itself provisioned for (`r.id = @ephemeral_run_id`).
- **Queued `worker_id` readers.** Generation-zero checker pins are placement hints, not executing ownership. Recovery and custody readers require a prior claim generation; health does not treat an unclaimed checker pin as released custody. Same-generation recovery and pinned-unclaimed snapshots are exercised by `api/internal/workersvc/cross_check_lane_recovery_livedb_test.go`.
- **Run-lane load.** Run-load predicates in `runtime.sql` and `credential_disabled.sql` distinguish dedicated occupancy: a child claimed on the lane (a) is excluded from run-lane load; a child claimed through (b) still counts as a run slot. A parity test, in the style of `runkind_sql_test.go`, enumerates those sites and fails if one excludes `chat` but not lane-claimed `cross_check` without an allowlisted reason.

### Ephemeral workers and provisioning

- `ListUnplaceableQueuedRunsForEphemeral` and `ListSaturationQueuedRunsForEphemeral` (`runtime.sql`, called from `api/internal/hostedsvc/ephemeral.go`) use the same eligibility predicate, lane capacity included. A plan-stage child triggers provisioning only when no worker can claim it through (a) or (b), and keeps PRD #2149's saturation behaviour otherwise; a child does not trigger provisioning while its lead's own worker (ephemeral or not) is eligible for it and has a free cross-check slot; otherwise it follows the existing capability-gap and saturation provisioning policy. Code-stage provisioning (never) is PRD #2170's. A pod provisioned for a plan-stage child claims it through its lane (`r.id = @ephemeral_run_id`).
- An own-parent child preserves its parent's ephemeral lease binding even on legacy run-slot fallback. An active child blocks lease entry, teardown and reaping.
- The controller strictly validates `UZI_WORKER_CROSS_CHECK_SLOTS` (default 1, range [0, 16]) and relays it through `RenderConfig` to `WORKER_CROSS_CHECK_SLOTS`. Chart `workers.crossCheckSlots` defaults to 1 and unconditionally quotes zero as an env value. Compose exposes `WORKER_CROSS_CHECK_SLOTS` (default 1). Slot changes roll through the spec hash without increasing pod preset resources.

### Health and visibility

- `health.go`'s `queuedReason` gains rungs for a `cross_check` run computed from the same predicate: no worker can ever claim it through (a) or (b) (`reasonNoCrossCheckWorker`), or every such worker is full (`reasonCrossCheckSlotsBusy`), shown on the lead as "waiting for a cross-check slot". A pre-lane worker that can take a plan-stage child through (b) counts as capable.
- API: `apitypes.WorkerDTO` exposes `active_cross_checks` and `max_cross_check_slots`. `active_runs` keeps counting the run lane, including children claimed through (b).
- Web: shared badge helpers in `web/src/lib/workerRuns.ts` show separate run/cross-check capacity, including `1/1 runs` and `1/1 cross-checks`. Existing active checks remain visible after unknown/disabled cap registration. Owner/admin DTOs expose both lanes; CLI `uzi worker list` and `uzi admin workers` use `RUN SLOTS` and `CROSS-CHECKS`, and the TUI includes both. Draining counts checker occupancy; fleet run-slot totals retain run-lane meaning.

### Isolation

A cross-check child is read-only by construction (PRD #2149 D3): no `Bash`, no write tools, the Codex checker under Landlock rooted at its checkout. (A Claude checker does not exist yet; the Codex-lead direction PRD adds it under the path guard, and its isolation note extends this one.) Checkout confinement and tool restrictions bound repository access; they do not prove network isolation or eliminate same-uid process residuals. The hosted uid split depends on the configured profile. Lead and checker share pod memory/CPU and OOM risk; default-one memory headroom is unmeasured. `docs/worker-setup.md` states this where `WORKER_CROSS_CHECK_SLOTS` is documented.

## Testing decisions

- **Claim (LiveDB via `./e2e/run-store-it.sh`):** a lane claimant gets only `cross_check` runs; the run lane never hands a child to a lane-advertising worker; a pre-lane worker still claims plan-stage children on the run lane and each one consumes a run slot; a full lane is not counted as capacity by any mirror; the inherited run-claim guards (docker eligibility, overflow exclusions, claim-generation increment and released-fence clearing, credential finishing, custody admission) each hold on the lane; affinity: own worker first, another worker only after the grace; a cordoned worker claims its pinned child and nothing else; an ephemeral worker claims its own lead's child and refuses every other child; capability mirrors for Codex and custom models agree with ClaimRun. Each predicate has a mutation that removes it and reddens a case.
- **Parity:** the `chat`/`cross_check` exclusion parity test; the queued-`worker_id` reader audit, one test per reader.
- **Worker:** the lane pool never exceeds its cap; a non-`cross_check` claim on the lane is refused and not executed; lane off (0) advertises nothing and never polls; shutdown drains the pool.
- **Provisioning:** a child does not trigger provisioning while its lead's own (ephemeral) worker is eligible with a free slot, and a plan-stage child does when that worker lacks the other family or its lane stays full; a child with no capable lane anywhere does.
- **Health and UI:** both new reasons; worker badges; `uzi worker list` column.

## Milestones

- [x] **M1: A worker cross-checks its own leads on a dedicated lane.** Agent loop, config and advertisement; `max_cross_check_slots` column and register; the lane route and the `@lane` parameter on `ClaimRun` with the shared child-eligibility predicate (affinity per stage, draining, capability and lane-capacity mirrors); the affinity hint at child creation with the reader audit; run-lane load exclusion with the parity test and the plan-stage pre-lane fallback; health rungs; API, web and CLI capacity display; `docs/worker-setup.md`, `docs/cross-check.md`, `docs/configuration.md`, `docs/cli.md` (generated mirrors synchronized with `task docs:sync`); `ARCHITECTURE.md` (worker lanes); an ADR for the lane seam (adr/2169-cross-check-lane.md); CHANGELOG. Blocked by: PRD #2149 M2. Gates: `task gate:api`, `task gate:agent`, `task gate:web`, LiveDB via `./e2e/run-store-it.sh`, `task gate:repo`.
- [x] **M2: Ephemeral and hosted.** The ephemeral lane predicate, provisioning placeability, controller env and chart value, `deploy/` docs, CHANGELOG. Blocked by: M1. Gates: as M1, plus `task gate:controller`.

No `.github/workflows/**` change in implementation or validation.

## Acceptance (hosted k8s, maintainer-owned)

Implementation may merge before acceptance under D2; the PRD moves to `prds/done/` only after maintainer-owned hosted evidence is recorded. Offline renders and local tests do not satisfy this checklist.

- [ ] Persistent cap-1 worker, ephemeral workers off: its own Claude lead's Codex plan checker claims within one poll, with no other worker online.
- [ ] Two leads on one worker: one checker slot processes their checks serially.
- [ ] Cordoned worker completes its pinned lead's check, respecting maintenance fences.
- [ ] Ephemeral lead's own pod checks it without provisioning an extra pod.
- [ ] Record peak pod memory with a lead and Codex checker together before keeping or raising default-one capacity.

## Validation provenance

These are direct lead observations supplied to the documentation milestone, not checks rerun by the documentation worker.

- M1a `4baea1e7`: `task gate:api`, `./e2e/run-store-it.sh` (2,371 top-level tests, zero skips), and `task gate:repo` passed. The 21 guard mutation groups compiled and asserted red.
- M1b `79a6ff2b`: `task gate:api`, `task gate:web` (5,891 unit / 12 Chromium), `task gate:repo`, and `task gate:agent` passed. Unsharded agent run at `UZI_AGENT_TEST_CONCURRENCY=2`: 10,332 pass, zero fail/cancel, **3 skips**. M4: 183 pass, zero skips, 37 clauses / 64 matrix cases. `./e2e/run-store-it.sh` passed at `68659396`: 2,371 top-level tests, zero skips. The subsequent `79a6ff2b` correction changed only CLI test expectations, leaving the LiveDB production source unchanged.
- M2 `3b2ab741`: `task gate:api`, `task gate:controller`, `task gate:repo`, and `./e2e/run-store-it.sh` exited 0. Skip guard recorded 2,374 top-level tests, zero skips; raw 5,447 includes nested tests. Three new DB tests had 24 passing leaves. Independent tester `1091` recorded 55 DB leaves / 29 controller cases, zero skips, and three compiled mutations red.
- Helm validation: actual offline renders with CNPG stripped passed for default 1, 0 and 16; no hosted deployment was exercised. The two API test-chain mechanical static-check fixes in `3b2` were reviewed by all roles; the original `1091` reviewer/tester/auditor reviews were clean, and committed fixed ranges received whole-unit review.
- Documentation source changes complete M1/M2's documentation scope. `task docs:sync` exited 0 and synchronized the generated `api/internal/uzidocs/embed` mirrors. Post-documentation gate results are recorded separately in the completion report.

These observations do not prove hosted uid containment, model/network isolation or hosted memory headroom. The acceptance checklist remains pending.

## Decision Log

- **D1. A separate lane with its own cap, not a higher run cap.** User decision 2026-10-03. The chat lane is the precedent (`chatClaimLoop`, `ClaimChatRun`, `WORKER_CHAT_SESSIONS`); judge and task-review claims take ordinary run slots and are not a lane precedent. A higher run cap lets leads fill the new slots, opens ADR-0042's residuals for write-capable siblings, and adds memory pressure with #2127 unexplained.
- **D2. Default one slot.** User decision 2026-10-03. A lead has at most one pending child per stage, a check takes minutes against a 30-minute deadline, and the pod's memory headroom with a concurrent Codex checker is unmeasured. Acceptance records it before any raise.
- **D3. Prefer the lead's own worker through `runs.worker_id`.** Reuses resume affinity and the draining rule (a cordoned worker re-claims only runs pinned to it) instead of a new placement column; the grace is the chat lane's, since a waiting lead needs a fast fallback, not the run lane's two-hour ceiling.
- **D4. Off the run lane only for lane-aware claimants.** A mixed fleet during a roll keeps PRD #2149's behaviour on older workers, so nothing strands while the worker image catches up (the worker pin is decoupled from app releases).
- **D5. An ephemeral worker serves its own lead's child.** Without it every ephemeral lead would provision a second pod for each check. The exception is limited to children whose lead is the pod's bound run.
- **D6. The lane is a selection parameter on `ClaimRun`, not a separate chat-style query.** A child must keep every guarantee a run claim gives today (docker eligibility, overflow exclusions, claim generation and fences, credential finishing, custody); `ClaimChatRun` skips several, so copying its shape would silently drop them.
- **D7. One child-eligibility predicate shared by claim, health and provisioning.** Separate mirrors drifted in the first draft (provisioning ignored a full lane, health ignored the pre-lane fallback); one definition, used everywhere, keeps "can this child run?" answered the same way.
- **D8. Code-stage rules belong to PRD #2170.** Buddy review 2026-10-07 at dispatch: storage and claim decoding admit only `stage = 'plan'` on main, and PRD #2170 is unfinished, so this PRD ships the plan stage and keeps the predicate and affinity keyed by stage for #2170 to extend.
