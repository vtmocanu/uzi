# PRD #841: Layered enable/disable for MR review rework (per-run, per-schedule, CLI, settings)

**GitHub Issue**: [#841](https://github.com/vtmocanu/uzi/issues/841)
**Status**: Created 2026-08-30. Completed 2026-08-30 (all five milestones landed on branch `agent/issue-841`).
**Priority**: Medium — nothing is broken, but the MR review rework watcher is the one auto-acting feature with no per-run, per-schedule, or CLI control, so a user cannot disable auto-rework for a single run, start a run with it off, or run "auto-rework only for scheduled jobs".

> **Scope guard (uzi-bound PRD): this PRD MUST NOT touch `.github/workflows/**`.** The worker PAT lacks `workflow` scope, so any workflow-file edit in the branch diff loses the whole push (`.claude/rules/prds.md`). None of the work below needs one — CI's gate jobs already invoke the `task` targets, which auto-pick up new tests. Validate CI-adjacent behavior with in-memory fixtures, never a real file under `.github/workflows/`.

> **Anchors are cited by file + symbol/line, resolved 2026-08-30 against `main` (migration head `00177`).** Grep the symbol if a line has drifted. Every fact below is derivable from this repo's own source with zero internet — this PRD is self-contained for an offline worker.

## Problem

The **MR review rework** watcher (`docs/mr-review-watcher.md`, PRD #700) auto-reworks a completed issue run's open MR when it gains new review comments on a green pipeline. Today it has exactly **two** control layers:

- **Per-user default** — `users.mr_rework_enabled` (nullable, default-ON: `NULL`/absent reads as enabled). Read/written via `PUT /api/me/settings` (`api/internal/handler/user_settings.go:221-243`, query `SetUserMrReworkEnabled` in `queries/users.sql:70-77`) with a Settings web toggle (`web/src/pages/RunDefaults.tsx:112-127`, `toggleMrRework`).
- **Admin instance kill-switch** — `app_settings.mr_rework_enabled` (`settings.Cache.MrReworkEnabled`, `settings/settings.go:816-826`), read once per repo by the poller detector and fail-closed.

The sibling feature **usage-limit park** (`wait_on_limit`, the checkbox in the run view "Wait out future Anthropic usage limits on this run") has **five** layers: per-user default, **per-run flag**, **CLI-at-start flag**, **per-schedule override**, plus the web checkbox. `mr_rework` is missing the middle three. Consequence, quoting the issue:

- A user cannot disable auto-rework for **one** run (e.g. an MR they want to review entirely by hand).
- A user cannot **start** a run with auto-rework off from the CLI.
- A user cannot say "auto-rework **only** for my scheduled jobs" (global off, schedule on), or the reverse.

## Solution

Give `mr_rework` the same layered control `wait_on_limit` already proved out, mirroring its plumbing at every layer, with **one deliberate divergence** the user chose: the model is **live-inherit**, not snapshot.

### Resolution model (the core contract)

Two new **nullable** columns, `runs.mr_rework_enabled` and `run_schedules.mr_rework_enabled`, each tri-state: `NULL` = "inherit", `true`/`false` = explicit override. The watcher's candidate query resolves the effective value **live at read time** with a COALESCE up the chain, so flipping a higher layer affects un-overridden runs immediately:

```
effective(run) = COALESCE(run.mr_rework_enabled, owner.mr_rework_enabled) IS NOT FALSE
```

At run creation the run's column is stamped by **pointer coalescing only** (no user-default snapshot — that is what keeps it live-inherit):

```
runs.mr_rework_enabled := requestOverride ?? scheduleOverride    // both *bool; result may be NULL
```

- **Manual run** (`CreateRun`): `requestOverride` = the create request's `mr_rework_enabled *bool` (CLI `--mr-rework` / web), `scheduleOverride` = nil.
- **Scheduled run** (`CreateScheduledRun` / `CreateScheduledAutopilotRun` / `CreatePromptRun`): `requestOverride` = nil, `scheduleOverride` = the schedule's `mr_rework_enabled *bool`.
- **All other create paths** (`CreateAutopilotRun` label poller, `CreateAutoCIFixRun`, `CreateAutoMRReworkRun`, `CreateSelfImproveRun`, task/handoff): leave the column `NULL` → inherit owner default, **exactly today's behavior**.

This delivers all four user scenarios:

| Scenario | How |
|---|---|
| Disable auto-rework for one run | web checkbox / `uzi run mr-rework <id> --enabled=false` → `runs.mr_rework_enabled=false` |
| Start a run with it off | `uzi run create … --mr-rework=false` → `runs.mr_rework_enabled=false` |
| Disabled in general settings | `users.mr_rework_enabled=false` (exists today); un-overridden runs inherit false |
| Only for scheduled jobs | `users.mr_rework_enabled=false` **and** `schedule.mr_rework_enabled=true` → scheduled runs stamp true, manual runs inherit false |

### Why nullable, not `wait_on_limit`'s `NOT NULL DEFAULT` (D1)

`wait_on_limit`'s run/schedule columns are `boolean NOT NULL DEFAULT` and **snapshot** the owner default at creation (`resolveWaitOnLimit`, `workersvc/limitwait.go:697-706`). `mr_rework` is deliberately the opposite: the existing `users.mr_rework_enabled` is **nullable, default-ON via `IS NOT FALSE`**, and the user chose **live-inherit** (a global flip should reach un-overridden runs, and a run's toggle stays meaningful the moment the user forms the opinion — while looking at the MR). So both new columns follow the existing nullable default-ON shape, and the resolver is pure pointer coalescing (no `GetUserByID` snapshot). This is the single design difference from the `wait_on_limit` mirror; everything else mirrors it.

### Editable until the MR merges or closes (D2)

`wait_on_limit`'s per-run route guards `status NOT IN ('completed','failed','cancelled')` (`SetRunWaitOnLimit`, `runtime.sql:3167-3190`) because that flag governs an **in-flight** run. `mr_rework` is the reverse: the watcher acts **after** the run completes (during Human Review, once the MR has comments). So the per-run toggle must stay editable on a **completed** run, until its MR merges or closes. Realization:

- **Route/store**: `SetRunMrReworkEnabled` is owner-scoped with **no status guard** at all. The write is inert once the MR is merged/closed (the candidate query already excludes `mr_state != 'opened'`), so no explicit terminal guard is needed; a write on any owned run re-reads and returns the run, mirroring `SetRunWaitOnLimit`'s 404-vs-200 re-read shape.
- **Web visibility**: a `canToggleMrRework(run)` helper shows the checkbox only for `run.kind === 'issue'` with `mr_state` null or `'opened'` (RunDTO already carries `mr_state`, `apitypes/run.go:162`; web `Run.mr_state`, `web/src/lib/api.ts:553`).

## Baked-in facts (exact anchors, resolved locally)

**Current `mr_rework` surface (to extend):**
- Migration `api/internal/store/migrations/00165_user_mr_rework_enabled.sql:15` — `ALTER TABLE users ADD COLUMN mr_rework_enabled boolean;` (nullable, no default).
- Candidate query `api/internal/store/queries/mr_rework.sql`, `ListMRReworkCandidates :many`: `per_branch` CTE selects `r.branch, r.mr_iid, r.user_id, r.id AS source_run_id`; outer filter at line ~58 `AND u.mr_rework_enabled IS NOT FALSE`. **This is the line the COALESCE changes.**
- Auto-run creator `api/internal/workersvc/mr_rework.go:60` `CreateAutoMRReworkRun` (leaves the new column NULL → inherit).
- Detector `api/internal/poller/mr_review_watch.go` (unchanged — it reads the candidate list; the eligibility resolution lives entirely in the SQL).

**`wait_on_limit` mirror targets (the pattern for each new piece):**
- Migrations: `00091_run_limit_wait.sql:48-53` (runs col), `00103_run_schedules.sql:29` (schedule col), `00144_wait_on_limit_default_on.sql` (default flip — **not** mirrored; mr_rework is nullable default-ON already).
- Store queries: `SetUserWaitOnLimit` (`users.sql:49-60`), `SetRunWaitOnLimit :execrows` (`runtime.sql:3167-3190`), `CreateRun` stamp `@wait_on_limit` (`runtime.sql:378-380`), `CreatePromptRun` stamp (`schedules.sql:306-308`), schedule CRUD `@wait_on_limit` (`schedules.sql` CreateRunSchedule/CreateDefaultSchedule/ResetDefaultSchedule/UpdateRunSchedule).
- Resolver + stamp: `workersvc/limitwait.go:697-706` `resolveWaitOnLimit`; stamp site `workersvc/service.go:4968-4980` inside `createRun`; callers `CreateRun` (4717), `CreateScheduledRun` (4740), `CreateAutopilotRun` (4752, forces nil), `CreateScheduledAutopilotRun` (4773), `CreatePromptRun` (`prompt.go:39/57`).
- Handlers/routes: `handler/waitonlimit.go` (`SetUserWaitOnLimit`, `SetRunWaitOnLimit`); create-request field `handler/workers.go:849-858` `WaitOnLimit *bool`; mounts in `handler/handler.go` — `/me/wait-on-limit` (861) and `/runs/{id}/wait-on-limit` (1399) are in the **cookie-only RequireAuth** group; `POST /repos/{id}/runs` (1165) is in **RequireUser** (cookie OR `uzc_` Bearer).
- DTOs: `apitypes/run.go:344-348` `WaitOnLimit bool`; `apitypes/schedule.go:46` (request `*bool`), `:86` (DTO bool), `:206` (default-catalog DTO bool).
- CLI: `run.go:484-486` flag, `run.go:2163-2196` `waitOnLimitFlag` tri-state helper, `run.go:1563-1569` `run get` row; `schedule.go:499/771` flags, `:1331` `schedule get` row; `uzicli/client.go:1282-1304` `CreateRun` body `WaitOnLimit *bool json:"wait_on_limit,omitempty"`.
- Schedule plumbing: `schedtmpl/schedtmpl.go:58` `WaitOnLimit = true`; `schedsvc/scheduler.go:577-599` `createIssueRun` threading + `:538` prompt path; `handler/schedules.go` create (235/361), `applyCreateDefaults` (1233-1250), `mergeSchedule` (1268-1280), PATCH override (1103-1105/1323-1324), `defaultEditableDiverges` (1679-1699).
- Web: per-run checkbox `web/src/pages/RunView.tsx:452-473` (`canToggleWaitOnLimit`, `lib/limitWait.ts`), Settings toggle `RunDefaults.tsx:365-374`, schedule form `components/ScheduleModal.tsx:209/433/470/974`, list chip `pages/Schedules.tsx:605-607`, api methods `lib/api.ts:2998` (`setWaitOnLimit`), `:3543` (`setRunWaitOnLimit`).

**sqlc regen** after any `migrations/` or `queries/` edit (`.claude/rules/go.md`): `cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate`. A query mutation must land in the generated `internal/store/*.sql.go` const, not only the `.sql`.

**Migration numbering**: draft the new file as `00178_run_schedule_mr_rework.sql`; renumber to the next free number above the live head at the landing rebase (`gate:repo`'s `check:migration-numbering` enforces uniqueness). No migration comment may contain the literal `+goose` in any form.

## Milestones

Five milestones. M1 is the foundation; M2 depends on M1; M3 and M4 both depend on M2 and are parallelizable; M5 depends on all. Every milestone folds in its own component's tests and leaves that component's gate green.

- [x] **M1 — Schema, store, and live-inherit resolution (api core).** One migration `00178_run_schedule_mr_rework.sql` adding `runs.mr_rework_enabled boolean` and `run_schedules.mr_rework_enabled boolean` (both **nullable, no default** = inherit; Down drops both). `sqlc generate`. New query `SetRunMrReworkEnabled :execrows` in `runtime.sql` (owner-scoped `UPDATE runs SET mr_rework_enabled = @mr_rework_enabled, updated_at = now() WHERE id = @id AND user_id = @user_id` — **no status guard**, D2; accepts NULL to clear back to inherit). Stamp `@mr_rework_enabled` in `CreateRun` (`runtime.sql`) and `CreatePromptRun` (`schedules.sql`); add the schedule column to `CreateRunSchedule`/`CreateDefaultSchedule`/`ResetDefaultSchedule`/`UpdateRunSchedule`. Thread a `mrReworkEnabled *bool` param through `createRun` (stamp into `store.CreateRunParams`, sibling to `WaitOnLimit`), its callers `CreateRun`/`CreateScheduledRun`/`CreateScheduledAutopilotRun` (pass through; autopilot/ci_fix/self_improve/task pass nil), and `CreatePromptRun`. **The candidate-query change**: in `ListMRReworkCandidates` add `r.mr_rework_enabled` to the `per_branch` CTE and change the filter to `AND COALESCE(per_branch.mr_rework_enabled, u.mr_rework_enabled) IS NOT FALSE`. RunDTO gains `MrReworkEnabled *bool json:"mr_rework_enabled"` (`apitypes/run.go`), mapped in `runToDTO` (`handler/workers.go:361`). **Tests (store `*LiveDB`, `-count=1` mandatory):** the COALESCE resolution as a truth table — source run NULL + owner NULL → candidate; source NULL + owner false → excluded; source false + owner true → excluded (per-run override wins); source true + owner false → candidate (per-run override wins); **plus a reused-branch row**: two completed `kind='issue'` runs on the **same branch** (a re-run reuses the deterministic `agent/issue-N` branch), the NEWER stamped `false`, asserting the branch is excluded — because `ListMRReworkCandidates`'s `per_branch` CTE is `DISTINCT ON (r.branch) … ORDER BY r.branch, r.created_at DESC`, so **eligibility binds to the newest issue run per branch** and the per-run column is read from that row (an older run's toggle does not move eligibility; the web checkbox/CLI target the newest run, so this matches the surfaced control). Mutate the **generated const** in `store/mr_rework.sql.go` to prove the query gates on the new column (a fold applied only to `.sql` is inert; `.claude/rules/go.md`). **Offline-worker gate note:** the worker's LOCAL gate is `task gate:api` — these `*LiveDB` tests **compile** under `test:api` and **self-skip** without `UZI_TEST_DATABASE_URL`; their assertions execute in **CI's `test:api-store-it`** (a CI-only gate, per the root `CLAUDE.md`). Do **not** treat an inability to run the Docker-backed `./e2e/run-store-it.sh` locally (it prints a loud `INFRASTRUCTURE FAILURE … NO TESTS RAN` if no Postgres) as a red gate or a code failure — it is a missing local dependency, and CI provides the behavioral verification.

- [x] **M2 — HTTP routes, create wiring, schedule handler + scheduler (api).** `PUT /api/runs/{id}/mr-rework` handler in a new `handler/mrrework_run.go` (or fold into `waitonlimit.go`'s shape): body `{"enabled": bool|null}` (null clears to inherit, mirroring the settings PATCH), owner-scoped `SetRunMrReworkEnabled`, re-read `GetRunByIDForUser` → 404-vs-200, return `{"run": runToDTO(...)}`. **🔴 Mount it in the RequireUser group (cookie OR `uzc_` Bearer), NOT the cookie-only RequireAuth group** `wait_on_limit` uses — there is no lock-consent dimension here (D3), and the CLI verb in M3 needs Bearer access; add a **router-level** auth test (a fake-client unit test bypasses the router and cannot catch a mis-mount — the uzi-cli plan-trap). **Mirror the existing precedent set in `api/internal/handler/cli_auth_livedb_test.go`**: assert `PUT /runs/{id}/mr-rework` is reachable (not 401) over a `uzc_` Bearer like `TestCLIReachesSettingsOverBearerLiveDB` does, and add it to the cookie-only counter-set `TestCLIRejectsBearerOnCookieOnlyRoutesLiveDB` **only if** it were cookie-only (it must NOT be — so it belongs in the reachable set). These are `*LiveDB` tests (they run under CI's store-it sweep, per the M1 offline-worker note). The create-run handler (`handler/workers.go` `CreateRun`) reads a new request field `MrReworkEnabled *bool json:"mr_rework_enabled"` and passes it into the create path. Schedule layer: `ScheduleRequest.MrReworkEnabled *bool` and `ScheduleDTO.MrReworkEnabled *bool` and the default-catalog DTO field (`apitypes/schedule.go`); `schedtmpl` default = **inherit**, expressed by leaving the field **unset (nil zero-value of the `*bool`)** — NOT a `*bool` const (Go has none; the `WaitOnLimit = true` mirror at `schedtmpl.go:58` is a plain `bool` const, which does not apply here), so default jobs follow the user's global setting unless explicitly set; handler create/`applyCreateDefaults`/`mergeSchedule`/PATCH override/`CreateDefaultSchedule`-seed thread it; `defaultEditableDiverges` includes a nil-safe `*bool` compare; `schedsvc/scheduler.go` `createIssueRun` + prompt path thread `sched.MrReworkEnabled` into the create calls. **Tests:** handler route + auth-group test (RequireUser reachable with a `uzc_` token, 401 pattern only where expected), schedule create/edit/reset round-trip (`schedules_test.go`/`*LiveDB`), `schedsvc/scheduler_test.go` threading, `apitypes/wire_test.go` for the three new DTO fields.

- [x] **M3 — CLI (`api/cmd/uzi/`).** `--mr-rework` tri-state flag on `run create` with a `mrReworkFlag(cmd) *bool` helper mirroring `waitOnLimitFlag` (`run.go:2163-2196` — returns nil unless `Changed`, so an unset flag inherits rather than forcing false); wire it through `uzicli/client.go` `CreateRun` (new body field `MrReworkEnabled *bool json:"mr_rework_enabled,omitempty"`). A `MR_REWORK` row in `run get` output (`inherit`/`on`/`off` from the `*bool`). New verb `uzi run mr-rework <id> [--enabled[=false]]` → `PUT /api/runs/{id}/mr-rework` (this is the CLI half of the RequireUser route from M2). `--mr-rework` tri-state on `schedule create` and `schedule edit` (mirror `schedule.go:499/771` and the `Changed`-gated edit wiring), a `MR_REWORK` row in `schedule get`, and restore-on-`schedule reset`. **Tests:** `cmd/uzi/run_field_test.go`/`render_test.go`/`commands_test.go`/`schedule_test.go` and `uzicli/client_test.go`, mirroring the existing wait-on-limit CLI coverage.

- [x] **M4 — Web (`web/src/`).** Per-run checkbox in `RunView.tsx` ("Auto-rework this MR's review comments") gated by a new `canToggleMrRework(run)` (`kind === 'issue'` and `mr_state` null/`'opened'`), showing the effective value `run.mr_rework_enabled ?? user.mr_rework_enabled ?? true`, toggling via a new `api.setRunMrRework(id, enabled)` → `PUT /runs/${id}/mr-rework`. Schedule control in `ScheduleModal.tsx` (a `Toggle` beside the wait-on-limit one, initialized to the effective default, writing an explicit `mr_rework_enabled` in the create/update payload). Extend the web `Run` type with `mr_rework_enabled?: boolean | null` (`lib/api.ts`, mirroring `mr_state`), the schedule request/DTO types likewise; add the mock-API method + fixtures (`web/src/mocks/`). The user-default Settings toggle already exists (`RunDefaults.tsx`) — no change beyond confirming it still round-trips. **Tests (vitest):** `RunView.test.tsx` (checkbox shown only on an issue run with an open MR, hidden on merged/closed, owner vs non-owner), `ScheduleModal.test.tsx`, mock-API test. Non-owner and terminal-MR states must be **positively** asserted (avoid a vacuous negative; `.claude/rules/web.md`).

- [x] **M5 — Docs, specs, gates, acceptance.** Update `docs/mr-review-watcher.md` **Enablement** section to document the three new layers and the resolution order, **including a one-line note that the per-run toggle binds to a branch's newest issue run** (if a branch is reused by a re-run, the newest run's setting governs eligibility) — user-facing doc: **no em dashes**, then run `task docs:sync` and commit the `api/internal/uzidocs/embed/` mirror (`TestEmbeddedDocsMatchSource` gates `test:api` on byte drift). Note the new layers in `ARCHITECTURE.md` (run-lifecycle / mr_rework area) and add a `specs/ai.md` design-decision entry (do **not** edit `specs/human.md` without user approval — a new user-facing capability routes through the user; the spec-keeper decides whether a `human.md` line is warranted and asks). Check `api/cmd/uzi/` parity was satisfied by M3 (the "new functionality ⇒ CLI" convention). Full gate green: `task gate:api`, `task gate:web`, `task gate:controller` (only if touched — it should not be), plus the live-DB sweep for M1/M2. Confirm `git diff --name-only <base>..HEAD` shows **zero** entries under `.github/workflows/`.

## Decision Log

- **D1 — Both new columns are nullable (inherit), not `NOT NULL DEFAULT` like `wait_on_limit`.** The existing `users.mr_rework_enabled` is nullable default-ON, and the user chose **live-COALESCE inherit** over snapshot: a global setting flip should reach un-overridden runs, and the run toggle is formed while looking at the MR, post-completion. So the resolver is pure `requestOverride ?? scheduleOverride` pointer coalescing (no owner-default snapshot), and eligibility is `COALESCE(run, owner) IS NOT FALSE` at read time. `(user 2026-08-30)`
- **D2 — The per-run toggle is editable until the MR merges/closes, not only while the run is non-terminal.** The watcher acts after the run completes, so a terminal-status guard (which `SetRunWaitOnLimit` carries) would lock the toggle exactly when it matters. `SetRunMrReworkEnabled` has no status guard; the flag is inert once `mr_state != 'opened'`, and the web hides the checkbox then. `(user 2026-08-30)`
- **D3 — The per-run route is RequireUser (cookie OR `uzc_`), diverging from `wait_on_limit`'s cookie-only route.** `wait_on_limit`'s per-run route is cookie-only because parking a run consumes resources (issue lock + worker disk), so consent must be interactive. Toggling `mr_rework` is a pure preference with no resource-consent dimension, and the user asked for CLI control ("disable it for a current run"), which needs a Bearer-reachable route. Guarded by a router-level auth test to pre-empt the cookie-only plan-trap. `(user 2026-08-30)`
- **D4 — No ADR; the rationale lives here.** `wait_on_limit` (PRD #35) has no ADR either; the resolution precedence is recorded in this Decision Log. The COALESCE invariant is guarded by the M1 mutation test rather than a prose ADR.
- **D5 — The schedule's `mr_rework` default is inherit (nil), not on.** Seeding default jobs to an explicit `true` would silently override a user who disabled `mr_rework` globally the moment they use a default schedule (least-surprise). Nil means default jobs follow the user's global setting; the "only for scheduled jobs" scenario is reached by the user explicitly setting the schedule to `true`. `(2026-08-30)`

## Risks

- **The candidate-query COALESCE is the whole feature and is invisible until a live-DB test exercises it.** A green `sqlc generate` + `go build` proves nothing about eligibility (`.claude/rules/go.md`: sqlc's type deduction is not Postgres's, and a mutation on `.sql` is inert). Mitigated by the M1 truth-table `*LiveDB` test mutating the generated const, with `-count=1`.
- **Offline Auto-worker may misread the Docker-backed live-DB harness as a code failure.** `./e2e/run-store-it.sh` needs a throwaway Postgres (Docker); a worker without Docker-in-Docker gets a loud `INFRASTRUCTURE FAILURE … NO TESTS RAN` non-zero exit that looks like a red gate. The `*LiveDB` truth-table and router-auth tests **compile** under `task gate:api` / `test:api` and **self-skip** without `UZI_TEST_DATABASE_URL`, so the worker's local gate is `task gate:api` (compile-level coverage), and the behavioral assertions run in **CI's `test:api-store-it`**. The worker must not treat a locally-unrunnable store-it sweep as a failure. (See the M1 offline-worker note.)
- **Touching `createRun`'s signature ripples to every caller.** `mrReworkEnabled *bool` must be threaded to `CreateRun`/`CreateScheduledRun`/`CreateScheduledAutopilotRun`/`CreateAutopilotRun`/`CreatePromptRun` and left nil on the auto/ci_fix/self_improve/task paths so their behavior is byte-identical to today. Mitigated by mirroring the exact `waitOnLimit` call sites (surface-mapped above) and by `go build` across the api module.
- **Cookie-only plan-trap on the new route (D3).** A route mounted in RequireAuth would 401 the CLI verb at runtime while every unit test passes. Mitigated by the explicit RequireUser mount + a router-level auth test.
- **DTO growth on `RunDTO`/`ScheduleDTO`.** Three new fields; `apitypes/wire_test.go` is the gate that forces each to be deliberate. The web `*bool | null` typing must preserve the omitted-vs-null-vs-value distinction (no `any` cast), mirroring `mr_state`.
- **Docs mirror drift.** Editing `docs/mr-review-watcher.md` without `task docs:sync` reddens `test:api` via `TestEmbeddedDocsMatchSource`. Mitigated by running the sync in M5 and committing the mirror.

## Acceptance

1. A user can disable auto-rework for a **single** completed run whose MR is still open, from the web checkbox and from `uzi run mr-rework <id> --enabled=false`; a subsequent poll tick does not queue an `mr_rework` run for that MR. Re-enabling (or clearing to inherit) restores it while the MR is still open.
2. `uzi run create --repo <id> --issue <iid> --mr-rework=false` produces a run whose MR is never auto-reworked; omitting the flag inherits the owner default; `--mr-rework` forces it on.
3. With `users.mr_rework_enabled=false` (global off) and a schedule's `mr_rework_enabled=true`, **scheduled** runs' MRs are auto-reworked while **manual** runs' MRs are not — proven by the M1 truth-table test and an end-to-end schedule fire.
4. The eligibility resolution is `COALESCE(run.mr_rework_enabled, owner.mr_rework_enabled) IS NOT FALSE`, proven by a `*LiveDB` test whose four rows cover per-run-wins-over-owner in both directions, with the mutation applied to the generated const.
5. The per-run route is reachable with a `uzc_` Bearer token (RequireUser), proven by a router-level auth test.
6. `docs/mr-review-watcher.md` documents the layered control and resolution order (no em dashes), the `uzidocs` mirror is regenerated and committed, `ARCHITECTURE.md` and `specs/ai.md` are updated, `specs/human.md` is untouched, and `git diff --name-only <base>..HEAD` shows zero `.github/workflows/` entries.
7. All gates green: `task gate:api`, `task gate:web`, the live-DB sweep, and `task gate:controller` only if it was touched (it should not be).

## Parallelization plan

| Phase | Milestone(s) | Depends on | Files touched (component) |
|---|---|---|---|
| 1 | M1 | — | migration, `queries/{runtime,schedules,mr_rework}.sql`, `store/*.sql.go`, `workersvc/{service,prompt}.go`, `apitypes/run.go`, `handler/workers.go` (api) |
| 2 | M2 | M1 | `handler/{mrrework_run,workers,schedules}.go`, `handler.go` routes, `apitypes/schedule.go`, `schedtmpl`, `schedsvc/scheduler.go` (api) |
| 3a | M3 | M2 | `cmd/uzi/{run,schedule}.go`, `uzicli/client.go` (CLI) |
| 3b | M4 | M2 | `web/src/{pages/RunView,components/ScheduleModal}.tsx`, `lib/api.ts`, `mocks/` (web) |
| 4 | M5 | M1–M4 | `docs/`, `uzidocs/embed/`, `ARCHITECTURE.md`, `specs/ai.md` |

M3 and M4 touch disjoint components (CLI vs web) and can run in parallel once M2's routes and DTOs exist. Driven as one gated uzi run (Auto mode), the worker implements them in frozen-milestone order; the table is the dependency rationale.
