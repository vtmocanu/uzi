# PRD #3167: Code cross-check coverage account and measured depth

**Status**: Draft. Child 2 of 3 under umbrella #3165 (Deeper, observable cross-check). PRD #3166 is done (2026-10-21). M1 has no blocker; M2 waits for M1; M3a waits for M1 and M2; M3b waits for M3a. M2 and M3a rebase onto PRD #3168 M1, which lands first (same file). Amended 2026-10-23 with acceptance-criteria findings (see Decision Log).

Resolved facts below were read at `main` `bf21acdc`; facts added 2026-10-23 were read at `72c97fc7`.

## Problem

Code cross-check (PRD #2170) gives the lead a cross-family second opinion before publication, and its findings are fixed in the lead's single fix pass. But a clean result says nothing about what was reviewed:

- The brief (`codeCrossCheckBrief`, `agent/src/codex/cross-check.ts`) is a short fixed checklist over a supplied diff, with optional reads. A valid `{"findings":[]}` ends the check.
- Nothing asks the checker to account for which changed paths it assessed. When the diff exceeds 1 MiB it is replaced by a notice, and reading the checked head cannot discover deleted files.
- The brief still carries the blanket "inline environment variables in gate commands" objection that #3091 replaced in the plan brief with effect-based wording (ADR-2149 records the code-stage change as deferred).
- After clean code checks, later implementation review still found defects (#2148 [three-run assessment](https://github.com/vtmocanu/uzi/issues/2148#issuecomment-6163933542)); one sampled check did catch a real request-type regression (`TS_GROUP_IDS_OPTIONAL`, PR #3063). Detection rate is unmeasured.
- Nothing asks whether the issue's acceptance criteria and the approved plan were delivered, and the checker cannot see the approved milestones: the code branch of `crossCheckPrompt` fences only `issue_title`, `issue_body`, `approved_plan` (`plan_md`), `code_context` and `committed_diff`, although the claim already carries them (`lead.MilestonesFrozen`, `api/internal/workersvc/code_cross_check.go`). #1231 (blocking completion auditor) closed not planned because its token caps cannot be enforced on subscription credentials; its auditing value moves here and into PRD #3168, as advice only.
- Families differ in bounded tool behaviour: Codex search refuses a whole query on any file over 64 KiB, more than 256 entries, 32 directories or 64 operations (`agent/src/codex/broker.ts`); Claude's Grep has different bounds. Large-file reads are #3073.

## Outcome

Every code check reports, next to its findings, an account of every changed path in the checked range: examined, excluded with a reason, or unassessed. The merge request shows the counts and the unassessed paths. A revised brief asks for correctness against the issue and plan, contracts and safety invariants, whether each new test can fail on the unfixed code, and which acceptance criteria from the issue and approved plan are undelivered or partly delivered, with #3091's effect-based environment wording. Criterion findings are advisory, like every code finding: code cross-check never blocks. A maintainer-run replay harness measures detection, false positives, coverage, cost, latency and timeout rate before and after, per checker family.

Acceptance examples:

1. A check of a 12-path change (one deleted, one renamed) returns two findings and judgments for nine paths. The MR section reads "Coverage: 9 examined, 0 excluded, 3 unassessed" and lists the three unassessed paths, including the deleted one.
2. A worker on the previous version checks a run against an upgraded API. The MR section shows findings as today and "coverage not reported".
3. The changed-path inventory exceeds its cap. The account says "inventory incomplete (N paths omitted)" and the MR never claims full coverage.
4. The approved plan has five acceptance criteria; the change delivers four. The checker returns a finding naming the undelivered criterion, `path` set to the most relevant file (or the PRD or plan source) and `line` 0 when none applies. The lead addresses or declines it in its single fix pass; publication is not blocked.
5. On the frozen corpus, the M3b report lists, per family, defects and undelivered criteria detected and missed, false positives, coverage, cost, latency and timeout/incomplete rate for the old and the new brief.

## Out of scope

- A blocking code stage, extra code rounds or a second check of the fixed head (PRD #2170 D4 stays); a completion permit or exact-final-head audit (#1231).
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
- **Acceptance-criteria findings (M3a brief).** Findings for acceptance criteria from the issue and the approved plan and milestones that the change does not deliver or delivers only partly. `path` names the most relevant file (or the PRD or plan source), `line` is 0 when none applies (both decoders already accept 0). Preserve defect findings first up to the existing cap. Append criterion findings and an omission note (one `minor` stating how many criteria were not reported) only when space remains; never evict a defect to insert the advisory omission note. A full defect report can leave no room for criterion notes. Marker, as in PRD #3168: every criterion finding, informational ones included, starts its `title` with a fixed prefix, `Requirement gap:` for an undelivered or partly delivered criterion and `Coverage uncertainty:` for verification that cannot be done here; defect findings use neither. Severity alone cannot distinguish a defect from coverage, so the brief uses this convention for defect-first ordering. It adds no field and does not authorize rewriting findings programmatically. Genuine hosted, live or maintainer-only verification is a `minor` `Coverage uncertainty:` finding; missing worker-deliverable implementation or regression tests stay criterion findings graded by impact. No new field: criterion findings are ordinary findings with today's dispositions. Safety carve-out, as in PRD #3168: an undelivered criterion the issue states as a safety, security or data-integrity constraint is a defect finding (`critical` or `major`, no coverage prefix); it stays advisory, since code cross-check never blocks.
- **Approved milestones in the checker prompt (M3a, worker only).** The code branch of `crossCheckPrompt` adds a nonce-fenced `approved_milestones` DATA excerpt: the frozen milestone list from the claim, serialized whole and bounded at 256 KiB (the server's cap in `codeCandidateDigest`, `api/internal/workersvc/code_cross_check_candidate.go`); the worker rechecks that bound like the branch's other caps. An absent list renders as `[]`. No API, claim or digest change: milestones already ride the claim and the candidate digest.
- **Replay harness (maintainer-run).** No replay entry point exists today: a checker runs only from a server claim (`agent/src/cross-check-runner.ts`). The harness builds a claim from frozen inputs and invokes the existing Codex and Claude checker classes with the maintainer's credentials. It is never run by a uzi run, and real-model replays need the maintainer's authorization. Pins and records the checker version, brief, model, effort and reader implementation for each run.
- **Corpus.** Past changes with independently verified defects plus clean controls, at exact pre-fix snapshots with only checker-visible inputs. Each defect is classified as detectable by a read-only reviewer or not (an execution-only defect such as a platform-specific test calibration is excluded from detection scoring). Candidates: the three implementation-review fixes in the #2148 assessment and the PR #3063 finding. Frozen before the brief changes; N repeats per case per family. Public results are sanitized aggregates only.

## Testing decisions

- Worker inventory: added, modified, deleted, renamed, binary and overflow cases from real git fixtures.
- Coverage validation: unknown paths dropped and counted, missing judgments become unassessed, caps produce incomplete status, findings retained in every case.
- Rollout both orders: new worker and old API (no coverage sent), old worker and new API (coverage not reported).
- LiveDB: column write-once with the verdict and immutability triggers, via `./e2e/run-store-it.sh`.
- MR section and CLI render tests for complete, partial, incomplete and not-reported coverage, following `agent/src/pr-description.ts` tests.
- Brief fixture tests: the effect-based environment cases from #3091 (scratch `TMPDIR` relocation accepted, validation-narrowing override flagged).
- Code-brief instruction-contract tests, the counterpart of `agent/test/plan-cross-check-guidance.test.ts` with their own rule regexes and an exact case set: criteria from issue and plan, `path`/`line` 0 rule, the two prefixes (required for criterion findings, forbidden for defects), defects first with no eviction, the omission note only when space remains, hosted-only verification as `minor`, missing tests still a finding, and no blocking. They pin instructions, not model behaviour.
- `crossCheckPrompt` code branch: `approved_milestones` is nonce-fenced DATA outside the trusted brief, carries the claim's list (including `[]`), and an over-cap list is refused like the other code inputs.

## Milestones

- [ ] **M1: Baseline today's code checker on a frozen corpus.** Replay harness, corpus with detectability classification, scoring, and a recorded baseline per family (detection, false positives, cost, latency, timeout/incomplete rate), run by the maintainer before any brief change. Blocked by: none. Acceptance: harness tests pass offline with a fake checker; the corpus and pinned versions are committed; the baseline aggregate is posted on this issue.
- [ ] **M2: Code checks report coverage and the MR shows it.** Inventory, coverage field, column, claim capability, MR section, run page, CLI, docs, changelog. Brief asks for the account but is otherwise unchanged. Blocked by: M1 (the baseline must be frozen and measured before any brief change), PRD #3166 (done); rebases onto PRD #3168 M1. Acceptance: examples 1-3; rollout-order and LiveDB tests; gates `task gate:agent`, `task gate:api`, `task gate:web`.
- [ ] **M3a: Brief, approved_milestones excerpt, tests (worker).** New brief with effect-based environment wording, acceptance-criteria findings and the safety carve-out, the `approved_milestones` prompt excerpt, code-brief instruction tests, dated ADR-2149 amendment, a note on PRD #2170's brief text, and a `specs/human.md` Code cross-check line updated with an `(AI-synced YYYY-MM-DD)` entry: advisory acceptance-criteria findings, the safety carve-out, never blocking. Blocked by: M1 (baseline frozen before any brief change), M2; rebases onto PRD #3168 M1. Acceptance: example 4 at instruction level; gates `task gate:agent`, plus `task gate:api` if docs change, and `task check-docs:web`.
- [ ] **M3b: Replay and hosted verification (maintainer).** Corpus cases with an undelivered criterion added, the pinned old brief rerun on them before comparing (separately authorized maintainer model calls), then a maintainer replay of the M3a brief on the M1 corpus and hosted verification. Blocked by: M3a. Acceptance: example 5 posted; any detection gain is reported with its false-positive, cost, latency and timeout change; unmeasured items named.

## Risks

- A deeper brief raises latency and the timeout/incomplete rate against the `min(15 min, deadline)` model timeout; M1 baselines it and M3b reports it.
- Family asymmetry in search and read bounds skews comparisons; report per family.
- Model nondeterminism; mitigated by repeats, never by a single run.
- Criterion findings compete with defect findings for the 20-finding cap and add up to 256 KiB of milestones to the prompt; defects come first and are never evicted, so a full defect report can leave no room for criterion notes; M3b reports the latency change.

## Decision Log

- **Worker owns the denominator.** A model listing its own coverage audits itself; the worker's git inventory is independent and catches deleted files.
- **Separate field and column, not findings.** Findings caps and the dispositions constraint cannot hold coverage without breaking existing rows.
- **Capability-gated wire change.** The strict verdict decoder would refuse a new field from a newer worker.
- **Baseline before brief change.** Otherwise improvement cannot be measured.
- **Gate evidence parked.** No safe, commit-bound gate record exists today.
- **Acceptance-criteria findings, advisory** (user decision 2026-10-23). #1231's auditing value moves here as ordinary findings; code cross-check stays non-blocking, so no new gate, field or permit.
- **Safety carve-out** (user decision 2026-10-23). An undelivered safety, security or data-integrity criterion is a defect finding, not coverage; still advisory.
- **M3 split** (2026-10-23). M3a is worker-deliverable; M3b needs maintainer credentials and hosted access, so it cannot ride the same run.
- **Fixed criterion prefixes and no eviction** (lead and buddy, 2026-10-23). Same marker as PRD #3168; defects keep their place within the cap.
- **Baseline rerun on new cases** (2026-10-23). Criterion cases added for M3b are scored against the pinned old brief before comparing, with separately authorized maintainer model calls.
- **Milestones excerpt worker-only.** The claim already carries the frozen milestones under the candidate digest, so only the prompt changes.
- **Order with PRD #3168.** Both edit `agent/src/codex/cross-check.ts` in different functions; PRD #3168 M1 lands first and M2/M3a here rebase onto it. M1's baseline is unaffected, since PRD #3168 does not touch the code brief.
