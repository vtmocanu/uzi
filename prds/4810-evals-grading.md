# PRD #4810: Evals grading, a blind lead and cross reviewer panel

**Status**: Draft  
**Priority**: Normal  
**Umbrella**: #4808  
**Blocked by**: #4809

## Problem

After #4809 an eval reports time, cost and whether each trial finished, but not whether the work is any good. A single model reviewer is a noisy and biased scorer: it favours its own model family, longer and more stylish diffs, and whichever answer it reads first, and its scores move with prompt wording. uzi already runs an opposite-family reviewer for live runs (plan and code cross-check, ADR-2149), but that child attaches only to a live parent under active custody (the creation guard in `api/internal/workersvc/code_cross_check.go`), can trigger a repair turn, and returns a verdict with findings rather than scores.

## Outcome

Every frozen trial of an eval is graded after it ends by a **lead reviewer** the owner configures (harness, optional custom provider, model, effort) and, when the owner enables it, an opposite-family **cross reviewer**. Both see the same frozen task, the same versioned rubric and the trial's anonymous diff applied to the pinned base, never which harness, model or provider wrote it. Each returns per-criterion scores and a verdict; neither sees the other's result. After all trials are graded, the lead reviewer (and the cross reviewer, when enabled) compares candidate pairs side by side in both orders. The report shows both reviewers' scores, a disagreement flag, per-candidate pass counts and whether every trial passed, the pairwise preference tally, and reviewer spend separately from trial spend.

Acceptance examples:

1. An eval with candidates `a` (Claude) and `b` (Codex), lead reviewer Claude and cross reviewer enabled: each of its 4 trials gets two `eval_grade` runs; `uzi eval report` shows, per trial, lead and cross scores for each rubric criterion, both verdicts, and `disagree` where the verdicts differ or a criterion differs by 2 or more points.
2. A grader's input, captured by the agent test harness, contains the frozen issue text, the rubric and the diff, and contains none of: the candidate label, harness, model, provider, branch name, commit messages or cost. A trial whose diff exceeds the grader's input budget is graded `unknown` with reason `diff_exceeds_budget`, never scored from a truncated diff.
3. With 2 trials per candidate, the preference step runs `a1` against `b1` and `a2` against `b2`, each in both orders; a pair that flips with order is recorded `inconsistent`, not as a win.

## Out of scope

- Hidden tests (#4812) join the pass rule in their own PRD; CI evidence (#4811) is shown beside the grades and never changes the pass classification.
- Statistics, intervals and composite scores: #4813.
- Calibrating reviewers against human-scored trials: not planned in this effort.
- Changing live-run cross-check behaviour: untouched.

## Modules and seams

1. **Grading config on the eval**: lead reviewer fields plus `cross_reviewer` (`off` default, or a model on the opposite harness resolved by `oppositeHarness` in `api/internal/workersvc/cross_check.go`). Frozen at eval create with the candidates; the same pair grades every trial (I3). A reviewer that resolves to the same model as a candidate is allowed but flagged `reviewer_shares_model` on the report. The comparison reuses the provider identity comparison (`providers.IdentitySide`) against each trial's recorded lead config, not the requested model; it never refuses, unlike the cross-check guard `checkSameModel`. Custom providers are accepted for either engine through the existing provider checker capabilities (`claude_provider_judge_v1` for Claude, `codex_provider_checker_v1` for Codex, `docs/configuration.md` "Custom model providers").
2. **Run kind `eval_grade`**: report-only, repo-ful, issue-less; its claim carries no `forge_pat` and #4809's `refuseEvalTrialRuns` routes refuse it too. Predicates stated: `Supporting` true, `Listed` false, `PlanningCapable` false, `WallTimed` false (its own bounded deadline, like cross-check), `JudgeEligible`/`CodeCrossCheckable`/`PlanCrossCheckable` false. Created by evalsvc only after the trial is frozen (I2). Modes: `single` (one trial, one reviewer) and `pairwise` (two trials, one reviewer, one order). Worker capability `eval_grade_v1`, required at claim, placement, health reason ("no worker that supports eval grading is online"), hibernation wake and ephemeral provisioning, plus the provider checker capability for a provider reviewer. Grade runs take an ordinary run slot, like the judge, not the cross-check slot pool; grading trial n may overlap trial n+1.
3. **Grader worker seam**: a fresh detached exact-base clone (`runnerCloneAtCommit` in `agent/src/git.ts`), the frozen patch applied, and the result verified against #4809's `tree_sha` before either mode runs; an apply failure or tree mismatch is `ungradable: artifact_mismatch`. The reviewer gets a tree-only export of that result with no `.git` directory, and the grade path guard also denies any `.git` path, so history, refs and other trials on a shared bare are unreachable (the checker tools have no `.git` exclusion today). Trial branch names carry only the run id. Residual, recorded: code style can still fingerprint a model. The reviewer reads the result through the existing read-only checker tools: Claude through `agent/src/claude-cross-check.ts` (Read, Grep, Glob, path guards, runner-UID isolation), Codex through `agent/src/codex/cross-check.ts` (Read, bounded Search, sandboxed fileop). The transport, call ceiling and confinement are reused; the prompt, output schema and validator are new (the adapters' validators expect cross-check verdicts). Pairwise mode uses the tool-free pass (`runReadOnlyModelPass`, `agent/src/model-pass.ts`) over the two diffs labelled `X` and `Y`.
4. **Result contract** (`eval_gradings`, `eval_preferences`): `rubric_version`; per criterion (`solves_issue`, `acceptance_criteria`, `correctness`, `tests`, `scope_discipline`, `maintainability`; higher is better for every criterion, so `correctness` 5 means low risk) an integer 1 to 5 or `null` for unknown, plus one evidence line of at most 300 characters; `verdict` `pass|fail|unknown`; at most 20 findings of at most 500 characters each; usage with `cost_status`. Preferences: `X|Y|tie|unknown` per order, folded to `a|b|tie|inconsistent|unknown`. Pairs are matched by ordinal on purpose (trials are independent samples, so any fixed pairing is fair and ordinal is reproducible); a pair where one side failed, is missing or is ungraded is not compared and is reported as `one_sided` with the reason, and the tally shows compared pairs out of requested pairs. Results arrive on `POST /api/worker/runs/{id}/eval-grade-result`: generation-fenced, body capped at 64 KiB, one result per grade run. The api validates the JSON shape and bounds and stores a malformed result as `unknown: malformed_result`; it never retries into a different reviewer.
5. **Input budget**: a stated byte limit for the whole grader input (frozen task, rubric, prompt wrappers, and one diff in `single` mode or both diffs in `pairwise` mode), the code cross-check context bound (2 x 262144 bytes, `code_cross_check_candidate.go`). The api computes it before creating a grade run, so an oversized input becomes `unknown: diff_exceeds_budget` without spending a run, never truncated and never retried on a different reviewer.
6. **Pass rule**: a trial passes when it completed, the lead verdict is `pass`, and the cross verdict is `pass` or the cross reviewer is off. `unknown` from any configured reviewer makes the trial `ungraded`, not failed. Per candidate the denominator is every requested trial except `infra_failure` trials (#4809) and ungraded trials, both listed with their counts: a candidate-caused failure (the trial failed, timed out, ended `needs_human`, or produced no diff) stays in the denominator as a non-pass. The report shows passes out of that denominator, the ungraded count, and `all trials passed`: `no` if any evaluable trial failed, `unavailable` if any non-infrastructure trial is ungraded or no evaluable trials remain, and `yes` otherwise.
7. **Consent and spend** (I6): eval confirm lists the maximum grading runs (trials times reviewers, plus pairwise runs) and the reviewer endpoints that will receive the diffs, and requires the owner's confirmation. Reviewer spend counts towards the eval's spend threshold and is reported on its own line. Grade admission checks the threshold too; a trial left ungraded by it is `ungraded: spend_threshold`. Bounds: at most 12 `single` and 12 `pairwise` grade runs per eval; grade runs admit one at a time per eval.
8. **CLI**: `uzi eval create --lead-reviewer <harness:model> [--cross-reviewer <model>|--no-cross-reviewer]`; `report` gains the scores, flags and preferences. `docs/evals.md` grading section, `task docs:sync`.

## Testing decisions

- Blinding: an agent test captures the exact grader input for both harnesses and asserts the forbidden fields are absent (example 2); a workersvc test asserts the claim payload for `eval_grade` carries no candidate fields.
- No feedback (I2): a test that a grade run cannot be created for a non-terminal trial and that no grade outcome changes trial state.
- Result contract: api table tests for bounds, malformed JSON, `null` criteria, and the pass rule.
- Pairwise folding: unit tests for every order combination.
- Capability gating: claim tests that a worker without `eval_grade_v1` never claims.
- Follow the cross-check checker tests (`agent/test/` cross-check suites) for confinement; confinement probes are owed on a capable host, not run in-run.

## Milestones

### M1: A lead reviewer grades every trial
**Blocked by**: none
Grading config; `eval_grade` kind (single mode) across api, fixtures, agent and web mirrors; capability; grader seam with patch apply; result contract and pass rule; report and CLI; consent listing; docs, changelog fragment.
Acceptance: example 2; a lead-only eval reports scores and pass counts; malformed and oversized results are `unknown`.

### M2: Optional cross reviewer and disagreement
**Blocked by**: M1
Cross reviewer resolution on the opposite harness; sealed independent results; disagreement flag; `reviewer_shares_model` flag; custom-provider reviewers on both engines (positive coverage), and fail-closed coverage when the required capability or provider binding is unavailable: the grade settles `unknown: reviewer_unavailable` and the diff is never sent to a fallback endpoint.
Acceptance: example 1.

### M3: Pairwise preferences in both orders
**Blocked by**: M2
Pairwise mode on the tool-free pass; order swap; folding; report tally.
Acceptance: example 3.

### Landing obligations
- `full agent suite: exact-head PR CI`.
- Full `test:api`, `test:web`, `gate:repo` pending exact-head PR CI; confinement probes on a capable host.

## Decision Log

- **New `eval_grade` kind, reusing the checker adapters.** The cross_check child requires live parent custody and can hand the lead a repair turn; both break I2. Rejected: extending `cross_check` (would weaken its custody invariants) and the judge (it reviews a trace, sees run identity and cost).
- **Grade the code, not only the diff.** The read-only checker tools let a reviewer read surrounding code, which a diff-only prompt cannot. Pairwise comparison stays tool-free to keep it cheap and symmetric.
- **Rubric scores plus a verdict.** Scores give the per-criterion view; the verdict gives a pass count. Rejected: a single 0 to 100 score (hides why).
- **Unknown is allowed and never counted as fail.** A forced guess on a truncated or unreadable diff is worse than a gap.
- **Same-model reviewers are flagged, not refused.** The owner may deliberately grade with the strongest model even when it is a candidate; the flag keeps the bias visible.
- **Disagreement is shown, not averaged.** Two reviewers agreeing is consistency, not correctness.
