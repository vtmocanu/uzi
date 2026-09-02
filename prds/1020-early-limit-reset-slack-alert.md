# PRD #1020 — Slack DM alert on early Anthropic 7-day rate-limit reset

**Issue**: [#1020](https://github.com/vtmocanu/uzi/issues/1020)
**Status**: Draft (held — see Dependencies)
**Priority**: Medium
**Standalone feature.** NOT an epic #915 refactor child; it adds behavior, it does not preserve it.

## Problem

Anthropic accounts carry a 7-day (weekly) usage limit alongside the 5-hour rolling one. When that weekly window is exhausted, uzi parks `wait_on_limit` runs until it reopens. Sometimes the weekly limit resets *earlier* than the reset time Anthropic advertised (observed on a full account, 2026-09-01). uzi already knows the account's weekly reset time (it is polled and stored), but it does nothing with an early reset: the user is not told, so they keep waiting and parked runs sit idle until the nominal reset even though the account could work sooner.

## Solution

A per-user setting (default **on**) that fires a loud Slack **DM to the user** the moment uzi's usage poller observes the account's 7-day window reset **at least 8h before** its previously-recorded expected reset. It reuses two existing subsystems and adds no new service, port, or trust boundary:

- **Detection** rides the existing per-token usage poller (PRD #53, `api/internal/usagepoller/engine.go`) and the `anthropic_rate_limits` gauge (`seven_day_pct`, `seven_day_resets_at`). Each tick already fetches the account's fresh weekly window; we compare it to the stored one.
- **Delivery** rides the existing notifications seam: `notifysvc.Service.Notify(...)` → slacksvc `handleNotify` (`api/internal/slacksvc/notifier.go:280`), which opens a DM to a linked user and **silently drops** users with no linked / confirmed Slack. "Inert without Slack" is therefore already the built-in behavior.

## Scope

### In scope
- One per-user boolean setting, default on: notify me when my 7-day limit resets early.
- Early-reset detection in the usage poller, threshold a hardcoded `8h` constant.
- A loud, warning-styled Slack DM (Block Kit): `🚨 7-DAY RATE LIMIT RESET EARLY`, reset ~Xh early, observed at `HH:MM` vs expected `HH:MM`.
- Delivery through the standard persist-first notifications seam (`notifysvc.Notify`): a durable inbox notification **and** the Slack DM (the DM is the loud surface). This pulls in a web inbox renderer for the new kind (M5).
- Toggle surfaces: HTTP route, web toggle, CLI (mirroring the existing `wait_on_limit` setting), each gated on the corresponding in-flight #915 child (see Dependencies).

### Out of scope
- The 5-hour window. Feature is weekly-only (the user's stated ask). The predicate reads only the `seven_day_*` gauge.
- Resuming parked runs early / re-probing to *cause* an early reset. This only observes and notifies; the existing park/sweep promotion is unchanged. (A future PRD may consume the same detection signal to promote parked runs; explicitly not this one.)
- A configurable threshold. `8h` ships as a named constant; make it a knob only if asked.
- Email, and any *new* delivery channel. A Slack-only variant that bypasses the inbox row was considered and rejected (D3): riding `notifysvc.Notify` is the idiomatic seam and the inbox row is a free durable record.

## Success criteria

1. With the setting on and Slack linked, when the poller observes the account's `seven_day` window move to a new window (`next.seven_day_resets_at > prev.seven_day_resets_at`) while poll time is `< prev.seven_day_resets_at − 8h`, the user gets exactly **one** Slack DM (plus the inbox row) naming expected vs observed reset and the hours saved.
2. A reset at or near the expected time fires **nothing**. The false-positive guard is mutation-checkable: the silent test fixture sits *modestly* inside the window (e.g. ~6h early — silent under `8h`, but would fire under a `0h` threshold), and a boundary fixture pins the `<` vs `<=` comparator. A test whose silent case is exactly on-time (earliness ≈ 0) does NOT kill the `8h→0` mutation and is insufficient.
3. With the setting off, nothing fires (no DM, no inbox row) even when an early reset is detected.
4. With no linked Slack, detection still runs; the inbox notification is still written and the Slack DM is silently dropped (no error) — inherited from `handleNotify`.
5. The alert fires **once** per early-reset event, not once per subsequent poll tick. Ordering is upsert-then-notify → at-most-once (a crash between upsert and notify drops the alert rather than duplicating it; acceptable for an advisory alert — see D7).
6. `task gate` green across api / web / agent (whichever a milestone touches); new tests mutation-checked (fail on the unfixed code, pass with the fix), per the repo's regression-test convention.

## Technical design

### Data / baseline (already present)
- `anthropic_rate_limits` gauge, keyed per **token** (`user_secret_id`), carries `seven_day_pct` (nullable `Int2`), `seven_day_resets_at`, `five_hour_*`, `source` (`limit_report` | poller sync), `synced_at` (`api/internal/store/anthropic_rate_limits.sql.go`). **Staleness is computed** from `synced_at` (3× the poll interval, D3), not a stored `stale` column.
- The gauge stores exactly **one** weekly window. The `seven_day_opus/sonnet/overage_included` spellings in `workersvc.rateLimitTypes` are alternate *names* for the same window — `limitWindowFor` collapses all four to the one `seven_day_*` column pair (`limitwait.go:442-472`). So "single weekly gauge" is correct.
- Weekly exhaustion is recorded by `MarkSevenDayExhausted(user_secret_id)` (sets `seven_day_pct=100`, `source='limit_report'`, stamps `seven_day_resets_at`) from the run-park path (`limitwait.go:619`), and/or by a poller sync reading a high `seven_day_pct` with a reset epoch. **Caveat**: `UpsertRateLimits` overwrites `seven_day_pct` wholesale, so a routine sync while still exhausted can lower a `limit_report` 100 to 99 (`fractionToPct` floors, `client.go:260`). The baseline must therefore NOT test `== 100` exactly (see predicate).
- The poller tick `pollToken(ctx, userID, secretID, …)` (`engine.go:235`) fetches a fresh reading and calls `UpsertRateLimits` (`engine.go:296`). It does **not** currently read the prior row. The poller is the **sole writer** of a token's row and runs one goroutine per token (`engine.go:185`), so a read-then-upsert in the same tick has no lost-update race.

### Detection predicate (M2)
In `pollToken`, read the current stored row as `prev` (new `:one` query `GetRateLimitsForToken(user_secret_id)`); the fresh reading is `next`; `now` is poll time; `T = prev.seven_day_resets_at`. Fire when **all** hold:

- **Was constrained**: `prev` exists; `prev.seven_day_resets_at` is set and was in the future relative to `prev.synced_at` (a genuine pending weekly reset `T`); and the window was actually limiting — `prev.source == 'limit_report'` **or** `prev.seven_day_pct >= exhaustionPct`. `exhaustionPct` is a named constant set **below 100** (e.g. 95), NOT `== 100`: a routine sync can floor a `limit_report` 100 down to 99 while the window is still exhausted, so an exact test silently misses (reviewer-confirmed against `fractionToPct`, `client.go:260`).
- **Reset observed (authoritative = the reset epoch moved)**: `next.seven_day_resets_at` is set and `> T` (the window advanced to a new, later boundary). A lone `seven_day_pct` drop with `resets_at` unchanged is **NOT** treated as a reset — that path fires on 100→99 poll jitter and would DM a spurious "reset early." The moved epoch is the only accepted reset signal.
- **Early**: `now < T − earlyResetThreshold` (`earlyResetThreshold = 8 * time.Hour`, named constant with its reason beside it).

**Idempotency** is edge-detected: the tick upserts `next` as the new `prev`, whose `resets_at` is now the new boundary, so the next tick's `T` differs and the compare cannot re-fire. **No dedupe column** — instead the tick order is **compute-decision → upsert `next` → (if fire) notify**, giving *at-most-once*: a crash between upsert and notify drops the alert (acceptable for an advisory) rather than duplicating it on restart. (Notify-then-upsert would risk a duplicate on restart; rejected, D7. A `seven_day_early_notified_at` column would give at-least-once but is not worth it.)

**NULL resets_at**: a token parked before it was ever polled has `seven_day_resets_at = NULL`, so "was constrained" fails and nothing fires — a deliberate miss (`T` unknown ⇒ "how early" is uncomputable).

**Per-token, not per-user**: a user with N tokens can get up to N alerts (one per credential that reset early). Acceptable for v1; per-user coalescing is a noted follow-up, not built here.

**Gate on the setting**: read the token owner's `users.notify_early_limit_reset` (via a `users` JOIN on `ListAnthropicTokensToPoll`, or a small getter) and skip detection work when off.

### Delivery (M3)
Delivery rides `notifysvc.Service.Notify(ctx, Notification{UserID, Kind, Payload, Slack: &SlackRender{…}})` (`service.go:136`; `Kind` is free text, no CHECK, so **no migration** for a new kind). `Notify` is **persist-first**: it `InsertNotification` (a durable inbox row + bell badge) and *then*, when `Slack` is set, DMs the linked user via `handleNotify` (drops unlinked/opted-out silently). **So this feature writes an inbox row on every fire, DM or not** — embraced, not bypassed (D3); it means M5 must add a **web inbox renderer** for the new kind (an unrendered kind shows as "unknown", `web/src/lib/notifications.ts`).

The slacksvc Block Kit render mirrors `notificationBlocks` (`notifier.go:339`): a `🚨`-led header, then `reset ~Xh early`, `observed at HH:MM`, `expected HH:MM` (user-local where the render has tz, else UTC ISO). **Wording note**: the reset is seen on the *next* tick, so the figure is "observed at poll time" and understates the true earliness by up to one `UsagePollInterval` — say "observed", do not imply an exact reset instant.

The engine gains a **nil-safe** `notifier` collaborator wired via a new `SetNotifier` (pattern: `forgesvc.SetNotifier` at `forgesvc/service.go:191`; `main.go` already builds `notifier` at ~`:485` and `usageEngine` at ~`:784`, a two-line wire). Nil notifier = detection logs but does not deliver (safe default, and the unit tests' default).

**Security parity**: follow `TestNotificationBlocksInjectedMentionInert` (`notifier_notify_test.go:228`) — no interpolated field may become a live Slack mention. The times/hours are numeric/formatted, so low risk, but assert it.

### Setting (M1 + M4/M5/M6)
Mirror the `wait_on_limit` setting end to end — and note this is a **gate-enforced DTO contract change** (the web toggle READS the current value off the user DTO), so it is not just a column:
- **Column**: `users.notify_early_limit_reset boolean NOT NULL DEFAULT true` (migration; number assigned at merge time per the goose convention — draft only). Down drops it.
- **Query**: `SetUserNotifyEarlyReset` (in `api/internal/store/queries/users.sql`, mirroring `SetUserWaitOnLimit` at `users.sql:49`), plus the flag surfaced to the poller (a `users` JOIN on `ListAnthropicTokensToPoll`, or a getter).
- **DTO** (M1): add `notify_early_limit_reset` to `apitypes.UserDTO` (`user.go:24`), populate in `toDTO` (`handler.go:467`), and update the contract fixtures `fixtures/api-contract/user.{full,zero}.json` + the web type `web/src/lib/apiTypes.ts:1309`. This is the read path the web toggle needs; the repo's DTO-contract gate fails if any of these drift.
- **Route** (M4): `PUT /api/me/notify-early-reset`, `{ "enabled": bool }` in, `toDTO(updated)` out — a near-copy of `SetUserWaitOnLimit` (`api/internal/handler/waitonlimit.go`), in its own small handler file (`handler/notifyearlyreset.go`; do **not** fold into `settings.go`). The route line registers in `handler.go`'s `Routes` (near `handler.go:951`) — this is the line #1008 restructures, hence M4's gate.
- **Web** (M5): a toggle on the same user-settings surface as the wait-on-limit default, **plus the inbox renderer** for the new notification kind. (#1007 already landed, so just rebase.)
- **CLI** (M6): a flag/subcommand to read/set it, mirroring `--wait-on-limit`, per the "new functionality ⇒ check `api/cmd/uzi/`" convention.

## Milestones

| # | Milestone | Primary files | Shares files with in-flight #915 child? | Phase |
|---|---|---|---|---|
| M1 | Setting storage + DTO + gauge getter: `users.notify_early_limit_reset` migration (default true), sqlc (`SetUserNotifyEarlyReset` in `users.sql`, `GetRateLimitsForToken`, `ListAnthropicTokensToPoll` users-JOIN), `UserDTO` field + `toDTO` + contract fixtures + web type | `store/migrations/*`, `store/queries/{users,anthropic_rate_limits}.sql`, generated sqlc, `apitypes/user.go`, `handler.go` (toDTO), `fixtures/api-contract/user.*.json`, `web/src/lib/apiTypes.ts` | No | 1 (parallel) |
| M2 | Early-reset detection in the usage poller: prior-row compare, `earlyResetThreshold` predicate (moved-epoch signal), setting gate, upsert-then-notify order, nil-safe notifier wiring; characterization + mutation-checked tests | `usagepoller/engine.go`, `usagepoller/*_test.go`, `main.go` (wire) | No | 2 (needs M1 + M3's kind) |
| M3 | Loud Slack DM render: `notifysvc` kind + slacksvc Block Kit render (observed-time wording), injected-mention-inert test | `notifysvc/*`, `slacksvc/notifier.go`, `slacksvc/*_test.go` | No | 1 (parallel; must precede M2 integration) |
| M4 | Toggle HTTP route `PUT /api/me/notify-early-reset` (own handler file) | `handler/notifyearlyreset.go`, route registration | **Yes — #1008** (handler route-table split) | 3 (after #1008) |
| M5 | Web toggle on the user-settings surface **+ inbox renderer for the new notification kind** | `web/src/**` settings surface, `web/src/lib/notifications.ts` | #1007 **already landed** (PR #1018) — unblocked, just rebase on main | 3 |
| M6 | CLI toggle (mirror `--wait-on-limit`) | `api/cmd/uzi/*` | **Yes — #1009** (CLI file splits) | 3 (after #1009) |
| M7 | Docs: user-facing setting page + `docs/scheduling.md`/limit docs cross-ref, CHANGELOG `[Unreleased]` | `docs/**`, `CHANGELOG.md` | No | 3 |

**Parallelization**: M1 and M3 have no dependency and run in Phase 1 (disjoint files). M2 needs M1 (getter + setting flag) *and* M3's notification-kind constant to exist before it compiles, so M2 is Phase 2. M4/M6 are the gated edges (wait for #1008/#1009 to land, then merge cleanly); M5 is unblocked (#1007 landed). M7 last.

## Dependencies

- **#1008** (handler route-table split, `In Progress`, driven by `watcher2`) → gates **M4**.
- **#1009** (CLI file splits, `In Progress`, driven by `watcher2`) → gates **M6**.
- **#1007** (web settings extraction) → **already landed (PR #1018, 2026-09-02)**; M5 is unblocked.
- The detection + delivery **core (M1–M3) has no dependency** — it shares no files with any in-flight child and could land first.
- Do not label the issue `Planned`/`uzi` (no sweep) until #1008 and #1009 have landed, so the whole PRD can go to uzi at once with the edges conflict-free. (Per the user's decision, 2026-09-02.)

## Risks & mitigations

- **False positives from poll jitter** (100→99 pct wobble). Removed by making the *moved reset epoch* the only accepted reset signal — a lone pct drop never fires. Plus the `≥8h` guard and the "was constrained" precondition. Asserted by SC2's modestly-early fixture (mutation-live) and a "pct jitter, `resets_at` unchanged → silent" case.
- **Missed detection from `==100` baseline drift**. A sync can floor a `limit_report` 100 to 99; the baseline uses `>= exhaustionPct` (<100) or `source=='limit_report'`, not exact equality.
- **Duplicate / dropped alerts** across ticks or restart. Edge detection (fired tick becomes the new baseline) + upsert-then-notify ordering give at-most-once; a mid-tick crash drops rather than duplicates. No dedupe column (D7).
- **Token in poller backoff** at the moment its window reopens → no fresh reading, so detection waits until backoff clears (`engine.go:230`, fail-closed), possibly past the alert's usefulness. Accepted; detection resumes when backoff clears.
- **Poller disabled** (`UsagePollInterval = 0`) → no detection. Acceptable and documented; on by default.
- **Multiple tokens** → multiple alerts for one user. Accepted for v1; per-user coalescing is a noted follow-up.
- **Edge merge conflicts** with #915 children. Removed by construction: M4/M6 wait for #1008/#1009. Rebase check: confirm neither child relocated the `main.go` service-wiring lines M2 also edits (low risk — wiring is near `main.go:784`, the route call site is near `:951`).

## Test plan

- **M2**: table-driven over `(prev, next, now)` — early ~6h **modestly inside the window** (fires, and keeps the `8h→0` mutation live), on-time (silent), a boundary case pinning `<` vs `<=`, not-previously-constrained (silent), setting-off (silent), **pct jitter with `resets_at` unchanged (silent** — the moved-epoch guard), moved-epoch (fires), per-token **fan-out count** (N early-resetting tokens → N alerts), setting-off suppresses all of a user's tokens, and once-only across two ticks. Mutation-check the `8h` constant, the `exhaustionPct` precondition, and the moved-epoch requirement (each, when removed/loosened, turns a silent case red).
- **M3**: render test — blocks + fallback carry expected/observed timestamps and the hours-early figure, **both tz branches** (user-local and UTC-ISO fallback) — plus the injected-mention-inert assertion.
- **M4/M5/M6**: handler/CLI/web tests mirroring the existing `wait_on_limit` toggle tests; a web inbox-renderer test for the new kind.
- **Ordering note**: the notify-then-crash-before-upsert window cannot be exercised by the in-process two-tick test; the at-most-once ordering is a reasoned decision (D7), not gated by that test.
- Full `task gate` for each touched component; serial component gates per repo convention.

## Decision log

- **D1 — Weekly only.** Feature reads only `seven_day_*`; the 5-hour window is explicitly out (user's ask). Revisit only on a separate request.
- **D2 — Threshold is a constant.** `earlyResetThreshold = 8h`, named, reason beside it; not user-configurable in v1 (YAGNI).
- **D3 — Reuse `notifysvc.Notify` (persist-first), accept the inbox row.** No new service/port/trust boundary; "inert without linked Slack" is inherited from `handleNotify`. `Notify` always writes a durable inbox row then DMs, so the feature ships an inbox notification **and** a DM. The Slack-only alternative (call slacksvc `PublishNotification` directly, no inbox row) was rejected as swimming against the seam; the inbox row is a free durable record. Cost: M5 adds a web renderer for the new kind.
- **D4 — Piggyback the poller, no new probe loop.** Detection is a compare inside the existing per-token tick; cadence is whatever `UsagePollInterval` already is (fine against an 8h threshold).
- **D5 — Detection/delivery core is separable from the toggle edges.** M1–M3 share no files with in-flight #915 work; M4/M6 are the only overlaps and are each gated on their child (M5's #1007 already landed). This is what lets the feature be authored now and shipped after the splits land.
- **D6 — Per-token detection, per-token alert.** v1 does not coalesce across a user's multiple credentials.
- **D7 — At-most-once via upsert-then-notify; no dedupe column.** Order is compute → upsert `next` → notify. A crash between upsert and notify drops the alert (acceptable for an advisory) rather than duplicating on restart. A `seven_day_early_notified_at` column would give at-least-once but is not worth it. The notify-then-crash window is not exercisable by the two-tick test, so this is a reasoned decision, not a test outcome.
- **D8 — The moved reset epoch is the only reset signal.** `next.seven_day_resets_at > prev.seven_day_resets_at` is authoritative; a lone `seven_day_pct` drop (with `resets_at` unchanged) is poll jitter and must not fire. The "was constrained" baseline uses `>= exhaustionPct` (<100) or `source=='limit_report'`, never `== 100` exact (a sync floors 100→99).
