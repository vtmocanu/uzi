---
name: uzi-crosscheck-eval
description: Evaluates uzi plan and code cross-check through real runs and independent review. Use when the user asks to evaluate, dogfood or assess cross-check quality, including caught defects, missed defects, noise and tool limits.
---

## Use

Evaluate actual cross-check behavior without prescribing improvements. Compose
[uzi-watcher](../uzi-watcher/SKILL.md) for dispatch, steering and recovery, and
[uzi-lander](../uzi-lander/SKILL.md) for independent review and landing. Use
uzi-cli for current commands and [cross-check docs](../../../docs/cross-check.md)
for eligibility and telemetry.

## Select

- Freshness-check the issue against current main with the buddy. Choose a sample
  that can exercise the intended stages; identify deliberate code-only samples.
- Inspect `uzi settings get --json`, effective stage settings, credentials,
  worker capabilities and supported lead/checker families. Verify the checker
  is the opposite family. Follow the user's harness choice and repo rules.
- Verify auto-approval eligibility before expecting a plan check. An external
  author can cause `AUTO_APPROVE_BLOCKED` / `author_not_eligible`; inspect actual
  run state and current eligibility rules rather than assume either stage ran.
- Follow watcher preflight and uzi-cli's current dispatch procedure. For an
  authorized auto-approved sample, use a disabled one-shot schedule with
  auto-approval, run it now, then delete the schedule. Verify the resulting run's
  effective settings. Bound retries to verified label-sync refusals; do not
  retry other eligibility failures blindly.

## Observe independently

- Record run and checker-child IDs, checked plan revision or commit SHA,
  lead/checker model and effort, rounds and owner interventions.
- At the plan stage, lead and buddy independently record their own findings
  before reading checker conclusions. Then inspect every plan-stage
  `cross_check` round: candidate, verdict, findings, revisions and dispositions.
- At the code stage, lead and buddy independently record findings on the exact
  checked diff before reading checker conclusions. Inspect `CODE_CHECK_*` summaries and the underlying
  checker result; compare bot reviews separately. Track subsequent head changes.
- For both stages, read the child checker's `check_activity` records and
  summaries, including read/search `ok`, `error`, `limit` and `denied` outcomes.
  Preserve bounded evidence references; use synthetic fixtures for probes and
  redact private data before public reporting.

## Compare

- Compare our findings with the checker's: caught, missed, noise and tool limits.
  Verify claimed defects against the checked revision. Record useful catches
  and clean results as well as failures; distinguish checker-driven changes
  from owner steering.
- Separate incomplete reviews from missed defects. A malformed/incomplete result
  supplies no valid verdict; failed or limited searches do not prove absence.
  Check documented stage behavior: code review is advisory and publication may
  proceed without a valid verdict. Do not infer a root cause from status alone.
- Act on verified findings: steer with `uzi run follow-up`; if the plan is
  fundamentally wrong, cancel and re-run gated through uzi-watcher. Record the
  intervention. Delegate owner-held parks, stuck recovery and API outages to
  watcher/lander; preserve their backup and fail-closed checks.

## Report

Write one assessment per run with scope, checked revisions, outcome, caught /
missed / noise / limits, evidence IDs and remaining uncertainty. Batch reporting
after the sample runs end. Report observations generically for every repo uzi
serves, using this repo's details only as evidence. Leave improvement choices to
the evaluating sessions; do not prescribe fixes.

Before posting, check closed issues/PRs, git history, ADR-2149 and the Decision
Logs in PRDs #2149 and #3168. Cite prior decisions; revisit them only with new
evidence and the user's acknowledgment. Verify the current cross-check umbrella
(starting points: #3165 and the original family-review scope #2148). Consult the
current cross-check domain owner when one exists, fold that review and obtain
buddy approval. Post batched comments on the verified umbrella or file labeled
issues through issue-triage; follow its review and architecture-review rules.
