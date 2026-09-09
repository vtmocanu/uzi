# PRD #1230: Semantic completion contract and plan coverage review

**Issue:** [#1230](https://github.com/vtmocanu/uzi/issues/1230)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Planned, blocked on #1226 through #1229 in the one-at-a-time sequence.
**Priority:** High.
**Execution:** Keep `Planned` without `uzi`. Add `uzi` only after #1229 merges and main CI is green.

This child makes arbitrary issue/PRD requirements explicit at planning time. It does not yet make semantic verdicts a completion-permit requirement; #1231 owns that enforcement. No implementation or validation may modify `.github/workflows/**`.

## Problem and outcome

#1226 prevents a run from omitting an entire frozen milestone, but a lead can still propose a plan that omits a source requirement or declare a milestone complete while only part of its prose acceptance is delivered. PR #1220 showed the latter for M3. A completion auditor cannot enforce requirements that never entered a stable contract.

Build the semantic lane dormant. This child creates the default-off storage and read seam of the per-repo semantic setting with no write path and no runtime effect; #1231 adds the writes and the activation fence, and only behind that fence does a run compile the issue snapshot, linked PRD snapshot and proposed plan into stable semantic criteria with evidence requirements. Independently compare that contract with the complete source before approval. Human-gated runs show uncertainties to the owner; an autopilot run with missing/uncertain coverage falls back to `awaiting_approval` rather than auto-approving or failing.

The dormant schema, compiler and reviewer land in this child, but a real run exposes no semantic contract, carries no semantic criteria and makes no reviewer call until #1231 opens the activation fence; enforcement stays on #1226's structural profile until then. That limitation must be explicit in the run and PRD status.

## Included

- Bounded source snapshots/hashes for issue, linked PRD and approved plan.
- Semantic criteria with stable server-minted IDs, kinds, done-when text and evidence slots.
- Independent plan-source coverage review, one verdict-bearing review per plan submission with each invocation a bounded tool-less turn, dormant in this child: reachable only from tests that set the stored setting directly, live only behind the activation fence #1231 owns.
- Plan-gate rendering and revision for missing/uncertain requirements.
- Autopilot fallback to human approval on uncertain coverage.
- Maintainer-only criteria represented as outstanding, not worker-completed.
- Source drift/contradiction signals and web/CLI contract visibility.
- The per-repo semantic setting storage and read seam: additive column, default off, readable by the API/CLI, no write path, and no effect on a run's profile or on reviewer execution. Writes (API, CLI, web) and the activation fence, gated on `completion_audit_v1` fleet support, are #1231's (its D5/D6). Setting intent alone does nothing before that fence, so coverage review and completion audit go live together.
- A dark semantic profile flag reserved for #1231 activation.

## Excluded

- Final completion auditing or semantic permit enforcement, delivered by #1231.
- Structured review findings, MR-rework gating or provider-context transport.
- Open-web requirement research by the worker.
- Making maintainer-only hosted-k8s validation block a worker PR.
- Treating mutable PRD checkboxes as authority.

## Decisions

### D1: Preserve schema version, add a semantic profile

Keep contract schema `v=1`; distinguish enforcement with `profile=structural|semantic`. This child populates semantic criteria/source fields but leaves the active permit profile structural behind a rollout field until #1231 can enforce it. Contract revision remains the owner-decision revision from #1227.

Each criterion records stable ID, milestone ID, bounded text, `kind=code|doc|test|maintainer`, and bounded evidence requirements. IDs are server-minted after validating the candidate; the lead cannot choose identities that collide or escape scope.

### D2: Review source coverage independently

The lead proposes criteria as part of `submit_plan`. A separate read-only plan-contract reviewer receives the server-provided issue/PRD snapshots and proposed plan/contract, treats repository/forge text as untrusted data, and reports source requirements as represented, missing or uncertain. It cannot implement, edit, approve or waive.

Each reviewer invocation is one tool-less turn on the advice-lane primitive (`runReadOnlyModelPass` with a new `coverage` label, parsing a schema-validated JSON report from the text turn as #1231's auditor does), not an agent loop, and it executes only behind the activation fence #1231 owns; in this child every real run stays structural, makes zero reviewer calls and freezes the structural contract as in #1226, and the reviewer is exercised through tests that set the stored setting directly. It produces one verdict-bearing review per plan submission from at most two invocations (the initial call and one non-resetting transport/schema retry) sharing an aggregate ceiling of 5 minutes wall and 40,000 normalized provider tokens, `sonnet`-tier by default, owner-configurable per repo and frozen for the run; there is no recovery loop. The ceiling also persists across the run: at most three verdict-bearing reviews (the initial submission plus two plan revisions) and an aggregate 15 minutes / 120,000 tokens of coverage review per run; a further revision reports coverage as uncertain without a model call and takes the D3 path. A reviewer that exceeds its ceiling, or fails transport or schema validation on both invocations, reports coverage as uncertain and takes the D3 path; it never blocks approval silently and never counts against #1231's completion-audit budget.

Criteria must be atomic per milestone: one criterion per independently verifiable behavior, so that a partially delivered milestone (the #1220 M3 case) has an unmet criterion rather than a coarse one the auditor cannot judge. The reviewer flags a criterion that bundles several behaviors as uncertain.

Missing/uncertain coverage returns through the existing plan-revision loop. For a human-gated run, the final gate renders all criteria and uncertainties before approval. The approved contract and hashes freeze atomically with plan/milestones.

### D3: Do not pretend autopilot has a human gate

On a semantic-profile run, autopilot normally freezes its plan on a running report. If independent source coverage is fully represented, it may continue through the existing auto-approve path. If coverage is missing/uncertain or the contract-review pass is unavailable on both invocations, disable the worker-side auto-approve skip and use the existing `SetRunAwaitingApproval` path from `running` with the coverage report. Gate notifications fire through the existing inbox/Slack paths, and wall time is banked while waiting as at other approval parks. The owner decides; the run does not fail or auto-approve uncertainty.

Add the exact server transition/guards required for this autopilot-to-gate fallback. Do not describe a nonexistent approval park.

### D4: Separate worker and maintainer criteria

`kind=maintainer` represents production validation the worker lacks authority/infrastructure to perform, such as hosted-k8s rollout observation. It is excluded from the worker permit predicate and rendered as an explicit outstanding checklist in the PR body and run detail. A worker never ticks it complete or lies to obtain a permit.

Code/doc/test criteria remain worker-verifiable. #1231 decides whether their evidence is satisfied, unsatisfied or unverified at the exact head.

### D5: Hash drift is a detector

Source hashes detect that the branch weakened/moved the PRD or that a new issue edit arrived after approval. Drift produces a visible contradiction and requires owner review or contract revision. It does not silently replace the approved contract, and checked boxes/summary words cannot mark a criterion complete.

## Milestones and dependency plan

Five sequential milestones because candidate generation, independent review and freeze share one approval transaction.

| Phase | Milestone | Dependencies | Primary areas | Outcome |
|---|---|---|---|---|
| 1 | M1: semantic contract DTO and bounds | #1226 contract seam | protocol/apitypes/store | Stable criteria and snapshots have one versioned shape. |
| 2 | M2: compiler and independent coverage review | M1 | prompt/tools, new plan-contract reviewer | Complete sources are compared with the proposed plan. |
| 3 | M3: human plan-gate integration | M1-M2 | runner gate, store approval queries, web | Owner sees and freezes the exact contract. |
| 4 | M4: autopilot fallback and maintainer criteria | M2-M3 | autopilot runner/SQL, PR/run display | Semantic-profile uncertainty cannot auto-approve; non-worker proof stays outstanding. |
| 5 | M5: drift, docs and adversarial tests | M1-M4 | tests, docs/specs/ADR | Omissions and false authority are regression-proof. |

- [ ] **M1: semantic contract DTO, per-repo setting and bounds.** Extend the existing contract without breaking #1226: source hashes/snapshots, criteria kinds/text/evidence, coverage verdict and inactive semantic profile marker. Add the per-repo semantic setting storage and read seam (additive column, default off, readable by API and `uzi repo`, no write path) with no runtime effect: every new issue run stays `profile=structural` regardless of the stored value until #1231's activation fence, and a structural-profile run carries no semantic criteria. Define count/text/snapshot/evidence bounds and legacy absence. Test malformed IDs, duplicate criteria, oversized fields and unknown kinds across API/agent wire. Gates: `task gate:api`, `task gate:agent`.
- [ ] **M2: compiler and independent coverage review.** Add lead proposal fields and a separate tool-less read-only reviewer on `runReadOnlyModelPass` whose authoritative prompt payload comes from the server, not repository instructions. Return represented/missing/uncertain source requirements with bounded citations into snapshots, and flag bundled non-atomic criteria as uncertain. Enforce the D2 ceilings (per review: at most two invocations, 5 minutes and 40k tokens aggregate, one non-resetting retry, no recovery loop; per run: three reviews, 15 minutes, 120k tokens, persisted across revisions) and keep the reviewer behind the activation fence: a normal run in this child makes zero reviewer calls; tests reach it by setting the stored setting directly and stubbing the fence. Test the #1220-shaped five-milestone source against a plan that omits M5 and a plan that collapses partial M3 criteria, plus ceiling exhaustion reporting uncertain and a real run (any stored setting value) making zero reviewer calls. Gate: `task gate:agent`.
- [ ] **M3: human plan-gate integration.** Render the proposed semantic contract and coverage report; route missing/uncertain results to revision; freeze the accepted contract/hashes with plan/milestones in one transaction. Test stale verdict, edited source, concurrent approval/revision and idempotent gate reports with LiveDB. Gates: `task gate:api`, `task gate:agent`, `task gate:web`.
- [ ] **M4: autopilot fallback and maintainer criteria.** On a semantic-profile run, permit autopilot only after a complete coverage verdict; otherwise disable the worker's auto-approve skip and report the existing owner-visible approval gate with notifications, banked wall time and no duplicate planning. Update `docs/scheduling.md` and autopilot documentation because `auto_approve=true` now has this explicit uncertainty fallback. Render maintainer criteria as outstanding and exclude them from worker completion counts. Test uncertainty/unavailable review, gate notification, timer/budget accounting, approval/resume and a hosted-k8s criterion that cannot deadlock worker completion. Gates: `task gate:api`, `task gate:agent`, `task gate:web`.
- [ ] **M5: drift, docs and adversarial tests.** Add source-hash drift and mutable-checkbox contradiction displays without treating them as authority. Document contract profiles, autopilot fallback and maintainer outstanding proof; update ADR 1225/specs and run `task docs:sync`. Mutation tests must prove removing source-coverage comparison admits the omitted M5 plan, while the fixed version requires revision. Run relevant gates once to logs.

## Acceptance criteria

- Every semantic criterion maps to an issue/PRD requirement and approved milestone with a stable server ID.
- On a semantic-profile run, a missing/uncertain source requirement cannot pass the human gate or autopilot silently; a structural-profile run makes no reviewer call and keeps its existing approval behavior.
- Semantic-profile autopilot uncertainty reaches `awaiting_approval`, not failed or running.
- The owner sees the exact contract and coverage report before it freezes.
- Maintainer-only proof is explicit/outstanding and cannot deadlock a worker permit.
- Source or checkbox drift cannot silently rewrite or satisfy the contract.
- #1226's structural permit remains wire-compatible until #1231 activates semantic enforcement; the per-repo setting has storage and a read seam only, with no write path and no profile or reviewer effect, until then.
- The worker needs no open-web access.

## Review record

- 2026-09-09: Split from #1225 so source-contract quality lands separately from the structural safety interlock and completion auditor. Reviewed by `@help1` and `@brainstorm`.
- 2026-09-09: Cost bounding pass with `@vasile` (Codex): coverage review becomes one verdict-bearing review per submission of at most two tool-less invocations under a 5-minute / 40k-token ceiling, with a run-level cap of three reviews and 15 min / 120k tokens, runs only under the per-repo semantic setting (only its default-off storage and read seam live in this child, with no write path and no runtime effect; #1231 owns writes and the activation fence), and must produce atomic criteria so the single-pass auditor can catch a partially delivered milestone.
