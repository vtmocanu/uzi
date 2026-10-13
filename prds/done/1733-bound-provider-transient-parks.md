# PRD #1733: Bound provider-transient recovery parks, then hand the run to its owner

**Issue:** [#1733](https://github.com/vtmocanu/uzi/issues/1733) (also implements [#2361](https://github.com/vtmocanu/uzi/issues/2361))
**Epic:** [#2746](https://github.com/vtmocanu/uzi/issues/2746)
**Status:** Done (2026-10-12). Acceptance examples 1–3, rollout controls and implementation milestones are evidenced below. Required API and web gates passed on the documentation candidate; prior agent, repo and LiveDB evidence remains attributed to its tested revision. Hosted observation remains an opportunistic follow-up.
**Execution:** one gated uzi run, one PR. Continue on the published working branch, preserving `ca44b5c1d0b55ac215ba784b10ee7acbbf55e4c3`; merge default revision `e219759078f1d71dae616ee77b7813d21fb9df61` without rewriting history, and never write to `main`. Do not touch `.github/workflows/**` in implementation or validation. M1 needs LiveDB evidence through `./e2e/run-store-it.sh`, so the run needs a Docker-capable worker (this repo's `required_capabilities` already asks for one). #1324 (forge outage at finalize) reuses this PRD's bound in a later PRD and is out of scope here.

## Problem

The following records the pre-implementation behavior, read on `main` `3e9c620c` and re-checked against `cc4ed18`. It is historical context, superseded by the implementation/evidence section below.

A provider error the harness classifies as transient parks the run in `recovery_wait` with no lifetime cap.

- **Claude:** `isProviderTransient` (`agent/src/sdk-executor.ts`) treats a failed `api_error` turn as transient when its status is missing, 408, 429 or 5xx, and since #1775 also a non-`api_error` reason that carries a 5xx, 408 or 429 status. After bounded in-process retries the executor throws `TransientRecoveryError` with the last provider error.
- **Codex:** `isCodexTransientClassification` (`agent/src/codex/terminal-normalize.ts`) accepts `transport` classifications; `codex-executor.ts` retries in process and then throws the same `TransientRecoveryError` (#2099).
- **The error class is shared with non-provider causes.** `sdk-executor.ts` also throws `TransientRecoveryError` for a positively empty turn (no provider error recorded), and `runner.ts` throws it for `RESUMED_GATE_RECOVERY_REASON` and `GATE_PRESENTATION_REFUSED_REASON`.
- The runner's `TransientRecoveryError` arm calls `handleRecoveryExhausted` (`agent/src/runner.ts`), which reports `{status: "recovery_wait"}` with no `recovery_cause`. The server parks it with a single fenced UPDATE, `setRecoveryWait` → `SetRunRecoveryWait` (`api/internal/store/queries/runtime.sql`; fence `status='running'`, `kind NOT IN ('judge','job','cross_check')`, `claim_released_at IS NULL`, nil-guarded `claim_generation`). `recoveryCauseStored` (`api/internal/workersvc/recoverywait.go`) persists only `vault_locked` and stores NULL for every other cause, although `provider_outage` is already in the wire vocabulary (`recoveryWaitCauses`) and in `runs_recovery_wait_cause_check`. Any api advertising `recovery_park_cause` accepts the token today.
- `PromoteRecoveryWaitRuns` re-queues the run at `recovery_retry_not_before` and resets `started_at = NULL` and `budget_paused_seconds = 0`, so **the wall clock is never a backstop**. Backoff: `recoveryParkFallbackFor`, doubling from `RUN_RECOVERY_PARK_BASE` (1m) to `RUN_RECOVERY_MAX_PARK` (30m), plus 5-30s jitter.
- `recovery_wait_count` is shared with the empty-turn path and only shapes backoff. The only capped parks are forge-unreachable and data-volume-full, which fail the run at their caps.
- **Codex `rateLimitExceeded` (#2361):** `CODEX_ERROR_INFO_MAP` (`terminal-normalize.ts`) maps it to category `rate_limit`, which the transient predicate rejects, so a persistent throttle fails the run through the generic path.

A permanent-but-status-less error, or a multi-day provider outage, therefore keeps the run cycling indefinitely (issue lock, worker slot and disk, custody) with generic "recovering" wording, until the owner notices.

## Outcome

Every provider-transient park is typed and counted, and an unbroken streak of them is bounded in elapsed time. At the bound the run stops retrying and waits for its owner as a `paused` hold, offering the owner Resume, Stop (where the existing run-kind and completed-work rules permit finalization, with its existing authorization and completion-permit requirements) or Cancel; an unavailable action explains why. The policy never fails a run.

Acceptance examples:

1. **Bound reached.** A Claude run keeps getting status-less `api_error` failures. Each park is `recovery_wait` with cause `provider_outage`; `uzi run get` shows "provider outage", the park count, the streak start and the next retry. At the first provider park after 24 h of streak, the run becomes `paused` with `hold_reason = 'provider_unavailable'`, keeps its worker affinity, and `uzi run get` lists Resume, Stop (if permitted) and Cancel. `uzi run resume` re-queues it with the streak cleared.
2. **Progress resets the streak.** A run parks twice for provider errors. On its next claim it completes two productive turns, then hits a provider outage again and parks. That park report says the flight completed productive turns, so a new streak starts at this park; the run is not held even if the first park was more than 24 h ago.
3. **Codex throttle.** A Codex turn failing with classification token `rateLimitExceeded` is retried in process, then parks as `provider_outage` under the same bound. `usageLimitExceeded` still takes the usage-limit park (`limit_wait`, #2360) and `sessionBudgetExceeded` keeps its current handling.

## Out of scope

- #1324 (forge outage at finalize): a later PRD reuses this bound.
- Empty-turn parks and the gate-recovery `TransientRecoveryError` callers: they stay untyped, uncounted and uncapped.
- Forge-unreachable and data-volume-full parks keep their own caps.
- The backoff schedule and the in-process retry counts.
- `limit_wait`, `pool_wait`, `codex_account_unavailable` holds.
- Promoting overdue `recovery_wait` rows directly into the hold (the bound is evaluated only at a park).

## Modules and seams

**1. Provider origin marked at the producer (agent).**
- `TransientRecoveryError` gains an explicit provider-origin marker, set only at the two provider producers: the `sdk-executor.ts` site when a provider error was recorded (`lastProviderErr` present) and the Codex transient site. The empty-turn throw and the runner's gate-recovery throws leave it unset and keep today's behaviour exactly.
- The flight also counts its **productive turns**: model turns of this execution, in any phase (planning, implementation, review), that completed without error. For Codex a turn is one `turn/start` that reached `turn/completed` without a failed status; for Claude one SDK query that returned a result without error. It is a per-flight, in-memory count starting at 0 on every claim, carried to the runner on the `TransientRecoveryError` together with the provider-origin marker, and reported only on the provider park, so it needs no persistence or deduplication.

**2. Worker park report and acknowledgement (`agent/src/runner.ts` `handleRecoveryExhausted`).**
- On a provider-origin park, and only when the api advertises the new protocol feature `recovery_cause_provider_outage` (meaning: this api stores, counts and bounds the cause, and may answer `paused`), the report carries `recovery_cause: "provider_outage"` and `productive_turns: <n>`. Otherwise it sends today's untyped body. Retry once with the untyped body and disable the cached feature only on a positively identified compatibility refusal, such as an older strict decoder rejecting the unknown `productive_turns` field. Preserve claim generation and every existing authorization field. Other 400 responses retain normal error handling; they must not silently disable provider accounting.
- Before sending a typed provider park, the runner runs the credential-requiring provider settlement it would otherwise run after the ack (`reapRecoveryProviderForSettle`, as the cancel arm already does), because a Codex reap is refused once the run is `paused` (`paused` is not in `codexActivelyClaimedStatuses`, `api/internal/workersvc/codexauthz.go`; the #1531 custody-leak class). On uncertainty it retains custody, as today.
- An accepted `paused` / `provider_unavailable` ack is a successful park disposition: preserve clone and HOME before settlement, retire the finalize record with the same reason as an accepted `recovery_wait`, return true, and never route through generic failure or ordinary teardown. A lost ack followed by ownership confirmation produces the same result. Call settlement only after positive pre-report reap/quiescence proof. `settleRecoveryGeneration` is not inherently credential-free: its legacy branch fetches with the forge PAT. Explicitly select a credential-free guarded path, or retain custody without settlement when that path is unavailable. A false or unknown reap result never permits settlement or cleanup. Apply the same proof bookkeeping to the `recovery_wait` ack without re-reaping a poisoned or disposed provider.

**3. Server park accounting (`api/internal/workersvc`, `runtime.sql`).**
- Migration `00328_provider_park_streak.sql` (renumbered at landing above `00327_code_cross_check_guidance.sql`) adds `provider_park_count integer NOT NULL DEFAULT 0` and `provider_park_streak_started_at timestamptz NULL`.
- A typed provider park is routed to a dedicated arm, shaped like `parkDataVolumeFull` (a transaction that locks the run row, re-checks the exact `SetRunRecoveryWait` fence, then writes with `RETURNING`), with `@now` passed from `s.now()` so tests cross the bound without sleeping. From that one locked snapshot it computes:
  - streak: a new streak (start = now, count = 1) when none is open or `productive_turns > 0`; otherwise count + 1, saturating at the column maximum;
  - outcome: if the bound is enabled and `now - streak_started_at >= bound`, the hold; else the ordinary `recovery_wait` park with the stored cause `provider_outage` and today's backoff.
- **The hold** mirrors the worker wall park `SetRunWallPark`, not the server sweep: `status='paused'`, `hold_reason='provider_unavailable'`, `claim_released_at` stays NULL, `worker_id` is kept, no `worker_active_runs` rows are deleted (a verified but unpublished capture may exist only on that worker, and releasing the claim would null `worker_id` on resume). It clears the `pause_*` columns and, in the same statement, settles **every** unapplied `kind='pause'` input (any body: `wall`, `now`, `milestone`), extending `SetRunWallPark`'s `consumed_wall` pattern, so a resumed flight is never handed a stale abort or re-parked. The hold counts as the owner's pending pause being satisfied: the run is already stopped and waiting for the owner. This departs from `SetRunPaused`'s documented rule that involuntary parks keep `pause_*`, because here the involuntary park is itself an owner-facing pause. It does not require a pending pause request (the hold is involuntary).
- A fenced-out, stale or duplicate park report changes nothing, including the counters. Typed accounting requires a positive claim generation on the report; the nil-generation compatibility path must never become authority for a new streak or a progress reset.
- **Never failed by a late report:** protect `provider_unavailable` while paused and through the resumed queued interval in the failure, supersession and question/follow-up guards in `runtime.sql`. Fresh claim clears the marker and advances the generation; a late report from the old worker must not overwrite the owner-resumed run.
- **Resume:** `ResumePausedRun` already resumes any paused run; for `provider_unavailable` it also clears both streak columns. `SetRunRunning` clears `hold_reason` on the next claim, as today. Cancel uses the existing paused-run cancel path. **Stop is the existing two-step:** the owner sets the run's scope ceiling with `uzi run stop` (milestone mode, applying the existing completed-work scope and authorization rules), then resumes; the worker finalizes at the ceiling under the existing completion-permit and budget rules. No new atomic Stop path is added; the hold's surfaces show the two-step, and a run kind that cannot be scoped shows why Stop is unavailable. Stop is offered only when at least one milestone is complete (mirroring `ErrWallParkStopUnavailable`); otherwise the surfaces explain why. If the finalize after Stop itself needs the provider and hits another outage, it parks again with a fresh streak: accepted behaviour. Test the two-step through the real endpoints. A refused resume leaves every column unchanged. Update `TestPromoteRecoveryWaitNowMirrorsSweepPass` to permit only the owner-driven streak reset as an explicit exception while still pinning every pre-existing common mutation and preserved column; automatic promotion and vault-unlock promotion preserve provider streak state. An owner credential change that promotes a parked run early (`PromoteRecoveryWaitRunNow`, the set-token path) and a **credential switch** during a streak both clear the streak (an owner action is a fresh attempt).
- **Precedence with the wall clock:** a provider park on a run already past its wall becomes `provider_unavailable`, not `budget_exhausted`; the owner's Resume then skips the budget check as for any non-budget hold, and the worker's own wall still applies to the resumed flight. Accepted: the owner is asked either way.
- **Bound evaluation timing:** at the next qualifying park, not at the exact deadline. Detection can lag by backoff plus jitter, queue and claim delay and the next attempt; no maximum overshoot is promised.

**4. Codex classification (`terminal-normalize.ts`).** The transient predicate also accepts a failure whose classification token is exactly `rateLimitExceeded`. It keys on the token, never on the `rate_limit` category, which `usageLimitExceeded` and `sessionBudgetExceeded` share. The resulting `TransientRecoveryError` carries the provider-origin marker.

**5. Config.** `RUN_PROVIDER_PARK_MAX_DURATION` (Go duration, default `24h`; `0` disables the hold, while typing and counting stay on), parsed beside `RUN_RECOVERY_MAX_PARK` in `api/internal/config/config.go`, documented in `docs/configuration.md`.

**6. Owner surfaces.**
- API: `apitypes/run.go` and `api/internal/handler/runs_dto.go` add `provider_park_count` and `provider_park_streak_started_at`; resume hints in `api/internal/handler/resume.go`; DTO contract fixtures and their web mirrors (`web/src/lib/apiTypes.ts`).
- CLI and TUI: `api/cmd/uzi/run.go` hold constants, `run_get.go`, `run_render.go` recovery suffix, `run_wait.go`, `tui_board.go`, `tui_board_rows.go`, `tui_detail.go`, `tui_render.go`.
- Web: `web/src/lib/runBadge.ts`, `web/src/pages/RunView.tsx`, `web/src/components/LastRun.tsx`, `web/src/mocks/data/runs.ts`.
- Slack: `api/internal/slacksvc/notifier_state.go` (`recovery_wait` and paused cases).
- Docs: `docs/run-recovery-wait.md` (qualify the former uncapped wording to empty-turn and resumed-gate recovery), `docs/run-pause.md`, `docs/cli.md`, `docs/run-health.md`, then `task docs:sync`.

Resource bounds: the one new input is `StateRequest.ProductiveTurns *int32` (`api/internal/workersvc/service.go`; the decoder uses `DisallowUnknownFields`, so the field must be declared). It is accepted only with `status=recovery_wait` and `recovery_cause=provider_outage` and answered 400 otherwise; a negative value is rejected, not clamped; the cause is validated against `recoveryWaitCauses` as today. The bound itself is the resource limit this PRD adds.

**Rollout (api first, mixed fleet).** Old workers keep sending untyped parks, which the server stores as today and never converts. A new worker against an old api sees no feature and sends untyped parks. Only a new worker against a new api sends typed parks, and that worker handles the `paused` ack. Feature absence disables new typed reports only after protocol negotiation refreshes; do not assume an api rollback instantly removes a feature cached by an existing worker. Test a cached-feature, new-field report against an older strict decoder: that compatibility refusal triggers the single untyped re-send above and disables the cached feature. Existing provider holds rely on the late-failure guard, which an older api lacks, so rollback is not declared behaviourally safe merely because the older api recognizes `paused`.

## Testing decisions

- **LiveDB (`api/internal/workersvc/`), following `forgepark_livedb_test.go`, `diskpark_livedb_test.go` and `recoverywait_livedb_test.go`:** typed park stores `provider_outage` and counts; streak past the bound converts to `paused`/`provider_unavailable` with `claim_released_at` NULL, `worker_id` kept and active-run rows intact; a pending `wall`, `now` or `milestone` pause input is consumed and `pause_*` cleared, and after resume the next `ConsumeRunInputs` returns no pause; bound `0` never converts; `productive_turns > 0` starts a new streak; untyped and `empty_turn` parks never touch the streak; a fenced-out or duplicate report changes nothing; a late same-generation `failed` report does not fail the hold; resume clears the streak, a refused resume changes nothing; early promote and credential switch clear the streak. Each fails on unfixed code (mutation discipline, `.claude/rules/go.md`). `TestRecoveryWaitCauseVocabularyMatchesCheck` stays green.
- **Protocol:** `api/internal/handler/protocol_features_test.go` lists `recovery_cause_provider_outage`.
- **Agent (real runner paths, both harnesses):** the marker is set only by the two provider producers (negative controls for the empty-turn throw, `RESUMED_GATE_RECOVERY_REASON` and `GATE_PRESENTATION_REFUSED_REASON`); the typed body is sent only with the feature; an older strict decoder's unknown-field refusal triggers one untyped re-send and disables the cached feature, while a new api rejecting an invalid typed request with 400 causes neither a re-send nor feature disablement (negative control); `productive_turns` counts only completed turns (a failed turn and a plan turn that errored do not count); provider settlement runs before the typed report; a `paused` ack settles credential-free, retires the finalize record, preserves the clone and returns true; a lost ack plus ownership confirmation behaves the same; Codex `rateLimitExceeded` retries then parks typed, `usageLimitExceeded` still throws `LimitReachedError`, `sessionBudgetExceeded` unchanged (follow the transient cases in `agent/test/codex-executor.test.ts`).
- **Surfaces:** CLI, TUI, web and Slack render tests following the existing `budget_exhausted` and forge-park tests; DTO contract fixtures.
- **Gates:** `task gate:api` (with `UZI_TEST_DATABASE_URL` unset), `./e2e/run-store-it.sh` with the new LiveDB tests reported as run, `task gate:agent`, `task gate:web`, `task gate:repo`.

## Milestones

**M1: Provider parks are typed, counted and bounded, with a safe hold (server + worker + CLI).** Blocked by: none.
Provider-origin marker and productive-turn count; feature-gated typed report; pre-report provider settlement; `paused` ack disposition; migration; dedicated locked park arm with streak, hold, pending-request consumption and late-failure guards; resume, early-promote and credential-switch streak clearing; config; protocol feature; DTO fields and contract fixtures; CLI `run get` / `run wait` / render with Resume, Stop and Cancel hints; `docs/run-recovery-wait.md`, `docs/configuration.md`, `docs/cli.md`; `task docs:sync`; CHANGELOG.
Acceptance: examples 1 and 2 at the LiveDB and agent seams for both harnesses; every negative control above; the rollout matrix (old worker / new api, new worker / old api, new / new).

**M2: Codex `rateLimitExceeded` takes the bounded park (#2361).** Blocked by: M1.
Token-keyed predicate; tests for the three `rate_limit` tokens.
Acceptance: example 3.

**M3: Web, TUI and Slack show the typed park and the hold.** Blocked by: M1.
Badge and RunView wording for `provider_outage` parks (count, streak start, next retry) and the `provider_unavailable` hold with Resume, Stop and Cancel and an explanation for an unavailable action; TUI board and detail; `LastRun`; mock data; Slack messages; `docs/run-pause.md`, `docs/run-health.md`.
Acceptance: render and notifier tests; mock-mode RunView shows both states.

A real multi-hour provider outage cannot be produced on demand; once this merges, an opportunistic line is added to the hosted acceptance issue #2747. Not a milestone.

## Implementation and evidence lifecycle

I0–I3b code is committed and reviewed on the current branch through
`e26b8b2e696bebceadadf0a248041708250981d9`: server accounting/hold and
worker custody/CLI (I0/I1), exact Codex throttle classification (I2),
web owner surfaces (I3a), and Slack/TUI (I3b). This records branch
implementation, not release publication or completed final validation.
Documentation and recorded decisions now describe the implemented contract;
embedded docs are synced by `task docs:sync`. The checklist records
completed implementation and required validation:

- [x] M1: typed, counted and elapsed-bounded provider parks; safe hold, resets, CLI and rollout controls.
- [x] M2: exact Codex `rateLimitExceeded` retry/park path with usage/session negative controls.
- [x] M3: web, TUI and Slack provider retry/hold presentation and owner actions.
- [x] Documentation, recorded decision reconciliation and embedded docs sync.
- [x] Required `task gate:web` passes without diagnostic timeout changes (`b80e69c6`, exit 0).

The completed implementation checklist permits moving this PRD to `prds/done/`. Hosted multi-hour observation is outside these milestones.

Acceptance examples 1–3 have handler/LiveDB and executor/runner evidence:

- **Examples 1 and 2:** `api/internal/workersvc/providerpark_livedb_test.go`
  drives real service/database transitions in
  `TestProviderParkTypedStoresCauseAndCountsLiveDB`,
  `TestProviderParkPastBoundHoldsLiveDB` and
  `TestProviderParkProductiveTurnsStartNewStreakLiveDB`.
  `TestProviderHoldResumeClearsStreakLiveDB` and
  `TestProviderParkOwnerPromoteClearsAutomaticPreservesLiveDB` exercise
  resets. `api/internal/handler/provider_hold_stop_livedb_test.go` drives
  real handlers in `TestProviderHoldStopThenResumeLiveDB` and
  `TestProviderHoldStopWithoutCompletedMilestone409LiveDB`.
- **Both harnesses:** `agent/test/runner-provider-outage-park.test.ts`
  runs Claude and Codex cases for typed reports with productive turns,
  positive-proof settlement, refused reap, paused acknowledgements,
  clone/HOME retention and lost-ack ownership confirmation. It also covers
  untyped empty-turn/gate controls, feature absence, strict-decoder fallback
  and current-API validation refusal. These use simulated API/transport and
  quiescence evidence, not a live hosted outage.
  `agent/test/sdk-executor.test.ts` covers “marks a sustained provider
  outage providerOrigin” and “notifies onModelTurnSettled once per COMPLETED
  turn: the failed provider attempt is not counted”.
  `agent/test/codex-executor.test.ts` covers planning settlement (#1733)
  and failed planning turns not counting as productive.
- **Example 3:** `agent/test/codex-executor.test.ts` includes
  “rateLimitExceeded exhaustion from the real executor reaches the runner
  typed provider_outage park”, exact retry/exhaustion cases and unchanged
  usage/session token controls. The classifier discrimination and retry
  path are exercised; this is not exhaustive hosted interruption proof.

Recorded validation (completion handoffs I2, I3a and I3b under local
scratch; scratch records are not shipped documentation):

- **API and repo:** required gates passed at `e26b8b2e`, exit 0.
  Full LiveDB harness at that ref passed **2631 PASS, zero skips**, exit 0,
  including provider accounting/hold, custody, Stop/Resume and
  `TestGetSlackRunContextProviderParkLiveDB`. The harness used
  `GOFLAGS=-timeout=20m` for its package-wide timeout, without stretching
  individual assertions.
- **Agent:** required gate passed at `de6db2e6`, exit 0; agent source
  remains unchanged through `e26b8b2e`. This is prior evidence, not an
  agent gate rerun at the latest ref.
- **Web:** required full gate **failed** at `8d791fb5`, exit 201, on
  `App.routes`' `beforeAll` 10s hook timeout. The same focused failure
  reproduced on base `de6db2e6`. A scratch configuration with a 30s hook
  timeout passed the seven focused route tests as a diagnostic only;
  browser tests passed (14 tests, four files, exit 0). Neither result makes
  the required web gate passed. Web source is unchanged through
  `e26b8b2e`; this was the concrete blocker before the I4 rerun.
  Required `task gate:web` at `b80e69c6` subsequently passed, exit 0,
  using the normal timeout settings, including all 14 browser tests.
  This closes the required web-gate blocker without changing the test.
  Required `task gate:api` also passed at `b80e69c6`, exit 0,
  with `UZI_TEST_DATABASE_URL` unset.
- **Unexecuted:** hosted `codex-refresh-lostreply-e2e` and
  `agent:worker-uid` lanes. Real hosted multi-hour outage observation is
  opportunistic outside this run, to be recorded after merge on #2747;
  it is not a completed milestone or acceptance observation.

Slack delivery remains best-effort: crash or failed marker persistence can
duplicate a notification; exactly-once delivery was not proved. Web action
proof uses mocked components rather than live server/runtime recovery.
The required local gates now have passing evidence at the revisions
recorded above; this does not claim hosted recovery has been validated.

## Decision Log

- **2026-10-11, maintainer:** bound all provider-transient parks by elapsed streak time (default 24 h, configurable, `0` disables); at the bound an owner hold, never a failure; same for both harnesses; typed cause and separate counter; #2361 routes Codex `rateLimitExceeded` in. Recorded on #1733.
- **Stored cause token is the existing `provider_outage`.** No CHECK widening or vocabulary drift. The decision's name `provider_transient` describes the class. Rejected: a new token.
- **Provider origin is marked at the producer, not inferred from the error class.** `TransientRecoveryError` also covers empty turns and gate recovery, which must stay untyped. Rejected: typing every `TransientRecoveryError`.
- **Progress is reported by the parking flight (`productive_turns` on the park report), not read from `iteration_count`.** Both executors restart their iteration at 0 per execution and report it before the turn runs, and the server keeps `GREATEST`, so `iteration_count` neither measures completed work nor rises on a resumed attempt below the old maximum. A per-flight count on the fenced park report needs no persistence or deduplication: a stale or replayed report is fenced out. Old workers send none, which means no reset, the conservative direction. **Amendment (PR #2797, fenced progress on non-provider parks):** a flight can complete turns and then park for a non-provider reason, which would leave a stale open streak. Such a flight reports `provider_progress_turns` on an ordinary (untyped or `empty_turn`) or disk (`data_volume_full`) recovery park, only when the api advertises `provider_progress_on_recovery_park_v1` and only with a positive `claim_generation`. The api clears the streak (count and start) in the same statement that applies the park, so the reset is atomic with the generation, status and release fences and a stale or replayed report cannot reset it. Missing or zero evidence preserves the streak. The worker's strict-decoder fallback is bounded: one re-send of the same park without the field, fence kept, the cached feature dropped, no third variant. Old workers and old apis preserve the streak conservatively. Rejected: `iteration_count`, `last_seq` (status lines advance it), `checkpoint_tip_at` (a park publishes a checkpoint), "became running" (fires on a bare claim).
- **The hold keeps the claim and worker affinity (mirrors `SetRunWallPark`).** A verified but unpublished capture may live only on that worker; releasing the claim nulls `worker_id` on resume. Rejected: the server sweep's claim release (`ParkRunsAtWall`).
- **Provider settlement runs before the typed report.** A Codex reap is refused once the run is `paused`. Rejected: settling after the ack (the #1531 leak class).
- **Bound evaluated at park time in a dedicated locked arm.** One writer, one snapshot. Rejected: converting overdue rows in `PromoteRecoveryWaitRuns` (a second writer); reusing the single-UPDATE `setRecoveryWait` (cannot compute streak and hold atomically with pending-request consumption).
- **Owner actions are Resume, Stop and Cancel.** Stop reuses the existing scope ceiling plus resume (two steps) rather than a new atomic Stop path for this hold. Simplest design that keeps Stop available; `SubmitInput`'s special-casing exists only for `budget_exhausted`. Rejected: a dedicated atomic Stop transition (new server path for a rare action).
- **Owner early promote and credential switch clear the streak.** Both are owner decisions to try again. Rejected: keeping the streak (the owner would be held again on the first park after their own action).
- **The hold settles every pending pause input.** A pending owner `now` pause left unapplied would abort the resumed flight and then fail to park (`SetRunPaused` needs `pause_requested_at`). The hold already satisfies the owner's intent to stop. Rejected: consuming only `wall` (the wall-park precedent), which leaves that hazard.
- **Feature-gated typed report.** The feature means "this api bounds the cause and may answer `paused`", so only workers that handle a `paused` ack ever receive one.
- **Alternative considered for the hold:** `recovery_wait` with a server-only cause excluded from promotion (the `worker_requeue_exhausted` pattern) would leave the worker unchanged, but needs a CHECK widening and departs from the decision's `paused` owner hold. Rejected.
