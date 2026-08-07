# PRD #239: Runs-in-progress count badge on the Runs menu item

**GitLab Issue**: [#239](https://gitlab.example.com/vtmocanu/uzi/-/issues/239)
**Status**: Draft (created 2026-08-07; reviewed same day by an architect agent against the codebase — 5 documentation-accuracy fixes applied, no blockers: `ListRunsForUser` rename, kind-set pinned, "same scope predicate" not "to the digit", `unread_count` is `RequireAuth` not `RequireUser`, favicon's third status idiom surfaced. See the Decision Log.)
**Priority**: Low
**Mock**: `prds/mockups/239-runs-in-progress-badge-mock.html` (shown to owner 2026-08-07)

## Problem

The Runs sidebar item (`web/src/components/AppShell.tsx`, the `/runs` `NavItem`)
carries no at-a-glance signal of factory activity. To answer "is anything running
right now, and how much?" you must open the Runs page. Meanwhile three siblings in
the same sidebar already answer their own version of that question inline:

- **Notifications** — a brand count pill of unread items (`unread`, PRD #46 M2).
- **Judge** — a brand count pill of the to-triage backlog (`judgeTodo`, PRD #98).
- **Workers** — a red *alert* pill of workers needing attention (`workersAttention`, PRD #113 M6).

Runs is the one high-traffic destination in the "Work" group with no badge, even
though "how busy is the factory" is exactly the kind of ambient signal a nav badge
is for. The count already exists conceptually — the Runs page renders it — it is
just not surfaced where the operator spends their time.

## Solution Overview

Add a fourth nav badge, on the Runs item, showing the caller's count of
**in-progress runs**, reusing the existing `NavItem` `badge` mechanism verbatim
(brand "count" tone, count pill expanded, brand dot on the collapsed rail, `sr-only`
count for assistive tech). It sits beside Notifications and Judge and reads as "a
queue to get to / activity", not the red "go look now" alert tone Workers uses.

The count is served by a **dedicated, lightweight, owner-scoped count endpoint**
(`GET /api/me/runs/in-progress-count` → `{ "count": <int> }`), computed by a single
indexed `count(*)` query and polled by `AppShell` on navigation — the same badge-source
shape as Notifications, Judge, and Workers. Mounted on `RequireUser` (so a CLI token
can read it too), matching the Judge (`/me/judge/stats`, `JudgeStats`) and Workers
(`/me/workers/upgrade-summary`, `WorkerUpgradeSummary`) endpoints; the Notifications
count (`/notifications/unread_count`) is the same *badge* pattern but is mounted on
`RequireAuth` (cookie-only), so it is a UX precedent, not the auth-model precedent.
No new service, table, trust boundary, or migration.

The one substantive design question is **which run statuses count as "in progress"**
(Decision 1 below). The rest is wiring that mirrors three existing precedents.

## Design Decisions

### Decision 1 — "In progress" = the caller's non-terminal runs (recommended)

`runs.status` has three terminal values (`completed`, `failed`, `cancelled`;
`TERMINAL_RUN_STATUSES` in `web/src/lib/api.ts`, mirrored by the DB CHECK). Two
status-set idioms already exist in the codebase for "a run that isn't finished":

| Idiom | Statuses included | Existing uses |
|---|---|---|
| **Non-terminal** (recommended) | `queued`, `claimed`, `running`, `awaiting_approval`, `awaiting_input`, `limit_wait` | `autopilot.sql`, `ci_fix.sql`, `selfimprove.sql`, `pipeline_statuses.sql`, `judge.sql` — the dominant store idiom |
| **Actively working** (narrower) | `claimed`, `running`, `awaiting_approval`, `awaiting_input` | `anthropic_rate_limits.sql`, `runtime.sql` (the `ListWorkersByUser` busy sub-select) — excludes `queued` (not yet started) and `limit_wait` (parked on a rate-limit window) |
| **Favicon "running"** (narrowest) | `queued`, `claimed`, `running` | `web/src/lib/favicon.ts` `RUNNING_STATUSES` (PRD #70) — the status-favicon partitions `awaiting_approval`/`awaiting_input` into a separate **"attention"** state and drops `limit_wait` |

**Recommendation: non-terminal — but the choice must be made against the favicon
partition, not just the two SQL idioms.** The status-favicon (`useFavicon`, PRD #70) is
already the shell's ambient "is anything running?" signal, and this PRD's Problem
("how busy is the factory") is essentially that same question rendered as a count. So
whichever set wins, it should be a *deliberate* relationship to the favicon's
partition, not an accidental third definition:

- Non-terminal is the plainest reading of "in progress" and the dominant store idiom
  (a `queued` run is in the pipeline; a `limit_wait` run is live but paused), but it
  folds the favicon's **attention** runs (`awaiting_*`) into the count — overlapping the
  Notifications badge's "waiting on you" job — and counts `queued`/`limit_wait`.
- The narrower sets read as more *distinct* from the sibling signals in the same
  sidebar. If we specifically want the badge to mean "actively consuming a worker", the
  "actively working" set is right; if we want it to mirror the favicon's dot exactly,
  reuse `RUNNING_STATUSES`.

Recorded as the primary question for review + owner sign-off. Corollary (NICE): whatever
set wins, consider making the badge and the favicon share **one** status partition, so
the two ambient "is it running" signals in the same shell never tell subtly different
stories (favicon "attention" vs a badge that folded that run into "in progress").

### Decision 2 — Reuse the "count" tone, not a new "alert" tone

The badge uses the default `badgeTone="count"` (brand), like Notifications and Judge.
Red (`alert`) is reserved for "a worker needs attention now" (PRD #113 Decision 2);
in-progress runs are normal, healthy activity, so brand is correct. `0` renders
nothing at all (no permanent zero ornament), matching every other badge.

### Decision 3 — A dedicated count endpoint, not client-side derivation from the runs list

`web/src/lib/api.ts` already has `listRuns()`, so the badge *could* derive the count
client-side. Rejected: that fetches every run row to use one integer, and it diverges
from the established badge precedents, which each have a single-number server endpoint
backed by an indexed `count(*)`. We add `GET /api/me/runs/in-progress-count` returning
`{ count }`, mounted on `RequireUser` (so a CLI token can read it too), owner-scoped by
the query's `user_id` filter.

Honest caveat: the app is **not** free of full-row runs fetches today — `useFavicon`
(`web/src/lib/useFavicon.ts`, PRD #70) already calls `listRuns()` on a ~20s interval,
*including while the tab is backgrounded*, to derive the favicon state. So the win here
is not "avoid full-row fetching app-wide"; it is "match the badge precedents and keep
the badge's own poll to a single integer on the on-navigation cadence". A per-user
in-progress count does not exist to reuse (`CountWorkerNonTerminalRuns` is per-worker;
`ListActiveRunsAll` is admin/all-users), so a new query is justified. Whether the
favicon poll and this badge should eventually share one source is the NICE follow-up in
Decision 1's corollary.

### Decision 4 — Badge scope = the Runs page's scope predicate (owner + kind-set), pinned

The count's run-scope is the **same predicate the `/runs` page's list uses**, so the
badge can never count a run the page would not show. That page query is
`ListRunsForUser` (`api/internal/store/queries/runtime.sql`), whose scope is already
settled and is pinned here rather than deferred:

```
WHERE r.user_id = @user_id AND r.kind NOT IN ('chat', 'judge')
```

So **`issue` + `ci_fix` + `self_improve` runs count; `chat` and `judge` do not** —
`chat` has its own `/chat` nav item, and `judge` is a repo-less meta-run
(`ListRunsForUser`'s own comment states this rationale). The count query applies this
same `kind NOT IN ('chat','judge')` filter plus the Decision 1 status set.

**Not "agrees to the digit".** The borrowed Judge principle was badge-count == tab-count,
but that does not transfer: the `/runs` page lists **all** the user's runs including
terminal ones (`LIMIT 200`), while this badge counts only the **non-terminal subset**,
so the two numbers will essentially never be equal. The invariant that actually holds is
**"same scope predicate (owner + kind-set)"**, not numeric equality — the badge is a
strict subset of what the page lists.

## Milestones

### M1 — Count endpoint (`api`)
- [x] `count(*)` query in `api/internal/store/queries/runtime.sql` (home of the
  run-lifecycle queries and the analogous per-worker `CountWorkerNonTerminalRuns`),
  owner-scoped, non-terminal (per Decision 1), with the `kind NOT IN ('chat','judge')`
  filter from Decision 4. `sqlc generate` regenerates the `.sql.go` const (verify the
  const moved, per `.claude/rules/go.md`). — `CountInProgressRunsForUser`; const present
  in `api/internal/store/runtime.sql.go`.
- [x] `GET /api/me/runs/in-progress-count` handler returning `{ "count": <int> }`,
  mounted on `RequireUser`, mirroring `WorkerUpgradeSummary` / `UnreadNotificationCount`.
  — `RunsInProgressCount` in `api/internal/handler/runs_in_progress_count.go`; route in
  `handler.go`; route-limiter mount guard row added.
- [x] Live-DB test (`*LiveDB`) exercising the query across every status (proves it
  runs against real Postgres, per the sqlc-green-is-not-evidence rule), plus a handler
  test for the auth + shape. — `TestCountInProgressRunsForUserLiveDB` (all 9 statuses +
  chat + judge + cross-user), `--- PASS` via `./e2e/run-store-it.sh`; handler tests
  `TestRunsInProgressCountRequireAuth` / `...Shape`.
- [x] `task gate:api` green (incl. `-race`, ratcheted lint, deadcode-at-zero). —
  fmt/vet/build/deadcode/`-race` all green; the ratcheted `lint:api` reports only the
  pre-existing inherited backlog because this clone's `origin/main` is a frozen mirror
  ~1375 commits behind the branch base, so it surfaces the whole backlog as false "new"
  findings. Verified this run's changed `.go` files carry ZERO findings in the unfiltered
  golangci-lint backlog.

### M2 — Runs nav badge (`web`)
- [x] `api.runsInProgressCount()` in `web/src/lib/api.ts` (real), with the matching
  method in `web/src/mocks/mockApi.ts` (typechecked against `realApi`'s shape — the
  mock counts non-terminal runs from its own fixtures so the demo build shows it). The
  mock's `listRuns` was tightened to also exclude `judge` (matching `ListRunsForUser`),
  so a `judge` fixture never leaks onto the demo `/runs` page.
- [x] `AppShell` owns a `runsInProgress` state + on-navigation poll (same cadence and
  keep-last-known-on-error handling as the Judge poll), passed to both `SidebarContent`
  mounts (desktop rail + mobile sheet) and rendered as the Runs `NavItem` `badge` with
  the default brand `count` tone.
- [x] Vitest coverage: badge renders the count, hides at 0, survives collapse as the
  sr-only count + dot; mock parity test. **The mock fixture must discriminate the
  boundary** — it must contain terminal *and* non-terminal runs, plus at least one
  `awaiting_*` and one `limit_wait` run, and an excluded `chat`/`judge` run, so the
  parity test pins Decisions 1 and 4 rather than snapshotting the demo's blind spot. —
  a non-terminal `judge` fixture was added; parity test derives the expected count from
  the fixtures and pins each exclusion. Badge assertions are scoped to the Runs nav item
  because the `count` tone shares the `"N unread"` aria-label with the Judge/Notifications
  badges (a global query is ambiguous).
- [x] `task gate:web` green. — 1699 tests pass.

### M3 — CLI parity, docs, and the decision record
- [x] **CLI check** (mandatory per the "new functionality ⇒ check `api/cmd/uzi/`"
  convention): decide whether `uzi runs` / status output should surface the same count.
  Either add it or record it as deliberately out-of-scope with rationale — do not leave
  the CLI silently stale. — **No CLI change (deliberate).** `uzi run list` already prints
  `ID KIND STATUS TITLE` and honours `--json`, so the caller can already derive the
  in-progress count by counting non-terminal, non-`chat`/`judge` rows; a dedicated count
  subcommand would be redundant with the badge's ambient-UI purpose. Recorded in
  `specs/ai.md` §490.
- [x] `specs/ai.md` records Decision 1 (the chosen status set) as an AI design decision.
  — `specs/ai.md` §490 records Decisions 1 and 4, the endpoint contract, and the CLI
  non-decision.
- [x] User-facing docs touched only if an existing page enumerates the nav badges;
  otherwise none (an inline count needs no new doc page). `web/scripts/check-docs.mjs`
  stays green. — no doc enumerates the sidebar badges (`docs/run-activity.md`'s "badge"
  refs are the page's run-status badges); `check-docs` green in the web gate. No doc change.

## Success Criteria

1. The Runs nav item shows a live brand count of in-progress runs, matching the
   `/runs` page, in both the expanded sidebar and (as a dot) the collapsed rail, on
   desktop and mobile.
2. The count comes from one indexed `count(*)` behind a `RequireUser` endpoint; the
   badge's own poll fetches a single integer, not run rows. (Note the favicon poll
   still fetches full rows on its own ~20s interval — Decision 3; this criterion is
   about the badge's endpoint, not an app-wide claim.)
3. The mock/demo build (`VITE_UZI_MOCK=1`) shows the badge from fixtures.
4. `0` renders nothing; a transient fetch error keeps the last known count.
5. Both gates green (`task gate:api`, `task gate:web`); the CLI is either updated or
   explicitly scoped out.

## Risks & Mitigations

- **Poll cost on every page.** Mitigated by an indexed `count(*)` and the existing
  on-navigation cadence (no new interval; the Judge badge already establishes this is
  acceptable). If a stuck run should refresh the badge without navigation, an interval
  can be added later — deliberately deferred, noted like the Workers badge's argument.
- **Badge/page disagreement.** Mitigated by Decision 4 (shared scope predicate).
- **Status-set churn.** If Decision 1 is later revisited, the single query const is the
  one place to change; `specs/ai.md` carries the rationale.

## Testing Strategy

- **api**: a `*LiveDB` test seeding runs across all statuses and asserting the count
  equals the non-terminal subset (content assertion, not a bare tally); handler test
  for auth + JSON shape. Run via `./e2e/run-store-it.sh` with a positive control.
- **web**: vitest on the badge render/hide/collapse behaviour and mockApi parity.
- **manual**: `VITE_UZI_MOCK=1 npm run dev`, confirm the badge matches the mock.

## Dependencies

None. Reuses `NavItem`/`badge` (PRD #46 M2), the `RequireUser` count-endpoint pattern
(PRD #113 M6, #98, #46), and the existing non-terminal status idiom. No migration, no
new service, no new trust boundary.

## Parallelization

- **Phase 1 (parallel):** M1 (`api`) and M2 (`web`) can start together — M2 works
  against the mock and the agreed `{ count }` contract; only the live wire-up needs
  M1's endpoint. Separate files, separate toolchains.
- **Phase 2 (sequential):** M3 after M1 (CLI reads the endpoint) and M2 (docs describe
  the shipped badge).

| Phase | Milestone | Repo/module | Depends on | Files (primary) |
|---|---|---|---|---|
| 1 | M1 | api | — | `store/queries/runtime.sql`, `handler/`, `handler.go` route |
| 1 | M2 | web | contract only | `lib/api.ts`, `mocks/mockApi.ts`, `components/AppShell.tsx` |
| 2 | M3 | api (cmd/uzi) + docs | M1, M2 | `cmd/uzi/`, `specs/ai.md`, docs |
