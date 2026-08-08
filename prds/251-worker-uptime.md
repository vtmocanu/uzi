# PRD #251: Worker uptime — show how long each worker has been online

**GitLab Issue**: [#251](https://gitlab.example.com/vtmocanu/uzi/-/issues/251)
**Status**: Draft (created 2026-08-08)
**Priority**: Low
**Mock**: `prds/mockups/251-worker-uptime-mock.html` (Settings → Workers with the uptime token added; shown to owner 2026-08-08)

> **Working-tree spike (read before starting M2).** An uncommitted `web/` spike already
> exists in the tree, self-labelled `PROTOTYPE (uptime spike)` — the one that produced the
> mock. It has already landed, uncommitted: `Worker.online_since` on the type
> (`web/src/lib/api.ts`), a page-local `formatUptime(sinceIso, nowMs)` + the `· up …` render
> token (`web/src/pages/WorkersSettings.tsx`), and `online_since` on the four `mockWorkers`
> fixtures (`web/src/mocks/data.ts`). It is **not** committed by this PRD (only the PRD +
> mock are), so it is M2's **starting point, not greenfield** — M2 finishes and productionises
> it (see M2). It does **not** touch the server, `RunsList.tsx`, `mockAdminWorkers`, or any
> test.

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
helper takes an ISO string + injectable `nowMs` and lives in `web/src/lib` (the working-tree
spike's page-local `formatUptime` is promoted and renamed as part of M2).

### Decision 5 — Placement: in the faint metadata line, between version and "last seen"

The token joins the existing `template … · v… · last seen …` line as `· up 3h 12m`,
`text-faint`, immediately after the version and before "last seen" (see the mock). It is
not a badge — it is not a state to alarm on — and it inherits the line's existing tone and
sanitisation context (the version/heartbeat spans around it are already faint). No new row,
no new colour.

## Milestones

### M1 — API: `online_since` column, liveness stamping, DTO (`api`)
- [ ] Migration: add `online_since timestamptz` (nullable) to `workers`. Number assigned
  at merge time per the goose convention (next free above the live head). **No backfill
  needed**: the `CASE` below stamps any already-online worker with a NULL anchor on its next
  register/heartbeat, so the column self-heals within one heartbeat (≤15s) rather than
  reading blank for the current fleet.
- [ ] `RegisterWorker` and `HeartbeatWorker` (`api/internal/store/queries/runtime.sql`):
  set `online_since` via the preserve-or-stamp `CASE` (Decision 2). `MarkStaleWorkersOffline`:
  also `SET online_since = NULL`. Regenerate with `sqlc generate` (no-op-clean in CI).
- [ ] `apitypes.Worker`: add `OnlineSince *time.Time json:"online_since"`. Map it in **both**
  DTO builders in `handler/workers.go` (~202, ~246) via the existing `timePtr(...)` helper.
- [ ] **Live-DB test** (`*LiveDB`, run via `./e2e/run-store-it.sh`): a fresh worker gets an
  anchor on first heartbeat; repeated heartbeats **preserve** it; `MarkStaleWorkersOffline`
  **clears** it; a heartbeat after going offline **re-stamps** a later anchor. This is a new
  query behaviour, so per `.claude/rules/go.md` it is not verified until a live-DB test has
  executed it (sqlc-green ≠ runs).
- [ ] Regression test: no claim/scheduling/sweeper query references `online_since` (mirrors
  PRD #49's stats_ display-only guard) — the field is liveness-write / DTO-read only.
- [ ] `task gate:api` green (fmt + vet + build + lint + deadcode + test, `-race`).

### M2 — Web: render uptime (`web`) — *finishes the working-tree spike, not greenfield*
The spike (see the note at the top) already did: `Worker.online_since` on the type, a
page-local `formatUptime` + the `· up …` render token in `WorkersSettings.tsx`, and
`online_since` on the four `mockWorkers` fixtures. M2's remaining deltas:
- [ ] **Promote + rename the helper**: move the page-local `formatUptime` into `web/src/lib`
  as `formatUptimeSince` (Decision 4 — avoid the `BuildInfoPopover` collision), add its unit
  test (`2d 4h` / `1h 23m` / `44m` / `<1m`, invalid → `""`, injectable `nowMs`), and repoint
  `WorkersSettings.tsx` at the import. Confirm the gate stays `w.status === "online" &&
  w.online_since` (Decision 3/5).
- [ ] `RunsList.tsx` admin fleet list: add the same token on the admin worker rows (**not**
  done by the spike).
- [ ] Mock fixtures: add `online_since` to each `mockAdminWorkers` entry too (the spike
  touched only `mockWorkers`) — a value for online rows (varied uptimes), `null` for offline.
- [ ] Tests: `WorkersSettings.test.tsx` asserts the uptime token for an online worker and its
  **absence** for an offline one (paired positive/negative per the copy-change rule in
  `.claude/rules/web.md`). `task gate:web` green.
- [ ] Manual: `VITE_UZI_MOCK=1 npm run dev` — uptime visible on online rows, hidden on the
  offline row, in both `ember` and `mission` themes.

### M3 — CLI, specs, docs
- [ ] `api/cmd/uzi/worker.go`: add a `UPTIME` column to the `uzi workers` table (header +
  cell), `-` when offline/anchorless (mirror `upgradeCell`'s empty→`-` convention). This
  needs a **Go-side** duration formatter (`now − online_since` → human string) net-new in
  `api/cmd/uzi` — the web `formatUptimeSince` is TypeScript and does not carry over. Update
  `worker_test.go`. **CLI check satisfied**: this IS the CLI change the "new functionality ⇒
  check `api/cmd/uzi/`" convention requires.
- [ ] `specs/ai.md`: record `online_since` as an AI design decision (api-owned anchor,
  continuous-online semantics, display-only).
- [ ] Docs: update the workers page only if it enumerates the row's fields; `check-docs`
  stays green. `ARCHITECTURE.md` worker-liveness note if warranted (link this PRD, don't
  duplicate).

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
