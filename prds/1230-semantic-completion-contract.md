# PRD #1230: Semantic completion contract and plan coverage review

**Issue:** [#1230](https://github.com/vtmocanu/uzi/issues/1230)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Planned; structural prerequisites #1226/#1227 and both-harness rollout #1626/#1627 are shipped. The complete child remains blocked on merged #3089 handoff/scope propagation and corrected #3090 non-closing publication for D4/M4. #1228 is unrelated provider-context work, not a technical prerequisite; #1229 remains not planned.
**Priority:** Normal (current issue taxonomy).
**Execution:** Keep `Planned` without `uzi`. Verify the final merged #3089/#3090 interfaces and obtain exact scope/plan review before dispatching this complete child; then require green main CI. M1–M3 can be designed independently, but that does not authorize dispatch with unshipped M4 prerequisites. This refresh changes documentation, not live enrollment or settings.

This child makes arbitrary issue/PRD requirements explicit at planning time. It does not yet make semantic verdicts a completion-permit requirement; #1231 owns that enforcement. No implementation or validation may modify `.github/workflows/**`.

## Problem and outcome

#1226 prevents a run from omitting an entire frozen milestone, but a lead can still propose a plan that omits a source requirement or declare a milestone complete while only part of its prose acceptance is delivered. PR #1220 showed the latter for M3. A completion auditor cannot enforce requirements that never entered a stable contract.

Build the semantic lane dormant: no repo setting/storage/read seam and no runtime activation in this child. #1231 introduces default-on instance activation, admin model/cost controls and a creation-time frozen enrollment profile, without a fleet-readiness latch. Once activated, compile issue/linked-PRD snapshots and the proposed plan into stable criteria with evidence requirements. Independently compare the complete source at the exact approval decision point. Human-gated runs show uncertainties; autopilot missing/uncertain coverage or a newly unapproved maintainer exception takes `awaiting_approval`, not silent acceptance.

The dormant schema, compiler and reviewer land in this child, but a real run exposes no semantic contract, carries no semantic criteria and makes no reviewer call until #1231 opens the activation fence; enforcement stays on #1226's structural profile until then. That limitation must be explicit in the run and PRD status.

## Included

- Bounded source snapshots/hashes for issue, linked PRD and approved plan.
- Semantic criteria with stable server-minted IDs, kinds, done-when text and evidence slots.
- Independent plan-source coverage review at the exact approval decision point, with each invocation a bounded tool-less turn. Dormant here, reachable only through explicit test injection until #1231 activates semantic enrollment.
- Plan-gate rendering and revision for missing/uncertain requirements.
- Autopilot fallback to human approval on uncertain coverage.
- Maintainer-only criteria represented as outstanding, not worker-completed.
- Source drift/contradiction signals and web/CLI contract visibility.
- A dormant semantic enrollment/contract schema, not an opt-out setting. #1231 owns default-on instance policy, admin controls and creation-time enrollment. No per-repo setting or ceilings ship at launch; future nullable overrides are deferred. Coverage review and audit activate together; missing capable workers queue semantic runs rather than changing their profile.
- A dark semantic profile flag reserved for #1231 activation.

## Excluded

- Final completion auditing or semantic permit enforcement, delivered by #1231.
- Structured review findings, MR-rework gating or provider-context transport.
- Open-web requirement research by the worker.
- Making maintainer-only hosted-k8s validation block a worker PR.
- Treating mutable PRD checkboxes as authority.

## Decisions

### D1: Preserve schema version, add a semantic profile

Keep completion contract schema version 1 and distinguish enforcement with `profile=structural|semantic`; do not assume the current JSON contains a separate `v` property. This child defines the dormant semantic shape but all real runs remain structural or legacy until #1231. That child persists semantic enrollment before the first claim, separately from the later atomic contract freeze, so planning-phase capability gating is non-vacuous. The frozen contract agrees with that enrollment; no second criterion ledger. Contract revision remains #1227's owner-decision revision.

Each criterion records stable ID, milestone ID, bounded text, `kind=code|doc|test|maintainer`, and bounded evidence requirements. IDs are server-minted after validating the candidate; the lead cannot choose identities that collide or escape scope.

### D2: Review source coverage independently

The lead proposes criteria as part of `submit_plan`. A separate read-only plan-contract reviewer receives the server-provided issue/PRD snapshots and proposed plan/contract, treats repository/forge text as untrusted data, and reports source requirements as represented, missing or uncertain. It cannot implement, edit, approve or waive.

Each reviewer invocation is one tool-less turn on `runReadOnlyModelPass` with a new `coverage` label, parsing a schema-validated JSON report as #1231's auditor does, not an agent loop. In this child every production run remains structural/legacy and makes zero calls; tests inject the dormant path. Per verdict-bearing review: at most two invocations (initial plus one non-resetting transport/schema retry), sharing 5 minutes and 40,000 normalized provider tokens; `sonnet`-tier default, instance-admin configurable and frozen per run. Per run: at most three verdict-bearing reviews and 15 minutes/120,000 tokens across approval decision points. Exhausted/unavailable review reports uncertainty and takes D3 without another model call. There is no recovery loop and this budget never counts against #1231's audit budget.

Criteria must be atomic per milestone: one criterion per independently verifiable behavior, so that a partially delivered milestone (the #1220 M3 case) has an unmet criterion rather than a coarse one the auditor cannot judge. The reviewer flags a criterion that bundles several behaviors as uncertain.

Missing/uncertain coverage returns through the existing plan-revision loop. For a human-gated run, the final gate renders all criteria and uncertainties before approval. The approved contract and hashes freeze atomically with plan/milestones.

Run coverage only for the exact candidate about to proceed: after an applicable required Plan cross-check APPROVE and before auto-approval, or when presenting a candidate at a human gate. If Plan cross-check is disabled/not required, coverage precedes the existing approval decision without introducing a new cross-family requirement. Intermediate automatic checker REVISE rounds consume no coverage review. Bind the verdict to the existing exact-candidate digest plus frozen source-snapshot and criteria-set identity; any changed candidate/source/criteria invalidates the coverage verdict and any applicable cross-check authorization before proceeding. Extend identity handling explicitly for semantic proposals without silently reinterpreting older stored cross-check digests. No string heuristic or separate verdict-key store is introduced.

The D2 budgets remain independent of Plan cross-check and human revisions: #3091's larger allowance grants no extra coverage calls, reset or waiver. Auto-approval needs a complete coverage verdict and any required checker APPROVE on the same current candidate. Coverage-cap exhaustion still takes the human gate, even with checker rounds left; display consumed/remaining budget and document this limit.

### D3: Do not pretend autopilot has a human gate

On a semantic-profile run, autopilot normally freezes its plan on a running report. If independent source coverage is fully represented, it may continue through the existing auto-approve path. If coverage is missing/uncertain or the contract-review pass is unavailable on both invocations, disable the worker-side auto-approve skip and use the existing `SetRunAwaitingApproval` path from `running` with the coverage report. Gate notifications fire through the existing inbox/Slack paths, and wall time is banked while waiting as at other approval parks. The owner decides; the run does not fail or auto-approve uncertainty.

Add the exact server transition/guards required for this autopilot-to-gate fallback. Do not describe a nonexistent approval park.

### D4: Separate worker and maintainer criteria

`kind=maintainer` represents genuine owner-only acceptance the worker lacks authority/infrastructure to perform, such as hosted-k8s rollout observation; failed worker code/doc/test/security/review requirements do not qualify. The proposed kind grants no exemption. Exact server-minted IDs, classification, remaining verification and conditions must be bound to an explicit human-approved plan or owner contract revision. Ordinary answers, mutable checkboxes, lead assertions and auto-approval alone cannot authorize a new exception. New or changed maintainer classification forces the existing human gate, including on autopilot, until the exact proposal is authorized.

Only frozen authorized maintainer criteria are excluded from worker requirements. When those alone remain and all worker/publication checks pass, delivery needs no second equivalent completion-time decision. Reuse the existing revision/decision/permit ledger, not a separate acceptance tracker. Delivery is explicitly non-closing across supported title/body/commit publication paths, leaving the source issue open. PR/run/CLI show pending unverified stable IDs and actionable steps; no display checkbox or `accept` marks unperformed acceptance satisfied.

Consume the revision-bound scope source #3089 actually lands and the corrected pre-push protections #3090 lands, retaining their documented forge/path boundaries. Both are unmerged dependencies at this refresh: verify merged SHAs and actual call order before dispatch, not helper declaration positions. #3090's structural milestone convention migrates into the single semantic criterion ledger, rather than remaining a parallel classification.

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

- [ ] **M1: semantic contract DTO and bounds.** Extend the existing contract without breaking #1226: source hashes/snapshots, criteria kinds/text/evidence, coverage verdict and dormant semantic enrollment marker. No repo preference column, setting/read/write seam or production activation is added. Every real run stays structural/legacy until #1231 and structural contracts carry no semantic criteria. Define count/text/snapshot/evidence bounds and legacy absence; test malformed/duplicate IDs, oversize and unknown kinds across API/agent wire. Gates: `task gate:api`, `task gate:agent`.
- [ ] **M2: compiler and independent coverage review.** Add lead proposal fields and the tool-less reviewer, with server-supplied snapshots treated as requirements data, never executable authority overriding worker rules. Return bounded coverage citations and flag non-atomic criteria. Enforce D2 ceilings at exact approval decision points. Test omitted M5, partial M3, multiple checker REVISE rounds making zero coverage calls, approval-point review on both harnesses, changed candidate/source/criteria invalidation and coverage-cap exhaustion with checker rounds left. Tests inject the dormant path; every production enrollment makes zero calls in this child. Gate: `task gate:agent`.
- [ ] **M3: human plan-gate integration.** Render the proposed semantic contract and coverage report; route missing/uncertain results to revision; freeze the accepted contract/hashes with plan/milestones in one transaction. Test stale verdict, edited source, concurrent approval/revision and idempotent gate reports with LiveDB. Gates: `task gate:api`, `task gate:agent`, `task gate:web`.
- [ ] **M4: autopilot fallback and authorized maintainer criteria.** Verify merged #3089/#3090 interfaces and pre-push call order first. In the dormant semantic path, autopilot needs complete coverage and no newly unapproved maintainer exception; otherwise use the existing human gate with notifications, banked wall and no duplicate planning. Freeze exact human-authorized IDs/conditions; render pending checklists and permit non-closing delivery without a redundant later decision only under that frozen authorization. Test ordinary answers, checkboxes and auto-approval cannot reclassify failed worker requirements; cover stale/foreign/revision refusals and migration from #3090's structural convention. Update scheduling/autopilot docs honestly; production remains dormant until #1231. Gates: `task gate:api`, `task gate:agent`, `task gate:web`; execute affected LiveDB tests through `./e2e/run-store-it.sh` with named PASS/no SKIP.
- [ ] **M5: drift, docs and adversarial tests.** Add source-hash drift and mutable-checkbox contradiction displays without treating them as authority. Document contract profiles, autopilot fallback and maintainer outstanding proof; update ADR 1225/specs and run `task docs:sync`. Mutation tests must prove removing source-coverage comparison admits the omitted M5 plan, while the fixed version requires revision. Run relevant gates once to logs.

## Acceptance criteria

- Every semantic criterion maps to an issue/PRD requirement and approved milestone with a stable server ID.
- On a semantic-profile run, a missing/uncertain source requirement cannot pass the human gate or autopilot silently; a structural-profile run makes no reviewer call and keeps its existing approval behavior.
- Semantic-profile autopilot uncertainty reaches `awaiting_approval`, not failed or running.
- The owner sees the exact contract and coverage report before it freezes.
- Maintainer-only proof remains outstanding; only exact frozen human-authorized classification excludes it from worker requirements and allows non-closing delivery without another equivalent decision. New unapproved classifications force a human gate, including autopilot.
- Source or checkbox drift cannot silently rewrite or satisfy the contract.
- #1226's structural permit remains wire-compatible; this child adds no per-repo setting or production calls. #1231 activates coverage and audit together by default for new eligible runs, with instance-admin controls; existing profiles never change.
- The worker needs no open-web access.

## Review record

- 2026-10-17 (AI-synced, planned refresh): default-on #1231 enrollment replaces default-off repo opt-in; controls/ceilings are instance-admin only at launch and repo overrides are deferred. #1230 stays dormant without a preference column; coverage runs at exact approval decision points, not intermediate checker revisions. Exact human maintainer authorization and independent numeric budgets remain. #1228 is not a semantic prerequisite; #1229 stays not planned; #3089/#3090 need merged integration evidence. This is design documentation, not rollout evidence; exact-diff peer/independent review is pending.

- 2026-09-09: Split from #1225 so source-contract quality lands separately from the structural safety interlock and completion auditor. Reviewed by `@help1` and `@brainstorm`.
- 2026-09-09: Cost bounding pass with `@vasile` (Codex): coverage review becomes one verdict-bearing review per submission of at most two tool-less invocations under a 5-minute / 40k-token ceiling, with a run-level cap of three reviews and 15 min / 120k tokens, runs only under the per-repo semantic setting (only its default-off storage and read seam live in this child, with no write path and no runtime effect; #1231 owns writes and the activation fence), and must produce atomic criteria so the single-pass auditor can catch a partially delivered milestone.
