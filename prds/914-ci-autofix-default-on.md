# PRD #914: CI autofix on by default (admin global + per-user tri-state)

**Issue**: #914
**Status**: Draft — ready for implementation
**Priority**: Medium
**Split from**: #908 (this was Part B of #908; #908 keeps Part A — autofix for scheduled runs).
**Scope**: `api/` + `web/` + **one migration**. No `agent/` change. No `.github/workflows/**` changes.

## Problem

CI autofix (`kind='ci_fix'`, `api/internal/poller/ci_autofix.go`) pushes a fix when an agent-MR
head pipeline fails. Its enablement is a single user-level boolean,
`users.ci_autofix_enabled BOOLEAN NOT NULL DEFAULT false` (migration 00115), gated in the
candidate query `ListCIAutofixCandidateRefs` (`ci_autofix.sql:67`, `AND u.ci_autofix_enabled`).
There is **no admin global** and the default is **off**, so most users never get automatic CI
fixes even where the feature would apply.

The sibling lane, MR rework (`kind='mr_rework'`, PRD #700/#841), already has the model we want: an
admin global setting (default-on) plus a per-user **nullable tri-state** (NULL = inherit). This
PRD brings CI autofix to that same model so it is **on by default**, reversibly.

## Solution

Mirror mr_rework's admin-global + per-user tri-state:

- **New admin global** `settings.ci_autofix_enabled`, default `"true"` — an instance-wide
  kill-switch, read fail-closed by the detector.
- **Per-user column becomes a nullable tri-state**: `users.ci_autofix_enabled` → nullable
  `boolean`, where NULL = inherit the global default. The candidate gate changes from
  `AND u.ci_autofix_enabled` (requires an explicit true) to `AND u.ci_autofix_enabled IS NOT FALSE`
  (NULL/true pass, only an explicit false excludes) — the exact shape mr_rework uses.

### The migration decision (the one behavior change — chosen by the user)

The current column is `NOT NULL`, so **history cannot distinguish "opted out" from "never chose"**
(both are `false`). The migration therefore:

- drops `NOT NULL` + the `DEFAULT false`, and
- folds existing rows: `UPDATE users SET ci_autofix_enabled = NULL WHERE ci_autofix_enabled =
  false` (fold ambiguous defaults/opt-outs into inherit = on), **preserving** existing `true` rows
  (the only trustworthy signal — nobody gets `true` by default).

**Consequence**: anyone who had deliberately turned CI autofix off is re-enabled (on-by-inherit).
This ships to all self-hosters, not just this instance. The user chose this consistent, reversible
model over the two alternatives (forward-only default that leaves existing users off; or a plain
backfill with no admin global). Mitigations: the admin global turns the whole instance off in one
place, any user can explicitly opt out again after the migration, and the docs must call out the
one-time re-enable so operators are not surprised by new token spend (R1).

### The 7 divergences from a straight mr_rework copy (grounded in current code)

mr_rework is the template, but ci_autofix's implementation differs in ways that make this more than
a copy:

1. **No admin global exists for ci_autofix at all** — `settings.go` has zero `ci_autofix` refs. The
   key const, default, `Defaults` map entry, accessor, and `validateBool` case are all NEW.
2. **The detector has no settings dependency** — `CIAutoFix` (`ci_autofix.go`) has no `set` field,
   `ciAutofixStore` has no settings accessor, and `NewCIAutoFix` + its `cmd/server/main.go:541`
   wiring take no `settings.Cache`. All NEW, to add the fail-closed global read.
3. **The per-user gate lives in SQL, not Go** — mr_rework resolves per-user in application code;
   ci_autofix enforces it in the candidate WHERE clause (`ci_autofix.sql:67`). The `IS NOT FALSE`
   edit has no mr_rework analog.
4. **Different API/UI surface** — ci_autofix is on the `User` DTO (`apitypes/user.go:32`) with
   dedicated endpoints (`handler/ci_autofix_toggle.go`, `PUT /me/ci-autofix` `handler.go:935`,
   `PUT /admin/users/{id}/ci-autofix` `handler.go:1182`), whereas mr_rework rides `PUT /me/settings`.
   The dedicated `setCIAutofixRequest{Enabled bool}` and web `(enabled: boolean)` signatures have no
   inherit/clear value today.
5. **SET-query return shape** — `SetUserCIAutofixEnabled` returns `RETURNING *` (full `User` →
   `toDTO`), and its param is a plain `bool`; both flip to `pgtype.Bool`.
6. **`bool → pgtype.Bool` ripple** — `store.User.CiAutofixEnabled` is `bool` today
   (`models.go:638`); the one hand-written reader is `handler/handler.go:476` (`toDTO`). Plus the
   DTO field, web `api.ts:34`, the SET params, and ~16 sqlc-generated scan sites (regenerate
   automatically but the type flips underneath any bare-bool use).
7. **No web admin card exists** — even mr_rework's global has no typed web admin field; it
   round-trips through the generic `GET/PUT /admin/settings`. So the ci_autofix global is
   backend-only unless someone adds an AppSettings field + card (not required here).

## Milestones

**Offline** = unit-testable with fakes. **LiveDB** = needs `./e2e/run-store-it.sh`. **web** = touches
`web/`.

- [ ] **M1 — Admin global `ci_autofix_enabled` setting + detector fail-closed read.** *(Offline)*
  In `api/internal/settings/settings.go` add (all NEW): `KeyCiAutofixEnabled = "ci_autofix_enabled"`,
  `DefaultCiAutofixEnabled = "true"`, the `Defaults` map entry, a three-state error-propagating
  accessor mirroring `MrReworkEnabled` (~settings.go:870), and the `validateBool` case (~:1439). Wire
  `settings.Cache` into the `CIAutoFix` detector (struct field + `ciAutofixStore` interface +
  `NewCIAutoFix` + `cmd/server/main.go:541`) and add the fail-closed read at the top of `detect`,
  mirroring `mr_review_watch.go:105-112`.

- [ ] **M2 — User column → nullable tri-state + candidate gate + Go-type ripple.** *(Offline + LiveDB)*
  New migration (number at merge time): `ALTER TABLE users ALTER COLUMN ci_autofix_enabled DROP NOT
  NULL, DROP DEFAULT`, then `UPDATE users SET ci_autofix_enabled = NULL WHERE ci_autofix_enabled =
  false`. Change `ci_autofix.sql:67` `AND u.ci_autofix_enabled` → `AND u.ci_autofix_enabled IS NOT
  FALSE`. Regenerate sqlc; the field `store.User.CiAutofixEnabled` flips `bool` → `pgtype.Bool` — fix
  the hand-written reader `handler/handler.go:476` (`toDTO`, resolve inherit→true or expose the
  tri-state) and the `SetUserCIAutofixEnabled` SET-query params.

- [ ] **M3 — Tri-state over the API + web toggles.** *(Offline; web)*
  Retrofit the dedicated endpoints to carry inherit/clear: `handler/ci_autofix_toggle.go`
  (`setCIAutofixRequest`, `SetCIAutofixEnabled` self, `SetUserCIAutofixEnabled` admin), the DTO
  `apitypes/user.go:32` + web `api.ts:34` `User.ci_autofix_enabled` → `boolean | null`. Update the web
  toggles to tri-state, mirroring RunDefaults' mr_rework checkbox (`checked={x !== false}`):
  `web/src/pages/RunDefaults.tsx` (self) and `web/src/pages/AdminUsers.tsx` (admin). No web admin card
  for the global (round-trips through `GET/PUT /admin/settings`).

- [ ] **M4 — Tests + docs.** *(Offline + LiveDB)*
  Migration test: an existing `false` row becomes NULL, a `true` row is preserved. Candidate query:
  a NULL-user run is a candidate (default-on), an explicit-`false` user is excluded, and with the
  admin global off the detector skips the whole repo (fail-closed). Settings accessor test mirroring
  mr_rework's. Web tri-state toggle test. Docs: wherever ci_autofix is documented, state it is on by
  default with the admin global + per-user opt-out, and note the one-time migration re-enables prior
  opt-outs.

## Success criteria

1. A user who never touched the setting (NULL) gets a `ci_fix` run on a failed agent-MR pipeline; an
   explicit per-user `false` gets none; the admin global set to false suppresses it instance-wide.
2. The migration turns a prior `false` row into inherit (on) and preserves a prior `true` row.
3. Full gate green (`task gate:api`, `task gate:web`) plus the `*LiveDB` sweep with a positive
   control (named tests `--- PASS`, `RUN > 0`, zero `--- SKIP`).

## Risks & dependencies

- **R1 (the headline risk) — the migration re-enables prior opt-outs.** The current column is NOT
  NULL, so the fold cannot preserve a deliberate opt-out; folding `false → NULL` turns CI autofix
  back on for anyone who had switched it off, across all self-hosters. Mitigations: the new admin
  global (one-place instance-off), explicit per-user re-opt-out, and a docs call-out so operators are
  not surprised by new token spend.
- **R2 — `bool → pgtype.Bool` semantic slips.** A site that treats the scanned value as a bare bool
  (or reads NULL as false) compiles but breaks inherit. The gate + M4's candidate-query test are the
  guard; audit the ~16 scan sites and the `toDTO` reader specifically.
- **R3 — sequencing with #908.** Both #908 (Part A, M4 widens `ci_autofix.sql`'s kind filter) and
  this PRD (M2 changes `ci_autofix.sql`'s user gate) edit the same query + its generated
  `.sql.go`. **Land this AFTER #908 merges** so M2 rebases onto the widened filter; racing both means
  the second MR conflicts on that file.
- **R4 — cost on all users.** Default-on means every user's failed agent-MR pipeline can trigger a
  `ci_fix` run burning their Anthropic token; bounded by the existing `CIAutofixMaxAttempts` cap and
  the new admin global + per-user opt-out.

## Workflow-scope note (uzi worker)

Per `.claude/rules/prds.md`: implementation and validation touch **no** `.github/workflows/**` —
changes are in `api/` (incl. one migration under `api/internal/store/migrations/`) and `web/`, and
the LiveDB sweep runs via `./e2e/run-store-it.sh`. So `git diff --name-only <base>..HEAD` shows zero
`.github/workflows/**` entries — safe for the worker PAT that lacks `workflow` scope.
