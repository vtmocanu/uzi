# PRD #4813: Evals statistics, uncertainty and trade-offs across evals

**Status**: Draft
**Priority**: Normal
**Umbrella**: #4808
**Blocked by**: #4809, #4810

## Problem

One eval answers "how did these two configurations do on this issue", with at most three trials each. That is too little data to name a winner: run-to-run variance in agent evaluations is large even at temperature 0, and the usual normal-approximation error bars are far too narrow at this sample size. An owner who runs the same two configurations on several issues has no way to combine those evals, and raw numbers side by side invite reading noise as a difference. Owners also weigh quality, cost and time differently, and a single averaged score hides that trade-off.

## Outcome

The CLI and API gain a **comparison view** (rendered later by the Evals tab, #4814) over one eval or a set of evals that share the same two candidate configurations (matched by a configuration fingerprint). It shows per-candidate pass rates with approximate small-sample (Wilson) intervals, paired per-issue differences in quality, cost and active time with seeded bootstrap intervals over issues, the set of candidates no other candidate beats on both quality and cost, and an owner-weighted composite score. When the data cannot separate the candidates, the view says `insufficient data` instead of ranking them.

Acceptance examples:

1. One eval, 3 trials per candidate, pass counts 2 of 3 and 3 of 3: the view shows both pass rates with their intervals, says `insufficient data` for the difference, and shows no winner.
2. Five evals on five issues with the same fingerprint: the view pairs the candidates per issue, shows the mean paired difference in lead-reviewer quality with a 95% bootstrap interval over issues (fixed seed, so a reload shows the same numbers), and names a better candidate on a metric only when that interval excludes zero.
3. Candidate `b` on a custom provider has `unreported` cost: the trade-off output lists it beside the non-dominated set with cost `unknown`, the composite excludes the cost term for both candidates and says so, and nothing treats the missing cost as zero.

## Out of scope

- New trials, graders or hidden tests: those PRDs (#4809, #4810, #4812) own the data; this PRD only reads it.
- Cross-owner or instance-wide leaderboards: an owner sees only their own evals.
- Significance testing beyond the stated intervals (no p-values, no multiple-comparison correction).

## Modules and seams

1. **Configuration fingerprint**: a sha256 over each candidate's frozen fields (harness, provider id and revision, model, effort) and the grading config (both reviewers, rubric version), computed on read from the frozen eval fields, so it needs no column and no backfill. Evals combine only when the unordered pair of candidate fingerprints and the grading fingerprint match exactly, so an eval with the two candidates in swapped `a`/`b` order still combines (candidates are matched by fingerprint, never by label). The unit is the distinct `(repo, issue)` pair. Evals on the same issue pool their trials only when they share the frozen issue digest and base SHA; when an issue has several snapshots, the most recent snapshot with graded trials for both candidates is used and the others are listed as excluded. The view lists any eval that was excluded and why.
2. **Statistics module** (`api/internal/evalsvc/stats`, pure Go, no I/O): **model-review pass rate** (#4810's rule; this PRD does not read #4812's holdout results, which the Evals tab (#4814) displays alongside these statistics, so F stays independent of #4812) with approximate Wilson score intervals, always printed with `n` and the ungraded count; paired per-issue differences of the per-issue candidate means; percentile bootstrap over issues with 2,000 resamples and a fixed seed derived from the sorted eval ids; `insufficient data` when fewer than 3 issues are paired or the interval includes zero. Per-issue quality is the mean of the lead reviewer's criterion scores over graded trials; ungraded trials are excluded and counted.
3. **Trade-off output**: the non-dominated set on (quality up, cost down) over candidates with known cost; candidates with `unreported` cost are listed separately, never placed in the set.
4. **Composite score**: owner weights for quality, cost and active time (0 to 100, normalised to sum to 1, stored per owner, default equal). An all-zero weight vector is refused. Fixed, documented scales so the size of a gap matters: quality `(score - 1) / 4` on the 1 to 5 rubric; cost and active time as `best / value` (1.0 for the cheaper or faster candidate, 0.5 for one twice as expensive or slow). A genuine metered zero is known, not missing: when the best value is 0, a 0 scores 1.0 and any non-zero value scores 0.0, so no division by zero occurs. Any term with a missing value is dropped for every candidate and named, and the remaining weights are renormalised to sum to 1; when every term is dropped, or every remaining weight is zero, the composite is `unavailable`. The composite is labelled with its weights wherever it appears.
5. **API and CLI**: `GET /api/evals/compare?ids=...` or `?fingerprint=...` (owner-scoped, at most 50 evals per request); `uzi eval compare <eval>...|--fingerprint <fp>`, `--json`; `uzi eval weights set`. The JSON shape is the contract the Evals tab (#4814) renders; this PRD ships no web UI. `docs/evals.md` statistics section with a short plain-language explanation of each interval; `task docs:sync`.
6. **Bounds**: at most 50 evals per comparison; computation runs in the request with the bootstrap capped at 2,000 resamples over at most 50 issues, so it is bounded in time and memory.

## Testing decisions

- Statistics: table tests with known inputs (Wilson intervals at n = 1, 2, 3; bootstrap determinism for a fixed seed; the `insufficient data` rule at 2 and 3 issues).
- Fingerprint: changing any frozen field changes it; evals with different fingerprints never combine.
- Missing cost: composite and trade-off tests for `unreported` and `subscription` costs.
- CLI: golden output for each state (insufficient data, winner on a metric, unknown cost).

## Milestones

### M1: Comparison view with intervals and insufficient-data rule
**Blocked by**: none
Fingerprint; statistics module; compare API and CLI with pass-rate intervals and paired differences; docs, changelog fragment.
Acceptance: examples 1 and 2.

### M2: Trade-off set and weighted composite
**Blocked by**: M1
Non-dominated set; owner weights; composite with dropped-term labelling.
Acceptance: example 3.

### Landing obligations
- Full `test:api`, `gate:repo` pending exact-head PR CI.

## Decision Log

- **Wilson intervals and a seeded percentile bootstrap.** Normal-approximation intervals understate uncertainty badly at these sample sizes; Wilson is exact enough and closed-form, and a seeded bootstrap keeps numbers stable across reloads. Rejected: Bayesian models (heavier to explain and to test for little gain at this scale).
- **Pair by issue, aggregate over issues.** Issues differ far more than trials; pairing removes the issue effect, and the issue is the unit of resampling.
- **`insufficient data` instead of a winner** whenever the interval includes zero or fewer than 3 issues pair. A tab that always names a winner would mislead most of the time.
- **Exact fingerprint match only.** Combining evals with a different model, effort or reviewer would compare different things.
- **Fixed scales, not min-max.** With two candidates, min-max turns any gap into the full 0 to 1 spread. Ratio-to-best keeps a 5 percent cost gap small.
- **Composite drops missing terms for everyone.** Ranking a candidate with unknown cost as free would reward missing data.
