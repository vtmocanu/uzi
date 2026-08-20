# PRD #422 — Surge upgrade: release without killing in-flight runs

**Issue**: #422
**Status**: Draft
**Priority**: High
**Author**: Vlad Mocanu
**Created**: 2026-08-20

> **Self-contained for an offline worker.** This PRD is handed to a uzi sweep worker with restricted egress (forge + `*.anthropic.com` + package caches only, no open web). Every load-bearing fact below is a **codebase read**, verifiable offline against a fresh clone. File:line citations are anchored at HEAD `87aa6dd5`; line numbers drift, so re-derive by symbol/function name, not by line. No milestone here depends on the open internet. Two milestones (M6 e2e, M8) need live infra (docker / k8s) and are explicitly flagged **not** completable by the sweep worker.

---

## Problem

Cutting a uzi release today **interrupts every in-flight run and then blocks it on a human**. Two mechanisms combine:

1. **Every release Recreate-rolls the whole hosted-worker fleet at once.** The chart wires `UZI_WORKER_IMAGE_TAG` to `Chart.AppVersion` (Model B), so a release bumps the worker image tag, which changes each worker Deployment's rendered pod-template `spec.Image`, which changes the `uzi.dev/spec-hash` annotation, which makes the controller patch every hosted-worker Deployment on its next reconcile. The roll strategy is `Recreate` (delete-then-create, a hard kill), chosen to avoid RWO-PVC Multi-Attach deadlock.
   - `deploy/chart/templates/controller-deployment.yaml` (`UZI_WORKER_IMAGE_TAG: {{ .Values.workers.image.tag | default .Chart.AppVersion }}`)
   - `deploy/chart/values.yaml` (`workers.image.tag` ships `""` = "Changing the tag is what rolls every hosted worker")
   - `controller/internal/kube/render.go` (`specHash` over the rendered pod template; `spec.Image` from `preset.go` `fmt.Sprintf("%s/%s:%s", repo, image, tag)`; `strategy: Recreate`; "Decision 9 rolls every worker on every release")
   - `controller/internal/kube/materializer.go` (`if obs.Generation == w.Generation && obs.SpecHash == wantHash { return nil }`, and the explicit comment "there is deliberately NO controller-side 'is the worker busy?' check")

2. **The killed run requeues, then strands on the vault-unlock gate.** On SIGTERM the worker does abort-then-drain: it aborts each in-flight run, fetches the run's git-committed work back into its bare repo on the durable PVC, and leaves the run **non-terminal**, so the API sweeper requeues it (`status='queued'`, keeps `worker_id`/`session_id`, bumps `requeue_count`). But the per-user vault DEK lives only in API process memory and is evicted on restart, so the new API boots with every vault **locked**. The claim gate then returns idle for any locked user, so the requeued run **sits in `queued` indefinitely** — no timeout, no failure — until the user manually re-enters their vault passphrase.
   - `agent/src/worker.ts` (abort-then-drain), `agent/src/main.ts` (SIGTERM handler), `agent/src/runner.ts` (`shutdown()`, fetch-back)
   - `api/internal/store/runtime.sql.go` (`RequeueRunsOfStaleWorkers`: SET status/requeue_count/health/updated_at only, worker_id/session_id preserved; the WHERE keys on **heartbeat freshness, not status**)
   - `api/internal/workersvc/service.go` (claim gate: `if s.vlt != nil && !s.vlt.Unlocked(wkr.UserID) { return nil, nil }`, with the note "A queued run sits indefinitely by design")
   - `api/internal/vault/vault.go` (in-memory DEK cache, "lost on process restart"); `docs/vault-threat-model.md` ("gone on lock or restart"; "The DEK cache is per-process")

**Observed on the dev cluster** (meta-dev-02, OIDC/Keycloak login, no `UZI_SEED_*` wired): after every upgrade the UI asks the user for their vault passphrase, and their runs do not resume until they type it. OIDC users have no login password, so they set a dedicated vault passphrase (`api/internal/handler/vault.go` `VaultPassphrase`/`VaultUnlock`); there is no auto-unlock for any non-seed user (`api/cmd/server/main.go` boot-unlocks only the seed admin when `UZI_SEED_EMAIL`+`UZI_SEED_PASSWORD` are set — not the case here).

**Net**: you cannot cut a release while runs are active without interrupting them and forcing a manual vault unlock afterward. For a product whose whole job is running long agent jobs (RUN_TIMEOUT is 6h on this cluster, and runs can pause at the approval gate for far longer), that makes releasing a disruptive, hand-held operation.

---

## Goal

**Release the whole uzi stack (api, web, db, controller) at any time, including during active runs, without interrupting or losing those runs.** Busy workers keep running their current version until they finish, and upgrade only afterward.

### Non-goals

- **Auto-unlocking OIDC users' vaults across a restart.** Explicitly out of scope (owner decision, 2026-08-20). This PRD removes the *need* to re-unlock after an app release by keeping the worker pod alive so the run never requeues; it does not change the vault's per-process unlock model. The requeue-then-manual-unlock path remains the unchanged fallback.
- **RollingUpdate / surge pods for workers.** Workers hold RWO PVCs; two pods cannot attach at once. `Recreate` stays. "Surge" here means the *stack* surges forward while *old workers* linger, not overlapping worker pods.
- **Multi-replica API.** The vault DEK cache assumes single-replica; unchanged.

---

## Solution overview

Two independent parts, shippable in order. Part A (M1–M2) delivers most of the value on its own; Part B (M3–M6) handles the deliberate-worker-roll case.

### Part A — Decouple the worker image tag from appVersion (app-only releases stop touching workers)

Today the only reason an api/web/db/controller release rolls **worker** pods is that the worker image tag defaults to `Chart.AppVersion`. Flip that default so `workers.image.tag` defaults to a **concrete pinned worker version** independent of appVersion. Then a stack release that does not deliberately move the worker tag renders an unchanged worker `spec.Image` — and therefore an unchanged spec-hash — so the controller rolls **zero worker pods**. (The api/web/controller pods still Recreate-roll; that is the point — only workers are spared.) Running runs continue uninterrupted on the old worker, which keeps talking to the new API.

This is safe because the worker↔API protocol is already **version-agnostic and additive** (Decision 2). Two things the naive version of this misses, both real (Decisions 3 and 9): the pinned tag must be a **concrete version string** (the spec-hash is over the tag *string*, so a floating tag like `:stable` would never change it and would defeat Part B's deliberate-roll trigger), and a worker pinned *behind* appVersion can light PRD #113's "outdated" upgrade badge unless the badge/roll-health path is handled — which is why Part A is two milestones, not one.

### Part B — Bounded drain for deliberate worker-image rolls

When you *do* bump the concrete worker tag (agent-image changes, security fixes), the controller must not hard-kill busy workers. Introduce a first-class **`draining`** signal on a worker, stored in its **own column** (not the heartbeat-derived `workers.status`, Decision 7): a draining worker keeps heartbeating and finishes its in-flight runs but claims no new ones; once idle, the controller performs the `Recreate` roll. The controller learns busy/draining state over the existing poll wire and requests a cordon over a **new control-write channel** (Decision 8). A configurable per-worker **drain deadline** (default ~24h) bounds the wait — past it the controller rolls anyway and the run falls back to today's requeue-resume. An operator **force-roll** override rolls immediately.

---

## Decision log

**Decision 1 — Decouple worker version from app version (reverses Model B's lockstep).**
Model B deliberately rolls every worker on every release (`controller/internal/kube/render.go`, `api/internal/workersvc/upgrade.go`) to guarantee worker and API never skew. This PRD trades that guarantee for release-during-runs. The cost — tolerating N-1 worker↔API skew — is bounded by Decision 2 and paid down by M7's compat contract. **PRD #113 already anticipated the decoupled-tag case**: the hosted-worker upgrade badge compares against `RollSignal.RolledTag` "because values.yaml may pin the worker image independently of the api's own release" (`api/internal/workersvc/upgrade.go`). So the skew accommodation partly shipped already; this is an ADR-worthy reversal nonetheless (M8 records it).

**Decision 2 — Skew is already tolerated by the protocol; make it a maintained contract, not an accident.**
Verified offline: the worker reports its version three ways (register body, heartbeat body, `X-Client-Version` header — `agent/src/client.ts`) and the API **never gates on it** — it is sanitized and stored for a display-only upgrade badge (`api/internal/handler/worker_protocol.go` "accepted, ignored"; `api/internal/workersvc/upgrade.go` `ClassifyUpgrade`, persisted nowhere). `api/internal/middleware/worker_auth.go` has zero version references (join-token auth only). The worker has no pg/DB dependency (`agent/package.json`), parses every response with a plain structural cast (no zod/runtime schema — `agent/src/client.ts`), and the API decodes worker *requests* with `DisallowUnknownFields`, which only rejects a *new* field from a *new* worker to an *old* API (not our direction). So an old worker against a new API is safe **by convention**. M7 turns convention into a guardrail.

**Decision 3 — The pinned worker tag must be a CONCRETE version, not a floating channel.**
The spec-hash is computed over the rendered pod template, in which the image is the literal `repo/name:tag` **string** (`controller/internal/kube/render.go` `specHash`; `controller/internal/preset/preset.go` builds the string). A floating tag (`:stable`, `:latest`) never changes that string, so repointing it would roll **nothing** — silently defeating Part B's deliberate-roll mechanism. The deploy default must therefore be a concrete `vX.Y.Z`-style worker version that an operator bumps to trigger a roll. M1 also resolves the **publish/advance story**: `.github/workflows/release.yml` publishes the worker image tagged by appVersion on each release, so the per-version image already exists; decoupling only changes which concrete tag the *deploy default* points at and makes advancing it a deliberate step (documented in M8's runbook).

**Decision 4 — Drain deadline default 24h, configurable, with force-roll override.**
A run's own execution is capped at `RUN_TIMEOUT` (6h on meta-dev-02 per the ArgoCD values; 2h default in `api/internal/config/config.go`). The unbounded part is the approval gate (`awaiting_approval`/`awaiting_input`) where a run waits for a human. 24h comfortably covers "6h run + overnight review". It must be configurable (chart value / controller env) and overridable by an operator force-roll so a critical fix is never stuck behind an idle-but-approval-parked worker. Past the deadline the controller rolls and the run takes today's requeue path (no committed work lost).

**Decision 5 — Reuse the existing active-run predicate, not a new busy computation.**
There is already an api-side active-run gate for worker *deletion*: `DeleteWorker` returns `ErrWorkerHasActiveRuns` (409) when a worker has active runs (`api/internal/handler/workers.go`, `api/internal/workersvc/service.go`). Drain reuses the same "does this worker have active runs?" predicate. Note `ListHostedWorkersForController` does **not** currently select any run/busy data (`api/internal/hostedsvc/service.go` reads only token/size/generation columns), so exposing busy-ness to the controller requires a new correlated subquery/count over `runs` — that is real work, scoped in M3.

**Decision 6 — `draining` stops claiming via the existing claim gate.**
The claim path already has two de-facto "stop claiming" levers (the vault gate and the per-worker concurrency cap, `api/internal/workersvc/service.go`). Add `draining` as a third: a draining worker's claim call returns idle. This keeps the worker process alive and finishing in-flight runs while taking on nothing new.

**Decision 7 — `draining` is an ORTHOGONAL column, never a `workers.status` value.**
`workers.status` is a two-value **heartbeat-derived liveness** column: it is written `'online'` on *every* register and *every* heartbeat (`api/internal/store/queries/runtime.sql` `RegisterWorker`/`HeartbeatWorker`) and swept `'offline'` only *from* `'online'` (`MarkStaleWorkersOffline`). A draining worker **must keep heartbeating** (Decision 8 rationale below), so a `draining` value placed in `status` would be clobbered back to `'online'` by the very next heartbeat and never observed by the claim gate. Draining is orthogonal to liveness, so it needs its own column (e.g. `draining_since timestamptz NULL`). Ripple queries that filter on `status='online'` must be updated to keep counting a draining-but-live worker: `MarkStaleWorkersOffline` (so a draining worker whose pod actually dies is still swept offline), the online-count query, and the per-user concurrency-cap query. **Invariant to state in code and tests**: a draining worker keeps heartbeating; because `RequeueRunsOfStaleWorkers` keys on heartbeat freshness (not status), a heartbeating draining worker is safe from mid-drain requeue.

**Decision 8 — Cordon is a NEW controller→API control-write channel, fail-safe, with a bounded blast radius.**
Today the controller only *polls* desired state (`hostedsvc.Poll`) and *pushes* display-only roll-health (`POST /api/controller/status`, whose error is deliberately swallowed — "an observability feature must never take down the thing it observes"). Cordoning a worker is a **control mutation** of desired state, a different trust/failure class: it needs a new authenticated endpoint + handler + apiclient method + its own wire contract (the status wire is golden-pinned with `DisallowUnknownFields`, `api/internal/hostedsvc/status_wire_contract_test.go`). It must **fail safe**: if the controller cannot read busy-ness or cannot write the cordon, it **defers the roll** (assume busy) rather than proceeding. Blast-radius note: this widens the controller's authority (a compromised controller could cordon the whole fleet), so scope the endpoint's auth to exactly the cordon action. M4 also fixes the now-stale `controller/internal/reconcile/reconcile.go` package doc ("It tells the api nothing"), already false since the Reporter landed.

**Decision 9 — Part A must not regress PRD #113's upgrade badge on meta-dev-02.**
The hosted-worker badge compares the worker's version against `RolledTag`, but only when a **fresh** controller roll-health signal exists (`api/internal/workersvc/upgrade.go` `signalFresh`, 60s TTL); with no fresh signal it falls back to `CPVersion` (= appVersion), and a worker pinned *behind* appVersion then classifies `outdated` and enters the nav attention set (`InUpgradeAttentionSet`). On meta-dev-02 the controller SA is currently **denied `list pods`** (`controller/internal/kube/materializer.go` note), so it emits no roll-health rows → no fresh `RolledTag` → a pinned-behind worker reads `outdated` fleet-wide. M2 resolves this: either grant the controller the `list pods` RBAC so it emits roll-health (fresh `RolledTag`), or teach the badge to treat an intentionally-pinned worker tag as up-to-date. This is a prerequisite for Part A, not a follow-up.

---

## Milestones

Each milestone includes its own tests. "Offline-verifiable" flags checks a restricted-egress sweep worker can actually run (Go/TS unit gates, `helm template` parse); "needs live infra" flags checks requiring docker (throwaway Postgres) or a real k8s cluster, called out explicitly so a run never claims them green.

- [ ] **M1 — Decouple worker image tag from appVersion (flip the default).**
  The tag is *already* independently pinnable (`deploy/chart/values.yaml` ships `workers.image.tag: ""`); the delta is flipping the **default** off `Chart.AppVersion` to a concrete pinned worker version (Decision 3), in `deploy/chart/templates/controller-deployment.yaml` and `values.yaml`, with docs for how the tag is advanced (the per-version image already ships from `release.yml`). Success: a `helm template` render (offline harness in the style of `scripts/assert-chart-render.sh`, which uses the `helm` binary — present in this env) shows the **controller** Deployment's `UZI_WORKER_IMAGE_TAG` env **unchanged** across an appVersion-only bump when the worker tag is pinned, and changed only when the worker tag moves. **Do not** cite `controller/internal/kube/render_test.go` for this — that test computes the spec-hash from the *controller's* config and never sees appVersion (which is chart-only); the relationship is a chart-render assertion. `controller/internal/preset` is **not** in scope (it maps template→image name, orthogonal to the tag default). *Offline-verifiable* (helm template parse; needs the `helm` binary).

- [ ] **M2 — Preserve PRD #113's upgrade badge under a decoupled tag (Decision 9).**
  Ensure a worker intentionally pinned behind appVersion does not read `outdated` fleet-wide. Resolve via the controller `list pods` RBAC (chart RBAC + `controller/internal/kube/materializer.go` roll-health path, so a fresh `RolledTag` is emitted) and/or badge logic in `api/internal/workersvc/upgrade.go` (`ClassifyUpgrade`/`InUpgradeAttentionSet`) that treats an intentional pin as up-to-date. Success: unit tests over the badge classifier prove a pinned-behind worker is not flagged `outdated` when the pin is intentional; the RBAC change renders in the chart. *Offline-verifiable* (Go unit for the classifier; chart render). The live RBAC effect on-cluster is human-validated (M8).

- [ ] **M3 — Worker `draining` state + busy/draining on the poll wire (api/db, atomic cross-module).**
  This is the foundational data change and it lands in **one MR** because the wire is golden-pinned: (a) new goose migration adding an orthogonal `draining` column to `workers` (drafts as `00137_*`; live head is `00136_then_fix.sql`; renumber above the live head at merge per strict-goose convention, `api/internal/store/migrate.go`) — **not** a `workers.status` value (Decision 7) — plus the ripple updates to `MarkStaleWorkersOffline`, the online-count query, and the concurrency-cap query; (b) the claim gate returns idle for a draining worker (`api/internal/workersvc/service.go`, alongside the vault gate and concurrency cap); (c) the drain **lifecycle**: who sets `draining` (M4's cordon write) and **who clears it after a roll** — a rolled pod re-registers under the same hosted id, and `RegisterWorker` does not touch the flag today, so an explicit clear-on-roll transition is required or a worker stays cordoned forever; (d) expose `busy` (active-run count via a new correlated subquery in `ListHostedWorkersForController`, Decision 5) and `draining` on the poll DTO — which is declared in **both** `api/internal/hostedsvc/protocol.go` `DesiredWorker` **and** `controller/internal/protocol/protocol.go` `DesiredWorker`, pinned by the shared wire golden `api/internal/hostedsvc/testdata/controller_poll_wire.json` (`api/internal/hostedsvc/wire_contract_test.go` + `controller/internal/protocol/protocol_contract_test.go`, both `DisallowUnknownFields`); update both structs, the `Poll` builder, and the golden together. Fakes to update are the **hostedsvc store fake** (`ListHostedWorkersForController`) and the claim-gate fakes — **not** the six `forge.Forge` fakes (unrelated). Success: unit tests prove the claim gate idles a draining worker while its in-flight run heartbeats/reports/completes, the DTO carries the new fields, and the wire golden round-trips in lockstep across both modules. **Needs live infra** for the migration/CHECK-acceptance and any `*LiveDB` store test (throwaway Postgres via `./e2e/run-store-it.sh` / `UZI_TEST_DATABASE_URL`); the sweep worker runs the pure-unit subset and reports which store tests it could not execute.

- [x] **M4 — Controller cordon-write + defer-roll (Decision 8).**
  Landed: new authenticated control-write `POST /api/controller/workers/{workerID}/drain` (`api/internal/handler/controller_cordon.go` + route in `handler.go`), backed by `hostedsvc.Service.Cordon` and the idempotent `CordonHostedWorker` query (`api/internal/store/queries/hosted_workers.sql`, sqlc-regenerated). Controller side: `apiclient.Client.RequestDrain` (fail-safe — every non-2xx incl. 404 is an error so the caller defers) satisfies the new nil-safe `kube.Cordoner`, injected into the materializer (`New` gains the param; `main.go` passes the apiclient). `reconcileWorker`'s drift path now cordons+defers a busy worker instead of hard-killing it and rolls only once idle; the stale `reconcile.go` package doc is corrected. Tests: handler contract (401/204/404/400/500), `hostedsvc` Cordon unit tests, `apiclient` RequestDrain tests, and `materializer_test.go` table cases (drift+busy⇒cordon+defer, drift+busy+draining⇒defer, drift+idle⇒roll, cordon-error⇒defer, nil-cordoner⇒defer, no-drift⇒nothing). Depends on M3. *Offline-verifiable* (controller + api unit tests).
  Original scope: Add the new authenticated controller→API endpoint (handler + apiclient method + wire contract) to request `draining`, distinct from the display-only status Report. In `controller/internal/reconcile` + `controller/internal/kube/materializer.go`: on spec-hash drift for a worker the API reports **busy**, cordon it (call the new endpoint) instead of rolling, and perform the `Recreate` roll only once the API reports it **idle**. Fail safe: if busy-ness is unreadable or the cordon write fails, **defer** the roll. Fix the stale `reconcile.go` package doc. Success: `reconcile_test.go`/`materializer_test.go` table tests show drift+busy ⇒ cordon+no-roll, drift+idle ⇒ roll, cordoned-then-idle ⇒ roll next tick, and cordon-write-fails ⇒ defer. Depends on M3. *Offline-verifiable* (controller + api unit tests).

- [ ] **M5 — Drain deadline + operator force-roll (Decision 4).**
  Configurable per-worker drain deadline (default ~24h) and a force-roll override (chart value + controller env + controller-visible signal). Past the deadline, or on force-roll, the controller rolls regardless of busy-ness and the run takes the existing requeue path. Success: unit tests cover deadline-not-reached (defer), deadline-exceeded (roll), force-roll (immediate); chart exposes the knobs with documented defaults. Depends on M4. *Offline-verifiable* (unit + chart render).

- [ ] **M6 — N-1 worker↔API compatibility contract (Decision 2).**
  Turn "safe by convention" into a guardrail: (a) an old-worker-image ↔ new-API skew check in the CI/e2e harness — CI is **GitHub Actions** (`.github/workflows/{ci,e2e,kind-smoke,release}.yml`), **not** `.gitlab-ci.yml` (which does not exist) — asserting an old worker can claim, run, report, and complete against a freshly-migrated new API; and (b) a lightweight guard flagging a migration that removes/renames a column a worker-facing endpoint still reads (additive-only discipline during a release window). Success: the skew check runs in Actions and fails if a worker-request/response contract breaks. **Partially needs live infra**: the migration-additivity check and DTO-shape unit tests run offline; the full old-image↔new-API e2e needs docker and is CI/human-validated (the sweep worker reports it not run here).

- [ ] **M7 — Docs, ARCHITECTURE, ADR, release runbook (docs only).**
  New `adr/0422-decouple-worker-version.md` (reverses Model B's lockstep — Decision 1). Update `ARCHITECTURE.md` (worker lifecycle / hosted workers), `deploy/README.md` and the chart value docs (worker-tag + drain knobs), `.claude/rules/stack.md` (the drain/cordon model, the Model-B note, **and fix its own stale `.gitlab-ci.yml` reference** — CI is GitHub Actions), the root `CLAUDE.md` notes describing Model B rolling every worker, and `.claude/agents/release.md` (how to cut an app-only release vs a deliberate worker-image roll, and the force-roll escape hatch). Success: docs build clean (`web/scripts/check-docs.mjs`); the ADR captures the tradeoff. *Offline-verifiable* (docs build). <!-- check-docs:ignore-path: adr/0422-decouple-worker-version.md is created by this PRD's M7, does not exist yet -->

- [ ] **M8 — CLI surface for drain/cordon/force-roll (code) OR explicit deferral.**
  Per the repo convention that new functionality checks `api/cmd/uzi/` (the CLI is a second API consumer), a `worker drain`/`cordon`/force-roll verb is **code + wire + tests**, not a docs task. Either add it (with its own tests) or explicitly defer it in the ADR with a reason. Do not let M7's docs build imply this is validated. *Offline-verifiable if built* (Go unit); listed separately from M7 so its status is honest.

- [ ] **M9 — Live validation on the dev cluster (human-run, NOT the sweep worker).**
  Prove on meta-dev-02: (1) an app-only release rolls zero **worker** pods and does not interrupt an active run or prompt for vault unlock; (2) a deliberate worker-tag bump cordons a busy worker, lets its run finish, then rolls it; (3) the drain deadline and force-roll behave; (4) no `outdated` badge regression (Decision 9). **The offline sweep worker cannot complete this** — leave it unchecked and note it requires cluster access.

---

## Parallelization plan

| Phase | Milestone | Depends on | Files (mostly) | Parallel with |
|---|---|---|---|---|
| 1 | M1 decouple tag default | — | chart templates/values | M2, M3, M6(offline half) |
| 1 | M2 badge/RBAC | — | `workersvc/upgrade.go`, chart RBAC, `materializer.go` roll-health | M1, M3 |
| 1 | M3 draining + poll wire | — | `store/migrations`, `workersvc`, `hostedsvc` (both DTOs + golden + `controller/internal/protocol`), fakes | M1, M2 |
| 2 | M4 cordon-write + defer | M3 | new endpoint/handler/apiclient, `reconcile`, `materializer.go` | — |
| 2 | M5 deadline + force-roll | M4 | controller config, chart values | (after M4) |
| 1–2 | M6 compat contract | — (validates M1/M3) | `.github/workflows/`, migration guard | M1, M3 |
| 3 | M7 docs/ADR/runbook | M1–M6 | `adr/`, `ARCHITECTURE.md`, `docs/`, `.claude/` | M8 |
| 3 | M8 CLI verb (or defer) | M3–M5 | `api/cmd/uzi` | M7 |
| 4 | M9 live validation | M1–M8 | none (cluster ops) | — |

**Phase 1 (parallel)**: M1, M2, M3, and M6's offline half touch mostly-disjoint files. Note M3 is itself cross-module (api + `controller/internal/protocol` + golden) but does **not** touch `controller/reconcile`, so it stays parallel to nothing controller-behavioral. **Phase 2 (sequential)**: M4 needs M3's wire fields + draining lifecycle; M5 needs M4. **Phase 3**: M7/M8 after code settles. **Phase 4**: M9 is human-run.

---

## Success criteria

1. An app-only release (no worker-tag move) rolls **zero worker pods** — provable by a `helm template` diff on the controller Deployment's `UZI_WORKER_IMAGE_TAG` env (offline) and observed on-cluster (M9). (api/web/controller pods still roll; that is expected.)
2. A running run survives an app-only release with **no requeue, no vault-unlock prompt, and no lost work** — valid precisely because the **worker pod survives** (not merely because the run is past claim): the vault gate is claim-only (`service.go` claim path), but any worker *restart* re-registers and requeues the held run, re-hitting the gate. So the invariant is "the worker pod is not rolled," which M1 guarantees for app-only releases.
3. A deliberate worker-image roll **cordons a busy worker, finishes its run, then rolls** — up to a configurable deadline (default ~24h), with an operator force-roll override.
4. Past the deadline (or on force-roll), the run falls back to the **existing** requeue-resume, losing only uncommitted worktree changes (unchanged from today).
5. An old worker can claim, run, report, and complete against a freshly-migrated new API — asserted by a GitHub Actions skew check (M6).
6. No `outdated` upgrade-badge regression from an intentionally-pinned worker tag (Decision 9, M2).
7. The release runbook and ADR document the new model and the app-only vs worker-roll distinction.

---

## Risks & mitigations

- **`draining` in `workers.status` would be silently clobbered by heartbeats** (the sharpest design trap): mitigated by Decision 7 (orthogonal column + ripple queries + the keep-heartbeating invariant). The unit tests must exercise register→heartbeat→claim, not just a run-level heartbeat, or the flaw ships green.
- **The busy/draining poll field is an atomic cross-module change**: both `DesiredWorker` structs + the shared golden + the `ListHostedWorkersForController` query land in one MR (M3), or the `DisallowUnknownFields` contract tests redden.
- **Cordon widens the controller's blast radius**: Decision 8 (new scoped control endpoint, fail-safe to defer-roll, tight auth).
- **`draining` never cleared after a roll** ⇒ worker cordoned forever: M3(c) specifies the clear-on-roll transition.
- **Skew breaks a worker mid-run despite Decision 2**: M6's skew smoke test + additive-migration guard; keep the test real (claim→run→report→complete), since a shape-only check misses a semantic change to an existing endpoint.
- **Part A lights the `outdated` badge on meta-dev-02** (controller denied `list pods` ⇒ no fresh RolledTag): Decision 9 / M2.
- **Unbounded drain wait** (approval-parked worker holds the old image): the deadline + force-roll (Decision 4).
- **Workers never get security fixes if the tag never moves**: the runbook (M7) makes "bump the concrete worker tag" a normal step; the deadline bounds exposure once a roll starts; force-roll handles urgent CVEs.
- **A node drain / OOM / eviction during the locked-vault window still strands a run** exactly as today (Success Criterion 2 only covers a controlled app-only release). Out of scope; the requeue-then-unlock fallback is unchanged.
- **The offline sweep worker cannot complete M6's e2e, M9, or the live half of M2/M3**: all flagged; the worker lands the offline-verifiable slices, runs the gates it can, and reports exactly which checks it could not execute (never "green" for an unrun slot — repo rule).

---

## Validation strategy

- **Offline (sweep worker can run)**: Go unit tests for M2 (badge classifier), M3 (claim gate idles draining; wire golden round-trip — the pure-unit parts), M4/M5 (controller `reconcile_test.go`/`materializer_test.go` table tests), via the repo's `task` targets; `helm template` render assertions for M1/M2/M5 (`helm` binary present); the additive-migration guard + DTO-shape tests for M6; docs build (`web/scripts/check-docs.mjs`) for M7; CLI unit tests for M8 if built. Read the gate honestly per `.claude/rules/*` — `task` reports 201 on failure, read the named failing test, lift the lint ratchet caps before quoting counts.
- **Needs live infra (human/CI)**: M3's migration/CHECK-acceptance and any `*LiveDB` store test (docker Postgres via `./e2e/run-store-it.sh`); M6's old-image↔new-API e2e (docker); the live RBAC effect in M2; and all of M9 (real k8s roll behavior on meta-dev-02). Marked explicitly so a run does not claim them done.

---

## References (all offline-readable in a fresh clone)

- Worker lifecycle / recovery: `agent/src/worker.ts`, `agent/src/main.ts`, `agent/src/runner.ts`, `agent/src/git.ts`; `api/internal/sweeper/sweeper.go`, `api/internal/workersvc/service.go`, `api/internal/store/runtime.sql.go`, `api/internal/store/queries/runtime.sql`.
- Vault: `api/internal/vault/vault.go`, `api/internal/handler/vault.go`, `api/internal/handler/oidc.go`, `api/cmd/server/main.go`, `docs/vault-threat-model.md`, `docs/oidc.md`.
- Controller / render / roll / poll wire: `controller/internal/reconcile/reconcile.go`, `controller/internal/kube/render.go`, `controller/internal/kube/materializer.go`, `controller/internal/kube/rollhealth.go`, `controller/internal/config/config.go`, `controller/internal/preset/preset.go`, `controller/internal/protocol/protocol.go`, `controller/internal/protocol/protocol_contract_test.go`, `controller/internal/apiclient/client.go`.
- Hosted service / upgrade badge: `api/internal/hostedsvc/service.go`, `api/internal/hostedsvc/protocol.go`, `api/internal/hostedsvc/wire_contract_test.go`, `api/internal/hostedsvc/status_wire_contract_test.go`, `api/internal/hostedsvc/testdata/controller_poll_wire.json`, `api/internal/workersvc/upgrade.go`.
- Worker↔API protocol: `agent/src/client.ts`, `api/internal/handler/worker_protocol.go`, `api/internal/middleware/worker_auth.go`, `api/internal/workersvc/claim.go`.
- Release model / chart: `deploy/chart/Chart.yaml`, `deploy/chart/values.yaml`, `deploy/chart/templates/controller-deployment.yaml`, `scripts/assert-chart-render.sh`, `deploy/README.md`, `.github/workflows/release.yml`, `.claude/agents/release.md`.
- Migrations: `api/internal/store/migrations/` (live head `00136_then_fix.sql`), `api/internal/store/migrate.go` (strict goose), `00020_workers_runs.sql` (the `workers.status` liveness column precedent — read its own comment that busy is not stored).
- Cluster config (not in this repo; context only, do not require): the ArgoCD values live in the private `meta-manager-argo` repo (`apps/uzi/values/meta-dev-02.yaml`) — `workers.enabled: true`, `RUN_TIMEOUT: 6h`, no `UZI_SEED_*`, OIDC login.
