# PRD #3167: Code cross-check coverage account and measured depth

**Status**: Draft. Child 2 of 3 under umbrella #3165 (Deeper, observable cross-check). Blocked by PRD #3166 (M2 and M3 only; M1 has no blocker; M2 also waits for M1).

Resolved facts below were read at `main` `bf21acdc`.

## Problem

Code cross-check (PRD #2170) gives the lead a cross-family second opinion before publication, and its findings are fixed in the lead's single fix pass. But a clean result says nothing about what was reviewed:

- The brief (`codeCrossCheckBrief`, `agent/src/codex/cross-check.ts`) is a short fixed checklist over a supplied diff, with optional reads. A valid `{"findings":[]}` ends the check.
- Nothing asks the checker to account for which changed paths it assessed. When the diff exceeds 1 MiB it is replaced by a notice, and reading the checked head cannot discover deleted files.
- The brief still carries the blanket "inline environment variables in gate commands" objection that #3091 replaced in the plan brief with effect-based wording (ADR-2149 records the code-stage change as deferred).
- After clean code checks, later implementation review still found defects (#2148 [three-run assessment](https://github.com/vtmocanu/uzi/issues/2148#issuecomment-6163933542)); one sampled check did catch a real request-type regression (`TS_GROUP_IDS_OPTIONAL`, PR #3063). Detection rate is unmeasured.
- Families differ in bounded tool behaviour: Codex search refuses a whole query on any file over 64 KiB, more than 256 entries, 32 directories or 64 operations (`agent/src/codex/broker.ts`); Claude's Grep has different bounds. Large-file reads are #3073.

## Outcome

Every code check reports, next to its findings, an account of every changed path in the checked range: examined, excluded with a reason, or unassessed. The merge request shows the counts and the unassessed paths. A revised brief asks for correctness against the issue and plan, contracts and safety invariants, and whether each new test can fail on the unfixed code, with #3091's effect-based environment wording. A maintainer-run replay harness measures detection, false positives, coverage, cost, latency and timeout rate before and after, per checker family.

Acceptance examples:

1. A check of a 12-path change (one deleted, one renamed) returns two findings and judgments for nine paths. The MR section reads "Coverage: 9 examined, 0 excluded, 3 unassessed" and lists the three unassessed paths, including the deleted one.
2. A worker on the previous version checks a run against an upgraded API. The MR section shows findings as today and "coverage not reported".
3. The changed-path inventory exceeds its cap. The account says "inventory incomplete (N paths omitted)" and the MR never claims full coverage.
4. On the frozen corpus, the M3 report lists, per family, defects detected and missed, false positives, coverage, cost, latency and timeout/incomplete rate for the old and the new brief.

## Out of scope

- A blocking code stage, extra code rounds or a second check of the fixed head (PRD #2170 D4 stays).
- Any command execution by the checker, including tests and gates.
- Minimum duration, turn or read quotas, or a blanket effort increase.
- Feeding gate results to the checker: no uniform, commit-bound gate record exists, and a worker-observed exit code does not prove required validation passed. Revisit only with a separately specified safe projection.
- Plan-stage coverage (PRD #3168); large-file reads (#3073); Codex search limits.

## Modules and seams

- **Changed-path inventory (worker).** Computed by the worker from the checked `base...head`, independent of the diff text: added, modified, deleted and renamed paths. Bounded by a path-count cap and per-path byte cap; overflow sets an `inventory_incomplete` flag with an omitted count. The model never authors the denominator.
- **Coverage judgments (model).** The checker returns, in a separate verdict field beside `findings`, a judgment only for paths it addresses: `examined` (what it inspected), `excluded` (reason), `unassessed`. Any inventory path without a judgment is `unassessed`. Judgments for paths not in the inventory are dropped and counted. The worker, not the model, marks which examined paths have recorded reads from PRD #3166. Coverage is a review account, never a certification of correctness.
- **Wire, schema and rollout.** Findings cannot carry coverage: they are capped at 20 entries, 2 KiB each, 32 KiB total, and dispositions are tied to the findings array. Coverage gets its own caps (entries, per-reason bytes, total) and a new nullable `cross_checks` column, written once with the verdict and covered by the existing immutability triggers; additive migration with a draft number, renumbered at merge. The verdict route rejects unknown fields (`DisallowUnknownFields`, `api/internal/handler/cross_check_worker.go`), so the API advertises support with an additive claim field and the worker sends coverage only when it sees that field (the ADR-2149 #2460 pattern). Rows without coverage render "coverage not reported". Valid findings are kept whenever coverage is missing, partial or over its caps; overflow sets an incomplete status with omission counts.
- **MR section** (`agent/src/pr-description.ts`): counts plus at most K unassessed paths through the existing sanitizers; the run page and `uzi run get` show the full stored account through the hardened renderer and `renderer.Plain`.
- **Revised brief.** Correctness against issue and plan, contracts and safety invariants, test validity, scope, docs and changelog, and #3091's effect-based environment wording replacing the blanket objection. Prompt changes are instructions; fixture tests pin the instructions, not model behaviour.
- **Replay harness (maintainer-run).** No replay entry point exists today: a checker runs only from a server claim (`agent/src/cross-check-runner.ts`). The harness builds a claim from frozen inputs and invokes the existing Codex and Claude checker classes with the maintainer's credentials. It is never run by a uzi run, and real-model replays need the maintainer's authorization. Pins and records the checker version, brief, model, effort and reader implementation for each run.
- **Corpus.** Past changes with independently verified defects plus clean controls, at exact pre-fix snapshots with only checker-visible inputs. Each defect is classified as detectable by a read-only reviewer or not (an execution-only defect such as a platform-specific test calibration is excluded from detection scoring). Candidates: the three implementation-review fixes in the #2148 assessment and the PR #3063 finding. Frozen before the brief changes; N repeats per case per family. Public results are sanitized aggregates only.

## Testing decisions

- Worker inventory: added, modified, deleted, renamed, binary and overflow cases from real git fixtures.
- Coverage validation: unknown paths dropped and counted, missing judgments become unassessed, caps produce incomplete status, findings retained in every case.
- Rollout both orders: new worker and old API (no coverage sent), old worker and new API (coverage not reported).
- LiveDB: column write-once with the verdict and immutability triggers, via `./e2e/run-store-it.sh`.
- MR section and CLI render tests for complete, partial, incomplete and not-reported coverage, following `agent/src/pr-description.ts` tests.
- Brief fixture tests: the effect-based environment cases from #3091 (scratch `TMPDIR` relocation accepted, validation-narrowing override flagged).

## Milestones

- [ ] **M1: Baseline today's code checker on a frozen corpus.** Replay harness, corpus with detectability classification, scoring, and a recorded baseline per family (detection, false positives, cost, latency, timeout/incomplete rate), run by the maintainer before any brief change. Blocked by: none. Acceptance: harness tests pass offline with a fake checker; the corpus and pinned versions are committed; the baseline aggregate is posted on this issue.
- [ ] **M2: Code checks report coverage and the MR shows it.** Inventory, coverage field, column, claim capability, MR section, run page, CLI, docs, changelog. Brief asks for the account but is otherwise unchanged. Blocked by: M1 (the baseline must be frozen and measured before any brief change), PRD #3166. Acceptance: examples 1-3; rollout-order and LiveDB tests; gates `task gate:agent`, `task gate:api`, `task gate:web`.
- [ ] **M3: Revised brief, measured against the baseline.** New brief with effect-based environment wording, dated ADR-2149 amendment and a note on PRD #2170's brief text, then a maintainer replay on the M1 corpus and hosted verification. Blocked by: M1, M2. Acceptance: example 4 posted; any detection gain is reported with its false-positive, cost, latency and timeout change; unmeasured items named.

## Risks

- A deeper brief raises latency and the timeout/incomplete rate against the `min(15 min, deadline)` model timeout; M1 baselines it and M3 reports it.
- Family asymmetry in search and read bounds skews comparisons; report per family.
- Model nondeterminism; mitigated by repeats, never by a single run.

## Decision Log

- **Worker owns the denominator.** A model listing its own coverage audits itself; the worker's git inventory is independent and catches deleted files.
- **Separate field and column, not findings.** Findings caps and the dispositions constraint cannot hold coverage without breaking existing rows.
- **Capability-gated wire change.** The strict verdict decoder would refuse a new field from a newer worker.
- **Baseline before brief change.** Otherwise improvement cannot be measured.
- **Gate evidence parked.** No safe, commit-bound gate record exists today.
