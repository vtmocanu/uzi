# PRD #40: Token Usage & Cost Reporting — Per Run, Per User, Factory-Wide

**GitLab Issue**: [vtmocanu/uzi#40](https://gitlab.example.com/vtmocanu/uzi/-/issues/40)
**Status**: Draft — reviewed 2026-07-10 by 2 agents (design review, fact-check); all blocking/major findings folded in below (marked ↳review where the design changed).
**Priority**: Medium
**Created**: 2026-07-10
**Mockup**: [prds/mockups/40-token-usage-mock.html](mockups/40-token-usage-mock.html) — approved by the user; implemented UI must be visually compared against it (M6). Shows all four surfaces: run view usage strip + per-phase table + finish lines, runs list meta line, "Your usage" dashboard card, admin factory total + per-user breakdown.

## Problem

Runs spend the user's own Anthropic tokens, but uzi is almost blind to consumption:

1. The SDK's terminal `result` frame carries full token accounting (`usage`: input/output/cache-read/cache-creation tokens; `modelUsage`: the same per model incl. `costUSD`), and the worker **drops all of it** — only `duration_ms`/`num_turns`/`total_cost_usd` are forwarded (`agent/src/sdk-messages.ts:113-115`).
2. What does survive (a per-phase `$x.yz`) is buried in the activity log finish line (`web/src/components/RunEvent.tsx:142-147`); there is no per-run total anywhere.
3. Failed runs report nothing at all: the error branch of `mapResult` forwards only `subtype` + `errors`, though the SDK error result carries the same `usage`/`total_cost_usd` fields — the runs most worth auditing are the least visible.
4. Nothing is aggregated: a user cannot see what they have consumed in uzi in total, and an admin cannot see what the whole factory (all users, all runs) has consumed.

## Solution Overview

Per the approved mock:

1. **Worker forwards token data**: `mapResult` passes `usage` + `modelUsage` through on both success and error result frames.
2. **API folds per-run usage**: on every result frame delivered to `AppendMessages`, the API upserts a `run_usage` snapshot table (latest-wins, idempotent) — server-side, no worker-protocol change beyond payload content; run/user/factory totals are sums over it.
3. **Run view**: usage strip in the header (tokens in with cache-hit bar, tokens out, duration, cost), collapsible per-phase table (one row per result frame), token counts on the activity-log finish lines.
4. **Runs list**: tokens + cost join each row's meta line; running runs show "so far" figures that update as phases complete.
5. **Dashboard, every user**: "Your usage" card — lifetime + last-7-days totals across their runs.
6. **Dashboard, admin only**: "Factory total" card (all users/all runs) + per-user breakdown table with share bars.

## Design Decisions

1. **Storage is a `run_usage` snapshot table upserted latest-wins — not SUM-columns on `runs`** (↳review: the original additive-fold-into-`runs`-columns design had an unfixable crash window, finding below; this shape follows multica's `task_usage` prior art — `inspiration/multica/server/migrations/032_task_usage.up.sql`, `server/pkg/db/queries/task_usage.sql` — per the inspiration-first convention). Schema: `run_usage(run_id FK→runs ON DELETE CASCADE, session_id text, model text, input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens bigint, cost_usd numeric(12,6), updated_at, PRIMARY KEY (run_id, session_id, model))` — one row per SDK `query()` invocation per model, fed from the frame's `modelUsage` (per-model breakdown persisted for free; UI breakdown stays out of scope).
2. **The fold runs on EVERY delivered result frame, so it is idempotent by construction and has no crash window** (↳review, was the blocking finding). `workersvc.AppendMessages` (`api/internal/workersvc/service.go:593`) defensively parses each *delivered* (not just newly-inserted) `status`/`error` message for `payload.event == "result"` and upserts `run_usage` with `ON CONFLICT (run_id, session_id, model) DO UPDATE SET … = EXCLUDED.…` (latest snapshot wins). Why not fold only newly-inserted frames: the message insert and the usage write are separate statements (the `Store` interface exposes no transaction seam — the same constraint that shaped `CreateStopVerdictInput`, `api/internal/store/queries/runtime.sql:387`), so a crash between them would skip the fold forever on retry (the seq dedup that makes the append idempotent is exactly what would suppress the retry's fold). With upsert-on-every-delivery, an un-acked batch is retried by the worker and the re-delivered frame simply rewrites the same row — at-least-once delivery + idempotent write = correct totals. Malformed/absent usage → skip, never fail the append. No new worker→API endpoint.
3. **Frame semantics (per-invocation vs cumulative-across-resume) are pinned by M1 before M2 builds anything** (↳review: the risk is real — multica found ACP usage frames are *cumulative* snapshots, `inspiration/multica/server/pkg/agent/hermes.go:1191`). Each phase is its own `query()` invocation resuming the prior session id (`driveTurn`, `agent/src/sdk-executor.ts:374-376`), and session ids evolve across turns. M1's experiment must cover BOTH same-process multi-turn resume AND cross-process resume from a persisted session id (the requeue path, `service.go:264-286`). The two possible outcomes map onto the same table with different rollup rules, so the storage shape survives either verdict: (a) *per-invocation deltas* (expected): run total = `SUM` over all rows; (b) *cumulative-across-resume*: rollup key collapses to `(run_id, model)` latest-wins, multica-style. No worker-side delta normalization in either case — the worker forwards raw SDK numbers, and server rollup + client per-phase table consume the same raw frames, so the two surfaces cannot diverge (↳review).
4. **Failed AND cancelled runs are counted** (↳review: cancelled added). The error branch of `mapResult` forwards `usage`/`modelUsage`/`total_cost_usd` too (the SDK's `SDKResultError` carries them), and the API folds `error`-kind result frames the same as `status`-kind ones. `AppendMessages` deliberately keeps no terminal-status guard: a result frame landing after a mid-flight cancel still folds — pre-cancel spend is real spend. A run that burned $3 before dying must show $3.
5. **Per-phase detail comes from the message stream, not new storage.** The run view already replays all messages (REST `?after=<seq>`, no LIMIT — full replay; + ws); the per-phase table and finish lines derive client-side from the result frames in that stream. `run_usage` exists so lists and rollups stay cheap queries, not to feed the phase table.
6. **Rollups are aggregate queries over `run_usage`**: per-run totals join a `SUM … GROUP BY run_id` into the run list/detail queries; `GET /api/usage` (self: lifetime + last-7-days totals, run count) and `GET /api/admin/usage` (factory totals + per-user rows) `SUM` over `run_usage` joined through `runs` to `users`. Pre-feature runs simply have no rows → the UI shows nothing (never a fake 0). At laptop scale this needs no denormalization.
7. **Visibility: self-only for users, everything for admins.** "Your usage" shows only the requesting user's runs. The factory total and per-user breakdown are admin-only (`/api/admin/usage`, admin-gated like the existing admin endpoints). Non-admins never see other users' consumption.
8. **Tokens are the headline, cost is secondary.** The SDK's `total_cost_usd` is an estimate (API-key pricing; subscription-auth users may see $0). The UI leads with token counts (always true) and shows cost alongside when present; a $0.00 cost with nonzero tokens renders as "—".
9. **Live updates ride the existing ws stream.** Usage changes exactly when a result frame is broadcast; the run view folds incoming frames into its local totals, and the runs list refreshes on its existing cadence. No new ws message kinds, no polling.
10. **One shared token formatter.** A `formatTokens` helper (`web/src/lib/`) renders 999→"999", 48_200→"48.2k", 1_280_000→"1.28M" with `tabular-nums`; every surface uses it so figures read identically across the app.

## Technical Design

### agent (worker)

- `agent/src/sdk-messages.ts` `mapResult`: success payload gains `usage` + `modelUsage` (unguarded passthrough, same style as `num_turns` — absent fields vanish in JSON serialization); error payload gains `usage`, `modelUsage`, `total_cost_usd`, `num_turns`, `duration_ms`.
- Tests (`agent/test/`): success and error frames carry usage through; malformed/absent usage yields payloads without the keys; the Decision-2 semantics experiment.

### api

- Migration (draft `00052_run_usage.sql` — **renumber to the live head at merge time** per the repo convention): the `run_usage` table (Decision 1).
- `api/internal/store/queries/`: `UpsertRunUsage` (latest-wins, Decision 2), run list/detail selects extended with `SUM … GROUP BY run_id` totals, `SelfUsage` + `AdminUsage` aggregates; `sqlc generate`.
- `api/internal/workersvc/service.go` `AppendMessages`: for every *delivered* message (including seq-deduped replays), defensively parse result-frame usage and upsert `run_usage` (Decision 2). Unit tests: totals across multi-frame runs, replayed-batch convergence (re-delivery rewrites identical rows — the crash-retry path), error-frame folding, malformed-payload no-op.
- `api/internal/handler/`: run list/detail responses gain the usage fields; new `GET /api/usage` (session-authed) and `GET /api/admin/usage` (admin-gated). Handler tests.

### web

- `web/src/lib/api.ts`: usage fields on `RunListItem`/run detail; `UsageSummary` + admin types; the two new calls.
- `web/src/lib/formatTokens.ts` (+ tests): Decision 10.
- `web/src/pages/RunView.tsx`: header usage strip + collapsible per-phase table (client-derived, Decision 5), live fold-in of ws result frames (Decision 9).
- `web/src/components/RunEvent.tsx` `describeStatus`/finish line: token bits join duration/turns/cost; error result lines show tokens too (Decision 4).
- `web/src/pages/RunsList.tsx`: tokens + cost in the row meta line (hidden when the run has no usage rows, Decision 6).
- `web/src/pages/Dashboard.tsx`: "Your usage" card for everyone; admin-only "Factory total" card + per-user table per the mock.
- Vitest for each surface; existing RunEvent/RunsList/Dashboard tests updated, not deleted.

### e2e

- ↳review: the stub executor (`agent/src/executor.ts`) emits NO result frame today — only `status` text frames; the real result frame exists only on the SDK path. M6 *adds* a synthetic result-frame emission (`event: "result"` with fixed usage numbers) to the stub, then `e2e` asserts the numbers land on the run and in `/api/usage`.

## Milestones

- [ ] **M1 — Worker forwards usage + semantics verdict**: `mapResult` success+error passthrough; the Decision 3 experiment covering BOTH same-process multi-turn resume and cross-process resume from a persisted session id (↳review), recording verdict (a) or (b) in this PRD before M2 starts; agent tests green. Validation: a real run's result frames show `usage` in `run_messages` payloads; the verdict is written down with evidence.
- [ ] **M2 — Persist + fold**: migration (`run_usage`), `UpsertRunUsage`, `AppendMessages` fold-on-every-delivery with replay-convergence/error-frame/malformed tests, rollup rule per M1's verdict, `go test ./...` green. Validation: a two-phase run ends with correct totals; re-delivering the whole batch changes nothing (↳review: this is the crash-retry path, not just a broadcast concern).
- [ ] **M3 — Read APIs**: usage on run list/detail, `GET /api/usage`, `GET /api/admin/usage` (admin-gated), handler tests. Validation: self endpoint sums exactly the caller's runs; admin endpoint matches the sum of per-user rows.
- [ ] **M4 — Run surfaces**: RunView usage strip + per-phase table + live ws fold-in, finish-line tokens (success and error), RunsList meta line, `formatTokens`. Validation: matches the mock's §1–2 for a completed, a failed, and a running run; pre-feature runs show no usage UI.
- [ ] **M5 — Dashboard surfaces**: "Your usage" card, admin "Factory total" + per-user table with share bars; non-admins see no factory data. Validation: matches the mock's §3–4; visibility rules verified from a non-admin session.
- [ ] **M6 — E2E + visual parity + specs**: NEW synthetic result-frame emission in the stub executor (↳review — none exists today) + e2e assertion, web-ux browser pass against the mock (findings fixed), `specs/ai.md` updated, `npm run build` (check-docs) green. The PRD does not close with unresolved visual drift.

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched | Repo area |
|---|---|---|---|---|
| 1 | M1 | — (payload contract fixed in this PRD) | `agent/src/sdk-messages.ts` + tests | agent |
| 2 | M2 | M1's semantics verdict (Decision 3) | migration, store, `workersvc` | api |
| 3 | M3 | M2 | handlers, store queries | api |
| 4 | M4 | M1–M3 | RunView, RunEvent, RunsList, `lib/api.ts`, `lib/formatTokens.ts` | web |
| 5 | M5 | M4 (↳review: shared `lib/api.ts` + `formatTokens.ts` — a real file collision, not "different pages") | Dashboard | web |
| 6 | M6 | all | e2e stub + assertions, specs | cross |

↳review: the original plan ran M1∥M2 and M4∥M5. Both pairs are now sequential — M2 needs M1's cumulative-vs-delta verdict before choosing its rollup rule, and M4/M5 collide on the two shared web lib files.

## Relationship to in-flight PRDs (#37, #38)

This PRD **starts after PRD #37 (per-run agent selection) and PRD #38 (activity feed redesign) land on `main`** — decided with the user 2026-07-10. The collisions are real, not theoretical:

- **#37 ⇄ M2**: #37's M2 edits `api/internal/workersvc/service.go` and adds a `runs` migration (draft `00061`); #40's M2 edits the same service file and adds its own migration (draft `00052`, the `run_usage` table). Draft numbers don't collide, but both get renumbered above the live head at their respective merges — #40's migration lands after #37's.
- **#38 ⇄ M4**: #38 restructures `RunEvent.tsx`/`ActivityFeed.tsx`; its Decision 12 turns status/meta lines into hairline-flanked dividers — exactly the surface #40's finish-line tokens extend. M4 is built against #38's landed feed (the mock already renders the finish line in #38's divider style).
- **No overlap**: #40's M1 (`sdk-messages.ts`) vs #37's M1 (`protocol.ts`); M3/M5/M6 touch files neither PRD changes. PRD #39 (chat agent, also in flight) shares no files with this PRD; its token spend is explicitly out of scope below.

## Review notes (verified non-issues)

- No result-frame path bypasses the fold point: frames reach the DB only via `ctx.emit` → `WorkerRunMessages` → `AppendMessages`; run completion, the sweeper, requeue, and register carry no usage.
- Full-run message replay is unbounded (`ListRunMessagesAfter` has no LIMIT), so the client-derived per-phase table can never be truncated; the LIMIT 200/500 elsewhere are run-*list* caps.
- Resume after requeue does NOT re-persist old frames (new turns get new gapless seqs), so there is no replay-of-old-frames double count; the only semantics risk is Decision 3's cumulative-vs-delta question.
- `/api/usage` and `/api/admin/usage` fit the existing session-authed and `RequireAdmin` route groups respectively; neither path exists today.

## Out of Scope

- Per-model breakdown **UI** (the data is persisted in `run_usage` rows for free; no surface renders it yet).
- Budgets, quotas, alerts, or any enforcement based on usage.
- Backfilling pre-feature runs (their columns stay NULL; the stream data to backfill from mostly doesn't include usage anyway).
- Per-repo rollups and time-series charts (the aggregates make them easy follow-ups).
- Counting tokens spent outside runs (e.g. the PRD #39 chat agent) — that feature should reuse these columns' pattern when it lands.

## Success Criteria

- Every run started after this lands shows tokens in/cached/out + cost on its run view, list row, and finish lines — including failed and cancelled runs.
- A user's dashboard total equals the sum of their runs' totals; the admin factory total equals the sum over all users.
- Worker retries / message replays never double-count (pinned by tests).
- Pre-feature runs render without usage UI — never a fabricated 0.
- Implemented UI passes the M6 side-by-side against the approved mock; `go test ./...`, `npm test`, `npm run typecheck`, agent tests, and e2e all green.
