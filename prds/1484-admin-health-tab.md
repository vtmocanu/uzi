# PRD #1484: Admin health tab, Overview health card and danger banner

**Issue:** [#1484](https://github.com/vtmocanu/uzi/issues/1484)
**Status:** Planned (2026-09-20).
**Execution:** Send to uzi, Auto mode, gated plan. API + web + CLI + Slack/inbox notice + docs + specs. One additive migration. **No `.github/workflows/**` touched** in implementation or validation (`.claude/rules/prds.md`). **The Helm chart is not touched at all**, `deploy/chart/templates/worker-rbac.yaml` least of all.
**Mock:** `prds/mockups/1484-admin-health-mock.html` (open locally; the dark top bar switches five scenarios, three surfaces and the viewer role). The mock is the accepted design. Its data is placeholder data.

Use current `main` and a new working branch, never write to `main`.

## Problem

An admin cannot see, from any uzi surface, that the instance is unable to run work.

The motivating incident: a released chart pinned the hosted worker fleet to a worker image tag that was never published. Every new worker pod sat in `ImagePullBackOff`, capacity went to zero, and queued runs never dispatched. ArgoCD read Synced and Healthy the whole time, because worker pods are created by `uzi-controller`, not by the chart. Inside uzi, each affected user could see a red upgrade badge on their own Workers page, and nothing else existed: the admin's cross-user worker list carries no roll health, there is no instance-level view, and nothing pushed a notice to anyone.

The same blind spot covers quieter failures that today exist only as log lines or as nothing at all: the worker controller going silent, a background loop that stopped ticking, a user-level schedule pause that makes labelled issues look queued forever, the CI watch sitting at its branch cap, a task run created but never dispatched.

## Solution

One read-only admin health endpoint, built from a **closed registry of checks** over data the api already holds plus three small pieces of new plumbing, surfaced five ways:

1. **Admin > Health**, the **last** tab in the admin tab strip: a verdict, a needs-attention list, grouped checks with evidence and a what-to-do line, and a cross-user fleet table that finally shows roll health.
2. **Overview**: an admin-only, self-hiding health card (one quiet line when healthy).
3. **App-wide banner** for admins on Danger only, with a 1 h snooze.
4. **`uzi admin health`**, plus the missing roll-health columns on `uzi admin workers`.
5. **One notice per admin per Danger episode** (web inbox and Slack DM), because a tab nobody is looking at does not wake anyone.

Plus one line for non-admins on Overview, built only from their own workers and runs, so a stuck user can tell a platform problem from a problem with their run.

**The boundary this PRD holds:** in-app health covers what only uzi knows about itself. It never reads the Kubernetes API. Pod-level state of the api, web, database and controller pods belongs to cluster monitoring, and is explicitly out of scope (see Out of scope).

## Resolved facts (read locally, no internet needed)

All verified against `main` at `66cad56e` on 2026-09-20, then independently fact-checked. Re-verify an anchor before relying on its line number.

### The api has no kube access, and that is enforced

- `controller/` is a separate Go module so `k8s.io/client-go` stays out of `api/go.mod`. `api/internal/hostedsvc/no_kube_dependency_test.go` asserts this over the whole api build graph.
- `deploy/chart/templates/api-deployment.yaml` sets `automountServiceAccountToken: false`.
- The controller's Roles (`deploy/chart/templates/worker-rbac.yaml`) cover only the two worker namespaces, with `pods: ["list"]` and nothing else on pods. It has no read access to the release namespace. There is no ClusterRole in the chart.
- Consequence: every Kubernetes-derived fact in this PRD arrives over the existing controller report, `POST /api/controller/status` (`api/internal/handler/controller_status.go`). This PRD adds no field to that wire format and changes no RBAC.

### Roll health already reaches the api, but only the owner's list reads it

- The controller classifies a worker pod as `stuck` immediately on `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError`, `CreateContainerError`, `InvalidImageName` (`controller/internal/kube/rollhealth.go`), and reports `phase`, `blocking_container`, `blocking_reason`, `restart_count`, `last_exit_code` per worker.
- The api validates, sanitizes (64-byte cap, control characters stripped) and persists it through `UpsertWorkerRollHealth` (`api/internal/store/queries/worker_roll_health.sql`). **The table is `worker_upgrade_reports`** (created by migration `00083_worker_roll_health.sql`; only the file names say "roll health").
- `api/internal/workersvc/upgrade.go` turns a fresh `stuck` signal into `upgrade_failed`.
- **The gap:** `AdminListWorkers` (`api/internal/handler/workers.go`) builds its DTOs with `workerDTOFromWorker`, which passes `Signal: nil`. Only the owner-scoped `ListWorkers` goes through `workerDTOFromRow` and `rollSignalFromRow`, which do the join. So on the admin list `upgrade_status` is a bare version compare and the `upgrade_blocking_*` fields are always null.
- The fix is two edits, not one: the admin list's query `ListAllWorkers` (`api/internal/store/queries/runtime.sql`) has no `worker_upgrade_reports` LEFT JOIN, and its row type differs from the per-user one, so it needs the join plus its own row-to-DTO mapper. `AdminWorkerDTO` embeds `WorkerDTO` (`api/internal/apitypes/worker.go`), where the `upgrade_blocking_*` fields already live, so this is a **population** change with no DTO shape change.
- `uzi admin workers` (`api/cmd/uzi/admin.go`) prints `ID OWNER NAME STATUS RUNS OUTBOX`. The owner-scoped `uzi worker list` has an UPGRADE column; the admin one does not.
- `k8s state.waiting.message` is deliberately never relayed (free text that carries paths, image references and Secret names). This PRD keeps that refusal.

### Workers are per user

A worker belongs to one owner and only that owner's runs claim it. "Fleet capacity" is therefore a per-owner question: the failure is an owner who has runs waiting and no worker of their own that can take them. The cordon column is `workers.draining_since` (non-cordoned means `draining_since IS NULL`).

### The controller's last report leaves no fleet-independent trace

The arrival time of a status report is stored only per worker (`worker_upgrade_reports.observed_at`). With zero hosted workers there is no row, so "when did the controller last report" is unanswerable today, and `ControllerStatus` writes nothing at all for a zero-worker report. One stored singleton row is needed, **written unconditionally on every report**. A dedicated one-row table is right; the `app_settings` key-value table is wrong for a value rewritten every 10 s (it carries an `updated_by` FK and settings-cache semantics). `controller_status.go` already stamps the api's own clock for freshness and treats the controller's `reported_at` as display only; keep that.

### Existing signals the registry reads, with no new storage

| Check | Source |
|---|---|
| Queued runs waiting | `runs.health = 'waiting_worker'` and `runs.health_since` (migration `00057_run_health.sql`), written by the sweeper (`api/internal/workersvc/health.go`) |
| Task runs never dispatched | `runs.dispatched_at IS NULL` on `kind = 'task'`, `status = 'queued'` (migration `00134_run_task_kind.sql`; this is the #1367 failure class). The `kind = 'task'` scope is load-bearing: `dispatched_at` is only ever set on a task run, so unscoped the predicate matches every queued run |
| Worker disk | `workers.stats_disk_pressure_streak >= 2` (migration `00192_worker_disk_pressure_streak.sql`), the same debounced signal the controller poll's `disk_pressure` is derived from, gated on a fresh heartbeat. **There is no `disk_pressure` field on `WorkerDTO`**; it carries only the disk byte stats |
| Database | `pgxpool.Pool.Stat()`, a ping, and goose's applied version against the embedded head |
| Slack | the socket manager state behind `GET /api/admin/slack/status` |
| Schedules paused | `users.schedules_paused` and `schedules_paused_until` (migration `00189_schedules_paused.sql`) |
| Board drift | `runs.move_pending_since` past the give-up cutoff (the predicate of `ListGaveUpColumnMoves`), **restricted to `issue_iid IS NOT NULL`** so the issue-less false positives of issue #1482 (judge, chat, prompt and task runs) never count, whether or not #1482 has landed |
| Recovery custody | `ListOwnersOverCustodyLimit`, the query `slacksvc/custody_episode.go` already uses |
| Release | the `far_behind` derivation in `api/internal/releasecheck/derive.go`, behind `GET /api/admin/release-check` |

**The run-health detector can be switched off.** `detectRunHealth` returns early when the `health_enabled` setting is false (default true), and it is the only writer of `waiting_worker`. With it off, the two checks that read `runs.health` have no signal and must say so (`unknown`), not read green.

### New plumbing, all in the api process or one migration

- **Controller last report**: the singleton row above.
- **Background loop beats**: four loops are started from `api/cmd/server/main.go` and matter to an admin: the forge poller (`engine.Run`), the sweeper (`sweep.Run`), the run-lifecycle reconciler (`lifecycle.RunReconciler`) and the scheduler (`scheduler.Run`, conditional). None records a last-tick time. A dead scheduler goroutine silently stops every scheduled job, which is distinct from a user's pause-all. Make the beat registry a leaf (inside `healthsvc` or its own tiny package) and have `main.go` inject a `Beat(name)` callback into each loop, exactly as it injects every other collaborator, so no loop package imports `healthsvc`. The judge is **not** a loop: it is a run kind claimed by workers. The other goroutines in `main.go` (privilege sweep, usage engine, agent-source runner, release runner, the vault-lock and custody-episode reconcilers) are out of v1.
- **CI watch at cap**: today a log line that fires every tick (`api/internal/forgesvc/pipeline_sync.go`, issue #1483). The watched set is capped by construction (`LIMIT MaxRefs`), so "more watched than the cap" can never be true: the check needs a count of **eligible** run branches before the `LIMIT`, per repo. It must not depend on #1483 landing.
- **Danger episodes**: see Danger notice.

### Two checks in the mock are deferred, with the reason

- **Version skew** (web, api and controller on different releases). Only the api build is stamped: `release.yml` passes `UZI_VERSION` to the api image alone, `controller/Dockerfile` has no `-X main.version`, and the web bundle has no build id. Stamping the other two needs a `.github/workflows/**` edit, which a uzi worker cannot push. Maintainer follow-up.
- **Forge sync freshness**. `synced_at` and `last_synced_at` exist per cached issue, per pipeline row and per repo (the GitHub Projects v2 link, migration `00140_github_project_sync.sql`), never per forge connection; `forge_connections.last_verified_at` is a credential check, not a sync. Follow-up once per-connection bookkeeping exists.

The mock lists both under "Not in v1".

### Precedents to copy, not reinvent

- **Admin route groups** (`api/internal/handler/routes_admin.go`): reads sit behind `mw.RequireUser` + `mw.RequireAdminRO`, which accepts a `uza_` CLI token. Writes sit behind `mw.RequireAuth` + `mw.RequireAdmin`, cookie plus CSRF only. The release banner snooze is the **route and auth** precedent only: its storage is one instance-wide `app_settings` row keyed to the release tag, which cannot hold a per-admin, per-episode snooze.
- **Router-level auth tests**: `api/internal/handler/cli_auth_livedb_test.go` (a table of admin routes) and `api/internal/handler/recovery_owner_holds_livedb_test.go` (a real chi router driven with `bearerReq` and `cookieReq`). A fake-client test bypasses the router and proves nothing about the mount.
- **Admin tabs**: three edits. A row in `APP_ROUTES` (`web/src/App.tsx`, `guard: "admin"`), an entry in `TABS` (`web/src/components/AdminShell.tsx`), and the page wrapped in `<AdminShell>`. `web/src/App.routes.test.tsx` mounts every `APP_ROUTES` row and fails if one throws; it cannot notice a row that was never added.
- **Self-gating admin banner**: `web/src/components/UpdateEscalationBanner.tsx`, mounted in `AppShell.tsx`. It makes no fetch at all for non-admins and uses `role="alert"`.
- **Self-hiding Overview alert**: `web/src/components/CustodyBoardAlert.tsx`, mounted once in `web/src/pages/Dashboard.tsx`, its own best-effort fetch on the 10 s `usePollWhileVisible` cadence.
- **Episode notice**: `api/internal/slacksvc/custody_episode.go`, itself modelled on `schedsvc.VaultLockReconciler`. A standalone reconciler wired from `main.go`, at most one notice per episode through the persist-first, per-user `notifysvc.Notify` seam (so it lands in the web inbox and, for a linked user, as a Slack DM), an atomic claim for dedup, and a re-arm on recovery. Its dedup is proven by a live-DB test in the **`store`** package (`api/internal/store/custody_episode_livedb_test.go`), not in `slacksvc`.
- **Closed, server-authored strings**: the run-health reasons in `api/internal/workersvc/health.go`.
- **`role="status"` is ambiguous in tests**: the always-present `RateLimitAnnouncer` owns one. Scope queries to the surface under test (`.claude/rules/web.md`).
- **Operator docs render in-app for admins**: `web/src/lib/docs.ts` shows `audience: operator` pages to admins, and `web/scripts/check-docs.mjs` gives operator pages their own `order` namespace.

### No workflow impact, and the traps to avoid

Nothing here needs a file under `.github/workflows/`.

- **Live-DB tests.** A `*LiveDB` test in a package the sweep does not enumerate is a two-list edit, and one of the two lists is `ci.yml`. The enumerated packages are `store`, `handler`, `forgesvc`, `schedsvc`, `workersvc` (`e2e/run-store-it.sh`). **The two tempting wrong homes in this PRD are the new `healthsvc` package and `slacksvc`.** Put query tests in `store` and route tests in `handler`. Do not add a package to either list.
- **Docs mirror.** Any milestone that edits a `docs/*.md` file runs `task docs:sync` and commits the mirror in the same commit, or `TestEmbeddedDocsMatchSource` reddens `gate:api`.
- **Dead code.** `deadcode:api` gates at zero: a new exported `healthsvc` symbol must be reachable from production code, not only from tests. `deadcode:web` (knip) gates unused exports: a new type exported from `web/src/lib/apiTypes.ts` needs a real consumer in the same milestone.
- **The route limiter table.** `route_limiter_mounts_test.go` walks every mounted route; a route with no `wantRouteMounts` row reddens it. Both new routes are `noLimiter`, like the release-check pair.

## Design (accepted from the mock)

### Severity

`ok`, `warn`, `danger`, `unknown`, `na`. Encoded in form as well as colour (a distinct glyph and pill shape per severity).

- A stale or missing signal is `unknown`, **never** `ok`.
- A check that cannot apply on this deployment (no hosted workers, Slack not configured) is `na`. It stays in the list; it does not disappear and does not turn green.
- The overall status is the worst of `danger`, then `warn`. `unknown` ranks as `warn` for the overall status. **Only `danger` raises the banner and the notice.**
- **A roll in progress must not alert.** Only a blocked pod does.
- Every non-`ok` check carries `since`, where the source has one.

### Checks in v1

Thresholds are constants in v1, named in one place, not settings.

| id | group | warn | danger | unknown / na |
|---|---|---|---|---|
| `fleet.roll` | workers | some hosted workers `upgrade_failed` | every hosted worker `upgrade_failed` | `unknown` when the newest roll signal is older than `ControllerSignalTTL` and `controller.report` is not `ok`; `na` when hosted workers are not configured |
| `fleet.capacity` | workers | none | for 5 min, at least one owner has a run with `health = 'waiting_worker'` and zero online workers with `draining_since IS NULL` | `unknown` when `health_enabled` is off |
| `fleet.disk` | workers | any worker with a fresh heartbeat and `stats_disk_pressure_streak >= 2` | none | none |
| `queue.waiting` | queue | oldest `waiting_worker` run at least 10 min | at least 30 min | `unknown` when `health_enabled` is off |
| `queue.undispatched` | queue | none | any `kind = 'task'`, `status = 'queued'` run with `dispatched_at IS NULL` older than 10 min | none |
| `controller.report` | control | none | no report for 5 min | `unknown` from 3 missed intervals up to 5 min, and for the first 5 min after api boot with no report yet; `na` when hosted workers are not configured |
| `db` | control | ping over 250 ms, or pool acquired at least 80% of max | ping fails, or applied migration version differs from the embedded head | none |
| `loops` | control | a loop's last beat older than 3 of its intervals | older than 10 intervals | `unknown` for a loop that has not beaten since boot and is younger than 3 intervals; a loop that is not started on this deployment (the conditional scheduler) is left out of the evidence, not counted |
| `forge.ciwatch` | integrations | any repo with more **eligible** run branches than `CI_WATCH_MAX_REFS`, so some go unwatched | none | `na` when `CI_WATCH_MAX_REFS` is 0 |
| `slack.socket` | integrations | configured and disconnected for 5 min | none | `na` when Slack is not configured |
| `schedules.paused` | housekeeping | any user has a pause-all in force and owns at least one enabled schedule | none | none |
| `board.drift` | housekeeping | at least one given-up column move in 24 h on a run with an issue | none | none |
| `custody.holds` | housekeeping | any owner at the custody admission limit | none | none |
| `release.check` | housekeeping | the release check reports `far_behind` | none | `na` when the release check is disabled |

"Hosted workers are configured" means `HOSTED_WORKER_VERSION` is non-empty or at least one `kind = 'hosted'` worker exists. The plan confirms that predicate against `api/internal/config/config.go` and says so.

`fleet.capacity` is deliberately narrow. `waiting_worker` covers many causes (vault locked, custody limit, all workers busy, no eligible worker), so the conjunction with "zero usable workers" is what makes it a capacity failure. A capability-gap run (online workers, none eligible) is not a capacity failure and surfaces through `queue.waiting` by age instead.

### Text discipline

Every `summary`, `action` and `command` is composed by the server from a fixed template per check id plus numbers, closed enums and identifiers the admin may already see (owner names, worker names, repo paths). **No free text from Kubernetes, a forge, a run or a worker is ever interpolated**, apart from the already-sanitized `blocking_reason` and `blocking_container` enums. `command` strings are fixed templates with literal placeholders such as `<worker-namespace>`; the api does not know the namespace and must not pretend to.

### The Health tab

Last in `TABS`, after Branding. `/admin` keeps redirecting to `/admin/users`. On a narrow screen the tab strip scrolls, so the active tab is scrolled into view, and the two real entry points are the sidebar Admin pip and the Overview card. The tab label carries a severity pip and a count when anything needs attention.

Layout, top to bottom: verdict with per-severity tally; the needs-attention list (links that open and scroll to the check); "Checked N s ago" and a Copy diagnostics button; then one card per group. `danger` and `unknown` checks render expanded. The workers group ends with the cross-user fleet table: Owner, Worker, Kind, Status, Version, Upgrade, Blocking, Since. The table reads `GET /api/admin/workers`, which now carries roll health.

### Overview

- **Admin card**, mounted beside `CustodyBoardAlert`: when everything passes it is one quiet line ("System health: all N checks passing") with an Open health link. Otherwise it shows the verdict and the top three attention items. It makes no request for a non-admin.
- **Banner**, mounted in `AppShell` beside `UpdateEscalationBanner`: admins only, `danger` only, `role="alert"`, the verdict line, the cause line, an Open health link, and **Snooze 1 h**. A snooze is per admin and applies to the current Danger episode only; a new episode shows the banner again. The banner follows `status`, so it appears at once; the Snooze button renders only while `episode_id` is non-null (an episode opens within one evaluation, see Danger notice).
- **Non-admin line**: shown when the viewer has at least one queued run, owns at least one hosted worker, and every hosted worker they own reads `upgrade_failed`. It is derived entirely from the `listRuns` and `listWorkers` responses Overview already polls. No new endpoint, nothing about any other user. Copy: "Your hosted workers cannot start right now: the worker image cannot be pulled. This is a platform problem, not your run. Queued runs start on their own once it is fixed." The cause clause follows the viewer's own `upgrade_blocking_*` fields through the existing `likelyCause` lookup in `WorkerUpgradeBadge.tsx`, which already yields the image-pull cause for `ImagePullBackOff` and `ErrImagePull`; it is never free text. Admins get the card instead of this line.

### CLI

- `uzi admin health` prints the non-`ok`, non-`na` checks (`SEVERITY CHECK SINCE SUMMARY`), the verdict and a tally. `--all` lists every check. `--json` emits the endpoint's document unchanged.
- Exit status: `0` unless the overall status is `danger`, then **`8`**, a new code in the CLI exit-code contract ("a health check reports danger"). `--strict` also exits 8 on `warn` or `unknown`. The contract lives in code, not only in docs: codes 0 to 7 are constants in `api/internal/uzicli/output.go` and `ExitCodeFor(err)` maps errors to them. Exit 8 is a **success-path** exit (HTTP 200 carrying `status: danger`), so the command prints its full output, then returns a sentinel error that `ExitCodeFor` maps to a new `ExitHealthDanger = 8`, with a test. Transport and auth failures keep their existing codes, so a probe can tell "unhealthy" from "could not ask".
- `uzi admin workers` gains `VERSION`, `UPGRADE` and `BLOCKING`, reusing `upgradeCell` from `api/cmd/uzi/worker.go`. Every server string goes through the bounded, package-local `cellText`, never the unbounded `CellText` (`.claude/rules/go.md`).

### Danger notice and episodes

The custody precedent dedups per owner with an implicit episode (a row exists while over the limit). This feature is instance-level, fans out to every admin, and exposes an explicit `episode_id` that the snooze keys on, so it needs its own storage. Draft shape, which the plan may rename:

- `health_episodes`: `id`, `opened_at`, `closed_at`. **At most one open episode**, enforced by a partial unique index on the open row, so opening is an atomic insert two replicas cannot both win.
- `health_episode_notices`: primary key `(episode_id, user_id)`. The insert is the atomic claim; the caller sends a notice only when the insert took.
- `health_banner_snoozes`: primary key `(episode_id, user_id)`, `snoozed_until`.
- A new `ListAdmins` query (`users.is_admin`).

A standalone evaluator, wired from `main.go` beside the custody episode reconciler, runs the same registry once a minute:

1. Overall status is `danger` and no episode is open: open one.
2. Overall status is `danger` on the evaluation **after** the one that opened the episode (the two-evaluation debounce): claim and send one notice to each admin through `notifysvc.Notify`, listing the danger checks at that moment.
3. Overall status is not `danger` and an episode is open: close it. That is the re-arm.

It reuses the existing health-notification enablement gate; no new enable flag. `warn` and `unknown` never notify. There is no recovery notice in v1. The chart runs a single api replica, so the two-replica race is defensive, consistent with the custody precedent.

## API contract

`GET /api/admin/health`, in the `RequireUser` + `RequireAdminRO` group.

```json
{
  "status": "danger",
  "checked_at": "2026-09-20T02:52:10Z",
  "counts": {"ok": 10, "warn": 1, "danger": 3, "unknown": 0, "na": 0},
  "snoozed_until": null,
  "episode_id": "b1f0c2",
  "checks": [
    {
      "id": "fleet.roll",
      "group": "workers",
      "title": "Worker image roll",
      "severity": "danger",
      "summary": "4 of 4 workers stuck rolling to 0.84.0-rc.2: ImagePullBackOff",
      "since": "2026-09-20T02:14:00Z",
      "evidence": [{"label": "Target tag", "value": "0.84.0-rc.2"}],
      "action": "Publish the image, or release a chart whose workers.image.tag names a published tag.",
      "command": "kubectl -n <worker-namespace> describe pod -l uzi.dev/hosted-worker-id=<id>",
      "doc": "worker-upgrades"
    }
  ]
}
```

- `id`, `group` and `severity` are closed enums. `since`, `action`, `command`, `doc`, `snoozed_until` and `episode_id` are nullable. `checks` is always the full registry in a stable order. `snoozed_until` is the caller's own snooze.
- `POST /api/admin/health/snooze`, in the `RequireAuth` + `RequireAdmin` group (cookie plus CSRF; no CLI verb, matching "no admin write verbs" in the CLI). It snoozes the caller's banner for 1 h against the open episode. 409 when no episode is open.
- `GET /api/admin/workers` is unchanged in shape. Its existing `upgrade_*` fields are now populated from the roll signal, exactly as `GET /api/workers` populates them.
- `GET /api/health` and `GET /api/version` do not change. `TestVersionEndpointCarriesNothingPrivate` must stay green untouched.
- New DTOs follow the three-file rule: the Go struct, `fixtures/api-contract/<dto>.{zero,full}.json` **recorded from the failing contract test's output, never hand-authored**, and `web/src/lib/apiTypes.ts`.

## Milestones

Per-milestone gate lines: M1, M2, M3, M6 `task gate:api`; M4, M5 `task gate:web`; M7 `task check-docs:web`; and `task gate:repo` once before finalize. Run each gate once, to a log, then read the log. Any milestone that edits `docs/*.md` also runs `task docs:sync` and commits the mirror.

- [x] **M1: health registry and endpoint.** New `api/internal/healthsvc` with the closed check registry and one `Evaluate` that the handler, and later the evaluator, both call. Every check whose source already exists: `fleet.roll`, `fleet.capacity`, `fleet.disk`, `queue.waiting`, `queue.undispatched`, `db`, `slack.socket`, `schedules.paused`, `board.drift`, `custody.holds`, `release.check`. `GET /api/admin/health`, contract fixtures, the limiter row, and the web type with a real consumer (the api client function) so knip stays green. Done when: a **router-level** test in `handler` (precedents above) proves anonymous 401, a `uzc_` token and a non-admin cookie 403, a `uza_` token 200; table tests drive each check through every severity it can take, including `unknown` with `health_enabled` off, asserting on the emitted `summary` text and severity, never on a count; a test feeds a hostile worker name carrying control characters and a hostile `blocking_reason` through the registry and asserts both render sanitized. Landed: `api/internal/healthsvc/{service.go,checks.go,thresholds.go}` (registry + `Evaluate`), `api/internal/handler/health_admin.go` + `health_admin_livedb_test.go` (`TestAdminHealthAuthLiveDB`, the 401/403/200 router-level matrix), `api/internal/healthsvc/healthsvc_test.go` (per-check table tests incl. the hostile-string case), `api/internal/apitypes/health.go`, `fixtures/api-contract/health_doc.{zero,full}.json`, `web/src/lib/apiTypes.ts` + `web/src/lib/api.ts` (`getAdminHealth`).
- [x] **M2: new plumbing, the admin worker list, episodes and snooze.** One additive migration (draft number, renumbered above the live head at merge with `task migration:renumber`; never write the goose annotation token in a comment): the controller-report singleton and the three episode tables. `ControllerStatus` writes the singleton on every report, including a zero-worker one. `controller.report`, `loops` (four loops, injected `Beat`) and `forge.ciwatch` (eligible-branch count) checks. `ListAllWorkers` joins `worker_upgrade_reports`, with its own row-to-DTO mapper. The once-a-minute evaluator, opening and closing episodes only (no notices yet). `POST /api/admin/health/snooze` and its limiter row. Done when: live-DB tests **in `store`** execute every new query, including two concurrent episode opens where exactly one wins; a `handler` test shows `AdminListWorkers` returning `upgrade_failed` with the blocking container and reason for another user's stuck hosted worker, and fails on the unfixed handler; a stale controller row yields `unknown` on `fleet.roll`, not `ok`; with hosted workers unconfigured both hosted checks are `na`; a zero-worker report still moves the singleton. Landed: migration `api/internal/store/migrations/00238_admin_health.sql` (already at the live head, no renumber needed), `api/internal/store/queries/health.sql` + `api/internal/store/health.sql.go`, `api/internal/healthsvc/{beats.go,episode.go}`, `api/internal/handler/health_admin_m2b_livedb_test.go` (`TestAdminHealthSnoozeAuthLiveDB`, `TestAdminHealthEpisodeAndSnoozePerCallerLiveDB`, `TestControllerStatusAdvancesSingletonEndToEndLiveDB`), `api/internal/handler/admin_workers_roll_livedb_test.go` (`TestAdminListWorkersRollHealthLiveDB`), `api/internal/store/health_plumbing_livedb_test.go` (incl. `TestHealthEpisodeConcurrentOpenLiveDB`, `TestControllerReportSingletonLiveDB`).
- [x] **M3: CLI.** `uzi admin health` with `--all`, `--strict`, `--json`; `ExitHealthDanger = 8` in `api/internal/uzicli/output.go`, the sentinel error and its `ExitCodeFor` case; `uzi admin workers` gains `VERSION`, `UPGRADE`, `BLOCKING`. The exit-code table and the new verb in `api/internal/uzicli/skill/SKILL.md` and `docs/cli.md`, then `task docs:sync`. Done when tests show: exit 0 on `ok` and on `warn`, 8 on `danger`, 8 on `warn` under `--strict`, the existing auth and unreachable codes unchanged on a 401 and a 5xx, the full table still printed before a non-zero exit, and a hostile 1 MB server string in a cell bounded by `cellText`. Landed: `api/cmd/uzi/admin.go` (`runAdminHealth`, `healthVerdictError`, the `admin workers` VERSION/UPGRADE/BLOCKING columns), `api/internal/uzicli/output.go` (`ExitHealthDanger`, `ErrHealthDanger`), `api/cmd/uzi/admin_health_test.go`, `api/internal/uzicli/output_test.go`, `api/internal/uzicli/skill/SKILL.md` + `docs/cli.md` (exit-code table + `admin health` entry).
- [x] **M4: the Health tab.** `/admin/health` as the last tab, the page per the mock, the fleet table, 10 s polling that keeps last-good on a failed fetch, tab pip, active tab scrolled into view on a narrow screen. Mock-mode scenarios `health-degraded`, `health-incident` and `health-silent` in `web/src/mocks/mockApi/`, listed in `docs/dev-conventions.md` (then `task docs:sync`). Done when: the route smoke test mounts the new row; component tests cover the five scenarios including `na` on a no-hosted-workers fixture; severity is asserted by accessible name or text, not by colour class; untrusted strings rendered into `title=` or `aria-label=` are asserted on the attribute. Landed: `web/src/pages/AdminHealth.tsx` (page + fleet table), `web/src/lib/useAdminHealth.ts` (shared last-good poll hook), `web/src/components/AdminShell.tsx` (Health tab + severity pip + active-tab scroll), `web/src/App.tsx` (route), `web/src/mocks/{data,mockApi}/health.ts` + workers-mock incident branch, `web/src/pages/AdminHealth.test.tsx` (5 states + fleet + pip + untrusted-attribute). Scenario branching uses the mock-scenario keys `health-degraded` / `health-incident` / `health-silent`.
- [x] **M5: Overview card, banner, pips and the non-admin line.** Done when: a non-admin session makes **no** request to `/api/admin/health` (asserted on the mock api call log, with a positive control that an admin session does make it); the banner renders only on `danger`, hides its Snooze button while `episode_id` is null, snoozes through the endpoint, and returns on a new `episode_id`; the non-admin line appears only when all three conditions hold and is absent for an admin; tests select the banner by a scoped handle, not a bare `getByRole("status")`. Landed: `web/src/lib/useAdminHealth.tsx` refactored into `HealthStatusProvider` + `useHealthStatus()` (the ONE isAdmin-gated poll, mounted once in `AppShell`, consumed by every surface incl. the M4 AdminShell tab pip + AdminHealth page); shared `web/src/lib/healthView.ts` (verdict/attention) and `web/src/components/healthSeverity.tsx` (`SEV`/`SeverityPill`/`HealthPip`); new `web/src/components/{HealthDangerBanner,HealthOverviewCard,HealthPlatformLine}.tsx`; `web/src/components/AppShell.tsx` (provider mount, banner beside `UpdateEscalationBanner`, sidebar Admin health pip on the nav `NavItem`); `web/src/pages/Dashboard.tsx` (card + non-admin line beside `CustodyBoardAlert`); `api.snoozeAdminHealth` (`web/src/lib/api.ts`) + its mock parity; mock `snoozeAdminHealth` snooze-flip and the `health-incident` non-admin stuck-fleet `listWorkers` branch (`web/src/mocks/mockApi/{health,workers}.ts`). Tests: `web/src/lib/useAdminHealth.test.tsx` (no-fetch-for-non-admin + admin positive control on the mock call log), `web/src/components/{HealthDangerBanner,HealthOverviewCard,HealthPlatformLine}.test.tsx`.
- [x] **M6: the Danger notice.** The evaluator gains the debounced, claimed fan-out. Done when tests show: no notice on the evaluation that opens an episode; exactly one notice per admin per episode, with the claim's atomicity proven by a live-DB test **in `store`** (the custody precedent), not in `slacksvc`; a fresh notice after an episode closes and a new one opens; nothing for `warn` or `unknown`; nothing when the health-notification gate is off; the notice body contains only server-authored text. Landed: `api/internal/healthsvc/episode.go` (`notifyAdmins`, `buildHealthEpisodeNotification`), `api/internal/healthsvc/episode_notice_test.go` (`TestEpisodeNotice_{OpenerTickNoNotice,ExactlyOncePerAdmin,FreshNoticeAfterReArm,NoNoticeForWarnOrUnknown,GateOffSuppressesNotice,ServerAuthoredBody,SlackMarkupInSummaryIsInert}`), `api/internal/store/health_plumbing_livedb_test.go` (`TestClaimHealthEpisodeNoticeAtomicLiveDB`, the atomic per-admin claim in `store`).
- [x] **M7: docs, specs, changelog, ADR.** A new `docs/admin-health.md` (`audience: operator`, an `order` unique among operator pages), covering each check, what it cannot see, and the probe recipe (`uzi admin health` from cron with a `uza_` token). Correct `docs/hosted-workers.md`, which says there is no in-app diagnostic, and `docs/worker-upgrades.md`. A line in `ARCHITECTURE.md` linking this PRD. `specs/human.md` entries tagged `(AI-synced 2026-09-20)`. `CHANGELOG.md` under `[Unreleased]`. An ADR numbered 1484, slug `in-app-health-boundary`, recording the boundary in Solution: in-app health never reads the kube API, the api stays credential-free, pod-level health belongs to cluster monitoring. `task docs:sync` and commit the mirror. Landed: `docs/admin-health.md` (`order: 55`, `audience: operator`), `docs/hosted-workers.md` + `docs/worker-upgrades.md` (cross-linked, corrected), `ARCHITECTURE.md` (Admin in-app health subsection), `specs/human.md` (Feature #1484, tagged `[AI-synced 2026-09-20, #1484]`), `CHANGELOG.md` (`[Unreleased]`), `adr/1484-in-app-health-boundary.md`, `api/internal/uzidocs/embed/*.md` (docs mirror, `task docs:sync`).
- [ ] **M8 (maintainer-owned, not the worker): hosted acceptance.** After the next release candidate deploys to the dev cluster: provision one throwaway hosted worker, point it at a non-existent image tag, and confirm `fleet.roll` turns `warn`, the fleet table names the blocking container and reason under another user's row, and `uzi admin health` agrees. Scale the controller to zero and confirm `controller.report` goes `unknown` then `danger`, the banner and one notice appear, and both clear on scale-up. Confirm compose shows the hosted checks as `na`.

### Execution plan

| Phase | Milestones | Depends on | Touches |
|---|---|---|---|
| 1 | M1 | none | `api/internal/healthsvc`, `api/internal/handler`, `api/internal/apitypes`, `fixtures/api-contract`, `web/src/lib` |
| 2 | M2 | M1 | `api/internal/store`, `api/internal/handler`, `api/internal/healthsvc`, `api/cmd/server/main.go`, the four loop packages |
| 3 (parallelizable) | M3, M4, M6 | M2 | M3: `api/cmd/uzi`, `api/internal/uzicli`, `docs/cli.md`. M4: `web/src/pages`, `web/src/components/AdminShell.tsx`, `web/src/App.tsx`, `web/src/mocks`. M6: `api/internal/healthsvc`, `api/internal/slacksvc`, `api/internal/store` |
| 4 | M5 | M4 (shares the mock scenarios and the api client) | `web/src/components/AppShell.tsx`, `web/src/pages/Dashboard.tsx`, new components |
| 5 | M7 | M1 to M6 | `docs/`, `api/internal/uzidocs/embed/`, `specs/human.md`, `ARCHITECTURE.md`, `CHANGELOG.md`, `adr/` |
| 6 | M8 | a published release candidate | the dev cluster, no repo files |

A single uzi run executes these in order. M3, M4 and M6 share no files except the docs mirror (M3 and M4 each run `task docs:sync`), so they can be split across sessions if this is ever hand-driven.

## Success criteria

1. With every hosted worker of at least one owner blocked on an image pull and that owner's runs queued, an admin sees `danger` on Overview and on every page (banner), the Health tab names the blocking container and reason per worker across users, `uzi admin health` exits 8, and each admin receives exactly one notice.
2. A silent controller turns roll health `unknown`, never green, while `fleet.capacity` (heartbeat-based) stays truthful.
3. A compose deployment with no hosted workers shows `na` for the hosted checks and an overall `ok`.
4. A worker mid-roll raises nothing.
5. A non-admin never triggers a request to an admin route, and sees the platform line only for their own blocked fleet.
6. `api/go.mod` still has no kube client, and `git diff --name-only <base>..HEAD` lists nothing under `deploy/chart/` or `.github/workflows/`.

## Risks

- **A health page that cries wolf gets ignored.** Mitigations: age thresholds on every time-based check, the two-evaluation debounce before a notice, `warn` never notifies, a roll in progress is `ok`, and `board.drift` excludes the known false positives of #1482 by predicate.
- **A health page that reads green while blind.** Two checks depend on the run-health detector and one on the controller's report; each reports `unknown` when its writer is off or silent.
- **The health page is down exactly when the api is down.** Accepted and documented. That failure belongs to cluster monitoring and to the public `GET /api/health` probe. The `uzi admin health` exit codes distinguish "unhealthy" from "unreachable" so an external probe still works.
- **Information exposure.** The endpoint is admin-only and carries owner names and worker names. It must never be reachable by a `uzc_` token or a non-admin cookie (router-level test in M1), and nothing from it may leak into `GET /api/health` or `GET /api/version`.
- **Untrusted strings in an admin surface.** Worker names are owner-supplied and render to an admin. They are already gated at write time by `termsafe.Validate`; the M1 hostile-input test, the M3 bounded-cell test and the M4 attribute assertions keep it that way.
- **Multi-replica loop beats** (D8): with more than one api replica the `loops` check reports the answering process only.
- **Evaluation cost.** The endpoint is polled every 10 s per open admin tab and once a minute by the evaluator. Every check is an indexed count or an in-memory read; the plan states the query plan for the run-table counts, and the handler caches one evaluation for 5 s.
- **Run size.** Seven worker milestones across api, web and CLI. The milestones are ordered so that a run stopped after M3 still ships a complete, useful slice (endpoint plus CLI); `uzi run scope` can cap it there.
- **Scope creep toward a monitoring product.** No history, no charts, no per-check configuration, no actions in v1.

## Out of scope

- **Pod-level state of the api, web, database and controller pods.** It would need a Role in the release namespace, widening a file whose own header calls it the security boundary of the hosted-worker feature, and it cannot report the outages it would exist for. The right home is a `/metrics` endpoint with an opt-in ServiceMonitor and PrometheusRule in the chart, as a separate PRD.
- Version skew and forge sync freshness (see Resolved facts).
- New fields on the controller report (`pod_phase` exposure, pending PVCs).
- Beats for the remaining `main.go` goroutines.
- Any write action: restart, retry, roll back, cordon. `specs/human.md` keeps diagnostics read-only in v1.
- History, time series, per-check thresholds as settings, a recovery notice, a TUI surface (PRD #1251 owns the TUI andon primitives).
- Fixing issues #1482 and #1483. This PRD is correct with or without them.

## Scope and decision log

- **D1 (user, 2026-09-20): Health is the last admin tab**, after Branding, not the first. Entry points are therefore the sidebar pip and the Overview card, and the strip scrolls the active tab into view.
- **D2 (user, 2026-09-20): the Danger banner has a "Snooze 1 h".** Per admin, per episode.
- **D3 (user, 2026-09-20): non-admins get a platform line on Overview**, in this PRD.
- **D4: the non-admin line needs no new endpoint.** Workers are per owner, so "the platform cannot run my work" is already visible in the viewer's own worker list. This removed a planned user-scoped status endpoint and its trust question.
- **D5: no kube access, no chart change, no controller wire change.** Everything Kubernetes-derived already arrives over the existing report. This is what makes the feature cheap and keeps guardrail layers untouched.
- **D6: `unknown` and `na` are first-class severities.** A stale signal must not read green, a signal whose writer is switched off must not read green, and compose must keep working without the hosted checks vanishing.
- **D7: closed registry, server-authored text.** Same discipline as run-health reasons, and it carries over the existing refusal to relay Kubernetes free text.
- **D8: loop beats are in-process, injected from `main.go`.** Correct for a single api replica, which is the chart default (`api.replicaCount: 1`). With several replicas the check describes the answering process; persisting beats is a follow-up if that ever matters.
- **D9: exit status 8 for Danger**, as a sentinel error on a success path. Reusing 1 would make "unhealthy" indistinguishable from "the command failed", which defeats the probe use case.
- **D10: notices go through `notifysvc`**, not straight to Slack, so an admin without a linked Slack identity still gets it in the web inbox, and the existing enablement gate applies.
- **D11: no recovery notice and no edit-in-place message in v1**, following the custody episode precedent.
- **D12: `board.drift` filters on `issue_iid IS NOT NULL`** so this PRD does not depend on #1482.
- **D13: version skew and forge sync are deferred**, the first because it needs a workflow edit a worker cannot push, the second because the bookkeeping does not exist.
- **D14: episodes are explicit rows**, not the custody precedent's implicit per-owner row, because the episode id is exposed in the API and keys the per-admin snooze and the per-admin notice claim. The evaluator lands in M2 (open and close only) so the banner's snooze works before the notice milestone.
- **D15: the scheduler is one of the four beating loops.** A dead scheduler silently stops every scheduled job, a different failure from a user's pause-all.
- **D16 (review, 2026-09-20): the CLI is its own milestone (M3)**, split out of M1 so a late CLI failure cannot reopen the registry. Two reviewers (scope and trust boundary; adversarial fact-check) found no blocking issue and no refuted fact; their corrections are folded in above: the `worker_upgrade_reports` table name, the `fleet.disk` source, the `health_enabled` dependency, the eligible-branch wording of `forge.ciwatch`, the episode storage, the exit-code implementation site, and the live-DB package trap for `healthsvc` and `slacksvc`.
