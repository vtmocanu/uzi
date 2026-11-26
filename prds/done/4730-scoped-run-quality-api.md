# PRD #4730: Scoped run diagnostics API: Quality

**Issue:** #4730.
**Status:** Done (#4730), completed 2026-11-26.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Read-only API, DTO contracts, docs and specs. Additive indexes only when supported by plans. No lifecycle, tracking, interface or CLI changes. Implementation and validation leave `.github/workflows/**` untouched.
**Evidence baseline:** `5208f3e0dfc96e8bdd0218ca49d9bbb38ee53151`. Assign migration numbers at landing.

## Progress

- [x] M1 owner/admin Quality evidence

## 1. Problem

Retained rework and review evidence describes separate activities. Aggregating it requires a consistent PR identity and explicit distinctions among attempt outcomes, successful assessments and unreviewed work.

## 2. Outcome

Owner and admin Quality reads report retained PR rework, CI-fix activity, cross-check outcomes and judge review coverage.

1. A selected merged PR with one manual and one out-of-window automatic rework appears once in the two-rework group.
2. Plan and code attempts have separate outcome series, bucketed by decision time. Pending attempts are a current count.
3. A failed judge review, an eligible unreviewed run and a completed review occupy distinct groups. A rejudge may change an older cohort's retained verdict.

## 3. Out of scope

New review or rework behavior, historical judge versions, complete lifetime rework history, actual PR merge timestamps and changes to existing review or cross-check routes.

## 4. Modules and seams

### Routes and shared read boundary

Add literal owner `/api/analytics/quality` and admin `/api/admin/analytics/quality` GET routes. Wire them through #4684's authentication, `RequireAdminRO`, distinct owner and admin-all query shapes, and route-owned repo visibility. Add both `noLimiter` mount rows. Use the existing options endpoint. Accept #4684's `window`, `tz`, `repo`, `harness`, `kind` and admin `user` selectors with the same validation and errors. Use section key `quality` for admission. Unspecified parameters and grouping controls are unsupported.

For admin all-users requests, omitted `user` and explicit `user=all` both return HTTP 400 with `window all not available for all users` when `window=all`. Options omits `all` for that scope. The owner route and admin `user=<uuid>` still accept `window=all`.

Reuse #4684's analytics package and shared read boundary. Use `analytics.Admission.Acquire` for per-caller and process-wide admission, `store.RunAnalyticsSnapshot` for the snapshot, `analytics.ComputeBounds` with the captured Go clock for calendar intervals, and `apitypes.AnalyticsEnvelopeDTO` for the common envelope. Use its half-open Go-computed boundaries and 104-bucket all-time cap. SQL does no independent calendar generation or clock reads. Apply the shared authority rules before aggregating. Repo visibility comes from the mounted route. The owner route masks inaccessible repo IDs and paths. Admin routes may reveal repo IDs and paths on authorized rows, including selected-user results. Return explicit `time_basis`, applicable population, observed and unknown counts where meaningful, and reasons for unavailable measures. Live measurements disclose their effective filters. Include empty buckets where the shared calendar emits them.

### Populations and evidence

- **Currently merged PRs by retained rework.** Select distinct PRs via authorized, filtered creation-cohort work. Use #4684's connection-local identity, scoped merge witness and earliest matching cohort bucket. Window, harness and kind filters select the PR cohort. Merge witnesses may come from outside those filters. Owner and admin-selected-user requests require a witness from the same user as the scoped cohort. Count distinct retained `mr_rework` runs associated through that identity and verified retained links, including manual and out-of-window runs. Apply the response's ownership boundary to associated evidence even when range/harness/kind select the seed cohort rather than its associated attempts. Group by 0, 1, 2 and 3+. A zero is zero matched retained runs. Explain lost, deleted or ambiguous links and external PRs connected more than once. `CreateAutoMRReworkRun` and `CreateManualMRReworkRunAndAdvance` stamp `repo_id`, `mr_iid` and `target_run_id`. `mr_rework_ledger.attempt_count` is automatic-only. Actual merge-day activity and run-end-to-merge duration are null with `not_tracked`.
- **CI-fix activity.** Count `ci_fix` runs and current outcome classes in their creation buckets. Reuse #4684's `run_outcome_classes` view in `00542_run_usage_model_totals.sql`. Report activity and applicable n. A kind filter excluding CI-fix makes this measure not applicable.
- **Cross-check evidence.** Select attempts through the authorized lead run. Lead repo, harness and kind determine selection. Checker harness is separate metadata. For decided attempts use `cross_checks.decided_at` independently of lead creation. Return a live pending count. Keep stage and stored result distinct: plan uses `verdict`, while code uses `outcome` and the legacy code `verdict` field is inert (`00324`). Use `cross_check_history.sql` as the provenance and classification reference, including replaced/interrupted evidence. A code checker still winding down after a decision is not a pending decision. Exclude malformed or missing time evidence from the dated series and disclose it. Expose aggregates and fixed classes, with no raw findings or prompts.
- **Judge verdict and coverage.** Use `runkind.JudgeEligible` (currently `issue` and `ci_fix`) and the stable target rules in `workersvc/judge_enqueue.go` (completed/failed and applicable failure-origin and pre-start exclusions). Define the eligible denominator from retained target properties. Current credential availability, consent and spend guards cannot establish historical eligibility or expected review completion. Document that selection limitation. Join `run_reviews` by scoped target and owner. `UpsertRunReviewWithRecommendations` retains one row per target; a rejudge overwrites it. Count `ideal`, `ok` and `issues` only when review status is `complete`. Keep `failed` and unreviewed eligible targets separate, and report complete-reviewed/eligible n by target creation bucket. Treat unavailable evidence explicitly.

Pin the DTO in recorded Go JSON fixtures and update existing TypeScript contract mirrors where required. Document the endpoint in `docs/` with title/order/audience frontmatter, use `task docs:sync` for the generated embed, and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`. Leave `specs/ai.md` frozen. Keep the existing endpoints and shared #4684 contract intact.

## 5. Testing decisions

- Verify cross-owner association isolation, duplicate PR collapse, two connections to the same project, current merge witness behavior, manual and out-of-window rework, and conflicting or missing retained links. An out-of-filter witness from the scoped user establishes a merge. A witness from another user alone does not establish a merge in either owner or admin-selected-user scope.
- Pin both stages with different stored result fields, lead versus checker harness, live pending versus decided-but-unsettled records, replaced attempts, and an old lead with a decision inside the window.
- Cover judge-eligible and excluded targets, failed fallback verdicts, missing reviews, null reasons, rejudge replacement in the original target bucket, and coverage denominators independent of current settings.
- Put LiveDB cases in `store` and `handler`. Use #4684's actual-router auth, owner isolation and admin-selected scope fixtures, plus its snapshot barrier, timeout, single-flight and cancellation patterns. Test both routes with a cookie admin, an admin-read bearer and an ordinary bearer owned by an admin. Check release on every exit and SQLSTATE 57014 mapping. Pin session-TimeZone independence and local DST and all-time boundaries where used. Through the router, test identical `window=all` refusal for omitted `user` and explicit `user=all` on the admin route, omission of `all` from admin all-users Options, and acceptance of `window=all` on the owner and admin-selected-user routes.
- Name LiveDB tests with the `LiveDB` suffix. Observe targeted regression mutations fail and restored code pass.
- **In-run checks** include `task check:api`, affected `go test -race -count=1` packages, and named PostgreSQL 17 LiveDB cases with RUN and PASS evidence and zero skips (Docker required). Run `task check:migration-numbering` and `task check:migration-additive` if adding an index. Run `task docs:sync`, `task check-docs:web`, and `task check:changelog-entry` with one lowercase UUID-v4 fragment. Sweep changed contract literals and identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`; run affected `task test:e2e-lib` / `task test:forge-fake` and name unrun phases.

## 6. Milestones

### M1. Owner/admin Quality evidence

Deliver the routes, aggregates, DTO fixtures, docs and tests. Acceptance is the examples and evidence rules above, including matched retained rework, stage-specific results and explicit coverage.

Use #4684's 100k-run synthetic seed or an equivalent, stating owner skew, range selectivity and fresh statistics. Capture owner and admin `EXPLAIN (ANALYZE, BUFFERS)` including generic prepared plans and measured request impact. Index only demonstrated gaps, following `00283` for concurrent builds in a NO TRANSACTION migration and invalid owned index cleanup. No allow-missing. State retained rework/review/attempt fan-out and show the new heavy joins under realistic skew.

### Landing obligations (not milestones)

Before merge, the exact PR head must pass full `test:api`, `gate:repo`, `scan:secrets`, `sast:semgrep` and the LiveDB sweep in PR CI. Report measured affected-job time and critical-path impact, separating cache, queue and retry effects where observable.

## 7. Decision Log

| Decision | Reason |
|---|---|
| Reuse #4684's shared boundary, scope and outcomes | A second implementation would risk authority and population drift. |
| Attribute PRs once and count retained matching rework | Manual activity belongs in the measure and the automatic ledger alone is incomplete. |
| Separate plan, code and judge populations | The sources answer different questions and have different retention and eligibility rules. |
| Use retained latest judge review | The store keeps one review per target, so historical versions cannot be reconstructed. |
| Fold the merge-witness lookup into one GROUP BY over a UNION ALL of the seed and witness keys | A per-row `IN (SELECT ...)` over the witness CTE becomes an unhashed SubPlan once the witness set outgrows `hash_mem`; the fold is independent of it (6.3 s vs 1.15 s at `work_mem` 64kB, section 8). A generic-plan guard test pins the shape. |
| Add no index | The bar was a 20% p95 gain on a Quality workload or removal of the 8 s timeout risk. The rework index gained at most 7.2% p95, the `decided_at` index was never chosen and the worst timing is 2.4 s. |
| Pending cross-checks have no time cutoff | Pending is a live count of undecided, uninterrupted attempts of any age, disclosed with `effective_filters` and `window: not_applied`. |
| Decide cross-check applicability per stage | Plan and code use their own cross-checkable kinds (`runkind.PlanCrossCheckable`, `runkind.CodeCrossCheckable`), and pending applies when either does. |
| Cover the SQLSTATE 57014 mapping with a unit test through `analyticsFail`, as #4684 does | #4684 has a store-level LiveDB timeout test of the shared snapshot (`TestAnalyticsSnapshotStatementTimeoutLiveDB`) but no handler-level one; Quality uses the same snapshot and `analyticsFail` path. |
| Known follow-up: narrow `cc_base` | `cc_base` carries `findings` for every cross-check (about 109 MB temp spill at 4 MB `work_mem`, about 40% of the all-users time). A candidate fix computes `findings_count` only for in-window decisions. Not done here. |

## 8. Performance evidence

Summary of the M1 evidence run (seed 0.4730, PostgreSQL 17 (postgres:17 image), 4 vCPU, warm cache, `work_mem` 4MB).

**Seed.** #4684's base seed (200 owners, largest owner 20.0% of runs, top 10 owners 59.3%) plus a Quality extension: 140,018 runs (20,018 `mr_rework`), 845,032 usage legs, 1,250 repos, 92,420 cross-checks (59,724 plan, 32,696 code), 35,722 reviews over 59,553 judge-eligible runs (60.0%, 15% of reviews failed). Fan-out: 1.60 plan attempts per lead that has any plan check (0.64 across all 93,520 delivery leads), and at most one review per eligible target (60% coverage). Statistics were fresh: the seed runs `ANALYZE` on `cross_checks`, `run_reviews`, `judge_skips` and `runs` after loading (`analytics_evidence_livedb_test.go`). 30d range selectivity is 9.94% of runs (largest owner 10.02%). 24,935 PRs, 69.0% with a merge witness; reworks per PR 0/1/2/3+ are 13,725 / 6,243 / 3,051 / 1,916.

**Store timings** (ms, n=20, custom plan unless noted, statement timeout off, **JIT on**):

| Workload | Custom median / p95 | Generic median / p95 |
|---|---|---|
| Owner 30d (largest owner) | 193.5 / 196.9 | 207.0 / 211.0 |
| Owner `window=all` | 702.0 / 712.4 | 560.6 / 574.9 |
| Admin user 30d (largest owner) | 200.9 / 203.7 | 211.9 / 215.4 |
| Admin all-users 30d | 1168.3 / 1181.4 | 631.5 / 636.5 |
| Admin all-users `window=all` | 2320.7 / 2361.9 | 2196.5 / 2224.8 |

**Request impact** (production router, cookie session, analytics pool of 20, JIT off as in production's analytics snapshot, sequential, N=25 after 2 warm-ups, all 200):

| Request | p50 ms | p95 ms | max ms |
|---|---|---|---|
| `GET /api/analytics/quality?window=30d` (largest owner) | 205.8 | 209.1 | 209.7 |
| `GET /api/analytics/quality?window=all` (largest owner) | 541.7 | 554.9 | 555.9 |
| `GET /api/admin/analytics/quality?window=30d` (all users) | 625.2 | 646.8 | 682.8 |
| `GET /api/admin/analytics/quality?user=<largest owner>&window=30d` | 205.7 | 216.1 | 216.4 |

**Plan nodes and buffers** (`EXPLAIN (ANALYZE, BUFFERS)`, custom plan, 30d):

- Owner 30d (largest owner, 2,811 runs): shared hit 13.6k / read 6.1k blocks, temp written 2,154 blocks; the same CTE structure as all-users, with Bitmap Heap Scans in place of Seq Scans.
- Admin all-users 30d: shared hit 43,679 / read 23,179, temp written 14,000 blocks (about 109 MB). The dominant cost is `cc_base` (Seq Scan of 92,420 `cross_checks` hash-joined to a Seq Scan of 140,018 `runs`, 251 ms) and the `cc_dec`/`cc_win`/`cc_cls` materialisation (341 to 494 ms, 13,998 temp blocks); the rework CTEs total about 200 ms (custom; about 150 ms generic).

**Corrections and caveats.**

- The store timings ran with the PostgreSQL default JIT on. JIT fired on three custom-plan workloads (221 to 471 ms inside execution); production's analytics snapshot sets JIT off and the HTTP table ran through that path. Most of the all-users 30d gap between generic and custom plans is JIT, but with JIT time subtracted the custom plan is still slower (738 vs 657, 683 vs 609 and 758 vs 654 ms across the baseline and two candidate-index states, one EXPLAIN each; temp written 14,000 vs 10,805 blocks), so the generic plan is about 10 to 15% cheaper there. For all-users `window=all` the gap is JIT.
- The `cross_checks` immutability and write-once triggers (migrations 00312, 00325, 00360) are BEFORE INSERT OR UPDATE, not UPDATE-only. The seed inserts rows in their final state, which pass them.
- The seed has no `cross_check` child runs, so its runs table is about 40% light for its cross-check volume, and it measures a single volume point.
- Admin all-users `window=all` is refused by the API, so its 2.4 s timing is an upper bound for the all-users windows a request can reach. No workload approached the 8 s statement timeout.
- Sequential requests cannot trigger the per-caller admission 503; busy is covered by unit tests.

**Witness lookup hardening** (EXPLAIN ANALYZE, all-users scope, 2,805-PR window, about 17k witness keys; old per-row `IN` vs the fold):

| `work_mem` | Old, custom / generic ms | New, custom / generic ms |
|---|---|---|
| 4MB | 956 / 686 | 936 / 657 |
| 256kB | 1005 / 679 | 1023 / 693 |
| 64kB | 6341 / 5983 | 1148 / 711 |

At 64kB the old plan runs the witness CTE as an unhashed SubPlan (loops=2805, about 11k rows each; 5.33 s of 6.34 s). The owner scope was unaffected (190 to 234 ms either way). `TestQualityGenericPlanShapeLiveDB` asserts from `EXPLAIN (VERBOSE, FORMAT JSON)` under `force_generic_plan` that no CTE Scan sits in an unhashed SubPlan, no Nested Loop filters on `mr_iid`, and the witness is read by a plain scan; its negative control swaps in a deliberately unhashed stand-in for the old `IN` lookup (the real old SQL cannot be forced unhashed at small test scale, because the planner hashes by estimated size), and the real old SQL at 64kB is the timing table above.

**Index decision** (custom-plan p95, positive is faster):

| Workload | Baseline ms | `runs(repo_id, mr_iid) WHERE kind = 'mr_rework'` | `cross_checks(decided_at)` |
|---|---|---|---|
| Owner 30d | 196.9 | -1.0% | -4.2% |
| Owner `window=all` | 712.4 | -2.8% | -1.9% |
| Admin user 30d | 203.7 | +7.2% | +1.6% |
| Admin all-users 30d | 1181.4 | +1.5% | +0.8% |
| Admin all-users `window=all` | 2361.9 | +3.7% | +2.8% |

The rework index was chosen by the planner and gained at most 7.2%; the `decided_at` index was never chosen, because `cc_base` reads the whole table through a materialized CTE with no `decided_at` predicate. No migration was added.

**Flake observed on a shared database.** `TestRunUsageGenericPlanLiveDB`'s Spend all-scope check was observed failing on an accumulated shared database at a0a259b79, where that test file and the Spend SQL were still identical to the base commit (Quality seed rows were added to it later, in this branch's m3); it passes on a fresh database. A base-commit reproduction was not recorded.

**Verified and still unverified.** Verified: the owner and admin plans, generic-plan behavior, witness and link association under the ownership boundary, cross-check stage fields, judge eligibility rules, request impact at the seed above, and that no index is warranted. Still unverified: legacy rework association completeness (the seed stamps every rework link, so it cannot show how many legacy rework runs lack one; counts are lower bounds), and behavior at other volumes or with cross-check child runs in the seed. Passing tests do not establish retention completeness.
