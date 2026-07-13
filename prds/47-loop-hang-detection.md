# PRD #47: Loop/hang detection — flag slow, stalled, or looping runs in UI and Slack

**GitLab Issue**: [#47](https://gitlab.example.com/vtmocanu/uzi/-/issues/47)
**Status**: In progress — M1-M4 implemented, reviewed, and audited clean (branch `feature/prd-47-loop-hang-detection`, tip bc83815 + M6 in flight); M6 e2e running, M5 docs+specs next. Migration landed as `00057` (PRD draft said 00054; head had moved). Pre-implementation review 2026-07-12 by 3 agents (design, security, fact-check); all blocking/major findings folded in below (marked ↳review where the design changed).
**Priority**: High (plan.md line 68)
**Created**: 2026-07-12
**Depends on**: PRD #4 (runs/sweeper), PRD #19 (app_settings), PRD #25 (Slack) — all done

## Problem

Every existing liveness mechanism is a terminal kill switch, and every one has a blind
spot that leaves a sick run looking healthy until it dies (or forever):

- `RUN_TIMEOUT` (default 2h) fails the run only after the full wall clock elapses —
  both server-side (sweeper, `api/internal/workersvc/service.go:1333`) and
  worker-side (`agent/src/sdk-executor.ts:528`, armed at :520). No signal at minute
  30 that something is off.
- `RUN_IDLE_TIMEOUT` (default 10m) is worker-side only and re-arms on **every** SDK
  message (`agent/src/sdk-executor.ts:436-441`, re-armed at :455). An agent
  busy-looping — re-running the same failing command, re-reading the same file —
  emits a steady message stream and never trips it. Nothing anywhere detects
  repetition.
- A `queued` run is never swept — no sweep query selects `status='queued'`
  (`api/internal/store/queries/runtime.sql:298-371`; deliberate, see the vault-gate
  comment at `service.go:314`) — and can sit invisible forever: no worker online,
  no signal.
- `awaiting_approval` has no server-side timeout at all; the worker-side
  `WORKER_PLAN_APPROVAL_TIMEOUT` (24h) rejects the plan at expiry, but nobody is
  reminded meanwhile.

plan.md line 68: "loop/hang detection — if something is taking too long flag it (in
ui and on slack) or if it seems stuck for too long."

## Solution Overview

A server-side **run-health detector** in the existing sweeper tick (15s,
`api/internal/sweeper/sweeper.go`) computes per-run signals from telemetry we already
persist, and sets a **non-terminal, self-clearing flag** on the run:

| Signal | Condition (defaults) | Flag |
|---|---|---|
| **stalled** | `running`, no new run_message for 5m, and no tool call in flight | ⚠ stalled |
| **looping** | `running` and ≥4 identical (name+input) `tool_use` calls among the last 12 | ⚠ looping |
| **slow** | `running` and `now - started_at` > 45m (soft cap, < RUN_TIMEOUT) | ⚠ slow |
| **stuck queued** | `queued` for > 10m | ⚠ waiting for worker |
| **approval idle** | `awaiting_approval` for > 1h (skipped for `auto_approve` runs) | ⏸ still needs approval |

The flag surfaces as:
1. **Web UI**: a warn-tone badge via the central `runBadge()` taxonomy
   (`web/src/lib/runBadge.ts:97-131`) so board cards, dashboard, runs list, and run
   view all pick it up from one place; owners see the reason.
2. **Slack**: the existing per-owner DM anchor (`api/internal/slacksvc/notifier.go`)
   gets a cooldown-limited threaded nudge and the root line's status label gains a ⚠
   variant. Clearing the flag edits the root back; no nudge spam.

The flag **clears itself** when the condition goes away (new activity, approval given,
worker claims the run) and is **reset on every transition out of `running`**
(↳review). No run is ever killed by this PRD — the existing watchdogs keep that job;
this is early warning only.

**Security posture** (↳review): health detection is an operability aid for
honest-but-stuck agents, **not a guardrail**. A hostile worker can suppress
`stalled` by emitting junk messages and evade `looping` by varying inputs; it cannot
forge health state (columns are sweeper-written server-side) or touch other users'
runs (`AppendMessages` gates on run ownership, `service.go:602`). RUN_TIMEOUT /
idle / iteration caps remain the only liveness backstops.

## Design Decisions

1. **Detection is server-side, in the sweeper — zero worker changes.** The worker
   already ships every discrete agent block to the API (`run_messages`, kinds
   `text|thinking|tool_use|tool_result|status|error|user_message|plan` — documented
   in the schema comment at `migrations/00020_workers_runs.sql:74`, not
   CHECK-enforced), including full tool names + inputs
   (`agent/src/sdk-messages.ts:56-66`). Everything the detector needs is in
   Postgres. Alternative rejected: worker-side loop detection — it dies with the
   worker, can't see `queued`/`awaiting_approval`, and the worker is the component
   we trust least to report its own sickness.

2. **`last_activity_at` column on `runs`, bumped inside the existing
   `AppendMessages` write path.** Today the finest activity marker is
   `max(run_messages.created_at)`; `runs.updated_at` deliberately does not advance
   per message (`api/internal/store/queries/runtime.sql:291-294`). Scanning
   `run_messages` per sweep for every active run is an avoidable per-tick aggregate;
   `AppendMessages` already calls `UpdateRunLastSeq` per batch (`service.go:636`),
   so the column is free — and the bump stays behind the existing
   `runOwnedByWorker` check; no standalone activity-bump endpoint is added
   (↳review). Migration (draft `00054` — `00053` is the next free number above the
   live head `00052` but is reserved by parallel PRD #45; renumbered at merge per
   convention) adds `last_activity_at timestamptz`, backfilled from `updated_at`.
   The stalled signal is `now - GREATEST(last_activity_at, started_at) > threshold`
   — `started_at` guards the checkout window before the first message.

3. **Health state is columns on `runs`, with an explicit lifecycle contract**
   (↳review — the exit contract was the review's blocking finding):
   `health TEXT NOT NULL DEFAULT 'ok'` (CHECK: `ok|stalled|looping|slow|waiting_worker|approval_idle`),
   `health_reason TEXT`, `health_since timestamptz`, `health_notified_at timestamptz`.
   - **Single writer**: only the sweeper's detector writes `health`/`health_reason`/
     `health_since`/`health_notified_at` while a run is in a flaggable status.
   - **Exit contract**: every query that moves a run out of its flaggable status
     resets `health='ok'`, `health_reason=NULL`, `health_since=NULL` —
     `SetRunCompleted`/`SetRunFailed`/`SetRunAwaitingApproval`
     (`runtime.sql:227-257`), `RequeueRunsOfStaleWorkers` (:342-351),
     `RequeueWorkerRuns` (:365-371), `SweepClaimedNeverStarted` (:298-305), claim.
     This closes the sweeper-vs-worker write race (sweeper flags, worker completes:
     whichever write lands last leaves a consistent row — the status write clears,
     the health write is status-scoped `WHERE status='running' AND …` and no-ops
     after the exit). `health_notified_at` is NOT cleared — it is a rolling
     last-nudge stamp (Decision 7).
   - **Render scoping as belt-and-braces**: `runBadge` renders the warn variant only
     for flaggable statuses, and the Slack root label appends ⚠ only when the
     context row is still in one.
   - Exactly one flag at a time, priority `looping > stalled > slow` (looping is the
     strongest evidence of pathology). Orthogonal to `status`. Alternative rejected:
     an events/episodes table — history can be reconstructed from Slack threads and
     run_messages timestamps; nothing queries past episodes yet.

4. **Loop detection = hash of the last N tool calls; the hash is compared, never
   surfaced.** Per active `running` run, the detector loads the last 12 `tool_use`
   rows (`ORDER BY seq DESC LIMIT 12` on the existing `(run_id, seq)` unique
   index), hashes `sha256(name + canonical-JSON(input))` in Go, transiently, and
   flags when any hash count ≥ 4. Window/threshold are code constants, not settings.
   **Neither the hash nor any tool name/input/repo content ever appears in
   `health_reason`, logs, or Slack** — run_messages are secret-scrubbed by the
   worker (`agent/src/batcher.ts:46-48`) but NOT scrubbed of repo content, so reason
   strings are fixed templates carrying only durations/counts (↳review, audit
   major). Accepted misses (documented, not solved here): A/B-alternating loops
   need window fill to flag; semantically-equivalent-but-textually-different inputs
   are not caught (LLM-judge territory, plan.md line 91); after a requeue/resume,
   pre-requeue calls remain in the window (gapless `seq`), so a fresh resume can
   re-flag briefly until new distinct calls push them out (↳review). False-positive
   guard: legitimate repetition (polling `go test` between distinct edits) is broken
   up by the interleaved distinct hashes.

5. **Thresholds are `app_settings` keys, runtime-tunable, no new env vars.** Per the
   PRD #19 decision (`api/internal/settings/settings.go` Defaults + typed accessors):
   `health_enabled` (default `"true"`), `health_stall_seconds` (300),
   `health_slow_seconds` (2700), `health_queued_seconds` (600),
   `health_approval_seconds` (3600), `health_nudge_cooldown_seconds` (1800). Admin
   Settings page (`web/src/pages/AdminSettings.tsx`) gets a "Run health" card.
   Two-layer validation (↳review — `Validate()` is pure with no config access, so
   the RUN_TIMEOUT rule can't live there):
   - **Write time** — net-new integer validator in `Validate()`: value must be in
     `{0} ∪ [60, 86400]`; `0` disables that signal; negatives, non-integers, 1–59,
     and >86400 rejected (the upper bound stops a fat-fingered value silently
     disabling a signal).
   - **Read time** — the sweeper's accessor clamps `health_slow_seconds` to
     `< RUN_TIMEOUT` (env, can change across restarts) and logs when it clamps.

6. **Health fields ride the run DTOs, but `health_reason` is owner-gated on the
   shared board** (↳review — audit blocking finding). The board `latestRunDTO` is
   delivered to every authenticated viewer for every card and already gates
   `failure_reason` behind `IsMine` (`api/internal/handler/board.go:118-123`,
   owner-only per PRD #33 Decision 5); `health_reason` (which can say "owner vault
   is locked") follows the same rule. The `health` enum + `health_since` are
   unconditional (non-sensitive, like `stop_kind`). The owner-scoped `runDTO`
   (`api/internal/handler/workers.go:160`, owner-only ListRuns + admin-only
   AdminListRuns) carries `health_reason` unconditionally, matching
   `failure_reason`. Web `LatestRun` type (`web/src/lib/api.ts:289`) gains the three
   fields.

7. **Slack: a dedicated health event, sweeper-owned nudge stamp, cooldown instead of
   per-episode** (↳review — `PublishState` carries only `status`, and a
   status-`running` publish is indistinguishable from an ordinary heartbeat, so
   health gets its own seam):
   - The `Broadcaster` interface gains `PublishHealth(runID, health, reasonCode)`;
     the live hub maps it to a WS run-update so browsers repaint within a tick; the
     Slack notifier enqueues a root re-render and, when the event is marked
     nudge-worthy, one threaded nudge.
   - **Single writer for `health_notified_at`**: the sweeper stamps it when (and
     only when) it emits a nudge-worthy event — on the ok→flagged transition, and
     only if `health_notified_at` is NULL or older than
     `health_nudge_cooldown_seconds`. The stamp persists across episodes and API
     restarts, which both bounds restart duplicates and damps flapping: a run
     oscillating around a threshold re-flags (episodes are inherently ≥ one
     threshold-width apart, since the condition must re-accumulate) but DMs at most
     once per cooldown window (↳review — replaces "once per episode").
   - **Delivery rules re-resolved per event** (↳review, audit major): every health
     post/edit goes through `GetSlackDeliveryForUser` exactly like `handle()` today
     (`notifier.go:133-143`) — the persisted `slack_run_messages` anchor is never
     sufficient on its own, so a user who opts out mid-run gets nothing.
     `GetSlackRunContext` (`queries/slack.sql:97-119`) is extended to select the
     health columns.
   - **String hygiene** (↳review): nudge text uses fixed templates, every
     forge-controlled field passes `EscapeMrkdwn`, and the whole message passes
     `ScrubSecrets`, same as `renderRoot` (`notifier.go:238-247`). Content stays
     minimized: reason label only, never run-message content (`PublishMessage`
     remains a no-op). Bounded queue, drop-don't-block, unchanged.
   - `approval_idle` nudges thread under the existing gate message so
     Approve/Reject buttons are one scroll away.

8. **`queued` stays unswept; it gains only a flag. `claimed` deliberately gets no
   flag** (↳review — made explicit): a wedged-but-heartbeating checkout is already
   requeued by `SweepClaimedNeverStarted` at `ClaimGrace` (5m), which is tighter
   than any flag would be. For queued runs, `health_reason` distinguishes what the
   server can see: "no worker online" (zero online workers) vs "owner vault is
   locked" (most actionable) vs "waiting for worker". Runs with `auto_approve` are
   excluded from the `approval_idle` signal — autopilot self-resolves its gate and
   must not nudge anyone to approve it (↳review).

9. **The stalled signal is suppressed while a tool call is in flight** (↳review —
   design major). `last_activity_at` only advances on new messages, and a long
   `go build`/test-suite/provision emits one `tool_use` then nothing until its
   `tool_result` — the worker's own idle watchdog re-arms per message and sits at
   10m for exactly this reason. So: if the newest message for the run is a
   `tool_use` with no matching `tool_result`, the run is *working*, not stalled —
   no flag, regardless of elapsed time (the wall-clock `slow` signal still covers
   pathological single calls). Detection: compare the newest `tool_use.id` against
   existing `tool_result.tool_use_id` payloads in the same window fetch as loop
   detection — no extra query.

10. **UI: one taxonomy change, four surfaces for free, plus a board strip.**
    `runBadge()` grows a warn variant: flagged runs in flaggable statuses render
    `⚠ <label> · <elapsed>` with a `warn` tone and keep the pulse. Board
    (`web/src/pages/Board.tsx`), dashboard, runs list, and run view all render
    through it already. The board's existing awaiting-approval attention strip
    (`Board.tsx:395-400`) generalizes ("1 run needs approval · 2 runs look stuck").
    Run view header shows `health_reason` (owner/admin) next to the LIVE STAGE
    label; `formatElapsed` reused for "stuck for Xm" from `health_since`.

11. **Detector cost stays O(active runs) per tick.** The status-age signals
    (stalled/slow/queued/approval) are one cutoff-driven, parameter-bound
    `UPDATE … RETURNING` each (same shape as existing sweep queries; thresholds
    always bound params, never string-formatted — ↳review); they no-op to zero rows
    in the healthy case. Loop detection is the exception (↳review — costlier by
    design): a per-`running`-run `LIMIT 12` window fetch + in-Go hashing, bounded
    by laptop-scale single-digit active runs. Settings read through the existing 5s
    cache.

## Milestones

- [x] **M1 — Schema + detector core** (9f97e38, +follow-up 4abe70c; reviewed+audited clean): migration (`last_activity_at`, `health`,
  `health_reason`, `health_since`, `health_notified_at`), `AppendMessages` bump,
  sweeper detector for stalled (with in-flight suppression)/slow/queued/approval
  signals + self-clear, **exit-transition resets in every status-leaving query**
  (the worker-path queries in `runtime.sql`, not just the sweeper — review
  blocking), settings keys with the `{0} ∪ [60, 86400]` validator + read-time
  RUN_TIMEOUT clamp, unit tests (clear-on-resume, threshold-disable, exit-race,
  auto_approve exclusion).
- [x] **M2 — Loop detection** (22b7b8f; reviewed+audited clean): tool_use window hashing, in-flight detection from
  the same window, priority ordering, tests with real-shaped payload fixtures
  (identical inputs, A/B alternation, interleaved-distinct false-positive,
  resume-boundary re-flag).
- [x] **M3 — API + web** (77c6bcd + 4abe70c + bc83815; reviewed+audited clean, web-ux browser-validated on an isolated stack — dashboard/runs-list coverage added via shared RunHealthBadge beyond the runBadge-only plan): health fields in run DTOs with **board `health_reason`
  gated behind `IsMine`** (audit blocking), `PublishHealth` on the broadcaster +
  live-hub WS mapping; `runBadge` warn variant, board strip generalization,
  run-view reason, admin settings "Run health" card; vitest coverage incl. a
  non-owner-sees-no-reason test.
- [x] **M4 — Slack nudges** (8169434; reviewed+audited clean — nudge-worthiness judged detector-side, stamped via COALESCE in the same SetRunHealth write): `PublishHealth` handling in the notifier — root-label
  flip + cooldown-gated threaded nudge + clear edit, per-event
  `GetSlackDeliveryForUser` re-resolution, `EscapeMrkdwn` + `ScrubSecrets` on all
  new strings, approval-idle threading under the gate message; tests for opt-out
  mid-run, cooldown, restart-no-dupe.
- [ ] **M5 — Docs + specs**: user-facing doc page (what the flags mean, what to do,
  how to tune thresholds, **explicit "this is not a guardrail" note**),
  `docs/configuration.md` + `docs/slack.md` updates, `specs/ai.md` design record;
  `specs/human.md` addition proposed to user.
- [ ] **M6 — E2E verification**: e2e scenario with the stub executor forcing a
  stall and a loop; full-stack smoke that flags appear, nudge once, and clear.

## Success Criteria

- A run that stops emitting messages (with no tool in flight) shows ⚠ in
  board/dashboard/run view within ~5m + one sweep tick, and its owner gets exactly
  one Slack nudge per cooldown window; when activity resumes, the flag and Slack
  label clear without human action.
- A long-running single tool call (e.g. a 8m build) is **not** flagged stalled.
- An agent repeating the same tool call 4+ times is flagged `looping` before
  `RUN_IDLE_TIMEOUT` would ever fire (it never fires — that's the point).
- A queued run with zero online workers (or a locked vault) says so, in words, to
  its owner within one sweep of the threshold; non-owners see only the ⚠ enum.
- A run that completes or requeues mid-episode ends with `health='ok'` — no stale
  ⚠ on terminal runs, in DB, UI, or Slack.
- No existing kill behavior changes: RUN_TIMEOUT / idle / iteration caps fire
  exactly as before (regression-tested).
- All signals tunable/disable-able at runtime from Admin Settings; disabled = today's
  behavior exactly.

## Out of Scope

- **Auto-remediation** (killing, requeueing, or messaging the agent to unstick it) —
  existing watchdogs keep the kill job; a future PRD can act on the flag.
- **Semantic loop detection / session judging** (plan.md line 91's LLM judge + inbox).
- **Per-tool-call timeouts** in the worker.
- **Notification inbox** in the web UI — Slack + badges only for now.
- **Historical health analytics** (episode tables, dashboards).
- **Adversarial-worker resistance** — see Security posture note; the flag is not a
  guarantee and must never be leaned on as one.

## Decision Log

- 2026-07-12 — PRD created from plan.md line 68 after codebase fact-finding; design
  chosen: server-side sweeper detection over worker-side (Decision 1), columns over
  episode table (Decision 3), settings over env (Decision 5).
- 2026-07-12 — Review round (3 agents). Design review's blocker (no health lifecycle
  on exit from `running`; sweeper/worker write race) became Decision 3's exit
  contract; its majors added in-flight suppression (Decision 9), the dedicated
  `PublishHealth` seam + single-writer cooldown stamp (Decision 7), and nudge
  cooldown over per-episode. Security audit's blocker (board DTO leaking
  `health_reason` to non-owners) became Decision 6; its majors hardened delivery
  re-resolution, fixed-template reasons, and the `{0} ∪ [60, 86400]` validator.
  Fact-check: 24/25 verified; migration-number wording corrected (00053 reserved by
  PRD #45, draft is 00054).
