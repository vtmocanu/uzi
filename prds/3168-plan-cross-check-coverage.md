# PRD #3168: Plan cross-check coverage account

**Status**: Draft, redesigned 2026-10-23 as advisory (see Decision Log). Child 3 of 3 under umbrella #3165 (Deeper, observable cross-check). PRD #3166 is done (2026-10-21). M1 has no blocker and lands before PRD #3167 M2 and M3a (same file; see Coordination). M1 is delivered through a child issue whose PR closes only the child; this issue stays open for M2. M2 is blocked by PRD #3167 M1 (replay harness).

Resolved facts below were re-read at `main` `72c97fc7`.

## Problem

A Plan cross-check APPROVE gates implementation of an auto-approved plan, but nothing says which of the issue's requirements the plan covers:

- The plan brief (`crossCheckBrief`, `agent/src/codex/cross-check.ts`) asks the checker to verify cited anchors and flag weak tests, scope, docs and overclaims. It never asks whether each requirement or acceptance criterion has a place in the plan.
- What an approving checker says is discarded. The runner adopts `checkedApproval` on `decision.kind === "approve"` (`agent/src/runner.ts`) and builds the `approval: "cross_check"` `PlanVerdict` (`agent/src/steering.ts`) from the candidate alone. Findings reach the lead only on REVISE, through `buildAutomaticRevisionPrompt` (`agent/src/prompt.ts`).
- #1231 (blocking completion auditor) was the planned home for acceptance-criteria auditing. It closed not planned: its token caps cannot be enforced on subscription credentials. Its auditing value moves into this PRD and PRD #3167, as advice only.

## Outcome

Each plan check enumerates the issue's requirements and acceptance criteria and reports each one the plan omits or covers only partly. Coverage is advisory and never blocking:

| What the checker found | Item | Effect on the verdict |
|---|---|---|
| A defect (wrong anchor, test that cannot fail, overclaim, scope, docs) | as today; `error` stays for defects | as today |
| A stated safety, security or data-integrity requirement the plan omits | a defect: `error`, REVISE as today | as today |
| A requirement the plan omits or covers only partly | `warning`, naming the gap | none by itself |
| A requirement only verifiable on hosted, live or maintainer-only surfaces | `info` | none |
| A requirement source it could not read (missing, external, unreadable, oversize) | uncertainty note in `summary`; never "covered" | none |

Every item of an approving verdict reaches the lead as fenced, non-gating advice in its implementation prompt. A REVISE or BLOCK for any other reason behaves exactly as today.

Acceptance examples:

1. The issue lists four acceptance criteria; the plan covers three. The checker returns APPROVE with one `warning` naming the missing criterion. The run implements with no further round and no human gate, and both executors' implementation prompts carry the warning, fenced as untrusted advice.
2. A plan's regression test cannot fail on the unfixed code. The checker returns REVISE with that item, as today.
3. The issue links a repository PRD over the read limit and an external page. The summary says neither was inspected; nothing from them is reported as covered; the verdict is unaffected.
4. Round 2: the plan's Cross-check dispositions omit the previous round's coverage warning but cover every defect item. That omission alone does not call for REVISE.

## Out of scope

- Any REVISE or BLOCK on coverage; a new verdict field, item category, gate, loop, capability, or change to revision limits.
- URL fetching or new checker tools; issue comments (not supplied to the checker today).
- #3097's prior-findings transport and its disposition rules, beyond the coverage exception below.
- Large-file reads (#3073) and search limits.
- Code-stage coverage (PRD #3167).
- Persisting approval notes across a reclaim.

## Modules and seams

- **Plan brief** (`crossCheckBrief`, shared by both families). Adds:
  - Enumerate requirements and acceptance criteria from the issue snapshot (`issue_title`, `issue_body` fences in `crossCheckPrompt`; comments are not supplied) and from repository PRDs the issue links, read with the existing Read and Search tools within the current deadline and limits.
  - Report each omitted or partly covered requirement as a `warning` naming the gap; one verifiable only on hosted, live or maintainer-only surfaces as `info`. `error` stays for defects.
  - Marker. Every advisory coverage item uses a fixed summary prefix, including informational coverage notes: `Requirement gap:` for an omitted or partly covered requirement, `Coverage uncertainty:` for a requirement that cannot be verified here. Defect items do not use either prefix. Severity alone cannot tell a `warning`-level defect from coverage, so the checker and the revision prompt use this convention for defect-first ordering and the coverage-disposition exemption. It adds no field and does not authorize rewriting a verdict programmatically. An uncertainty note in `summary` uses the `Coverage uncertainty:` prefix too.
  - A missing, external, unreadable or oversize source gets an uncertainty note in `summary` and is never treated as covered. "No defect found" is distinct from "requirement covered".
  - Coverage items alone never justify REVISE or BLOCK. An omitted requirement the issue states as a safety, security or data-integrity constraint is a defect, not coverage.
  - Within the 20-item cap defect items come first. Coverage items fill only the space defects leave; those that do not fit are counted in `summary` ("N coverage notes omitted"). A coverage item or the omitted count never displaces a defect, so a full defect report can leave no room for coverage items.
  - Round 2 and later: today's brief requires a disposition for each of the preceding check's delivered items and calls a missing one REVISE. A missing or declined disposition for a prior coverage item never by itself justifies REVISE; defect items keep today's rules.
  - Instructions only. Nothing claims deterministic verification of the checker's judgment.
- **Revision prompt** (`buildAutomaticRevisionPrompt("plan", …)`). Carries the same coverage exception to the lead: disposition coverage notes where useful; omitting one is not itself a reason for another round. Defect wording unchanged.
- **Approve delivery (worker only).** The `approval: "cross_check"` `PlanVerdict` variant gains the approving verdict's findings (`checkedApproval.findings`, already decoded by `decodePlanCrossCheckResponse`, `agent/src/client.ts`); the runner sets it where it builds that verdict. Each executor renders it beside `checkedImplementationContext` (`agent/src/checked-plan.ts`) in its implementation prompt: `checkedContext` in `agent/src/sdk-executor.ts`, `implementPrompt` in `agent/src/codex/codex-executor.ts`. Approval advice includes the bounded summary and items. A nonempty summary is delivered even when items is empty. Only absent findings, or an empty summary with no items, leave the implementation prompt unchanged (byte-identical to today).
  - This deliberately widens delivery to every item of an approving verdict, defect `info` and `warning` items included, not only coverage.
  - No item category field and no wire, API, schema or capability change: `validateVerdict` (`agent/src/codex/cross-check.ts`), the client findings decoder and `NormalizeCrossCheckFindings` (`api/internal/workersvc/cross_check_findings.go`) stay unchanged.
  - Best-effort: held in worker memory, not re-delivered after a reclaim. When the dormant semantic human gate drops `checkedApproval`, nothing new reaches the lead; this is a documented limitation.
- **Trust and scope.** Notes are nonce-fenced untrusted advice with `buildAutomaticRevisionPrompt`'s fencing (nonce retry, entity-escape fallback) and wording: verify against the code and issue, decline conflicting suggestions, keep existing permissions. One fence implementation shared by both prompts, not a copy. The lead may implement an omitted requirement only within the issue's stated scope and must name it in the PR description; code cross-check reviews it. `buildAutomaticRevisionPrompt("code", ...)` output stays byte-identical.
- **Docs.** `docs/cross-check.md` ("implements exactly the approved checked plan") and the matching `ARCHITECTURE.md` sentence state that an approval may carry advisory notes, that coverage never revises or blocks, and the in-scope implementation rule; then `task docs:sync`. A dated amendment to `adr/2149-cross-check.md`.
- **Evaluation (M2).** Reuses PRD #3167's replay harness with a plan corpus (including #2888 at its pre-fix snapshot), the pre-M1 brief pinned by commit, N repeats per family: approve/revise/block rates, coverage warnings judged true or false, cost, latency and timeout rate.

## Coordination

- **#3097** changes the same brief's disposition contract. Whichever lands second rebases onto the other. This PRD does not change #3097's disposition rules beyond the coverage exception; #3097's "a supported decline cannot waive a mandatory requirement" keeps applying to defect items.
- **PRD #3167** also edits `agent/src/codex/cross-check.ts` (`codeCrossCheckBrief`, the code branch of `crossCheckPrompt`); this PRD edits `crossCheckBrief` only. Intended order: this M1 first, then PRD #3167 M2 and M3a rebase onto it. PRD #3167 M1's baseline is unaffected: the code brief is untouched here.

## Testing decisions

- Instruction-contract tests in `agent/test/plan-cross-check-guidance.test.ts`: new `rules` regexes for requirement enumeration from issue and linked PRDs, the `warning` / `info` / `error` split, uncertainty never covered, coverage never REVISE or BLOCK, defect-first ordering with the omitted count, and the coverage-disposition exception, checked for both family tool strings. New cases in `fixtures/plan-cross-check/cases.json`, with the exact-case-set test extended. They pin instructions, not model behaviour.
- Instruction-level regression: brief and revision prompt say a missing coverage disposition alone does not call for REVISE, while the existing `omission` rule for defect dispositions still matches.
- Approve delivery, through the runner and both executors: an approve verdict with only coverage items stays approve, consumes no automatic round, forces no human approval, and its items reach both executors' implementation prompts inside the nonce fence with the advisory wording. A summary-only approval (a `Coverage uncertainty:` note, an omitted-coverage count) reaches both executors fenced identically to items; absent findings, and an empty summary with no items, leave both prompts byte-identical. An item carrying its own closing tag stays contained.
- Instruction contract for the marker: both prefixes are required for coverage items, forbidden for defects, and drive ordering and the disposition exemption; the omitted count never displaces a defect.
- Regression: `buildAutomaticRevisionPrompt("code", ...)` output is byte-identical to today for string advice and for code findings (fixed nonce source), so the shared fence refactor cannot drift the code repair prompt.
- Instruction contract for the safety carve-out: an omitted stated safety, security or data-integrity requirement is a defect (`error`, no coverage prefix) and may call for REVISE.
- Regression: REVISE and BLOCK for non-coverage reasons are unchanged; existing exact-candidate approval, revision accounting and BLOCK human-fallback tests (`agent/test/runner-plan-cross-check.test.ts`, `agent/test/plan-cross-check-rounds-gate.test.ts`) stay green.
- Replay comparison (M2), maintainer-run.

## Milestones

- [ ] **M1: Advisory coverage in plan checks, delivered on approve.** Brief, revision prompt, approve delivery, tests, docs and `task docs:sync`, dated ADR-2149 amendment, changelog fragment, and the `specs/human.md` lines on plan cross-check (dispositions and fenced advice, ~211-213) updated with an `(AI-synced YYYY-MM-DD)` entry: coverage notes are advisory, a missing coverage disposition alone is not REVISE, an approval's findings reach the lead as fenced advice, and the safety carve-out. Delivered through a child issue whose PR closes only that child. Blocked by: none (rebase onto #3097 if it lands first). Acceptance: examples 1-4 at instruction and delivery level; the tests above; gates `task gate:agent`, `task gate:api` (docs mirror, `TestEmbeddedDocsMatchSource`), `task check-docs:web`, `task gate:repo`.
- [ ] **M2: Measure coverage advice.** Maintainer replay of the pre-M1 and M1 briefs on the plan corpus, per family. Blocked by: M1, PRD #3167 M1. Extends the PRD #3167 harness to build plan-stage claims (candidate, planning diff, round) if its M1 does not. Acceptance: aggregate posted on this issue, coverage warnings judged true or false, unmeasured items named; this issue closes with M2.

## Risks

- Noise: every approval may now carry notes. Mitigated by the advisory wording, the 20-item cap with defects first, and the scope rule.
- Widened delivery: approve-verdict defect notes now reach the lead, which may over-act on them. The scope rule and the code cross-check bound it.
- The checker may omit the marker, mark a defect, or call for REVISE on coverage anyway. The marker is an instruction convention, never parsed to rewrite a verdict; fixtures pin instructions only, and M2 reports misclassifications.
- Rollout, worker-only: an old lead worker discards approve items as today; an old checker sends no coverage items and the new lead renders whatever items arrive. Both orders are safe; no API change.

## Decision Log

- **Advisory, never blocking** (user decision 2026-10-23, supersedes 2026-10-20 BLOCK/REVISE rules). Coverage items never justify REVISE or BLOCK, unreadable sources are disclosed, and a missing coverage disposition is not itself a reason for REVISE. #1231's auditing value moves here and into PRD #3167 as advice, because token caps cannot be enforced on subscription credentials.
- **Safety carve-out** (user decision 2026-10-23). An omitted requirement the issue states as a safety, security or data-integrity constraint is a defect (`error`, REVISE as today), not coverage: advisory handling must not let such an omission through an automatic approval.
- **M1 through a child issue** (user decision 2026-10-23). The child's PR closes only the child; this issue stays open for M2.
- **Superseded 2026-10-20 rules.** REVISE for an unmet required item and BLOCK (with cause categories and a "could not verify" presentation on the run page, CLI and Slack) for unassessable required evidence. Dropped with that presentation work.
- **Deliver every approve item and the summary.** The smallest change: the findings are already decoded on the worker. Delivering coverage items only would need a category field. A summary-only approval still carries uncertainty notes and omitted counts, so it is delivered.
- **Fixed coverage prefixes, not a field** (lead and buddy, 2026-10-23). Severity alone cannot separate `warning` defects from coverage; `Requirement gap:` and `Coverage uncertainty:` make ordering and the disposition exemption checkable without a wire change. No code parses them to change a verdict.
- **No eviction** (2026-10-23). Coverage never displaces a defect within the cap.
- **#3097 declines** (2026-10-23). #3097's "a supported decline cannot waive a mandatory requirement" applies to defect items; a declined coverage gap stays advisory.
- **Semantic human gate** (2026-10-23). Notes not reaching the lead when the dormant semantic human gate drops the checker approval is an accepted, documented best-effort limitation.
- **No new fields.** Severity and `summary` carry the account; consistent with #3091.
- **Coordinate with #3097 instead of waiting.** Both change the same brief; whichever lands second rebases.
- **Replay as its own milestone.** M1 does not wait on PRD #3167's harness.
