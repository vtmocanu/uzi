# PRD #636 — Multi-repo custom schedules (grouped siblings) + Default-jobs tab polish

**Issue**: [#636](https://github.com/vtmocanu/uzi/issues/636)
**Status**: Done — all milestones (M1–M6) landed on `agent/issue-636`. The migration landed as `00161_run_schedules_sibling_group.sql` (next free above the live head `00160`).
**Priority**: Medium
**Author**: design session, 2026-08-23 (revised same day after a 3-reviewer pass — scope, milestones, data-model)
**Depends on**: PRD #589 (default-jobs catalog + `RepoMultiSelect` + the client-side multi-repo fan-out pattern + the Default-jobs expandable summary/sub-row layout) and PRD #344 (schedule repoint-on-edit). Both landed.

---

## Problem

A custom "My schedule" targets exactly one repo, and the web UI has no way to change that: `run_schedules.repo_id` is scalar (`uuid NOT NULL`), the create modal's repo picker is a single `<Select>` (`web/src/components/ScheduleModal.tsx:582`), and the web Clone button clones into the source's own repo with no repo choice (`web/src/pages/Schedules.tsx:225`, `api.cloneSchedule(s.id)` with no repo arg). So running the same custom job on N repos means authoring it N times by hand.

Default jobs, by contrast, already offer a first-class multi-repo experience: a shared `RepoMultiSelect`, a client-side fan-out that creates one independent schedule per repo, and a grouped view where a default enabled on N repos collapses into one expandable summary row over per-repo sub-rows (PRD #589 M6, `web/src/components/DefaultJobs.tsx`). PRD #589 M6 even specified the same multi-repo affordance for the custom modal ("Modal: multi-repo select ('N repos → N schedules')") but it shipped single-select, so the two tabs are inconsistent and the custom path is strictly weaker.

Separately, two small Default-jobs tab polish items surfaced while reviewing this: (1) every row carries a redundant `DEFAULT` chip even though the tab header already says "Default jobs", and (2) the Target cell packs name + `DEFAULT` + lock + type pill (`prompt`/`sweep`/`self-improve`) inline, so a longer name ("Weekly test improvement") wraps the type pill onto a second line.

## Solution overview

Give custom schedules the same multi-repo experience as default jobs, built deliberately as **independent sibling rows**, not a linked entity:

- **No change to the one-row-one-repo model.** `repo_id` stays scalar; a custom job on N repos is N independent `run_schedules` rows, each editable, pausable, and removable on its own. This is exactly how default jobs already behave (enabling a default on N repos creates N independent `origin='default'` rows; the "expandable summary" is only a view keyed on `catalog_slug`, not a shared record).
- **A display-only `sibling_group_id`.** Add a nullable `sibling_group_id uuid` to `run_schedules`. It is purely a **view grouping key** for custom rows (the analog of `catalog_slug` for defaults) and carries **no behavior**: editing one sibling never touches another. `NULL` means "standalone row" (the common single-repo case). This is the only schema change beyond one supporting index.
- **Extract a neutral summary/sub-row from the Default-jobs layout.** The grouped view is not a drop-in reuse: `DefaultJobs.tsx`'s `CatalogRow`/`SubRow`/`EnableAnotherRepo` are un-exported and bound to catalog concepts (a `CatalogEntry`, the `DEFAULT` chip, the lock, a read-only prompt, and an "enable another repo" filter keyed on `catalog_slug`). M3 **extracts a neutral, catalog-agnostic summary+sub-row** that both the Default-jobs tab and the custom My-schedules tab render, so a custom group shows with no `DEFAULT` chip, no lock, an editable prompt, and per-sub-row edit. Treat M3 as a partial rewrite of that layer, not a parameter tweak.
- **Multi-repo create reuses `RepoMultiSelect` and the client-side fan-out** (`enableDefault` in `Schedules.tsx:175`).
- **"Add another repo"** on an existing custom row/group replicates the config onto a new repo as a new sibling, via a **dedicated `POST /api/schedules/{id}/add-repo` endpoint** (not an overload of `/clone`, see Decision 5).
- **CLI parity** and **docs**.
- **Default-jobs tab polish** ships as an independent milestone (web-only, no dependency on the multi-repo work).

### Why independent siblings, not a linked job (decided)

"Consistent with how default jobs look" points at independent siblings, because that is what default jobs *are*. A truly linked multi-repo job (edit once, propagate to all repos; remove a repo from a set) would need a new schema concept (a group/join table), fire-time fan-out, and edit-propagation/removal semantics, and it would make custom jobs behave **differently** from default jobs, not more like them. Rejected. `sibling_group_id` buys the *look* (grouped, expandable) and nothing else; behavior stays fully per-row.

---

## Decisions (Decision Log)

1. **Independent siblings, not a linked entity.** `repo_id` stays scalar; N repos = N independent rows. Editing/pausing one never affects a sibling.
2. **`sibling_group_id uuid` is display-only.** Nullable column on `run_schedules`; `NULL` = standalone. It groups custom rows in the view the way `catalog_slug` groups default rows. No FK to another schedule (it is a group tag, not a row reference), no fire-path use, no CHECK coupling. `schedsvc` never reads it.
3. **Grouping renders a summary only for a non-null id with ≥2 live members.** A `NULL` id is always a standalone row; a non-null id with **exactly one** live member also renders as a standalone row (never a one-child "group"). This makes the view correct regardless of *how* a group shrank to one — an app delete, a partial-failure create, or a repo-disconnect CASCADE (see Decision 10) — with no reliance on a DB write having run. As hygiene, the app-driven delete path (`DeleteRunSchedule`) opportunistically clears the group id off the last surviving sibling, but the **load-bearing** rule is the view collapse, because the CASCADE path runs no application code.
4. **Client-side fan-out, consistent with PRD #589.** Multi-repo create is N independent `POST /repos/{id}/schedules` calls; the **client generates one `sibling_group_id` (uuid v4)** and passes it in all N create bodies so the rows share a group. A single-repo create sends no group id (`NULL`). No new batch server route. The server **validates** the field: a malformed value is a `400`, not a `500`; and it is **create-only** — `UpdateRunSchedule`'s SET list omits it, so a PATCH cannot set or clobber it, mirroring the create-vs-PATCH asymmetry already documented for `RepoID` at `apitypes/schedule.go:33-36`. Owner-scoping (`ListRunSchedulesForUser … WHERE user_id`) bounds the blast radius: a forged/duplicate group id can only mis-group the caller's own rows (cosmetic; siblings stay independent), never pull in another user's rows.
5. **"Add another repo" is a dedicated endpoint, `POST /api/schedules/{id}/add-repo`** (body `{"repo_id"}`), **not** an overload of `/clone`. `/clone` keeps its current semantics untouched (it clones into the source's own repo by default, or a different repo with `{repo_id}`, producing a standalone `origin='user'` row and **no** group). The new endpoint, in **one transaction** (`store.Queries.WithTx`): (a) ensures the source has a group id via a coalescing, race-safe UPDATE — `UPDATE run_schedules SET sibling_group_id = COALESCE(sibling_group_id, @new_group) WHERE id = @source AND user_id = @uid AND origin = 'user' RETURNING sibling_group_id` (two racing calls both coalesce to one id under the row lock, so no split-group); (b) copies the source's current config into a new row on the target repo carrying that returned group id. Owner-scoped (404 for a foreign source or target repo), `origin='user'` sources only.
6. **Multi-repo is a create/add concept, not an edit concept.** The modal shows `RepoMultiSelect` only in **create** mode; **edit** keeps the single repoint `<Select>` unchanged (PRD #344 move-one-repo). Matches DefaultJobs (multi-select is for enable, per-row actions are single-repo).
7. **Partial failure does not roll back.** N independent creates; on a mid-fan-out failure the already-created rows persist and the UI reports "created N of M, K failed" (the `enableDefault`/CLI convention). A group left with one live member after a partial failure is handled by Decision 3 (renders standalone).
8. **Sweep-label guardrail runs per selected repo** on multi-repo create, reusing PRD #589 M4 (`labels/check` + `labels/ensure`, advisory, never blocks).
9. **Default-jobs polish is scoped and independent.** Drop the `DEFAULT` chip only on Default-tab **rows** (My-schedules rows never render it — `Schedules.tsx:259` filters that tab to `origin='user'`); harden the Target-cell flex/wrap so the type pill never dangles; **keep** the lock icon (it encodes baked/read-only prompt, distinct from DEFAULT). The modal-header `DEFAULT` badge is out of scope (optional follow-up).
10. **One sibling per repo within a group, enforced by a partial unique index.** Add `CREATE UNIQUE INDEX … ON run_schedules (sibling_group_id, repo_id) WHERE sibling_group_id IS NOT NULL`. It excludes today's rows (all `NULL`) and multi-repo fan-out (distinct repos per group), and it turns the two ways two siblings could land on one repo into clean, loud errors instead of a broken sub-row list: a duplicate `add-repo` onto a repo already in the group conflicts at INSERT (return an idempotent-safe `409`, no corruption), and a PRD #344 repoint of a grouped sibling onto a repo already occupied by a sibling in the same group is rejected the same way. `repo_id`'s `ON DELETE CASCADE` (repo disconnect) deletes the sibling with no app code and can shrink a group to one member — handled by Decision 3's view collapse.

---

## Current state (verified 2026-08-23 — baked for an offline worker)

**Schema.** `run_schedules` — `api/internal/store/migrations/00103_run_schedules.sql`: `repo_id uuid NOT NULL REFERENCES repos(id) ON DELETE CASCADE` (`:17`), plus `origin`/`catalog_slug`/`customized` (PRD #589). The table's shape/timing CHECKs (`:34-45`) reference no `repo_id` or cardinality, so a nullable additive column needs no CHECK change. **Live migration head is `00160` (`00160_agent_source_staged.sql`)**; a new migration is renamed to the next free number above the live head at merge (CLAUDE.md — draft this one as `00161`).

**Queries.** `api/internal/store/queries/schedules.sql`: `CreateRunSchedule` (`:6`, the single user-insert path; both the create handler and clone go through it via struct literals, so a new nullable `SiblingGroupID` param zero-values to `NULL` for untouched callers), `ListRunSchedulesForUser` (`:92`, `WHERE user_id = @user_id`), `UpdateRunSchedule` (`:109`, sets `repo_id` for the #344 repoint; its SET list omits `sibling_group_id`), `DeleteRunSchedule` (`:148`, a plain owner-scoped DELETE with no group cleanup today), `ClaimDueSchedules` (`:152`, `SELECT … WHERE enabled AND status='active' AND next_fire_at <= now()` — no group predicate). After editing `queries/` or `migrations/`, run `cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate` (CI asserts the regenerate is a no-op).

**DTO.** `api/internal/apitypes/schedule.go`: `ScheduleDTO` (`:64`) carries `Origin`/`CatalogSlug`/`Customized` (`:107-109`) and `RepoID` (`:66`); the create/update input type carries `RepoID` (`:37`) with a documented create-vs-PATCH asymmetry (`:33-36`); `ScheduleCloneRequest` (`:207`). Add `SiblingGroupID *string` to the read DTO and an optional, create-only `sibling_group_id` to the create input.

**Clone endpoint (verified — the copy-config mechanics `add-repo` reuses internally).** `POST /api/schedules/{id}/clone` — `handler/schedules.go:672-769`: owner-scoped (404 on a foreign source at `:679`, 404 on a foreign/absent target repo at `:699-701`), optional `{"repo_id"}` body clones into a different owned repo, produces a fresh `origin='user'`, `catalog_slug=NULL`, `customized=false` row via `CreateRunSchedule` (`:744`). **It runs three separate non-transactional `h.q` calls and stamps no group id.** `add-repo` is a *separate* endpoint that shares the field-copy logic but adds the transactional group stamping (Decision 5); `/clone` is left exactly as-is.

**Transaction precedent (de-risks Decision 5).** `Handler` holds `pool *pgxpool.Pool` (`handler.go:42`) and `store.Queries.WithTx(pgx.Tx)` exists (`store/db.go:28`), already used by `auth.go`, `agent_templates.go`, `settings.go`, `hosted_workers.go`. The atomic UPDATE-source + INSERT-sibling is direct-precedent plumbing, though it is net-new on this endpoint (the clone path has no `Begin`/`WithTx` today).

**Web — My schedules.** `web/src/pages/Schedules.tsx`: `mine = schedules.filter(s => s.origin === "user")` (`:259`) is the My-schedules set; it renders a flat table of `ScheduleRow`. `cloneSchedule` (`:220`) calls `api.cloneSchedule(s.id)` (no repo arg). "New schedule" + per-row ✎ open `ScheduleModal`. `enableDefault` (`:175`) is the fan-out pattern: `repoIds.map(rid => api.enableCatalogSchedule(rid, slug))` with a "Enabled … on ok of N repos; failed" message (`:191-194`).

**Web — the create modal.** `web/src/components/ScheduleModal.tsx`: single-repo `<Select value={repoId}>` (`:582`); submit calls `api.createSchedule(repoId, buildInput())` on create (`:467`), `api.updateSchedule` on edit (`:465`). `buildInput` (`:419`).

**Web — the reusable pieces.** `RepoMultiSelect` — `web/src/components/RepoMultiSelect.tsx`, props `{repos, selected, onChange, disabledIds?, disabledReason?, label?, id?}` (`:14-30`; note: `disabledIds`/`disabledReason`, **not** a boolean `disabled`), currently used only by `DefaultJobs.tsx`. Grouped/expandable layout — `DefaultJobs.tsx`: `CatalogRow` summary (`:184`, takes a `CatalogEntry`, renders the `DEFAULT` chip and lock), `SubRow` per-repo line (`:376`), `EnableAnotherRepo` (`:456`, filters to repos not yet materialized for the slug), grouped by `catalog_slug` via `catalog.entries.map` (`:157`) and an `expanded: Set<string>` (`:70`), `Chip` helper (`:509`). These are un-exported and catalog-coupled — M3 extracts a neutral variant (see Solution overview).

**CLI.** `api/cmd/uzi/schedule.go`: `create` takes a repeatable `--repo` (`Flags().StringArray("repo", …)`, `:409`) and fans out one create per repo (loop at `:394-399`); `clone` takes `--repo` to clone into a different repo (`:342`). Client methods `CreateSchedule`/`CloneSchedule` on `Client`/`HTTPClient`/`FakeClient`. `docs/cli.md` + the `uzi-cli` SKILL are parity-checked (skill-drift gate).

**Offline-resolvable.** Every fact above is a codebase read; nothing in this PRD needs the open internet. A uzi worker (restricted egress: forge + `*.anthropic.com` + package caches) can implement and verify it fully. Gates are the existing `task` targets CI already invokes; no new toolchain, no new CI job.

---

## Scope constraints

- **No `.github/workflows/**` changes** in implementation or validation (worker PAT lacks `workflow` scope; `.claude/rules/prds.md`). This PRD needs none — schema + api handlers/queries + web + CLI + docs, all gated by existing `task` targets.
- **No fire-path change.** `sibling_group_id` is display-only; `schedsvc` fire logic is untouched. Adding the column must not alter how any schedule fires.
- **No linked-schedule behavior.** Do not add edit-propagation, a shared record, or removal-from-a-set semantics (Decision 1). Each sibling stays a normal independent row.
- **`/clone` semantics are frozen.** Group stamping lives in the new `add-repo` endpoint only; do not change what `/clone` does.
- Migration gets its final number (next free above the live head) at merge.

---

## Milestones

Ordered; each is independently landable and gate-green including its own tests (first coverage lands with the code, per `.claude/rules/go.md`).

### M1 — Schema + api: `sibling_group_id`, `add-repo` endpoint (api + store)
- Migration (draft `00161`): `ADD COLUMN sibling_group_id uuid` (nullable, no default, no FK) on `run_schedules`, plus the partial unique index `(sibling_group_id, repo_id) WHERE sibling_group_id IS NOT NULL` (Decision 10). Backfill is a no-op (existing rows stay `NULL`).
- `CreateRunSchedule` accepts and inserts `sibling_group_id` (nullable). `ScheduleDTO` surfaces `SiblingGroupID *string`; the create input accepts an optional, **create-only, validated** `sibling_group_id` (malformed → `400`). `sqlc generate` (assert no-op regenerate).
- **New `POST /api/schedules/{id}/add-repo`** (Decision 5): owner-scoped, `origin='user'` only, in one `WithTx`: coalescing source UPDATE (`COALESCE(sibling_group_id, @new) … RETURNING`) then a config-copy INSERT on the target repo with the returned group id. A duplicate target repo conflicts on the unique index → `409` (idempotent-safe). `apitypes.AddScheduleRepoRequest`. `/clone` is untouched.
- **Delete hygiene:** `DeleteRunSchedule` clears the group id off the last surviving sibling when a delete drops the group to one live member (best-effort; the view collapse in M3 is the load-bearing guarantee).
- **DoD tests (LiveDB, via `./e2e/run-store-it.sh`):** migration + index apply; a create with a `sibling_group_id` stores it, one without stores `NULL`; add-repo on a `NULL`-group source coalesces and both rows share one id; add-repo on an already-grouped source makes the new row join it; a duplicate add-repo (same target repo) hits the unique index and returns `409` without creating a second row; a foreign source/repo 404s; a repoint (`UpdateRunSchedule`) onto a repo already in the sibling's group is rejected by the index. **Regression guard (not a discriminating test — schedsvc reads none of the column, so this only proves the additive column did not break the `SELECT *`/`RETURNING *` struct scan):** a pre-existing ungrouped schedule still fires after the migration.
- **Deps**: none. **Gate**: `task gate:api` + store-it sweep (mandatory — every DoD test here is LiveDB and self-skips under a bare `gate:api`).

### M2 — Web: multi-repo create in the schedule modal
- In **create** mode, replace the single repo `<Select>` with `RepoMultiSelect`; **edit** mode keeps the single repoint `<Select>` unchanged (Decision 6). Show the "N repos → N schedules" affordance.
- On submit with N≥1 repos: for N==1 send no group id (standalone); for N>1 generate one `sibling_group_id` (uuid v4) client-side and fan out N independent `api.createSchedule(repoID, input)` calls, each carrying that group id. Accumulate results; on partial failure keep what landed and show "created N of M, K failed" (Decision 7).
- Wire the **sweep-label guardrail per selected repo** for a sweep-target create (reuse PRD #589 M4; advisory, never blocks).
- **DoD tests (vitest):** create with 2 repos issues 2 `api.createSchedule` calls whose recorded args carry the **same** group id (assert cross-call equality of the recorded arg, not a fixed uuid), and 1-repo create carries **no** group id; edit mode renders the single repoint select, not `RepoMultiSelect`; partial-failure message renders; sweep-warn renders for a sweep target with a missing label. Follow the copy-change / negative-assertion discipline in `.claude/rules/web.md`.
- **Deps**: M1. **Gate**: `task gate:web`.

### M3 — Web: neutral grouped view + "add another repo" (the layout extraction)
- **Extract** a neutral, catalog-agnostic summary+sub-row from `DefaultJobs.tsx`'s `CatalogRow`/`SubRow` (Solution overview): the summary carries the **schedule name + repo-count + expand toggle only** (no `DEFAULT` chip, no lock, no single cadence — siblings may have diverged, so per-repo cadence/target/next/last/options/on live in the **sub-rows**); the Default-jobs tab re-renders through the extracted component (default variant re-adds its chip/lock).
- Group the `mine` rows (`Schedules.tsx:259`) by `sibling_group_id` per Decision 3: `NULL` and single-live-member non-null ids render as standalone `ScheduleRow`s; a non-null id with ≥2 live members renders as one expandable summary over per-repo sub-rows. Per-sub-row controls: edit (✎), pause/resume, run-now, remove.
- **"Add another repo"** action on a summary (and on a standalone row, which promotes it into a group): calls the M1 `add-repo` endpoint; auto-expand and show the new sibling.
- **DoD tests (vitest):** two `mine` rows sharing a group id render as one expandable summary with 2 sub-rows; **two `NULL`-group rows render as two standalone `ScheduleRow`s with NO summary/expand control present** (assert the *absence* of the group affordance, not merely that two rows appear — the naive `groupBy(NULL)` bug renders one summary with two sub-rows and would pass a bare "2 rows" check); a non-null group with one live member renders standalone; "add another repo" calls the endpoint and the new sibling appears; per-sub-row edit/pause/remove target only that row.
- **Deps**: M1 (the DTO field and the endpoint are M1 deliverables; M3's vitest mocks the API, so it needs no M2 code — M3 may run in parallel with M2). **Gate**: `task gate:web`.

### M4 — CLI parity
- `uzi schedule create --repo A --repo B` (already repeatable, loop `:394-399`): when N>1, generate one `sibling_group_id` and pass it in each create so the rows share a group; N==1 sends none.
- New `uzi schedule add-repo <id> --repo <repoID>`: calls the M1 endpoint (client method `AddScheduleRepo` on `Client`/`HTTPClient`/`FakeClient`). `--json` clean on stdout; a `409` duplicate prints a friendly "already on that repo" and exits 0-or-clean.
- `docs/cli.md` + the `uzi-cli` SKILL updated (skill-drift parity green).
- **DoD tests:** `schedule create` fan-out stamps a shared group id across N creates and none for a single repo; `add-repo` issues the call; a duplicate `add-repo` is reported cleanly; partial-failure still reports landed rows.
- **Deps**: M1. **Gate**: `task gate:api`.

### M5 — Default-jobs tab polish (web-only, independent)
- Drop the `DEFAULT` chip on Default-tab **rows** (`DefaultJobs.tsx` / the extracted default variant) — redundant with the tab header; it appears nowhere else on a row.
- Harden the Target cell: put name + type pill + lock in a flex container that keeps the type pill on the name line and wraps as a left-aligned unit, so a longer name never dangles the pill under the description. **Keep** the lock icon.
- **DoD tests (vitest):** the Default-tab row no longer renders a `DEFAULT` chip (`queryByText('DEFAULT')` → null — the fully discriminating half); the type pill and lock render as siblings inside the name's flex container, ordered before the description node (structural assertion — jsdom has no layout engine, so this asserts DOM structure/order, **not** visual non-wrap; a pure-CSS regression that keeps DOM order would not be caught here).
- **Deps**: none (can run in Phase 1 with M1). **Gate**: `task gate:web`.

### M6 — Docs + final integration sweep
- `docs/scheduling.md`: document custom multi-repo create (N repos → N schedules), "add another repo", the grouped/expandable My-schedules view, and that siblings are independent (edits do not propagate). Update `ARCHITECTURE.md` only if a note is warranted; `docs/cli.md` for the new verbs; record decisions in `specs/ai.md`. `check-docs` (`web/scripts/check-docs.mjs`) must pass.
- Final `./e2e/run-store-it.sh` sweep across the landed milestones.
- **Deps**: M1–M5. **Gate**: `task gate:web` (check-docs), `task gate:repo`, store-it sweep.

---

## Parallelization plan

| Phase | Milestones | Rationale |
|---|---|---|
| 1 | **M1** (schema+api+add-repo), **M5** (Default-tab polish) | M1 unblocks the multi-repo work; M5 touches only the Default-jobs layout and is fully independent. |
| 2 | **M2** (web create), **M3** (grouped view + add-repo), **M4** (CLI) | All three depend on M1 only. M2 (modal) and M3 (list view + layout extraction) touch mostly separate files; M4 is CLI. M3 is the riskiest (the extraction), so it can be sequenced first-among-equals but does not block M2/M4. |
| 3 | **M6** (docs + final sweep) | After behavior lands. |

## Risks

- **Add-repo grouping race** (Decision 5). Mitigate: the coalescing `UPDATE … COALESCE(…, @new) … RETURNING` under the row lock (not a read-then-write), plus the `(sibling_group_id, repo_id)` unique index; M1 LiveDB tests for null-source, already-grouped-source, duplicate-repo (409), and foreign-repo.
- **Singleton non-null group** from a delete, a partial-failure create, or a repo-disconnect CASCADE. Mitigate: Decision 3's view collapse (load-bearing, needs no DB write) + best-effort delete-path cleanup; explicit M3 test.
- **Layout extraction leaks default-only chrome.** `CatalogRow`/`SubRow` assume a catalog entry. Mitigate: extract a neutral summary/sub-row (M3) and re-render the Default-jobs tab through it, rather than parameterizing in place; the default variant re-adds chip/lock.
- **`/clone` regression.** Overloading clone would silently group same-repo clones. Mitigate: a *separate* `add-repo` endpoint; `/clone` frozen (scope constraint).
- **Additive-column scan break.** A new column flows into every `SELECT *`/`RETURNING *` struct. Mitigate: M1's regression guard (a pre-existing schedule still fires) — noted as a canary, not a discriminating test, since no code reads the column.

## Success criteria

1. Creating a custom schedule with 2 repos selected in the modal yields 2 independent schedules sharing a sibling group, shown as one expandable summary row (name + repo-count) over 2 sub-rows.
2. "Add another repo" on an existing custom schedule adds a sibling on the new repo via the dedicated `add-repo` endpoint (source + new share a group, allocated race-safely); a duplicate add-repo is a clean no-op/409.
3. Editing or pausing one sibling does not affect the others (independent rows).
4. A single-repo custom schedule renders as a plain standalone row; a group that shrinks to one live member (delete / partial failure / repo-disconnect) also renders standalone; many single-repo customs never collapse into one group.
5. Default-jobs tab: no `DEFAULT` chip on rows (discriminating test); the type pill and lock sit in the name's flex container ordered before the description (structural test; visual non-wrap is design intent, not asserted in jsdom); the lock icon is retained.
6. CLI: `schedule create --repo A --repo B` makes 2 grouped siblings; `schedule add-repo <id> --repo X` adds one; `docs/cli.md` + SKILL parity green.
7. `task gate` green across api/web/repo; `./e2e/run-store-it.sh` sweep green.
