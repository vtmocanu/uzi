# Epic PRD #1225: Completion interlock with same-run rework and durable holds

**Issue:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Epic, not directly runnable. Implementation is split into eight ordered child PRDs.
**Priority:** High.
**Origin:** Review of [PR #1220](https://github.com/vtmocanu/uzi/pull/1220) and run `b0ffd0ea` on 2026-09-09.
**Execution:** Keep only the `PRD` label on this epic. Exactly one child carries `uzi` at a time; #1226 is first. Reviewers: existing `@help1` and `@brainstorm` Claude sessions.

No implementation or validation under this epic may create or modify `.github/workflows/**`. Child migrations are numbered above the live head when each child lands. The children run sequentially to avoid shared `runner.ts`, completion-state and migration-constraint collisions.

## Problem and intended outcome

Uzi asks a lead to declare only milestones it actually completed, but trusts that declaration. `SetRunCompleted` accepts any subset; the worker opens the PR before reporting terminal completion; the normal issue body includes `Closes #N`.

PR #1220 proved the failure: its run froze `m1..m5`, declared `m1..m4`, used only 8 of 25 iterations, had no scope cap, contradicted its own PRD on M3/M5, and still opened a closing PR. This is a lifecycle bug, not a prompt-quality problem.

The final product rejects an incomplete completion attempt without failing the run or losing its work. It checkpoints Git and provider context, returns exact missing requirements to the same lead session while progress continues, and parks recoverably for owner action after bounded no progress. A closing PR for an interlocked issue run requires a profile-appropriate server permit bound to the exact final head: structural for structural-profile runs, independently audited semantic for semantic-profile runs.

> Reject the completion attempt, not the run. Fail closed on publication and completion, preserve the work, and rework with the same lead by default.

## Ordered children

| Order | Child | Delivery | Depends on |
|---|---|---|---|
| 1 | [#1226 Structural completion interlock and exact-head permit](done/1226-structural-completion-interlock.md) | Frozen structural contract, hard claim clause, same-lead rework, permit and recoverable same-worker hold | none |
| 2 | [#1227 Owner completion decisions and scope-reduced delivery](1227-completion-owner-decisions.md) | Continue/partial/accept authority, real contract revisions and truthful closing behavior | #1226 |
| 3 | [#1228 Shared encrypted provider-context generations](1228-shared-context-generations.md) | One dark storage/codec primitive for run holds and #1214 PR sessions | #1227 by queue policy; no technical dependency |
| 4 | [#1229 Durable run-hold capture and cross-worker resume](1229-durable-run-holds.md) | Verified provider context plus Git before park; restore before inspect on another worker | #1226 + #1228 |
| 5 | [#1230 Semantic completion contract and plan coverage review](1230-semantic-completion-contract.md) | Source-backed criteria, independent plan coverage and autopilot fallback | #1226 |
| 6 | [#1231 Independent completion auditor and semantic permit](1231-completion-auditor.md) | Exact-head full audit (tool-less, two-pass cap, fixed token/wall budget), captured gate evidence, semantic permit, per-repo semantic opt-in and bounded recovery | #1226 + #1230 |
| 7 | [#1233 Structured completion blockers and governed MR rework](1233-structured-blockers-mr-rework.md) | Server-owned blocker dispositions and contract-governed automatic/manual rework | #1227 + #1230 + #1231 |
| 8 | [#1232 Completion interlock integration, rollout and epic close-out](1232-completion-interlock-integration.md) | End-to-end/race proof, default enablement, consolidated docs and epic close | #1226-#1231 + #1233 |

Queue policy: remove `uzi` from the current child after its PR merges and main CI is green, then add `uzi` to the next child. The scheduled sweep's `max_issues=3` is not a serialization guarantee; the single eligibility label is.

## Binding cross-child contracts

These seams are fixed here so each merged child extends rather than replaces its predecessor.

1. **Contract identity.** `completion_contract_version` is nullable for legacy rows. Schema v1 has `profile=structural|semantic` and a separate monotonically increasing contract revision. #1226 creates structural contracts; #1230 fills semantic criteria; #1231 activates semantic enforcement.
2. **Hard worker protocol.** Interlocked claims have dedicated positive clauses outside `required_capabilities`, owner override and the capability kill-switch. Structural contracts require `completion_interlock_v1`; semantic contracts additionally require `completion_audit_v1`.
3. **Permit wire.** The request reserves optional audit and finding fields from #1226. Structural permits accept null/empty fields; semantic permits require them after #1231.
4. **Completion transaction.** Permit consumption and `SetRunCompleted` are one service transaction with a generation-activation hook so #1229/#1214 do not replace the terminal seam.
5. **Hold writer.** `SetRunCompletionHold` positively admits owned `running|awaiting_input` attempts. It is distinct from owner-requested `SetRunPaused`.
6. **Capture hook.** Park ordering is Git capture/ACK, `captureHoldContext`, hold transition/ACK, then release. #1226 returns `same_worker_only`; #1229 supplies durable context without reordering the fence.
7. **Claim handoff.** Completion contract/attempt/hold metadata is one additive object. Provider context uses a distinct server handoff, never `session_id` as authority.
8. **Owner decision path.** One `completion_decision` input/endpoint begins with continue in #1226 and gains partial/accept in #1227.
9. **No-progress routing.** After attempt one, no-progress and budget exhaustion call one completion-hold function; before attempt one, legacy behavior remains.
10. **Alignment.** Keep ADR-456 alignment in `phasePublish`. Structural permits rebind to its final head. Semantic evidence invalidated by a changed head recovers/requeues for new gates and audit rather than moving the large finalizer into the lead loop.
11. **Context owner kinds.** #1228 defines `run_hold|pr_session`; #1229 implements run-hold policy; #1214 later implements PR-session policy. Their authorization, consent and lifetimes never cross.
12. **MR rework.** It stays legacy until #1233 copies/activates the source contract and current structured findings. #1214 context is optional continuity, never completion evidence.

## Issue relationships and queue order

#1214 currently retains `Planned` but not `uzi`. Re-add `uzi` only after #1228 has merged and its PRD has been amended to consume the shared primitive, and never on a night when a #1225 child also carries `uzi`. The preferred completion-first order is #1226, #1227, #1228, #1229, #1230, #1231, #1233, #1232, then #1214. A maintainer may choose #1214 after #1228, but it and #1229/#1231/#1233 share runner/resume seams and must still execute one at a time.

#1088 provider-outage recovery is related but not a prerequisite. #1202 already provides on-demand post-PR rework. #1171 may later supply a verified Codex codec; an unmerged/provisional branch is not production support.

## Epic acceptance

- [ ] #1226 merged: a 4/5 completion cannot open a closing PR; same-lead structural rework and safe hold exist.
- [ ] #1227 merged: only the owner/admin can continue, reduce exact scope or accept exact criteria with a reason.
- [ ] #1228 merged: one encrypted, bounded provider-context primitive exists for both consumer lifecycles.
- [ ] #1229 merged: completion holds preserve Git and supported provider context across worker replacement.
- [ ] #1230 merged: the per-repo semantic setting has default-off storage and a read seam with no write path or runtime effect, and the dormant compiler/reviewer produce independently reviewed source-backed semantic contracts under test.
- [ ] #1231 merged: on repos with the semantic setting on, a bounded full exact-head audit gates completion on capable workers.
- [ ] #1233 merged: structured blocker policy gates completion and governed automatic/manual MR rework.
- [ ] #1232 merged: integrated recovery/compatibility/mutation proof passes and new issue runs use the structural interlock by default; the semantic profile stays a per-repo opt-in.
- [ ] #1214 is amended to consume #1228 and is queued only after the shared seams are stable.

## Maintainer-operated post-merge validation

These are release checks, not worker-frozen child milestones:

- [ ] Same-worker completion rework retains the original provider session on hosted k8s.
- [ ] Worker deletion followed by another worker restores exact Git and provider context.
- [ ] Mixed old/new worker rollout leaves contracted runs visibly waiting and never misclaims them.
- [ ] API restart between permit and terminal report recovers idempotently.
- [ ] Full, reduced-scope and accepted-criteria PR bodies have correct closing behavior on the live forge.

Record sanitized commands/outcomes, commit/image versions and dates. Never mark a row complete merely because component gates passed.

## Risks and durable decisions

- Structural containment lands before semantic judgment and states that limitation.
- Same-worker context lands before durable cross-worker recovery and is exposed as degraded, not hidden.
- Independent plan/final audits add bounded owner-token cost, and the bound is concrete: the coverage review is at most two invocations per verdict-bearing review sharing 5 min and 40k tokens, capped at three reviews and 15 min / 120k tokens per run and the completion audit at most two tool-less passes (5 min each, 10 min aggregate, 100k tokens) over a worker-built packet with captured gate evidence, so review time scales with the diff, not with implementation time. Deterministic checks run first. Both run only on repos that opt into the semantic profile; the structural interlock costs no tokens and is the default.
- A PAT-holding compromised worker remains outside scope; the hard claim clause addresses normal version skew.
- Finalize-time base-align conflicts, non-fast-forward rejection and push-secret blocking retain their existing typed preserved-work behavior; this epic changes incomplete-completion handling, not every terminal finalization error.
- A provider without a verified codec cannot be called cross-worker durable.
- Moving the ADR-456 alignment block would be a large risky refactor; the children instead invalidate/recover semantic evidence on head drift.
- No child may publish a partial closing PR because a later child is pending. Each PR states its own limited delivered behavior and leaves the epic open.

ADR 1225 is created by #1226 and amended by later children. It records contract freeze, hard capability, permit/head binding, same-lead rejection, hold/capture order, owner authority, independent semantic audit and context-owner separation. `specs/human.md` receives a narrow structural requirement in #1226 and the full approved requirement in #1231.

No `docs/ROADMAP.md` exists in the inspected checkout.

## Review record

- 2026-09-09: Initial combined PRD drafted after a three-round design discussion between the lead Codex session and `@brainstorm`.
- 2026-09-09: `@help1` found five blocking defects: bypassable capability gating, unreachable pause transition, failing no-progress path, non-worker k8s acceptance and excessive scope.
- 2026-09-09: `@brainstorm` independently confirmed all five, identified ADR-456 movement as an additional risk, and proposed stable cross-child seams.
- 2026-09-09: User chose an epic with smaller tasks executed one by one. Final deliverability review split structured blockers/MR rework from the auditor; the eight-child sequence above is the reconciled design. No implementation run started.
- 2026-09-09: Both peers re-reviewed the final document set after corrections and approved it with no blocking findings. Residual operational risks are recorded in the owning children; only #1226 is sweep-eligible.
- 2026-09-09: Maintainer asked that a long implementation run not earn a proportionally long review. Lead Claude session and `@vasile` (Codex) agreed the bounds now recorded in #1230 D2, #1231 D1/D2/D4/D6 and #1232 D4: tool-less auditor on the advice-lane primitive, captured gate artifacts with re-execution as fallback, no incremental audits, two-audit cap with numeric wall/token ceilings, and the semantic profile as a per-repo opt-in with structural as the default. #1226 is unchanged.
