# PRD #4123: Weekly limit warning and switch reason

**Issue**: #4123
**Status**: Draft
**Priority**: Medium

## Problem

A run can stop for days on a 7-day usage limit with no warning beforehand. When it parks, the park notice (web, TUI, Slack `⏸️ Paused · usage limit`) names the window and the resume time, but not why uzi stayed on that credential or what the owner could change. Observed: a run hit its weekly limit and did not move to the owner's other subscription token, because that token was not in the auto-select pool; the owner was away for hours and found out afterwards.

The inputs already exist. The Anthropic usage poller (`api/internal/usagepoller`) stores each token's overall 7-day percent and reset (`anthropic_rate_limits`); the Codex usage poller (`api/internal/codexusagepoller`) stores each linked account's windows (`codex_account_rate_limits`); the weekly usage reserve (PRD #3522, `api/internal/usagereserve`, `users.weekly_reserve_pct`) already holds new runs and pauses running ones at a per-user 7-day threshold. Nothing warns before either limit, and nothing says why a run could not move.

"Credential" below means an Anthropic subscription token or a linked Codex subscription account (Codex is metered per account, not per login alias). OpenAI API keys have no weekly window and are excluded.

## Outcome

Owners learn ahead of time that a credential's weekly allowance is nearly spent, and, for every affected run, whether uzi can move it and what to change if not. The same server-computed facts render in web, TUI/CLI and Slack.

Acceptance examples:

1. Owner has Anthropic tokens A (pooled, worker on auto) and B (not pooled, 20% weekly used). Run R is on A. A's overall weekly reading reaches 86% with no reserve set. Within one poll, the run page and the Overview "Needs your decision" card show a warning: "A is at 86% of its weekly limit (resets Thu 09:00). R cannot move: B has headroom but is not in the pool." with "Add B to the pool". The owner gets one Slack DM saying the same. A later reading at 90% in the same week sends no second DM.
2. Owner has one Codex account at 88%, a Codex run on it, no reserve. Web, TUI and Slack show a notice: the run stays on this account; with waiting on usage limits enabled and a provider reset known, it waits for the reset at <time> if its remaining park budget covers it; otherwise it will fail at the limit. Pooling another account is offered for future runs only.
3. Run R parks on `limit_wait` with `rate_limit_type = seven_day` while pinned to token A. `uzi run get R` and the run page show reason `pinned` and the fix `uzi run set-token R <label>` (or auto-select). The Slack park message carries the same line. A `five_hour` or `seven_day_opus` park is unchanged.

## Out of scope

- The 5-hour window and the per-model and overage 7-day windows (`seven_day_opus`, `seven_day_sonnet`, `seven_day_overage_included`). Only the overall 7-day window drives this PRD.
- OpenAI API keys.
- Switching an existing run's harness, or an existing Codex run's account. New-run harness choice stays with #3845; Codex account choice stays at run creation (#3719).
- Automatic switching. A pin or an unpooled credential may be deliberate; uzi only explains and offers the fix.
- Burn-rate forecasting. v1 uses a fixed margin.
- Alerts with no non-terminal run on the credential.
- Guaranteed Slack delivery of the warning, and any in-app inbox entry (see D11).

## Modules and seams

### 1. `workersvc`: next-claim switch reason (new, read-only)

- **Interface**: `NextClaimReason(ctx, run) (SwitchReason, alternative *CredentialRef, futureRuns *SwitchReason)`.
- **Derivation**: M1 factors the selection core of `autoChoice` (`workersvc/secretchoice.go`: `claimExclude`, the reserve filter `gate.dropBlocked`, `autoselect.Select`, `autoselect.Floor`, then the pool_wait or reserve hold) into one function called by both the real claim and `NextClaimReason`, which runs it read-only. The two cannot diverge. It does not read `anthropic_select_reason` / `codex_select_reason`, which record a past pick.
- **Values**:
  - `can_switch`: the shared selection would bind a different credential on the next claim. A `Floor` pick (a stale or unmeasured pooled token) is `can_switch` with headroom unknown.
  - `pinned`: Anthropic run credential override.
  - `not_auto`: the worker's credential mode (Anthropic) is not auto-select.
  - `headroom_outside_pool`: the selection finds nothing to move to, and another of the owner's usable credentials has weekly headroom but is not pooled (named).
  - `pool_exhausted`: the selection finds no eligible pooled alternative it could claim. Being near the warning margin is not exhaustion.
  - `no_alternative`: no other usable credential of that harness (none, disabled, unlinked or failing).
  - `switch_pending`: a credential switch is already in flight; `run set-token` returns 409 then (`api/cmd/uzi/credential_switch_hold.go`), so no fix is offered.
  - `frozen`: Codex. The account is bound at run creation (`codex_pool_select.go` `checkFrozenCodexAccount`) and the `limit_wait` reclaim keeps account and alias, so an existing Codex run never moves. `futureRuns` carries what pooling or `uzi settings codex-mode auto` would change for runs created afterwards.
  - `unknown`: the selection itself cannot run (for example the owner's credentials cannot be read).
- **Tests**: `workersvc` tests per value and harness, with and without a reserve. Mutation: break the reserve filter or the exclude inside the shared function and watch a `can_switch` test fail.

### 2. `weeklylimit.ForRun`: the block (new server function)

- **Interface**: `ForRun(ctx, run, snapshot) *WeeklyLimit` = `{credential_label, harness, pct, effective_limit, resets_at, tier: warning|notice, reason, alternative_label, future_runs, fix: [{kind, label}], wait: waits|fails|reserve_pause|unknown}`. Called by the run DTO builder and by the Slack park notifier (which reads its own run-context query, `GetSlackRunContext`, so it must call this function rather than re-derive from its row). One computation, every surface.
- **Present** when the run is parked on `limit_wait` with `rate_limit_type = seven_day` or on a weekly-reserve pause (M1), or when its credential is near or at its effective weekly limit (M2). Computed on read; nothing persisted.
- **`fix` kinds** map to existing actions only: `pool_on` (`uzi token pool <label> --on`, `--kind codex` for Codex), `auto_select` (worker auto; `uzi settings codex-mode auto` for Codex), `set_token` (`uzi run set-token`, Anthropic only, `run_credential.go` refuses Codex), `resume_reserve` (`uzi run resume <id>`), `edit_reserve` (Settings). Tier is `warning` when the run itself has a fix, else `notice`.
- **`wait`** states what will actually happen, from the run's own state: `waits` only when wait-on-limit is on, a provider reset is known, and the remaining park budget covers it (`RUN_LIMIT_MAX_PARK` per park, `RUN_LIMIT_MAX_WAITS` parks per run, `workersvc/limitwait.go`; a Codex window without a usable reset takes the capped backoff and can exhaust the budget before the week reopens, `docs/run-limit-wait.md`); `fails` when it does not; `reserve_pause` releases on a fresh reading below the reserve or a manual resume; `unknown` otherwise. No surface promises a resume the run will not get.
- **Read cost**: one per-owner snapshot per request (the `reserveSnapshot` pattern in `workersvc/usage_reserve.go`), memoised per credential. `RunDTO` carries the full block on run detail; `RunSummaryItemDTO` carries only `{tier, credential_label}`, so a list request costs one evaluation per distinct credential, not runs times credentials.
- **Escaping**: labels are owner-authored text, escaped per surface: Slack `EscapeMrkdwn` (blocks and fallback text), the bounded `cellText` in `api/cmd/uzi/render.go` (not the unbounded `CellText`) for TUI/CLI, React text on web. Each surface gets a hostile-label test.

### 3. `usagereserve`: weekly near-limit evaluation (extend, pure; M2)

- **Interface**: `EvaluateNear(threshold *int16, marginPts int, reading) NearVerdict` = `{State: below|near|at|unknown, EffectiveLimit, Pct, ResetsAt}`. Effective limit is the reserve when set, else 100.
- The weekly extraction is inline in `EvaluateClaude` / `EvaluateCodex` today: factor it (including `Stale`, `PreReset` and the fail-closed rule for invalid windows) into one helper used by both the reserve and `EvaluateNear`, so they cannot disagree. Anthropic: `seven_day`; Codex: the window with `limit_window_seconds = WeeklyWindowSeconds` (604800), primary or secondary, highest utilization when several.
- **Tests**: table tests in `usagereserve_test.go`.

### 4. Surfaces (render only; none re-derives)

- **Web**: a banner on the run page (`RunCredential` area) from `weekly_limit`. The "Needs your decision" card (Overview preview, #4041 and #4067, which #3877 promotes) gains a `limit` kind for `tier = warning` runs, grouped per credential, from `RunSummaryItemDTO`. Parked runs are excluded from `limit`: a `seven_day` park already appears under the existing `park` kind (`web/src/lib/overview.ts`). Notice-tier runs show the banner only.
- **TUI and CLI**: `uzi run get` and run detail render one line plus the fix command; `--json` carries the block.
- **Slack**: the existing `limit_wait` park message (`limitWaitDetail`, `slacksvc/notifier_state.go`) and the weekly-reserve pause message (`weeklyReserveHold`) append the reason and fix when the park is `seven_day`. The early warning is a DM through `notifysvc` (module 5).

### 5. Warning notification (M3)

- **Trigger**: wired into both pollers the way the PRD #1020 early-reset notifier is (`usagepoller.SetNotifier` / `EarlyResetNotifier`, `codexusagepoller/early_reset.go`): after a successful fenced reading write, never before. When `EvaluateNear` returns `near` or `at` and the credential has non-terminal runs, notify the owner once, grouping the affected runs.
- **Dedupe**: `weekly_limit_alerts (id, anthropic_secret_id NULL REFERENCES user_secrets ON DELETE CASCADE, codex_account_id NULL REFERENCES codex_provider_account ON DELETE CASCADE, warned_reset_at)`, a CHECK that exactly one of the two IDs is set, and a unique index on each ID. A conditional upsert (`WHERE warned_reset_at IS DISTINCT FROM excluded.warned_reset_at`) claims the window; zero rows affected means already warned. At most one row per credential, so no pruning job is needed. Only a valid provider reset arms a warning, normalised to the hour: `usagereserve.CodexWindowReset` falls back to the reading time when a window has no valid reset, which would change the key every poll, so a reading without a valid reset sends nothing.
- **Atomic claim**: the dedupe upsert and the `notifications` row commit in one transaction (M3 adds a transaction-scoped variant of `notifysvc.Notify`; none exists today), and Slack is attempted only after commit. A failed row write rolls the claim back, so the next poll retries; a claim never commits without its row.
- **Delivery**: a new kind, credential-fenced like `NotifyEarlyReset` / `NotifyCodexEarlyReset`. The Slack DM is best-effort: the durable Slack path deliberately excludes credential-fenced notifications (`notifysvc/service.go`, "a stale fenced DM must never be replayed"). The `notifications` row is not owner-visible (PRD #1650 retired the in-app inbox; the table is a pruned, write-only event log). A lost DM is accepted: the live web/TUI banner still shows the warning.
- **Bounds**: one row per credential; one notification per credential per weekly window.

### 6. Per-user margin setting (M2)

- `users.weekly_warning_margin_pts smallint NOT NULL DEFAULT 15`, CHECK 0..50; 0 disables the warning (the park reason is unaffected). Written by a dedicated `PUT /api/me/weekly-warning-margin` (each per-user toggle has its own PUT, `routes_me.go`; `/me/usage-reserve` is not widened, its body requires both reserve fields), read back read-only in `GET /api/me/settings` beside `weekly_reserve_pct`. Web Settings and `uzi settings` expose it. Migration precedent: `00366_users_weekly_reserve.sql`; the number is assigned at merge time.

## Testing decisions

- Selection and reason: `workersvc` tests per value and harness (module 1), including Floor giving `can_switch`, every existing Codex run giving `frozen`, and the mutation control.
- Block: handler tests for presence and absence (5-hour and per-model parks have no block), the summary projection, and the memoised per-owner snapshot.
- Evaluation: `usagereserve_test.go` table tests (staleness, reserve vs none, Codex primary/secondary weekly, several weekly windows, clamping); the reserve's existing tests stay green across the extraction refactor.
- Slack: extend the park cases in `slacksvc/notifier_test.go` ("Paused · usage limit") and `notifier_reserve_test.go` (with and without the reason, hostile label in blocks and fallback); notification tests: second reading same window sends nothing, two Codex polls without a valid reset send nothing, a new window sends again, credential fencing drops a stale credential.
- Web: Vitest for the banner and the `limit` kind (and its exclusion of parked runs), mock data in `web/src/mocks/data/`. CLI: the `api/cmd/uzi/render_test.go` and `reserve_hold_render_test.go` patterns.
- Live DB (`e2e/run-store-it.sh`, `store` is in its package list): both migrations, the conditional upsert, the cascade, existing user rows reading margin 15, a reserve-only user's behaviour unchanged.
- E2E: `e2e/phases/16-usage-limit-park.sh` parks on `five_hour` and asserts the `limit_wait` feed message and a "usage limit" failure reason. M1 leaves it unchanged and either adds a `seven_day` park check or names it as unrun in the PR; per the repo rule, `git grep -F` every changed literal across `e2e/`.

## Milestones

### M1: Parked runs say why they could not switch

Modules 1, 2 (park cases), 4 (run page, `uzi run get`/TUI, Slack park and reserve-pause messages). The selection refactor lands here. No migration.

- **Blocked by**: none.
- **Acceptance**: example 3; a `five_hour` park renders exactly as today; a Codex weekly park shows `frozen` with future-runs fixes; the claim path behaves identically after the refactor (existing claim and autoselect tests green); one computation rendered on all three surfaces; docs (`docs/run-limit-wait.md` and the credentials page, `task docs:sync`) describe the reasons.

### M2: Early warning on web and CLI, with the margin setting

Module 3, the near/at cases of module 2 for running and queued runs, the `RunSummaryItemDTO` projection, the run-page banner, the `limit` decision kind, TUI/CLI rendering, module 6 (migration).

- **Blocked by**: M1.
- **Acceptance**: examples 1 and 2 on web and CLI; margin 0 disables the banner but not the park reason; a stale reading shows usage unknown and no banner; a parked run is not listed twice; the setting round-trips in web and `uzi settings`.

### M3: Slack warning, once per weekly window

Module 5 (migration).

- **Blocked by**: M2.
- **Acceptance**: example 1's DM behaviour; no notification without non-terminal runs, without a fresh reading, or without a valid provider reset; a new weekly window notifies again; a failed notification-row write rolls back the claim and the next poll notifies (test); a Slack failure loses only the DM, the banner stays.

## Decision Log

- **D1. One PRD, three milestones.** The scope gate's large-PR alarm fired (server, web, CLI/TUI, Slack). Kept as one outcome: the park reason (M1) and the warning (M2/M3) share the selection function and the block; M1 is independently useful and lands first. Rejected: two PRDs splitting one shared module across two designs.
- **D2. Compute on read; persist only the warning dedupe.** The banner is derived per request from current readings, so it cannot go stale or need clearing. Rejected: a persisted alert-state table driving the banner.
- **D3. The reason runs the real selection, shared with the claim.** `*_select_reason` describes the last pick, and eligibility predicates alone (`autoselect.Classify`) are the wrong layer: `Floor` spends a stale pooled token, so a predicate-based reason would say `unknown` where the run moves. Rejected: re-implementing the rules in a parallel reason function.
- **D4. Codex runs are `frozen`.** The Codex account is bound at run creation and the `limit_wait` reclaim keeps it. Codex fixes are offered for future runs only, and existing Codex runs get the notice tier.
- **D5. Overall 7-day window only.** A park is matched by `rate_limit_type = seven_day` for both harnesses (Codex parks store the same value, `agent/src/codex/rate-limits.ts`); readings by Anthropic `seven_day` and Codex `limit_window_seconds = 604800`. The per-model and overage windows share a gauge column in the window mapper, so that mapping alone cannot identify an overall-weekly park. User decision: the 5-hour window is no longer in use.
- **D6. Effective limit is the reserve when set.** A reserve pause stops the run before 100%, so warning against 100% would arrive after the pause. Rejected: always 100%.
- **D7. Default margin 15 points, per-user, 0 disables.** A starting product choice, not measured; revisit with use. Rejected: a fixed instance-wide margin.
- **D8. Conditional wait text.** The notice says what the run will actually do, from wait-on-limit, provider reset, park budget and reserve state. Rejected: a blanket "will resume after reset".
- **D9. Warn only with non-terminal runs on the credential.** An idle owner near the limit has nothing at risk; the Settings usage view already shows the percent.
- **D10. Nothing switches automatically.** Pins and unpooled credentials can be deliberate (reserved for interactive work).
- **D11. Best-effort Slack DM; the banner is the guaranteed surface.** The durable Slack path excludes credential-fenced notifications by design, so a replayed DM can never name a credential that changed. The in-app inbox is retired (PRD #1650), so the notification row is not a fallback; the live banner is. The dedupe claim and the row commit atomically so a failed write never suppresses later attempts. Rejected: a custom durable path for this one kind, which would reopen the stale-fenced-DM hazard.
- **D12. One server function for the block.** DTO and Slack notifier both call `weeklylimit.ForRun`. Rejected: extending the notifier's run-context query to re-derive the reason.
- **D13. One dedupe row per credential, keyed on a valid provider reset.** The reading-time fallback for a reset-less Codex window would make every poll a new key. One row per credential with a cascade needs no pruning. Rejected: a row per (owner, credential, window), which grows and needs a sweeper prune.
