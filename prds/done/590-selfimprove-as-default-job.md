# PRD #590 — Self-improvement as a default scheduled job (retire the bespoke engine)

**Issue**: [#590](https://github.com/vtmocanu/uzi/issues/590)
**Status**: Done (M1–M3 landed; PRD #589 dependency merged). Self-improvement now runs as a per-user `self_improve` default scheduled job; the bespoke `api/internal/selfimprove` engine is deleted and enabled legacy installs are boot-migrated to a materialized `default`-origin schedule row.
**Priority**: High
**Depends on**: **PRD #589** (default-jobs catalog + enablement machinery — `origin`/`catalog_slug`/`customized`, `go:embed` catalog, enable/reset/clone, prompt-by-reference fire path).
**Author**: design session, 2026-08-22

---

## Problem

Self-improvement (PRD #46) is a **separate bespoke engine** (`api/internal/selfimprove/engine.go`) with its own `app_settings` keys, admin settings card, `Boot`+ticker actor, and run-creation path — duplicating the general scheduler (`api/internal/schedsvc`, PRD #241), whose own comment says it is "modeled on selfimprove/engine.go". It is admin-only and instance-wide, so a user cannot run self-improvement on their own repo, and there is no self-improve entry in the Schedules UI.

This resolves brainstorm **#301** (replace the self-improvement engine with a scheduled prompt) and closes **#296** (the self-improve tracking issue) and **#524** (a flaky admin self-improvement test that dies with the card).

## Solution overview

Re-express self-improvement as **one special `self_improve` default job** on top of PRD #589's catalog. Only the *engine* goes; the run kind and worker behavior stay.

- Add a **`self_improve` schedule target** and a **7th catalog entry** (`self-improve`). Its directive is baked **worker-side** (`agent/src/prompt.ts` `buildSelfImprovePlanPrompt`), so this entry carries **cadence + model only — no prompt-by-reference** (the "each entry has a baked catalog prompt" invariant does not apply to this slot; the catalog schema must allow a promptless `self_improve` entry).
- **Relocate** the server-side orchestration from the engine into `schedsvc` — it is *not* "just a trigger change": `ensureTrackingIssue`, `composeRunDescription` (the backlog fold), `MarkImproveUziRecommendationsAddressed`, the vault-lock skip, and the `selfimprove_started`/`selfimprove_skipped` notifications are all server-side and must move.
- **Generalize to per-user / any repo**, dropping the admin-only gate. Keep the guardrail (repo required, human merges).
- **Per-user, fold-when-present recommendation model** (Decision below): scope the existing `improve_uzi` query by user and fold whatever open recs that user has; a repo/user with none gets an empty fold = pure codebase review. No per-repo recommendation category is invented.
- **Delete** the engine, the `selfimprove_*` app_settings + typed getters + writable-allowlist branch, the handler + routes, the admin `SelfImproveSettingsCard`, the `UZI_SELFIMPROVE_CHECK_INTERVAL` config surface, and `docs/self-improvement.md`. **Boot-migrate** an enabled install to a materialized `default`-origin `self_improve` schedule row.

---

## Decisions (Decision Log)

1. **`self_improve` is a schedule target**, firing `kind='self_improve'` runs through the existing `workersvc.CreateSelfImproveRun` seam. The `self-improve` catalog entry carries cadence (`0 4 */2 * *` ≈ every 2 days) + model, no prompt.
2. **Relocate, don't delete, the orchestration.** `ensureTrackingIssue` (label `uzi-self-improve`), the backlog fold, mark-addressed, vault-lock skip, and started/skipped notifications move into `schedsvc`. The worker-side directive (`buildSelfImprovePlanPrompt`) is untouched.
3. **Per-user, fold-when-present.** `improve_uzi` has no repo dimension (`review_recommendations` has no `repo_id`) and means "improve the uzi platform". Scope the existing query by `run_reviews.user_id` (which exists) and fold the user's open `improve_uzi` recs into any self-improve run; empty for users without them → pure codebase review. Keep the `improve_uzi` judge category unchanged (it is load-bearing elsewhere: `issuedraft.go`, CLI `review backlog --category`, web judge, `KnownImproveUziTargets`).
4. **Per-user, generalized, admin gate dropped.** Any user can enable the `self-improve` default on a repo they own.
5. **Fix the instance-wide unique index (blocker).** Make self-improve dedup per-repo instead of instance-wide.
6. **Additive then subtractive.** Land the new fire path with the engine still running (M1), prove green, then delete the engine + boot-migrate (M2).

---

## The blocker: instance-wide self-improve unique index

`uq_runs_one_active_self_improve ON runs (kind) WHERE kind='self_improve' AND status NOT IN ('completed','failed','cancelled')` (`api/internal/store/migrations/00058_run_judge_self_improve_kinds.sql:54-56`) admits exactly **one** non-terminal `self_improve` row **across the whole install** (it indexes on `kind`). `CreateSelfImproveRun` (`api/internal/workersvc/self_improve.go:29,54-56`) relies on it as the dedup backstop (`ErrActiveSelfImproveExists`). Generalizing self-improve to per-user/any-repo (Decision 4) turns this "dedup" into a **global serializer**: while user A's cycle is non-terminal, user B's scheduled fire fails the unique insert and is silently dropped. **Fix:** replace with a per-repo partial unique index (e.g. `ON runs (repo_id) WHERE kind='self_improve' AND status NOT IN (terminal)`), and update `CreateSelfImproveRun`'s dedup accordingly. Decide whether per-repo or per-(user,repo) is the right grain (per-repo matches "one self-improve MR per repo at a time").

---

## Current state (verified 2026-08-22 — baked for an offline worker)

- **Engine** `api/internal/selfimprove/engine.go` — `Boot()` + ticker; `tick()`: enabled → due (`now - selfimprove_last_run_at ≥ interval`) → resolve owner+repo → skip checks → `runCycle()`. Skips (each with a throttled inbox notification, cadence not advanced): active run in flight, **vault locked** (`:193-196`), repo disconnected/not-owned (`:198-200`), "enabled but repo/owner unconfigured" (`:178`). Wired `api/cmd/server/main.go:653-663`. Config `UZI_SELFIMPROVE_CHECK_INTERVAL` / `SelfimproveCheckInterval` (`api/internal/config/config.go:245-246`).
- **Orchestration to relocate:** `ensureTrackingIssue()` (`engine.go:303-326`), `composeRunDescription()` (`engine.go:351-371`), mark-addressed (`engine.go:244-252`), notifications `selfimprove_started`/`selfimprove_skipped` (`engine.go:263,291`).
- **Settings (delete):** `app_settings` keys `selfimprove_enabled` (default false), `selfimprove_interval` (48h), `selfimprove_repo`, `selfimprove_user_id`, `selfimprove_last_run_at` (`api/internal/settings/settings.go:92-96`); typed getters `:676-733`; the 3 engine-managed keys excluded from the writable allowlist `:297-313`. Handler `api/internal/handler/selfimprove.go` (`GET|PUT /admin/selfimprove`, `RequireAdmin`, `handler.go:983,1019`) + `selfimprove_test.go`.
- **Run creation (keep):** `api/internal/workersvc/self_improve.go` (`CreateSelfImproveRun`, `ErrActiveSelfImproveExists`); kind const `RunKindSelfImprove="self_improve"` (`workersvc/judge.go:24`). Queries `api/internal/store/queries/selfimprove.sql`: `CreateSelfImproveRun`, `CountActiveSelfImproveRuns`, `ListOpenImproveUziRecommendations` (instance-global, `:69-85`), `MarkImproveUziRecommendationsAddressed`.
- **kind-shape CHECK:** `kind='self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL` (`00134_run_task_kind.sql:35`) — so the fire path **must** ensure/reuse the tracking issue and pass its iid before `CreateSelfImproveRun`.
- **Recommendations schema:** `review_recommendations` (`00059_run_reviews.sql:44-51`) — no `repo_id`; `improve_uzi` is a CHECK-enum judge category (`:47-49`); `run_reviews` is anchored by `user_id` (`:24`). Precedent for per-user scoping: `ListKnownImproveUziTargetsForUser` (`judge_known_targets.sql:20-28`).
- **Scheduler seam (add):** `fireOne` switches on `issue|sweep|prompt`, parks else (`schedsvc/scheduler.go:230-242`); the `Scheduler` struct's `RunCreator`/`Store` interfaces contain **none** of the self-improve seams and no `VaultGate` — all must be added.
- **Web card (delete):** `web/src/pages/AdminSettings.tsx:940-1069` `SelfImproveSettingsCard` (+ `api.ts:2158-2176,2569-2577`, tab reg `:389`). Flaky test #524 in `AdminSettings.test.tsx`.
- **Cross-refs to repoint on delete:** `ARCHITECTURE.md` "modeled on the selfimprove engine" (usagepoller ~`:368`, schedsvc ~`:487`); `web/src/lib/boardCards.ts:32` (`selfimprove.TrackingLabel`); `web/src/lib/notifications.test.ts:32,37` fixture. `slacksvc/notifier.go:418-423` keys on run **kind** (safe — kind stays). `docs/self-improvement.md`.

---

## Scope constraints

- **No `.github/workflows/**` changes** (worker PAT lacks `workflow` scope; see `.claude/rules/prds.md`). This PRD needs none.
- Do **not** delete `workersvc/self_improve.go` or the `self_improve` run kind — the consumer (engine) goes, the producer path stays.
- Do **not** touch the `improve_uzi` judge category — only scope its self-improve *consumer* query by user.

---

## Milestones

### M1 — Additive: `self_improve` schedule target + relocated orchestration (engine still running)
- Catalog: add the promptless `self-improve` entry (cadence+model) + support a `self_improve` target in the `schedtmpl` schema and the `run_schedules` target domain/shape CHECK (`target='self_improve'` ⇒ `repo_id NOT NULL`, `prompt`/`labels`/`issue_iid` NULL; the run's issue_iid comes from the tracking issue at fire time, not the schedule row).
- Scheduler: add a `self_improve` case to `fireOne`; add the seams to the `Scheduler` interfaces (`CreateSelfImproveRun`, the per-user backlog query, mark-addressed, `ensureTrackingIssue`/`CreateIssue`/`ListIssues`, `VaultGate.Unlocked`). Relocate `ensureTrackingIssue` + `composeRunDescription` + mark-addressed + vault-lock skip + started/skipped notifications into `schedsvc`.
- Recommendation fold: scope `ListOpenImproveUziRecommendations`/`MarkImproveUziRecommendationsAddressed` by `run_reviews.user_id` (per-user); fold-when-present.
- **Index blocker:** migration to replace `uq_runs_one_active_self_improve` with a per-repo partial unique index; update `CreateSelfImproveRun` dedup.
- Engine still runs in this milestone (do not delete yet) — but guard against double-firing (e.g. leave `selfimprove_enabled=false` in dev, or have the engine no-op when a `self_improve` schedule exists for the same repo).
- **DoD tests (LiveDB):** a `self_improve` schedule fire creates a `kind='self_improve'` run, reuses the tracking issue, folds the user's recs (and marks addressed), skips on locked vault; two users' fires do not serialize (per-repo index); dedup still blocks a second active run on the same repo.
- **Deps**: PRD #589 (catalog machinery). **Gate**: `task gate:api` + store-it sweep.

### M2 — Subtractive: delete the engine + boot-migrate
- Boot migration as an **exported querier-level function** (not inline in `main.go`, so it is LiveDB-testable): materialize a `default`-origin `self_improve` schedule row from `selfimprove_enabled=true` (repo=`selfimprove_repo`, owner=`selfimprove_user_id`, cadence from `selfimprove_interval`→cron), then retire the keys. **Idempotent** (row-exists guard). **Failure modes:** a disconnected/deleted `selfimprove_repo` (FK NOT NULL) → skip + log, do not fail boot; the "enabled but repo/owner unconfigured" state → skip. Disabled install → seed nothing.
- Delete: `api/internal/selfimprove/` + its `main.go` wiring + `UZI_SELFIMPROVE_CHECK_INTERVAL` config; the `selfimprove_*` keys + typed getters + writable-allowlist branch; `handler/selfimprove.go` + routes + `selfimprove_test.go`; the web `SelfImproveSettingsCard` + `api.ts` bits + the #524 test; `docs/self-improvement.md`. Repoint the ARCHITECTURE.md "modeled on" prose, `boardCards.ts` `TrackingLabel`, and the `notifications.test.ts` fixture.
- **DoD tests (LiveDB):** enabled install → one row (twice → still one); disabled → none; disconnected-repo → skip, boot succeeds.
- **Deps**: M1. **Gate**: `task gate:api`, `task gate:web`, `task gate:agent`.

### M3 — Web + docs
- Web: the `self-improve` default appears in the Default jobs tab (PRD #589's UI), admin-gate removed; no admin settings card.
- Docs: remove `docs/self-improvement.md` (or a redirect note to scheduling); update `ARCHITECTURE.md` (drop the `selfimprove` service, note the `self_improve` schedule target + per-user fold); `specs/ai.md` decisions. `check-docs` passes.
- **Deps**: M1, M2, PRD #589 M6. **Gate**: `task gate:web`, `task gate:repo`.

## Risks

- **Double-firing during M1** (engine + schedule both live). Mitigate: the additive milestone must prevent the engine and a `self_improve` schedule from both firing for the same repo (dedup index helps, but avoid the wasted cycle).
- **Boot migration data loss.** Materialize before retiring keys; idempotent; bad-repo tolerant. Test all three states.
- **Per-user fold semantics.** Folding a user's `improve_uzi` recs into a self-improve run on a *non-uzi* repo is semantically loose but harmless (advisory data, usually empty). Accepted (Decision 3).

## Success criteria

1. Self-improvement runs as a `self_improve` default job on any repo the user owns; no admin card, no `selfimprove_*` settings.
2. Two users' self-improve runs do not serialize globally (per-repo index).
3. An enabled install migrates to a `self_improve` schedule row on boot (idempotent; bad-repo tolerant); a disabled install seeds nothing.
4. The tracking-issue reuse, per-user rec fold + mark-addressed, vault-lock skip, and started/skipped notifications behave as before, now from `schedsvc`.
5. `task gate` green across api/web/agent/repo; store-it sweep green. #301, #296, #524 closed.
