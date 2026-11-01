# PRD #3708: Plan cross-check before the human plan gate

**Status**: Draft
**Umbrella**: #3165 (deeper, observable cross-check).
**Priority**: Normal
**Revisits**: PRD #2149's out-of-scope line "Human-gated runs" (deferred there; no Decision Log entry rejected it). It keeps unchanged ADR-2149's and PRD #2149 D16's contract that a human revision triggers no fresh checker pass (ADR-2149, "Consistent with D16, a human revision of the plan triggers no new cross-family checker pass").

## Problem

A run whose plan stops at the human plan gate from the start (no auto-approve, or auto-approval blocked) gets no plan cross-check: Plan cross-check (PRD #2149, ADR-2149) enrolls only auto-approved runs. An auto-approved run whose check fails or asks for changes already reaches the human gate with the checker's findings; a run that was gated from the start never gets them. The owner reviewing that plan reads it alone, with no second opinion from the other model family.

Evidence from 2026-10-29 to 10-31 (recorded on #3165):
- On two plans gated from the start (#3244, #3638) the owner and a second reviewer found substantive gaps by hand: a dropped landing-order integration, a classification path that was not read-only, and incomplete carry/retire inventories. No checker saw those plans.
- On auto-approved plans in the same period the checker raised valid items: on #3628 a red/green baseline the issue required, which the owner's and the second reviewer's independent reviews had both missed; on #3686 a predicate that deviated from the issue's definition, which the lead then revised automatically before a second round approved.

## Outcome

When the owner has **Plan cross-check** enabled, a run gated from the start goes through the same checked-plan loop as an auto-approved run before its first human gate. The opposite model family checks the plan (a Claude lead's on Codex, a Codex lead's on Claude). Changes requested go back to the lead automatically; the lead verifies each finding against the code, revises or declines it with a reason (#3168 dispositions), and resubmits, within the existing automatic revision budget. **The difference from an auto-approved run is the end: whatever the final verdict, the run stops at the human gate.** No verdict, including APPROVE, can start implementation. The gate shows the final verdict, every round's findings, and the round count; the lead's responses appear as today, in the plan's own "Cross-check dispositions" section (untrusted lead claims, not verified resolution). The human approves, revises or rejects exactly as today.

The loop is bounded but the human waits for it: normally up to `PLAN_CROSS_CHECK_TIMEOUT` (30 minutes by default, including checker queue time; shorter on installations with a short `RUN_TIMEOUT`; configurable up to 2 hours) per checked candidate, for at most `PLAN_CROSS_CHECK_MAX_REVISIONS + 1` candidates (default 5). The lead keeps its worker slot while it waits, and the pending time is excluded from its wall budget as for required checks.

Acceptance examples:
1. Plan cross-check on, Claude and Codex both usable. A run is created without auto-approve on the Claude harness. Its lead submits a plan; the Codex checker returns REVISE with two items. The lead revises one and declines the other with a reason; the second round returns APPROVE. The run reaches `awaiting_approval` and does not implement. The run page and `uzi run get` show "Plan cross-check: passed after 2 rounds" with round 1's two findings; the presented plan carries the lead's dispositions section. The owner approves and the run implements the presented plan.
2. Same setup, but the checker cannot start, fails, misses its deadline, returns BLOCK, or the automatic budget runs out. The run still reaches `awaiting_approval` with the latest candidate, and the gate shows the outcome ("unavailable", "timed out", "blocked", "revisions exhausted") with any findings. Approve, revise and reject work as today.
3. The owner revises at the gate. The revised plan follows the existing human-revision path without a fresh checker pass (ADR-2149 / D16). Earlier checker results remain historical. This PRD does not activate or enroll the semantic coverage profile. A gated Codex-lead run behaves like example 1 with a Claude checker when its worker advertises the needed capabilities; otherwise its gate opens at once showing "unavailable, not supported on this worker".
4. Plan cross-check off: no `cross_check` run is created and every path is today's. Auto-approved runs and their required-check semantics (including fallback gates and D16) are unchanged.

## Out of scope

- Implementing without the human approve on a gated run, whatever the verdict.
- A fresh checker pass after a human revision (unchanged ADR-2149 / D16 contract).
- A separate consent toggle or a separate gated-run budget (reuses the Plan cross-check setting and `PLAN_CROSS_CHECK_MAX_REVISIONS`).
- Seeded plans (`plan_source='seeded'`), gateless kinds (`task`, `chat`, `judge`, `job`), and isolated-lane runs, as in PRD #2149.
- The code stage.
- Findings in Slack: the gate card shows the result line only (PRD #2149 D10).
- Opening the gate early and attaching the check later (D2).

## Modules and seams

Reuse the existing machinery (PRD #2149, #2150 rounds and Codex-lead direction, #3168 coverage advice and dispositions, ADR-2149). The implementing lead verifies every anchor against current `main`.

1. **Enrollment snapshot (api).** Keep today's required predicate inside the run INSERT (`api/internal/store/queries/runtime.sql`, PRD #2149 D8: consent + auto-approve + non-seeded) **unchanged**. Add a separately snapshotted mode for runs gated from the start: consent on, gated from the start, non-seeded, eligible kind and lane. Enrollment records the owner's frozen choice, not runtime availability: a temporarily missing credential, slot or capability makes the check **unavailable** at gate time; it never silently turns an enrolled run into "neither". A run is required, checked-then-gated, or neither, never two. Existing rows keep their current meaning (no backfill into the new mode).
2. **Version matrix.** A new worker capability gates the new mode, advertised only once both directions are delivered (D7). Pin all four combinations: new api + new worker runs the loop; new api + old worker presents an ordinary gate and the **server** records the check as unavailable from the claim's known capabilities (an old worker cannot report a new outcome); old api + new worker never submits a check and presents an ordinary gate; old api + old worker is today's behavior.
3. **A genuinely gated check path (agent).** `RunRunner.gatePlan` (`agent/src/runner.ts`) and `checkPlan` (`agent/src/plan-cross-check-gate.ts`) today return approval authority on APPROVE (`checkedApproval`) and forced-gate fields otherwise, and own the candidate transport reservation and the preparation / reconciliation receipts. The new mode needs its own result shape: every outcome, APPROVE included, ends in the ordinary human gate on the latest canonical candidate, with no `checkedApproval` grant and no forced-gate reason. Automatic REVISE rounds work as in #2150 and do not spend the human revision allowance. Preserve cancellation, stale-claim fencing, exact-plan / sequence / ACK barriers and fail-closed transport ownership: a checker failure is not permission to swallow an unrelated ownership or preparation failure. Both executors (Claude `sdk-executor`, Codex `codex-executor`) share one path.
4. **Identity and recovery.** Each verdict binds to the exact canonical candidate and the gate presentation it precedes. A late or old-claim result never attaches to a newer candidate, changes a decided gate, or mints a gate from status evidence. Reclaim or retry of the same candidate creates no second child and does not reset its deadline or the loop's budget. A candidate already checked is never checked twice.
5. **Server backstop.** In the new mode a checker APPROVE is candidate data, never an implementation grant; check eligibility is independent of auto-implementation permission. Enforce it where approval is decided today: `SubmitPlanCrossCheck`, `SetRunAutopilotPlan`, `SetRunRunning` and the human input / presentation binding (the lead verifies exact names on `main`). Never set `auto_approve` to reuse the check path. Implementation requires a current human approval bound to the exact presented candidate: the existing ordinary (non-required) `SetRunRunning` branch accepts any prior applied approval, and the new mode must not inherit that shortcut. Ordinary planning and heartbeat `running` reports stay valid. Tests reject a direct autopilot plan write, a post-gate implementation handoff without the current matching human approve, a stale earlier approve after re-gating, and a late checker result. Tests also cover a human revise attempted before the first gate opens (handled exactly as a revise outside a gate is today) and a revision after the gate (no further check).
6. **Presentation.** Run page (web), `uzi run get` (`PLAN_CHECK_*` rows with mode and rounds) and `uzi run get --json` show the outcome and **every round's findings**: today's plan-check summary carries only the latest round, so persist or read the earlier rounds' findings per loop, round and candidate, bounded by the round cap and the existing findings caps. The smallest representation: bounded per-round findings, plus the next candidate's own "Cross-check dispositions" prose, clearly labelled as untrusted lead claims. Dispositions stay that prose section: no structured disposition store, a missing response is shown as "not reported" and never inferred as addressed, and dispositions are never an approval condition (#3168 coverage exceptions unchanged). The code-stage disposition API is a different contract and is not reused here. While the loop runs, the run page shows that a check is in progress and its deadline. Findings stay fenced as untrusted, possibly wrong advice (#3168 rules unchanged). Slack: one result line. Docs state the possible wait before the gate opens: with defaults up to 5 checks x 30 min = 2 h 30 min of checker waiting before the first gate (10 h at the 2 h maximum deadline), plus the lead's own revision turns, which are charged to the run as usual. This is an upper bound, not a promised gate time.
7. **Resource bounds.** Per run, at most `PLAN_CROSS_CHECK_MAX_REVISIONS + 1` checked candidates (only before the first human gate), each with a deadline persisted once per candidate (`PLAN_CROSS_CHECK_TIMEOUT`, including queue time) and a checker turn bounded by `CROSS_CHECK_MODEL_TIMEOUT` (15m default) and the remaining deadline. Existing cross-check lane and slots; the lead slot is held while waiting; pending time is banked and excluded from the lead's wall budget through the existing accounting. Only the owner's own credentials. No new config.

## Testing decisions

- Agent: the new mode's gate path for APPROVE (gate, never implement), REVISE with automatic rounds and dispositions, BLOCK, failure, timeout, exhausted budget, unavailable, and an old server; plus interleavings: lost submit ACK, lost gate ACK, late verdict after timeout, cancellation during the loop, reclaim mid-loop (no duplicate child, no reset deadline), a human revise attempted before the gate opens (handled as today outside a gate), and a human revise after the gate (no fresh check). Follow the existing plan-cross-check gate and rounds tests.
- Api: LiveDB tests that the INSERT snapshot is unchanged for required runs and correct for the new mode; that no checker outcome can move a new-mode run to `running`; that an old worker's claim yields a server-recorded unavailable outcome; and existing confinement regressions for the checker stay green.
- Web and CLI: rendering of mode, verdict, findings, dispositions and rounds; absent fields from an older server render nothing.
- E2E: this changes product contract strings and events, so `git grep -F` the plan-cross-check literals under `e2e/` and update stale expectations; name unrun phases in the PR.
- Every new behaviour gets a red/green regression check against current `main`.
- Gates: `task gate:api`, `task gate:web`, `task gate:repo`; for `agent/**`, focused checks plus `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` and `task test:codex-m4` when Codex runtime is touched; full agent suite: exact-head PR CI.

## Milestones

This is one PR (both milestones ship together; D7). It is wide (api, agent, web, CLI, docs); no split yields an independently valuable smaller outcome, because a Claude-only half would advertise a capability the Codex direction cannot honour.

### M1: A Claude lead's gated plan is cross-checked on Codex before the first human gate
Blocked by: none.
Enrollment snapshot, version matrix, gated check path, identity and recovery, server backstop, run page, CLI, Slack line, docs (`docs/cross-check.md`, `docs/cli.md`, `task docs:sync`), changelog fragment. Acceptance examples 1, 2, 4 and the revise part of 3. The new capability is not advertised yet.

### M2: A Codex lead's gated plan is cross-checked on Claude; the capability is advertised
Blocked by: M1.
The Codex-lead direction with a Claude checker (`cross_check_codex_lead_v1`), the unsupported-worker path, family and capability parity tests, then advertise the new capability. Acceptance example 3.

## Decision Log

- **D1. Checked, then always gated.** The check and the lead's automatic revisions improve the plan; only the human approves. User decision 2026-10-31: wait for a better plan rather than present the first draft. Rejected: one advisory check with no automatic rounds (the human would act on raw findings the lead never answered); approving automatically on a pass (that is the auto-approved mode).
- **D2. The first gate waits for the loop, within the bounds above.** User choice 2026-10-31. Reuses the existing flow and ordering. Rejected: opening the gate at once and attaching the check later (a new update path, and an approve could race the findings).
- **D3. Reuse the Plan cross-check consent and the existing automatic budget.** User choice 2026-10-31. Rejected: a separate toggle or a gated-only budget; revisit with measured waits if gates feel slow.
- **D4. No fresh check after a human revision.** User decision 2026-10-31. Keeps ADR-2149 / D16 unchanged and the budget simple. Rejected: a fresh loop per human revision (amends a deliberate contract, multiplies waits and needs new round keying).
- **D5. Opposite family always, no fallback**, as ADR-2149.
- **D6. A check failure never parks.** Unavailable, failed, timed-out, blocked or exhausted outcomes present the gate with the reason.
- **D7. Both directions ship together**, and the capability is advertised only after M2, so no worker claims a mode it cannot run for a Codex lead.
- **D8. Enrollment is the owner's frozen choice; availability is a gate-time outcome.** Rejected: computing the mode from credential availability at INSERT, which would hide a consented run's missing check.
