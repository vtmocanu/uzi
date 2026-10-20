# PRD #3168: Plan cross-check coverage account

**Status**: Draft. Child 3 of 3 under umbrella #3165 (Deeper, observable cross-check). Blocked by PRD #3166, PRD #3167 M1 (replay harness) and #3097's design decision (both change the plan brief; deciding #3097 first prevents conflicting contracts).

Resolved facts below were read at `main` `bf21acdc`.

## Problem

A Plan cross-check APPROVE gates implementation of an auto-approved plan, but nothing says what the checker verified. The plan brief (`crossCheckBrief`, `agent/src/codex/cross-check.ts`) asks it to verify cited anchors and flag weak tests, scope, docs and overclaims, with no account of which acceptance criteria, anchors and tests it assessed. And when a checker cannot inspect something the plan depends on, today it either approves without saying so or asks for revisions the lead cannot satisfy: #2888's plan check spent its rounds partly on "could not verify `runner.ts` anchors: E_OVERSIZE, even on a ranged read" and ended `revisions exhausted` ([#2148 assessment](https://github.com/vtmocanu/uzi/issues/2148#issuecomment-6163933542)).

## Outcome

Each plan check accounts for the issue's acceptance criteria, the plan's cited anchors and its tests, and the verdict follows three rules:

| What the checker found | Verdict |
|---|---|
| It inspected a required item and the plan does not satisfy it | REVISE (existing bounded rounds) |
| It could not inspect evidence a required item depends on | BLOCK, naming the item and a cause category: `oversize`, `search_limit`, `external_link`, `binary`, `other` |
| Optional evidence was unavailable | Disclosed in the account; no effect on the verdict by itself |

BLOCK keeps its existing human fallback, so an unassessable required item parks the run once, with a reason a session can act on (fix the tool gap or review by hand), instead of burning revision rounds.

Acceptance examples:

1. A Claude-lead plan cites three functions in a file over the read limit. The Codex checker BLOCKs with "could not verify: `agent/src/runner.ts` anchors (oversize)"; the run parks with that reason on the run page, CLI and Slack.
2. A plan's regression test cannot fail on the unfixed code. The checker returns REVISE with that item, as today.
3. A plan links an issue comment for background only. The checker cannot read it, notes "external link not inspected" in its summary, and may still APPROVE.

## Out of scope

- #3097's A/B choice (prior-findings transport); this PRD adopts whatever contract #3097 decides.
- New verdict fields, a new gate, a new loop or a change to revision limits.
- Large-file reads (#3073) and search limits: this PRD makes them visible, it does not fix them.
- Code-stage coverage (PRD #3167).

## Modules and seams

- **Plan brief.** Adds the account and the three rules. Cause categories and the required/optional distinction are model-reported: the brief instructs, fixture tests pin the instructions, and nothing claims deterministic verification of the checker's judgment. "No defect found" is distinguished from "requirement verified" in the account.
- **Bounds.** The account lives within existing caps (summary 4 KiB, at most 20 items). When it cannot fit, the checker states the coverage as incomplete with an omitted count; an omitted **required** assessment is never treated as verified, so it cannot support APPROVE.
- **Wire and schema.** None. The plan verdict keys stay `items`, `summary`, `verdict` (strict on both families and the API): unmet items are ordinary REVISE `items`, the account and any disclosed limitation go in `summary`, and an unassessable required item is a BLOCK whose `items` carry the item and cause. Follows #3091's "no new fields" precedent.
- **Surfaces.** Today a BLOCK renders the generic reason `plan cross-check: blocked` (`agent/src/runner.ts`), and Slack shows the reason only, so an item and cause carried in `items` do not reach Slack by themselves. This PRD adds a bounded, sanitized "could not verify: <item> (<cause>)" presentation on the run page, `uzi run get` and Slack, built from existing fields where possible. The stored gate-reason enum and approval authority stay unchanged. Docs state that BLOCK may now mean "could not verify" and list the cause categories.
- **Evaluation.** Reuses PRD #3167's replay harness with a plan corpus (including the #2888 case at its pre-fix snapshot), N repeats per family, reporting approve/revise/block rates, unassessable causes, cost, latency and timeout rate before and after.

## Testing decisions

- Brief fixture tests per family: each table row, each cause category, and the optional-evidence case; negative cases where an unassessable optional item must not BLOCK.
- Presentation regression tests: a BLOCK with item and cause renders the bounded, sanitized "could not verify" text on the run page, CLI and Slack; control and bidi characters are stripped; the gate-reason enum is unchanged.
- An account that overflows its caps is reported incomplete, and an omitted required assessment does not support APPROVE (fixture test).
- Existing BLOCK human-fallback, revision accounting and exact-candidate approval tests stay green; no budget reset or new loop.
- Replay comparison as in PRD #3167, maintainer-run.

## Milestones

- [ ] **M1: Plan checks account for coverage and park on unverifiable required evidence.** Brief, fixtures, docs (`docs/cross-check.md`, then `task docs:sync`), dated ADR-2149 amendment, changelog, then a maintainer replay before and after. Blocked by: PRD #3166, PRD #3167 M1, #3097 decision. Acceptance: examples 1-3; fixture tests; replay report posted with per-family block causes; gates `task gate:agent`, `task gate:api`.

## Risks

- More parks while tool gaps remain (oversize files before #3073, Codex search limits). Intended: each park names a fixable cause. Report park counts by cause after rollout.
- The model may misclassify required versus optional; fixtures pin instructions only, so the replay report must show misclassifications found.

## Decision Log

- **BLOCK, not REVISE, for unassessable required evidence** (user decision 2026-10-20). The lead cannot fix a checker tool gap by revising; REVISE only burns rounds (#2888). BLOCK parks once with an actionable cause.
- **Disclose, not block, for optional evidence.** Blocking on background links would park routinely without improving safety.
- **No new fields.** The account fits the existing `items` and `summary`; smallest change, consistent with #3091.
- **Wait for #3097.** Both change the same brief; deciding it first avoids two conflicting disposition contracts.
