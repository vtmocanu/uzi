# PRD #914: CI autofix on by default (simple)

**Issue**: #914
**Status**: Draft — ready for implementation
**Priority**: Medium
**Split from**: #908 (this was #908's former Part B; #908 keeps Part A — autofix for scheduled runs).
**Scope**: `api/` (one migration) + `docs/`. No `web/` change, no Go type change, no gate change.
No `.github/workflows/**` changes.

## Problem

CI autofix (`kind='ci_fix'`, `api/internal/poller/ci_autofix.go`) pushes a fix when an agent-MR
head pipeline fails. It is user opt-in, default-off: `users.ci_autofix_enabled BOOLEAN NOT NULL
DEFAULT false` (migration 00115), gated in `ListCIAutofixCandidateRefs`
(`ci_autofix.sql:67`, `AND u.ci_autofix_enabled`). Because the default is off and there is no
promotion, most users never get automatic CI fixes even where the feature applies.

## Solution

Turn CI autofix on for everyone — **the deliberately simple version**. Flip the column default to
`true` and backfill existing rows on; keep everything else exactly as it is:

- **No type change** — `ci_autofix_enabled` stays a plain `NOT NULL bool`.
- **No candidate-query change** — `ci_autofix.sql:67` `AND u.ci_autofix_enabled` keeps working;
  after the backfill every user is `true`, and new users default `true`.
- **No web change, no `bool → pgtype.Bool` ripple, no admin global.** Opt-out still works exactly as
  today (a user sets their toggle to off).

### Why not the mr_rework mirror (tri-state + admin global)?

That was considered and **deliberately rejected as over-scoped for this ask.** Mirroring mr_rework
(a nullable tri-state + an admin global setting + web tri-state UI) would be the "consistent" end
state, but it pulls in the full `bool → pgtype.Bool` ripple and web work for what is fundamentally a
default flip. The user chose simple. Two honest consequences of that choice, recorded as decisions:

- **D1 — turn everyone on (backfill), do NOT preserve existing opt-outs. This is a deliberate
  simplicity-over-best-practice trade.** Best practice for a cost-incurring feature is to never
  silently override a user's explicit opt-out (i.e. preserve it), but preserving is impossible
  without making the column nullable (the current `NOT NULL` data cannot distinguish "opted out"
  from "never chose") — exactly the tri-state work being avoided here. So the backfill re-enables
  anyone who had turned CI autofix off. This is the explicit ask ("turn it on for everyone"); on
  this instance it is a non-issue (single maintainer), and for self-hosters it is a documented
  one-time change (M2 docs). If preserving opt-outs later matters, the tri-state upgrade
  (mirroring mr_rework) is the follow-up.
- **D2 — no admin kill-switch in this PRD.** With no admin global, an operator turns CI autofix off
  only per-user, not fleet-wide. Acceptable for the simple version; a fleet-wide admin global
  (mirroring `settings.mr_rework_enabled`) is a clean **follow-up** if an operator ever needs it. Flag
  it in the docs so the absence is a known, deliberate gap, not a surprise.

### Independent of #908 (no sequencing)

Unlike the earlier combined plan, the simple version **does not touch `ci_autofix.sql`** — it only
changes a migration + docs. So it does **not** conflict with #908's M4 (which widens
`ci_autofix.sql`'s kind filter) and needs no "land after #908" sequencing. The two PRDs are fully
independent and can run in parallel.

## Milestones

**Offline** = unit-testable. **LiveDB** = needs `./e2e/run-store-it.sh`.

- [ ] **M1 — Migration: default-on + backfill.** *(Offline + LiveDB)*
  New migration (number at merge time; live head is 00181):
  - Up: `ALTER TABLE users ALTER COLUMN ci_autofix_enabled SET DEFAULT true;` then
    `UPDATE users SET ci_autofix_enabled = true WHERE ci_autofix_enabled = false;`
  - Down: `UPDATE users SET ci_autofix_enabled = false; ALTER TABLE users ALTER COLUMN
    ci_autofix_enabled SET DEFAULT false;` — **lossy by construction**: the Up discards which rows
    were originally off, so Down cannot restore them and simply returns everyone to off. State this
    in the migration comment.
  No `sqlc` type change (the column stays `NOT NULL bool`), so `store.User.CiAutofixEnabled` stays
  `bool` and there is no reader ripple. LiveDB test: a user row created after the migration defaults
  to `true`; a pre-existing `false` row is now `true`; and `ListCIAutofixCandidateRefs` returns that
  user's failed-pipeline branch (with a token on file). A user who explicitly sets `false` afterward
  is excluded (the existing gate still enforces opt-out). Run via `./e2e/run-store-it.sh` with a
  positive control (named tests `--- PASS`, `RUN > 0`, zero `--- SKIP`).

- [ ] **M2 — Docs + stale-comment fix.** *(Offline)*
  Update wherever ci_autofix is documented to say it is **on by default** with a per-user opt-out
  (there is no admin global — note that as a deliberate, followed-up gap), and note the one-time
  migration re-enables prior opt-outs. Fix the now-stale in-tree comment at
  `api/cmd/server/main.go:538-540` ("per-user ci_autofix_enabled (default-OFF)") to say default-ON;
  the "kill-switch is simply NOT wiring it" half stays true (this PRD adds no global). Update the web
  copy that reads "Off by default." at `web/src/pages/RunDefaults.tsx:513` — a **string-only** edit,
  no logic change (the toggle stays a plain bool). If `check-docs.mjs` runs in the web build, keep it
  green.

## Success criteria

1. A newly created user gets a `ci_fix` run on a failed agent-MR pipeline without opting in; an
   existing user who was off is now on; a user who explicitly turns it off afterward gets none.
2. The migration flips existing `false` rows to `true` and sets the column default to `true`.
3. Full gate green (`task gate:api`, plus `task gate:web` for the M2 copy edit) and the `*LiveDB`
   sweep with a positive control.

## Risks

- **R1 — re-enables prior opt-outs (accepted).** The backfill turns CI autofix on for everyone,
  including anyone who deliberately turned it off. This is the explicit "turn it on for everyone"
  ask; the docs (M2) call it out so self-hosters are not surprised by new token spend. Preserving
  opt-outs was rejected because it requires the nullable tri-state (not simple).
- **R2 — no fleet-wide kill-switch.** Without an admin global, an operator can only opt users out
  one at a time. Acceptable for the simple version; the admin global is a clean follow-up (mirror
  `settings.mr_rework_enabled`).
- **R3 — Down migration is lossy.** Rollback returns everyone to off; it cannot restore who was
  originally off (the Up discarded that). Stated in the migration comment.
- **R4 — cost on all users.** Default-on means every user's failed agent-MR pipeline can trigger a
  `ci_fix` run burning their token; bounded by the existing `CIAutofixMaxAttempts` cap. (A cheaper
  fleet-wide bound would be R2's admin global — follow-up.)

## Workflow-scope note (uzi worker)

Per `.claude/rules/prds.md`: implementation and validation touch **no** `.github/workflows/**` —
changes are one migration under `api/internal/store/migrations/`, plus docs and one web string, and
the LiveDB sweep runs via `./e2e/run-store-it.sh`. So `git diff --name-only <base>..HEAD` shows zero
`.github/workflows/**` entries — safe for the worker PAT that lacks `workflow` scope.
