# PRD #2360: Park a Codex run on a usage limit and resume it at the window reset

**Status**: Draft
**Issue**: [#2360](https://github.com/vtmocanu/uzi/issues/2360)
**Priority**: High
**Related**: [PRD #35](done/35-run-limit-retry.md) and [ADR-0035](../adr/0035-run-limit-retry.md) (the `limit_wait` park this PRD extends), [PRD #1209](1209-codex-account-rate-limits.md) (Codex account meters; it excluded parking), [PRD #1590](1590-codex-quarantine-claim-hold.md) (the Codex account hold, a different trigger), #2099 (transient Codex provider retry), #1595 (unusable credential account mid-run, a different trigger), #2361 (follow-up: a persistent Codex `rateLimitExceeded` throttle).

## Problem

A Claude run that exhausts its owner's usage window parks in `limit_wait` and resumes on its own when the window resets. A Codex run that exhausts its subscription account's window **fails** instead: the owner has to notice, wait out the window by hand (days, for a weekly window) and start again. `wait_on_limit` (on by default) is never consulted for a Codex run, and the failure is recorded as a generic `agent_failure`.

Code path today (main at 504d24519):

- `agent/src/codex/terminal-normalize.ts:130-132` maps the app-server tags `usageLimitExceeded`, `rateLimitExceeded` and `sessionBudgetExceeded` to category `rate_limit`.
- `CodexHarness.decodeTerminal` (`agent/src/codex/codex-harness.ts:1943-2003`) sets no `limitEvidence` and its `materialize: (_limit)` ignores its argument, always producing `CodexTurnFailedError`. The Codex executor calls `materialize(undefined)` (`agent/src/codex/codex-executor.ts:3410-3413`).
- The runner's park branch requires `err instanceof LimitReachedError` (`agent/src/runner.ts:2666`). The generic failure path derives `fail_origin` only from `ProviderPolicyRefusal`, `TerminalReportError` or `failOriginForReason` (`runner.ts:3970-3976`), so a Codex limit reaches the API classless and is stored as `agent_failure`.
- `specs/human.md` (Bug #2099 section) states a Codex rate/usage limit "fails as before". This PRD changes that requirement.

## Outcome

A Codex **subscription** run whose turn dies on a **window** usage limit parks in `limit_wait` with a reset taken from Codex's structured rate-limit snapshot and resumes at the reset through the existing `limit_wait` lifecycle, on the same Codex account. Usage limits that do not clear with time fail with a typed `rate_limited` origin instead of `agent_failure`.

Acceptance examples (each an automated test at the seams named below):

1. **Window park.** A Codex issue run (`auth_mode = subscription`, `wait_on_limit` on) receives, in one turn, `account/rateLimits/updated` with `{"rateLimits":{"limitId":"codex","primary":{"usedPercent":41,"windowDurationMins":300,"resetsAt":T1},"secondary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":T2},"rateLimitReachedType":"rate_limit_reached"}}` (`T1`, `T2` epoch seconds in the future), then a non-retrying `error` and a failed `turn/completed`, both with `codexErrorInfo: "usageLimitExceeded"`. The worker reports `limit_wait` with `limit_resets_at = T2*1000` and `rate_limit_type = "seven_day"`; the run row is `limit_wait` with `retry_not_before = T2 + jitter`; the sweeper's `PromoteLimitWaitRuns` requeues it after that instant.
2. **Opt-out.** Same turn with `wait_on_limit` off: the run fails with `fail_origin = rate_limited` and `failure_reason = "Codex usage limit (seven_day) reached; resets at <T2 as RFC3339>"`.
3. **Not a window limit.** Each of these fails with `fail_origin = rate_limited`, never parks, and sends no limit fields (so the reason never promises a reset): the bucket's last `rateLimitReachedType` is `workspace_owner_credits_depleted` (or any value other than `rate_limit_reached`, including an unrecognized string); no window is exhausted and `rateLimitReachedType` is absent; no snapshot arrived in the turn; the run's `auth_mode` is `api_key`.
4. **Resume.** A promoted run is reclaimed with its approved plan (no second approval gate) and its work restored (uncommitted edits via the park sink's WIP marker). On a worker where the session resolves (no Docker attempt paths) it resumes the same Codex thread; on a Docker-wired worker (`attemptPaths = !!dockerHost`, `runner.ts:2172`) it takes the existing lineage-break path with work restored from the branch, tracking ref or checkpoint.
5. **Precedence.** A pending cancel or pause when the turn dies on a window limit wins: the run cancels or pauses and never reports `limit_wait`.
6. **No Anthropic leakage.** A parked Codex run on a worker whose Anthropic bind mode is `auto` gets no pool lowering, no `anthropic_rate_limits` gauge write, a NULL `limit_dead_secret_id`, and is absent from `ListLimitWaitReeval`.
7. **Account quarantined while parked.** A promoted Codex run whose account was quarantined during the park is held by the ClaimRun gate and parked `recovery_wait` / `codex_account_unavailable` (PRD #1590), not failed.
8. **`sessionBudgetExceeded`** keeps its current behaviour and origin.

## Facts this PRD relies on (resolved upstream; the implementing worker has no web access)

Read from upstream Codex at commit `01fc69f4026735edfdf6789820549727a4867b11` (the pinned 0.159.3), under `codex-rs/`:

- **`usageLimitExceeded` covers three causes** (`protocol/src/error.rs`, `to_codex_protocol_error`): `UsageLimitReached` (HTTP 429, body `error.type == "usage_limit_reached"`, `codex-api/src/api_bridge.rs`) is a window limit that clears at a reset; `QuotaExceeded` (insufficient quota, credit or spend caps) and `UsageNotIncluded` (plan) never clear with time. The tag alone cannot tell them apart.
- **Codex does not retry `UsageLimitReached`.** Before returning it, core stores the snapshot carried on the error **if the error carries one** (`core/src/session/turn.rs:1679-1684`) and emits a token-count event (`core/src/session/turn.rs`; `core/src/session/mod.rs` `update_rate_limits`), which the app-server forwards as `account/rateLimits/updated` **before** the terminal `error` (`willRetry: false`) and the failed `turn/completed`. When the 429 carries no snapshot, the turn's last snapshot is from an earlier sampling response. `rateLimitReachedType` is set from the `x-codex-rate-limit-reached-type` header when present.
- **No reset is on the error wire.** `TurnError` is `{message, codexErrorInfo, additionalDetails, misalignment}` (`app-server-protocol/src/protocol/v2/thread_data.rs`); the reset appears only in message text, which uzi never reads.
- **Snapshot wire shape** (`app-server-protocol/src/protocol/v2/account.rs`, camelCase): notification method `account/rateLimits/updated`, params `{ rateLimits: RateLimitSnapshot }`. `RateLimitSnapshot { limitId?: string|null, limitName?, normalModelSlug?, primary?: RateLimitWindow|null, secondary?: RateLimitWindow|null, credits?, individualLimit?, spendControlReached?, planType?, rateLimitReachedType?: string|null }`. `RateLimitWindow { usedPercent: int, windowDurationMins: int|null, resetsAt: int|null }`; `resetsAt` is absolute Unix epoch **seconds**. `rateLimitReachedType` values: `rate_limit_reached`, `workspace_owner_credits_depleted`, `workspace_member_credits_depleted`, `workspace_owner_usage_limit_reached`, `workspace_member_usage_limit_reached`. `limitId` defaults to `codex`; distinct `limitId`s are distinct quota buckets (`codex-api/src/rate_limits.rs` parses separate header families). Conventionally `primary` is the 300-minute window and `secondary` the 10080-minute window.
- **Merge semantics** (`core/src/state/session.rs:445-464`): a new snapshot **replaces** `primary`, `secondary` and `rateLimitReachedType` with its own values (null included); only descriptive metadata carries over. The notification is emitted per sampling response and once more from the 429's headers.
- **Which window blocked the turn is not on the v2 wire** (`limit_window_minutes` stays internal); it must be inferred from the snapshot.
- **`rateLimitExceeded`** is a short-term throttle that Codex retries internally (default maximum of 5 stream retries, `core/src/responses_retry.rs`, `willRetry: true` notifications). Handling a residual throttle is #2361, not this PRD.
- **`sessionBudgetExceeded`** is local: the `rollout_budget` feature's token budget for a session tree (`core/src/rollout_budget.rs`, `core/src/agent/control/budget.rs`), default off, sticky for the session. Waiting cannot clear it.
- **Resume** goes through ordinary `thread/resume`; nothing marks the thread failed after a usage-limit turn (`ext/goal/src/extension.rs` only stops an active goal).

## Out of scope

- **Server cross-check against the Codex account poller** (`codex_account_rate_limits`). Parked: this PRD parks only with a worker-observed reset or the bounded fallback, and the snapshot it reads is the turn's last one (usually from the terminal 429). A correct cross-check would need bucket identity carried to the server, frozen-account resolution, `generation`/`credential_revision` and enablement fencing, and moving the freshness rule out of `handler/codex_ratelimits.go` (which `workersvc` cannot import). Revisit only if live parks are seen promoting into a still-exhausted window.
- **A residual `rateLimitExceeded` throttle** after Codex's own retries: follow-up #2361 (it would use #2099's retry and `recovery_wait`, whose provider-outage path has no lifetime cap, `workersvc/sweep.go:293`).
- **Cross-account failover** while parked. The Codex binding is write-once and readmission is same-identity only (`codex_binding.sql`, ADR-1590).
- **Claim-time admission** of Codex runs on an exhausted account, and **early promotion** on the run's own account before the stamp.
- **Calling `account/rateLimits/read`** from the worker.
- **`sessionBudgetExceeded`** and **`api_key` quota exhaustion** beyond the `rate_limited` origin.
- Judge, job and chat lanes: `CodexAdviceHarness` is a separate class (`codex-advice-harness.ts:210`), job and chat are Claude-only, and `SetRunLimitWait` excludes `kind IN ('judge','job')`.
- A database migration: none is needed.

## Modules and seams

### Agent: Codex rate-limit observer and classifier (`agent/src/codex/`)

- **Decode.** `transport.ts` `decodeNotification` (~600-680) decodes method `account/rateLimits/updated` into a typed `rate_limits_updated` notification (still counted as liveness) instead of the generic `activity` fallthrough.
- **Observer** (pure, per turn, reset at each `turn/start`): keeps the latest snapshot **per `limitId` bucket** (`limitId` null or absent keys as `codex`), at most 4 buckets, applying upstream's replacement rule: a snapshot replaces that bucket's `primary`, `secondary`, `rateLimitReachedType` and `spendControlReached` (null clears). It also records whether the turn's **last** snapshot was accepted and, if so, which bucket it named. A snapshot is **not accepted** when `limitId` is present but not a string of 1-64 characters from `[A-Za-z0-9._:-]`, or names a fifth distinct bucket; a non-accepted last snapshot means **insufficient evidence** (no fallback to an earlier retained bucket). `limitId` is used only as an equality key and never echoed or logged.
- **Bounds (untrusted input).** Integers must satisfy `Number.isSafeInteger`. `resetsAt` is accepted only in [1e9, 4.1e9] seconds, then ×1000; a window with an invalid `resetsAt` has no reset. A negative or non-integer `usedPercent` invalidates the window; `>= 100` means exhausted. `windowDurationMins` must be 1..44640. `rateLimitReachedType` is kept as one of the five known values, `absent`, or `unrecognized` (never echoed). `spendControlReached` is kept only as `true`, `false` or absent. No other field (`limitName`, `planType`, `credits`, promo text) is retained. Memory is constant.
- **Classifier** (pure, unit-tested), at a failed terminal whose final classification is `usageLimitExceeded`, on the bucket named by the turn's last accepted snapshot:
  - `auth_mode` is `api_key` (`CodexAppServerAuthMode`, `appserver-auth.ts:33`), no snapshot this turn, the last snapshot not accepted, `spendControlReached` is `true`, or `rateLimitReachedType` is anything other than `absent` or `rate_limit_reached` (incl. `unrecognized`) → **non-window limit**.
  - Exhausted windows `E` (`usedPercent >= 100`) empty and `rateLimitReachedType` absent → **non-window limit**.
  - `E` empty and `rateLimitReachedType == rate_limit_reached` (rounding) → **window limit**, `E` := the window(s) with the **highest** `usedPercent` (ties keep all).
  - **Window limit**: if every window in `E` has a valid reset, `resetsAtMs` = the **latest** of them and `window` = that window's duration mapped `300 → five_hour`, `10080 → seven_day`, else `unknown`; if any window in `E` lacks a valid reset, no `resetsAtMs` (the server's fallback applies) and `window` from the window that has one, else `unknown`.
- **Seam reuse.** `decodeTerminal` fills `HarnessTerminal.limitEvidence` from the classifier (`explicitExhaustion: true`, `latest: {status: "rejected", resetsAtMs, window}`) for a window limit; `codex-executor` calls `materialize(classifyLimitEvidence(terminal.limitEvidence, now))` as `sdk-executor.ts:4050-4068` does; Codex's `materialize` builds `LimitReachedError({resetsAtMs, rateLimitType: window})` from its argument (as `claude-harness.ts:575-590`). `classifyLimitEvidence` drops a past reset, which leaves the server's fallback; that is intended.
- **Non-window limit**: `materialize` returns `CodexTurnFailedError` carrying a typed `failOrigin: "rate_limited"`; its message stays the closed, sanitized `codex turn failed: failed (usageLimitExceeded)`.
- **Precedence.** The executor's catch checks sticky cancel, pause, wall and vault-lock deferral before treating a `LimitReachedError` as a park, matching the #2099 transient arm (`codex-executor.ts` ~3570-3610).
- `LimitReachedError`'s message (`limit.ts:64`) becomes provider-neutral ("usage limit reached").

### Agent: runner (`agent/src/runner.ts`)

- The existing `LimitReachedError` branch (`runner.ts:2666-2850`) is reused unchanged: pre-report reap with the Codex per-sink reconcile while actively claimed, park sink (WIP marker, fetch-back, bridge, checkpoint publish), `limit_wait` report with `checkpoint_contains_latest`, or `failed` with limit fields when `wait_on_limit` is off.
- `reportGenericFailure`'s origin derivation (`runner.ts:3970-3976`) honours a `CodexTurnFailedError.failOrigin` the way it honours `TerminalReportError.failOrigin`. `rate_limited` is already a valid worker-reported origin (`failorigin.go:196`).

### API (`api/internal/workersvc/`, `api/internal/store/queries/runtime.sql`)

- **No new park policy branch.** A Codex run never records `anthropic_secret_id` (claim assembly skips the Anthropic ladder for `harness = codex`, `claim_assembly.go:279-288`), so `setLimitWait` already fetches no candidates and writes no gauge (`limitwait.go:557,649`), `NextAvailable` refuses `uuid.Nil` (`autoselect/available.go:92`), `limit_dead_secret_id` stays NULL, and `ListLimitWaitReeval` requires `limit_dead_secret_id IS NOT NULL` (`runtime.sql`, `name: ListLimitWaitReeval`). `uzi run set-token` refuses Codex (`run_credential.go:102-116`). Tests pin each of these (example 6) so a later change cannot silently break them.
- **Provider-aware reason.** `limitFailureReason` takes the provider from the run's harness ("Codex usage limit" / "Anthropic usage limit"). `limitAwareFailureReason(req)` gains the run (or its harness) so the failed arm (example 2) is provider-aware too.
- **Codex capability at park.** `SetRunLimitWait` revokes the Codex capability (`codex_cap_hash = NULL`, `codex_claim_epoch + 1`) when `harness = 'codex'`, as a `CASE` inside that one statement (not a second query) so park and revoke stay atomic under its generation and `claim_released_at IS NULL` fences, as `PromoteLimitWaitRuns` does on promotion (`runtime.sql:~2498-2503`). The park sink's boundary reconcile has already run before the report (`runner.ts:2675-2680`), so nothing legitimate needs the capability afterwards; a late refresh or release from the parked flight is refused. Today `limit_wait` is in `codexActivelyClaimedStatuses` (`codexauthz.go:71`) and the capability would otherwise stay live for up to `RUN_LIMIT_MAX_PARK` (8 days).
- **`wait_on_limit`.** The existing per-user default (on) and per-run override govern Codex parks too; no new setting. `docs/run-limit-wait.md` states the cost (issue lock and run-bound worker held for up to `RUN_LIMIT_MAX_PARK`).

### Web, CLI, docs, spec

Name the provider from `RunDTO.harness` (`apitypes/run.go:171`) where the run is at hand, else use provider-neutral "usage limit". Sites:

- `web/src/lib/runBadge.ts:612`, `web/src/components/RunEvent.tsx:1025-1026`, `web/src/components/ActivityFeed.tsx:867-869`, `web/src/lib/limitWait.ts`
- The reused setting's labels, provider-neutral since they now govern both: `web/src/pages/RunDefaults.tsx:534` (card title "Anthropic usage limits" → "Usage limits") and `web/src/pages/RunView.tsx:942-949` (the per-run "Wait out future Anthropic usage limits on this run" toggle and its two read-only variants); update `docs/run-limit-wait.md:15-16`, which quotes them
- `api/cmd/uzi/run_render.go:2201` ("waiting: Anthropic usage limit") and `api/internal/uzicli/skill/SKILL.md:351`
- `docs/run-limit-wait.md` (incl. a Codex section: what parks, what fails, no token switching while parked, the Docker lineage-break resume), `docs/cli.md:2268`, `docs/configuration.md:247`, `docs/run-health.md:140`; then `task docs:sync`
- `ARCHITECTURE.md:642,670-671`
- `specs/human.md`: replace the Bug #2099 "Codex rate/usage limit fails as before" clause, tagged `(AI-synced YYYY-MM-DD)`
- `CHANGELOG.md` `[Unreleased]`

## Risks

- **Rollout skew.** A new worker against an old API still parks (`setLimitWait` does not check the harness) but with "Anthropic" wording, and without the capability revocation. An old worker against a new API behaves as today. No capability gate is needed. The hosted fleet gains this only after a worker-tag roll.
- **Long holds.** A weekly Codex park holds the issue's one-active lock and, for a run-bound ephemeral hosted worker, the worker and its PVC (`limit_wait` is non-terminal, `hosted_workers.sql:194`) for up to 8 days. Accepted by maintainer decision (Decision 11).
- **Judge and dashboards.** Codex limit failures move from `agent_failure` to `rate_limited`, which is classed transient and is not judged (`judge_enqueue.go:26,233`), and they move on the failed-run dashboard. Intended.
- **Snapshot provenance residual.** The shared `usageLimitExceeded` tag cannot prove the terminal 429 carried the snapshot the classifier reads (`core/src/session/turn.rs:1679-1684` updates rate limits only when the error carries one). A terminal `QuotaExceeded` or `UsageNotIncluded` preceded in the same turn by a sampling snapshot that already shows an exhausted window (and no rejection type or spend-control flag) would park instead of failing. Bounded: such a run re-hits the same error on resume and is failed by `RUN_LIMIT_MAX_WAITS` or `RUN_LIMIT_MAX_PARK`, and a turn that saw an exhausted window would normally have died on `UsageLimitReached` itself. Accepted rather than adding a request path; the `spendControlReached` and `rateLimitReachedType` rejections narrow it.
- **No reset observed** (both windows exhausted, one reset invalid; or `rate_limit_reached` with no valid reset): the exponential fallback (15m doubling, cap 4h) with `RUN_LIMIT_MAX_WAITS` (default 5) can fail a weekly park after ~7.75h. Accepted; the poller cross-check is the remedy if it is seen live.

## Testing decisions

- **Observer and classifier** (agent unit tests beside `agent/test/codex-harness.test.ts:2286-2295`, which today pins `usageLimitExceeded` to a thrown failure): replacement semantics incl. a later snapshot nulling an exhausted window or the type; interleaved buckets; last-bucket selection; a malformed or oversized `limitId` and a fifth-bucket last snapshot (insufficient evidence, never an earlier bucket); `spendControlReached: true`; the rounding branch picking the highest-`usedPercent` window (primary 99% resetting in 2h, secondary 60% in 6 days → parks on the 2h reset); partial resets; `rate_limit_reached` with no window at 100; unrecognized type; out-of-range and unsafe integers; seconds-to-ms; `api_key`; no snapshot; `sessionBudgetExceeded` unchanged.
- **Executor and runner** (`agent/test/codex-executor.test.ts` fake app-server, driving the real decoded terminal, not an injected `LimitReachedError`): window limit reports `limit_wait` with the reset and runs the park sink; opt-out reports `failed` with limit fields; non-window reports `failed` with `fail_origin = rate_limited` and no limit fields; cancel/pause precedence; checkpoint publish failure reports no `checkpoint_contains_latest`; a non-Docker reclaim resumes the same thread.
- **API** (`limitwait_test.go` and `*LiveDB`, following `promote_limit_wait_now_parity_test.go`): provider-aware reasons on both arms; example 6's structural exclusions; the park-time capability revocation, a refused late capability op, and post-park custody settlement still succeeding; a stale claim generation's park rejected; promote then #1590 quarantine hold (example 7); an approved run promoted from `limit_wait` resumes without a new gate.
- **Discipline.** Each bug-specific regression test is observed failing on main and passing with the fix (record both in the PR). Supporting negative cases may already pass on main.
- **Gates**: `task gate:agent`, `task gate:api`, `task gate:web`, `task gate:repo`, `task check-docs:web`, and the live-DB lane `./e2e/run-store-it.sh` with the new `*LiveDB` tests named in the output as `--- PASS` and zero `--- SKIP`.
- No file under `.github/workflows/` is created or changed, by implementation or validation.

## Milestones

### M1: A Codex window-limit death parks, resumes at the reported reset, and other usage limits fail as `rate_limited`

**Blocked by**: none.

Everything in Modules and seams, with its tests, docs, spec and changelog.

Acceptance: examples 1-8 pass as tests at the named seams; the gates above are green.

## Acceptance (live, not a milestone)

Acceptance issue #2362 (owner: maintainer, no sweep label) checks on a hosted deployment that a real Codex usage-limit death parks with the provider's reset, survives a worker roll while parked, and resumes after the reset (same thread on a non-Docker worker; lineage-break with work restored on the Docker lane).

## Decision Log

1. **Park only window limits, identified from the structured snapshot.** `usageLimitExceeded` also covers quota and plan failures that never clear; parking those would hold the issue and the worker until the wait budget fails them anyway. Rejected: park on the tag alone.
2. **Fail closed to `rate_limited` on missing or unrecognized evidence.** No snapshot, an unknown `rateLimitReachedType` or `api_key` auth cannot prove a reset exists. Rejected: park with the fallback (holds an issue for a limit that may never clear).
3. **Worker-observed snapshot is the only reset source.** It is the only structured reset Codex exposes, emitted for the same account, from the terminal 429 when that error carries one and otherwise from the turn's latest sampling response (see Risks). Rejected: message-text parsing (uzi never reads provider prose); `account/rateLimits/read` (extra request path, auth-mode caveats); the poller cross-check (see Out of scope).
4. **Per-bucket, replacement semantics, last bucket wins.** Mirrors upstream's own merge (`core/src/state/session.rs:445-464`) and keeps unrelated quotas apart. Rejected: per-field latest-wins (can retain a cleared exhaustion or mix buckets).
5. **Latest reset among exhausted windows; no reset when any exhausted window lacks one.** The turn cannot run until every exhausted window reopens; a partial reset would guarantee a re-park. Rejected: earliest reset.
6. **Reuse the `limitEvidence` → `classifyLimitEvidence` → `materialize` seam.** One classification path for both harnesses. Rejected: deciding inside Codex's `materialize` (a parallel path).
7. **Reuse `five_hour`/`seven_day`/`unknown`.** They name the window, not the vendor; the migration 00091 CHECK and the UI already accept them. Rejected: new `codex_*` values (migration and UI work for no new information).
8. **No server policy branch; pin the structural exclusions with tests.** The Anthropic ladder is already skipped for Codex at claim assembly, which makes every Anthropic step in the park path inert. Rejected: a duplicate harness branch.
9. **Same-account resume only.** The frozen binding forbids switching accounts.
10. **`RUN_LIMIT_MAX_PARK` caps one park's computed delay** (8 days by default, enough for a weekly window); `RUN_LIMIT_MAX_WAITS` caps the number of parks.
11. **Reuse `wait_on_limit` and its default (maintainer decision).** One recovery preference across providers is the consistency asked for. Rejected: a separate Codex default (a setting, a migration and UI for a preference the owner already states).
12. **Revoke the Codex capability atomically with an accepted park.** Provider persistence and reconciliation finish before the report; post-park custody settlement uses forge credentials, not the capability. A live capability for up to 8 days is avoidable exposure. Rejected: keep it live until promotion. Requires a regression proving post-park settlement still succeeds and the old capability is rejected immediately.
