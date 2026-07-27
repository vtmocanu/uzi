# PRD #35: Retry after Anthropic usage limit — park runs until the limit resets

**GitLab Issue**: [vtmocanu/uzi#35](https://gitlab.example.com/vtmocanu/uzi/-/issues/35)
**Status**: Draft — reviewed 2026-07-12 by 3 agents (design review, security audit, fact-check); all blocking/major findings folded in below (marked ↳review where the design changed). **Re-verified 2026-07-27 against HEAD `ca597779`** (1343 commits / 185 merges after authoring) — citations refreshed, six claims refuted, four prescriptions dropped as already-satisfied (marked ↳2026-07-27). The original header claimed "Fact-check: every claim verified"; that was not true of the prior-art paragraph, which was wrong at authoring time against a submodule pointer that never moved — see the Prior-art note below.
**Priority**: Medium
**Created**: 2026-07-12
**Depends on**: PRD #4 (runs/claim/sweeper, done), PRD #42 (worker run slots, done).
**Related (done since authoring, and they change this PRD's surface)**: PRD #111 (auto-select Anthropic credential at claim time — a promoted run re-runs selection, see Decision 6e), PRD #104 (named tokens; `runs.anthropic_secret_id` and siblings), PRD #53 (per-token rate-limit gauge — the server now has its own reset clock, see Decision 4), PRD #108 (auto-stop sweeper pass + `rmTreeForce` cleanup, see Decisions 3 and 6a), issue #105 (resume-dropped degradation — softens Decision 6a), PRD #64 / #112 (CLI + TUI are a third consumer of run status, see M-CLI), PRD #47 (run health — a parked run must read as "waiting", not "stalled"; its exit contract is now on every server-side status write), PRD #40 (token usage, done), PRD #32 (vault gate, done), PRD #25 (Slack notice).
**Also related**: specs/ai.md §188/§191 (per-user token concurrency budget — the pressure that makes limits likelier; still out of scope here).

## Problem

When a run exhausts the user's Anthropic subscription usage limit (five-hour or
seven-day window), the run dies:

1. The SDK absorbs *transient* 429/overloaded errors itself — internal bounded
   retries honoring `retry-after` (`api_retry` frames, `bridge.mjs` `retryRequest`).
   uzi never sees those. But a *sustained* limit (window reset minutes-to-hours out,
   beyond the SDK's retry budget) surfaces as a terminal error result, and the worker
   collapses every error result into a thrown `agent run failed: <subtype>`
   (`agent/src/sdk-executor.ts:858-873`; for a usage-limit death the subtype is
   `error_during_execution`) → `status=failed` with that string as the whole
   explanation (`agent/src/runner.ts:460-473`). No branch inspects *why*.
2. The SDK tells us everything we'd need and uzi throws it away:
   `mapSdkMessage` handles only `assistant|user|result|system(init)`
   (`agent/src/sdk-messages.ts:221-252`), silently dropping `rate_limit_event`
   frames (`SDKRateLimitInfo`: `status: 'rejected'`, `resetsAt` epoch,
   `rateLimitType: 'five_hour'|'seven_day'|…`, sdk.d.ts:4250-4262), `api_retry`
   progress, and the result frame's `terminal_reason`
   (`'blocking_limit'|'rapid_refill_breaker'`, sdk.d.ts:6909 — now a 19-member
   union, both members still present) that names the usage-limit death explicitly.
   Re-verified 2026-07-27: zero hits for `rate_limit_event`, `api_retry` or
   `terminal_reason` anywhere in `agent/src/`.
3. Nothing retries a worker-reported failure. Requeue exists only for worker death
   (stale heartbeat / register orphan, `runtime.sql:818-834` `RequeueRunsOfStaleWorkers`
   and `:851-864` `RequeueWorkerRuns`), and there is no not-before/backoff gate
   anywhere in the claim path — a requeued run is claimable on the next poll.
   Re-verified 2026-07-27: zero hits for `not_before` / `retry_at` / `backoff` /
   `next_attempt` across all migrations and queries.

Issue #35 (user, Romanian): if we hit the limit, retry after a delay — back off until
the limit resets; each user can opt in per run or set a default in settings.

Prior art check (inspiration submodules; ↳2026-07-27 rewritten — the original
paragraph was wrong): **no auto-retry anywhere**, which is the load-bearing half and
still verified — bottega retries once on a stale-OAuth 401 (`retryOn401.ts`),
dot-agent-deck fails on any non-200, and multica's retryable set excludes provider
limits outright (`server/internal/service/task.go:1910`, `retryableReasons` =
`{runtime_offline, runtime_recovery, timeout, codex_semantic_inactivity}`).
**But multica does classify usage limits**, contrary to the original claim:
`server/pkg/taskfailure/failure.go:115` defines `ReasonAgentProviderQuotaLimit`
("monthly usage limit, credits exhausted. Not retryable until the account is
topped up") alongside `ReasonAgentProviderCapacityOrRateLimit` (`:120`), matched in
`classify.go:108-129`. So Decision 8's "opt-out runs still get a better death" has
direct prior art. **Our deliberate divergence: multica classifies by matching error
text; we pin to structured SDK signals (Decision 1), which does not rot when a
provider rewords a message.** PRD #42 Decision 12 explicitly punted with "the SDK's
own retry/backoff is the handler" — correctly scoped to shared-token 429 contention;
this PRD supersedes that stance for sustained limits.

## Solution Overview

1. **Detect**: the worker captures `rate_limit_event` frames (a small observer beside
   `mapSdkMessage`) and, when a turn ends in an error result, classifies it as a
   **limit failure** (Decision 1's precedence rules). Everything else keeps today's
   failure path.
2. **Park (opt-in)**: instead of failing, the worker reports a new run status
   **`limit_wait`** carrying `limit_resets_at` + `rate_limit_type`, **preserving the
   per-run SDK home, the worktree, and the skills plugin dir on disk** (see
   Decision 6a — three removals, not two). The run's worker slot is released (the
   execution ends, unlike the slot-holding `awaiting_approval` gate); the SDK session
   id is already persisted on every state report (`runs.session_id`,
   `00020_workers_runs.sql:40`).
3. **Resume with backoff**: the server stamps `retry_not_before = resets_at + jitter`
   (clamped), and a new sweeper pass promotes `limit_wait → queued` once the clock
   passes. The existing claim path resumes the SDK session with worker affinity, and
   a run whose plan was already approved skips the plan gate on resume (Decision 6b).
   If the limit is hit again, the run parks again — bounded by a park-count cap.
4. **Opt-in, two scopes** (issue requirement): a per-run flag chosen at start
   (checkbox on the run-start modal) whose default comes from a per-user setting
   (Settings page), following the `users.autopilot_enabled` precedent
   (`00037_autopilot_mapping.sql:28`, `PUT /me/autopilot`). Opt-out runs keep
   failing — but now with an explanatory reason ("Anthropic usage limit
   (five_hour) reached; resets at …") instead of the bare subtype.
5. **Surface it**: run view + runs list + board card show a "limit wait" pill and a
   resume countdown; the feed gets a `limit_wait` message; the CLI/TUI gain the
   status (M-CLI); Slack (when enabled) posts a one-line thread notice.

## Design Decisions

1. **Detection pins to structured SDK signals, never text — `terminal_reason` is
   primary (↳review N5).** The `"Claude AI usage limit reached|<epoch>"` string is a
   Claude Code product string absent from `@anthropic-ai/claude-agent-sdk`
   **0.3.219** (↳2026-07-27: the pin moved from 0.3.201; re-verified by grep of the
   installed bundle's `sdk.mjs`/`bridge.mjs` — 0 hits in both). Classification of an
   error result:
   (a) `SDKResultError.terminal_reason ∈ {blocking_limit, rapid_refill_breaker}`
   (sdk.d.ts:4282, 6909) ⇒ limit failure; (b) absent a limit-shaped
   `terminal_reason`, the latest captured `rate_limit_event` with
   `status:'rejected'` counts **only when corroborated by a future `resetsAt`** —
   a stale 'rejected' event followed by an unrelated death (e.g. `error_max_turns`)
   must not park the run. `rate_limit_event` is a top-level member of the
   `SDKMessage` union (sdk.d.ts:4019; the type itself at :4237-4245), so the observer
   sees it on the normal query iterator. The observer records `resetsAt`,
   `rateLimitType`, and `overageResetsAt`; `resetsAt` is a bare `number` in the
   typings — normalize seconds-vs-milliseconds defensively (`< 10^12 ⇒ seconds`) and
   treat an absent or past value as "unknown".
   **Open (↳2026-07-27)**: `terminal_reason` is also declared on `SDKResultSuccess`
   (sdk.d.ts:4316), not only on the error variant. M1 must decide whether a *success*
   result carrying `blocking_limit` is reachable and what it means — the current
   reading is that it is not a failure and must not park, but that needs a frame
   from a real session or an SDK-source read before it is asserted.
2. **Transient vs sustained boundary is the SDK's own retry budget.** `api_retry`
   frames show the SDK retrying with `retry-after` honored; uzi acts only when the
   turn *terminates* in a limit-classified error result. No uzi-side retry loop
   inside a turn — the 10m in-turn idle timer stays untouched
   (`sdk-executor.ts:54` `DEFAULT_IDLE_TIMEOUT_SECONDS`, tripped at `:779`; the value
   now arrives via `ctx.config?.idle_timeout_seconds` at `:389`, though
   `RUN_IDLE_TIMEOUT` is still the env, `config.go:621`). Optional nicety: map
   `api_retry` to a terse feed line so a visibly-retrying run doesn't look hung
   (helps PRD #47 too).
3. **A new non-terminal status `limit_wait`, not a reuse of `queued` and not a
   worker-held park.** Reusing `queued` gives no not-before gating and hides the
   state from the user; holding the slot like `awaiting_approval` would pin a worker
   slot for up to days and still get reaped: the server sweeper fails `running` runs
   past RUN_TIMEOUT=2h (`runtime.sql:789-801`) and five_hour/seven_day resets exceed
   that. `limit_wait` is:
   - added to the status CHECK (`00020_workers_runs.sql:37` — still the inline
     `00020` constraint; the 14 subsequent `ALTER TABLE runs` sites only touch
     `runs_kind_check` / `runs_kind_shape` (00053, 00058) and
     `runs_anthropic_select_reason_check` (00089), never the status CHECK);
   - excluded from every sweeper fail/requeue filter. ↳2026-07-27 **the original
     argument no longer explains why.** `Sweep` (`service.go:3066`) now runs **nine**
     counters, not five (`SweepResult`, `service.go:3045-3062`, adds
     `ChatIdleCompleted`, `ProposalsRecovered`, `HealthChanged`, `AutoStopped`), and
     PRD #108's auto-stop pass is a **negative** SQL guard — `FailRunAutoStop`
     (`runtime.sql:731`) matches `status NOT IN ('completed','failed','cancelled')`,
     which a parked run is inside. Exclusion comes from Go, at
     `internal/workersvc/autostop.go:210` (`if run.Status != "running"`). The outcome
     is safe but the "already exclusive, pinned by test" claim must be
     **re-established** against a pass that did not exist when it was written.
     **↳M0 — re-established at HEAD, and it holds.** `autostop.go:210` reads
     `if run.Status != "running" { s.persistFail.evict(c.runID); return false }`. The
     other passes were re-read too and each is exclusive by a *positive* status
     allowlist or a kind scope — including the two register-time orphan queries
     (`FailWorkerRunsOverCap`, `RequeueWorkerRuns`, `runtime.sql:847`/`:860`), which
     are outside `Sweep` entirely and which this PRD never listed: a worker restarting
     leaves a parked run alone. The full table is in the design brief so the coder does
     not "fix" a filter that is already right. **The promotion pass goes after
     `SweepStuckConfirmingProposals` and before `s.persistFail.prune(now)`**
     (`service.go:3180`), counter `SweepResult.LimitPromoted`. Ordering is genuinely
     free — every pass is disjoint from `limit_wait` as a source *and* from `queued` as
     a target — so the reason is shape: `Sweep` is already "status transitions first,
     then the prune → detector → auto-stop block", and promoting before the detector
     makes a promoted run health-visible in the same tick rather than the next.
     Silver lining: that same line calls `s.persistFail.evict(c.runID)`, so the
     promotion pass needs no `evict` of its own (unlike the requeue sites at
     `service.go:3089` and `:3153`). `SweepIdleChatRuns` is kind-scoped and
     `SweepRunningTimeout` carries `kind <> 'chat'` (`runtime.sql:801`), so neither
     reaches a parked issue run;
   - **already** counted as active by `uq_runs_one_active_per_issue` — no edit
     (↳2026-07-27: `00043_ci_fix_runs.sql:27-29` is a negative guard,
     `WHERE kind = 'issue' AND status NOT IN ('completed','failed','cancelled')`).
     Intended: no second run on the issue while one is parked;
   - **already** a notifier no-op — no edit (↳2026-07-27:
     `runlifecycle/lifecycle.go:143-155` `notifierDecision` has `default: {act:false}`
     and the `Notify` gate at `:191` fires only for `queued/completed/failed/cancelled`,
     so the card does not move);
   - **already** invisible to the PRD #47 health detector — no edit, and it can
     therefore never read "stalled" (↳2026-07-27: `ListActiveRunsForHealth`,
     `runtime.sql:1286`, uses a **positive** allowlist
     `status IN ('queued','running','awaiting_approval')`).

     **↳M0 — that set has THREE hand-written mirrors, not one, and this inventory
     listed none of them.** All three are positive allowlists, so all three exclude a
     parked run for free and **none needs an edit** — but M5 and M4 work from this
     list, so an unlisted mirror gets either rediscovered as a "gap" or "fixed" into a
     real bug. They are: `web/src/lib/runBadge.ts:138` `isHealthFlaggableStatus` (the
     web badge gate), `api/internal/slacksvc/health.go:51` `isHealthFlaggableStatus`
     (the Slack root's ⚠ variant, a hand-written twin of the SQL set), and the SQL
     itself. **Do not add `limit_wait` to any of them**: the detector's signals
     (stalled / looping / slow) all describe a *running* agent, and a parked run's flag
     is cleared at park entry by the exit contract below, so there is nothing for a
     flaggable park to show but a false alarm;
   - still needing the **reconciler** mapping `limit_wait → In Progress with
     act:true` (`runlifecycle/lifecycle.go:161-172`; ↳review N3 — an act:false
     default would leave a pending move marker unhealed and trip the 30m give-up
     warn).
4. **The park is server-timed, worker-free, and server-sanitized.** Worker reports
   `{status: "limit_wait", limit_resets_at, rate_limit_type}` via the existing state
   endpoint (the `RunState` union at `agent/src/protocol.ts:15-19` gains the status;
   the `StateRequest` interface at `protocol.ts:666-710` gains the fields;
   `workersvc.SetState` dispatch `service.go:2104-2143` gains a case — the default arm
   rejects unknown statuses today (`:2142-2143`), so this is required wiring). The
   execution then ends and the slot frees (PRD #42 concurrency benefits).
   Server-side handling:
   - `rate_limit_type` is **allowlisted against the SDK union**
     (`five_hour|seven_day|seven_day_opus|seven_day_sonnet|seven_day_overage_included|overage`
     — re-verified 2026-07-27, still exactly these six at sdk.d.ts:4250-4262),
     anything else coerced to `"unknown"` before it reaches the DB, DTOs, feed, or
     Slack (↳audit MAJOR — the state path does no control-char sanitization of
     free text, unlike the register path's `sanitizeSelfReported`; an enum
     allowlist closes injection, length, and control-char concerns in one move).
   - `limit_resets_at` parses to a typed timestamp; `retry_not_before` is
     server-computed: known reset ⇒ `resets_at + jitter(60–180s)` (stampede
     avoidance across a user's parked runs); unknown reset ⇒ exponential fallback
     `15m × 2^(limit_wait_count−1)` capped at 4h; clamp: if
     `retry_not_before − now > RUN_LIMIT_MAX_PARK` (env, default `8d` — covers a
     seven_day window) the run **fails** with a clear reason instead of parking.
     The clamp is server-side because a compromised worker must not park a run for
     years.

     **↳M2 RULING (2026-07-27) — the exponential form STANDS; the shipped flat 15m
     constant (`workersvc/limitwait.go`, `limitParkFallback`) is a regression and is to
     be changed.** The coder flagged the constant honestly and its supporting finding is
     CORRECT and must survive the fix: the classifier's primary signal is
     `terminal_reason`, which names the cause and **carries no timestamp**, so a limit
     death with no reset is an ordinary outcome rather than a malformed report. The
     conclusion drawn from it is backwards — that finding establishes the unknown-reset
     path is **common**, which argues for *more* coverage, not less.

     **The decisive argument, which neither the constant nor the escalation made: an
     exponential schedule's error is recoverable in one direction and fatal in the
     other, and it errs on the recoverable side.** If the window actually reopened
     early, the first resume succeeds and the schedule never advances — the pessimism
     costs nothing. If the window is genuinely long, the schedule buys the hours the run
     needs. A flat constant inverts exactly that: it costs nothing when the guess was
     pessimistic and **kills the run** when it was optimistic. At
     `RUN_LIMIT_MAX_WAITS=5`, flat 15m spends the whole budget in **75 minutes**, while
     `15/30/60/120/240` spans **7h45m** — and the five-hour window is this PRD's
     headline case, so the flat form fails precisely the run the PRD exists to save,
     burning five clone-and-resume cycles doing it.

     **Exposure is narrower than Decision 4 assumed, but not narrow.** Steps 1-3 of the
     computation (worker reset → dead-credential gauge cross-check → `NextAvailable`)
     supply a base in most cases, so the fallback is reached only when all three are
     silent. That is the standing state for **any deployment with the usage poller
     disabled** (`UZI_USAGE_POLL_INTERVAL=0`): nothing refreshes `synced_at`, every
     candidate classifies `stale`, `Measured` is false, and neither the cross-check nor
     `NextAvailable` can contribute. Combined with the coder's own `terminal_reason`
     finding, that is a whole class of deployment for which the fallback is the **usual**
     path rather than the corner. *(Consequence worth knowing: the e2e overlay sets
     `UZI_USAGE_POLL_INTERVAL=0`, so M6 exercises this branch by construction.)*

     🔴 **Implement the exponent against the VARIABLE, not against this prose.**
     `limitParkInput.LimitWaitCount` is *"how many times this run has ALREADY parked"*
     and the budget guard reads `LimitWaitCount >= MaxWaits`, so it is **0 on the first
     park**. The `2^(n−1)` above is written for a 1-based attempt number; applied
     literally to that field it yields **7.5 minutes** on the first park. The correct
     form is `15m << LimitWaitCount`, capped at 4h. *(The cap does not bind at the
     default budget — the fifth park is exactly 4h — so it guards an operator who raises
     `RUN_LIMIT_MAX_WAITS`, rather than constraining anything at 5.)*

     What the coder got right and must be kept: the base stays **15 minutes** and stays a
     **constant rather than an env knob** — it is the answer to "we know nothing", and an
     operator with something to say has `RUN_LIMIT_MAX_WAITS` and `RUN_LIMIT_MAX_PARK` to
     say it with. Only the schedule changes.

     **↳M0 — the jitter is the MECHANISM, not a garnish, and it must not be
     "simplified" away.** Decision 6e makes promotion pool-aware, which manufactures a
     correlated **wave**: N runs parked against one exhausted credential all become
     promotable at the same reset, and `PromoteLimitWaitRuns` is a single UPDATE that
     releases every eligible row in one tick — so the sweeper does the opposite of
     spreading them. The jitter is the only thing that separates them, and it works
     **with** PRD #111's in-flight counter rather than instead of it: jitter separates
     the promotions, claims then serialize, and each run's pick is *recorded*
     (`recordRunCredential`) before the next run's ranking query reads it, so the
     existing counter sees it.

     **Widening that counter to include parked runs cannot help, and the reason is
     worth knowing because it is the obvious fix**: a parked run's
     `anthropic_secret_id` names the credential it *spent*, not the one it is about to
     spend (nothing clears it until the next claim overwrites it). Counting parked runs
     therefore piles phantom load onto the already-excluded exhausted token and adds
     **zero** asymmetry between the candidates the wave will actually converge on. The
     counter's `limit_wait` exclusion is correct, and correct for Decision 6e's own
     reason: a counted run is one that has chosen. Full argument, the accepted residual
     (two promotions closer than one claim round-trip can still collide — a race that
     predates this PRD and that PRD #111 accepted explicitly) and the rejected
     alternatives are in **ADR-35 D4**. If it ever bites, the knob is the jitter
     RANGE, not the counter; the signal is `SweepResult.LimitPromoted > 1` on one tick.
   - ↳2026-07-27 **the "worker-reported is the only source" premise is obsolete.**
     PRD #53/#104 shipped `anthropic_rate_limits`, keyed on `user_secret_id` with
     `five_hour_resets_at` / `seven_day_resets_at` (`00080_rate_limits_per_token.sql:40-52`),
     refreshed by `api/internal/usagepoller/engine.go`. The server therefore holds an
     **independent** reading of the same clock, which beats clamping an untrusted
     number. M2 must either cross-check the worker's `limit_resets_at` against the
     row for the run's credential (preferring the server's when they disagree) or
     state why not. **↳M0 — it cross-checks, and the dead credential's gauge row is
     already in the `ListAutoSelectCandidates` result Decision 6e fetches, so there is
     no extra query.** Map the allowlisted type to the column
     (`five_hour` → `five_hour_resets_at`; the four `seven_day*` members →
     `seven_day_resets_at`; `overage`/`unknown` → no cross-check) and take
     **`max(worker reset, gauge reset)`** when the dead candidate is `Measured`. Both
     are then statements about the *same* window, and promoting before the later of
     them guarantees a re-park; the window mapping is what stops `max` over-delaying a
     five-hour block by a seven-day rollover. This is accuracy, not security — the
     security properties are unchanged and come from `RUN_LIMIT_MAX_PARK` and
     Decision 5's accepted self-scoped-retry bound. PRD #53 framed the split itself
     (`prds/done/53-rate-limits.md:14-16`: *"PRD #35 (retry after limit) handles the
     recovery; this PRD handles the foresight"*) — the recovery half can now be built
     on real foresight data.
   - ↳2026-07-27 **PRD #47 exit contract.** Every server-side status write in
     `runtime.sql` now carries `health = 'ok', health_reason = NULL,
     health_since = NULL`. `SetRunLimitWait` and the promotion query must decide
     whether they do too — a parked run is not unhealthy, so the current reading is
     yes for both, but it is a deliberate choice and belongs in the query comment.

     **↳M0 — it is NOT a choice. Both queries MUST carry it, and Decision 3 above
     holds the proof this bullet does not connect to.** `ListActiveRunsForHealth`
     (`runtime.sql:1271-1276`) is a **positive** allowlist, which Decision 3 banks as
     a free win (a parked run is invisible to the detector and can never read
     "stalled"). It cuts the other way too, and nothing checked that direction:
     **whatever flag was live at park time FREEZES for the whole park**, because
     nothing will ever revisit the row to clear it. A run parked while flagged
     `stalled` stays `stalled` for days. It is user-visible, not cosmetic —
     `crewStateFor` (`api/cmd/uzi/tui_lanes.go:235-248`) reads `runHealth` through
     `stalledHealth` after the terminal and gate checks, so a parked run falls
     straight into `crewStalled`. That is **Success Criterion 2 failing through the
     health column instead of the status column**, by a route this PRD never checks.
     Both query comments must cite the detector's positive allowlist as the reason.
     `health_notified_at` is not reset, per every other exit contract in the file.

     **Do not "fix" it by adding `limit_wait` to `ListActiveRunsForHealth`.** The
     detector's signals all describe a *running* agent, and making a park flaggable
     re-introduces the false alarm Decision 3 depends on avoiding. Clearing at park
     entry is the fix. The web mirror follows: `isHealthFlaggableStatus`
     (`web/src/lib/runBadge.ts:138`) must not gain `limit_wait` either.

   - ↳M0 **THE PARK ACKNOWLEDGEMENT CONTRACT — the worker cannot currently tell
     whether its park was applied, and the failure mode is an unbounded disk leak on
     the DESIGNED path.** `WorkerClient.reportState` returns `Promise<void>`
     (`agent/src/client.ts:142`) and `isAlreadyTerminal` returns true for **any** 409
     (`:250`), which `reportState` logs and returns as success. That is right for
     every status shipped today, all of which are *terminal* reports where "the server
     already moved on" genuinely is success. `limit_wait` is the first **non-terminal**
     report whose consequence is that the worker **skips three filesystem removals**.

     "Not parked" has five causes and **three of them are designed outcomes**: the
     `running`-only source guard failing, `RUN_LIMIT_MAX_WAITS` exhausted, the
     `RUN_LIMIT_MAX_PARK` clamp exceeded, `wait_on_limit=false` (coerced, Decision 5),
     and the run already terminal (Decision 11's cancel). The middle three are the
     designed terminal paths for a run that keeps hitting limits — the exact
     population this PRD serves — so uncorrected, **every run that exhausts its retry
     budget leaks the clone, the plugin dir and up to ~170 MB of run HOME**.

     **Ruling**: `reportState` stops discarding the response body and returns
     `{applied, status}`. No new endpoint and no new field are needed — both the 200
     and the 409 paths already return `{"run": runToDTO(run)}`
     (`handler/worker_protocol.go:483` and `:486`); the information is on the wire and
     is being thrown away.

     🔴 **The worker skips cleanup IF AND ONLY IF the returned status is
     `"limit_wait"` — not `if (applied)`.** On the three designed paths above the
     server *fails the run*, so `applied` is **true** and an `applied`-keyed branch
     leaks on the most common cause of all. The status is the discriminator; `applied`
     is diagnostics. A positive test on one literal is chosen over enumerating the
     five causes because the default arm is then "clean up", so a future status, an
     old server, a parse failure and an unforeseen cause all land on the safe side —
     an enumeration goes stale, this cannot. On not-applied the worker cleans up
     fully, logs at warn, and sends **no** further state report (the server already
     owns the narrative; a `failed` report on top would clobber a `cancelled`).

     Two comments go false and must be corrected in the commits that break them:
     `client.ts:246-247` ("an idempotent success for any state report") and
     `worker_protocol.go:480-482`. And `worker_protocol.go:472`'s 400 text —
     `"state must be one of running, awaiting_approval, completed, failed"` — must
     gain `limit_wait`.
5. **Bounded, idempotent, source-guarded parks.** `runs.limit_wait_count` increments
   per park; `RUN_LIMIT_MAX_WAITS` (env, default 5) caps it — the N+1th limit
   failure fails the run ("usage-limit retry budget exhausted"). The
   `SetRunLimitWait` transition applies **only from `status='running'`** (↳review
   N4 + audit m-1): the ownership query filters id+worker_id only
   (`runtime.sql:415-417` — `SELECT * FROM runs WHERE id = @id AND worker_id = @worker_id`),
   so without a source guard a worker could park an
   `awaiting_approval`/affinity-`queued` run, and a re-delivered state report would
   double-bump the counter — `running`-only makes re-delivery a 0-row no-op and
   legit re-parks always come from `running`.

   **↳M0 — the source guard is only half of it: `SetRunRunning` can SILENTLY UN-PARK
   a run, and the park is not durable without the mirror guard.** `SetRunRunning`'s
   predicate is `status NOT IN ('completed','failed','cancelled')`
   (`runtime.sql:589`), so a `running` heartbeat delivered *after* the park report —
   the batcher retries, and reordering is possible — flips `limit_wait` back to
   `running` with a `worker_id` whose execution has already ended. The run then sits
   `running` until RUN_TIMEOUT fails it, and the park is lost with no signal anywhere.
   `SetRunRunning` and (for symmetry) `SetRunAwaitingApproval` need
   `AND status <> 'limit_wait'`. Resume is unaffected: the run is `claimed` when the
   worker reports `running` on resume. The terminal setters stay unguarded — "terminal
   always wins" is the existing invariant across the file, and the cancel path depends
   on it.

   **↳M0 — an opt-out run's `limit_wait` report is COERCED to failed server-side, not
   refused.** Pushing `wait_on_limit` into the SQL predicate looks safer and is not: a
   0-row result maps to `applied=false` → 409, and the worker **treats a 409 as
   "already terminal" success** (`service.go:2100-2103`), so the report vanishes and
   the run rots at `running` until RUN_TIMEOUT. The Go side reads `owned.WaitOnLimit`
   and, when false, routes to `SetRunFailed` with the Decision 8 reason built
   server-side from the allowlisted type and reset — the same branch point as the
   `RUN_LIMIT_MAX_WAITS` cap check. The worker's own opt-out path (report `failed`
   directly) stays the normal one; this is the guard behind it.

   Parking does **not** touch
   `requeue_count` (mirrors the vault lock-race precedent, specs/ai.md §139): the
   counters stay semantically distinct — requeue_count = worker deaths,
   limit_wait_count = limit parks. Documented worst case (↳audit m-2): the caps
   compose to ≈ 40 days (5 × 8d) that one run can hold the one-active-per-issue
   lock — recompute if either env default changes; cancel-while-parked (Decision 11)
   is the escape hatch, and a compromised worker mislabeling failures as limits buys
   at most 5 self-scoped retries on its own user's token (informational, accepted).
6. **Park preserves the on-disk session; resume rides the claim machinery
   (↳review BLOCKING B1 + MAJOR M2 — this decision was rebuilt).**
   - **(a) Cleanup carve-out — three removals, not two (↳2026-07-27).**
     `RunRunner.execute`'s `finally` (`agent/src/runner.ts:474-512`) unconditionally
     removes **all three** of:
     1. the runner clone / worktree — `this.git.removeRunnerClone(worktreePath)` (`:486`);
     2. the skills plugin dir the executor synthesized — `fs.rm(skillsPluginDir(worktreePath))`
        (`:497`, PRD #16 M4). It sits **outside** the clone, so preserving only 1 and 3
        would leave a resumed run without it;
     3. the per-run SDK home where the resumable transcript lives —
        `rmTreeForce(runHome)` (`:511`; PRD #108 M6 changed this from `fs.rm` because
        the Go module cache under it is written mode 0555 and `force:true` suppresses
        ENOENT, not EACCES).

     As first drafted, every park would have resumed into a fresh session. The park
     path is carved out of terminal cleanup for **all three**: on a `limit_wait`
     report the runner returns without removing any of them (exactly the state a
     worker-death requeue leaves behind), so `options.resume`
     (`sdk-executor.ts:405`, applied at `:763-764`) finds the transcript on resume.
     A run that later ends terminally cleans up as today.

     **Disk bound (↳2026-07-27, revised upward).** `runner.ts:505` records **167.3 MB
     of Go module cache measured for one run**, so the parked-disk worst case is
     roughly `RUN_LIMIT_MAX_WAITS`-concurrent-parked-runs × (clone + plugin dir +
     up to ~170 MB of run HOME). Still bounded by the park caps, but this is a real
     number to state in `docs/configuration.md`, not a rounding error.

     **Severity: BLOCKING → MAJOR (↳2026-07-27).** Issue #105 shipped
     `sessionTranscriptResolvable` / `resumeDropped` / `priorWork`
     (`agent/src/runner.ts:232-258`): a dropped resume now degrades honestly with a
     feed line and a prior-work hint in the plan prompt, instead of dying on
     `error_during_execution`. So a missed carve-out costs context, not the run.
   - **(b) Resume skips the plan gate when the plan was already approved.** The
     executor unconditionally runs Phase-1 planning + `gatePlan` regardless of
     `ctx.sessionId` (`sdk-executor.ts:408-520`; re-verified 2026-07-27 — `// ---
     Phase 1: planning turn ---` at `:408` with no `ctx.sessionId` branch anywhere in
     `:405-470`, and `ctx.gatePlan` at `:485`/`:500`/`:514`), so a naive resume would
     re-generate a plan, re-park the run at `awaiting_approval` for a human, and
     could even fail with `REASON_NO_PLAN` when the resumed session declines to
     re-emit `signal_plan`. Fix: the claim payload gains `plan_approved` (derived
     server-side: a consumed `approve_plan` input row exists for the run, OR
     `auto_approve`) plus the run row's `plan_md`. On resume with
     `plan_approved && session resumable`, the worker skips Phase 1 and the gate
     and enters the implement⇄review loop seeded with the delivered plan. A park
     *before* approval (during planning or at the gate — only reachable via the
     planning turn itself dying on a limit) resumes into planning as today.

     **↳M0 — `plan_approved` is a GATE-BYPASS PRIMITIVE; its invariant belongs in the
     query comment where it is enforced.** The natural derivation reuses
     `SetRunRunning`'s predicate (`runtime.sql:590-594`), whose own comment records an
     accepted residual: a consumed round-1 input lets a stale round-2 pre-gate report
     through. For `SetRunRunning` that residual hides a gate; for `plan_approved` it
     tells the worker to skip Phase 1 and **implement an unreviewed `plan_md`** — same
     residual, materially worse blast radius. It is sound today for a **structural**
     reason, which is exactly why it must be written down rather than relied on: a park
     is `running`-only (Decision 5), and a revise round sits at `awaiting_approval`
     because `SetRunRunning` refuses that transition without a consumed `approve_plan`.
     So the ordinary multi-round revise flow **cannot reach a park at all**, and the
     only surviving residual is the stale pre-gate report the existing comment already
     names. State the invariant — *no `awaiting_approval` report rewrites `plan_md`
     after the consumed `approve_plan` that made `plan_approved` true* — plus that
     two-clause argument, so the next reader sees the soundness is a property of the
     query PAIR, not of the worker's loop. No tighter derivation is cheaply available:
     `runs` carries no `plan_md_set_at` for `consumed_at` to be compared against.

     **↳M0 — `plan_md` is ALREADY on the claim payload** (`workersvc/claim.go:38`, fed
     unconditionally by `service.go:1328` and pinned by
     `claim_wire_contract_test.go:38`), so the claim gains **two**
     fields (`wait_on_limit`, `plan_approved`), not three. Derive the human half in
     SQL as a projected column on `GetRunClaimContext` — **cast it**
     (`(EXISTS (…))::boolean`), because sqlc types a bare expression as `interface{}`
     (CLAUDE.md, measured 2026-07-26) — and OR it with `run.AutoApprove` in Go.

     **↳M0 — the skip must ALSO require a non-empty `plan_md`, and the PRD's rule does
     not.** An autopilot run never reports `awaiting_approval`, so `runs.plan_md` is
     NULL for it, while `plan_approved` is true by virtue of `auto_approve`. Skipping
     Phase 1 on that combination enters the implement loop with no plan at all. The
     worker's gate is `plan_approved && claim.plan_md non-empty && session resumable`:
     the server states the fact, the worker decides what it can do with it. Residual,
     accepted: an autopilot run re-plans on resume. Its resumed session still holds
     the plan in the transcript and it cannot gate, so the cost is one turn.
     Persisting `plan_md` for autopilot runs would remove even that, and is PRD #37's
     scope, not this one's.
   - **(c) Affinity actually favors the prior worker.** Promotion resets
     `updated_at`, so `ClaimRun`'s affinity grace window **restarts** at promotion
     (`runtime.sql:464-472` — the predicate
     `AND (r.worker_id IS NULL OR r.worker_id = @worker_id OR r.updated_at < @affinity_cutoff)`
     plus `ORDER BY COALESCE(r.worker_id = @worker_id, false) DESC` prefer the prior
     worker; `WORKER_AFFINITY_GRACE` = 2m, `config.go:628`); the first draft's "grace
     will have long expired" was wrong (↳review; re-verified 2026-07-27). Honest
     residual, now partly handled upstream: if another of the user's workers claims
     after the fresh 2m grace, the session directory is absent there ⇒ fresh session
     + branch-attach. With (b)'s `plan_approved` skip this still bypasses the gate,
     and issue #105's `resumeDropped` path (`runner.ts:232-258`) warns and seeds
     prior work rather than failing — so the cost is conversation context only.
     Most users run one worker.
   - **(d) Clocks.** Promotion (`limit_wait → queued`) sets `started_at = NULL`
     (new design intent — the analogous `RequeueClaimedRunToQueued`
     (`runtime.sql:770`) never needed to, since `claimed` runs have no `started_at`)
     so the resumed run gets a fresh RUN_TIMEOUT wall; `session_id`, `last_seq`,
     `worker_id` are left untouched, as in every existing requeue query.
   - **(e) Which credential a promoted run spends (↳2026-07-27 — NEW, and it must be
     answered before M1).** PRD #111 shipped auto-select
     (`api/internal/autoselect/autoselect.go`), which ranks a user's opted-in
     Anthropic credentials by rate-limit headroom at claim time and skips any below
     `MinHeadroom` (`StatusBelowThreshold`, `:96-124`). Two consequences this PRD
     predates:
     - for multi-token users a park is now **rarer** — the ranker routes around an
       exhausted credential before the run ever starts;
     - a promotion re-enters the claim path, so **auto-select runs again and may pick
       a different credential**. `retry_not_before` was computed from the *original*
       credential's reset, so the gate can pointlessly delay a run that could execute
       right now on another token.

     The run already carries `anthropic_secret_id`, `anthropic_secret_label`,
     `anthropic_select_reason`, `anthropic_headroom_pct`
     (`00086_run_anthropic_secret.sql:63-66`).

     **RESOLVED (M0, 2026-07-27): re-select freely at claim — PRD #111 is untouched —
     and make the PARK-TIME stamp pool-aware instead.** The PRD's original
     recommendation ("re-select and recompute") inverts the causality and is not
     buildable: `retry_not_before` gates **promotion**, and re-selection happens at
     **claim**, strictly downstream. There is no point at which the newly chosen
     credential is known and the gate has not already fired.

     So the computation moves to where the information exists. At park time,
     `retry_not_before` stops meaning "the exhausted credential's reset" and becomes
     *"the earliest moment this user could plausibly spend anything"*:

     ```
     base := crossCheckedReset(worker-reported reset, gauge row for the DEAD credential)
     if alt, ok := autoselect.NextAvailable(cands, deadSecretID, policy, now); ok && alt < base {
             base = alt
     }
     retry_not_before = clamp(base + jitter(60–180s))
     ```

     `NextAvailable` is a new **pure** function in `api/internal/autoselect` (the
     package that already owns this vocabulary, D21's "one classifier"), over the rows
     `ListAutoSelectCandidates` already returns. Excluding the dead credential: a
     candidate that `Classify`s `eligible` contributes **now** (it is spendable
     immediately); a `Measured` but below-threshold candidate contributes its
     **binding-window** reset (`resetKey`'s rule, exported rather than duplicated);
     everything unmeasurable contributes **nothing** — an unknown must neither pull
     the floor to `now` nor push it out.

     Why the other two lose: **pin** adds a fourth resolution mode ahead of
     `claimSecretID` that outranks the claiming worker's configured
     `anthropic_bind_mode` (a change to PRD #111's resolution path, which Out of Scope
     protects) and delivers the worst outcome for exactly the multi-token users
     PRD #111 exists for; **re-select and accept the stale gate** fails Success
     Criterion 6 verbatim.

     Also rejected, and worth recording because it is the obvious idea: having the
     **promotion pass** ask "does this user have any credential with headroom" instead
     of consulting a stamped clock. It turns the only single-statement pass in `Sweep`
     into a per-user Go loop; it is **broken outright when the poller is disabled**
     (`UZI_USAGE_POLL_INTERVAL=0`, which is what the e2e overlay sets — every candidate
     classifies `stale`, so no parked run ever promotes); and the gauge lags the poll
     interval, so a run that just died on a limit still reads as having headroom,
     promotes, re-parks, and burns `limit_wait_count` to the cap in minutes.

     The properties that make the ruling safe: a **single-token user** has one
     candidate, it is the dead one, it is excluded, no leg contributes, and the stamp
     is that credential's cross-checked reset — today's behaviour exactly. A
     **poller-disabled deployment** classifies everything `stale`, no leg contributes,
     and the stamp is the worker's reported reset — today's behaviour exactly. A run
     whose `anthropic_secret_id` is NULL skips both legs (without the exclusion id,
     leg 1 could fire on the dead credential's own stale reading). Stated residual: a
     user with more pooled credentials than `RUN_LIMIT_MAX_WAITS` can burn the budget
     cycling through them, and the run then fails "usage-limit retry budget exhausted",
     which is honest.
7. **Opt-in: per-run flag defaulted by a per-user setting.**
   - `users.wait_on_limit` boolean NOT NULL DEFAULT false; `PUT /api/me/wait-on-limit`
     + Settings toggle (exact clone of the autopilot-enabled plumbing,
     `users.sql:43-47`, `handler/autopilot.go`; inherits RequireAuth→ValidateCSRF).
     All three citations re-verified exact 2026-07-27.
   - `runs.wait_on_limit` boolean NOT NULL DEFAULT false, stamped at creation:
     `CreateRun` body (today `{issue_iid}` only, `handler/workers.go:667-669`) gains
     optional `wait_on_limit`; absent ⇒ user default. Autopilot-created runs take
     the user default.

     **↳M0 — there are FOUR creation paths, not one, and a `DEFAULT false` column
     with a partial set of writers silently opts users out.** They are `CreateRun`
     (`runtime.sql:277`, manual *and* autopilot), `CreateCIFixRun` (`ci_fix.sql:3`),
     `CreateSelfImproveRun` (`selfimprove.sql:3`), and `CreateJudgeRun`
     (`judge.sql:6`, which stamps false — Decision 14). Resolve the effective flag in
     the Go service layer and pass it explicitly on each; do not push the defaulting
     into SQL, where a fifth path added later would silently miss it.

     **↳2026-07-27 — RULED BY THE USER. The per-run surface is a run-view toggle, not
     a start-run modal.** This bullet said "the start-run modal shows a
     pre-checked/unchecked box". **That modal does not exist and never did**:
     `startRun` is a one-click button calling `api.createRun(repoId, iid)` directly,
     from `web/src/pages/Board.tsx:211-215` and `web/src/pages/IssueView.tsx:70-75`.
     Building one would change existing behaviour on every run a user starts, so the
     question was escalated rather than decided by the team — it reinterprets a
     requirement the user stated in issue #35 ("each user can opt in per run or set a
     default in settings"). The user accepted the recommendation:

     - **Start stays one click** and inherits `users.wait_on_limit`. No modal, no
       confirmation step, no change to either existing surface.
     - **`wait_on_limit` ships as an optional field on `POST /api/runs`**, so CLI and
       API callers have per-run control at creation.
     - **Per-run control in the web is a toggle on the RUN VIEW**, live while the run
       is non-terminal.

     **The coverage argument is what justifies the reinterpretation, and it is not a
     consolation prize — the toggle is a strictly larger surface than the modal would
     have been.** A start-run modal can only reach runs a human starts by hand.
     Autopilot, `ci_fix` and `self_improve` runs are created by the poller and the
     self-improve engine with **no start affordance at all**, so a modal could never
     have given them a per-run opt-in — and `ci_fix` and `self_improve` both park
     (Decision 14). The toggle covers every kind that can park, including the three
     the modal structurally could not. It also arrives at the better moment: a user
     learns they want to wait when they are looking at a run, not before it exists.

     **Semantics of the toggle, so it is not mistaken for a control it is not.** It
     changes the flag for **future** limit failures only and never changes the run's
     current status: flipping it off on an already-parked run does **not** un-park or
     cancel it (Decision 11's cancel is the control for that), and flipping it on
     while parked is a no-op. On a terminal run the endpoint is a no-op and the UI
     renders the toggle inert.
   - The flag rides the claim payload like `auto_approve` (top-level, re-read from
     the row on every claim) so a resumed run keeps its opt-in.
   - `app_settings` is not used — it is admin/instance-wide only ("there is no
     user_settings table", `00044_slack.sql:5`); the caps are env config like the
     other run clocks.
8. **Opt-out runs still get a better death — and the SERVER composes the sentence
   (↳M0).** Even with `wait_on_limit=false`, a limit-classified failure reports
   `failure_reason = "Anthropic usage limit (<type>) reached; resets at <ISO>"`
   (type from the allowlisted enum, no secrets involved) and emits a `limit_hit`
   feed message.

   **↳M0 — if the WORKER composes that string, this PRD ships a Success Criterion its
   own design violates.** A worker-composed reason receives only
   `sanitizeFailureReason` (`service.go:3252` — `stripNUL` plus a rune cap), so
   "a compromised worker cannot smuggle a non-enum `rate_limit_type` past the server"
   is simply false on that path. Fix: the worker sends structured `rate_limit_type` +
   `limit_resets_at` on the **`failed`** report too, and the server composes the
   sentence from the allowlisted enum. One allowlist, both paths. Two constraints:
   an unrecognised type coerces to `"unknown"` and absent fields leave today's
   behaviour untouched (a terminal report is the one call a worker must not be able to
   fail on a technicality — `service.go:2171-2174`'s stated principle, followed rather
   than departed from); and the server replaces the worker's `failure_reason` text
   only when the fields are present, so no other failure path is disturbed.
   `rate_limit_type` needs no `stripNULParam` — the enum allowlist is strictly
   stronger, since a non-member becomes the literal `"unknown"` and no
   worker-controlled byte reaches the column — strictly better than today's `agent run failed:
   error_during_execution`. (Prior art exists for this half, in multica's
   `pkg/taskfailure` — see the Prior-art paragraph.)
9. **Chat lane: message only, no auto-retry.** Chat already survives an erroring
   turn (parks for the next user message, `chat-executor.ts:455-486` — the error path
   is `:465-482`, throwing `chat turn failed: ${errorSubtype}` at `:482`). The same
   classifier upgrades the feed line to "usage limit reached; resets at <t>" so the
   user knows why their chat went quiet. Auto-retrying a conversation turn is out of
   scope.

   **↳M1 — this Decision STANDS AS WRITTEN, and chat deliberately does NOT adopt the
   structured kinds.** `chat-executor.ts:495` keeps `kind: "error"` with improved text
   (`:491-495`, via the shared `classifyLimitFailure` + `describeLimit`). Recorded as a
   decision rather than left as an unfinished edge, because "the run lane got
   structured kinds and chat did not" otherwise reads as an omission:
   - **Chat never parks.** The structured kinds exist to carry `resets_at` for a
     countdown; a chat turn has no countdown, so the payload would reduce to a
     `rate_limit_type` and nothing else.
   - Adopting them would pull the two new kinds into the **chat render surface**,
     widening the web lane's scope for no user-visible gain over the improved sentence.
   - **No new trust exposure**, which is the one that was actually checked before
     ruling rather than reasoned about: that path already renders arbitrary
     worker-authored text through `kind: "error"`. Had it introduced an untrusted field
     into a surface not already treating worker text as untrusted, the ruling would have
     gone the other way.

   **One asymmetry an auditor will grep for, so it is named here: `describeLimit`
   interpolates the raw `rateLimitType` into prose on this path, which is exactly what
   Decision 8 forbids on the run path.** It is not a Decision 8 violation. Decision 8
   guards `runs.failure_reason` — a persisted run field rendered in the run view, the
   CLI and Slack, reachable with only NUL-strip and a length cap. This is a **feed
   message**, whose text is worker-authored free text by design and already escaped as
   such. The consequence to state plainly: **the server's enum allowlist does not reach
   the chat path at all**, and that is accepted, because it is the pre-existing property
   of every chat error line rather than something this PRD introduces.
10. **Feed + UI + DTO plumbing (↳review N1 — the Go DTO edit is hand-maintained and
    now named; ↳2026-07-27 — it moved, and gained a consumer).** New run_message kinds
    `limit_wait` (payload: `resets_at`, `rate_limit_type`) and `limit_hit` —

    **↳M1 — `attempt` is DROPPED from that payload; the code is right and this
    Decision was wrong.** The worker **cannot know it**: the park count is
    `runs.limit_wait_count`, which the SERVER increments inside `SetRunLimitWait` —
    strictly after the message is emitted and after the batcher carrying it is closed
    — and the claim payload does not deliver a prior value either (it carries
    `requeue_count`, a different counter, for worker deaths). Any emitted `attempt` is
    therefore a stale N−1 that disagrees with the run row it describes.

    **The counterargument, and what settles it, because otherwise the next reader
    re-opens this.** A feed row is a historical record, so reading "attempt N" from the
    live DTO instead means a run that parked three times shows three rows that say the
    same thing. That is acceptable because **a feed row is inherently distinguished by
    its own gapless `seq` and timestamp** — two parks are two rows at two times
    whatever the payload says — and additionally by `resets_at` whenever the reset is
    known, which is the common case. *(Stated this way on purpose: "distinguished by
    `resets_at` by construction" is **too strong**. `resets_at` is OMITTED when the
    reset is unknown — that is precisely the case Decision 4's exponential fallback
    exists for — so two unknown-reset parks do emit identical payloads. The position in
    the feed is what always distinguishes them; the timestamp is the field that never
    goes missing.)* The run-view header reads the authoritative `limit_wait_count` off
    the DTO, where it is always current.

    **`resets_at` rides the wire as an ISO string, not the SDK's bare number.** The SDK
    field carries a seconds-versus-milliseconds ambiguity that the worker normalises;
    putting the normalised form on the wire means no consumer ever re-derives that
    decision. Omitted entirely when unknown, never `null`, so "unknown" is one shape on
    the wire rather than two —
    `run_messages.kind` carries no DB CHECK (`00020_workers_runs.sql:74`, confirmed;
    PRD #39 Decision 8, its ↳review D12 thread), rendered with explicit cases
    in `RunEvent.tsx`/ActivityFeed (escaped, no raw sinks). The new run fields
    (`retry_not_before`, `limit_resets_at`, `limit_wait_count`, `wait_on_limit`)
    must be added to:
    - the DTO **type**, which now lives in its own wire package —
      `api/internal/apitypes/run.go:10` `type RunDTO struct` (it was in
      `handler/workers.go` when this PRD was written);
    - `runToDTO`, still in `api/internal/handler/workers.go` but at `:302`;
    - the web `Run` interface (`web/src/lib/api.ts:779`) and the `RunStatus` union
      (`:727-734`);
    - **`web/src/lib/runBadge.ts:10-13` (↳2026-07-27 — a second tone surface this PRD
      never named)**, the board-card badge taxonomy, whose own comment reads *"Tones
      mirror StatusPill's RUN_STATUS_TONES (ui.tsx) so one status renders one color
      everywhere"*. Missing it yields a `limit_wait` pill that is correct in the run
      view and neutral-grey on the board;
    - **the CLI/TUI (↳2026-07-27)** — see M-CLI. `apitypes` is now a shared wire
      package the CLI constructs directly (`api/cmd/uzi/commands_test.go:138`), so
      this is a three-consumer change, not two.

    `RunStatus`, `RUN_STATUS_TONES` (warn) — **in `web/src/components/ui.tsx`, NOT `web/src/lib/`; the bare `ui.tsx` below reads as a sibling of `runBadge.ts` and is the one citation here that sends a reader to the wrong directory** — and the run view header get `limit_wait`
    with a countdown to `retry_not_before` and "attempt N/cap". Unknown-status
    fallback means a half-shipped web build degrades to a neutral pill, not a crash
    (`ui.tsx:206` opens the map; the fallback
    `RUN_STATUS_TONES[status] ?? { tone: "neutral" }` is at `:217`).
11. **Cancel while parked works server-side (↳review MAJOR M1; ↳2026-07-27 — half of
    this decision was void, and was void at authoring).** A parked run keeps
    `worker_id` and its worker keeps heartbeating for other runs, so `hasLivePoller`
    (`service.go:3023-3038`) would report a live poller and `SubmitInput` would
    enqueue a cancel that nothing consumes until resume. Fix: `hasLivePoller` treats
    `status='limit_wait'` like `'queued'` (no live steering by definition) — the
    check at `:3024-3026` is **positive** (`if run.Status == "queued" || !run.WorkerID.Valid`),
    so this wiring is genuinely required.
    **`CancelRunServerSide` needs no change.** `runtime.sql:679-690` guards
    `WHERE id = @id AND user_id = @user_id AND status NOT IN ('completed','failed','cancelled')`
    — a negative predicate that covers any new non-terminal status for free. This was
    already true when the PRD was written (`git show 9cbc29a2:…/runtime.sql`, lines
    309-317, identical predicate), so the original prescription was contradicted by
    its own citation. This is still the escape hatch for the multi-day park
    (Decision 5's worst case). Approve/reject/follow-up inputs queue as today and
    deliver on resume.
12. **Vault gate composes.** Promoted runs re-enter `queued` and remain subject to
    the vault-unlock claim gate (PRD #32): a run whose reset elapsed while the vault
    is locked simply waits in `queued` like any other run. No special case.
13. **Migration numbering** (↳2026-07-27 — refreshed): draft `00095_limit_wait.sql`
    (collision-avoidance only; renumber above the live head at landing per
    CLAUDE.md). **Live head is now `00089_run_select_reason_check.sql`** (62 files;
    the sequence is sparse — 00003-00009 and 00076 are absent, which is normal here),
    so the renumber target is **00090+**, and the draft's 00095 leaves a five-wide
    gap other in-flight PRDs may claim. The old note that "PRD #50 has reserved
    `00057`" is **void** — 00057 was consumed by `00057_run_health.sql` (PRD #47).
    One migration: runs columns (`wait_on_limit`, `limit_resets_at`,
    `retry_not_before`, `limit_wait_count`, **`rate_limit_type`**), users column
    (`wait_on_limit`), status CHECK widened with `limit_wait`, plus a partial index
    `(retry_not_before) WHERE status = 'limit_wait'` for the promotion pass
    (↳audit m-3 — cheap now, moot to retrofit). Verified 2026-07-27: none of the five
    run columns, nor `users.wait_on_limit`, nor the index name exists today.

    **↳M0 — the fifth column is new, and it is required, not tidiness.** This list
    held four columns while Decision 4 had the worker report `rate_limit_type`,
    Decision 10 rendered it, and M5's Slack line needed it:
    `GetSlackRunContext` (`slack.sql:110-123`) is an **explicit column list selected
    `FROM runs r`**, so "paused: usage limit (five_hour)" cannot be assembled from a
    `run_messages` payload. Named `rate_limit_type` rather than
    `limit_rate_limit_type` to match the SDK field, the wire field, the DTO field, the
    feed payload key and the Slack render — a differently-named column buys prefix
    grouping at the price of a rename at four boundaries.

    **No CHECK on `rate_limit_type`.** The vocabulary is the SDK's six members plus
    the server's `"unknown"` coercion and is **not ours to close** — an SDK pin bump
    can add a member. `00086`'s own comment makes exactly this argument for
    `anthropic_select_reason` ("a CHECK here would be rewritten before it ever guarded
    anything"), and only `00089` closed it once the vocabulary had settled. A Go test
    pinning the allowlist against the SDK typings is the guard instead.

    **Read the status CHECK's constraint name off a live database rather than
    assuming it.** the CHECK is on `00020_workers_runs.sql:37`, declared inline with no name, so
    Postgres auto-generated one; `runs_status_check` is the expected form, and a wrong
    `DROP CONSTRAINT` fails at boot rather than in a test. `\d runs` settles it.
14. **Which run kinds park (↳2026-07-27 — NEW; RESOLVED at M0).** `runs_kind_check` is
    now `('issue','ci_fix','chat','judge','self_improve')`
    (`00053_chat_runs.sql:23`, `00058_run_judge_self_improve_kinds.sql:29`); this PRD
    reasoned only about issue runs with a chat carve-out (Decision 9). The ruling is
    exhaustive over all five, because `ci_fix` was left implicit too:

    | kind | parks? | what happens instead |
    |---|---|---|
    | `issue` | **yes** | as designed |
    | `ci_fix` | **yes** | see below |
    | `self_improve` | **yes** | see below |
    | `judge` | **no** | Decision 8's better death + a `limit_hit` feed message |
    | `chat` | **no** | unchanged — Decision 9's feed-line upgrade only |

    - **`ci_fix` parks.** It rides `RunRunner.execute` (`runner.ts:569`), the ordinary
      claim lane, the ordinary executor and the ordinary plan gate — every mechanism
      M1 and M2 touch. Excluding it would mean *writing a guard*, i.e. paying work to
      not have the feature. Created by the poller with no user in the loop, so it
      takes the owner's `users.wait_on_limit` default.
    - **`self_improve` parks.** Same argument (`runner.ts:378`, `:577`), and it is the
      run kind whose loss hurts most: long, repo-ful, `auto_approve`, expensive. The
      counter-argument is that it holds `uq_runs_one_active_self_improve`
      (`00058:54-56`, a negative guard) for the park window — but the engine's
      documented behaviour on a blocked tick is *"a cycle is still in flight, skip the
      tick"* (`self_improve.go:15-18`), which is precisely true of a parked run, and a
      *failed* self_improve unblocks the index only by throwing the whole run away.
      Cancel-while-parked (Decision 11) is the escape hatch. Note it routes to
      `judgeSecretID`, never `autoChoice` (`service.go:1094-1100`), so Decision 6e's
      pool legs are inert for it — no kind check needed, it falls out.
    - **`judge` does NOT park**, on three grounds. (1) Structural, and decisive: it is
      executed by `JudgeRunner.execute` (`agent/src/judge-runner.ts`, dispatched at
      `worker.ts:164`) — a different file with its own error path and its own cleanup,
      which never touches `runner.ts`'s `finally`. Parking it means duplicating
      detection + park + carve-out into a second executor, roughly doubling M1's
      surface for the cheapest run kind in the product. (2) Its value decays and it
      cannot be re-enqueued: `maybeEnqueueJudge` fires once, on the *reviewed* run's
      committed terminal transition (`judge_enqueue.go:33-42`), and that run never
      transitions again — but equally, a judge parked for seven days reviews a run
      nobody remembers, and losing a retrospective loses no user work. (3) It is
      already free of side effects: `maybeEnqueueJudge`'s Gate 0 is a positive
      allowlist and Gate 1 excludes `judge` outright, so `limit_wait` can never
      recurse.

      **What judge gets instead**: `judge-runner.ts:295` already reports `failed` with
      a reason; M1 routes that reason through the same classifier, so an SDK limit
      death yields `"Anthropic usage limit (five_hour) reached; resets at <ISO>"` plus
      a `limit_hit` message rather than `agent run failed: error_during_execution`.
      The classifier is a **pure function over SDK frames** in its own module,
      imported by both executors and owned by neither. `SetRunLimitWait` additionally
      carries `AND kind <> 'judge'` so the refusal is enforced server-side, not by
      worker discipline.

      Out of scope, named so it is not lost: re-enqueueing a judge that died on a
      limit is PRD #46's enqueue policy, not this PRD's.

## Milestones

- [x] **M0 — Resolve the two open questions** (blocked M1): which credential a
  promoted run spends (Decision 6e), and whether `judge` / `self_improve` runs park
  (Decision 14). **Done 2026-07-27**; both rulings are folded into the decisions
  above. The implementation brief — contracts, file map, migration column list,
  sweeper placement, milestone dependency graph — is
  `.claude/agent-team-tasks/prd-35-design.md`, and it records six findings that
  change the milestones below (see the 2026-07-27 M0 Decision Log entry).

- [ ] **M-SEAM — Freeze the wire contract before the parallel milestones** (new at
  M0; blocks M1/M2/M3/M4/M-CLI). M1..M4 as written are **not** file-disjoint — all
  four need the `RunState`/`StateRequest`/`ClaimPayload`/`RunDTO`/`RunStatus`
  additions, and M2/M3/M5 all edit `workersvc/service.go` plus the sqlc-generated
  tree. One mechanical, zero-behaviour commit lands the migration, the sqlc
  regeneration, the two protocol.ts field groups, the mirrored Go structs, the five
  DTO fields + `runToDTO`, the `RunStatus` union member, and the two envs. Every
  member each downstream milestone reads is enumerated in the design brief; a field
  missing from those lists means the seam is incomplete and is an escalation, not an
  edit to a frozen file.
- [ ] **M1 — Worker detection + park path**: rate-limit observer beside
  `mapSdkMessage` (captures latest `rate_limit_info`), classification per Decision 1
  precedence (terminal_reason primary; corroborated-rejected secondary), typed
  `LimitReachedError` with normalized `resetsAt` + `rateLimitType`, runner branch:
  opt-in ⇒ report `limit_wait` **and skip all three cleanups** (Decision 6a —
  runner clone, skills plugin dir, run HOME), opt-out ⇒ improved `failure_reason` +
  `limit_hit` message; resume-skips-gate path (Decision 6b, consuming claim
  `plan_approved`/`plan_md`); chat feed-line upgrade; unit tests with real-shaped SDK
  frames (rejected event + corroborating resetsAt, blocking_limit result,
  stale-rejected + error_max_turns NOT classified, transient api_retry NOT
  classified, seconds-vs-ms normalization, **and a carve-out test that asserts a
  parked run's `skillsPluginDir` and run HOME both still exist on disk**).
- [ ] **M2 — Schema + server park machinery**: migration (Decision 13, renumbered
  00090+), `SetRunLimitWait` transition (source-guarded `running`-only, idempotent on
  re-delivery, allowlists `rate_limit_type`, computes/clamps `retry_not_before`,
  bumps `limit_wait_count`, enforces `RUN_LIMIT_MAX_WAITS` / `RUN_LIMIT_MAX_PARK`
  fail-instead-of-park, PRD #47 health exit contract per Decision 4), optional
  cross-check of the worker's reset against `anthropic_rate_limits` (Decision 4),
  sweeper promotion pass (`limit_wait → queued`, `started_at=NULL`, no
  `requeue_count` bump, no `persistFail.evict` needed — see Decision 3),
  `hasLivePoller` extension (Decision 11 — **no `CancelRunServerSide` change**),
  claim payload gains `wait_on_limit` + `plan_approved` + `plan_md`, DTO fields
  (Decision 10), sweeper-exclusion tests **re-established against today's nine-pass
  `Sweep`** — a parked run is never RUN_TIMEOUT-failed, stale-requeued, or
  auto-stopped (PRD #108); cancel-while-parked applies immediately.
- [ ] **M3 — Opt-in surfaces**: `users.wait_on_limit` + `PUT /me/wait-on-limit` +
  Settings toggle; `CreateRun` wire defaulting from the user setting; **all four**
  creation paths stamp the flag (Decision 7); tests (default inheritance, explicit
  override, flag survives resume).

  **↳2026-07-27 scope change, per the user's ruling in Decision 7.** The start-run
  modal checkbox is **dropped — there is no modal, and none is being built**; the
  one-click start is untouched. In its place:
  - `PUT /api/runs/:id/wait-on-limit` (owner-scoped, RequireAuth → ValidateCSRF like
    the `/me` sibling), guarded by the same **negative** predicate
    `CancelRunServerSide` uses — `status NOT IN ('completed','failed','cancelled')` —
    so it is a no-op on a terminal run and covers `limit_wait` for free.
  - The run-view toggle, **inert on terminal runs**, wired to that endpoint.
  - Tests: the toggle flips the column on a running run; is a no-op on a terminal one;
    **does not change `status` on an already-parked run** (the one assertion that
    stops a future reader mistaking it for a cancel); a `ci_fix` and a `self_improve`
    run each inherit the user default at creation, since those are two of the three
    kinds the dropped modal could never have reached.

  The web half of this belongs to the **web lane**, not the api lane, so `RunView.tsx`
  keeps one owner alongside M4's countdown strip.
- [ ] **M4 — Web UX**: `RunStatus`/tones/`limit_wait` pill, run-view countdown strip
  (resets-at + attempt N/cap), runs list + board card badge **including
  `web/src/lib/runBadge.ts`** (Decision 10), `limit_wait`/`limit_hit` render cases in
  RunEvent/ActivityFeed; vitest coverage incl. escaped rendering.

  **↳M0 constraints, both load-bearing.** (a) **The board-card work must be a
  ZERO-LINE `web/src/pages/Board.tsx` delta.** PRD #102 (board v2) is rewriting that
  file in a parallel worktree. It does not touch `runBadge.ts`, whose header states
  why it exists: *"Kept out of Board.tsx so the mapping is unit-tested in isolation."*
  `Board.tsx:606` calls `runBadge(run, Date.now())` and renders whatever comes back,
  and `runBadge`'s `default:` arm (`runBadge.ts:261`) already renders an unknown
  status as a neutral pill — so a proper `limit_wait` badge is a change to
  `runStatusTone` (`:96`) and `runBadge`'s switch (`:231`), both inside
  `runBadge.ts`, and the collision disappears instead of needing sequencing. A
  `Board.tsx` edit is an escalation, not a merge problem. (b) **`LatestRun`
  (`web/src/lib/api.ts:317`) gains nothing** — it is a deliberately narrow board
  projection with no reset timestamps. The board card therefore shows a **static**
  "limit wait" pill and the **countdown lives only on the run view**, which reads the
  full `Run`. The two constraints compose: a board countdown would have forced a
  `LatestRun` widening *and* a `Board.tsx` prop change.
- [ ] **M-CLI — CLI + TUI** (↳2026-07-27 — new, and repo-mandated: "New uzi
  functionality ⇒ check whether `api/cmd/uzi/` needs a matching CLI change"):
  `terminalRunStatuses` (`api/cmd/uzi/run.go:48`) must not gain `limit_wait` (it is
  non-terminal) but every status-switch around it must handle it — the
  awaiting-approval branch at `run.go:842` is the shape to follow — plus the TUI crew
  lane mapping (`api/cmd/uzi/tui_lanes_test.go:143-152`). A parked run should read as
  waiting with its countdown, not fall through to a default lane.
- [ ] **M5 — Slack + docs + specs**: notifier gains `statusLabel` (`slacksvc/notifier.go:588`)
  + `renderThread` (`:549`) cases for `limit_wait` (else a park edits the root to raw
  "limit_wait" and posts nothing — both switches have `default:` arms that behave
  exactly that way today) and `GetSlackRunContext` (`:265`) selects the reset
  timestamp for the "paused: usage limit (five_hour); resumes ~<t>" line —
  `EscapeMrkdwn` applied per-field even though the type is allowlisted upstream;
  `docs/` page section + `docs/configuration.md` **and `.env.example`** for the new
  envs (↳M0 — `.env.example` was owned by no milestone and is assigned here; every
  comparable run knob appears in both files: `RUN_TIMEOUT:221`,
  `RUN_IDLE_TIMEOUT:223`, `RUN_MAX_REQUEUES:231`, `WORKER_AFFINITY_GRACE:243`) **and
  the parked disk cost** (Decision 6a); `specs/ai.md` design record; `specs/human.md`
  addition proposed to the user.

  **↳M0 — M5 must NOT "fix" `api/internal/slacksvc/health.go:51`.** Its
  `isHealthFlaggableStatus` is a positive allowlist that already excludes a parked run
  correctly; it is now listed in Decision 3's inventory as already-satisfied.
- [ ] **M6 — E2E**: stub-executor scenarios — opt-in park → sweeper promotes (short
  test clock) → resume **skips the gate** and completes; opt-out fails with
  explanatory reason; cancel-while-parked cancels immediately; full-stack smoke.

## Success Criteria

- A run that hits a sustained usage limit with opt-in enabled ends up `completed`
  after the window resets, with zero human intervention: SDK session resumed (same
  worker) and no second trip through the plan gate for an already-approved plan.
- A parked run is visibly "waiting" in web (countdown), CLI/TUI, and Slack, never
  "stalled", and is never killed by RUN_TIMEOUT, stale-worker sweeps, or the PRD #108
  auto-stop pass.
- Cancelling a parked run takes effect immediately (server-side), not on resume.
- Opt-out runs fail with a reason that names the limit type and reset time.
- Transient 429s behave exactly as today (SDK-internal, invisible except optional
  feed lines).
- A compromised worker cannot park a run beyond `RUN_LIMIT_MAX_PARK`, bypass the
  park-count cap, park a run not in `running`, or smuggle a non-enum
  `rate_limit_type` past the server.
- A promoted run does not sit behind a `retry_not_before` computed from a credential
  it is no longer going to spend (Decision 6e).

## Out of Scope

- Per-user Anthropic token concurrency budget / proactive throttling on
  `allowed_warning` (specs/ai.md §188/§191 — separate, recorded enhancement).
- Auto-retrying chat turns (Decision 9).
- Billing-class errors (`billing_error`, `credits_required`, `oauth_org_not_allowed`)
  — terminal today, terminal after this PRD (they don't reset on a clock).
- Pre-parking other queued runs of the same user when one parks (they'll classify
  and park individually on claim).
- API-key (non-subscription) rate-limit tiers — same detection applies if the SDK
  emits the frames, but no work is specific to them.
- A cumulative wall-clock cap tighter than count×clamp (the ~40d worst case is
  documented in Decision 5; cancel is the escape hatch).
- Changing PRD #111's auto-select ranking itself — this PRD only decides what a
  *promotion* does with it (Decision 6e).

## Decision Log

- 2026-07-12 — PRD created from issue #35; supersedes PRD #42 D12's "SDK backoff is
  the handler" for sustained limits. Detection pinned to structured SDK signals
  after verifying the Claude-Code text token does not exist in the agent SDK bundle.
- 2026-07-12 — Review round (3 agents). Blocking: park path must preserve the
  on-disk SDK home/worktree (runner cleanup carve-out) — without it every resume was
  a fresh session. Majors: `hasLivePoller` must treat `limit_wait` as pollerless
  (cancel-while-parked), resume must skip the plan gate for approved plans
  (`plan_approved` claim field). Audit major: server-side allowlist of
  `rate_limit_type`. Minors folded: `running`-only idempotent transition, reconciler
  act:true mapping, DTO plumbing named, promotion index, env renamed
  `RUN_LIMIT_MAX_PARK`, affinity-grace-restarts correction, citation fixes.
- 2026-07-27 — Staleness re-verification against HEAD `ca597779` (1343 commits since
  authoring). **The core thesis survives unbuilt**: no rate-limit detection, no
  not-before gate, and no park status exist anywhere in the tree, and the four
  load-bearing mechanisms (unconditional plan gate, `SetState`'s rejecting default
  arm, affinity grace restarting on `updated_at`, worker-death-only requeue set) all
  re-verified true. **Refuted and fixed**: SDK pin is 0.3.219 not 0.3.201; the DTO
  moved to `apitypes` and gained the CLI as a third consumer; `CancelRunServerSide`
  never needed extending (negative guard, true at authoring too); migration head is
  00089 and PRD #50's 00057 reservation is void; Decision 3's sweeper-exclusion
  argument no longer covers PRD #108's auto-stop pass; the prior-art paragraph was
  wrong about multica, which does ship a usage-limit classifier. **Dropped as
  already-satisfied**: active-run index, notifier no-op, health-detector exclusion,
  `CancelRunServerSide`. **New scope**: Decision 6e (which credential a promoted run
  spends, given PRD #111 auto-select), Decision 14 (do judge/self_improve runs
  park), M-CLI, `runBadge.ts`, the three-way cleanup carve-out, and the PRD #47
  health exit contract. ~25 further citations had drifted line numbers only and were
  refreshed in place. Decision 6a downgraded BLOCKING → MAJOR on the strength of
  issue #105's resume-dropped degradation.
- 2026-07-27 — **M0 design gate closed** (architect). Full brief:
  `.claude/agent-team-tasks/prd-35-design.md`.

  **Decision 6e — re-select freely at claim; make the PARK-TIME stamp pool-aware.**
  The PRD's own recommendation was unbuildable as written (`retry_not_before` gates
  promotion, which is strictly upstream of the claim where re-selection happens), so
  the computation moved to park time, where the user, the run and the candidate rows
  are all in hand. `retry_not_before` now means "the earliest moment this user could
  spend *anything*", via a new pure `autoselect.NextAvailable` over the existing
  `ListAutoSelectCandidates` rows. Rejected: **pin** (adds a fourth resolution mode
  ahead of `claimSecretID`, and is worst for the multi-token users PRD #111 exists
  for); **accept the stale gate** (fails Success Criterion 6 verbatim); and
  **headroom-checking in the promotion pass** (turns `Sweep`'s only single-statement
  pass into a per-user loop, is broken outright with the poller disabled — the e2e
  overlay's own configuration — and thrashes on gauge lag). Also rejected: gating the
  pool legs on `runs.anthropic_select_reason`, which is more precise but promotes a
  column `00086` documents as display-only into a load-bearing one to save at most one
  re-park. Single-token users and poller-disabled deployments are bit-identical to
  today by construction, which is the property the whole ruling was shaped around.

  **Decision 14 — park = {issue, ci_fix, self_improve}; no park = {judge, chat}.**
  `ci_fix` was implicit in the PRD and is now explicit. `judge` is refused on a
  structural ground rather than a policy one — it runs in a different executor
  (`judge-runner.ts`) that never touches `runner.ts`'s cleanup, so parking it would
  roughly double M1 for the cheapest run kind — and gets Decision 8's better death
  instead, enforced server-side by `AND kind <> 'judge'` on `SetRunLimitWait`.

  **Six findings that change the work.** (1) The migration needs a **fifth** `runs`
  column, `rate_limit_type` — M5's Slack line reads it off the run row via
  `GetSlackRunContext`'s explicit column list and cannot get it from a feed payload.
  (2) `SetRunRunning`'s `status NOT IN (terminal)` guard lets a late `running`
  heartbeat **silently un-park** a run, which then rots until RUN_TIMEOUT; it and
  `SetRunAwaitingApproval` need `AND status <> 'limit_wait'` (terminal setters stay
  unguarded — "terminal always wins"). (3) There are **four** run-creation paths that
  must stamp `wait_on_limit`, not the one the PRD names. (4) **There is no run-start
  modal** — `startRun` is a one-click button in `Board.tsx:211` and
  `IssueView.tsx:70`; Decision 7's checkbox has nowhere to live, and building a modal
  changes existing UX, so the surface is an open question for the user (the
  recommendation is a run-view toggle instead, which also covers autopilot, `ci_fix`
  and `self_improve` runs that have no start affordance at all). (5) A `limit_wait`
  report for an opt-out run must be **coerced to failed server-side**, not refused —
  a 0-row refusal maps to 409, which the worker treats as success, losing the report.
  (6) `plan_md` is **already** on `ClaimPayload` (`protocol.ts:451`), so the claim
  gains two fields, not three.

  **Parallelism**: 4 concurrent coders after a 1-coder M-SEAM commit — agent (M1),
  api (M2 → M3 → M5-slack, serial within the lane because all three edit
  `workersvc/service.go` and regenerate the shared sqlc tree), web (M4), cli (M-CLI)
  — plus a docs/specs worker from the seam onward. M6 (e2e) is last and serial.
- 2026-07-27 — **M0 rev 2**, folding in the priming-pass findings (each re-derived at
  HEAD before acting). **Four rulings.** (a) The PRD #47 health exit contract on
  `SetRunLimitWait` and the promotion query is **mandatory, not a deliberate choice**:
  `ListActiveRunsForHealth`'s positive allowlist means a flag live at park time
  freezes for the whole park, and `crewStateFor` renders it — Success Criterion 2
  failing through the health column rather than the status column. (b) **The park
  acknowledgement contract**: `reportState` returns `{applied, status}` instead of
  `void`, and the worker skips cleanup **iff the returned status is `limit_wait`**,
  never `iff applied` — on the three *designed* not-parked paths (budget exhausted,
  clamp exceeded, opt-out coercion) the server fails the run and `applied` is true, so
  an `applied`-keyed branch leaks ~170 MB on the most common cause. The escalation was
  right that this is the normal path rather than the cancel race. (c) Decision 8's
  opt-out sentence is **composed by the server** from the allowlisted enum, or the
  no-smuggling Success Criterion is false on that path. (d) `plan_approved`'s
  gate-bypass invariant goes in the query comment with its two-clause reachability
  argument. **Constraints added**: M4's board work must be a zero-line `Board.tsx`
  delta (PRD #102 is rewriting it; `runBadge.ts` is untouched by it and is where the
  mapping belongs) and must not widen `LatestRun`, so the countdown is run-view only.
  Migration `00090` confirmed free across all sibling worktrees, still to be
  re-verified at the landing rebase. Six citation nits corrected, of which one
  mattered: `RUN_STATUS_TONES` is in **`web/src/components/ui.tsx`**, and the PRD's
  bare `ui.tsx` read as a sibling of `web/src/lib/runBadge.ts`.

  **The milestone graph is unchanged** — none of the four rulings moves a file
  between lanes; (b) lands in the agent lane and the seam, (a)/(c)/(d) in the api
  lane, and the M4 constraints remove a collision rather than creating one.
- 2026-07-27 — **M2 divergence ruled: the exponential fallback stands; the shipped
  flat 15m constant is a regression.** `workersvc/limitwait.go`'s `limitParkFallback`
  replaced Decision 4's `15m × 2^(limit_wait_count−1)` with a flat 15 minutes. It was
  flagged honestly by the coder as "a constant nobody specified" and endorsed as filling
  a gap; it does not fill a gap, it **replaces a specified behaviour** answered in the
  same Decision being implemented. The coder's supporting finding is right and survives
  the fix — `terminal_reason` names the cause and carries no timestamp, so an
  unknown-reset limit death is ordinary rather than malformed — but the conclusion
  inverts it: that finding makes the unknown path COMMON, which argues for more coverage.
  **The argument that settles it, made by neither side: an exponential schedule's error
  is recoverable in one direction and fatal in the other, and it errs on the recoverable
  side.** Pessimism costs nothing (the first resume succeeds and the schedule never
  advances); optimism kills the run. Flat 15m spends the whole default budget in 75
  minutes against a five-hour window — this PRD's headline case. Exposure is narrower
  than Decision 4 assumed (steps 1-3 usually supply a base) but is the STANDING state
  for any deployment with the usage poller disabled, where nothing is `Measured` and
  neither the cross-check nor `NextAvailable` can contribute — and the e2e overlay is
  one such deployment, so M6 exercises the branch by construction. Implementation trap
  recorded with the ruling: `LimitWaitCount` is the count BEFORE the increment, so
  `2^(n−1)` applied literally gives 7.5 minutes on the first park; the correct form is
  `15m << LimitWaitCount`.

  **Second reported divergence — `plan_approved`'s non-empty-plan requirement — is NOT
  one, and the spec draft's provenance claim is wrong.** Decision 6b has carried that
  requirement since M0 rev 2 (`1605b78d`): *"the skip must ALSO require a non-empty
  `plan_md`, and the PRD's rule does not … the worker's gate is `plan_approved &&
  claim.plan_md non-empty && session resumable`"*. The code implements exactly that
  (`sdk-executor.ts`, `planApproved === true && !!sessionId && !!approvedPlan?.trim()`).
  So `specs/ai.md`'s *"the non-empty-plan requirement the PRD's rule lacked"* describes
  the behaviour correctly and its **provenance** incorrectly — it reads the original
  Decision 6b sentence and misses the ↳M0 addendum directly beneath it. Worth correcting
  rather than letting stand: filed as written it records an implementation-time AI
  decision where the truth is a documented design ruling, which is exactly the
  distinction the human/ai spec split exists to keep.

- 2026-07-27 — **Two M1 rulings recorded, in both cases because the CODE disagreed
  with a Decision as written and the code was right.**

  **Decision 10 loses `attempt` from the feed payload.** The worker cannot know it:
  `runs.limit_wait_count` is incremented by the server inside `SetRunLimitWait`,
  strictly after the message is emitted and after its batcher is closed, and the claim
  payload carries no prior value (`requeue_count` is a different counter). Any emitted
  `attempt` is a stale N−1 disagreeing with the row it describes. The counterargument —
  a feed row is a historical record, so reading the live DTO gives three identical rows
  for a run that parked three times — is answered by the row's own `seq` and timestamp,
  which always distinguish two parks, plus `resets_at` whenever it is known. **The
  original justification offered ("distinguished by `resets_at` by construction") was
  too strong and is recorded in its corrected form**: `resets_at` is omitted when the
  reset is unknown, which is exactly the case Decision 4's exponential fallback exists
  for, so two unknown-reset parks emit identical payloads and the position in the feed
  is what always separates them. Left uncorrected, the next reader finds that case and
  re-opens a settled decision believing they found a bug. Also recorded: `resets_at`
  rides the wire as an **ISO string**, not the SDK's bare number, so no consumer
  re-derives the seconds-versus-milliseconds normalisation.

  **Decision 9 stands as written: chat keeps `kind: "error"` and does NOT adopt the
  structured kinds** — chat never parks so there is no countdown to carry, adopting
  them would widen the web lane into the chat render surface for no visible gain, and
  the path already renders worker-authored text so there is no new trust exposure (the
  one leg actually checked rather than reasoned about). One asymmetry named so an
  auditor does not file it: `describeLimit` interpolates the raw `rateLimitType` into
  prose here, which Decision 8 forbids on the run path. It is not a violation —
  Decision 8 guards the persisted `runs.failure_reason`, this is a feed message whose
  text is untrusted by design — but the consequence is stated plainly: **the enum
  allowlist does not reach the chat path at all**, and that is pre-existing rather than
  introduced here.

  **Pattern noted in ADR-35's Consequences**: D4's refutation is the fourth instance in
  this PRD of "a value was reachable by a fragile derived path while the authoritative
  source sat one layer away", alongside the truncation fix, the lenient fake, and this
  `attempt` ruling. Each fix was structural, and each derived path *worked* on the
  happy path, which is why the shape survives review. The process form of the lesson
  belongs in `.claude/agent-team.md`, not the ADR.

- 2026-07-27 — **M0 rev 3**, from the M-SEAM review. **Ruling: the promotion-wave
  stampede is real, but the proposed fix cannot work and the existing design already
  contains the mechanism.** Decision 6e makes promotion pool-aware, which manufactures
  a correlated wave that did not exist before it — N runs parked on one exhausted
  credential all become promotable at the same reset, and `PromoteLimitWaitRuns`
  releases every eligible row in one UPDATE. The review proposed widening
  `ListAutoSelectCandidates`' in-flight counter to include parked runs. **It cannot
  spread anything**: a parked run's `anthropic_secret_id` names the credential it
  *spent*, not the one it is about to spend, so counting parked runs piles phantom load
  onto the already-excluded exhausted token and adds zero asymmetry between the
  candidates the wave converges on. The general form, which is the durable part: the
  in-flight bias works because it is **per-token asymmetric**, a run that has not yet
  chosen contributes no asymmetry, and a load term applied equally to every candidate
  cannot change a ranking — **parked load is structurally unrankable**. So the counter's
  `limit_wait` exclusion is not a gap Decision 6e opened; it is correct for Decision
  6e's own reason (a counted run is one that has chosen). What *does* spread the wave is
  time: the jitter separates the promotions, claims serialize, and each pick is recorded
  before the next ranking reads it — **the jitter and the counter are one mechanism, not
  two**, which reclassifies the jitter from mitigation to load-bearing. Also rejected:
  staggering the claim (duplicates the jitter one layer down) and `LIMIT`ing the
  promotion batch (converts a rare collision into a guaranteed delay for every wave's
  tail). Accepted residual: two promotions closer than one claim round-trip can still
  converge — a race that **predates this PRD**, which PRD #111 accepted explicitly, made
  likelier by the correlation and bounded by the jitter; if it bites, the knob is the
  jitter RANGE and the signal is `SweepResult.LimitPromoted > 1` on one tick. Recorded as
  **ADR-35 D4**, since it is a consequence of D2 rather than of a milestone.

  **Two inventory corrections in the same pass.** Decision 3's already-satisfied list
  named the health detector's SQL allowlist but **none of its two hand-written
  mirrors** — `web/src/lib/runBadge.ts:138` and `api/internal/slacksvc/health.go:51`,
  both positive allowlists, both already correct, both now listed so M4 and M5 neither
  rediscover them as gaps nor "fix" them into bugs. And `.env.example` was owned by no
  milestone; assigned to **M5**, since every comparable run knob appears in both it and
  `docs/configuration.md`.

- 2026-07-27 — **Both M0 open questions ruled BY THE USER**; both went the way the
  architect recommended.

  **Q1 — the per-run opt-in surface: run-view toggle, no start-run modal.**
  Escalated to the user rather than decided by the team, because it **reinterprets a
  requirement the user stated in issue #35** — *"each user can opt in per run or set a
  default in settings"* — and because the PRD's own prescription named a surface that
  does not exist (`startRun` is a direct `api.createRun` call from
  `Board.tsx:211-215` and `IssueView.tsx:70-75`; there is no modal to add a checkbox
  to, and adding one changes existing behaviour on every hand-started run). The user
  was shown the coverage argument and took it: a modal can only ever reach
  human-started runs, while autopilot, `ci_fix` and `self_improve` runs have **no
  start affordance at all** — and `ci_fix` and `self_improve` both park (Decision 14).
  The toggle is therefore a **strictly larger** surface than the modal it replaces,
  not a reduced one. Start stays one click and inherits the user default;
  `wait_on_limit` ships on `POST /api/runs` for CLI/API callers; the web's per-run
  control is a run-view toggle on a non-terminal run, which changes future limit
  behaviour only and never the current status. Full semantics in Decision 7; M3's
  scope updated (modal dropped, `PUT /api/runs/:id/wait-on-limit` added).

  **Q2 — `RUN_LIMIT_MAX_WAITS` stays at 5.** It is a *retry* budget, not a
  credential-count budget; an operator with a large pool raises it via env. The
  documented ≈40-day worst case (5 × 8d, Decision 5) is unchanged.

  **Provenance, for the spec-keeper.** Q1 is a **user-confirmed reinterpretation of a
  user-stated requirement** — the strongest category, and the one that must not be
  filed as an AI design decision. The requirement text belongs to `specs/human.md`
  (never edited without user approval); the *mechanism* chosen to satisfy it (run-view
  toggle, endpoint shape, terminal-run inertness) is ours and belongs in `specs/ai.md`.
  Q2 is a user ruling on a knob's default, not a requirement change.

  **ADR filed**: [`adr/0035-run-limit-retry.md`](../adr/0035-run-limit-retry.md)
  records Decision 6e — the ruling, the two rejected options, and specifically the
  "unbuildable as written" reasoning (promotion is strictly upstream of the claim
  where re-selection happens), which is the part not recoverable from the outcome. It
  is deliberately **not yet linked from `ARCHITECTURE.md`**: that file describes what
  uzi *is*, and nothing here is built. The link lands with the archival to
  `prds/done/`.
