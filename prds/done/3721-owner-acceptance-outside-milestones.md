# PRD #3721: Plan owner-only acceptance outside the milestones, so runs stop asking at completion

**Status**: Done (2026-10-31). Delivered: shared lead guidance on both harnesses, non-closing delivery tests, docs, issue-author guidance, watcher/lander skills, spec and ADR-1225 addendum. Landing obligation (not a milestone): full agent suite: exact-head PR CI, verified by the lander before merge.
**Priority**: Normal
**Revisits**:
- #3090's planning guidance, which puts human or live acceptance in "a separate, final informative milestone". That milestone is what makes runs park at completion (below). #3090's other rules stay: ordinary answers never supply a deferral (#3089), worker obligations are never reclassified as acceptance, and unperformed checks are never described as done.
- #3473, which parked product changes to the completion interlock or the lead prompt unless the docs-only fix proved insufficient. The evidence below is that proof.
- Not revisited: #1230's dormant `maintainer` criterion kind and its ledger (`kind=maintainer`, `maintainer_authorizations`, `completion_pending`). This PRD adds no structural classification beside it (#3090: "Do not implement a competing structural classification"). #1231, its activation, closed because whole-call token caps cannot be enforced with subscription credentials; that is unchanged.

## Problem

An issue run parks at completion and waits for the owner whenever its plan contains work the worker cannot do. Following the shared guidance, the lead puts that work in a final "informative" milestone. The structural completion contract freezes one criterion per milestone, so at completion that milestone is unmet, and the run waits for `uzi run decide --partial`. In most cases the owner already saw the same item in the plan.

Evidence: of 152 issue runs on this repository created 2026-10-21 to 10-31, **45 parked for that decision** (3 of them auto-approved). By the deferred milestone's stated reason:
- about 18 deferred mainly **exact-head PR CI** (for example #3707, #3656, #3657, #3617, #3526, #3530, #3527, #3531). The lander checks it before every merge anyway; as a milestone it only adds a question.
- about 17 deferred **live or hosted acceptance** (for example #3638, #3654, #3675, #1563, #3304, #3563), which a worker cannot perform.
- about 10 deferred **genuinely unfinished worker work or an owner scope cut** (for example a failed local gate, #3462). Those must keep asking; this PRD does not change them.

## Outcome

Owner-only items are named in the plan, approved (or rejected) with it, and never become milestones. The run delivers without a completion question and never closes the issue while such items remain.

1. **Landing checks are not milestones.** Only checks inherently requiring the PR/merge environment (for example exact-head PR CI), or an already-authorized issue-specific landing fallback, go in a "Landing obligations" plan section. Required in-run checks remain worker obligations even if the lander repeats them or the worker lacks time, permissions or tooling. This convention applies to new, unfrozen plans only; existing frozen criteria still require the formal owner decision or reconciliation paths.
2. **Owner acceptance is a plan section, not a milestone.** Live, hosted or human-judgment acceptance goes in an "Owner acceptance" plan section, with what remains, why the worker cannot do it, who does it, and the steps. On a gated run the human sees it at the plan gate; on an auto-approved run the plan cross-check sees it when the owner has enabled it, and otherwise nobody sees it before the PR, where the lander's pre-merge check is the only guard.
3. **Delivery is honest and non-closing.** When the plan has owner-acceptance items, the lead declares the delivery `non_closing` (honored since #2172; #3090's commit-message protection engages on it) and lists those items in the PR as pending and not verified.
4. **Doubt is raised at planning.** The lead resolves classification from the issue, the repository and prior owner instructions. If consequential uncertainty remains and `ask_user` reaches a human (no autopilot note; a plan force-gated on autopilot still auto-answers `ask_user`), it uses the existing `ask_user` mechanism before the final `submit_plan`. On an auto-approved run `ask_user` reaches no human, so the lead keeps each consequentially doubtful item as a required milestone and records the uncertainty in the plan; auto-approval, a checker approval and the auto-answer supply no waiver, and if such a milestone remains unmet the existing formal owner completion decision applies (D5). It never silently omits ambiguous work from the milestones or relies on a question embedded in plan prose. Worker obligations (code, docs, tests, mandatory gates, security, review) are never moved to either section.

Acceptance examples:
1. A gated run plans M1 (code) plus an "Owner acceptance" section (hosted rollout observation). The owner approves. The run completes M1, declares non-closing, and opens a PR that lists the hosted observation as pending. No completion question; the issue stays open.
2. The plan lists "full agent suite: exact-head PR CI" under "Landing obligations". No milestone contains it; the run completes without a question, and the PR carries `Closes #N` (only owner-acceptance items make a delivery non-closing). The lander verifies CI before merge.
3. A run whose local gate failed still parks at completion and asks the owner, exactly as today.

## Out of scope

- Any server-side change to the completion contract, permit or freeze (see D2).
- The dormant semantic profile and its maintainer kind.
- Changing the owner `partial` / `accept` decisions or #3090's non-closing safeguards.

## Modules and seams

The implementing lead verifies every anchor against current `main`.

1. **Shared lead guidance**, both harnesses: `COMPLETION_DELIVERY_GUIDANCE` in `agent/src/prompt.ts` and the matching rule in `api/internal/agenttmpl/builtins/lead.md` (lines ~47-52). Replace both the informative-milestone sentence ("Plan human/live acceptance as a separate, final informative milestone ...") and the sentence "Pending human/live acceptance requires a formal owner partial decision ..." with the four rules in the Outcome; the partial decision applies only to a frozen milestone that remains unmet. Keep the rest of #3090's text (no reclassification of worker obligations; ordinary answers and plan approval never supply a deferral of a frozen milestone; never describe unperformed checks as done). Update the user doc `docs/run-completion-hold.md` ("Planning work and pending acceptance") the same way, then run `task docs:sync` and commit the `api/internal/uzidocs/embed/` mirror. Update the tests that pin the old text (`agent/test/prompt.test.ts`, `agents.test.ts`, `codex-render.test.ts`).
2. **Non-closing at completion (confirm and test; no code change expected).** The lead lists owner-acceptance items as `pr_summary.scope_notes` of kind `deferred` (both harnesses; rendered as "Deferred:") and declares `non_closing` on `signal_done` (both harnesses; `nonClosingDelivery` in `agent/src/runner.ts` honors the declaration with no unmet milestone, and #3090's commit guard engages on it). No new field or tracker. Scope notes only summarize the acceptance (at most eight notes of 200 sanitized bytes each, and the 6 KiB renderer fallback can omit all prose); the submitted plan keeps the complete what/why/actor/steps. Before merge, the lander compares that plan with the rendered PR and restores omitted or truncated acceptance in a supported owner-maintained area or a durable linked handoff. Missing prose never establishes that acceptance is absent. Test both harnesses, plus long and multiple notes and the renderer fallback. No cap increase.
3. **Issue-author guidance**: `AGENTS.md` (the Run economy rules around lines 193 and 227) and `.agents/skills/issue-triage/references/on-deck.md` (line ~35) tell authors to write "full agent suite: exact-head PR CI" as acceptance. Say that it is a landing obligation the plan lists outside its milestones. The repo rule that the label "does not waive explicit issue requirements" stays.
4. **Watcher and lander skills** (`.agents/skills/uzi-watcher`, `.agents/skills/uzi-lander`): a plan-trap check that a milestone consisting only of exact-head PR CI or owner-only acceptance is a revise; the lander checks the PR's owner-acceptance list before merge and leaves the issue open. After a non-closing delivery merges, the lander applies issue-triage's "Acceptance only" disposition (remove `uzi`, `bug`, `Planned`, `on-deck`; add `acceptance`) only after verifying that implementation is complete and only maintainer acceptance remains, so nightly sweeps cannot pick the issue up again (the scheduler has no "already delivered" skip). Unfinished worker work, mixed scope and ordinary owner scope cuts keep their actual remainder and get the appropriate existing triage disposition. Non-closing alone does not establish acceptance-only status.

## Testing decisions

- Agent: guidance tests on both harnesses (the new section rules present; the informative-milestone rule gone).
- Agent: a run with no unmet milestones that declares `non_closing` delivers a non-closing PR, on Claude and Codex, and the commit-message protection holds.
- Red/green against current `main` for each behaviour change.
- Gates: focused checks plus `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent`; `task test:codex-m4` if Codex runtime is touched; full agent suite: exact-head PR CI.

## Milestones

### M1: Landing checks and owner acceptance leave the milestone list
Blocked by: none.
Guidance (seam 1), the non-closing confirmation and any small PR-summary change (seam 2), issue-author docs (seam 3), skills (seam 4), changelog fragment. All acceptance examples.

## Decision Log

- **D1. A plan section, not a milestone.** The structural contract only enforces milestones, so an item that inherently cannot run in a worker (a PR/merge-environment check or genuine owner acceptance) should not be one. Required in-run checks stay milestones or worker obligations. The plan gate is where the owner (or the checker) already judges the split. Rejected: keeping the informative milestone and asking at completion (45 parks in ten days).
- **D2. No server change now.** Simplest design that removes the questions: guidance plus the existing `non_closing` path. Rejected for now: recording owner-acceptance IDs on the bound `approve_plan` row and pre-deferring them at freeze. That needs the approve queries, `SetRunRunning`'s fallback freeze, milestone reconciliation, every approval surface (web, CLI, TUI, Slack) rendering the marker, and permit-scope changes at revision 1; and it would sit beside #1230's dormant ledger. Revisit if leads fail to declare non-closing or hide worker work in the section.
- **D3. Trade-off accepted.** The owner-acceptance list is lead prose and non-closing depends on the lead declaring it. The plan gate, the plan cross-check and the lander's pre-merge check are the guards.
- **D4. Unfinished worker work keeps asking.** A failed gate or incomplete code still parks at completion; only items that were owner-only at planning time leave the milestone list.
- **D5. No `ask_user` on auto-approved runs (owner decision 2026-10-31).** An auto-approved run's `ask_user` reaches no human: `AUTOPILOT_PLAN_NOTE` (`agent/src/prompt.ts`) tells the lead not to call it, and the runner answers every call with `AUTOPILOT_SENTINEL_ANSWER` (`agent/src/runner.ts`). So rule 4's question applies only where `ask_user` reaches a human (the plan prompt carries no autopilot note; a plan force-gated on autopilot, e.g. a ci_fix CI-config plan, still auto-answers it); on autopilot a consequentially doubtful item stays a required milestone, the plan records the uncertainty, and an unmet one reaches the owner through the existing completion decision. Rejected: a runner change making autopilot `ask_user` reach a human (outside D2), and keeping the literal "including on auto-approved runs" wording (contradicts `AUTOPILOT_PLAN_NOTE`).
