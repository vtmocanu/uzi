# PRD #4123: Weekly limit warning and switch reason

**Issue**: #4123
**Status**: Done (2026-11-26). M1–M3 shipped in v0.87.0-rc.23 (#4623). Owner acceptance: live Slack DM verified on the hosted instance; hosted banner and Overview rendering waived by the maintainer.
**Priority**: Normal

## Problem

A run can stop for days on a 7-day usage limit with no warning beforehand. When it parks, the park notice (web, TUI, Slack `⏸️ Paused · usage limit`) names the window and the resume time, but not why uzi stayed on that credential or what the owner could change. Observed: a run hit its weekly limit and did not move to the owner's other subscription token, because that token was not in the auto-select pool; the owner was away for hours and found out afterwards.

The inputs already exist. The Anthropic usage poller (`api/internal/usagepoller`) stores each token's overall 7-day percent and reset (`anthropic_rate_limits`); the Codex usage poller (`api/internal/codexusagepoller`) stores each linked account's windows (`codex_account_rate_limits`); the weekly usage reserve (PRD #3522, `api/internal/usagereserve`, `users.weekly_reserve_pct`) already holds new runs and pauses running ones at a per-user 7-day threshold. Nothing warns before either limit, and nothing says why a run could not move.

"Credential" below means a vendor subscription credential: an Anthropic subscription token (`anthropic_token`) or a linked Codex subscription account (Codex is metered per account, not per login alias). OpenAI API keys have no weekly window and are excluded. Custom model providers (#4464, #4323) are excluded too (see Applicability).

## Outcome

Owners learn ahead of time that a credential's weekly allowance is nearly spent, and, for every affected run, whether uzi can move it and what to change if not. The same server-computed facts render in web, TUI/CLI and Slack.

Acceptance examples:

1. Owner has Anthropic tokens A (pooled, worker on auto) and B (not pooled, 20% weekly used). Run R is on A. A's overall weekly reading reaches 86% with no reserve set. Within one poll, the run page and the Overview "Needs your decision" card show a warning: "A is at 86% of its weekly limit (resets Thu 09:00). R cannot move: B has headroom but is not in the pool." with "Add B to the pool". The owner gets one Slack DM saying the same. A later reading at 90% in the same week sends no second DM.
2. Owner has two linked Codex accounts, C1 at 88% and C2 at 10%, with a running Codex run on C1 and no reserve. Web, TUI and Slack show the warning. Automatic re-selection keeps the run on C1 (the account stays bound through limit-wait reclaims). The owner may switch the live run with `uzi run set-token <run> <C2 label>` while the run is eligible for the existing live Codex switch; pooling C2 is offered for future runs. If the run is parked, the switch is not offered (parked Codex runs refuse it). The wait line says that, with waiting on usage limits enabled and a provider reset known, the run waits for the reset at <time> if its remaining park budget covers it; otherwise it fails at the limit.
3. Run R parks on `limit_wait` with `rate_limit_type = seven_day` while pinned to token A. `uzi run get R` and the run page show reason `pinned` and the fix `uzi run set-token R <label>` (or auto-select). The Slack park message carries the same line. A `five_hour` or `seven_day_opus` park is unchanged.

## Applicability

- Only vendor subscription credentials are eligible: `anthropic_token` and linked Codex subscription accounts.
- Exclude OpenAI API keys and every provider-bound run before reading vendor meters or evaluating vendor switch actions. Use the run's frozen binding, not the owner's current source setting:
  - a vendor-bound run stays eligible after its owner selects a custom provider for new runs;
  - a provider-bound run never falls back into this feature.
- No custom-provider limits, key probing, reserves, account mapping or provider switching is added.
- This PRD does not depend on #4323 landing. It also does not repair the separately recorded provider/reserve residual (#4505).

## Out of scope

- The 5-hour window and the per-model and overage 7-day windows (`seven_day_opus`, `seven_day_sonnet`, `seven_day_overage_included`). Only the overall 7-day window drives this PRD. Existing `five_hour` parsing, meters, selection, parks and E2E behaviour stay unchanged.
- OpenAI API keys and custom model providers.
- Switching an existing run's harness. New-run harness choice stays with #3845.
- Any new switching eligibility, and any automatic switch. A pin or an unpooled credential may be deliberate; uzi only explains and offers existing actions. The existing owner-initiated live switch (Claude #4298, Codex #4474) is offered only where it already applies.
- Burn-rate forecasting. v1 uses a fixed margin.
- Alerts with no non-terminal run on the credential.
- Guaranteed Slack delivery of the warning, and any in-app inbox entry (see D11).

## Modules and seams

### 1. `workersvc`: next-claim switch reason (new, read-only)

- **Interface**: `NextClaimReason(ctx, run, snapshot) (SwitchReason, alternative *CredentialRef, futureRuns *SwitchReason)`.
- **Shared kernel**: M1 extracts the deterministic decision inside `autoChoice` (`workersvc/secretchoice.go`): `claimExclude`, the reserve filter `reserveGate.dropBlocked`, `autoselect.Select`, `autoselect.Floor`, then the pool_wait or reserve hold. It becomes one pure function over explicit inputs (candidates, exclusions, reserve, policy, clock). The real claim and `NextClaimReason` both call it, so the two cannot diverge.
  - Keep in the existing claim adapter: all claim reads, decryption and the open-failed retry, locks, mutations, and error/status handling.
  - The claim path gains no database or network work and never calls a display or notification service.
  - Existing selector results, errors and query counts stay identical.
- **Resolution precedence**: follow the real precedence for the run's lane (`claimSecretID` / `resolveClaimSecret` / `runOverrideChoice`):
  1. provider exclusion;
  2. judge/self-improve policy where applicable;
  3. per-run override;
  4. worker mode and pool.
  A per-run auto overrides a non-auto worker. Without authoritative worker/credential context, return `unknown`; never invent a binding from the owner's current default.
- **Read-only**: the preview reads metadata only. It never opens credentials, probes a provider, refreshes a meter, or writes claim or hold state. It is advice from one snapshot, revalidated by the real action or claim. It does not read `anthropic_select_reason` / `codex_select_reason`, which record a past pick.
- **Selection semantics**: match the shipped reset-first ranking and #4407 `Floor` behaviour, including its known-exhausted last resort. A different `Floor` candidate does not prove usable headroom.
- **Values**:
  - `can_switch`: the shared selection would bind a different credential on the next claim. A `Floor` pick (stale or unmeasured) is `can_switch` with headroom unknown. It does not mean a running session switches automatically, or that the provider will accept it.
  - `selection_unchanged`: the real selector picks the same credential although another eligible pooled credential exists. For example, current A outranks a less-used B because A resets sooner.
  - `pinned`: Anthropic run credential override.
  - `not_auto`: the effective Anthropic credential mode for this run is not auto-select.
  - `headroom_outside_pool`: the selection finds nothing to move to, and another of the owner's usable credentials has weekly headroom but is not pooled (named).
  - `pool_exhausted`: the selection finds no eligible pooled alternative it could claim. Being near the warning margin is not exhaustion.
  - `no_alternative`: no other usable credential of that harness (none, disabled, unlinked or failing).
  - `switch_pending`: a live credential switch is requested or settling (`credential_switch_settling` / `credential_switch_unsettled`, or a pending request stamp), so no switch fix is offered. Map from the existing typed refusals (`handler/run_credential.go`), not from one hold name.
  - `account_bound`: Codex. Automatic re-selection keeps the account bound at creation (`codex_pool_select.go` `checkFrozenCodexAccount`) through limit-wait reclaims. `futureRuns` carries what pooling or `uzi settings codex-mode auto` would change for runs created afterwards. A separate manual-switch flag says whether the existing live Codex switch applies (see module 2).
  - `unknown`: the selection itself cannot run, or no authoritative context exists.
- **Manual switch availability** is computed separately from automatic next-claim selection. Claude uses the in-place switch (#4298). Codex uses `requestCodexCredentialSwitch`, which today accepts running and worker-held `awaiting_approval` / `awaiting_input` / `awaiting_followup` runs on an eligible lane (`codexSwitchLaneEligible`) and refuses queued and parked runs. The preview reuses those eligibility checks; it adds no new eligibility. A request is not an applied switch, and cross-account session continuation is not promised.
- **Tests**: `workersvc` table tests per value and harness, with and without a reserve. Include:
  - reset-first `selection_unchanged`;
  - per-run auto on a fixed worker;
  - judge and non-switchable lanes;
  - absent worker;
  - all-known-exhausted `Floor`;
  - reserve exclusion and the `claimExclude` boundary;
  - live, parked and pending Codex switch cases, and an unsupported worker.
  Parity: existing claim/autoselect tests stay green, with identical results and query counts. Mutation: break the reserve filter or the exclude inside the shared kernel and watch a parity test fail.

### 2. `weeklylimit.ForRun`: the block (new server function)

- **Interface**: `ForRun(ctx, run, snapshot) *WeeklyLimit` = `{credential_label, credential_key, harness, pct, effective_limit, resets_at, tier: warning|notice, reason, alternative_label, future_runs, fix: [{kind, target}], wait: waits|fails|reserve_pause|unknown}`. `credential_key` is an owner-scoped stable grouping identity: the token id, or the Codex vendor account identity, never a display label or alias. The run DTO builder and the Slack park notifier both call it; the notifier must call this function rather than re-derive from its `GetSlackRunContext` row. One computation, every surface.
- **Present** in either case:
  - the run is parked on `limit_wait` with `rate_limit_type = seven_day`, or on a weekly-reserve pause (M1);
  - its credential is near or at its effective weekly limit (M2).
  It is computed on read; nothing is persisted.
- **`fix`**: structured, allowlisted actions with owner-scoped target identities, mapped to existing actions only.
  - `pool_on`: `uzi token pool <label> --on`, with `--kind codex` for Codex.
  - `auto_select`: worker auto; `uzi settings codex-mode auto` for Codex future runs.
  - `set_token`: `uzi run set-token`. Offered for Anthropic, and for Codex only when the existing live Codex switch applies to this run.
  - `resume_reserve`: `uzi run resume <id>`.
  - `edit_reserve`: Settings.
  Tier is `warning` when the run itself has a fix, else `notice`.
- **`wait`** states what will actually happen, from the run's own state, and reuses the existing wait policy with the stored retry time and budgets. Rendering never rolls jitter or writes park state.
  - `waits`: only when wait-on-limit is on, a provider reset is known, and the remaining park budget covers it. Per park the ceiling is `RUN_LIMIT_MAX_PARK`; per run the count is `RUN_LIMIT_MAX_WAITS` (`workersvc/limitwait.go`). A Codex window without a usable reset takes the capped backoff and can exhaust the budget before the week reopens (`docs/run-limit-wait.md`).
  - `fails`: when it does not wait.
  - `reserve_pause`: released by a fresh reading below the reserve, or by a manual resume. It must respect `reserveEnforced`, manual-resume exemptions, chat exclusion and `pause-running = false`. Never promise that an exempt running run will pause at the threshold.
  - `unknown`: otherwise.
  No surface promises a resume the run will not get.
- **Read cost**: per request, fetch the owner-scoped settings, candidate metadata and meter readings once per owner. Memoise the meter evaluation by credential/account and time. Compute reason, actions and wait per run from that shared snapshot, because pins, worker mode, lane, switch state, exclusions and park budget differ between runs on one credential. Do not cache a whole `ForRun` result by credential.
  - `reserveSnapshot` (`workersvc/usage_reserve.go`) reads no meters when no reserve is set. Use its batching pattern with a warning-specific need-to-read condition; do not call it unchanged and get an empty snapshot.
  - A newly queued, unbound Anthropic run shows no fabricated credential warning.
  - `RunDTO` carries the full block on run detail. `RunSummaryItemDTO` carries only `{tier, credential_key, credential_label}`. Preserve the existing owner/admin disclosure boundary for labels and alternatives.
- **Escaping**: labels are owner-authored text, escaped per surface.
  - Slack: `EscapeMrkdwn` (blocks and fallback text).
  - TUI/CLI: the bounded `cellText` in `api/cmd/uzi/render.go` (not the unbounded `CellText`) for every label cell. Exception: the fix-command cell is sanitized (`uzicli.CellText`) but not truncated, because a valid 64-byte label of `'` characters shell-quotes to 258 runes and a 200-rune cap would break the command; `set-token` has no stable-id form. A label that is not command-safe (sanitizing would change it, or it contains a replacement character) gets no command string at all, only a rename hint (D17).
  - Web: React text.
  - A rendered fix command that contains a label is shell-quoted correctly, or uses an existing stable-id form; `cellText` sanitization alone does not quote shell metacharacters.
  - Each surface gets a hostile-label test with quotes, backticks and `$(...)` syntax, assembled at runtime.

### 3. `usagereserve`: weekly near-limit evaluation (extend, pure; M2)

- **Interface**: `EvaluateNear(threshold *int16, marginPts int, reading) NearVerdict` = `{State: below|near|at|unknown, EffectiveLimit, Pct, ResetsAt}`. Effective limit is the reserve when set, else 100 (a nil reserve means 100, not "no reading").
- The weekly extraction is inline in `EvaluateClaude` / `EvaluateCodex` today. Factor it, including `Stale`, `PreReset` and the fail-closed rule for invalid windows, into one helper used by both the reserve and `EvaluateNear`, so they cannot disagree.
  - Anthropic: `seven_day`.
  - Codex: the window with `limit_window_seconds = WeeklyWindowSeconds` (604800), primary or secondary; the highest utilization when there are several.
- **Tests**: table tests in `usagereserve_test.go`. The reserve's existing tests stay green across the refactor.

### 4. Surfaces (render only; none re-derives)

- **Web**:
  - A banner on the run page (`RunCredential` area), from `weekly_limit`.
  - The default Overview (`OverviewNext`, `/dashboard`; #4553) "Needs your decision" card gains a `limit` decision kind for `tier = warning` runs, grouped by `credential_key`, from `RunSummaryItemDTO`. Do not build a second overview, and do not change `parkDisposition`.
  - Suppress a duplicate `limit` row only when the run is actually represented by an owner-decision `park` row. Timed `limit_wait` and automatic weekly-reserve pauses are classed `automatic` (`web/src/lib/overview.ts`), so they are not on the card today. Such a run gets the `limit` row when it has an applicable fix.
  - Notice-tier runs show the banner only. Owner-only personal decisions stay separate from the admin factory list.
- **TUI and CLI**: `uzi run get` and run detail render one line plus the fix command; `--json` carries the block.
- **Slack**: the existing `limit_wait` park message (`limitWaitDetail`, `slacksvc/notifier_state.go`) and the weekly-reserve pause message (`weeklyReserveHold`) append the reason and fix when the park is `seven_day`. The early warning is a DM through `notifysvc` (module 5).

### 5. Warning notification (M3)

- **Trigger**: wired into both pollers the way the PRD #1020 early-reset notifier is. Anthropic: `usagepoller.SetNotifier` / `EarlyResetNotifier`, with detection in `usagepoller/engine.go`. Codex: the `codexusagepoller` equivalents and `early_reset.go`.
  - It fires after a successful fenced reading write, never before.
  - When `EvaluateNear` returns `near` or `at`, the margin is non-zero, and the credential has non-terminal runs, notify the owner once, grouping the affected runs.
- **Window identity**: only a usable provider reset arms a warning.
  - `usagereserve.CodexWindowReset` returns an absolute valid timestamp, else `readAt + nonnegative relative seconds`, else nil. A nil result, or a relative zero (which yields the reading instant), sends nothing.
  - Banners may still say "reset unknown".
- **Dedupe**: `weekly_limit_alerts`, one bounded notification-state row per credential/account (`anthropic_secret_id NULL REFERENCES user_secrets ON DELETE CASCADE`, `codex_account_id NULL REFERENCES codex_provider_account ON DELETE CASCADE`). A CHECK enforces that exactly one of the two IDs is set, with a unique index on each. Dedupe is per vendor account, not per alias.
  - **The reset timestamp alone is not the window identity.** Re-arm only on an accepted fresh reading that proves a reset under the shared `ClaudeResetObserved` / `CodexResetObserved` semantics (`usagereserve`). That includes the unchanged-boundary utilization-drop arm, or a verified fresh post-boundary reading.
  - Serialize the row's observation/rearm update, so the same reset cannot be consumed twice, and delayed or out-of-order observations cannot re-arm an older window.
  - Store only the minimal bounded current-window/rearm and warned state that check needs. Within that state, atomically claim the warning and insert its notification row once.
  - Use the existing row/transaction mechanisms: no per-window history table, durable Slack queue or background job. With no usable reset evidence, no reset-keyed warning is sent.
  - The implementation plan must name the observation fence and the rearm transition. No arbitrary time tolerance is assumed.
  - At most one row per credential, so no pruning job is needed.
- **Atomic claim**: the dedupe upsert and the `notifications` row commit in one transaction.
  - Unchanged `notifysvc.Notify` cannot run inside it, because it prunes and enqueues Slack immediately. M3 adds a persist-only helper that takes the caller's transaction.
  - Publish and prune happen only after a successful commit; a rollback emits no Slack.
  - A failed row write rolls the claim back, so the next poll retries. A claim never commits without its row.
- **Delivery**: a new kind, credential-fenced like `NotifyEarlyReset` / `NotifyCodexEarlyReset`.
  - The existing credential/account authority fence (Codex: owner/account/generation/credential_revision/enablement_sig) is carried through creation and queued delivery.
  - The Slack DM is best-effort. The durable Slack path deliberately excludes credential-fenced notifications (`notifysvc/service.go`, "a stale fenced DM must never be replayed"), and that is not reopened for this kind. Ordinary Slack consent and link checks apply.
  - The `notifications` row is not owner-visible (PRD #1650 retired the in-app inbox; the table is a pruned, write-only event log). A lost DM is accepted: the live web/TUI banner still shows the warning.
- **Bounds**: one row per credential; at most one notification per credential per observed anchored weekly period (D16). This is not a guaranteed seven elapsed days. The grouped run list is capped: a total plus a truncated list, using existing render limits.

### 6. Per-user margin setting (M2)

- **Column**: `users.weekly_warning_margin_pts smallint NOT NULL DEFAULT 15`, CHECK 0..50.
  - 0 suppresses the early banners, the `limit` decision items and the early-warning notifications, including at-limit readings.
  - Park explanations (M1) are unaffected.
- **Write route**: a dedicated `PUT /api/me/weekly-warning-margin` (each per-user toggle has its own PUT in `routes_me.go`; `/me/usage-reserve` is not widened, because its body requires both reserve fields).
  - It uses `RequireUser`, outside the cookie-only settings write group: CLI Bearer works, and cookie writes require CSRF.
  - Identity is the authenticated owner, never a body user id.
  - It strictly accepts an integer 0..50 and rejects malformed values without a write. Unrelated settings are preserved.
- **Read**: read back in `GET /api/me/settings` beside `weekly_reserve_pct`.
- **Clients**: web Settings and `uzi settings` expose it.
- **Migration**: the migration is additive (precedent: the weekly reserve columns); its number is assigned at merge time.

## Testing decisions

- **Selection and reason**: `workersvc` table and parity tests (module 1), including the mutation control.
- **Block**:
  - handler tests for presence and absence (5-hour and per-model parks have no block);
  - the summary projection and its additive DTO full/zero fixtures and web/CLI mirrors;
  - two runs sharing one credential but differing in override, worker mode or park count;
  - a no-reserve owner, a resume exemption, pause-running off, margin 0;
  - unbound queued runs, and two aliases of one Codex account;
  - provider-bound and API-key runs never get a block.
- **Evaluation**: `usagereserve_test.go` table tests (staleness, reserve vs none, Codex primary/secondary weekly, several weekly windows, clamping, invalid windows).
- **Slack**:
  - extend the park cases in `slacksvc/notifier_test.go` ("Paused · usage limit") and `notifier_reserve_test.go`, with and without the reason, and a hostile label in blocks and fallback;
  - notification tests:
    - a second reading in the same window sends nothing;
    - warn at 90% with reset T, then a fresh 0% with T, then a fresh 90% with T sends exactly one new warning, for the newly observed allowance;
    - a forward reset recognized by the shared helper, including a move between one hour and one day, re-arms without an invented threshold;
    - a delayed pre-reset reading after a rearm cannot undo or repeat it;
    - a delayed replay of window A after observed window B does not notify again (unlike a genuinely observed refill with an unchanged reset timestamp);
    - two concurrent observers of the same reset re-arm once, and two near-limit claims in that new state create one notification;
    - small unproven timestamp jitter, duplicate readings and aliases do not re-arm;
    - no, malformed or relative-zero reset sends nothing;
    - margin 0 sends nothing;
    - aliases, rotation, relink and enablement changes;
    - credential fencing drops a stale credential;
    - a notification-row failure rolls back the claim;
    - nothing is dispatched before commit.
- **Margin route**: auth, CSRF, owner isolation, malformed input with no write, and a CLI round-trip.
- **Web**: Vitest for the banner and the `limit` kind (timed `seven_day` parks, automatic reserve pauses, true owner parks, no double listing, same-label/different-account grouping), with mock data in `web/src/mocks/data/`. CLI: the `api/cmd/uzi/render_test.go` and `reserve_hold_render_test.go` patterns, including shell-quoting of fix commands.
- **Live DB** (`e2e/run-store-it.sh`; `store` is in its package list): both migrations, the serialized rearm and warning claim, the cascade, existing user rows reading margin 15, and a reserve-only user's behaviour unchanged.
- **E2E**: `e2e/phases/16-usage-limit-park.sh` parks on `five_hour` and asserts the `limit_wait` feed message and a "usage limit" failure reason. Keep that contract. Either add a `seven_day` park check, or name it unrun in the PR. Per the repo rule, `git grep -F` every changed literal across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`, and run `task test:e2e-lib` and `task test:forge-fake`.

## Validation boundary

- **In-run**:
  - `task check:api`; affected Go packages with `-race -count=1`;
  - targeted LiveDB tests for the migrations, the dedupe and the notification transaction, in serial packages on owned throwaway databases, with a named PASS and zero SKIP;
  - `sqlc generate` plus a drift check; migration numbering and additivity checks;
  - `task check:web` with touched Vitest/browser proofs;
  - `task docs:sync` and `task check-docs:web`;
  - `task check:changelog-entry`, and `task check:token-literals` for assembled credential fixtures.
  No `task gate` and no full component gate.
- **Landing obligations (outside milestones)**: full `test:api`, the canonical store-integration sweep and the applicable full web/browser suites on exact-head PR CI. Keep `.github/workflows/**` out of implementation and validation. The design needs no agent/runtime change; if agent code becomes necessary, get scope review and use the label `full agent suite: exact-head PR CI`.
- **Owner acceptance (non_closing, outside milestones)**: a live Slack DM and hosted rendering on a real near-limit credential. Deterministic fake-Slack and database proofs are milestone tests, not owner acceptance.

## Milestones

### M1: Parked runs say why they could not switch

**Status**: implemented; unit, handler, CLI, web and Slack render tests plus mutation checks green in-run.


Modules 1, 2 (park cases), 4 (run page, `uzi run get`/TUI, Slack park and reserve-pause messages). The selection-kernel extraction lands here, with parity proven first. No migration.

- **Blocked by**: none.
- **Acceptance**:
  - example 3;
  - a `five_hour` park renders exactly as today;
  - a parked Codex weekly park shows `account_bound` with future-runs fixes and no live-switch fix;
  - the claim path behaves identically after the extraction (existing claim and autoselect tests green, same query counts);
  - one computation rendered on all three surfaces;
  - docs (`docs/run-limit-wait.md` and the credentials pages, `task docs:sync`) describe the reasons.

### M2: Early warning on web and CLI, with the margin setting

**Status**: implemented; the margin route LiveDB tests (auth, CSRF, default 15, CHECK) passed on a throwaway database.


Module 3, the near/at cases of module 2 for running and queued runs, the `RunSummaryItemDTO` projection, the run-page banner, the `limit` decision kind, TUI/CLI rendering, module 6 (migration).

- **Blocked by**: M1.
- **Acceptance**:
  - examples 1 and 2 on web and CLI;
  - margin 0 disables the banner and decision item but not the park reason;
  - a stale reading shows usage unknown and no banner;
  - a run is never listed twice;
  - the setting round-trips in web and `uzi settings`.

### M3: Slack warning, once per weekly window

**Status**: implemented; the weeklywarn state-machine tests and 15 LiveDB tests (ordering, rollback, concurrency, fences, cascade, vault reset, relink) passed on a throwaway database. Live Slack delivery is owner acceptance.


Module 5 (migration).

- **Blocked by**: M2.
- **Acceptance**:
  - example 1's DM behaviour;
  - no notification without non-terminal runs, without a fresh reading, without a usable provider reset, or with margin 0;
  - a reset observed under the shared reset semantics (including a refill at an unchanged reset timestamp) notifies again, and a delayed or replayed earlier window does not;
  - a failed notification-row write rolls back the claim and the next poll notifies (test);
  - a Slack failure loses only the DM, and the banner stays.

## Decision Log

- **D1. One PRD, three milestones.** The scope gate's large-PR alarm fired (server, web, CLI/TUI, Slack). Kept as one outcome: the park reason (M1) and the warning (M2/M3) share the selection kernel and the block. M1 lands first and is useful alone; the milestones are serial and not independent releases while the DTO/policy contract is incomplete. Rejected: two PRDs splitting one shared module across two designs.
- **D2. Compute on read; persist only the warning dedupe.** The banner is derived per request from current readings, so it cannot go stale or need clearing. Rejected: a persisted alert-state table driving the banner.
- **D3. The reason runs the real selection, shared with the claim.** `*_select_reason` describes the last pick. Eligibility predicates alone (`autoselect.Classify`) are the wrong layer: `Floor` spends a stale pooled token, so a predicate-based reason would say `unknown` where the run moves. The shared piece is a pure decision kernel; claim I/O stays in the claim adapter. Rejected: re-implementing the rules in a parallel reason function.
- **D4. Codex runs are account-bound for automatic selection (revised 2026-11-22).** The Codex account is bound at run creation, and limit-wait reclaims keep it, so automatic pooling changes affect future runs only. Since #4474, the owner can switch an eligible live, worker-held Codex run with `uzi run set-token`; queued and parked runs refuse. The preview offers that action only where the existing switch applies, and adds no new eligibility. Superseded: the 2026-11-09 text "an existing Codex run never moves" and "set_token is Anthropic only".
- **D5. Overall 7-day window only.** A park is matched by `rate_limit_type = seven_day` for both harnesses (Codex parks store the same value, `agent/src/codex/rate-limits.ts`); readings by Anthropic `seven_day` and Codex `limit_window_seconds = 604800`. The per-model and overage windows share a gauge column in the window mapper, so that mapping alone cannot identify an overall-weekly park. The five-hour window is out of scope for this warning; existing `five_hour` parsing, meters, selection, parks and E2E behaviour remain unchanged.
- **D6. Effective limit is the reserve when set.** A reserve pause stops the run before 100%, so warning against 100% would arrive after the pause. Rejected: always 100%.
- **D7. Default margin 15 points, per-user, 0 disables.** A starting product choice, not measured; revisit with use. Rejected: a fixed instance-wide margin.
- **D8. Conditional wait text.** The notice says what the run will actually do, from wait-on-limit, provider reset, park budget and reserve state (including exemptions). Rejected: a blanket "will resume after reset".
- **D9. Warn only with non-terminal runs on the credential.** An idle owner near the limit has nothing at risk; the Settings usage view already shows the percent.
- **D10. Nothing switches automatically.** Pins and unpooled credentials can be deliberate (reserved for interactive work).
- **D11. Best-effort Slack DM; the banner is the guaranteed surface.** The durable Slack path excludes credential-fenced notifications by design, so a replayed DM can never name a credential that changed. The in-app inbox is retired (PRD #1650), so the notification row is not a fallback; the live banner is. The dedupe claim and the row commit atomically, so a failed write never suppresses later attempts. Rejected: a custom durable path for this one kind, which would reopen the stale-fenced-DM hazard.
- **D12. One server function for the block.** The DTO and the Slack notifier both call `weeklylimit.ForRun`. Rejected: extending the notifier's run-context query to re-derive the reason.
- **D13. One bounded notification-state row per credential, re-armed only by an observed reset (revised 2026-11-22).** Re-arm follows the shared `ClaudeResetObserved` / `CodexResetObserved` semantics, including a refill at an unchanged reset timestamp, under a serialized observation/rearm update. A reset-less or relative-zero Codex window sends nothing. One row per credential with a cascade needs no pruning. Rejected:
  - `IS DISTINCT FROM` on the reset timestamp: A→B→A warns twice;
  - hour truncation: jitter across an hour boundary changes the key;
  - a forward-only timestamp threshold: it misses a refill at an unchanged reset timestamp, and recognized resets that move less than the threshold;
  - a row per (owner, credential, window): it grows and needs a sweeper prune.
- **D14. Vendor subscriptions only; no #4323 dependency (2026-11-22).** Applicability follows the run's frozen binding: provider-bound and API-key runs are excluded before any vendor meter read. Custom providers have no weekly subscription meter in uzi.
- **D15. Observation gap after the fenced meter write is an accepted residual (2026-11-22, maintainer lead and buddy).** The warning ledger's observation transaction stays separate from the fenced meter write and runs after it, and the fenced meter write paths are not changed. A crash, or a failed observation transaction, between the meter commit and the observation commit can lose a refill-only observation; that period may then get no second DM. This is accepted under D11 (the Slack DM is best-effort; the live banner is the guaranteed surface). It is a new decision, not a pre-existing allowance. It amends D11's sentence "a failed write never suppresses later attempts": that holds for the notification-row write (the claim rolls back and the next near reading notifies, because the observation already committed), not for a lost observation.
- **D16. Canonical Codex anchored period and the observed-period bound (2026-11-22).** A Codex weekly window (604800 s, primary or secondary) is identified by bucket id plus boundary, never by slot or payload order. The anchor is the valid weekly window with the highest utilization whose reset is after the observation instant (ties: earliest reset, then smallest bucket id, then primary). A sticky per-period flag (`anchor_multi`) records that the anchored bucket has shown two or more weekly windows; while it is set, a single remaining window is never inferred to be a forward roll, and only a matched unchanged-boundary refill or a verified fresh post-boundary reading (the observation instant is past the anchored boundary with no same-boundary window) starts a new period. The bound is at most one DM per credential per observed anchored period, not a guaranteed seven elapsed days. This amends D13 ("re-armed only by an observed reset") to also allow the verified post-boundary re-anchor, and an anchor cleared when no valid window resets in the future. Residual (data ambiguity): if a second weekly window B was absent from every accepted reading of the period, the flag stays false, so A vanishing while B appears alone with a later reset is treated as a forward roll and that period can get one extra DM. A window absent from every accepted reading is indistinguishable from a genuine early reset. Deleting an Anthropic token or a full vault reset removes the ledger row (a new ledger, which can warn again in the same calendar week); a Codex relink reuses the account row, so its ledger survives.
- **D17. Fix commands are copyable and safe (2026-11-22).** Every label-bearing fix command puts all flags before `--` and then the shell-quoted label (a valid label such as `--default` stays positional); `uzi token pool` always carries `--kind`. Grandfathered labels that are not command-safe get no command.
- **D18. A run is never listed twice on the Overview (2026-11-22).** The `limit` decision item is skipped for any run already shown on the "Needs your decision" card as a plan, question or owner park, not only an owner park, so the module 4 suppression rule is widened to satisfy M2's "a run is never listed twice". The credential row's run count therefore counts only runs not listed elsewhere; the run page banner still shows for every affected run.
- **D19. Near-limit blocks on other-window parks (2026-11-22).** A run parked on `five_hour` or a per-model window whose credential's overall weekly reading is near or at the limit gets the M2 near-limit block on web/CLI/TUI (the Slack park message is unchanged). Its wait is `unknown`, because its stored retry/reset stamps belong to the other window. Below the margin such parks render exactly as before.
- **D20. Implementation choices (2026-11-22).** The `--` command grammar and an always-present `--kind` (D17); a run-scoped `uzi run set-token <id> --auto` alongside worker auto; the warning fence instant is the exact persisted reading time (µs) compared in SQL; the observation and claim are separate transactions so a failed notification-row write never loses rearm evidence (D15); a pure step-0 validity gate runs before any transaction, so a rejected reading takes no lock and writes no row; the DM content is built before the claim transaction, which re-checks every gate.
