# PRD #1227: Owner completion decisions and scope-reduced delivery

**Issue:** [#1227](https://github.com/vtmocanu/uzi/issues/1227)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Planned, blocked on #1226.
**Priority:** High.
**Execution:** Keep `Planned` without `uzi`. Add `uzi` only after #1226 merges and main CI is green.

This child extends #1226's single completion-decision path. It does not add semantic auditing or durable cross-worker context. No implementation or validation may modify `.github/workflows/**`.

## Problem and outcome

#1226 can automatically rework missing milestones and let an owner continue with guidance, but a genuinely changed priority still has no truthful bounded resolution. The lead must not waive its own work, and count-based `scope_ceiling` cannot represent an arbitrary set of milestones selected at a completion hold.

Let the owner/admin explicitly reduce the contract scope by stable IDs or accept named unmet criteria with a required reason. Every decision creates a new contract revision and invalidates prior permits. Reduced-scope PRs are visibly partial and never close the issue; accepted criteria are named in a warning block before a closing PR is allowed.

## Included

- Extend `completion_decision` with `partial` and `accept` values.
- Exact ID validation, owner/admin authorization and required reasons.
- Contract revision and permit invalidation.
- A distinct `scope_reduced` terminal disposition.
- Partial/accepted PR titles and bodies.
- Web and CLI decision controls and audit history.

## Excluded

- Semantic criteria beyond #1226's structural milestone IDs; #1230/#1231 extend the same model.
- Lead-authored waiver or reviewer self-dismissal.
- `scope_ceiling` reinterpretation.
- Provider context export, audit/finding systems, mr_rework gating or hosted-k8s validation.

## Decisions

### D1: One owner-only decision channel

Reuse #1226's endpoint and `run_user_inputs.kind=completion_decision`:

```text
{decision:"continue", guidance?}
{decision:"partial", keep:[milestone_ids], reason}
{decision:"accept", criteria:[criterion_ids], reason}
```

Reject unknown IDs, duplicates, empty keep sets, criteria already satisfied/out of scope, empty reasons and non-owner/admin callers. Treat repeated identical submissions idempotently; a conflicting second decision receives a typed conflict rather than last-write-wins.

### D2: Partial is a real contract revision

`scope_ceiling` remains the existing loop-top count directive. `partial` creates revision N+1 with exact `scope.in` and `scope.out` sets and records removed IDs as owner-deferred. Add `stop_kind=scope_reduced`; do not widen the meaning of `scope_capped`.

The next completion attempt structurally checks the new in-scope set and needs a new permit. Its PR title/body state partial delivery, list deferred IDs and omit every closing keyword.

### D3: Acceptance is exact and visible

`accept` records only named criterion IDs and the owner's reason. It does not mark unrelated requirements complete. The next permit request treats those exact IDs as owner-accepted under the new revision. A closing PR includes a warning block with IDs, criterion text and reason.

The lead has no route/tool for either decision and receives only the settled outcome in its follow-up prompt.

## Milestones and dependency plan

Five sequential milestones; the existing DTO/endpoint from #1226 is the integration seam.

| Phase | Milestone | Dependencies | Primary areas | Outcome |
|---|---|---|---|---|
| 1 | M1: decision schema and revision transaction | #1226 | migration, completion service/store | Decisions are valid, idempotent and invalidate permits atomically. |
| 2 | M2: partial scope behavior | M1 | server contract policy, agent result/PR body | Exact reduced scope produces partial delivery with no close. |
| 3 | M3: exact accept behavior | M1 | permit policy, PR body/audit summaries | Only named accepted criteria count and reasons are visible. |
| 4 (parallel after M1) | M4: web controls | M1 DTOs | run view/components/tests | Owner can inspect and confirm partial/accept decisions. |
| 5 | M5: CLI, docs and adversarial tests | M1-M3 wire contract | `api/cmd/uzi/`, docs/specs/tests | CLI parity and mutation-proof behavior are complete. |

- [ ] **M1: decision schema and revision transaction.** Widen the decision domain and add `scope_reduced`; validate owner/admin authority, exact IDs and reasons. In one transaction record the input, create contract revision N+1, invalidate every prior permit and return the new bounded contract summary. Test duplicates, conflicts, races, non-owner hiding and stale revisions with LiveDB. Gate: `task gate:api`, `task scan:secrets`.
- [ ] **M2: partial scope behavior.** Add exact `scope.in/out` policy and agent settlement. The subsequent attempt checks only in-scope milestones but retains the full history. Render `[partial]`, list deferred IDs/reason and omit `Closes`/equivalent closing syntax. Mutation-test that restoring unconditional closing language fails a byte-level body assertion. Gates: `task gate:agent`, `task gate:api`.
- [ ] **M3: exact accept behavior.** Add owner-accepted criterion identities to the permit computation. Require a reason and render the exact IDs/text/reason in a warning block. Prove accepting one criterion cannot accept siblings, a head/revision change invalidates the permit, and the lead cannot call the owner endpoint. Gates: `task gate:api`, `task gate:agent`.
- [ ] **M4: web controls.** Extend Completion blocked UI with exact partial selection, accept selection, required reason, confirmation and accessible errors. Show the resulting revision and immutable decision history. Cover stale form/conflict, unknown criterion, non-owner, partial and accepted PR previews using real DTO states. Gate: `task gate:web`.
- [ ] **M5: CLI, docs and adversarial tests.** Add `uzi run decide <id> --partial <ids> --reason <text>` and `--accept <ids> --reason <text>` with stable JSON and terminal-safe rendering. Update CLI/run-hold docs and technical specs; run `task docs:sync`. Regression matrix: no reason/unknown ID/non-owner rejected; partial no-close; accept warning; concurrent decision/permit; legacy PR body byte-identical. Run relevant component gates once to logs.

## Acceptance criteria

- Only owner/admin callers can reduce or accept completion scope.
- Every decision is exact, reasoned, durable and revisioned.
- Every prior permit is invalid after a decision.
- Partial delivery uses `scope_reduced`, lists deferred IDs and never closes the issue.
- Accepting one criterion cannot affect another; the closing PR exposes the reason.
- The lead cannot access the decision authority.
- Existing `scope_ceiling`, `scope_capped` and legacy PR behavior remain unchanged.
- Web and CLI expose the same decision semantics.

## Review record

- 2026-09-09: Split from epic #1225 so owner policy does not enlarge the first structural containment run. Reviewed boundaries with `@help1` and `@brainstorm`.
