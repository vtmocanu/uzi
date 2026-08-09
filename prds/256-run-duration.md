# PRD #256: Run duration on runs — Runs page + board + CLI

**GitLab Issue**: [#256](https://gitlab.example.com/vtmocanu/uzi/-/issues/256)
**Status**: 📝 Draft — architect-reviewed 2026-08-08 (findings F1–F6 incorporated); ready for implementation
**Priority**: High — top priority, do next (owner, 2026-08-09). Deferred only to avoid a Board.tsx collision with the in-flight #196; send the full gated PRD once #196 lands.
**Mock**: `prds/mockups/256-run-duration-mock.html` (the Runs page with the duration token added; shown to owner 2026-08-08)

## Problem

The Runs page (`web/src/pages/RunsList.tsx`) lists each run with its issue title, the
repo/worker, and a wall-clock timestamp — `new Date(run.updated_at).toLocaleString()`,
the `08/08/2026, 17:39:36` in the owner's screenshot — but **not how long the run has
been going**. To learn that an active run has been working for 90 minutes, or that one
has been parked awaiting your approval for half an hour, you have to open it. "Open one
to watch it live" is the page's own subtitle; the list itself cannot answer "how long?"
at a glance.

The board (`web/src/pages/Board.tsx`) is only half-covered: a **running** card's badge
already shows `running {formatElapsed(now − created_at)}` (`web/src/lib/runBadge.ts:289`),
but no other state does — a queued, awaiting-approval, needs-answer, or limit-wait card
shows no elapsed at all.

Separately, and surfaced while investigating the above: inside a run,
`formatDuration` (`web/src/components/RunEvent.tsx:409`) is the one duration formatter in
the app that **never rolls over to hours**. It powers the run header/terminal line
(`RunView.tsx:97`), the terminal total (`RunView.tsx:425`), the Duration stat
(`RunUsage.tsx:163`), and the run-history rows (`IssueView.tsx:32`), so a 90-minute run
reads `90m 00s` and a 2-hour run (the `RUN_TIMEOUT` wall-clock cap) reads `120m 00s`. It
also has a `Math.round` seconds-carry that can render `1m 60s`. Every other formatter
(`formatElapsed`, `formatUptimeSince`, the rate-limit and pipeline helpers) already rolls
to `1h 30m` using `Math.floor`.

## Solution Overview

Show a run's duration as a small faint mono token, derived **client-side** from the
timestamps the run rows already carry — no API, DTO, or schema change. This mirrors the
just-shipped worker-uptime token (`up 3h 12m`, PRD #251), which is the visual and
architectural precedent throughout.

- **Shared helper** (`web/src/lib/runDuration.ts`, new): a pure
  `runDurationLabel(run, nowMs)` returning `"running 1h 30m"` / `"queued 4m"` /
  `"waiting 34m"` / `"ran 42m"` / `""`. It reuses the board's existing `formatElapsed`
  (`runBadge.ts:136`, `Math.floor`-based, rolls to hours) so the two surfaces read the
  same, and takes `nowMs` as a parameter (never `Date.now()` internally) so it is
  deterministic under test — the `runBadge`/`formatUptimeSince` discipline. Its input
  type makes `started_at`/`finished_at`/`claimed_at` **optional**, so both the full
  `Run` (Runs page) and the narrow board `LatestRun` satisfy it — see Decision 6.
- **Runs page** (`RunsList.tsx`): the token joins each row's meta line, **kept
  alongside** the existing timestamp (owner decision 2026-08-08). One `RunRow` serves the
  owner's Active/Past lists **and** the admin "Active runs · all users" list
  (`RunsList.tsx:200`, `:224`, `:247`), so the single edit covers all three. Reuse the
  existing `useNow` (`web/src/lib/rateLimits.ts:319`) at a 1s tick to keep active
  durations advancing (the page loads once and does not otherwise refresh).
- **Board** (`Board.tsx` + `runBadge.ts`): the same token on every card via the same
  helper, and the running badge's inline elapsed moves to that uniform token (Decision 4).
  The board's run projection is narrow (created/updated only), so this is a **degraded,
  still-web-only** variant by owner decision — see Decision 6.
- **CLI** (`api/cmd/uzi/run.go`): an `AGE` column on `uzi run list`, per the repo's "new
  functionality ⇒ check the CLI" convention. Reuses the existing Go duration twin
  `formatUptimeDuration`/`uptimeCell` (`api/cmd/uzi/worker.go`) rather than a fresh
  formatter. Display-only — the list DTO it prints already carries the timestamps.
- **Format fix** (`RunEvent.tsx`): `formatDuration` gains an hour tier (`1h 30m 00s`),
  fixing the `90m 00s` / `120m 00s` bug, and its `Math.round` seconds-carry (`1m 60s`) is
  closed at the same time.

This is a **display-only** change: no run-lifecycle query, sweeper, or claim path reads
these values; they are derived at render from timestamps already on the wire.

## Design Decisions

### Decision 1 — Client-derived from existing timestamps, no API change (chosen)

`RunListItemDTO` embeds `RunDTO`, which already carries `claimed_at`, `started_at`,
`finished_at`, `created_at`, `updated_at` (`api/internal/apitypes/run.go:125-129`); the
server sends the whole row via `SELECT sqlc.embed(r)` in `ListRunsForUser`
(`api/internal/store/queries/runtime.sql:397`); and web's `RunListItem extends Run`
(`web/src/lib/api.ts:1073`, fields at `:972-976`). So every timestamp needed for the
**Runs page and the CLI** is already on the wire. Rejected: an api-computed
`duration_seconds` field — it would go stale between polls, needs a DTO change, and buys
nothing a client clock cannot do for a display value.

### Decision 2 — Per-state anchor and verb (the Runs page / full-`Run` table)

| Status | Anchor | Token |
|---|---|---|
| `queued` | `created_at` | `queued 4m` |
| `claimed` | `claimed_at` (→ `created_at`) | `claimed 20s` |
| `running` | `started_at` (→ `created_at`) | `running 1h 30m` |
| `awaiting_approval` / `awaiting_input` / `limit_wait` | `updated_at` | `waiting 34m` |
| `completed` / `failed` / `cancelled` | `finished_at − started_at` (static) | `ran 42m` |

Returns `""` when the anchor is missing (a pre-feature or not-yet-started run) — never a
fabricated `0`, matching the row's existing token/cost convention. A run cancelled/failed
before it started has `started_at = null`, so its `ran` branch yields `""`; that is
correct, and M2 tests it. Clock skew / future anchors floor to `0s` via `formatElapsed`'s
`Math.max(0, …)` (`runBadge.ts:137`).

**The `waiting` anchor is `updated_at`, and this is the one soft spot** (flagged in
review): a parked run has no dedicated "entered this state at" timestamp on the row, and
`updated_at` — the time of the last state change — is the closest honest proxy for "how
long it has been parked". Contrary to an earlier draft, **no heartbeat under-counts it**:
`SetRunRunning` is status-guarded and skips `limit_wait` / un-consumed `awaiting_approval`
(`runtime.sql:745-761`), `UpdateRunLastSeq` bumps `last_activity_at` not `updated_at`
(`runtime.sql:1193`), and `SetRunHealth` deliberately avoids `updated_at`
(`runtime.sql:1862-1864`). The **real** mid-park bump vectors, all user-initiated and
low-impact, are `SetRunWaitOnLimit` (wait-on-limit toggle, covers all three parked
states, `runtime.sql:1969`), `SetRunMRState` (no MR exists pre-implement, so it won't
fire for `awaiting_approval`), and the board column-move writes
(`RecordRunColumnMove`/`ClearRunMovePending`/`ClearIssueRunsMovePending`,
`runtime.sql:1795-1818`) — none status-guarded, so a card drag resets "waiting Xm" to 0.
Acceptable to ship and watch; the escalation, if it ever bites, is a `state_since` column
stamped on the transition, deliberately **out of scope** here (a schema change for a
display value).

### Decision 3 — Live tick on the Runs page (chosen), board rides its existing poll

The Runs page loads once (`RunsList.tsx:161-163`, `useEffect(load)`, no interval), so
without a tick the duration would freeze between navigations. Reusing `useNow(1000)`
(`rateLimits.ts:319`) re-renders the rows so active durations advance. **Known side
effect, accepted as positive:** the existing worker-uptime token (`up …`) on the same
page begins advancing too (it is static today). The board already polls every 10s
(`usePollWhileVisible`), which re-renders it, so it needs no new timer — its sub-10s
staleness matches how the running badge already behaves. Rejected: static-until-reload on
the Runs page — a duration that never moves reads as broken.

### Decision 4 — One placement on the board: the meta-line token, not the badge

Today the board shows running elapsed **inside** the status badge (`running 1h 30m`,
`runBadge.ts:289`). Adding the uniform token would double it for running cards, so the
elapsed moves out of the badge (badge → `running`) into the meta-line token for every
state. **This is both a placement AND an anchor change and both must be stated:** on the
Runs page `running` counts from `started_at` (Decision 2), but on the board it stays
`created_at` (the board projection has no `started_at` — Decision 6), so a running card
and its Runs-page row may show slightly different elapsed (the board includes queue time,
as it already does today). Accepted. `runBadge.test.ts` assertions (`:65-68`, `:124`,
`:248`) that expect `"running 4m"` in the badge change to expect a bare `"running"` badge
plus the token. Rejected alternative (b): keep elapsed in the running badge and add the
token only for non-running states — no shipped-behavior change, but placement is
inconsistent, which defeats the point.

### Decision 5 — The `formatDuration` fix ships in this PRD, not deferred

Same subject (run duration, shown honestly), small and self-contained, with an existing
test to extend (`RunEvent.test.tsx:73-80`). Bundling it keeps "durations roll over to
hours" a single verifiable property across the list, the board, and the run detail.

### Decision 6 — The board is web-only and DEGRADED, by owner decision (chosen)

The board card's run summary is a deliberately narrow projection — `LatestRun` carries
only `created_at` / `updated_at`, **not** `started_at` / `finished_at` / `claimed_at`
(`api/internal/handler/board.go:135-136`; web mirror `api.ts:382-383`;
`mapLatestRun` never selects them, `board.go:146`). Rather than widen the board DTO +
query + web type (which would drop the "web-only" framing and add a Go milestone), the
owner chose (2026-08-08) to keep the board **web-only and accept a degraded token**:

| Board card | Anchor available | Token |
|---|---|---|
| `queued` | `created_at` | `queued 4m` |
| `claimed` | `created_at` (no `claimed_at`) | `claimed …` |
| `running` | `created_at` (no `started_at`; = today's badge) | `running 1h 30m` |
| `awaiting_*` / `limit_wait` | `updated_at` | `waiting 34m` |
| `completed` / `failed` / `cancelled` | none (no `finished_at`/`started_at`) | *(no token, as today)* |

Crucially this needs **no board-specific code**: the same `runDurationLabel` fed a
`LatestRun` (whose optional `started_at`/`finished_at`/`claimed_at` are simply
`undefined`) falls back to `created_at` for running/claimed and returns `""` for terminal
— the degradation is a property of the helper's fallbacks, not a second code path. If
"ran 42m" on closed board cards is later wanted, the follow-up is widening `latestRunDTO`
+ `ListLatestRunsForRepoRow` + web `LatestRun` (+ `board_latestrun_test.go`).

## Milestones

**Phase 1 — parallel (separate files/modules, no shared edits):**

- [ ] **M1 — Format fix.** `formatDuration` (`RunEvent.tsx`) rolls to hours
  (`1h 30m 00s`) and closes the `1m 60s` seconds-carry; extend `RunEvent.test.tsx`
  (90-min, 2-hour, just-under-hour, and a `59.6s`-style carry case); update the helper's
  doc comment (its examples stop before an hour, which is what let the bug stand). Verify
  the fix reaches all callers (`RunView.tsx:97`, `:425`, `RunUsage.tsx:163`,
  `IssueView.tsx:32`, `RunEvent.tsx:446`/`:643`/`:824`). Fixes the `90m` report.
- [ ] **M2 — Shared helper.** `web/src/lib/runDuration.ts` + `runDuration.test.ts`:
  `runDurationLabel(run, nowMs)` per Decision 2, reusing `formatElapsed`; input type has
  optional `started_at`/`finished_at`/`claimed_at` (Decision 6). Tests cover every state,
  the null-`started_at` terminal → `""` case, and a future-anchor → `0s` case.
  Foundation for M3 + M4.
- [ ] **M5 — CLI column.** `uzi run list` gains an `AGE` column (`api/cmd/uzi/run.go`),
  reusing `formatUptimeDuration`/`uptimeCell` (`worker.go`); test in the run-cmd test.
  Independent Go module.

**Phase 2 — after M2 (parallel to each other, file-disjoint):**

- [ ] **M3 — Runs page.** Duration token on each `RunRow` meta line, kept alongside the
  timestamp (owner decision), live via reused `useNow(1000)`; covers owner Active/Past +
  admin lists. Tests (`RunsList.test.tsx`): token present + advancing for an active run,
  `ran …` for a terminal one.
- [ ] **M4 — Board (degraded, Decision 6).** Uniform duration token across card states
  via the shared helper; move running elapsed out of the badge into the token
  (Decision 4). Update `runBadge.test.ts` badge assertions; add `Board.test.tsx` coverage.
  Edits `runBadge.ts` (M2 only imports `formatElapsed` from it, no edit → no conflict).

**Phase 3 — sequential, last:**

- [ ] **M6 — Docs, mock, gates.** Commit `prds/mockups/256-run-duration-mock.html`; note
  the decision in `specs/ai.md`; `task gate:web` + `task gate:api` green; a both-themes
  browser pass of the Runs page (the token is a theme-token-inheriting `text-faint` span,
  structurally theme-safe, but confirm once).

Dependency graph: M1 ∥ M2 ∥ M5 → (M3 ∥ M4) → M6. M1 and M5 have no dependants. M4 edits
`runBadge.ts`; M2 only imports from it, so no conflict; `formatElapsed` stays exported
(still used by `healthBadge`, `runBadge.ts:217`), so no dead-export/knip issue.

## Success Criteria

1. On the Runs page, every active row shows a live-advancing duration
   (`running`/`queued`/`waiting`) beside its timestamp, and every terminal row shows a
   static `ran …`; the admin all-users list shows it too.
2. On the board, active cards (queued/running/waiting) show a duration in one consistent
   place; terminal cards are unchanged (Decision 6). Running cards no longer carry elapsed
   inside the badge.
3. `uzi run list` prints an `AGE` column.
4. A 90-minute span renders `1h 30m` on the list and board, and `formatDuration` renders
   `1h 30m 00s` in the run header / terminal total / Duration stat / history rows — never
   `90m`, `120m 00s`, or `1m 60s`.
5. No API, DTO, or schema change; `task gate:web` and `task gate:api` green.

## Risks

- **`waiting` anchor accuracy** (Decision 2) — `updated_at` resets on a wait-on-limit
  toggle or a card drag. Low-impact and user-initiated; ship and watch, `state_since` is
  the out-of-scope escalation.
- **Board/list running-anchor mismatch** (Decision 4/6) — accepted: board counts running
  from `created_at` (incl. queue, as today), the list from `started_at`. A widened board
  DTO is the follow-up if parity is wanted.
- **Board running-badge change** (Decision 4) — visible change to a shipped surface;
  contained to `runBadge.ts` + its tests, and it is the optional board track.

## Decision Log

- 2026-08-08 — Owner: show duration **alongside** the existing timestamp on the Runs
  page, not replacing it.
- 2026-08-08 — Owner: include the board track (#3), **web-only degraded** variant
  (Decision 6) after the `LatestRun`-projection constraint was surfaced in review.
- 2026-08-08 — `formatDuration` `90m`/`120m`/`1m 60s` bug confirmed by reading the code;
  folded into this PRD (Decision 5).
- 2026-08-08 — Architect review (issue #256): findings F1 (board `LatestRun` projection →
  Decision 6), F2 (waiting under-count vectors corrected in Decision 2), F3 (Decision 4
  anchor+placement split stated), F4 (CLI is `uzi run list`, reuse
  `formatUptimeDuration`), F5 (`RunView.tsx:425` added to M1), F6 (null-`started_at` /
  clock-skew tests added to M2) — all incorporated.
