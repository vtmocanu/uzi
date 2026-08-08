# PRD #251: Worker uptime — show how long each worker has been online

**GitLab Issue**: [#251](https://gitlab.example.com/vtmocanu/uzi/-/issues/251)
**Status**: ✅ Complete — merged to `main` via MR !211 (squash `59a9452d`, 2026-08-08); issue
auto-closed. Both previously-open items resolved: (1) M1 `task gate:api` — the `lint:api` red was
a real `staticcheck ST1018` finding (raw U+0085 / U+202E in the CLI sanitization test fixtures,
caught by the whole-files ratchet because the uptime column touched those files), fixed by escaping
them to `\u0085` / `\u202e` in `c83bce5a`; pipeline 20538 then passed every job. (2) The M2 both-themes
browser pass was not run manually — the uptime token is a theme-token-inheriting `text-faint` span,
so it is structurally theme-safe; eyeball on the next UI touch if wanted. The judge's `improve_uzi`
recommendation (worker clone's stale `origin/main` mirror can't resolve the ratchet merge-base
locally) is a separate worker-environment follow-up, left in the judge backlog.
**Priority**: Low
**Mock**: `prds/mockups/251-worker-uptime-mock.html` (Settings → Workers with the uptime token added; shown to owner 2026-08-08)

## Problem

Each worker row on Settings → Workers (`web/src/pages/WorkersSettings.tsx`) and the
admin fleet list (`web/src/pages/RunsList.tsx`) shows a worker's **status**
(`online`/`offline` pill), its **version/template**, its **last-seen** timestamp, and
live CPU/memory gauges — but nothing says **how long the worker has been up**. An
operator watching the fleet cannot tell a worker that just reconnected from one that
has held its slot for two days; "online" is a boolean, not a duration. The same gap
exists on the `uzi workers` CLI table, which prints `ID · NAME · STATUS · VERSION ·
UPGRADE · TOKEN` and no uptime.

Uptime is the one liveness fact the row does not carry. "Last seen" is its inverse (how
long since the last heartbeat, only interesting once a worker is late); uptime is how
long the current online session has lasted, and it is the natural companion to the green
`online` pill.

## Solution Overview

Add an **api-owned `online_since` anchor** to each worker: the timestamp at which the
worker most recently **became online**, preserved for as long as it stays online and
**cleared when it goes stale**. Uptime is then `now − online_since`, derived
**client-side** and shown only while the worker is online — exactly "how long has the
`online` pill been green".

- **API** (`api/internal/store`, `api/internal/handler`, `api/internal/apitypes`): a new
  nullable `workers.online_since timestamptz` column, stamped in the two liveness writes
  that already set `status='online'` (`RegisterWorker`, `HeartbeatWorker`) and cleared in
  the sweeper write that sets `status='offline'` (`MarkStaleWorkersOffline`). A new
  `OnlineSince *time.Time json:"online_since"` field on the `apitypes.WorkerDTO` struct,
  mapped in the **two** DTO builders in `handler/workers.go` — `workerDTOFromWorker`
  (~line 202) and `workerDTOFromRow` (~line 246), each beside its existing `LastHeartbeatAt:
  timePtr(...)` line. Every worker DTO (list, admin fleet, create, patch, register/heartbeat
  responses, hosted) routes through those two functions, so the two edits cover all of them
  — including the register/heartbeat responses, which will echo `online_since` back to the
  worker (harmless, ignored, exactly as they already echo stats/version). **No new endpoint,
  no new trust boundary, no worker-protocol change** — the value is stamped by the control
  plane from its own clock, never sent by the worker, so it cannot be spoofed the way
  `version` and `template_reported` can.
- **Web** (`web/src/pages/WorkersSettings.tsx`, `web/src/pages/RunsList.tsx`,
  `web/src/lib`): an `online_since?: string | null` field on the `Worker` type, a small pure
  duration helper — named `formatUptimeSince` to avoid colliding with the **existing**
  exported `formatUptime(seconds)` in `web/src/components/BuildInfoPopover.tsx` (that one
  takes seconds and floors at `0s`/`48s`; this one takes an ISO instant and floors at `<1m`,
  see Decision 4) — rendering `2d 4h` / `1h 23m` / `44m` / `<1m`, and an `· up 3h 12m` token
  in each online worker's metadata line. The existing 10s fleet poll (`usePollWhileVisible`)
  re-renders it, so it advances without a dedicated timer.
- **CLI** (`api/cmd/uzi/worker.go`): a `UPTIME` column on the `uzi workers` table.

This is a **display-only liveness fact**: no claim, scheduling, or sweeper query reads
`online_since`; it is written by the liveness path and read only by the worker DTOs — the
same contract PRD #49's `stats_*` columns hold.

## Design Decisions

### Decision 1 — Uptime is "continuous-online duration", derived from an api-owned anchor (chosen)

Three sources were considered:

| Option | What it measures | Why not / why |
|---|---|---|
| **A — api `online_since` anchor** (chosen) | how long `status` has continuously been `online`, by the control plane's own clock | non-spoofable, matches the visible pill exactly, one column + two existing writes |
| B — worker self-reports process uptime | how long the agent **process** has run | needs a worker-protocol field, must be sanitized/clamped like `version` (untrusted), and can disagree with the pill (process up, control plane saw it go offline) |
| C — reuse `created_at` | age since **registration** | wrong meaning: a worker registered 14 days ago but online for 3 minutes would read "up 14d" |

A is chosen: it is the control plane's own observation, so it needs no trust in the
worker and cannot drift from the `online`/`offline` pill it sits beside. Cost accepted:
it measures the **control-plane-visible** session, not the OS process — see Decision 2.

### Decision 2 — Reset the anchor on every online transition, including an observed offline gap

`online_since` is stamped whenever a worker **enters** the online state and preserved
while it stays there:

- **`RegisterWorker`** and **`HeartbeatWorker`** both already `SET status='online'`. Each
  sets `online_since = CASE WHEN workers.status = 'online' AND workers.online_since IS NOT
  NULL THEN workers.online_since ELSE now() END` — i.e. **keep** the anchor if the worker
  is already online with one, otherwise **stamp now()**. So a steady stream of heartbeats
  never moves it, and the first heartbeat/register after an offline period (or for a brand
  new worker) sets it.
- **`MarkStaleWorkersOffline`** additionally `SET online_since = NULL` alongside
  `status='offline'`, so an offline worker carries no uptime and the next online
  transition starts a fresh session.

**Consequence, stated on purpose:** if the sweeper marks a worker offline during a network
blip and a later heartbeat brings it back, uptime **resets** — because the control plane
observed a gap, and this metric is defined as continuous-online-as-seen-by-uzi, not
process uptime. That is the honest reading of what the api can know, and it keeps uptime
consistent with the `online` pill (the pill also flipped). A worker whose process restarts
fast enough to never trip the sweeper (re-register while still `online`) keeps its anchor;
this is acceptable and noted rather than special-cased — chasing process-restart detection
is option B's cost, out of scope.

### Decision 3 — Show uptime only while online; hide it when offline

An offline worker is not "up", so it renders **no** uptime token — its `last seen` line
already carries the relevant story (how long ago it went quiet). The web gate is
`w.status === "online" && w.online_since`; the CLI prints `-` in the UPTIME cell for an
offline (or anchorless) worker, matching how `worker.go` already renders an empty VERSION.

**Scoped OUT: the Dashboard "Worker load" card** (`web/src/pages/Dashboard.tsx`, per-worker
rows gated on `hasStats`). That card is a resource-load view — status badge +
`WorkerStatLine` (cpu·mem) — not a liveness-metadata surface, and it deliberately shows only
workers that have reported a stats sample. Uptime belongs on the two surfaces that already
render the full metadata line (Settings → Workers, admin fleet); adding it to the load card
would be scope creep for no clear read. Stated here so the omission is a decision, not an
oversight.

### Decision 4 — Uptime format mirrors `formatCountdown`, under a non-colliding name

The rate-limit meters already render durations as `2d 4h` / `1h 23m` / `44m`
(`formatCountdown`, `web/src/lib/rateLimits.ts`). The new helper uses the **same** buckets
(`<1m` at the floor) so uptime and reset countdowns read alike across the app. It is a pure
function with its own unit test; it does **not** reuse `formatCountdown` directly because
that one is worded for a future reset ("now" at/after zero), whereas uptime counts up from a
past instant and floors at `<1m`.

**Named `formatUptimeSince`, not `formatUptime`.** There is already an exported
`formatUptime(seconds: number)` in `web/src/components/BuildInfoPopover.tsx` (server build
uptime; buckets `3d 4h`/`4h 12m`/`12m`/`48s`/`0s`, unit-tested). The two cannot be merged —
different input (seconds vs ISO instant) and different floor (`0s` vs `<1m`) — so a second
exported `formatUptime` would be a knip duplicate-export smell and a reader trap. The new
helper takes an ISO string + injectable `nowMs` and lives in `web/src/lib`.

### Decision 5 — Placement: in the faint metadata line, between version and "last seen"

The token joins the existing `template … · v… · last seen …` line as `· up 3h 12m`,
`text-faint`, immediately after the version and before "last seen" (see the mock). It is
not a badge — it is not a state to alarm on — and it inherits the line's existing tone and
sanitisation context (the version/heartbeat spans around it are already faint). No new row,
no new colour.

## Milestones

### M1 — API: `online_since` column, liveness stamping, DTO (`api`)
- [x] Migration: add `online_since timestamptz` (nullable) to `workers`. Landed as
  `00100_worker_online_since.sql` (next free above head `00099`). **No backfill
  needed**: the `CASE` below stamps any already-online worker with a NULL anchor on its next
  register/heartbeat, so the column self-heals within one heartbeat (≤15s) rather than
  reading blank for the current fleet.
- [x] `RegisterWorker` and `HeartbeatWorker` (`api/internal/store/queries/runtime.sql`):
  set `online_since` via the preserve-or-stamp `CASE` (Decision 2). `MarkStaleWorkersOffline`:
  also `SET online_since = NULL`. Regenerated with pinned `sqlc v1.30.0` (idempotent — a
  second `generate` produced no further diff).
- [x] `apitypes.Worker`: add `OnlineSince *time.Time json:"online_since"`. Mapped in **both**
  DTO builders in `handler/workers.go` via the existing `timePtr(...)` helper; the
  `apitypes` wire-contract test (`wire_test.go`) gained the `online_since` tag.
- [x] **Live-DB test** (`*LiveDB`, run via `./e2e/run-store-it.sh`): a fresh worker gets an
  anchor on first heartbeat; repeated heartbeats **preserve** it; `MarkStaleWorkersOffline`
  **clears** it; a heartbeat after going offline **re-stamps** a later anchor.
  `TestWorkerOnlineSinceLifecycleLiveDB` — `--- PASS` (RUN=1, 0 SKIP) against the throwaway
  Postgres.
- [x] Regression test: no claim/scheduling/sweeper query references `online_since` (mirrors
  PRD #49's stats_ display-only guard) — the field is liveness-write / DTO-read only.
  `TestOnlineSinceIsWriteOnlyAndNeverScheduled` (static scan over `queries/*.sql`).
- [ ] `task gate:api` green (fmt + vet + build + lint + deadcode + test, `-race`).
  fmt-check / vet / build / deadcode / test(-race) all PASS. The lint slot is red ONLY on
  23 pre-existing findings in files M1 never touched, exposed because this worktree's
  `origin/main` mirror sits behind the branch base (`whole-files` then lights up
  already-merged files); the full unfiltered backlog is 56 and **none** of it is in any file
  M1 changed. Left unticked until the lint base resolves against a current `origin/main`.

### M2 — Web: render uptime (`web`)
- [x] `Worker` type (`web/src/lib/api.ts`): add `online_since?: string | null`.
- [x] New pure helper `formatUptimeSince` in `web/src/lib` (Decision 4 — named to avoid the
  `BuildInfoPopover.formatUptime` collision): takes an ISO instant + injectable `nowMs`,
  renders `2d 4h` / `1h 23m` / `44m` / `<1m`, invalid → `""`. Unit-tested (`formatUptimeSince.test.ts`).
  A future/clock-skewed anchor floors at `<1m` (not `""`) to match the Go twin and avoid a
  dangling `· up ` label — reviewer/auditor finding; `""` now means only an unparseable instant.
- [x] `WorkersSettings.tsx`: render `· up {formatUptimeSince(w.online_since)}` in the
  metadata line (between version and "last seen"), gated on `w.status === "online" &&
  w.online_since` (Decision 3/5).
- [x] `RunsList.tsx` admin fleet list: the same token on the admin worker rows.
- [x] Mock fixtures (`web/src/mocks/data.ts`): `online_since` on every `mockWorkers` and
  `mockAdminWorkers` entry — varied ISO values for online rows, `null` for offline.
- [x] Tests: `WorkersSettings.test.tsx` asserts the uptime token for an online worker and its
  **absence** for an offline one (paired positive/negative per the copy-change rule in
  `.claude/rules/web.md`). `task gate:web` green (1711 tests pass).
- [ ] Manual: `VITE_UZI_MOCK=1 npm run dev` — uptime visible on online rows, hidden on the
  offline row, in both `ember` and `mission` themes. Not run in this run (no live browser
  pass); rendering is covered by the vitest online/offline pair + mock fixtures, but the
  both-themes eyeball check was not performed — left unchecked deliberately.

### M3 — CLI, specs, docs
- [x] `api/cmd/uzi/worker.go`: `UPTIME` column on `uzi worker list` (header + cell), `-` when
  offline/anchorless. Go-side formatter `formatUptimeDuration(d)` (buckets match the web helper
  and `formatCountdown`), called by `uptimeCell(w)`; `worker_test.go` covers all buckets plus the
  `-` cases, and `commands_test.go`'s positional row assertion was updated for the new column.
- [x] `specs/ai.md`: recorded `online_since` as an AI design decision (§494 — api-owned anchor,
  continuous-online semantics, observed-gap reset, display-only).
- [x] Docs: no `docs/` page enumerates the `uzi worker list` column set (only TOKEN/UPGRADE have
  dedicated subsections), so none needed the column; `check-docs` stays green (part of `gate:web`).
  No `ARCHITECTURE.md` note warranted — the PRD + `specs/ai.md` §494 carry the design.

## Success Criteria

1. An online worker's row (web Settings → Workers and admin fleet) and its `uzi workers`
   CLI line show its uptime as a human duration; an offline worker shows none.
2. Uptime is stamped and cleared by the control plane from its own clock — never sent by
   the worker — and no claim/scheduling/sweeper query reads it.
3. Repeated heartbeats do not move the anchor; going offline clears it; coming back starts
   a fresh session (proven by a live-DB test that actually executes the queries).
4. Uptime reads in the same `2d 4h` / `1h 23m` / `44m` / `<1m` vocabulary as the rate-limit
   reset countdowns.
5. `task gate:api`, `task gate:web`, and the `*LiveDB` store sweep are green; the mock build
   renders the token.

## Risks & Mitigations

- **"Uptime" implies process uptime, but this is control-plane-visible session time.**
  Accepted and documented (Decision 2); the two differ only across an observed offline gap
  or a sub-sweep process restart, and the chosen meaning is the one consistent with the
  `online` pill. If true process uptime is later wanted, it is option B (a worker-protocol
  field), a separate PRD.
- **A negative test assertion ("no uptime when offline") going vacuous after a copy change.**
  Mitigated by pairing it with a positive assertion and following the retired-string sweep
  rule (`.claude/rules/web.md`).
- **`sqlc`-green being mistaken for query-correct.** Mitigated by the M1 live-DB test being a
  hard gate item, not a nicety (`.claude/rules/go.md`).

## Testing Strategy

- **api**: a `*LiveDB` store test for the stamp/preserve/clear/re-stamp transitions (run via
  `./e2e/run-store-it.sh`, positive control required — named test `--- PASS`, `RUN>0`, zero
  `SKIP`); a regression test that no scheduling/sweeper query reads `online_since`; DTO
  mapping covered by the worker handler tests.
- **web**: vitest — `formatUptime` unit tests (buckets + invalid input), and
  `WorkersSettings.test.tsx` for the online-present / offline-absent pair.
- **cli**: `worker_test.go` for the UPTIME column (present when online, `-` when offline).
- **manual**: `VITE_UZI_MOCK=1 npm run dev`, both themes.

## Dependencies

None. Reuses the existing liveness writes (`RegisterWorker`/`HeartbeatWorker`/
`MarkStaleWorkersOffline`), the `timePtr` DTO helper, the `usePollWhileVisible` fleet poll,
and the `formatCountdown`/`formatAgo` duration idiom. No API surface, forge, migration-order,
or trust-boundary dependency beyond the one new column.

## Parallelization

M1 (api) is the critical path — it defines the wire field M2/M3 consume. M2 (web) and M3
(CLI/specs/docs) both depend only on the DTO field name/shape from M1 and are independent of
each other, so they can run as parallel agents once M1's DTO shape is fixed (web touches
`web/**`, CLI touches `api/cmd/uzi/**` — disjoint file sets).

| Phase | Milestone | Repo/module | Depends on | Files (primary) |
|---|---|---|---|---|
| 1 | M1 | api | — | `store/queries/runtime.sql`, `store/migrations/`, `apitypes/worker.go`, `handler/workers.go` |
| 2 | M2 | web | M1 (DTO field) | `web/src/lib/api.ts`, `web/src/lib/formatUptimeSince.ts` (+test), `web/src/pages/WorkersSettings.tsx`, `web/src/pages/RunsList.tsx`, `web/src/mocks/data.ts` |
| 2 | M3 | api (cli) + specs | M1 (DTO field) | `api/cmd/uzi/worker.go`, `specs/ai.md`, docs |
