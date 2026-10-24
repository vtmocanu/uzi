# PRD #1230: Semantic completion contract and plan coverage review

**Issue:** [#1230](https://github.com/vtmocanu/uzi/issues/1230)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Done for its dormant scope (2026-10-23): M1-M5 merged (PR #3243, `847e512a`) and owner check m6 evidenced on 0.86.0-rc.22. Activation (#1231) is not planned, so the semantic path stays dormant; see #1225. The text below records the design as delivered, including the #1231 activation preconditions that no longer have an owner.
**Priority:** Normal (current issue taxonomy).
**Execution:** None. Delivered; never dispatch. Do not activate the dormant semantic path without a new reviewed design (#1231 is not planned).

This child makes arbitrary issue/PRD requirements explicit at planning time. It does not yet make semantic verdicts a completion-permit requirement; #1231 owns that enforcement. No implementation or validation may modify `.github/workflows/**`.

## Owner transport activation precondition (AI-synced 2026-10-21)

Maximum-size owner semantic transport is not production-readiness proof. An owner semantic gate can approach 80 MiB before the ordinary run fields, while the production API listener has 15-second read and write timeouts. A four-core worker reported about 11 seconds of server CPU for a maximum-size owner decision and race-enabled maximum-size LiveDB timeouts. These reports are not a measured production wall-time margin. The race fixtures use two-minute client budgets and omit the production listener deadlines; the non-race fixtures retain them. #1231 must verify maximum-size owner reads and decisions against production transport deadlines before activation.

## Problem and outcome

#1226 prevents a run from omitting an entire frozen milestone, but a lead can still propose a plan that omits a source requirement or declare a milestone complete while only part of its prose acceptance is delivered. PR #1220 showed the latter for M3. A completion auditor cannot enforce requirements that never entered a stable contract.

Build the semantic lane dormant: no repo setting/storage/read seam and no runtime activation in this child. #1231 introduces default-on instance activation, admin model/cost controls and a creation-time frozen enrollment profile, without a fleet-readiness latch. Once activated, compile issue/linked-PRD snapshots and the proposed plan into stable criteria with evidence requirements. Independently compare the complete source at the exact approval decision point. Human-gated runs show uncertainties; autopilot missing/uncertain coverage or a newly unapproved maintainer exception takes `awaiting_approval`, not silent acceptance.

The dormant schema, compiler and reviewer land in this child, but a real run exposes no semantic contract, carries no semantic criteria and makes no reviewer call until #1231 activates default-on semantic enrollment; enforcement stays structural/legacy until then. That limitation must be explicit in the run and PRD status.

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

Missing/uncertain coverage uses the existing human-gate revision path only, never checker-REVISE provenance. The human gate renders all criteria and uncertainties before approval; the approved contract/hashes freeze atomically with plan/milestones.

Run coverage only for the exact candidate about to proceed: after an applicable required Plan cross-check APPROVE and before auto-approval, or when presenting a candidate at a human gate. If Plan cross-check is disabled/not required, coverage precedes the existing approval decision without introducing a new cross-family requirement. Intermediate automatic checker REVISE rounds consume no coverage review. Bind the verdict to the existing exact-candidate digest plus frozen source-snapshot and criteria-set identity; any changed candidate/source/criteria invalidates the coverage verdict and any applicable cross-check authorization before proceeding. Extend identity handling explicitly for semantic proposals without silently reinterpreting older stored cross-check digests. No string heuristic or separate verdict-key store is introduced.

For semantic proposals, include the canonical criteria set and frozen source identity in the exact candidate identity and checker payload. A dedicated durable checker protocol capability is a hard claim clause on the checker child, mirrored in every claim, placement, health, spread and provisioning copy (the ADR-2149 `cross_check_codex_lead_v1` precedent) and outside overrides. The digest scheme is selected by the run's frozen semantic enrollment, never by the claiming worker's capability or the #2150 per-sequence snapshot. With no capable checker, the check takes the existing deadline-to-human-gate path; it never falls back to the structural digest or payload. Existing structural/legacy digest meanings stay unchanged. M4 records this semantic digest extension as an ADR-2149 amendment together with the coverage-park reason, and tests capability refusal for old checkers/workers. A human revision never reuses historical checker approval as a certificate for the new plan.

On autopilot, missing, uncertain, exhausted or unavailable coverage goes directly to D3's human gate, even after a checker APPROVE; it never starts an automatic revision round. The checker verdict is retained as historical evidence. Human revisions follow ADR-2149 D16: no new cross-family checker pass is triggered for the human-revised plan. Coverage is separate: present that revised candidate's own coverage report at its human decision point under D2's non-resetting budgets; never reuse the earlier candidate's verdict. If coverage is exhausted or unavailable, show the revised candidate's unresolved coverage and consumed/remaining budget without another model call. Exact owner approval may authorize that candidate with the displayed uncertainty and binds its digest, source snapshot and criteria set; any later candidate/source/criteria change invalidates that approval binding. Approval authorizes only the maintainer-kind IDs and conditions rendered explicitly in that candidate (D4); uncertain coverage never creates or widens a maintainer classification. Auto-approval still requires complete coverage. Record this additive coverage-park reason in an ADR-2149 amendment when M4 lands; do not fabricate checker REVISE provenance.

The D2 budgets remain independent of Plan cross-check and human revisions: #3091's larger allowance grants no extra coverage calls, reset or waiver. Auto-approval needs complete coverage and any required checker APPROVE on the same current candidate. Coverage-cap exhaustion takes the human gate without a model call, regardless of remaining checker allowance; display consumed/remaining budget and document this limit. A provider safety-policy refusal in a coverage advice pass is coverage-unavailable under D3, not a #3068 lead-policy hold, harness switch or waiver.

### D3: Do not pretend autopilot has a human gate

On a semantic-profile run, autopilot normally freezes its plan on a running report. If independent source coverage is fully represented, it may continue through the existing auto-approve path. If coverage is missing/uncertain or the contract-review pass is unavailable on both invocations, disable the worker-side auto-approve skip and use the existing `SetRunAwaitingApproval` path from `running` with the coverage report. Gate notifications fire through the existing inbox/Slack paths, and wall time is banked while waiting as at other approval parks. The owner decides; the run does not fail or auto-approve uncertainty.

Add the exact server transition/guards required for this autopilot-to-gate fallback. Do not describe a nonexistent approval park.

### D4: Separate worker and maintainer criteria

`kind=maintainer` represents genuine owner-only acceptance the worker lacks authority/infrastructure to perform, such as hosted-k8s rollout observation; failed worker code/doc/test/security/review requirements do not qualify. The proposed kind grants no exemption. Exact server-minted IDs, classification, remaining verification and conditions must be bound to an explicit human-approved plan or owner contract revision. Ordinary answers, mutable checkboxes, lead assertions and auto-approval alone cannot authorize a new exception. New or changed maintainer classification forces the existing human gate, including on autopilot, until the exact proposal is authorized.

Only frozen authorized maintainer criteria are excluded from worker requirements. When those alone remain and all worker/publication checks pass, delivery needs no second equivalent completion-time decision. Reuse the existing revision/decision/permit ledger, not a separate acceptance tracker. Delivery is explicitly non-closing across supported title/body/commit publication paths, leaving the source issue open. PR/run/CLI show pending unverified stable IDs and actionable steps; no display checkbox or `accept` marks unperformed acceptance satisfied.

Consume #3089's granted revision-bound `completion_scope` and #3090's shipped pre-push selection/post-push consistency seams at the merged SHAs above. The publication-start running ACK's `completion_revision` and `completion_deferred` select protection; the post-push permit must match the acknowledged revision and deferred-ID set, and its full scope remains rendering/publication authority. Verify actual call order, not helper declaration positions. (AI-synced 2026-10-18)

Frozen maintainer-pending criteria add an effective non-closing source to the same computation as lead-declared intent (stored reason, attempt/claim `non_closing`, feature `completion_non_closing_v1`), owner `partial` and scope-capped delivery. Feed the existing early/exact-tip closing-commit guard and both consistency-checked projections, never a second scanner or ledger. Preserve existing milestone-ID semantics: represent pending criterion identities in an explicitly versioned extension or sibling projection derived from the same frozen contract/revision, with equal pending identities before/after push; never silently insert criterion IDs into the legacy deferred-milestone set or double-count the same work. Rendering reuses #3090's unchecked server-sourced deferred block, with all displayed IDs/text/reasons/conditions redacted and escaped; authoritative actionable conditions come only from the frozen authorization, while lead prose stays untrusted outside that block. The pre-push refusal inherits #3090's hold-not-fail and zero-attempt admission path, including its documented last-resort and version boundaries. (AI-synced 2026-10-18)

Semantic support requires explicit negotiated semantic capability; `completion_non_closing_v1` alone is not semantic readiness. A worker lacking the necessary publication capability must not publish maintainer-pending delivery as closing: capability routing and fail-closed hold apply, without changing structural/legacy profiles. #1231 owns production activation. The `mr_rework`, branch-adopting `ci_fix`, previously published history and Forgejo/Gitea any-branch residuals stay outside this issue's protection as documented by #3090; [#3120](https://github.com/vtmocanu/uzi/issues/3120) tracks that separate follow-up. On semantic runs, owner-only acceptance uses `kind=maintainer` criteria instead of #3090's separate-final-milestone guidance; structural runs retain that convention. No stored contract is rewritten. (AI-synced 2026-10-18)

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

- [x] **M1: semantic contract DTO and bounds.** Extend the existing contract without breaking #1226: source hashes/snapshots, criteria kinds/text/evidence, coverage verdict and dormant semantic enrollment marker. No repo preference column, setting/read/write seam or production activation is added. Every real run stays structural/legacy until #1231 and structural contracts carry no semantic criteria. Define count/text/snapshot/evidence bounds and legacy absence; test malformed/duplicate IDs, oversize and unknown kinds across API/agent wire. Gates: `task gate:api`, `task gate:agent`.
- [x] **M2: compiler and independent coverage review.** Add lead proposal fields and the tool-less reviewer, with server-supplied snapshots treated as requirements data, never executable authority overriding worker rules. Return bounded coverage citations and flag non-atomic criteria. Enforce D2 ceilings at exact approval decision points. Test omitted M5, partial M3, multiple checker REVISE rounds making zero coverage calls, approval-point review on both harnesses, changed candidate/source/criteria invalidation and coverage-cap exhaustion with checker rounds left. Tests inject the dormant path; every production enrollment makes zero calls in this child. Gate: `task gate:agent`.
- [x] **M3: human plan-gate integration.** Render the proposed semantic contract and coverage report; route missing/uncertain results to revision; freeze the accepted contract/hashes with plan/milestones in one transaction. Test stale verdict, edited source, concurrent approval/revision and idempotent gate reports with LiveDB. Gates: `task gate:api`, `task gate:agent`, `task gate:web`.
- [x] **M4: autopilot fallback and authorized maintainer criteria.** Verify merged #3089/#3090 interfaces and pre-push call order first. In the dormant semantic path, autopilot needs complete coverage and no newly unapproved maintainer exception; otherwise take the human gate directly with notifications, banked wall and no duplicate planning or synthetic checker REVISE. Add the ADR-2149 coverage-park amendment. Freeze exact human-authorized IDs/conditions; render pending checklists and permit non-closing delivery without a redundant decision only under that authorization. Test ordinary answers/checkboxes/auto-approval cannot reclassify failed worker requirements, stale/foreign/revision refusals, and no double-counting of a #3090-style deferred milestone and maintainer criterion in the dormant path. Update scheduling/autopilot docs honestly; production stays dormant until #1231. Gates: `task gate:api`, `task gate:agent`, `task gate:web`; execute affected LiveDB tests through `./e2e/run-store-it.sh` with named PASS/no SKIP.
- [x] **M5: drift, docs and adversarial tests.** Add source-hash drift and mutable-checkbox contradiction displays without treating them as authority. Document contract profiles, autopilot fallback and maintainer outstanding proof; update ADR 1225/specs and run `task docs:sync`. Mutation tests must prove removing source-coverage comparison admits the omitted M5 plan, while the fixed version requires revision. Run relevant gates once to logs.

## Deferred owner acceptance (m6)

- [x] **m6: post-deploy dormancy check (AI-synced 2026-10-21; evidenced 2026-10-23 on 0.86.0-rc.22: `semantic_planning` has 0 rows; a contract-v1 structural issue run completed on an rc.22 worker. The schema has no advice-pass label, so the original "no coverage usage label" check is not directly observable. Reviewed production code keeps coverage unavailable without the injected test transport. Auxiliary searches found no `coverage` matches in the inspected usage/session fields).** The repo owner or maintainer confirms after deploy that `SELECT count(*) FROM semantic_planning` returns zero, run usage has no `coverage` label, and a structural run completes on an existing worker. The worker has no deploy or cluster access. The owner's partial decision keeps run milestones m1–m5 and defers only m6; it waives no implementation validation. Evidenced; #1230 closed as completed for its dormant scope.

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

- 2026-10-21 (AI-synced, PR #3243 review): three dormant-path fixes from bot review. (1) A semantic candidate submitted without a proposal is an exact replay: the server rebuilds the proposal from the persisted candidate's embedded proposed criteria and accepts only an identical fingerprint, so a gate re-presented after a release/reclaim reopens instead of failing; a replay never mints a first or new candidate. (2) A worker state ACK carrying `completion_pending` sets `X-Uzi-Completion-Pending: v1`, and the worker adds a 20 MiB allowance to that ACK's cap only under it; the structural cap is unchanged and a header without a projection fails closed. (3) An issue linking more than eight PRDs keeps its exact issue capture (no PRD is read; coverage stays unknown), since the server refuses issue bytes that differ from the frozen run.

- 2026-10-21 (AI-synced, delivery): M1–M5 implemented dormant on the #1230 branch. Semantic planning storage is migration 00347; capability tokens `cross_check_semantic_plan_v1` (checker claim clause) and `completion_semantic_publication_v1` (lead claim clause and API feature); one server coverage authority; autopilot parks with a bounded `semantic_park` reason (none when only the checker gates); maintainer-pending criteria publish non-closing with ACK/permit sibling equality; D5 drift observations are a detector only (bounded, sticky contradiction, resumed runs observe via the worker coverage status). Mutation proof: removing the represented comparison in the server coverage authority or the agent display predicate admits the omitted M5 plan and turns the tests red. The omitted-M5 regression covers a requirement the report marks missing or uncertain; a requirement absent from the report altogether is outside what the server can know. Deferred: title-only issue edits are not compared; the presentation-phase PRD read is at the capture commit.

- 2026-10-18 (AI-synced, dispatch readiness): verified the shipped #3098/#3117 interfaces; clarified candidate-bound human approval after bounded coverage uncertainty, distinct from ADR-2149 D16's cross-family checker; pinned maintainer-pending publication to the existing guards and versioned same-contract projections. This remains dormant design, not activation or deployment evidence. Exact revised-head peer review is required before landing the refresh.

- 2026-10-17 (AI-synced, planned refresh): default-on #1231 enrollment replaces default-off repo opt-in; controls/ceilings are instance-admin only at launch and repo overrides are deferred. #1230 stays dormant without a preference column; coverage runs at exact approval decision points, not intermediate checker revisions. Exact human maintainer authorization and independent numeric budgets remain. #1228 is not a semantic prerequisite; #1229 stays not planned; #3089/#3090 need merged integration evidence. This is design documentation, not rollout evidence; exact-diff peer/independent review is pending.

- 2026-09-09: Split from #1225 so source-contract quality lands separately from the structural safety interlock and completion auditor. Reviewed by `@help1` and `@brainstorm`.
- 2026-09-09: Cost bounding pass with `@vasile` (Codex): coverage review becomes one verdict-bearing review per submission of at most two tool-less invocations under a 5-minute / 40k-token ceiling, with a run-level cap of three reviews and 15 min / 120k tokens, runs only under the per-repo semantic setting (only its default-off storage and read seam live in this child, with no write path and no runtime effect; #1231 owns writes and the activation fence), and must produce atomic criteria so the single-pass auditor can catch a partially delivered milestone.
