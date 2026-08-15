# PRD #320 — Run queue priority: background runs yield to interactive work, with a manual expedite

- **Issue**: https://gitlab.example.com/vtmocanu/uzi/-/issues/320
- **Source idea**: `ideas/bingo/2026-08-11-run-queue-priority.md` (feature-bingo run, 2026-08-11)
- **Mock**: interactive mock of the Runs page with the feature (priority pills, Expedite, fail-open) built during PRD authoring
- **Status**: Draft
- **Priority**: Medium

## Problem

On a busy factory a human pressing **Start run** waits behind background retrospection.

- Every finished issue/ci_fix run auto-enqueues a **judge** run (`maybeEnqueueJudge`, `api/internal/workersvc/judge_enqueue.go`).
- **self_improve** and scheduled **prompt** runs join the same claim lane; the lane's only kind carve-out is `AND r.kind <> 'chat'` (`api/internal/store/queries/runtime.sql`, the `ClaimRun` query).
- The queue has **no priority term**. `ClaimRun`'s ordering is (verbatim, `runtime.sql:561`):

  ```sql
  ORDER BY COALESCE(r.worker_id = @worker_id, false) DESC, r.created_at ASC
  ```

  i.e. resume-affinity first, then strict FIFO by creation time.
- `WORKER_MAX_CONCURRENT_RUNS` defaults to 1, so on a saturated worker the human's interactive run waits behind whatever judge/self_improve work was enqueued earlier.

The fix is a **kind-derived priority** (judge / self_improve demoted, everything else normal) plus a **per-run manual expedite**, inserted into `ClaimRun`'s ORDER BY between affinity and `created_at`, with an **age-based fail-open** so background work can never starve.

## Goals

- Interactive runs (issue, ci_fix) are claimed ahead of background runs (judge, self_improve) waiting on the same worker, without changing eligibility (WHERE) or any other scheduling behavior.
- A per-run manual **Expedite** bumps any queued run to the front; it is reversible (clear the override).
- Background work is never starved: a demoted run waiting past a grace window collapses to normal priority.
- The priority state is legible on the Runs list, the run page, the CLI, and the health/queued-reason so a demoted run reads as *deprioritized*, not *stuck*.
- Claim ordering and every UI/CLI rendering derive priority from **one** source (a SQL function), so they can never disagree.

## Non-goals

- **Demoting** an interactive run, or arbitrary user-set numeric priorities. Scope is a single manual *expedite* (bump to front) plus *undo* (clear). Free-form demotion is a possible later extension (Decision 6).
- Changing **eligibility** (the `fn_worker_can_claim` WHERE predicate) or the PRD #216 fleet-spread clause. This PRD touches only the ORDER BY term. (Coordination with #216/#84 below.)
- Cross-worker or cross-user global priority. Priority orders one user's own queue for one claiming worker, exactly like the existing affinity/FIFO terms.
- Chat runs. They are on a separate lane (`ClaimChatRun`, `kind = 'chat'`) and are out of scope by construction.
- **The Kanban board's per-card status pills** (`api/internal/handler/board.go`, `mapLatestRun` / `assembleCards`). The priority pill and Expedite action ship on the Runs list and the run page only; the board is intentionally untouched by this PRD. An expedited queued run shows its priority on the Runs surfaces, not on the board card.

## Context and current facts (verified against the tree, 2026-08-15)

All facts below are codebase-internal and were confirmed by reading the tree; nothing here needs the open web (this PRD is queued for an offline `Night-Shift` sweep worker).

- **Claim lane**: `ClaimRun` (`api/internal/store/queries/runtime.sql:473`) is a single `UPDATE ... WHERE id = (SELECT ... ORDER BY ... FOR UPDATE SKIP LOCKED LIMIT 1)`. The ORDER BY at `:561` is the only place ordering is decided.
- **Reused-expression idiom**: `fn_worker_can_claim(is_docker, allowlist, run_repo_id, run_kind)` is an `IMMUTABLE` SQL function (migration `00113_fleet_aware_claim.sql`) called twice in `ClaimRun` (claiming worker + each candidate peer) so eligibility is stated once and can never diverge. **The two new priority functions (rank + class) follow this exact pattern.**
- **Existing grace-cutoff pattern**: `ClaimRun` already takes `@affinity_cutoff` and `@spread_cutoff` params. `WORKER_SPREAD_GRACE` (default `3 × WorkerPollInterval`) and `WORKER_AFFINITY_GRACE` (default `2m`) are parsed in `api/internal/config/config.go` and turned into cutoffs in `api/internal/workersvc/service.go` where `ClaimRun` params are built. **The new `RUN_BACKGROUND_GRACE` knob mirrors this wiring pattern** (parse + build-a-cutoff), but sets its own larger default (`15m`, not the sub-minute poll-cadence value) — see D4.
- **Run kinds**: `issue`, `ci_fix` (migration `00043`), `chat` (`00053`), `judge` + `self_improve` (`00058`), `prompt` (`00104`). Confirmed no `priority` column exists on `runs` today.
- **INSERT sites**: seven `INSERT INTO runs` sites across `runtime.sql`, `chat.sql` (×2), `schedules.sql`, `selfimprove.sql`, `ci_fix.sql`, `judge.sql`. Because the kind default lives in the function (not the column), **none of these insert sites change and no backfill is needed** — `runs.priority` is nullable and defaults NULL.
- **Migration head**: live head is `00123_user_sidebar_tokens.sql`. The new migration drafts as `00124` and is **renamed to the next free number at landing** (Goose numbers are assigned at merge time, per repo convention).
- **Queued reason**: `api/internal/workersvc/health.go` holds the reason strings (`reasonWaitingWorker`, `reasonAllWorkersBusy`, ...). `runs.health` is CHECK-constrained but `runs.health_reason` is free text, so a new reason string needs no migration.
- **Handler**: run routes are registered in `api/internal/handler/handler.go` under a `r.Route("/runs", …)` group that has **two sub-groups** — a **`RequireUser`** group (reachable by a CLI Bearer token: `CreateRunInput`, disposition) and a cookie+CSRF **`RequireAuth`** group (`SetRunWaitOnLimit`, rejudge). `runs.go` holds only **reads** (`ListRuns`, `AdminListRuns`, `AdminListWorkers`); the correct precedent for an owner-scoped single-column *mutation* is `SetRunWaitOnLimit` in `api/internal/handler/waitonlimit.go` (a `PUT /{id}/wait-on-limit`). There is **no** `CancelRun` handler — "cancel" is a run *input* via `CreateRunInput`. No `/runs/{id}/priority` route exists.
- **DTO**: `runToDTO(r store.Run)` (`api/internal/handler/workers.go`) is a **pure function of the row** (no `now()`, no config); `apitypes.RunDTO` has an **exact-key wire-contract test** (`apitypes/wire_test.go`, `runDTOKeys`) that must be updated for any new field.
- **CLI**: run subcommands live in `api/cmd/uzi/run.go`; the enumerated subcommand list in `api/cmd/uzi/commands_test.go` (`"run": {...}`) is asserted and must gain `expedite`. The generated skill lives at `api/internal/uzicli/skill/SKILL.md`; CLI docs at `docs/cli.md`.
- **Web**: the row renderer is `RunRow` in `web/src/pages/RunsList.tsx` (badge cluster beside `StatusPill`); the run page pill/actions are in `web/src/pages/RunView.tsx` (StatusPill at `:597`, owner steer gated by `canSteer`). Badge tones mirror `RUN_STATUS_TONES` via `web/src/lib/runBadge.ts` and `web/src/components/ui.tsx`.

## Design decisions (Decision Log)

**D1 — Priority lives in `IMMUTABLE` SQL functions, not inline SQL and not in Go.** Add **two** co-located functions, both mirroring `fn_worker_can_claim`:
- `fn_run_priority(run_kind text, priority smallint, is_stale boolean) RETURNS smallint` — the ordering rank, called by `ClaimRun`'s ORDER BY.
- `fn_run_priority_class(run_kind text, priority smallint, is_stale boolean) RETURNS text` — the display class in `{normal, background, expedited, restored}`, called by the read queries.

Both are needed because the class carries a distinction the rank cannot: a `restored` run (demoted but past grace) and a `normal` run both rank `1`, so the label must be derived separately — and it is derived **in SQL**, from the same inputs and the same demotion predicate, so ordering and label can never drift and neither is re-implemented in Go. This is the enforced version of "one status renders one color everywhere." Adding a kind to the demotion set later (D5) edits both functions in this one migration, never any Go.

**D2 — Column is a nullable manual override; kind default lives in the function.** Add `runs.priority SMALLINT NULL`. `NULL` means "no manual override — use the kind default." The function:

```sql
fn_run_priority(run_kind, priority, is_stale) =
  COALESCE(
    priority,                                    -- manual override wins
    CASE WHEN run_kind IN ('judge','self_improve') AND NOT is_stale
         THEN 0                                   -- background (demoted)
         ELSE 1                                   -- normal (interactive / stale-restored)
    END
  )
```

Expedite writes `priority = 2` (above normal); undo writes `NULL` (back to kind default). Because the default is computed, the seven INSERT sites and all seeding paths are untouched and no backfill runs.

**D3 — The priority term slots between affinity and FIFO.** New ORDER BY:

```sql
ORDER BY COALESCE(r.worker_id = @worker_id, false) DESC,
         fn_run_priority(r.kind, r.priority, r.created_at < @background_grace_cutoff) DESC,
         r.created_at ASC
```

WHERE is untouched: eligibility (`fn_worker_can_claim`) and the #216 fleet-spread predicate are unchanged. Resume affinity still wins over priority (a resuming worker reclaims its own run first, as today). Within one priority level, FIFO by `created_at` is preserved.

Note the #216 fleet-spread lives in the **WHERE** and can defer a run to a less-loaded peer *regardless of priority*, so "expedite to the front" holds among the rows a given worker is **eligible** to claim, not globally across the fleet. This is acceptable under the non-goal above (priority orders one worker's own candidate set, exactly as affinity/FIFO do).

**D4 — Fail-open by run age (`created_at`).** `is_stale = r.created_at < @background_grace_cutoff`, driven by a new `RUN_BACKGROUND_GRACE` config knob with a literal default of **`15 * time.Minute`**. Only the *plumbing pattern* mirrors `WORKER_SPREAD_GRACE` (parse in `config.go`, subtract from `now()` to build a cutoff in the params builder), **not its value**: the sibling grace knobs are a deliberately sub-minute, poll-cadence timescale (`WORKER_AFFINITY_GRACE` = 2m, `WORKER_SPREAD_GRACE` = `3 × WorkerPollInterval` = 9s by default), whereas this knob measures run-age starvation and is minutes-to-tens-of-minutes. A demoted run older than the grace collapses to normal priority so background work can never starve. `created_at` (not `updated_at`) is chosen so the guard measures *total* time since enqueue; the #216 spread deliberately uses `updated_at` for a different reason (not re-deferring a freshly-requeued run), and the two cutoffs stay independent. **Open for reviewer**: if demoted runs are found to be requeued often, revisit `created_at` vs `updated_at` here.

**D5 — Demotion set is `{judge, self_improve}` only.** Scheduled `prompt` runs are user-initiated and may be time-sensitive (a nightly sweep the user set), so they stay normal. This matches the source idea. **Open for reviewer**: whether scheduled `prompt`/sweep runs should also demote is a real question; default is "no."

**D6 — Manual control is expedite + undo, not arbitrary priority.** A single verb bumps a queued run to the front (`priority = 2`); undo clears it (`priority = NULL`). No demote verb, no numeric input — smallest surface that solves the stated problem, and `SMALLINT` leaves headroom for a future demote value (e.g. `-1`) without a migration.

**D7 — Expedite is `queued`-only and owner-scoped, and must be CLI-reachable.** Ordering only matters before a run is claimed, so `PATCH /api/runs/{id}/priority` accepts the change only while `status = 'queued'` (409/conflict otherwise), and only from the run's owner (the same owner gate `CreateRunInput` uses). Two implementation constraints, both load-bearing:
- **Template**: model the handler on `SetRunWaitOnLimit` (`api/internal/handler/waitonlimit.go`), the existing owner-scoped single-column run mutation, not on the read handlers in `runs.go`.
- **Route group**: register it in the **`RequireUser`** sub-group of `handler.go`'s `/runs` route (Bearer-token reachable), **not** the cookie+CSRF `RequireAuth` group where `wait-on-limit` sits — otherwise the M5 `uzi run expedite` CLI verb (a CLI token) cannot reach it.

CLI mirrors with exit 5 on a non-queued run, which is essentially free: the handler returns 409 and the CLI client maps 409 → `ExitConflict` (5) automatically.

**D8 — DTO exposes a computed priority *class*, sourced from the one SQL function.** `apitypes.RunDTO` (and the list item) gains a `priority` string in `{normal, background, expedited, restored}`, derived **only** by `fn_run_priority_class(kind, priority, created_at < @background_grace_cutoff)`. Implementation note (so the "one source" guarantee is real, not aspirational): `store.Run` is the bare table row and cannot carry a computed column, so `runToDTO` gains an explicit `priorityClass string` parameter and stays a **pure function of its inputs** — no `now()`/config reaches into it. The class value is produced by the SQL function on every DTO path: the list/detail read queries (`ListRuns`, `GetRun`, admin lists) select it as an extra output column (sqlc returns a row struct embedding the run plus `priority_class`); the few paths that return a bare `store.Run` after a mutation obtain it with a one-line `SELECT fn_run_priority_class(...)`. No demotion logic is written in Go. The exact-key wire test (`apitypes/wire_test.go`) is updated for the new field. `restored` is the read-time name for "demoted but past grace" (the *rank* function returns normal because `is_stale`; the *class* function names it `restored`). The web renders the pill from this class, so badge and claim order are the same SQL decision.

**D9 — Queued reason reflects priority.** `health.go` gains a reason so a demoted queued run reads "deprioritized — yields to interactive work" (and, once past grace, "priority restored — no longer yielding") instead of the generic "waiting for a worker," which reads as a fault. Free-text `health_reason`, no migration.

**D10 — Coordination with #216 and #84 (both open).** #216's fleet-spread clause is **already merged** into `ClaimRun` (the spread `NOT EXISTS` block and `fn_worker_can_claim` are live). This PRD adds **only** an ORDER BY term and does not touch the spread WHERE clause, so the conflict surface is a single line. #84 (capability-aware scheduling) is **pending** and, per `fn_worker_can_claim`'s own comment, *extends that function's signature* — i.e. it edits **eligibility (WHERE)**, orthogonal to this PRD's **ordering (ORDER BY)**. Land order does not matter for correctness, but whoever lands second rebases the one-line ORDER BY / one-line WHERE respectively. This PRD must not duplicate or fork `fn_worker_can_claim`.

## Milestones

Seven milestones. Success criteria are behavioral and validated by the repo's gate (`task gate:api`, `task gate:controller` where relevant, `task gate:web`) plus live-DB tests where the change is a store query. `-count=1` and `-race` are already enforced at the Go gates.

### M1 — Schema + priority functions (foundation)
- Migration (drafts `00124`, renamed at landing): `ALTER TABLE runs ADD COLUMN priority SMALLINT NULL` + **both** `IMMUTABLE LANGUAGE sql` functions — `fn_run_priority(text, smallint, boolean) RETURNS smallint` (rank) and `fn_run_priority_class(text, smallint, boolean) RETURNS text` (display class), sharing the demotion predicate — with a matching `+goose Down` (drop both functions, drop column).
- `sqlc generate` regenerates models; no query uses the column yet.
- **Success**: migration applies and rolls back cleanly on a live DB; `fn_run_priority` returns 0 for a non-stale judge/self_improve run, 1 for issue/ci_fix/prompt and for a stale demoted run, and the override value when `priority` is non-null; `fn_run_priority_class` returns `background` / `restored` / `expedited` / `normal` over the matching inputs. A focused live-DB (or pure-SQL) test asserts both truth tables side by side, so the rank and class can never disagree.

### M2 — Claim ordering + background-grace fail-open (core behavior)
- Add the priority term to `ClaimRun`'s ORDER BY (D3). Add `RUN_BACKGROUND_GRACE` to `config.go` with a literal **`15 * time.Minute`** default (D4) and thread `@background_grace_cutoff` into the `ClaimRun` params builder in `workersvc/service.go` (same shape as the existing `AffinityCutoff`/`SpreadCutoff`).
- Surface the knob on the compose path: add `RUN_BACKGROUND_GRACE` to `docker-compose.yml` (api env block, beside `WORKER_SPREAD_GRACE`) and `.env.example`. The Helm chart does not template these grace knobs, so the k8s path takes the `config.go` default — note this; no chart change required.
- **Success** (live-DB `ClaimRun` tests, the existing pattern in `store/claim_fleet_placement_integration_test.go`): with one busy worker and a full queue, an interactive run is claimed before an earlier-created background run; an expedited run is claimed before everything; a background run older than the grace is claimed at normal priority (fail-open); resume affinity still wins over priority; FIFO holds within a priority level. No change to which runs are *eligible* (WHERE) — a regression test pins the #216 spread behavior unchanged.

### M3 — Expedite endpoint + DTO class (API + handler + store)
This milestone owns the whole **server side** of the priority class, so its own success criterion (the DTO reflecting the class) does not depend on the later web milestone.
- Store: `SetRunPriority` query (owner-scoped, `queued`-only: set `priority = $val` or clear to NULL). Add `fn_run_priority_class(...)` to the run **read** queries (`ListRuns`, `GetRun`, admin lists), passing `created_at < @background_grace_cutoff`, so each returned row carries a `priority_class`.
- DTO: add `priority` to `apitypes.RunDTO`; give `runToDTO` an explicit `priorityClass string` parameter (sourced from `fn_run_priority_class`, so it stays pure — D8) and thread it through the ~15 call sites; update the exact-key wire test `apitypes/wire_test.go`.
- Handler: `PATCH /api/runs/{id}/priority` modeled on `SetRunWaitOnLimit` (`handler/waitonlimit.go`), owner gate, 409 on non-queued, registered in the **`RequireUser`** sub-group of `handler.go`'s `/runs` route (CLI-reachable — D7).
- **Success**: owner expedites a queued run → `priority = 2` and its DTO `priority` reads `expedited`; undo → NULL and the class returns to `background`/`normal`; a demoted run past grace reads `restored`; non-owner gets 404; non-queued run gets 409. Handler tests + the wire test cover each path.

### M4 — Queued-reason wording (deprioritized vs stuck)
- `health.go` learns a deprioritized reason and a restored reason (D9); the health detector receives the same `RUN_BACKGROUND_GRACE` cutoff (`now()` minus the grace) so it can tell a still-yielding demoted run from a grace-restored one, and maps a demoted queued run to the deprioritized reason rather than the generic waiting-for-worker reason.
- **Success**: a demoted queued run surfaces "deprioritized — yields to interactive work"; past the grace it reads as restored; an ordinary interactive queued run is unchanged. Health tests assert the mapping. (Small milestone; kept separate because it is independently testable, but it could fold into M2/M3 without loss.)

### M5 — CLI `uzi run expedite`
- `uzi run expedite <run-id>` (and clear/undo, e.g. `--clear`) calling the M3 endpoint; add `expedite` to the enumerated subcommand list in `commands_test.go`.
- Update `api/internal/uzicli/skill/SKILL.md` (the run-verbs section) and `docs/cli.md`.
- **Success**: `uzi run expedite <queued-id>` returns 0 and bumps the run; a non-queued run returns exit 5; `--json` returns a stable envelope; `commands_test.go` passes with the new verb; SKILL.md/docs describe it. (Per repo convention, new API functionality checks the CLI — this milestone is that check.)

### M6 — Web: priority pill + Expedite action (consumes M3's DTO field)
The server `priority` field ships in M3; this milestone is the TS/web side only.
- Types: `RunListItem` / `RunDTO` in `lib/api.ts` gain the `priority` class; `runBadge.ts` maps class → pill tone/label (single source, mirroring `RUN_STATUS_TONES`).
- `RunRow` (`RunsList.tsx`): render the priority pill in the badge cluster on queued rows (`background`/`expedited`/`restored`; `normal` renders nothing, keeping the row quiet). Add an owner-only **Expedite** action on queued rows.
- `RunView.tsx`: priority pill beside the status pill (StatusPill render at `RunView.tsx:597`); Expedite/undo action gated by owner + `queued` (reuse the `canSteer` gate).
- The Kanban board is intentionally out of scope (non-goal): board cards keep their existing status pill.
- **Success**: `task gate:web` green; a background queued row shows the pill and an Expedite control; clicking it calls the endpoint and the pill flips to expedited; a normal interactive row shows no pill; vitest covers the pill mapping and the gated action. Validated in `VITE_UZI_MOCK=1` for rendering (note: mock fixtures validate rendering, not backend behavior).

### M7 — Integration, docs, specs
- One end-to-end ordering scenario (the M2 story) as an integration/live-DB test if not already covered by M2.
- User-facing docs: a short "queue priority / expedite" note wherever run lifecycle is documented (`docs/run-health.md` is the natural home for the deprioritized/restored queued-reason wording), following existing frontmatter rules.
- `specs/ai.md` records the priority-ordering decision (append-only, AI-design decision).
- **Success**: docs build (`web/scripts/check-docs.mjs`) passes; `specs/ai.md` updated; full `task gate` green across components.

## Parallelization plan

Per the repo's PRD workflow. Note: the `Night-Shift` sweep implements this as **one** offline worker run, so the phases below are for a human running `/prd-start` with a team; a single worker executes them in dependency order.

| Phase | Milestones | Depends on | Files touched | Can run in parallel? |
|---|---|---|---|---|
| 1 | **M1** schema + 2 functions | — | `store/migrations/`, generated models | Foundation; must land first |
| 2 | **M2** claim ordering · **M3** expedite endpoint + DTO | M1 | M2: `runtime.sql` (ORDER BY), `config.go`, `workersvc/service.go`, `docker-compose.yml`, `.env.example`. M3: `runtime.sql` (`SetRunPriority` + `priority_class` in read queries), `handler/waitonlimit.go`-style handler, `handler.go`, `apitypes/run.go` + `apitypes/wire_test.go`, `handler/workers.go` (`runToDTO`) | Both edit `runtime.sql` + regenerate sqlc, so coordinate that one file / regen; otherwise independent |
| 3 | **M4** health reason · **M5** CLI · **M6** web | M4←M2; M5←M3; M6←M3 | M4: `workersvc/health.go`. M5: `cmd/uzi/`, `commands_test.go`, SKILL.md, `docs/cli.md`. M6: `web/**` (consumes M3's DTO field) | Fully parallel (disjoint files) |
| 4 | **M7** integration + docs + specs | all | `docs/`, `specs/ai.md`, tests | Sequential, last |

## Testing strategy

- **Ordering is a store query**, so it is tested against a **live DB** (the existing `ClaimRun` test pattern), not a fake. Cover: interactive-beats-background, expedite-beats-all, fail-open-after-grace, affinity-still-wins, FIFO-within-level, and a regression that the #216 spread WHERE is unchanged.
- **`-count=1`** is mandated at the Go gates (fixtures cross the module boundary); do not weaken it.
- **Function truth table** (M1) as a small live-DB or pure-SQL test.
- **Handler** (M3) and **health** (M4) with table tests.
- **CLI** (M5): the `commands_test.go` enumeration plus a fake-backed expedite test; exit-code contract (0 success, 5 non-queued conflict).
- **Web** (M6): vitest for the pill mapping and the gated action; a `VITE_UZI_MOCK=1` pass for rendering only (mock fixtures cannot validate backend priority behavior — that is what the live-DB tests are for).

## Risks and mitigations

- **R1 — Merge collision with #216/#84 on `ClaimRun`.** Mitigated by touching only the ORDER BY (this PRD) vs the WHERE (#84); #216 spread is already merged. Whoever lands second rebases one line. Do not fork `fn_worker_can_claim`.
- **R2 — Priority/UI drift.** Mitigated by D1/D8: the claim rank and the display class are **two co-located SQL functions** sharing one demotion predicate, and the class is computed in the read queries (never re-derived in Go), so ordering and labels are the same decision. Adding a kind to the demotion set edits only that migration.
- **R3 — Starvation of background work.** Mitigated by the D4 fail-open (`RUN_BACKGROUND_GRACE`), tested explicitly in M2.
- **R4 — Silent behavior change for existing users.** The default keeps interactive runs first and demotes only judge/self_improve; no run becomes *unclaimable* (priority is an ORDER BY term, never a WHERE filter). The migration adds a nullable column with a computed default, so no backfill and no INSERT-site churn.
- **R5 — Goose number collision.** The migration drafts as `00124` and is renamed to the next free number above the live head at landing (repo convention).

## Offline-worker readiness (Night-Shift sweep)

This PRD is queued for a `Night-Shift` sweep, which runs an offline worker with no open-web egress. Every fact it relies on is codebase-internal and stated above as a resolved fact with its file/line, verified against the tree on 2026-08-15. No milestone depends on an external lookup, a docs site, or a web search. The one deliberately-deferred choice (D4 `created_at` vs `updated_at`, D5 whether `prompt` demotes) is flagged as an in-body decision with a stated default, not left as an unresolved external dependency.
