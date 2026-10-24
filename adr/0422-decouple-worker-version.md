# ADR-422: Decouple the hosted-worker image tag from appVersion, and drain busy workers instead of hard-killing them

**Status**: Accepted (PRD #422 M1–M6 merged; M8 manual-cordon deferral superseded by the owner scope amendment below; M9 live-cluster validation remains open — see "Maintainer hand-off / follow-ups")
**Date**: 2026-08-20
**Deciders**: Vlad Mocanu + agent team (PRD #422 Decision Log)
**PRD**: [prds/done/422-surge-upgrade.md](../prds/done/422-surge-upgrade.md) — the PRD carries the full Decision Log, milestone tests, and the offline/live-infra split; this ADR carries the durable design shape and its rationale.

## Decision (summary)

Reverse Model B's worker-version lockstep. `workers.image.tag` is now a **concrete pinned version, independent of `Chart.AppVersion`** (`deploy/chart/values.yaml`, `deploy/chart/templates/controller-deployment.yaml`), so an app-only release (api/web/db/controller bump) rolls **zero** worker pods when worker rendering is also unchanged; a controller template change can still cause a paced same-image roll. Advancing the worker fleet is now a deliberate operator step: bump `workers.image.tag` to a new concrete version. When that deliberate bump does move the tag, the controller no longer hard-kills a busy worker: it **cordons** it (a new `draining_since` signal), lets it finish its in-flight run, then rolls once genuinely idle. Issue #3103 supersedes the original busy-image deadline: drift can wait indefinitely, with a global emergency force-roll escape hatch. The worker↔API protocol was already additive and version-agnostic; this PRD turns that from an accident into a guarded, tested contract.

## Context

Model B (the original chart versioning scheme, `deploy/README.md`) pins `chart.version == appVersion == the release git tag`, and the hosted-worker controller extended that lockstep to the worker image: `UZI_WORKER_IMAGE_TAG` defaulted to `.Chart.AppVersion`. That meant **every** release — including one that touched only `api`/`web` — bumped the worker image tag, which changed every worker Deployment's rendered pod-spec hash, which made the controller `Recreate`-roll (delete-then-create, a hard kill; workers hold RWO PVCs so `RollingUpdate` is not an option — see "Surge means the stack, not the workers" below) the **entire hosted-worker fleet** on its next reconcile, with no check for whether a worker was busy.

A hard-killed worker's in-flight run does not simply die: the worker does abort-then-drain (fetches committed work back to the durable PVC, leaves the run non-terminal) and the API sweeper requeues it. But the per-user vault DEK cache lives only in API process memory and is evicted on restart, so the new API boots with every vault locked, and the claim gate idles any locked user's runs indefinitely. A requeued run from a routine app release therefore **stranded on the vault-unlock gate** until a human manually re-entered their passphrase — turning every release into a disruptive, hand-held operation for a product whose job is running long agent jobs.

## The decisions

### D1 — Decouple worker version from app version (reverses Model B's lockstep)

`workers.image.tag` no longer defaults to `.Chart.AppVersion`; it is a required, concrete pinned version in `values.yaml` (shipped as `"0.48.0"`, the appVersion at the time this landed — a coincidence, not a re-coupling). The controller-deployment template wraps the value in Helm's `required`, so a blank tag fails the render loudly rather than silently emitting an unpinned image. This is a deliberate, recorded reversal of the "worker and API can never skew" guarantee Model B provided, traded for the ability to release the app stack without touching running workers. PRD #113's upgrade badge already anticipated the decoupled case (it compares against `RollSignal.RolledTag`, "because values.yaml may pin the worker image independently of the api's own release"), so the skew accommodation partly pre-existed; this ADR is what makes the reversal official.

### D2 — Skew is a maintained contract, not an accident

The worker reports its version three ways (register body, heartbeat body, `X-Client-Version` header — `agent/src/client.ts`), and the API never gates on it: it is stored for a display-only upgrade badge only. `api/internal/middleware/worker_auth.go` authenticates by join token, with zero version checks. The worker has no direct DB dependency and decodes every API response with a plain structural cast; the API decodes worker *requests* with `DisallowUnknownFields`, which only rejects a genuinely new field from a newer worker (not the N-1 direction this PRD relies on). So an old worker against a new API was already safe by convention. M6 turns that convention into a guarded contract: an additive-migration guard flags a migration that removes or renames a column a worker-facing endpoint still reads, and a `LiveDB` test asserts an old-shaped worker can claim, run, report, and complete against a freshly-migrated new API.

### D3 — The pinned tag must be a concrete version, never a floating channel

The controller's spec-hash is computed over the rendered pod template, where the image is a literal `repo/name:tag` string. A floating tag (`:stable`, `:latest`) would never change that string when repointed, silently defeating the whole deliberate-roll mechanism — the fleet would never roll no matter what the underlying image contained. `deploy/chart/templates/controller-deployment.yaml` therefore wraps `workers.image.tag` in `required` (a blank value fails the render) and `scripts/assert-worker-tag-decoupled.sh` pins the expected default (`PINNED_TAG`) as an offline, non-vacuous harness: it renders the chart three ways (appVersion-only bump, tag-only bump, both) and asserts the worker env only moves with the tag.

### D4 — Cordon-then-drain until idle, with a global force-roll override

**Policy supersession (#3103, 2026-10-18):** the original elapsed busy-image deadline is replaced by indefinite busy-drift deferral. The historical implementation/validation record below is not evidence of a current image timeout.

Rather than hard-killing a busy worker on a deliberate tag bump, the controller now requests a cordon through a new authenticated control-write endpoint, `POST /api/controller/workers/{id}/drain` (`RequireController`; `api/internal/handler/controller_cordon.go`). This is deliberately a **new, narrowly-scoped** channel, distinct from the existing display-only `POST /api/controller/status` report: cordoning mutates desired state (it sets `draining_since`), which is a different trust/failure class than an observability push whose errors are deliberately swallowed. On spec-hash drift, if the API reports an admitted worker busy, the controller cordons it and defers the roll instead of rolling immediately; it performs the `Recreate` roll only once the API reports the worker idle. Without global ForceRoll, failures reading busy-ness or writing the cordon **fail safe**: the roll is deferred rather than treating missing observation/control as permission to kill.

All actual spec-hash/generation drift — image pin, template or size — and drift-associated recycle wait indefinitely for genuine idle. This includes run-bound workers outside pacing and calls with no pacing entry or a plain drain reason. Any assigned nonterminal run blocks: queued, paused and all waits/gates count. The worker may finish its own pinned continuations/checkers, but refuses unrelated new roots, unassigned children and judge intake. An admitted busy drainer occupies pool concurrency (default one), supplies no replacement coverage, canary proof or post-promotion proof, and can hold its pool indefinitely. Unadmitted healthy peers normally stay uncordoned and claimable, except for the controller-only custody-held layout migration drain described below; there is no legacy-recycle escape; separately fenced DinD maintenance remains independent. Idle guards and custody still apply; an empty blocker list is not custody clearance. No timer automatically uncordons a worker.

`UZI_WORKER_DRAIN_DEADLINE` (chart `workers.drainDeadline`, default `24h`, unchanged positive-only parsing) now bounds only independent **nondrifted** pressure/nix-size maintenance, measured from API `draining_since`. That maintenance can interrupt busy runs and delete /data unless custody-held. Shortening the deadline no longer forces image upgrades; there is no new knob. Restoring an unchanged retained zero-replica Deployment while busy retains its existing guard.

`UZI_WORKER_FORCE_ROLL` (chart `workers.forceRoll`, default `false`) is a **global** emergency bypass of pacing, holds, incarnation proof and busy deferral for drift, plus busy deferral for independent maintenance. It can interrupt runs and lose all capacity; it is neither targeted nor a recovery guarantee. Custody-held data and independent DinD readiness, identity and pod-absence guards remain authoritative; DinD has no deadline or ForceRoll bypass. Restore `false` after the emergency. Waiting indefinitely trades upgrade/security latency and pool progress for preservation of assigned runs.

The migration exception reuses `image_roll` after ordinary admission planning;
it grants no replacement admission, reservation, canary, roll slot or pod-template
patch while custody holds. Run-bound workers remain outside migration drain
selection. A distinct live eligible uncordoned same-owner/lane sibling must
individually cover the full frozen capability set; busy is acceptable, free
slots are not required. Full-poll observed held drains of any reason (including
unrenderable targets), selected drains and possibly committed drains prevent
additional selection in that owner/lane. This is not a physical one-drain guarantee.

Reconciliation retries eligible undrained workers after registration, controller
restart or an explicit undrained poll, without a fixed time bound. Failed writes
retain uncertainty until evidence resolves it, but that uncertainty is
process-local and lost on restart. A delayed timed-out cordon can still commit
after an explicit undrained poll. The accepted controller-only scope adds no
server-side fence, schema, protocol or API product change. Retained drains can
block later ordinary rolls and remain even if the covering sibling disappears,
potentially removing all intake coverage. Registration and poll/write failures
can leave claim gaps; custody-clear ordinary release predicates can reopen
claims before a later re-drain. Worker-pin rollback retains custody refusal, then ordinary
release rules apply; deployment order is independent for this exception because
the existing reason is reused. See
[migration drain operational limits](../docs/hosted-worker-roll-verification.md#migration-drain-policy-and-operational-limits)
for operator decisions, idle terminal holds and chat continuation.

### D5 — `draining_since` is an orthogonal column, never a `workers.status` value

`workers.status` is a two-value, heartbeat-derived liveness column: both `RegisterWorker` and `HeartbeatWorker` write `status='online'` on every call, and the offline sweep only demotes `'online'` → `'offline'`. A draining worker **must keep heartbeating** — that is what keeps it counted as live and keeps its in-flight run safe from the stale-worker requeue sweep, which keys on heartbeat freshness, not status — so encoding drain state inside `status` would be clobbered back to `'online'` by the worker's own next heartbeat and never observed by the claim gate. Migration `00138_worker_draining.sql` therefore adds a standalone, nullable `workers.draining_since timestamptz` column instead: `NULL` means not draining, a timestamp is the cordon time. The claim gate (`api/internal/workersvc/service.go`) treats a draining worker as a third "stop claiming" lever, alongside the existing vault gate and per-worker concurrency cap: it scopes ordinary claims to the worker's own assigned runs, so pinned continuations can finish while unrelated new roots and unassigned children are refused; pinned checkers may finish, while judge intake stays closed. `RegisterWorker` clears automatic `draining_since` on registration when the maintenance phase permits it (clear-on-roll); requested/ready/stopping/recycling maintenance retains the drain (`api/internal/store/queries/runtime.sql`). A rolled pod re-registers under the same hosted id. Registration does not clear the independent owner manual intent introduced in D5a.

### D5a — Owner manual cordon is persistent intent (scope amendment 2026-10-22)

Implementation recorded 2026-10-24. This supersedes the manual-drain deferral
in follow-up (e), including its proposed admin scope. The owner can suspend new
intake on a persistent hosted or external worker (`ephemeral=false`) with
`uzi worker cordon <id> [-m <reason>]` and clear that intent with
`uzi worker uncordon <id>`; both accept `--json`. The Workers page offers
the same owner controls. There is no new admin write, force action or activation
flag, and no worker image/protocol prerequisite.

Manual intent is stored separately as `manual_cordoned_at`,
`manual_cordoned_by` (stable actor UUID), `manual_cordoned_by_label` (sanitized
snapshot, at most 200 Unicode code points, falling back to the UUID), and nullable
`manual_cordon_reason`. Automatic `draining_since`/reason and maintenance fields
remain independently owned. Registration, roll, restart, deadline expiry,
ForceRoll and reason-owned automatic release cannot clear manual intent.
Explicit owner uncordon or worker deletion ends it. Offline status remains a
separate liveness signal.

**Owner API contract.** `POST /api/workers/{id}/cordon` sets manual intent;
`DELETE /api/workers/{id}/cordon` clears it. Both use existing `RequireUser`:
session cookies require CSRF; user-scoped CLI tokens use the existing bearer
path. Ownership is strict even for an admin. The owner-qualified worker lock
precedes persistent-kind validation: a foreign worker returns 404, while an
owned ephemeral worker returns HTTP 409 with exactly
`manual cordon is only supported for persistent workers`. Supported persistent
kinds are hosted and external. No controller credential or admin-write route is
added (`api/internal/handler/routes_workers.go`,
`api/internal/handler/manual_cordon.go`).

Both verbs accept an empty body or one strict JSON object, bounded to 8 KiB of
encoded body bytes; unknown fields, non-object values and trailing JSON are
rejected. The optional `reason` is a string or null; raw strings over 500 Unicode
code points are rejected before trimming and sanitization. Blank sanitized
reasons become SQL NULL. Actor identity comes from authentication, never JSON.
Repeating the current state preserves the first timestamp, actor and reason and
adds no event. A real transition and one `worker_cordon_events` event commit in
the same transaction; uncordon's event keeps the previous reason. This adds no
audit read/prune API, retention policy or DB-enforced append-only guarantee
(`api/internal/store/manual_cordon.go`, migrations 00352/00353/00354).

**Intake boundary.** Compatible API claim paths serialize with cordon through a
worker-row lock at READ COMMITTED and re-read intake state after that lock.
Manual cordon blocks unrelated new roots, unassigned children and judge intake;
already assigned continuations/checkers can finish subject to the existing
vault, capacity, capability, maintenance and custody fences. It does not cancel
runs, create continuation affinity, end chats or bypass maintenance. New chat
continuation affinity excludes a cordoned worker; existing held chat work may
continue. The combined predicate is `fn_worker_accepts_new_intake`, mirrored by
`WorkerAcceptsNewIntake`; continuation exceptions remain at their intake sites
(`api/internal/store/queries/runtime.sql`, `api/internal/store/queries/chat.sql`,
`api/internal/workersvc/ephemeral_lease.go`).

Older serving API replicas may still admit new work after a successful cordon
until every serving replica is compatible; a stalled rollout extends that
interval. Older controllers may miscount manual workers as coverage. Compatible
controllers are required for ordinary coverage guarantees; decisions from an
earlier poll may finish. Deploy compatible APIs and controllers before relying
on the combined policy; worker images need no feature-specific upgrade.

**Roll boundary.** Manual workers supply no serving/replacement coverage.
Replacement-incarnation proof and reservation completion are distinct from
serving eligibility: an idle manually cordoned worker may roll and return still
cordoned; a busy one waits under #3103. A multi-worker pool whose workers are
manually cordoned may remain held; manual cordon grants no zero-coverage bypass.
ForceRoll retains its existing global emergency trade-off without clearing
manual intent. #3103 indefinite drift waits, #3153/#3307 migration coverage and
custody, and independent DinD/pressure/maintenance guards remain authoritative
(`controller/internal/kube/roll_pacing.go`).

**Diagnosis boundary.** Owner and admin read projections expose manual metadata
and combined fences; health/admin surfaces stay read-only. A manual wait
diagnosis requires a nonempty suitable population entirely manually cordoned
and no claimable assigned continuation. A suitable uncordoned busy or
automatically drained peer prevents that diagnosis. Manual waits are never
upgrade-only waits and receive no upgrade-only queue exemption
(`api/internal/workersvc/health_manual_cordon.go`,
`api/internal/workersvc/health_pacing.go`). This amendment adds no TTL,
provisioning repair, retention policy or automatic release.

### D6 — The controller poll DTO carries `busy` and `draining_since`

`ListHostedWorkersForController` previously selected no run data at all; exposing busy-ness to the controller required a new correlated subquery over `runs`. The poll wire (`api/internal/hostedsvc/protocol.go` and `controller/internal/protocol/protocol.go`, both `DesiredWorker` structs, pinned by the shared golden `controller_poll_wire.json` under `DisallowUnknownFields`) now carries both fields, updated in lockstep across both modules in one change so the contract tests never see one side drift ahead of the other.

### Surge means the stack, not the workers

"Surge upgrade" in this PRD's title refers to the **app stack** (api/web/db/controller) surging forward on every release while old worker pods linger until they finish their work — never to overlapping worker pods. Workers hold RWO PVCs, so two pods can never attach the same volume at once; `Recreate` (delete-then-create) stays the worker roll strategy. This PRD does not add a `RollingUpdate` or surge-replica path for workers, and does not touch the vault's per-process unlock model, multi-replica API, or the requeue-then-manual-unlock fallback — all explicitly out of scope (PRD #422 non-goals).

## Consequences

- **An app-only release rolls zero worker pods when worker rendering is unchanged.** The pin is independent of appVersion; a controller change to the rendered worker template can still initiate a paced same-image roll.
- **A deliberate worker-tag bump cordons admitted busy workers and waits indefinitely for idle (#3103).** This can hold a pool and delay security fixes indefinitely. `workers.drainDeadline` retains its default `24h` for independent nondrifted maintenance only. Global `workers.forceRoll` can interrupt runs and lose all capacity, without a recovery guarantee or bypass of custody-held data or independent DinD guards.
- **The requeue-then-manual-unlock fallback is entirely unchanged.** This PRD removes the *need* to hit it on ordinary releases; it does not fix the vault's single-process unlock model, and a node drain, OOM, or eviction outside a controlled release still strands a run on it exactly as it did before.
- **An out-of-tree per-cluster values file that left `workers.image.tag` empty (relying on the old empty→appVersion default) now fails the Helm render.** This is a real behavior change for operators: `required` refuses a blank tag, so any values file must set a concrete pinned worker version going forward.
- **N-1 worker↔API skew is now a maintained, tested contract**, not an incidental property: `task check:migration-additive` flags a migration that would break it, and a `LiveDB` skew test exercises an old worker's full claim→run→report→complete cycle against a freshly-migrated new API.
- **Workers never receive security fixes on their own** if the tag never moves — the release runbook (`.claude/agents/release.md`) now documents "bump the concrete worker tag" as a normal, distinct release step, with indefinite busy-drift deferral potentially extending exposure after a roll starts and global force-roll available as an explicit emergency trade-off.

## Maintainer hand-off / follow-ups

- **(a) Nothing to do.** The M6 additive-migration guard and the old-worker skew `LiveDB` test already run inside **existing** CI jobs — `lint-repo` invokes `task gate:repo` (which includes `check:migration-additive`), and `test-api-store-it` sweeps `-run 'LiveDB$' ./internal/handler/...`. No workflow file was added or edited to land M6.
- **(b) Optional: wire the chart-render assertions into CI.** `task render:worker-tag-check` (`scripts/assert-worker-tag-decoupled.sh`) and `task render:drain-knobs-check` prove the decoupling and the drain knobs by rendering the chart with `helm template`, the same way `scripts/assert-chart-render.sh` is already invoked in the CI helm-render job. They are not wired into `.github/workflows/ci.yml` by this PRD — the worker that shipped M1–M6 had a PAT scoped without `workflow`, so it could not touch `.github/workflows/**`. A maintainer with that scope can add the two `task` calls next to the existing `assert-chart-render.sh` step.
- **(c) M2's alternative controller-`RolledTag` path is deferred.** The upgrade-badge fix (Decision 9 in the PRD) ships via the classifier route (an intentionally-pinned-behind worker is compared against the pinned worker version, so it is not flagged `outdated`), which needs no controller signal. The alternative — having the controller emit a fresh `RolledTag` roll-health signal so the badge compares against the tag the controller actually rolls to — is **not** shipped here: the controller does not populate `RolledTag` today (`grep -rn RolledTag controller/` is empty), so the api's `RollSignal.RolledTag` path is never fed live. Note this gap is the missing **emission code**, not a missing RBAC grant — the controller already holds `pods: list` (`deploy/chart/templates/worker-rbac.yaml`, granted in PRD #113 M3). Adding the emission and validating it on-cluster is an M9 follow-up, not required for the badge fix to work.
- **(d) M9 (live-cluster validation on the dev cluster) is human-run**, not completable by an offline sweep worker: proving on a real cluster that an app-only release with unchanged worker rendering rolls zero worker pods, that a deliberate tag bump cordons/drains/rolls correctly, that indefinite busy-drift deferral, independent maintenance deadlines and global force-roll behave under the current #3103 policy, and that there is no badge regression, all need real k8s and a running fleet.
- **(e) Historical M8 deferral, superseded for owner manual cordon on 2026-10-22 (D5a; implementation recorded 2026-10-24).** The original decision below proposed an admin-scoped follow-up. The accepted replacement is owner-scoped persistent intent and the cordon/uncordon commands in D5a; it adds no force-roll CLI or admin write. The following is the original rationale, not a description of the current command surface:
  1. The cordon control-write (`POST /api/controller/workers/{id}/drain`) is `RequireController`-scoped — authenticated by the shared controller token, not a user or admin session. The `uzi` CLI (`api/cmd/uzi/`) authenticates as a **user**, so it cannot reach that endpoint. No admin- or user-facing endpoint stamps `draining_since` (only the controller path does).
  2. Force-roll and the drain deadline are **deploy-time** controller env / chart values (`workers.forceRoll`, `workers.drainDeadline`, set via `helm`) with **no** runtime API surface to set them. There is nothing a CLI verb could POST to flip them.
  3. The CLI is deliberately read-mostly for workers (`docs/cli.md`: "No `worker create` and no `admin` writes"; minting a join token is web-only). A worker-write verb would be the first and cuts against that convention.
  The original deferral concluded: building a CLI verb would therefore require **new** product surface — an admin-scoped, operator-initiated manual-drain endpoint distinct from the controller's cordon — which this PRD's controller-driven model does not need. If operator-initiated manual drain is wanted later, it is a clean follow-up (new admin endpoint + CLI verb + tests). Recorded here as an explicit deferral per the PRD's "add it OR defer it in the ADR with a reason."
- **(f) Reverted-drift cordon recovery (issue #458 — landed, not deferred; historical mechanism).** The following records #458's original single-writer model, not the current release predicate: #1470 added reason-owned drain release and independent maintenance fences; #3103 removed elapsed-time busy-drift replacement. No timer now authorizes uncordon. A worker cordoned on drift (D4/D8) whose drift was then REVERTED before it rolled — an operator resets `workers.image.tag` mid-rollout — used to stay cordoned forever: both the drain-deadline check and the force-roll override live INSIDE `reconcileWorker`'s drift branch, *after* the no-drift early return, and `draining_since` is cleared only by `RegisterWorker` on an actual roll (a reverted drift has nothing to roll). Fixed by clearing `draining_since` on the no-drift reconcile path via a new symmetric uncordon control-write — `DELETE /api/controller/workers/{id}/drain` (`ControllerUncordonWorker` → `hostedsvc.Uncordon` → `UncordonHostedWorker`) and `apiclient.ClearDrain` — the exact mirror of D8's cordon `POST`. **Invariant this relies on:** `draining_since` has a single writer and a single meaning — the controller's own drift-cordon (there is no user/admin/node-maintenance drain) — so "no drift + still draining" can only be a reverted drift or a just-committed roll, both correct to clear; if a second `draining_since` writer is ever added, the unconditional no-drift uncordon must be revisited. The clear is fail-safe (on error, log and retry next tick — a failed uncordon self-heals) and idempotent (converges to NULL alongside `RegisterWorker`; `ClearDrain` treats a 404 as success, the opposite of the cordon `RequestDrain`'s fail-closed 404-is-error). This is the recovery corner flagged under (d)'s still-open live-cluster validation, so validating it on-cluster belongs with M9.

## #3103 local verification 2026-10-18

The local results below were inspected by the implementation lead. They do not
establish hosted acceptance or CI validation. The receipt baseline is clean HEAD
`0250a7c7f529dd8269804f925bde03a9696ff9ce`.

| Local check | Tested revision | Recorded result |
|---|---|---|
| `task gate:controller` | `77c0229d0a7a2afbf4201484287c28e08d8bbb8c` | Exit 0; controller tree unchanged at the receipt baseline. |
| `env -u UZI_TEST_DATABASE_URL task gate:api` | Receipt baseline | Exit 0; race enabled, sqlc drift clean, API deadcode 0 findings against a 0 baseline. |
| `task gate:web` | Receipt baseline | Exit 0; browser suite passed 8 files / 28 tests. |
| `task gate:repo` | Receipt baseline | Exit 0; tracked-secret scan and canaries passed; SAST clean. |
| `task render:drain-knobs-check` | Receipt baseline | Exit 0; confirmed 24h default, 1h override, and ForceRoll false/true. |
| Disposable full store integration harness (`./e2e/run-store-it.sh`, scratch-backed TMPDIR) | `e308096d64ec9905afbb7f28dd10fbfde8ae7d54` | Exit 0; 2,984 top-level tests passed, plus 5,121 subtest passes; 0 skips across 7 packages. |
| `task test:e2e-lib && task test:forge-fake` | `e308096d64ec9905afbb7f28dd10fbfde8ae7d54` | Exit 0; forge fake reported 10 passed, 0 skipped. |

The ordinary API gate unsets `UZI_TEST_DATABASE_URL` per
[the Go rules](../.claude/rules/go.md#live-db-tests-livedb) to prevent package
binaries racing a shared database; that gate does not establish LiveDB proof.
The full `./e2e/run-store-it.sh` harness owns a throwaway database, sets
`UZI_TEST_DATABASE_URL` itself and runs the listed packages serially with
`-race -p 1 -run 'LiveDB$'` and a skip guard; named PASS evidence and zero skips
are recorded below and in the table. `TMPDIR="$PWD/.uzi/scratch"` locates harness
logs and inherited Go temporary files within the permitted worktree. Database
setup, test selection and skip checks remain controlled by the harness.

The gates ran serially, with no source changes during them. The implementation
lead verified that the new blocker query and its generated code were unchanged
since the store-harness revision. E2E sources
were likewise unchanged. The repeated M4 string sweep of E2E phases, library,
library tests and forge fake found unchanged credential-pool cases; no assertions
or fakes needed edits.

Executed live-DB PASS evidence includes `TestWorkerUpgradeBlockersProjectionLiveDB`,
`TestWorkerUpgradeBlockersOwnerAdminLiveDB`, and
`TestHostedRollRetainedClaimsLiveDB` (the API parent injects the controller bridge
and proves its child did not skip), plus `TestCrossCheckLaneEligibilityLiveDB`
for own-draining and foreign-draining cases.

M1 red/green evidence records `TestBusyDriftNeverExpires` failing before the
policy fix (exit 1) and passing afterward (exit 0), using a fake clock for
1h, 24h, equality and exceeded boundaries; no real 24-hour wait was performed.
Those initial logs do not establish an embedded source SHA; the root lead
observed the tested source. The first full-DB query test failed on fixture
active-branch-ref uniqueness; unique UUID refs fixed it in the store-harness
revision, followed by three narrowed tests with 0 skips and the green full harness.

A disposable-DB EXPLAIN measurement used 78 fixture workers, 8,000 terminal-history
rows (90% assigned to one worker), an authorized batch of 4, fresh ANALYZE and
`idx_runs_worker`: execution was 2.505 ms. This is a measurement, not a production
performance guarantee; no index or migration was added.

**Unrun:** isolated hosted drain, new-root refusal, existing-work completion and
idle-upgrade acceptance. No expressly isolated disposable hosted worker was
supplied; shared workers, runs and volumes were not mutated. PRD #1470 hosted
acceptance remains unchecked; #3103 is delivered without closure pending that
acceptance. This evidence gives no
lost-source or shutdown-preservation guarantee. The original receipt-only
documentation commits did not alter tested product trees or API embedded docs.
