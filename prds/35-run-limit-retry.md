# PRD #35: Retry after Anthropic usage limit — park runs until the limit resets

**GitLab Issue**: [vtmocanu/uzi#35](https://gitlab.example.com/vtmocanu/uzi/-/issues/35)
**Status**: Draft — reviewed 2026-07-12 by 3 agents (design review, security audit, fact-check); all blocking/major findings folded in below (marked ↳review where the design changed). Fact-check: every claim verified; four citation nits fixed.
**Priority**: Medium
**Created**: 2026-07-12
**Depends on**: PRD #4 (runs/claim/sweeper, done), PRD #42 (worker run slots, done). Related: PRD #40 (token usage — touches the same `mapResult` seam), PRD #47 (run health — a parked run must read as "waiting", not "stalled"), PRD #25 (Slack notice), specs/ai.md §188/§191 (per-user token concurrency budget — the pressure that makes limits likelier; still out of scope here).

## Problem

When a run exhausts the user's Anthropic subscription usage limit (five-hour or
seven-day window), the run dies:

1. The SDK absorbs *transient* 429/overloaded errors itself — internal bounded
   retries honoring `retry-after` (`api_retry` frames, `bridge.mjs` `retryRequest`).
   uzi never sees those. But a *sustained* limit (window reset minutes-to-hours out,
   beyond the SDK's retry budget) surfaces as a terminal error result, and the worker
   collapses every error result into a thrown `agent run failed: <subtype>`
   (`agent/src/sdk-executor.ts:509-522`; for a usage-limit death the subtype is
   `error_during_execution`) → `status=failed` with that string as the whole
   explanation (`agent/src/runner.ts:259-272`). No branch inspects *why*.
2. The SDK tells us everything we'd need and uzi throws it away:
   `mapSdkMessage` handles only `assistant|user|result|system(init)`
   (`agent/src/sdk-messages.ts:135-160`), silently dropping `rate_limit_event`
   frames (`SDKRateLimitInfo`: `status: 'rejected'`, `resetsAt` epoch,
   `rateLimitType: 'five_hour'|'seven_day'|…`, sdk.d.ts:3984-3999), `api_retry`
   progress, and the result frame's `terminal_reason`
   (`'blocking_limit'|'rapid_refill_breaker'`, sdk.d.ts:6553) that names the
   usage-limit death explicitly.
3. Nothing retries a worker-reported failure. Requeue exists only for worker death
   (stale heartbeat / register orphan, `runtime.sql:381-410`), and there is no
   not-before/backoff gate anywhere in the claim path — a requeued run is claimable
   on the next poll.

Issue #35 (user, Romanian): if we hit the limit, retry after a delay — back off until
the limit resets; each user can opt in per run or set a default in settings.

Prior art check (inspiration submodules): none of bottega/multica/dot-agent-deck
handles this — bottega retries once on a stale-OAuth 401 (`retryOn401.ts`), multica's
lease/attempt retry is worker-death recovery ("Agent-side errors … intentionally
excluded"), dot-agent-deck fails on any non-200. PRD #42 Decision 12 explicitly
punted with "the SDK's own retry/backoff is the handler" — correctly scoped to
shared-token 429 contention; this PRD supersedes that stance for sustained limits.

## Solution Overview

1. **Detect**: the worker captures `rate_limit_event` frames (a small observer beside
   `mapSdkMessage`) and, when a turn ends in an error result, classifies it as a
   **limit failure** (Decision 1's precedence rules). Everything else keeps today's
   failure path.
2. **Park (opt-in)**: instead of failing, the worker reports a new run status
   **`limit_wait`** carrying `limit_resets_at` + `rate_limit_type`, **preserving the
   per-run SDK home and worktree on disk** (↳review BLOCKING — see Decision 6). The
   run's worker slot is released (the execution ends, unlike the slot-holding
   `awaiting_approval` gate); the SDK session id is already persisted on every state
   report (`runs.session_id`, `00020_workers_runs.sql:40`).
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
   resume countdown; the feed gets a `limit_wait` message; Slack (when enabled)
   posts a one-line thread notice.

## Design Decisions

1. **Detection pins to structured SDK signals, never text — `terminal_reason` is
   primary (↳review N5).** The `"Claude AI usage limit reached|<epoch>"` string is a
   Claude Code product string absent from `@anthropic-ai/claude-agent-sdk` 0.3.201
   (verified by grep of `sdk.mjs`/`bridge.mjs`). Classification of an error result:
   (a) `SDKResultError.terminal_reason ∈ {blocking_limit, rapid_refill_breaker}`
   (sdk.d.ts:4015, 6553) ⇒ limit failure; (b) absent a limit-shaped
   `terminal_reason`, the latest captured `rate_limit_event` with
   `status:'rejected'` counts **only when corroborated by a future `resetsAt`** —
   a stale 'rejected' event followed by an unrelated death (e.g. `error_max_turns`)
   must not park the run. `rate_limit_event` is a top-level member of the
   `SDKMessage` union (sdk.d.ts:3772), so the observer sees it on the normal query
   iterator. The observer records `resetsAt`, `rateLimitType`, and
   `overageResetsAt`; `resetsAt` is a bare `number` in the typings — normalize
   seconds-vs-milliseconds defensively (`< 10^12 ⇒ seconds`) and treat an absent or
   past value as "unknown".
2. **Transient vs sustained boundary is the SDK's own retry budget.** `api_retry`
   frames show the SDK retrying with `retry-after` honored; uzi acts only when the
   turn *terminates* in a limit-classified error result. No uzi-side retry loop
   inside a turn — the 10m in-turn idle timer (`RUN_IDLE_TIMEOUT`,
   `sdk-executor.ts:49,467`) stays untouched. Optional nicety: map `api_retry` to a
   terse feed line so a visibly-retrying run doesn't look hung (helps PRD #47 too).
3. **A new non-terminal status `limit_wait`, not a reuse of `queued` and not a
   worker-held park.** Reusing `queued` gives no not-before gating and hides the
   state from the user; holding the slot like `awaiting_approval` would pin a worker
   slot for up to days and still get reaped: the server sweeper fails `running` runs
   past RUN_TIMEOUT=2h (`runtime.sql:358-366`) and five_hour/seven_day resets exceed
   that. `limit_wait` is: added to the status CHECK
   (`00020_workers_runs.sql:36-37` — still the inline `00020` constraint; later
   migrations only touched `runs_kind*`), excluded from every sweeper fail/requeue
   filter (they match `claimed|running|awaiting_approval` — already exclusive,
   pinned by test; `SweepIdleChatRuns` is kind-scoped and can't touch it), counted
   as active by `uq_runs_one_active_per_issue` (intended: no second run on the
   issue while one is parked), and mapped in the board lifecycle
   (`runlifecycle/lifecycle.go:143-172`): **notifier** no-op (card doesn't move),
   **reconciler** `limit_wait → In Progress with act:true` (↳review N3 — an
   act:false default would leave a pending move marker unhealed and trip the 30m
   give-up warn).
4. **The park is server-timed, worker-free, and server-sanitized.** Worker reports
   `{status: "limit_wait", limit_resets_at, rate_limit_type}` via the existing state
   endpoint (the `RunState` union at `agent/src/protocol.ts:15-19` gains the status;
   the `StateRequest` interface at `protocol.ts:492-524` gains the fields;
   `workersvc.SetState` dispatch `service.go:753-808` gains a case — the default arm
   rejects unknown statuses today, so this is required wiring). The execution then
   ends and the slot frees (PRD #42 concurrency benefits). Server-side handling:
   - `rate_limit_type` is **allowlisted against the SDK union**
     (`five_hour|seven_day|seven_day_opus|seven_day_sonnet|seven_day_overage_included|overage`),
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
     The clamp is server-side because the reset is worker-reported and a
     compromised worker must not park a run for years.
5. **Bounded, idempotent, source-guarded parks.** `runs.limit_wait_count` increments
   per park; `RUN_LIMIT_MAX_WAITS` (env, default 5) caps it — the N+1th limit
   failure fails the run ("usage-limit retry budget exhausted"). The
   `SetRunLimitWait` transition applies **only from `status='running'`** (↳review
   N4 + audit m-1): the ownership query filters id+worker_id only
   (`runtime.sql:177-179`), so without a source guard a worker could park an
   `awaiting_approval`/affinity-`queued` run, and a re-delivered state report would
   double-bump the counter — `running`-only makes re-delivery a 0-row no-op and
   legit re-parks always come from `running`. Parking does **not** touch
   `requeue_count` (mirrors the vault lock-race precedent, specs/ai.md §139): the
   counters stay semantically distinct — requeue_count = worker deaths,
   limit_wait_count = limit parks. Documented worst case (↳audit m-2): the caps
   compose to ≈ 40 days (5 × 8d) that one run can hold the one-active-per-issue
   lock; cancel-while-parked (Decision 11) is the escape hatch, and a compromised
   worker mislabeling failures as limits buys at most 5 self-scoped retries on its
   own user's token (informational, accepted).
6. **Park preserves the on-disk session; resume rides the claim machinery
   (↳review BLOCKING B1 + MAJOR M2 — this decision was rebuilt).**
   - **(a) Cleanup carve-out.** `RunRunner.execute`'s `finally` unconditionally
     removes the per-run SDK home (`agent-home/<runId>` — where the resumable
     session transcript lives) and the worktree on any normal return
     (`agent/src/runner.ts:273-301`). As first drafted, every park would have
     resumed into a fresh session. The park path is carved out of terminal cleanup:
     on a `limit_wait` report the runner returns **without** removing the run home
     or the worktree (exactly the state a worker-death requeue leaves behind), so
     `options.resume` (`sdk-executor.ts:314,451`) finds the transcript on resume.
     Disk is bounded by the park caps; a run that later ends terminally cleans up
     as today.
   - **(b) Resume skips the plan gate when the plan was already approved.** The
     executor unconditionally runs Phase-1 planning + `gatePlan` regardless of
     `ctx.sessionId` (`sdk-executor.ts:316-348`), so a naive resume would
     re-generate a plan, re-park the run at `awaiting_approval` for a human, and
     could even fail with `REASON_NO_PLAN` when the resumed session declines to
     re-emit `signal_plan`. Fix: the claim payload gains `plan_approved` (derived
     server-side: a consumed `approve_plan` input row exists for the run, OR
     `auto_approve`) plus the run row's `plan_md`. On resume with
     `plan_approved && session resumable`, the worker skips Phase 1 and the gate
     and enters the implement⇄review loop seeded with the delivered plan. A park
     *before* approval (during planning or at the gate — only reachable via the
     planning turn itself dying on a limit) resumes into planning as today.
   - **(c) Affinity actually favors the prior worker.** Promotion resets
     `updated_at`, so `ClaimRun`'s affinity grace window **restarts** at promotion
     (`runtime.sql:200-203` — predicate + ORDER BY prefer the prior worker); the
     first draft's "grace will have long expired" was wrong (↳review). Honest
     residual: if another of the user's workers claims after the fresh 2m grace,
     the session directory is absent there ⇒ fresh session + branch-attach; with
     (b)'s `plan_approved` skip this still bypasses the gate but loses conversation
     context — acceptable, and most users run one worker.
   - **(d) Clocks.** Promotion (`limit_wait → queued`) sets `started_at = NULL`
     (new design intent — the analogous `RequeueClaimedRunToQueued` never needed
     to, since `claimed` runs have no `started_at`) so the resumed run gets a fresh
     RUN_TIMEOUT wall; `session_id`, `last_seq`, `worker_id` are left untouched, as
     in every existing requeue query.
7. **Opt-in: per-run flag defaulted by a per-user setting.**
   - `users.wait_on_limit` boolean NOT NULL DEFAULT false; `PUT /api/me/wait-on-limit`
     + Settings toggle (exact clone of the autopilot-enabled plumbing,
     `users.sql:43-47`, `handler/autopilot.go`; inherits RequireAuth→ValidateCSRF).
   - `runs.wait_on_limit` boolean NOT NULL DEFAULT false, stamped at creation:
     `CreateRun` body (today `{issue_iid}` only, `handler/workers.go:408-410`) gains
     optional `wait_on_limit`; absent ⇒ user default. Autopilot-created runs take
     the user default. The start-run modal shows a pre-checked/unchecked box.
   - The flag rides the claim payload like `auto_approve` (top-level, re-read from
     the row on every claim) so a resumed run keeps its opt-in.
   - `app_settings` is not used — it is admin/instance-wide only ("there is no
     user_settings table", `00044_slack.sql:5`); the caps are env config like the
     other run clocks.
8. **Opt-out runs still get a better death.** Even with `wait_on_limit=false`, a
   limit-classified failure reports
   `failure_reason = "Anthropic usage limit (<type>) reached; resets at <ISO>"`
   (type from the allowlisted enum, no secrets involved) and emits a `limit_hit`
   feed message — strictly better than today's `agent run failed:
   error_during_execution`.
9. **Chat lane: message only, no auto-retry.** Chat already survives an erroring
   turn (parks for the next user message, `chat-executor.ts:455-486`). The same
   classifier upgrades the feed line to "usage limit reached; resets at <t>" so the
   user knows why their chat went quiet. Auto-retrying a conversation turn is out of
   scope.
10. **Feed + UI + DTO plumbing (↳review N1 — the Go DTO edit is hand-maintained and
    now named).** New run_message kinds `limit_wait` (payload: resets_at,
    rate_limit_type, attempt) and `limit_hit` — `run_messages.kind` carries no DB
    CHECK (PRD #39 Decision 8, its ↳review D12 thread), rendered with explicit cases
    in `RunEvent.tsx`/ActivityFeed (escaped, no raw sinks). The new run fields
    (`retry_not_before`, `limit_resets_at`, `limit_wait_count`, `wait_on_limit`)
    must be added to the hand-maintained `runDTO`/`runToDTO`
    (`api/internal/handler/workers.go:128-262`) and the web `Run` interface
    (`web/src/lib/api.ts:518-577`). `RunStatus` union, `RUN_STATUS_TONES` (warn),
    and the run view header get `limit_wait` with a countdown to
    `retry_not_before` and "attempt N/cap". Unknown-status fallback means a
    half-shipped web build degrades to a neutral pill, not a crash (`ui.tsx:207`).
11. **Cancel while parked works server-side (↳review MAJOR M1 — needs a
    `hasLivePoller` change, not just SQL).** A parked run keeps `worker_id` and its
    worker keeps heartbeating for other runs, so `hasLivePoller`
    (`service.go:1336-1351`) would report a live poller and `SubmitInput` would
    enqueue a cancel that nothing consumes until resume. Fix: `hasLivePoller`
    treats `status='limit_wait'` like `'queued'` (no live steering by definition),
    and `CancelRunServerSide` (`runtime.sql:309-317`) is extended to cover
    `limit_wait`. This is the escape hatch for the multi-day park (Decision 5's
    worst case). Approve/reject/follow-up inputs queue as today and deliver on
    resume.
12. **Vault gate composes.** Promoted runs re-enter `queued` and remain subject to
    the vault-unlock claim gate (PRD #32): a run whose reset elapsed while the vault
    is locked simply waits in `queued` like any other run. No special case.
13. **Migration numbering**: draft `00095_limit_wait.sql` (collision-avoidance only;
    renumber above the live head at landing per CLAUDE.md — live head today is
    `00056_oidc.sql`, and PRD #50 has reserved `00057`). One migration: runs columns
    (`wait_on_limit`, `limit_resets_at`, `retry_not_before`, `limit_wait_count`),
    users column (`wait_on_limit`), status CHECK widened with `limit_wait`, plus a
    partial index `(retry_not_before) WHERE status = 'limit_wait'` for the
    promotion pass (↳audit m-3 — cheap now, moot to retrofit).

## Milestones

- [ ] **M1 — Worker detection + park path**: rate-limit observer beside
  `mapSdkMessage` (captures latest `rate_limit_info`), classification per Decision 1
  precedence (terminal_reason primary; corroborated-rejected secondary), typed
  `LimitReachedError` with normalized `resetsAt` + `rateLimitType`, runner branch:
  opt-in ⇒ report `limit_wait` **and skip run-home/worktree cleanup** (Decision 6a),
  opt-out ⇒ improved `failure_reason` + `limit_hit` message; resume-skips-gate path
  (Decision 6b, consuming claim `plan_approved`/`plan_md`); chat feed-line upgrade;
  unit tests with real-shaped SDK frames (rejected event + corroborating resetsAt,
  blocking_limit result, stale-rejected + error_max_turns NOT classified, transient
  api_retry NOT classified, seconds-vs-ms normalization, cleanup carve-out).
- [ ] **M2 — Schema + server park machinery**: migration (Decision 13),
  `SetRunLimitWait` transition (source-guarded `running`-only, idempotent on
  re-delivery, allowlists `rate_limit_type`, computes/clamps `retry_not_before`,
  bumps `limit_wait_count`, enforces `RUN_LIMIT_MAX_WAITS` / `RUN_LIMIT_MAX_PARK`
  fail-instead-of-park), sweeper promotion pass (`limit_wait → queued`,
  `started_at=NULL`, no `requeue_count` bump), `hasLivePoller` +
  `CancelRunServerSide` extensions (Decision 11), claim payload gains
  `wait_on_limit` + `plan_approved` + `plan_md`, DTO fields (Decision 10),
  sweeper-exclusion tests (a parked run is never RUN_TIMEOUT-failed or
  stale-requeued; cancel-while-parked applies immediately).
- [ ] **M3 — Opt-in surfaces**: `users.wait_on_limit` + `PUT /me/wait-on-limit` +
  Settings toggle; `CreateRun` wire + start-modal checkbox defaulting from the user
  setting; autopilot runs inherit the default; tests (default inheritance, explicit
  override, flag survives resume).
- [ ] **M4 — Web UX**: `RunStatus`/tones/`limit_wait` pill, run-view countdown strip
  (resets-at + attempt N/cap), runs list + board card badge, `limit_wait`/`limit_hit`
  render cases in RunEvent/ActivityFeed; vitest coverage incl. escaped rendering.
- [ ] **M5 — Slack + docs + specs**: notifier gains `statusLabel` + `renderThread`
  cases for `limit_wait` (else a park edits the root to raw "limit_wait" and posts
  nothing) and `GetSlackRunContext` selects the reset timestamp for the
  "paused: usage limit (five_hour); resumes ~<t>" line — `EscapeMrkdwn` applied
  per-field even though the type is allowlisted upstream; `docs/` page section +
  `docs/configuration.md` for the new envs; `specs/ai.md` design record;
  `specs/human.md` addition proposed to the user.
- [ ] **M6 — E2E**: stub-executor scenarios — opt-in park → sweeper promotes (short
  test clock) → resume **skips the gate** and completes; opt-out fails with
  explanatory reason; cancel-while-parked cancels immediately; full-stack smoke.

## Success Criteria

- A run that hits a sustained usage limit with opt-in enabled ends up `completed`
  after the window resets, with zero human intervention: SDK session resumed (same
  worker) and no second trip through the plan gate for an already-approved plan.
- A parked run is visibly "waiting" in web (countdown) and Slack, never "stalled",
  and is never killed by RUN_TIMEOUT or stale-worker sweeps.
- Cancelling a parked run takes effect immediately (server-side), not on resume.
- Opt-out runs fail with a reason that names the limit type and reset time.
- Transient 429s behave exactly as today (SDK-internal, invisible except optional
  feed lines).
- A compromised worker cannot park a run beyond `RUN_LIMIT_MAX_PARK`, bypass the
  park-count cap, park a run not in `running`, or smuggle a non-enum
  `rate_limit_type` past the server.

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
