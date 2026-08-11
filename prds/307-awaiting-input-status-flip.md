# PRD #307: Flip run status back to `running` after a plan-phase clarification is answered

**Issue**: [#307](https://gitlab.example.com/vtmocanu/uzi/-/issues/307)
**Priority**: High (parked runs report a false state to every surface; the web page actively advises cancelling a healthy run)
**Status**: Planned
**Anchor**: root-cause references below are against `main` @ `ce654073`.

## Problem

When a run parked at `awaiting_input` during the **planning** phase receives its answer, the agent session resumes and works normally, but `runs.status` is never flipped back to `running`. The row then reports `awaiting_input` for the duration of the post-answer planning phase (`updated_at` frozen at the park time, `iteration_count: 0`) while the worker streams tool activity, until the next transition (`gatePlan` -> `awaiting_approval`, another question park, or run failure) finally moves it.

Consequences (all confirmed on the issue's run `85667e44-…`):

1. CLI `uzi run wait` returns immediately on the stale `awaiting_input`; there is no transition to wait for, so a headless caller must poll the feed for `submit_plan` instead.
2. The web run page renders the "needs your answer" badge from the stale status, finds no open question, and shows "The agent's question could not be read … Cancel run" over a healthy run. Following that advice cancels a working run.
3. Slack answering is corrupted: routing keys on `run.Status == "awaiting_input"` (`api/internal/slacksvc/replier.go:260`), so a reply that arrives after the park is already resolved is still accepted as an `answer` and then silently discarded instead of being treated as a follow-up. Confirmed live: a second answer row was recorded with `kind = answer` 8 seconds after the question was already resolved, which is only possible if the row was still `awaiting_input` at that moment.

## Root cause

The `awaiting_input -> running` transition already exists and its guard already passes; nothing ever triggers it on the plan-phase resume.

- `SetRunRunning` (`api/internal/store/queries/runtime.sql:840-845`) admits `awaiting_input -> running` when a consumed `answer` row exists whose `question_id` matches `runs.open_question_id`, and it clears `open_question_id` as part of the same statement. After the answer is consumed, that guard is satisfied.
- But the server only runs `SetRunRunning` when the **worker sends a `running` state report**, and on the plan-phase clarification resume no such report is sent. The worker has exactly two sources of a `running` report:
  - `onSessionId` (`agent/src/runner.ts:716`) fires once per run: it is latched by `state.reportedSessionId` (`agent/src/sdk-executor.ts:702`, set true at `:1700-1701`) and never resets. The seq-76 `init` is a re-init of the *same* session, so `sessionIdOf` yields the already-reported id and the callback does not fire again.
  - `reportIteration` (`agent/src/runner.ts:769-773`) reports `running` with the iteration counter, but it only runs inside the **implement** loop.
- A plan-phase clarification (`sdk-executor.ts` `drivePlanningTurn`, `:1539`; the ask itself is `askUserOrContinue` at `:1575`) resumes into more **planning** turns, so `reportIteration` never runs and `onSessionId` has already latched. Result: no `running` report, the row stays `awaiting_input`. (The codebase already documents this gap: `api/internal/store/awaiting_input_integration_test.go:315-324` and the `SetRunAwaitingApproval` guard comment note that the plan/pre-run path reaches the plan gate WITHOUT an intervening `running` report.)

Why other paths do not hit this:
- An **implement-phase** clarification (`sdk-executor.ts:1276-1313`) self-heals: the next iteration's `reportIteration` reports `running`.
- The **plan-gate approve** path self-heals the same way: the implement loop reports `running` right after approval.

## Solution (Decision)

**Primary fix (worker-side):** emit a `running` state report when a clarification resolves with an answer, so the plan-phase resume gets the same `running` report the implement loop already produces via `reportIteration`. This reuses the existing, tested `SetRunRunning` path, which already clears `open_question_id` and enforces the guard.

Insertion point: the runner's `askUser` (`agent/src/runner.ts:1963`), at the single choke point where a verdict resolves (`settle`, `:2057`). When the resolved verdict is `kind === "answer"`, emit `reportState({ status: "running" })` (fire-and-forget with the existing bounded-retry + `catch`, mirroring `onSessionId` at `:716`). Because `askUser` is shared by both the plan-phase and implement-phase parks, this covers both uniformly; on the implement path it is a harmless idempotent `running -> running` alongside the next `reportIteration`.

By the time `awaitAnswer` resolves, the answer has already been consumed server-side: `ConsumeRunInputs` stamps `consumed_at = now()` in the same `RETURNING` CTE that hands the row to the worker (`runtime.sql:1837-1842`), so the poll response cannot exist without the stamp, and the worker only routes/resolves after the poll returns. The report therefore always lands after `consumed_at` is set and the `SetRunRunning` guard is satisfied. A retry-delayed or reordered copy of this report is additionally fail-closed by the identity-keyed guard (a report for a superseded question does not match `open_question_id`; `awaiting_approval`/`limit_wait` are excluded), so the extra fire-and-forget report introduces no new reordering hazard.

**Run-kind scope:** `issue` and `ci_fix` runs share `RunRunner.askUser`, so the fix covers both uniformly. `chat` runs use a separate `ChatRunner`/`ChatSteering` with no clarification park, so they are unaffected.

**Must NOT do:** add a new server-side status-transition site (e.g. flipping in the consume path). `runtime.sql` warns that any new non-terminal transition destination must carry its own `open_question_id` clear ("a third non-terminal destination added later needs its own"), and getting that wrong re-opens the stale-answer identity bug. The worker-side fix avoids the hazard entirely by routing through the one existing clear.

**Explicitly out of scope (optional hardening, deferred):** a server-side flip when the matching answer is consumed, which would close the tiny window where a worker dies between consuming the answer and sending the report (the run self-heals on requeue today). Not needed for correctness; revisit only if that window is ever observed to matter.

Do not emit the `running` report on the `cancel` verdict (the run is being cancelled) or on the timeout path (it throws). Only a successful `answer` verdict flips the status.

## Milestones

Sequential (single component, no parallelism). Small fix; milestone count reflects that rather than padding.

- [x] **M1 — Worker emits `running` on clarification-answer resolution.** In `agent/src/runner.ts` `askUser`, on an `answer` verdict (not cancel, not timeout), emit `reportState({ status: "running" })` mirroring the `onSessionId` fire-and-forget shape. Autopilot short-circuit and cancel/timeout paths unchanged. *(Landed: the `settle` closure now emits `reportState({ status: "running" })` guarded by `v.kind === "answer"`, with the fire-and-forget `.catch` + `runLog.warn` shape from `onSessionId`.)*
- [x] **M2 — Regression tests.** Agent-side (`node --test`): assert `askUser` emits a `running` report after an `answer` verdict, and does NOT emit it on `cancel`/timeout/autopilot. Assert the report carries `status: "running"`. The seam already exists: `agent/test/runner-ask-user.test.ts` records every state report via `api.states`/`statuses(runId)` and injects inputs on a park via `api.onState`, driving parks with `StubExecutor{planGate}` / `askingExecutor`. An assertion "a `running` report follows an `answer` verdict" fails against pre-fix code and passes after — a real discriminating control. **Honest scope:** this runner-layer test exercises the fix's *mechanism* through a stub (ask-then-gate shape); it does not reproduce the production `drivePlanningTurn` root-cause path (latched `onSessionId` + absent `reportIteration`), which lives in the SDK executor. That is acceptable for a unit test of a one-line worker change; run the full `task gate:agent` to catch any exact-sequence assertion elsewhere. *(Landed: three tests added — answer-emits-`running` (verified red against pre-fix code, index-of-park discriminated so the claim-time `running` at runner.ts:423 does not mask it), autopilot never-parks, and timeout emits-no-settle-`running`; cancel documented in-test as guarded by the same `v.kind === "answer"` check rather than given heavy scaffolding. `task gate:agent` green.)*
- [ ] **M3 — Verify the read-side surfaces, fix the now-stale rationale docs, record the decision.** Confirm that once the status flips: CLI `uzi run wait` observes the `awaiting_input -> running` transition; the web run page no longer shows the "question could not be read / Cancel run" banner after an answer; a late Slack reply is routed as `follow_up` (status `running`), not a discarded `answer`. **Doc correction (repo fix-the-doc rule):** post-fix, a `running` report now usually intervenes on the plan/pre-run path, so the direct `SetRunAwaitingApproval` `open_question_id` clear becomes a *fallback* for the report-dropped/late case rather than "the normal pre-run path". Update the two comments that now read imprecisely: the `SetRunAwaitingApproval` guard comment in `api/internal/store/queries/runtime.sql` (~`:934-946`) and `api/internal/store/awaiting_input_integration_test.go:315-324`. Add a Decision-Log note (this file) and a short pointer near `askUser`'s new report explaining why the plan-phase report exists.
  - Landed so far: both doc comments corrected to frame the direct clear as a report-dropped/late/never-sent **fallback** (the SQL and the live-DB test comment); a `// PRD #307:` pointer added at `askUser`'s new report explaining why the plan-phase report exists. **Still open:** the live read-side verification (CLI `uzi run wait`, web banner, Slack `follow_up` routing) — those need a running stack and were not exercised in this worker-only run.

## Success criteria

1. After a plan-phase answer is consumed, `runs.status` becomes `running` and `runs.open_question_id` is `NULL`, with `updated_at` advanced.
2. Implement-phase clarifications continue to flip correctly (no regression).
3. `uzi run wait` on a gated run blocks through the clarification park and returns on the real transition, not on a stale `awaiting_input`.
4. The web run page shows the answered/resuming state, not the false "Cancel run" banner, after the answer is consumed.
5. A Slack reply that lands after the park is resolved is handled as a follow-up on a `running` run, not accepted-then-discarded as an `answer`.
6. Full gate green: `task gate:agent` (worker change + tests) and any touched Go gate.

## Risks and mitigations

- **Touching the `open_question_id` invariant.** Mitigated by design: the fix routes through the existing `SetRunRunning` clear and adds no new transition site. Reviewer must confirm no new server-side status setter was introduced.
- **Idempotent double-report on the implement path.** `running -> running` is idempotent by construction in `SetRunRunning`; the extra report is harmless. Covered by M2's implement-phase assertion.
- **Fire-and-forget timing window.** The `running` report may land shortly after the next turn starts streaming; the status is still corrected. If a reviewer prefers the status correct before resume, awaiting the report in `askUser` is acceptable but must not block on retry storms — keep the bounded-retry `catch`.

## Testing strategy

- Agent suite (`node --test` via `task test:agent`) for the worker behavior (M2), using a fake `reportState` to assert the emitted status. The plan-phase assertion is the primary regression guard and must fail against pre-fix code.
- No new live-DB test is required: the `awaiting_input -> running` DB transition and its `open_question_id` clear are already covered by the store suite; this PRD changes what the worker *reports*, not what `SetRunRunning` does.

## Decision Log

- **D1 — Worker-side report over server-side flip.** Chosen because it reuses the one existing `open_question_id`-clearing transition (`SetRunRunning`) instead of creating a second one, sidestepping the hazard `runtime.sql` explicitly documents. Trade: the window between the worker consuming the answer and sending the report stays uncovered, but it is negligibly small (the two happen back to back in the same worker tick); accepted.
- **D2 — Put the report in `askUser`'s `settle` choke point.** One place covers both park phases uniformly; the implement-phase redundancy is a harmless idempotent report. Rejected alternative: a plan-loop-only report after `askUserOrContinue`, which would leave the two phases asymmetric for no benefit.
- **D3 — Server-side consume-time flip deferred, not adopted.** A server-side flip would close the consume-to-report window, but that window is negligibly small, and closing it would cost a new server-side status-transition site that must carry its own `open_question_id` clear (the exact hazard D1 avoids). Not worth the trade unless the window is ever observed to matter. (Note: `/inputs` is consume-on-read, so an answer consumed just before a worker death is not re-delivered and the resumed worker re-parks and would expire at `QUESTION_TIMEOUT` — a pre-existing property of the clarification feature, not introduced or worsened by this fix.)
