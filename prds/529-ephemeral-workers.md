# PRD #529: Auto-provision ephemeral workers on unmet capability

**Status**: Planned (queued for the `Planned` uzi sweep)
**Priority**: Medium (the issue is explicitly "not near-term"; see Risks)
**Depends on / relates to**: PRD #84 (capability-aware scheduling — closed, designed the gate to support this as a mode), PRD #58 (hosted k8s workers — closed, shipped *static* provisioning only and left spawn-on-demand + teardown as explicit non-goals), PRD #422 (worker roll/drain), issue #79 (ephemeral-worker cost / nix-store GC — the cost tradeoff, resolved in-body below), issue #91 / [ADR-91](../adr/0091-runner-cross-run-persistence-residual.md) (the runner cross-run/cross-repo executable-persistence residual this feature structurally closes — a run-bound worker has its own fresh `-nix`/`-data` PVCs, so there is no shared `/nix` or provisioning HOME to plant into).

## Problem

When a run needs a capability no online worker has (today the vocabulary is `{docker, jvm}` — `api/internal/capability/capability.go`), PRD #84 blocks it and offers a remediation spectrum (Decision 9): **(a)** switch to an eligible worker you already have, **(b)** provision a *persistent* hosted worker (shipped, PRD #58), **(c)** auto-provision an *ephemeral* worker for just this run, then tear it down (deferred — this PRD).

Only (a) and (b) exist. So a user whose run needs `docker` (or `jvm`, or a bigger size) must keep a persistent capable worker running, paying for idle capacity between the rare runs that need it. Option (c) removes that: the capable worker appears exactly when a run needs it and disappears when the run finishes.

## What already exists (verified against the tree 2026-08-22)

This feature is **almost entirely api-side**. The two-agent codebase survey found the controller needs **zero changes for teardown**, and every provisioning primitive already exists.

**Controller is a pure desired-state reconciler (no change needed to create or tear down):**
- The `workers` table IS the desired-state store; a hosted worker is a `workers` row with `kind='hosted'` (migration `api/internal/store/migrations/00066_hosted_workers.sql:24-26`, constraint `ck_workers_hosted_metadata:34-37`). There is **no** separate "desired worker" table.
- The controller polls the full fleet every 10s (`GET /api/controller/poll` → `PollResponse{Workers []DesiredWorker}`, `api/internal/hostedsvc/protocol.go:40-95`), sourced by `ListHostedWorkersForController` (`api/internal/store/queries/hosted_workers.sql:6-45`) = every `kind='hosted'` row.
- Presence in the poll ⇒ the controller creates Deployment + PVCs + Secret (`controller/internal/kube/materializer.go:484-652`). **Absence** of a previously-stamped worker ⇒ `teardown` deletes the Deployment + Secret + all PVCs (`controller/internal/kube/materializer.go:709-725`, Pass 3 at `:466-479`). So **dropping a `workers` row is the entire teardown primitive** — no ownerRef, no TTL, no controller code to add.
- `Busy` (holds ≥1 non-terminal run) is already computed api-side in `ListHostedWorkersForController` (`hosted_workers.sql:31-33`, predicate `status NOT IN ('completed','failed','cancelled')`) and surfaced on `DesiredWorker.Busy`. A run reaching terminal flips `busy` to false on the next poll — the natural "safe to reap" edge.

**Provisioning primitives already exist:**
- `CreateHostedWorker` (`hosted_workers.sql:78-98`) is **deliberately UNGUARDED** — "the quota decision belongs to the caller" (its own comment) — so an internal auto-provisioner can call it under a different (or no) quota policy. Paired with `hostedsvc.SealJoinToken` in one transaction (`api/internal/handler/hosted_workers.go:219-283` is the persistent-path template to mirror).
- Capability → template mapping is a code constant: `templateCaps` (`capability.go:75+`, `{"base":{}, "jvm":{jvm}}`); `docker` maps to the `docker_enabled` / `DesiredWorker.Docker` dimension, not a template. `capability.Unmet(...)` (pure, offline) already computes the exact unmet set.
- The run's needs are already stored and inferred: `runs.required_capabilities text[]` (migration `00142`), `runs.size_class text` (`s`/`m`/`l`, migration `00145`) — and `size_class` lines up 1:1 with `workersize.Names` (`api/internal/workersize/workersize.go` = `["s","m","l"]`). **Note: there is no `runs` column named `n`** — that is a Go local for a worker *count*; the size is `size_class`.

**The trigger point already exists:** the pre-claim "no online worker can satisfy this" detector `queuedReason` (`api/internal/workersvc/health.go:504-536`) calls `CountOnlineWorkersSatisfyingCaps` (`runtime.sql:2522-2536`, same effective-caps fold as `fn_worker_can_claim`, excludes draining workers) and returns `reasonNoEligibleWorker` when the count is 0. Today it produces a *display reason*; this PRD turns the same condition into an *action*.

**The one real gap = ephemerality:** no `ephemeral` marker or owning-run FK on `workers`; no auto-provision loop; no run-completion → teardown trigger; no orphan GC. That gap is this PRD.

## Solution overview

A background control loop in the api finds **unplaceable queued runs** (0 online workers satisfy their capabilities) belonging to users who opted into ephemeral auto-provisioning, and, under a per-user concurrent-ephemeral cap, provisions **one run-bound ephemeral hosted worker** per such run. The worker boots, registers, claims its run (steered by soft affinity), executes, and on the run's terminal transition the api **deletes the worker row** — the controller reaps the pod with no controller change. An orphan/failure reaper covers the runs that never claim, the provisions that never boot, and the cancellations.

**Scope decision — pre-claim trigger only (Path 1).** PRD #84 Decision 9 defines two dispatch paths. This PRD implements **Path 1** (pre-claim: a queued run nothing online can satisfy). **Path 2** (a run that planned on a capable-to-plan worker and hit the approve-time capability `409` from `capabilityGate`, `service.go:5099-5111`) requires the *automatic requeue-to-a-capable-worker* machinery that PRD #84 M4 **explicitly deferred**; wiring ephemeral provisioning into Path 2 is a follow-on, noted in Risks, not in this scope. Path 1 is the common case (`docker`/`jvm`/size known from the repo hint or a prior plan) and needs no new requeue machinery.

**Flag-off, opt-in, capped — by design** (see the cost tradeoff under Risks). Nothing auto-spawns unless a user turns it on, and never more than a small cap of concurrent ephemeral workers per user.

## Design decisions

1. **The `workers` row is the desired-state unit; ephemerality is two columns on it, not a new table.** `ephemeral bool NOT NULL DEFAULT false` + `ephemeral_run_id uuid REFERENCES runs(id)` (nullable; the run this worker exists to serve). This keeps the controller's poll/reconcile/teardown untouched — an ephemeral worker is an ordinary hosted row that the api happens to create and delete on its own schedule.

2. **Teardown is "drop the row", triggered by the owning run reaching terminal — never while non-terminal.** The api deletes the ephemeral row on the terminal writers (`SetRunCompleted`/`SetRunFailed`/`CancelRunServerSide`/`CancelRunByWorker`, `runtime.sql`), i.e. exactly when `busy` would flip false. The controller's existing Pass-3 teardown then removes Deployment + PVCs + Secret. This reuses the safety the persistent `DeleteWorkerForUser` path already has (it 409s while a non-terminal run is held) — the ephemeral path simply keys deletion off the terminal transition so the pod is always idle when reaped.

3. **Resolve `{template, size, docker}` from the run's stored requirements — deterministic and offline.** `template` from `capability.Unmet` ∩ template-derived caps (`jvm` → `jvm`, else `base`); `docker` = `docker ∈ required_capabilities`; `size` = `run.size_class` clamped to a valid `workersize` name (default `m` when blank, matching the preset default). A pure function, unit-tested with no DB.

4. **Steer the spawned worker's run onto it with soft affinity — accept the residual race.** At provision the worker id is known (`CreateHostedWorker RETURNING`); set `run.worker_id` to it and refresh `updated_at` so the affinity clause in `ClaimRun` (`runtime.sql:581-583`, `worker_id = @worker_id OR updated_at < @affinity_cutoff`) makes the ephemeral worker the exclusive claimant during the grace window. There is **no hard pin** in the schema (confirmed), so a sibling *could* claim after the grace lapses; the orphan reaper (Decision 6) makes that self-correcting rather than trying to add hard pinning.

5. **A dedicated background loop, flagged off, capped per user, idempotent.** A new pass (wired beside the existing `hostedsvc.ExpirePendingTokens` loop at `api/cmd/server/main.go:540`) selects unplaceable queued runs, and for each — when the feature flag is on for that user and the user is under the concurrent-ephemeral cap — provisions one worker. Idempotency: a run that already has an ephemeral worker (`ephemeral_run_id = run.id`) is skipped, so a slow boot never double-spawns.

6. **Orphan / provision-failure GC is a reaper over the same two columns.** An ephemeral worker is deleted (row → controller reaps) when: its `ephemeral_run_id` run is terminal or absent; OR it never reached `status='online'` within a provision deadline (reusing the pending-token-expiry model, `ExpirePendingHostedWorkerTokens`, `hosted_workers.sql:177-206`); OR it has sat idle with no owned non-terminal run past a short grace (the sibling-stole-the-run case from Decision 4).

7. **Cost is accepted, documented, and defended by the flag-off default — not solved here (issue #79).** Every ephemeral worker pays a fixed cold-start: a flat 20Gi `-nix` PVC plus a `-data` PVC (5/10/20Gi by size) provisioned and bound, a `seed-nix` init container copying the baked ~2.6GiB nix closure into the PVC (`render.go:898-922`), then pod boot + register before the run can claim. That is real per-run overhead and it is **size-independent** (the nix PVC is flat). Whether ephemeral-per-run beats a small warm pool is a genuine product tradeoff, not a fact to look up; v1 resolves it by shipping **off by default, opt-in, capped**, and defers the warm-pool alternative to an issue-#79 follow-on. This keeps the PRD internet-independent — the cost facts above are all in-repo/measured.

## Technical design

### 1. Schema + model (M1)
- Migration: add `workers.ephemeral bool NOT NULL DEFAULT false` and `workers.ephemeral_run_id uuid REFERENCES runs(id)` (nullable). A partial index `WHERE ephemeral` mirrors `idx_workers_hosted`. `sqlc generate` locally; regenerate `hosted_workers.sql.go` / `models.go`.
- `resolveEphemeralSpec(run) → (template, size string, docker bool, err)` in `api/internal/capability` (or a small new file), pure, unit-tested (`go test`), reusing `capability.Unmet` + `templateCaps` + `workersize.Valid`.

### 2. Auto-provisioner + trigger loop (M2, M3)
- New store query: unplaceable queued runs = queued runs whose owner opted in, that have `required_capabilities` no online worker satisfies (`CountOnlineWorkersSatisfyingCaps == 0`), and that have **no** existing `ephemeral_run_id` worker. Live-DB tested.
- `CreateEphemeralHostedWorker`: a `CreateHostedWorker` variant (or the same query with the two new columns set) run inside a tx with `SealJoinToken`, mirroring `provisionHostedWorker` (`hosted_workers.go:219-283`) but **without** the user quota check — instead a **concurrent-ephemeral cap** (count of that user's live ephemeral workers) and the feature flag.
- Set `run.worker_id` + bump `updated_at` in the same tx (Decision 4).
- Loop wired at `cmd/server/main.go` beside `ExpirePendingTokens`.

### 3. Teardown on terminal (M4)
- On each terminal writer, if the run has a bound ephemeral worker, delete that `workers` row (a new `DeleteEphemeralWorkerForRun` query, idempotent). The controller reaps within one poll. Add a live-DB test asserting the row is gone and (via a fake/observed poll set) would drop from `ListHostedWorkersForController`.

### 4. Orphan / failure GC (M5)
- Reaper pass (same loop as M2): delete ephemeral rows per Decision 6. Reuse/extend the token-expiry deadline knob for the never-booted case.

### 5. Config + chart + surfacing (M6, M7)
- New env/flags: feature enable (per-user setting + a global admin gate, matching `KeyCapabilityAwareScheduling`), `UZI_EPHEMERAL_MAX_PER_USER` (cap), `UZI_EPHEMERAL_PROVISION_DEADLINE`. Rendered in `deploy/chart/templates/api-deployment.yaml` / `values.yaml` (api-side; **no controller change, no `.github/workflows` touch**).
- CLI parity (`api/cmd/uzi`): `uzi worker list` marks ephemeral workers; the queued-run reason surfaces "auto-provisioning a worker". Docs: `specs/ai.md`, `ARCHITECTURE.md` (Agent-runtime/worker section), a user-facing scheduling doc; drop #529 from any "not yet in scope" list.

## Milestones

- [ ] **M1 — Ephemeral worker model + spec resolver.** Two `workers` columns (`ephemeral`, `ephemeral_run_id`) + partial index; `sqlc` regen; pure `resolveEphemeralSpec(run)` unit-tested. **Success:** given a run with `required_capabilities={docker}`, `size_class=l`, the resolver returns `{template: base, size: l, docker: true}`; `{jvm}` → `{template: jvm, ...}`; blank size → `m`. `task gate:api` green.
- [ ] **M2 — Auto-provisioner + trigger loop (flagged, capped, idempotent).** The unplaceable-queued-run query + `CreateEphemeralHostedWorker` (tx with `SealJoinToken`, no user quota, concurrent cap + flag) + the background pass. **Success (live-DB):** with the flag on and a base-only fleet, a queued run needing `docker` provisions exactly one ephemeral hosted row bound to it; a second loop tick provisions none (idempotent); with the flag off, none; past the cap, none.
- [ ] **M3 — Claim targeting.** The provisioned worker is set as the run's preferred claimant (`worker_id` + affinity). **Success (live-DB):** the run claims the ephemeral worker over an unrelated sibling within the grace window.
- [ ] **M4 — Teardown on run completion.** Terminal writers drop the bound ephemeral row; controller reaps (no controller change). **Success (live-DB):** on `SetRunCompleted`/`SetRunFailed`/cancel, the ephemeral row is deleted and no longer appears in `ListHostedWorkersForController`; a non-terminal run never triggers deletion.
- [ ] **M5 — Orphan & provision-failure GC.** Reaper for terminal/absent owning run, never-booted-past-deadline, and idle-stolen cases. **Success (live-DB):** each of the three orphan shapes is reaped; a healthy in-flight ephemeral worker is left alone.
- [ ] **M6 — Config, chart knobs, quota, cost doc.** Feature flag (per-user + admin), concurrent cap, provision deadline; api-deployment/values chart wiring; the #79 cost tradeoff written into `specs/ai.md` with the in-repo measured facts and the warm-pool deferral. **Success:** flags default OFF; `deploy/chart` renders; no `.github/workflows` file touched.
- [ ] **M7 — Tests, CLI parity, docs/specs.** Full live-DB coverage (provision/target/teardown/GC/cap/flag); `uzi worker list` marks ephemeral + queued-reason surfacing; `specs/ai.md`, `ARCHITECTURE.md`, user scheduling doc. **Success:** `task gate:api` + `gate:controller` (unchanged, must stay green) + `gate:web` (if CLI/docs touch it) green; CLI shows an ephemeral worker end to end.
- [ ] **M8 — Close out the ADR-91 residual.** Confirm each ephemeral worker gets its own fresh `-nix` + `-data` PVCs (no shared `/nix`/provisioning HOME across runs), so the runner cross-run/cross-repo executable-persistence vector recorded in [ADR-91](../adr/0091-runner-cross-run-persistence-residual.md) is structurally eliminated for ephemeral usage. If this ships with the issue-#79 shared-nix-store optimization instead of per-worker `-nix` PVCs, re-check ADR-91's `/nix` caveat rather than assuming ephemerality closed it. **Success:** the property is asserted (the render provisions per-worker PVCs — Decision 7); update ADR-91's status to note #529 as shipped and close [issue #91](https://github.com/vtmocanu/uzi/issues/91) citing the ADR.

## Risks & open questions

- **Is ephemeral worth it vs. a warm pool? (issue #79, unresolved by design.)** The cold-start cost (Decision 7) is real and size-independent. v1 does not decide this; it ships off-by-default/opt-in/capped and defers the warm-pool comparison. **Recommendation:** treat M1–M7 as delivering the mechanism, and gate any default-on rollout on real cold-start timings measured after landing.
- **Path 2 (approve-time `409`) is out of scope.** Ephemeral provisioning off the plan-gate block needs PRD #84's deferred auto-requeue machinery. Filed as a follow-on, not built here. Path 1 (pre-claim) covers the common capability-known case.
- **Soft affinity, not a hard pin.** A sibling worker can claim the run after the grace window; the orphan reaper makes this self-correcting (the now-idle ephemeral worker is reaped) rather than adding hard-pin schema. Acceptable for v1; revisit if races are observed.
- **Provision latency is inherent** (provision → PVC bind → seed-nix copy → boot → register → claim). The queued run simply waits, surfaced via its existing health reason; no new status. The provision deadline (M5) bounds a stuck provision.
- **Cap interaction with the persistent quota.** Ephemeral workers are counted by a *separate* concurrent-ephemeral cap, not the persistent per-user provision quota (`CountHostedWorkersForUser`), so an opted-in user is not blocked by having filled their standing quota. Confirm this is the intended policy at review.

## Offline-worker readiness

Authored to be implemented by an offline (gated) uzi worker in one pass. **No milestone requires an open-web lookup:** every fact is an in-repo `path:line` citation verified against the tree 2026-08-22, and the one external-sounding question (issue #79's cost tradeoff) is resolved in-body from measured, in-repo facts (Decision 7) rather than left as a task. Live-DB tests use the repo's throwaway-Postgres harness (`./e2e/run-store-it.sh`; see `.claude/rules/go.md`); `sqlc generate` is local; `task gate:api` / `gate:controller` are local.

**Workflow-scope guardrail (`.claude/rules/prds.md`):** this PRD touches only `api/`, `deploy/chart/`, `prds/`, `specs/`, `docs/`, and tests — **never** `.github/workflows/**`. Neither implementation nor validation may create or edit a workflow file (the worker PAT lacks `workflow` scope; a single touch loses the whole branch). The controller module needs no change at all.

## Decision log

- **2026-08-22 (authoring).** Two-agent codebase survey established that teardown needs **zero controller changes** (dropping a `workers` row is the whole primitive, `materializer.go:709-725`) and that every provisioning primitive already exists (`CreateHostedWorker` unguarded, `SealJoinToken`, `capability` template map, `runs.size_class` ↔ `workersize.Names`). Scoped the feature to **Path 1 (pre-claim)** because Path 2 needs PRD #84's deferred auto-requeue. Chose **two columns on `workers`** over a new table to keep the controller untouched. Resolved issue #79's cost question **in-body** as flag-off/opt-in/capped with the warm-pool alternative deferred, keeping the PRD internet-independent.
