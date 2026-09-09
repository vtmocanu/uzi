# PRD #1232: Completion interlock integration, rollout, and epic close-out

**Issue:** [#1232](https://github.com/vtmocanu/uzi/issues/1232)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Planned, blocked on #1226 through #1231 and #1233.
**Priority:** High.
**Execution:** Keep `Planned` without `uzi`. Add `uzi` only after #1233 merges and main CI is green.

This child owns no new completion contract or storage policy. It integrates the previously merged children, proves their combined lifecycle, flips the rollout default, consolidates documentation and closes epic #1225. No implementation or validation may modify `.github/workflows/**`.

## Problem and outcome

The completion interlock spans planning, worker claims, the lead loop, publication, storage, pause/recovery, owner decisions, MR rework, web and CLI. Component tests can each pass while their cross-service ordering still loses work, reuses stale evidence, opens the wrong PR or strands a run during version skew.

Add production-shaped end-to-end and recovery coverage for the integrated feature, rehearse additive rollout and rollback, enable the interlock by default for new issue runs, consolidate the decision record and mark the epic complete only when every child and worker-runnable acceptance criterion is satisfied.

## Included

- A real API plus worker isolated-compose integration harness for the #1220 reproduction.
- Cross-child order, failure, retry, race and legacy compatibility matrices.
- Worker capability/version rollout and temporary feature-flag removal/default flip.
- Full component/repository gates run once to logs.
- User/operator/CLI/architecture/ADR/spec consolidation and epic progress update.
- A maintainer-operated hosted-k8s checklist recorded separately from worker completion.

## Excluded

- New contract, permit, auditor, finding, context-generation or owner-decision semantics.
- Automatic production deployment, release tagging or merge.
- Making kube credentials available to an implementation worker.
- Workflow edits or real workflow fixtures.
- Claiming maintainer validation ran when the environment could not run it.

## Decisions

### D1: Test the externally observable lifecycle

The primary fixture reproduces run `b0ffd0ea`: five frozen milestones, only four declared, an explicit contradiction for M3, no scope reduction and unused budget. It must prove no PR call, same-session M5/M3 rework, checkpoint-before-denial and eventual exact permitted publication after correction.

The integrated suite uses real API/store transactions and the production worker boundary in an isolated compose project. Fakes may control forge/provider responses, but assertions target final state, PR body/head, stored attempts/decisions/context and resumed behavior rather than merely calls.

### D2: Exercise every cross-service recovery seam

Cover response loss and death before/after permit, PR creation, context capture, hold, decision and completion. Verify idempotency and exact-head/contract-generation fences. Cover API restart, worker replacement, stale claims, old worker, rollout flag off, unsupported codec and legacy kinds.

Mutation controls remove the structural denial, permit guard, exact-head binding, capture-before-park fence, semantic-audit requirement and blocker-disposition guard one at a time. Each mutation must fail a named observable assertion.

### D3: Keep maintainer proof outside the frozen worker contract

The worker can run component gates, LiveDB and isolated compose. Hosted-k8s worker replacement, image roll, API restart under GitOps and production capability rollout require maintainer authority. Record them as an explicit post-merge checklist in the issue/PR, not a frozen milestone or satisfied claim.

### D4: Enable by default only after compatibility proof

Rehearse API-first and worker-first skew. Gated runs must remain visibly queued until a capable worker exists; legacy contract-null runs remain operable. After the fleet capability path is proven, make all new issue runs use the structural/semantic completion profile by default and retire the temporary admin rollout flag. Rollback may stop creating new contracts but cannot make existing contracted runs claimable by old workers.

### D5: Close the epic without hiding outstanding operations

Move completed child PRDs under `prds/done/` through their own landing workflows. This child updates #1225's PRD/checklist and may move the epic record only after every child is merged. Maintainer hosted-k8s rows remain explicitly outstanding until a maintainer records their result; they do not cause the worker to lie or prevent its PR.

## Milestones and dependency plan

Five sequential milestones; all implementation-run milestones are worker-runnable.

| Phase | Milestone | Dependencies | Primary areas | Outcome |
|---|---|---|---|---|
| 1 | M1: integrated #1220 lifecycle fixture | #1226-#1231 + #1233 | e2e/API/agent harness | Missing and partial work reworks before any closing PR. |
| 2 | M2: failure, race and recovery matrix | M1 | integration/LiveDB/real Git tests | Every cross-service crash/race preserves state and resumes correctly. |
| 3 | M3: compatibility and rollout rehearsal | M1-M2 | claim/DTO/config tests, isolated compose | Old/new version skew is safe and visible. |
| 4 | M4: full gates and adversarial mutations | M1-M3 | Task gates and mutation harness | Regression channels prove the load-bearing guards. |
| 5 | M5: default enablement, docs and epic close-out | M1-M4 | config, docs/specs/ADR/PRDs | Correctness becomes default and records are synchronized. |

- [ ] **M1: integrated #1220 lifecycle fixture.** Run real API/store plus worker in an isolated project. Freeze five milestones, submit four with contradictory M3 evidence and verify the same session receives M5/M3, no forge create occurs, work is checkpointed and the run stays non-terminal. Complete the missing work and verify full exact-head audit, permit, PR head/body, context capture and atomic terminal state. Gate: relevant e2e target plus `task gate:api` and `task gate:agent`.
- [ ] **M2: failure, race and recovery matrix.** Exercise worker/API loss before/after permit, branch push, PR create/adopt, context generation, hold report, owner decision and completion response. Include stale claim, contract/head/decision races, two-worker restore, audit outage/cap and unsupported codec. Assert the observable final run/PR/context state and no only-copy cleanup. Gates: integration, LiveDB and real-Git tests.
- [ ] **M3: compatibility and rollout rehearsal.** Prove flag-off/contract-null paths are byte-compatible, old workers cannot claim contracted runs through override/kill-switch, API-first deployment waits visibly, worker-first deployment ignores unused optional fields, and rollback cannot strip an existing contract. Rehearse in isolated compose with exact container names outside `uzi-*`; never tear down the real stack. Gate: relevant API/agent/web tests.
- [ ] **M4: full gates and adversarial mutations.** Run `task gate` once to a log and inspect every named slot. Run targeted red mutations for missing-milestone denial, hard claim clause, permit guard, exact head, capture ACK, semantic audit and finding settlement, then restore and verify the tree. No workflow fixture or credential-shaped literal. Record unavailable environment-only checks rather than claiming green.
- [ ] **M5: default enablement, docs and epic close-out.** Enable the interlock by default for new issue runs and remove/retire the temporary rollout flag while retaining legacy-row behavior. Consolidate run lifecycle, pause/recovery, plan approval, MR rework and CLI docs; run `task docs:sync`; update ADR 1225, `ARCHITECTURE.md`, `specs/human.md`, `specs/ai.md`, #1214 pointers and the #1225 child checklist. Move the epic PRD only when all children are merged. Emit the maintainer hosted-k8s checklist and leave each row honestly pending until externally verified.

## Acceptance criteria

- The complete #1220 failure reproduces before guards and cannot open a closing PR after them.
- Same-session rework, owner holds/decisions, cross-worker context and semantic audit function together through real API/worker boundaries.
- Every crash/race either completes idempotently or remains recoverable/non-terminal with no work loss.
- Old workers cannot claim contracted runs; legacy runs remain unchanged.
- New issue runs use the interlock by default after rollout; existing contract authority cannot be rolled back away.
- Every load-bearing guard has a targeted red mutation and green restored proof.
- The worker completes all frozen criteria without kube credentials, workflow edits or open web.
- The epic and #1214 records accurately reflect merged dependencies and outstanding maintainer validation.

## Maintainer-operated hosted-k8s checklist

These rows are required release evidence but are not worker-frozen milestones:

- [ ] Same-worker incomplete completion reworks in the original provider session.
- [ ] Worker deletion followed by another worker restores Git and provider context.
- [ ] Worker image roll preserves contracted runs and capability matching.
- [ ] API restart between permit and completion recovers idempotently.
- [ ] Rollout with a mixed old/new worker fleet leaves gated runs visibly waiting, never misclaimed.
- [ ] A full and a scope-reduced PR render the correct closing behavior on the live forge.

Record commands, sanitized outcomes, commit/image versions and dates. If the implementation worker cannot run them, keep them unchecked and state that explicitly in the PR.

## Review record

- 2026-09-09: Split from #1225 after review found hosted-k8s proof could not be a worker completion gate and the combined PRD was too large. This child owns integration and close-out only; it introduces no new contract.
