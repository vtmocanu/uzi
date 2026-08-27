# PRD #589 — Default scheduled jobs (catalog, multi-repo, clone)

**Issue**: [#589](https://github.com/vtmocanu/uzi/issues/589)
**Status**: Complete — landed on branch `agent/issue-589` (2026-08-22)
**Priority**: High
**Author**: design session, 2026-08-22
**Follow-up**: PRD-B (self-improvement as a default job) is split out — see the sibling issue. This PRD builds the catalog + enablement machinery; PRD-B adds the `self_improve` catalog entry and retires the bespoke engine on top of it.

---

## Problem

uzi ships **no ready-made schedules**. A user connects a forge and faces a blank Schedules page, with no starting point for the standing automations the dev team already runs (bug-hunt, docs hygiene, test improvement, bug/Planned sweeps, feature-bingo). This resolves brainstorm **#303** (seed the standing automations for new forges). It also fixes **#341** (a report-only scheduled `prompt` run that makes no commits still opens an empty MR), which the report-only default jobs would otherwise hit nightly.

## Solution overview

Ship a **catalog of default jobs** — a `go:embed`'d set (mirroring the builtin **agent** templates, `api/internal/agenttmpl/builtins`) whose **prompt is baked and read-only**, while cadence, model, and run options are editable with **Reset to default**. A user **enables a default per repo**, which materializes a real `run_schedules` row stamped `origin='default'`. Because a default row's prompt is **stored by reference** (`catalog_slug`, resolved from the catalog at fire time), a shipped prompt improvement reaches every enabled default automatically on the next release — sidestepping the agent-template gap #201 documents (`ON CONFLICT DO NOTHING` never updates a seeded row).

Layered on top:
- **Multi-repo enablement** — enable a default (or create a custom schedule) against several repos at once → one independent `run_schedules` row per repo (scalar `repo_id`), each tunable/pausable separately. Applies to custom jobs too.
- **Clone to custom** — clone any job (default or custom) into a new `origin='user'` row; cloning a **default lifts the prompt lock** (copies the baked text into an editable row). Serves both "customize a default" and "replicate to another repo".
- **Two-tab Schedules page** — **Default jobs** / **My schedules**, both rendered in the same table for consistency (uzi's `AdminShell` tab pattern). Default rows carry a `DEFAULT` chip, a 🔒 baked-prompt marker, and Reset/Clone actions; a default enabled on several repos shows one summary row that **expands into per-repo sub-rows** (layout A).
- **Sweep-label guardrail** — enabling a `sweep` default whose label does not exist on the target repo warns and offers to create it via `forge.EnsureLabels`.

### Guardrail preserved (why there are no repo-less rows)

`run_schedules.repo_id` is `uuid NOT NULL REFERENCES repos(id)` (migration `00103_run_schedules.sql:17`), and the create endpoint is `POST /repos/{id}/schedules` (`handler.go:1084`). The catalog approach **needs no relaxation of that**: a default is a template that materializes a row only when enabled *for a repo*, so a schedule still never exists without a repo. Deliberate divergence from #303's "seed paused repo-less rows" framing, and simpler (no NULL `repo_id`, no per-forge seeding trigger/idempotence).

**Complexity trade-off (honest framing):** the catalog approach trades seed-time complexity for **fire-time + reconciliation complexity** — prompt-by-reference resolution in the fire path (M2), a `customized`/Reset divergence model mirroring the agent-template builtin machinery, and idempotent enable per user×repo×slug plus multi-repo fan-out. It is simpler on the DB invariant and forge-seeding axes, not simpler outright.

---

## Decisions (Decision Log)

1. **Catalog, not seeded rows.** Defaults are a `go:embed`'d catalog; enabling materializes a `run_schedules` row. No repo-less rows, no schema relaxation for repo. (Resolves #303 gap 1.)
2. **Prompt stored by reference.** A `default`-origin row carries `catalog_slug`, not a prompt copy; the scheduler resolves the baked prompt/labels/guidance from the catalog at fire time. Shipped prompt fixes reach enabled defaults automatically. Editable fields (cron, tz, model, auto_approve, wait_on_limit, max_issues) are stored per-row; a `customized` flag gates Reset. (Sidesteps #201.)
3. **Prompt is locked for defaults; clone to edit.** The web modal shows a default's prompt read-only with "clone to edit". Clone copies the baked text into an `origin='user'` row where it is editable.
4. **All catalog prompts are genericized** to work on any repo (feature-bingo writes to an `ideas/` folder, creating it if absent; no `api/internal/...` or `check-docs.mjs` hardcodes). (Resolves #303 gap 2.)
5. **Defaults ship `auto_approve = true`, `wait_on_limit = true`.** They produce PRs, never merge, so an unattended off-hours run is acceptable; a human still merges. (Design session; overrides #303 gap 6.)
6. **Sweep enable checks labels.** On enabling a `sweep` default (or creating/editing a sweep schedule) whose label is missing on the target repo, warn and offer to create it via `forge.EnsureLabels`. (Resolves #303 gap 3.)
7. **Cadence via cron approximation.** Catalog cadences are 5-field cron. No new interval-timing mode.
8. **Multi-repo layout: one row, expandable per-repo (layout A).**
9. **Fix #341 here; close #317 separately as a duplicate of #396.** #341 (empty MR on report-only prompt) is real and blocks the report-only defaults. #317 (resume replays a missed slot) is **already fixed**: `handler/schedules.go:330-350` re-arms `next_fire_at` on an enabled-only recurring resume via `ResumeRecurringSchedule` (landed for #396); both the CLI (`uzicli/client.go` sends a minimal `{enabled}` body so `onlyEnabled` fires) and web (`Schedules.tsx` PATCHes only `{enabled}`) route through it. The re-arm fires only on an **enabled-only** PATCH (a combined config+resume would fall to the plain flip; neither surface does that). Verified 2026-08-22. Close #317; no code.
10. **Self-improvement is out of scope here — PRD-B.** The catalog ships **6** generic entries (prompt + sweep). The `self_improve` target, its 7th catalog entry, the engine retirement, the per-user fold, the boot migration, and the instance-wide-unique-index fix all live in PRD-B, built on this PRD's machinery.

---

## Current state (verified 2026-08-22 — baked for an offline worker)

**Scheduler (keep, extend).**
- Table `run_schedules` — `api/internal/store/migrations/00103_run_schedules.sql`: `repo_id uuid NOT NULL`, `target` (`issue|sweep|prompt`, CHECK `run_schedules_target_shape` at `:37-41` hard-requires `target='prompt' AND prompt IS NOT NULL`), `timing`, `cron_expr`, `run_at`, `timezone`, `next_fire_at`, `last_fired_at`, `auto_approve` (default true), `wait_on_limit`, `enabled`, `status`. Later migrations: `00104` (`runs.schedule_id`, `prompt` kind), `00111` (`max_issues`, `guidance`), `00118` (`model`), `00119` (`override_subagent_model`), `00122` (`last_fire` jsonb). **Current migration head is `00150` (`00150_github_project_link_done_option.sql`); new migrations rename to the next free number above the live head at merge** (see CLAUDE.md).
- Service `api/internal/schedsvc/`: `scheduler.go` (`Boot` + `tick` → `ClaimDueSchedules` → `fireOne` (switches on `issue|sweep|prompt`, parks anything else, `:230-242`) → `fireIssue`/`fireSweep`/`firePrompt` → `advance`/`park`). The `Scheduler` struct (`:130-152`) has **no catalog dependency today**. `firePrompt` reads the prompt straight off the row and derives the title first: `title := promptTitle(sched.Prompt.String)`, `prompt := sched.Prompt.String` (`:401,410-411`). `cron.go` (`NextFire`), `presets.go`, `last_fire.go`, `outcome.go`, `skip_reason.go`. Wired `api/cmd/server/main.go:674`.
- Queries `api/internal/store/queries/schedules.sql`: `CreateRunSchedule`, `GetRunScheduleForUser`, `ListRunSchedulesForUser`, `UpdateRunSchedule`, `SetRunScheduleEnabled`, `ResumeRecurringSchedule`, `DeleteRunSchedule`, `ClaimDueSchedules`, `AdvanceSchedule`, `SetRunScheduleStatus`, `ListSweepCandidateIssues`, `HasActiveRunForSchedule`, `CreatePromptRun`.
- Handlers `api/internal/handler/schedules.go`: `CreateSchedule`, `ListMySchedules`, `GetSchedule`, `PatchSchedule`, `DeleteSchedule`, `RunScheduleNow`, `PreviewSchedule`. Routes (`handler.go`): `GET /api/me/schedules`, `POST /api/schedules/preview`, `GET|PATCH|DELETE /api/schedules/{id}`, `POST /api/schedules/{id}/run-now`, `POST /api/repos/{id}/schedules`.
- DTOs `api/internal/apitypes/schedule.go`. Web: `web/src/pages/Schedules.tsx`, `web/src/components/ScheduleModal.tsx`, `web/src/lib/api.ts` (`Schedule*` types), `web/src/lib/schedulePresets.ts`. CLI: `api/cmd/uzi/schedule.go`.

**#341 (fix).** `agent/src/runner.ts`: declared `report_only` opens no branch/MR (~1163-1211, issue #299); the **undeclared** zero-diff guard is `issue`-only (`if ((claim.kind ?? "issue") === "issue")`, ~1237); a `prompt` run that commits nothing and did not declare `report_only` falls through to the prompt push/MR path (~1346, branch naming ~2287) and opens an empty MR. Fix: extend the undeclared-zero-diff handling to `kind='prompt'` → complete report-only (no branch, no MR). Leave `issue`/`ci_fix` unchanged (they are expected to commit). Align with ADR `0279-report-only-completion.md`.

**Forge label capability (decision 6).** `api/internal/forge/forge.go:381-385`: `ListLabels(ctx, projectID)` and `EnsureLabels(ctx, projectID, labels)` exist on the `Forge` interface (all three drivers implement them). No new forge method needed.

**Pattern to copy (decisions 1-3).** Builtin agent templates: `api/internal/agenttmpl/builtins/*.md` (`go:embed`), reconcile + `RefreshPristineBuiltin` + `customized` flag + `ResetBuiltinAgentTemplate` (`api/internal/store/agent_templates_builtins.go`, PRD #275). Parse/validity tests guard the embedded set; the `agent_templates_builtins_test.go` precedent tests the **query** directly (a LiveDB test cannot reach boot wiring), which M2's reset test should mirror.

**Testing discipline (from `.claude/rules/go.md`).** `task gate:api` runs `test:api` with `UZI_TEST_DATABASE_URL` **unset**, so every `*LiveDB` test self-skips — schema/CHECK/fire-path behavior is only exercised via `./e2e/run-store-it.sh` (which hardcodes `-count=1`). The catalog `go:embed` files live **inside** `api/internal/schedtmpl/`, so they are Go source inputs and **do** invalidate the test cache — the cross-module-boundary cache hazard does not apply here; the risk is a **non-discriminating** test (see M2).

---

## The default catalog (6 entries, genericized)

Each entry: `slug`, `name`, `description`, `target`, baked `prompt` (or sweep `labels`+`guidance`), `cron`, `timezone` (default `UTC`), `model`, `auto_approve=true`, `wait_on_limit=true`.

| slug | name | target | cron | model | notes |
|---|---|---|---|---|---|
| `test-improvement` | Weekly test improvement | `prompt` | `0 8 * * 1` | default | report-only weeks allowed (needs #341) |
| `docs-hygiene` | Docs hygiene | `prompt` | `0 3 * * 1` | default | mechanical doc fixes; report-only (needs #341) |
| `bug-hunt` | Bug hunt — deep audit | `prompt` | `0 4 * * 3` | default | one subsystem, reviewer+auditor confirm; report-only (needs #341) |
| `feature-bingo` | Feature bingo | `prompt` | `0 3 * * 2` | fable | writes an idea file under an `ideas/` folder |
| `bug-triage` | Bug triage sweep | `sweep` (`bug`) | `0 2 * * *` | default | `max_issues=3`; label-check (decision 6) |
| `planned-sweep` | Planned-work sweep | `sweep` (`Planned`) | `0 2 * * *` | default | `max_issues=3`; label-check (decision 6) |

Prompts are authored generically in M1. A report-only prompt must instruct "open no MR, leave a note" and rely on the #341 fix so it does not leave an empty MR. (The 7th entry, `self-improve`, is added by PRD-B.)

---

## Scope constraints

- **No `.github/workflows/**` changes** in implementation or validation (the worker PAT lacks `workflow` scope; a workflow-file touch is an atomic push rejection — see `.claude/rules/prds.md`). Verified: this PRD needs none — it adds schema/handlers/UI/CLI/docs + one `agent/` fix, all gated by existing `task` targets CI already invokes. No new toolchain, no new CI job.
- **No `self_improve` target, no engine changes** — that is PRD-B.
- Migrations get final numbers at merge (next free above the live head, currently `00150`).

---

## Milestones

Ordered; each is independently landable and **gate-green including its own tests** (first coverage lands with the code, per `.claude/rules/go.md` — there is no separate test milestone).

### M1 — Schema + catalog foundation — DONE (this branch)
- Migration on `run_schedules`: add `origin text NOT NULL DEFAULT 'user'` (`user|default`, CHECK), `catalog_slug text` (NULL for user rows), `customized boolean NOT NULL DEFAULT false`. **Relax `run_schedules_target_shape`** (DROP/re-ADD) so `prompt`/`labels` may be NULL **only when `origin='default'`**; a `user`-origin row still hard-requires its prompt/labels. Backfill: existing rows are `origin='user'` (default covers it).
- `go:embed` catalog package `api/internal/schedtmpl/` with the 6 entries; a parse/validity test (valid cron, known target, non-empty prompt for prompt / labels for sweep), mirroring the agenttmpl builtins test.
- `sqlc generate` (CI asserts the regenerate is a no-op).
- **DoD tests (LiveDB, via `./e2e/run-store-it.sh`):** migration applies; a `default`-origin row with NULL prompt is accepted; a `user`-origin row with NULL prompt is **rejected** (the negative case that justifies gating the relaxation on `origin`).
- **Deps**: none. **Gate**: `task gate:api` + store-it sweep.

### M2 — Catalog API + prompt-by-reference resolution + Reset — DONE (this branch)
- Inject a catalog resolver into the `Scheduler` struct. In `firePrompt`/`fireSweep`, when the fired row is `origin='default'`, **resolve prompt/labels/guidance from `catalog_slug` BEFORE deriving the title** (today's order reads `sched.Prompt.String` first, which for a NULL default prompt yields the "Scheduled prompt run" fallback + empty body — must reorder). Editable fields still come from the row.
- Store queries + endpoints: `GET /api/schedule-catalog` (catalog + per-repo enablement state), `POST /repos/{id}/schedule-catalog/{slug}` (enable → one `default`-origin row; idempotent per user×repo×slug), `POST /api/schedules/{id}/reset` (restore editable fields to catalog defaults, `customized=false`). Owner-scoped; repo ownership via `repoForRequest`.
- `customized` set when an editable field diverges from the catalog default (in `PatchSchedule`/`UpdateRunSchedule`).
- **DoD tests (LiveDB):** **discriminating** prompt-by-reference — assert the enabled `default` row stores **NULL** `prompt`, then set the row's `prompt` column to a poison value and assert the fire still uses the **catalog** text (i.e. `fireOne` resolves via `catalog_slug`, not the column); Reset restores editable fields and clears `customized`; enable is idempotent (same user×repo×slug twice → one row).
- **Deps**: M1. **Gate**: `task gate:api` + store-it sweep.

### M3 — Multi-repo enablement + clone (api + CLI) — DONE (this branch)
- Enable-across-repos: **client-side fan-out chosen** — multi-repo enable is N independent calls to the existing idempotent per-repo endpoint (`POST /api/repos/{id}/schedule-catalog/{slug}`), no new batch server route. The CLI's `schedule catalog enable <slug> --repo A --repo B` loops the per-repo call once per `--repo`. Documented in the CLI command's `Long` help + `EnableCatalogSchedule` client comment + `SKILL.md`/`docs/cli.md`. (The web M6 will do the same loop.)
- Custom create across-repos: **same client-side fan-out** — `uzi schedule create` takes `--repo` **repeatably** (`Flags().StringArray("repo", …)`) and loops one independent `CreateSchedule(repoID, req)` per `--repo`, yielding one `run_schedules` row per repo. A single `--repo` is the unchanged one-create path (dumps the single DTO). Both fan-outs accumulate per-repo results and, on a mid-loop failure, print what already landed before the error propagates (both endpoints are idempotent/safe to retry). Documented in the create command's `Long` help + `SKILL.md`/`docs/cli.md`; covered by `TestScheduleCreateFanOut` / `TestScheduleCreateSingleRepo` / `TestScheduleCreateFanOutPartialFailure`.
- Clone: `POST /api/schedules/{id}/clone` (RequireUser, in the `/schedules` group) → a new `origin='user'` row copied from the source; a cloned **default** resolves its catalog job by `catalog_slug` and copies the baked `Prompt` (prompt target) or `Labels`+`Guidance` (sweep target) into the new row's columns (lock lifted), setting `catalog_slug=NULL`, `customized=false`. Optional `{"repo_id"}` body clones into a different owned repo (replication path); absent → source's repo. No new SQL query — `CreateRunSchedule` already covers every needed column (yields `origin='user'`). `apitypes.ScheduleCloneRequest` added.
- CLI: `uzi schedule catalog list|enable`, `uzi schedule reset`, `uzi schedule clone` added to `api/cmd/uzi/schedule.go`; client methods `ListScheduleCatalog`/`EnableCatalogSchedule`/`ResetSchedule`/`CloneSchedule` on `Client`+`HTTPClient`+`FakeClient`; `docs/cli.md` + `SKILL.md` updated (skill_drift parity green).
- **DoD tests:** multi-repo enable fan-out = N rows (`TestEnableCatalogTwoReposLiveDB` + CLI `TestScheduleCatalogEnableFanOut`); multi-repo custom-create fan-out issues N creates in order, a single `--repo` issues one, and a mid-loop failure still reports the landed schedules (CLI `TestScheduleCreateFanOut` / `TestScheduleCreateSingleRepo` / `TestScheduleCreateFanOutPartialFailure`); clone unlocks prompt (`TestCloneDefaultPromptUnlocksLiveDB`: `origin='user'`, `catalog_slug=NULL`, editable prompt copied from catalog, then edited); sweep selector copy + user-row copy + clone-to-repo + foreign-repo 404 covered.
- **Deps**: M2. **Gate**: `task gate:api`.

### M4 — Sweep-label guardrail — DONE (this branch)
- Two repo-scoped forge primitives, behind the per-user forge limiter in the repos `RequireUser` group: `POST /api/repos/{id}/labels/check` (body `{"labels"}` → `{"missing"}`, the WARN — `ListLabels` then a case-sensitive diff matching how the drivers decide label existence) and `POST /api/repos/{id}/labels/ensure` (body `{"labels"}` → `{"ensured"}`, the CONFIRM — `EnsureLabels` with a default color). Owner-scoped via `repoForRequest` (404 for a foreign repo). Handler `api/internal/handler/labels.go`; DTOs `api/internal/apitypes/schedule.go`; route-limiter rows added to `route_limiter_mounts_test.go`.
- CLI: `Client.CheckRepoLabels`/`EnsureRepoLabels` on `HTTPClient`+`FakeClient`. The guardrail is wired into `schedule catalog enable` (selector resolved from the catalog `CatalogEntryDTO.Labels` for a sweep slug), `schedule create` (selector from the `--label` values), and — closing the "editing" case named above — `schedule edit` (when the edit provides `--label` on a sweep target; the repo id and target come from the schedule `edit` already GETs, so no extra fetch), each with a `--create-missing-labels` flag: without it, warn only (to stderr, so `--json` stdout stays clean) and still enable/create/edit; with it, `EnsureRepoLabels` the missing labels first, then proceed.
- **Never blocks the write, graceful-degrade even on the guardrail's OWN forge errors** (M4 fixup): `runSweepLabelGuardrail` is purely advisory and returns no error. A failed label CHECK (expired token, rate limit, forge unreachable) prints a `WARNING` to stderr and proceeds; a failed label CREATE (with `--create-missing-labels`) likewise warns and proceeds (the label is idempotently retryable). This preserves the pre-M4 property that `catalog enable`/`create` succeed with the forge down — the enable computes `next_fire_at` from the catalog cron and reads nothing else from the forge. `SKILL.md`/`docs/cli.md` updated to state the invariant is now actually true, including on forge error; skill-drift parity green.
- **DoD tests:** fake-forge unit tests (`labels_test.go`: `TestMissingLabels`, `TestEnsureRepoLabels{,Empty,ForgeError}`); owner-scoping 404 LiveDB (`labels_livedb_test.go`: `TestRepoLabelsForeignRepo404LiveDB`); CLI warn/create/no-op (`schedule_catalog_test.go`: `TestScheduleCatalogEnableSweep{WarnsOnMissingLabel,CreatesMissingLabel,NoMissingLabel}`, `TestScheduleCatalogEnablePromptNoGuardrail`); never-blocks-on-forge-error (`TestScheduleCatalogEnableSweepProceedsOn{Check,Ensure}Error`); edit-path guardrail (`TestScheduleEditSweep{WarnsOnMissingLabel,CreatesMissingLabel}`, `TestScheduleEditNoLabelChangeNoGuardrail`). `internal/store` untouched (forge I/O, no query change).
- **Deps**: M2. **Gate**: `task gate:api` (green).

### M5 — #341: report-only prompt opens no MR (agent-only, independent) — DONE (this branch)
- `agent/src/runner.ts`: extend the undeclared zero-diff handling to `kind='prompt'` → complete report-only (no branch, no MR) instead of opening an empty MR. Leave `issue`/`ci_fix` unchanged.
- **DoD tests:** a `node --test` runner test — a zero-commit `prompt` run opens no MR; a committing `prompt` run still opens one. Close #341.
- **Deps**: none (Phase 1). **Gate**: `task gate:agent`.

### M6 — Web: two-tab Schedules page + layout A + sweep-warn UI — DONE (this branch)
- Tabs via the `AdminShell` pattern (`web/src/components/AdminShell.tsx:32-50`: container `flex gap-1 overflow-x-auto border-b border-edge`, active tab `border-brand text-fg`, inactive `border-transparent text-muted`). Both tabs render the same table (`Target · When · Next run · Last run · Options · On`).
- Default tab: repo-guardrail bar (enable disabled until a repo is chosen), `DEFAULT` chip, 🔒 baked-prompt (read-only in the modal, "clone to edit"), Reset + Clone actions; a default on N repos = one summary row expanding into per-repo sub-rows (layout A) with per-repo cadence/toggle/run-now/remove + "enable on another repo".
- My schedules tab: today's list + a Clone action (cloned-from label). Modal: multi-repo select ("N repos → N schedules"); prompt read-only for a default, editable for custom/clone.
- **Sweep-warn UI (owns success criterion 6):** enabling a sweep default whose label is missing shows the warn + "create label" confirm from M4.
- **DoD tests:** vitest — tab switch renders the right set; default row shows DEFAULT chip + locked prompt; enable disabled with no repo; clone modal shows editable prompt; sweep-warn renders. Follow the copy-change / negative-assertion discipline in `.claude/rules/web.md`.
- **Deps**: M2, M3, M4. **Gate**: `task gate:web`.

### M7 — Docs + final integration sweep — DONE (this branch)
- `docs/scheduling.md`: Default jobs section (catalog, enable-per-repo, locked prompt, Reset, Clone, multi-repo, auto-approve default, sweep-label warn). Update `ARCHITECTURE.md` (note the default-job catalog) and `docs/cli.md` (new verbs); record decisions in `specs/ai.md`. `check-docs` (`web/scripts/check-docs.mjs`) must pass.
- Final full `./e2e/run-store-it.sh` sweep across the landed milestones.
- **Deps**: M3, M4, M6. **Gate**: `task gate:web` (check-docs), `task gate:repo`, store-it sweep.

---

## Parallelization plan

| Phase | Milestones | Rationale |
|---|---|---|
| 1 | **M1** (schema+catalog), **M5** (#341, agent-only) | M1 unblocks everything; #341 touches only `agent/` and is fully independent. |
| 2 | **M2** (catalog API + fire-path resolution) | Depends on M1; the single riskiest change (hot-path resolver reorder + struct change). |
| 3 | **M3** (multi-repo+clone), **M4** (sweep-label) | Both depend on M2, touch mostly separate files. |
| 4 | **M6** (web) | Depends on M2/M3/M4 DTOs + the sweep-warn payload. |
| 5 | **M7** (docs + final sweep) | After behavior lands. |

## Risks

- **Fire-path resolver is a hot path** (`firePrompt` at `scheduler.go:401,410`). The reorder (resolve-before-title) and the new `Scheduler` dependency must not regress non-default prompt/sweep rows. Mitigate: keep `origin='user'` rows on the exact current path; the resolver only engages for `origin='default'`. Covered by the M2 discriminating test.
- **Non-discriminating prompt-by-reference test** — an enable→read→compare test reads the same embedded bytes both sides and passes even if the enable path wrongly *copied* the prompt into the row. Mitigate: the M2 test asserts NULL storage + poisons the row column (see M2 DoD). Stated so no one "simplifies" it back.
- **Target-shape CHECK relaxation** must not let a `user` row skip its prompt. Mitigate: the relaxed CHECK is gated on `origin='default'`; M1 has the negative LiveDB test.
- **Sweep-warn ownership crack** — the warn/confirm has an api half (M4) and a web half (M6); M6 depends on M4 so the UI has a payload to render. Called out so neither milestone drops it.

## Success criteria — ALL MET (landed on `agent/issue-589`, 2026-08-22)

1. [x] A connected user sees a **Default jobs** tab with 6 catalog entries; enabling one on a repo creates a running schedule with a **locked** prompt. — M1 catalog (6 entries) + M2 enable + M6 Default-jobs tab (DEFAULT chip + read-only baked prompt).
2. [x] Enabling a default on 2 repos creates 2 independent schedules with per-repo cadence, shown as one expandable row. — M3 client fan-out (`TestEnableCatalogTwoReposLiveDB`) + M6 layout A (summary row → per-repo sub-rows).
3. [x] Cloning a default yields an editable custom schedule (prompt unlocked, `catalog_slug` cleared). — M3 `POST /api/schedules/{id}/clone` (`TestCloneDefaultPromptUnlocksLiveDB`) + M6 clone action.
4. [x] A default row stores **no** prompt copy; a shipped catalog prompt change is what fires (proven by the M2 poison test). — M2 discriminating `TestFirePromptDefaultResolvesFromCatalog` / `TestFireSweepDefaultResolvesFromCatalog` (poison the row column, assert catalog text fires).
5. [x] A report-only scheduled `prompt` run that commits nothing opens **no** MR (#341). — M5 separate `kind='prompt'` report-only terminal (does not weaken the issue-kind guard, ADR-0279) + inverted `runner-push-mr.test.ts`.
6. [x] Enabling a sweep default whose label is missing warns and offers to create it. — M4 `labels/check` + `labels/ensure` (advisory, never blocks) + M6 sweep-warn "Create label" confirm.
7. [x] `task gate` green across api/web/agent/repo; store-it sweep green. #341 and #303 closed; #317 closed as a duplicate of #396. (Self-improvement + #301/#296/#524 are PRD-B.) — `task gate:api`/`gate:web`/`gate:agent`/`gate:repo` all exited 0 this run; the consolidated `./e2e/run-store-it.sh` sweep exited 0 ("Store integration tests passed"). #317 is code-free (Decision 9, already fixed by #396).
