# PRD #914: CI autofix on by default (mirror mr_rework — admin global + per-user tri-state)

> Anchors re-verified against main at fcfd8aa on 2026-09-03, after the epic #915 file splits.
> **Precondition status 2026-09-05: #908 is on main** (`forgesvc/scheduled_mr_watch.go` present; `ListCIAutofixCandidateRefs` filters `r.kind IN ('issue','ci_fix','prompt','self_improve')`). The precondition gate passed and this PRD was implemented.
>
> **As-built deltas (2026-09-05):** the migration landed as **00190** (the live head was 00189, not the drafted 00185/00186). The DTO/type became a nullable `*bool` / `boolean|null` **end-to-end** (M2's `toDTO` maps `pgtype.Bool`→`*bool` null-preserving via the existing `boolPtrValue`, and M3 carried the write-path pointer for clear-to-inherit) rather than the drafted interim "resolve inherit→true, DTO stays bool" — the tri-state renders on the web via `checked={x !== false}`, mirroring mr_rework. The admin global read reuses `Cache.CiAutofixEnabled` mirroring `MrReworkEnabled` (fail-closed in the detector).

**Issue**: #914
**Status**: Done — implemented 2026-09-05 (all milestones landed; gate:api + gate:web green; 5-package `*LiveDB` sweep green)
**Priority**: Medium
**Split from**: #908 (this was #908's former Part B; #908 keeps Part A — autofix for scheduled runs).
**Scope**: `api/` (one migration) + `web/`. No `agent/` change. No `.github/workflows/**` changes.

## 🔴 HARD PRECONDITION — DO NOT START UNLESS #908 HAS ALREADY MERGED TO `main`

**If you are a uzi worker (or any implementer) picking this up: STOP and check first.** This PRD
edits the exact same query that #908 edits (`ListCIAutofixCandidateRefs` in
`api/internal/store/queries/ci_autofix.sql`). It **must not be started before #908 lands** — doing
so produces a guaranteed merge conflict and can build on a candidate query that has not yet been
widened for scheduled runs.

**Precondition check (offline — run against the cloned `main` you are working from):**

1. The recorder file `api/internal/forgesvc/scheduled_mr_watch.go` **exists** (it is created only by
   #908), **and**
2. `ListCIAutofixCandidateRefs` in `api/internal/store/queries/ci_autofix.sql` already lists
   `'prompt'` and `'self_improve'` in its `r.kind IN (...)` filter (widened by #908's M4).

**If EITHER check fails, #908 has NOT landed. Do NOT plan, do NOT implement, do NOT open an MR.**
Immediately cancel/abort this run and report that #914 is blocked on #908 (not yet merged). It is
correct to cancel here — this is a deliberate ordering gate, not a failure. The run can be
re-triggered after #908 is on `main`.

Only when **both** checks pass may you proceed with the milestones below.

## Problem

CI autofix (`kind='ci_fix'`, `api/internal/poller/ci_autofix.go`) pushes a fix when an agent-MR
head pipeline fails. Its enablement is a single user-level boolean,
`users.ci_autofix_enabled BOOLEAN NOT NULL DEFAULT false` (migration 00115), gated in the candidate
query `ListCIAutofixCandidateRefs` (`ci_autofix.sql:76`, `AND u.ci_autofix_enabled`). There is
**no admin global** and the default is **off**, so most users never get automatic CI fixes.

The sibling lane, MR rework (`kind='mr_rework'`, PRD #700/#841), already has the model we want: an
admin global setting (default-on) plus a per-user **nullable tri-state** (NULL = inherit). This PRD
brings CI autofix to that same model so it is **on by default**, with a fleet-wide kill-switch, and
stops the two sibling autofix features from diverging.

## Solution

Mirror mr_rework's admin-global + per-user tri-state, and turn everyone on:

- **New admin global** `settings.ci_autofix_enabled`, default `"true"` — an instance-wide
  kill-switch, read fail-closed by the detector (mirrors `MrReworkEnabled`).
- **Per-user column becomes a nullable tri-state**: `users.ci_autofix_enabled` → nullable
  `boolean`, NULL = inherit the global default. Gate changes from `AND u.ci_autofix_enabled` to
  `AND u.ci_autofix_enabled IS NOT FALSE` (NULL/true pass, explicit false excludes) — mr_rework's
  shape.
- **Turn everyone on now**: the migration folds existing `false → NULL` (inherit = on) and preserves
  existing `true`. New users get NULL (no default) = inherit = on automatically.

### Migration decision (turn everyone on) and its consequences

The current column is `NOT NULL`, so history cannot distinguish "opted out" from "never chose"
(both are `false`). The chosen approach **turns everyone on** by folding `false → NULL`:

- **This re-enables prior opt-outs** — a deliberate, accepted trade (the ask was "on for everyone").
  Best practice would preserve explicit opt-outs, but that is what the tri-state *going forward*
  gives us; for the *existing* NOT-NULL data it is unrecoverable, so we fold. On this instance
  (single maintainer) it is a non-issue; for self-hosters it is a documented one-time change (R1).
- **New users get default-on for free.** `CreateUser` and the OIDC insert (`queries/users.sql`) do
  **not** set `ci_autofix_enabled` — they relied on the column DEFAULT. Dropping the default means a
  new row is NULL = inherit = on, with no seeding change and no path that silently re-introduces
  `false` (a confirmed positive, not a regression — stated because a reader might fear `DROP DEFAULT`
  orphans user provisioning).

### The 7 divergences from a straight mr_rework copy (grounded in current code, review-verified)

mr_rework is the template, but ci_autofix differs in ways that make this more than a copy:

1. **No admin global exists for ci_autofix** — `settings.go` (package `settings`) has zero
   `ci_autofix` refs. The key const, default, and `Defaults` map entry belong in
   `api/internal/settings/keys.go` (mirroring `KeyMrReworkEnabled` at `:224`,
   `DefaultMrReworkEnabled` at `:339`, and its `Defaults` entry at `:449`); the accessor belongs
   in a new `api/internal/settings/settings_ci_autofix.go`, mirroring `MrReworkEnabled` in
   `settings_mr_rework.go:35`; and the `validateBool` case is added to the dispatch switch in
   `settings.go` at `:377-380` (`validateBool` itself at `:442`). All NEW.
2. **The detector has no settings dependency** — `CIAutoFix` (`ci_autofix.go:59-60`) has only `q
   ciAutofixStore`, no `set`; `ciAutofixStore` (`:25-26`) has no settings accessor; `NewCIAutoFix`
   (`:74`) + its `cmd/server/main.go:541` wiring take no `settings.Cache`. All NEW, to add the
   fail-closed global read (mirror `mr_review_watch.go:105-112`, whose accessor genuinely
   propagates the store error / fails closed).
3. **The per-user gate is in SQL, not Go** — `ci_autofix.sql:76` (generated `ci_autofix.sql.go:143`).
   The `IS NOT FALSE` edit has no mr_rework analog. **No second gate exists**: the only other
   `ci_fix` create path is the manual "Fix CI" button (`handler/ci_fix.go` → `CreateCIFixRun`), which
   does not read `ci_autofix_enabled` at all (human-approved by design), so changing only the SQL is
   sufficient.
4. **Different API/UI surface** — ci_autofix is on the `User` DTO (`apitypes/user.go:36`) with
   dedicated endpoints (`handler/ci_autofix_toggle.go`, `PUT /me/ci-autofix` `routes_me.go:125`,
   `PUT /admin/users/{id}/ci-autofix` `routes_admin.go:99`), not `PUT /me/settings`. Both handler
   methods call the single store query `SetUserCIAutofixEnabled` (one param struct to flip). The
   `setCIAutofixRequest{Enabled bool}` and web `(enabled: boolean)` signatures have no inherit/clear
   value today. **No CLI surface** (`api/cmd/uzi` has none — correct to omit).
5. **SET-query return shape** — `SetUserCIAutofixEnabled` returns `RETURNING *` (full `User` →
   `toDTO`); its param flips `bool → pgtype.Bool`.
6. **`bool → pgtype.Bool` ripple** — `store.User.CiAutofixEnabled` is `bool` (`models.go:639`); the
   one hand-written reader is `handler/handler.go:477` (`toDTO`; it is display-only, but a naive
   `.Bool` there would show "Off" for a default-on NULL user, so resolve inherit→true). The two
   hand-written writers are `handler/ci_autofix_toggle.go:37` and `:65`. Plus ~16 sqlc-generated
   scan sites (15 in `users.sql.go` + `slack.sql.go:98`) that regenerate automatically — the
   compiler catches every bare-bool use.
7. **No web admin card** — even mr_rework's global has no typed web admin field; it round-trips
   through the generic `GET/PUT /admin/settings`. The ci_autofix global is backend-only.

## Milestones

**Offline** = unit-testable with fakes. **LiveDB** = needs `./e2e/run-store-it.sh`. **web** = touches
`web/`.

- [x] **M1 — Admin global `ci_autofix_enabled` setting + detector fail-closed read.** *(Offline)*
  In `api/internal/settings/keys.go` add (all NEW): `KeyCiAutofixEnabled = "ci_autofix_enabled"`,
  `DefaultCiAutofixEnabled = "true"`, and the `Defaults` map entry (mirroring `KeyMrReworkEnabled`
  `:224`, `DefaultMrReworkEnabled` `:339`, `Defaults` entry `:449`). In a new
  `api/internal/settings/settings_ci_autofix.go` add a three-state error-propagating accessor
  mirroring `MrReworkEnabled` (`settings_mr_rework.go:35`). In `settings.go` add the
  `validateBool` case to the dispatch switch at `:377-380` (`validateBool` itself at `:442`). Wire
  `settings.Cache` into the `CIAutoFix` detector (struct field + `ciAutofixStore` interface +
  `NewCIAutoFix` + `cmd/server/main.go:541`) and add the fail-closed read at the top of `detect`,
  mirroring `mr_review_watch.go:105-112`.
  **Also fix the now-stale comment at `cmd/server/main.go:538-539`** ("per-user ci_autofix_enabled
  (default-OFF)" and "kill-switch is simply NOT wiring it") — both become false once the global exists.

- [x] **M2 — User column → nullable tri-state + candidate gate + Go-type ripple.** *(Offline + LiveDB)*
  New migration (number at merge time; live head 00185, so this lands at 00186):
  - Up: `ALTER TABLE users ALTER COLUMN ci_autofix_enabled DROP NOT NULL, DROP DEFAULT;` then
    `UPDATE users SET ci_autofix_enabled = NULL WHERE ci_autofix_enabled = false;` (existing `true`
    preserved).
  - Down (**lossy — state it in the migration comment**): `UPDATE users SET ci_autofix_enabled =
    false WHERE ci_autofix_enabled IS NULL;` then `ALTER TABLE users ALTER COLUMN ci_autofix_enabled
    SET NOT NULL, SET DEFAULT false;`. Rollback collapses every inherit-on (NULL) row to off; it
    cannot restore the pre-Up values. (Unlike mr_rework's Down, which is a trivial `DROP COLUMN`
    because 00165 *added* a nullable column — this ALTERs an existing NOT-NULL one, so the template's
    Down does not transfer.)

  Change `ci_autofix.sql:76` `AND u.ci_autofix_enabled` → `AND u.ci_autofix_enabled IS NOT FALSE`.
  Regenerate sqlc; `store.User.CiAutofixEnabled` flips `bool` → `pgtype.Bool` — fix `handler/handler.go:477`
  (`toDTO`, resolve inherit→true) and the `SetUserCIAutofixEnabled` param.

- [x] **M3 — Tri-state over the API + web toggles.** *(Offline; web)*
  Retrofit the dedicated endpoints to carry inherit/clear: `handler/ci_autofix_toggle.go`
  (`setCIAutofixRequest`, both handler methods → the single `SetUserCIAutofixEnabled` param as
  `pgtype.Bool`), the DTO `apitypes/user.go:36` + web `apiTypes.ts:23` `User.ci_autofix_enabled` →
  `boolean | null`. Update the web toggles to tri-state, mirroring RunDefaults' mr_rework checkbox
  (`checked={x !== false}`): `web/src/pages/RunDefaults.tsx` (self, incl. the "Off by default." copy at
  `:577` — **the file has FOUR "Off by default." lines, at `:360`/`:391`/`:449`/`:577`; only `:577`
  is the ci_autofix card**, checkbox `checked={user?.ci_autofix_enabled ?? false}` at `:587`) and
  `web/src/pages/AdminUsers.tsx` (admin). **Do not miss these ripple sites** (review-found): the two
  web API client methods `web/src/lib/api.ts:369-370` (`setUserCIAutofixEnabled`) + `:427-428`
  (`setCIAutofixEnabled`) `(enabled: boolean)` → `boolean | null`; both mock setters
  `web/src/mocks/mockApi/settings.ts:679-688` (`setCIAutofixEnabled` `:679`, `setUserCIAutofixEnabled`
  `:685`); and the fixtures that hardcode `ci_autofix_enabled: false`
  (`web/src/mocks/data/users.ts` ×6, lines 16/37/59/77/97/115, + ~10 web test files) — update the
  ones M4's positive-control tests assert on so they don't assert "off" for a default-on user.

- [x] **M4 — Tests + docs.** *(Offline + LiveDB)*
  Migration test: an existing `false` row becomes NULL, a `true` row is preserved, a fresh row is NULL.
  Candidate query (**extend `api/internal/store/ci_autofix_integration_test.go:55-59`, which seeds only
  `{true,false,true}` — add a NULL case**): a NULL-user run is a candidate (default-on), an explicit
  `false` user is excluded, and with the admin global off the detector skips the whole repo
  (fail-closed). Settings accessor test mirroring mr_rework's. Web tri-state toggle test. Docs:
  wherever ci_autofix is documented, state it is on by default with the admin global + per-user
  opt-out, and note the one-time migration re-enables prior opt-outs. `*LiveDB` sweep via
  `./e2e/run-store-it.sh` with a positive control (named tests `--- PASS`, `RUN > 0`, zero `--- SKIP`).

**Ordering**: M1 (global + detector) and M2 (migration + gate + types) are largely disjoint and can
run in parallel; M3 depends on M2's type flip; M4 tests all three.

## Success criteria

1. A user who never touched the setting (NULL) — including every newly created user — gets a `ci_fix`
   run on a failed agent-MR pipeline; an explicit per-user `false` gets none; the admin global set to
   false suppresses it instance-wide.
2. The migration turns a prior `false` row into inherit (on), preserves a prior `true` row, and a
   fresh user row is NULL (on).
3. Full gate green (`task gate:api`, `task gate:web`) plus the `*LiveDB` sweep with a positive control.

## Risks & dependencies

- **R1 — re-enables prior opt-outs (accepted).** Folding `false → NULL` turns CI autofix on for
  everyone, including anyone who deliberately turned it off. Explicit ask; mitigated by the new admin
  global (one-place instance-off), explicit per-user re-opt-out, and a docs call-out (M4) so operators
  are not surprised by new token spend.
- **R2 — Down migration is lossy.** Rollback collapses every inherit-on (NULL) row to off; it cannot
  restore pre-Up values. Stated in the migration comment (M2). The feature is reversible (admin
  global); the *schema rollback* is not.
- **R3 — sequencing with #908 (now a HARD precondition, see the top of this PRD).** Both #908
  (Part A, M4 widens `ci_autofix.sql`'s kind filter, line 44) and this PRD (M2 changes
  `ci_autofix.sql`'s user gate, line 67) edit the same query + its generated `.sql.go`. This is no
  longer "prefer to land after" — it is a **STOP gate**: the worker must run the precondition check
  at the top and **cancel the run** if #908 has not merged, rather than produce a conflicting MR.
- **R4 — `bool → pgtype.Bool` semantic slips.** A site treating the scanned value as a bare bool (or
  reading NULL as false) compiles but breaks inherit — the `toDTO` display reader especially. The gate
  + M4's candidate-query NULL-case test are the guard.
- **R5 — cost on all users.** Default-on means every user's failed agent-MR pipeline can trigger a
  `ci_fix` run burning their token; bounded by the existing `CIAutofixMaxAttempts` cap (`main.go:541`)
  and the new admin global + per-user opt-out.

## Workflow-scope note (uzi worker)

Per `.claude/rules/prds.md`: implementation and validation touch **no** `.github/workflows/**` —
changes are in `api/` (incl. one migration under `api/internal/store/migrations/`) and `web/`, and the
LiveDB sweep runs via `./e2e/run-store-it.sh`. So `git diff --name-only <base>..HEAD` shows zero
`.github/workflows/**` entries — safe for the worker PAT that lacks `workflow` scope.
