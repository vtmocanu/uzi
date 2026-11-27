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
| Add no index | The bar was a 20% p95 gain on a Quality workload or removal of the 8 s timeout risk. With JIT on, the rework index appeared to cut all-users 30d custom p95 by 23.5%, but the cause is undetermined (an index-driven plan change or a different statistics sample; the baseline and candidate plans both carried about 449 ms of JIT, and the candidate plan had a different shape, 9,758 against 12,993 temp blocks written). The decision rests on the production-like JIT-off check in both plan modes, which puts the candidate within -6.1% to +5.6% on every workload (all-users 30d custom 688 to 688 ms), so the bar is not met in either mode. That check is one run at n=20 on a fresh reseed whose counts differ slightly (140,113 runs against 140,169), with ANALYZE after CREATE INDEX. The `decided_at` index was never chosen, and the worst observed statement timing is about 2.2 s in the timed runs (EXPLAIN ANALYZE runs reached about 2.3 s; section 8). |
| Pending cross-checks have no time cutoff | Pending is a live count of undecided, uninterrupted attempts of any age, disclosed with `effective_filters` and `window: not_applied`. |
| `cc_base` has no creation-time cutoff; every non-pending attempt that is not dated is undated | A `created_at < now` cutoff silently dropped rows with malformed time evidence (non-finite `created_at` including `+infinity`, a far-future `created_at`, `decided_at` before `created_at`, missing or non-finite `decided_at`). They are now counted as `undated` and disclosed. The dated series is bounded by `decided_at` in `[from, now)`. Residual gaps, not disclosed in the response: the dated rule has no upper bound, so a non-pending attempt with a finite future `decided_at` (or a far-future `created_at` with `decided_at >= created_at`) is dated but outside `[from, now)` and counted nowhere (a far-future `created_at` makes an attempt undated only when `decided_at < created_at`); and attempts whose stage cannot apply to the lead kind (plan attempts on task leads, any attempt on chat, judge or job leads) are excluded under every filter. |
| Cross-check population is per stage | `cc_base` keeps plan attempts only for plan-cross-checkable lead kinds and code attempts only for code-cross-checkable lead kinds (`runkind.PlanCrossCheckable`, `runkind.CodeCrossCheckable`), so counts agree with `applicable` (kind=task: plan not applicable with zero counts; retained plan attempts on other lead kinds are not counted). |
| Decide cross-check applicability per stage | Plan and code use their own cross-checkable kinds (`runkind.PlanCrossCheckable`, `runkind.CodeCrossCheckable`), and pending applies when either does. |
| Cover the timeout-to-503 path at three levels: unit test of the 57014 mapping through `analyticsFail`, store-level snapshot test of server-side 57014, and a handler LiveDB test of a real query timeout through the router | `TestAnalyticsQualityStatementTimeoutIs503WithRetryAfterAndReleases` pins the mapping and slot release through the handler. `TestAnalyticsSnapshotStatementTimeoutLiveDB` (#4684) covers the shared snapshot. `TestAnalyticsQualityStatementTimeoutThroughRouterLiveDB` holds an `ACCESS EXCLUSIVE` lock on `runs` and pins a real query timeout through the live router: 503, `Retry-After`, the timed-out body text and that the next request returns 200 (slot released). The snapshot's 8 s Go deadline normally fires before the server-side `statement_timeout`, so this test does not itself prove server-side 57014 classification; that is pinned at store level by `TestAnalyticsSnapshotStatementTimeoutLiveDB`, and the handler unit test pins the 57014 to 503 mapping. |
| Known follow-up: narrow `cc_base` | `cc_base` carries `findings` for every cross-check, and the `cc_*` CTEs write about 100 MB of temp (12,993 blocks at 4 MB `work_mem`, all-users 30d custom plan). A candidate fix computes `findings_count` only for in-window decisions. Not done here. |

## 8. Performance evidence

Re-run of the M1 evidence at commit `0d4a4919da868f5e7e6fe294d68d631698308d1a` (seed 0.4730, fresh full reseed), replacing the earlier figures. Environment: PostgreSQL 17.12 (Debian 17.12-1.pgdg13+1, `postgres:17` image, `--shm-size=1g`, recorded by `SELECT version()` in the run), `work_mem` 4MB, `jit` on by default, 4 processors (`GOMAXPROCS=4`), warm cache. Raw logs were kept outside the tree (scratch evidence directories of the run).

**Harness runs and flags.** All use `-tags analyticsevidence -count=1 -v -p 1` against a LiveDB PostgreSQL 17 (`UZI_TEST_DATABASE_URL`).

- Store (510.6 s, exit 0, PASS): `UZI_ANALYTICS_PLAN_EVIDENCE=1 UZI_EVIDENCE_KEEP=1 UZI_EVIDENCE_ONLY=quality UZI_EVIDENCE_INDEXES=5,6 UZI_EVIDENCE_LOG=<file>`, `-run TestAnalyticsPlanEvidenceLiveDB ./internal/store/`. Fresh seed (`UZI_EVIDENCE_RESEED` unset), baseline on (`UZI_EVIDENCE_BASELINE` unset), `UZI_EVIDENCE_RUNS` default 20 (n=20). Baseline measured in custom and generic plan modes; candidates in custom mode only. Statement timeout off, **JIT on** (PostgreSQL default).
- HTTP (exit 0, PASS): `UZI_ANALYTICS_PLAN_EVIDENCE=1 UZI_EVIDENCE_LOG=<file>`, `-run TestQualityHTTPEvidenceLiveDB ./internal/handler/`, reusing the kept seed (`UZI_EVIDENCE_KEEP=1` from the store run). Production router and snapshot path (8 s statement timeout, **JIT off** as in production's analytics snapshot).
- JIT-off index check (exit 0, PASS): `UZI_ANALYTICS_PLAN_EVIDENCE=1 UZI_EVIDENCE_ONLY=quality UZI_EVIDENCE_INDEXES=5 UZI_EVIDENCE_LOG=<file>`, `-run TestAnalyticsPlanEvidenceLiveDB ./internal/store/`, no `UZI_EVIDENCE_KEEP` (the seed was truncated at the end). It ran from a scratch copy of the commit whose only change set `jit = off` (the harness session, the timing statements and the `EXPLAIN` statements) and measured the candidate in both plan modes (custom and generic) instead of custom only. All 20 EXPLAINs reported JIT 0.

**Seed.** #4684's base seed plus a Quality extension: 140,169 runs (20,169 `mr_rework`), 848,520 usage legs, 1,250 repos, 200 users, 92,547 cross-checks (59,560 plan, 32,987 code), 35,818 reviews over 59,553 judge-eligible runs (60.1%; 5,391 failed reviews), 2,988 judge skips. Table sizes: runs 114 MB, `cross_checks` 956 MB (878 MB TOAST), `run_reviews` 7,152 kB. Owner skew: the largest owner holds 27,891 runs (19.9%), the top 10 owners 59.3%. Kind mix: chat 2,450, ci_fix 8,212, issue 60,035, job 3,563, judge 20,467, mr_rework 20,169, prompt 6,096, self_improve 7,249, task 11,928. Statistics were fresh: the seed runs `ANALYZE` on `cross_checks`, `run_reviews`, `judge_skips` and `runs` after loading (`analytics_evidence_livedb_test.go`).

**30d window** (from `analytics.ComputeBounds` with the harness clock 2026-11-20T12:00:00Z): from 2026-10-22T00:00:00Z (day-truncated) to the 12:00 clock, 30 buckets; `window=all` starts 2023-11-06T00:00:00Z with 80 buckets. Selectivity: 9.85% of 140,169 runs overall (13,801 in window) and 9.93% of the largest owner's 27,891 (2,770). Rework cohort in the 30d window: 2,810 PRs from 4,619 delivery runs overall; 559 PRs from 903 delivery runs for the largest owner. Cross-check decisions in the window: plan 5,915, code 3,349, of which 243 belong to leads created before the window (the harness prints one total for both stages). The 5,915 is the harness's raw count; the all-users statement counts 5,116 plan rows after the per-stage filter.

**Fan-out.**

- PRs: 24,935 distinct (repo, PR), 68.9% with a merge witness (17,189 witnessed). Reworks per PR 0/1/2/3+: 13,697 / 6,265 / 3,013 / 1,960. Of the retained reworks, 25.2% are manual, 91.1% were created before the 30d window and 6.9% have an unverified link.
- Plan attempts per lead that has any plan check (1/2/3 rounds): 20,630 / 11,098 / 5,578, so 37,306 leads out of 93,520 delivery leads (39.9%). Final plan verdicts: approve 22,390, revise 29,693, block 3,594, failed 3,738, pending 145 (the totals count attempts, not leads).
- Code attempts per lead: at most 1; outcomes completed 26,340, failed 6,577, pending 70.
- Reviews per eligible target: at most 1 (59,553 eligible, 35,818 reviewed, 60.1%).

**Store timings** (ms, n=20, statement timeout off, **JIT on**):

| Workload | Custom median / p95 | Generic median / p95 |
|---|---|---|
| Owner 30d (largest owner) | 182.3 / 185.6 | 199.3 / 202.1 |
| Owner `window=all` | 679.7 / 701.4 | 526.7 / 533.4 |
| Admin user 30d (largest owner) | 178.6 / 183.1 | 195.0 / 198.6 |
| Admin all-users 30d | 1140.3 / 1170.0 | 588.7 / 599.4 |
| Admin all-users `window=all` | 2154.8 / 2179.5 | 2069.5 / 2097.2 |

**Request impact** (HTTP run: production router, cookie session, analytics pool of 20, admission capacity 5, JIT off, sequential, N=25 after 2 excluded warm-ups, every response 200, no 503 timeout or busy; largest owner 27,891 runs):

| Request | p50 ms | p95 ms | max ms |
|---|---|---|---|
| `GET /api/analytics/quality?window=30d` (largest owner) | 198.6 | 208.5 | 215.0 |
| `GET /api/analytics/quality?window=all` (largest owner) | 532.6 | 539.4 | 540.5 |
| `GET /api/admin/analytics/quality?window=30d` (all users) | 602.8 | 741.1 | 746.0 |
| `GET /api/admin/analytics/quality?user=<largest owner>&window=30d` | 194.8 | 202.3 | 202.9 |

Warm-ups (ms, two per scope): owner 30d 225.6 / 185.1, owner all 526.0 / 526.0, all-users 30d 753.2 / 742.3, admin user 198.4 / 187.7. First-200 response sizes: 24,836 / 62,802 / 25,273 / 24,836 bytes. Inside one snapshot (`deps.quality`, 10 runs, excluding HTTP and admission), p50 / p95: owner 30d 190.4 / 205.3, owner all 499.7 / 507.5, all-users 30d 591.2 / 614.5, admin user 30d 189.8 / 206.1.

**Plan nodes and buffers** (baseline, one `EXPLAIN (ANALYZE, BUFFERS)` each; JIT on):

| | Owner 30d custom | Owner 30d generic | All-users 30d custom | All-users 30d generic |
|---|---|---|---|---|
| execution / planning ms | 186.8 / 8.3 | 216.3 / 6.2 | 1152.1 / 8.0 | 626.5 / 6.3 |
| shared hit / read blocks | 12,346 / 7,223 | 117,945 / 2,724 | 60,091 / 7,045 | 559,905 / 18,375 |
| temp written / read blocks | 1,966 / 2,950 | 1,966 / 2,950 | 12,993 / 17,876 | 9,758 / 14,641 |
| JIT ms (functions) | 0 | 0 | 449.4 (566) | 0 |

Custom-plan node times: `cc_base` (hash join of `cross_checks` and `runs`) 79.7 ms for the largest owner (17,256 rows; Seq Scan of `cross_checks` 43.4 ms) and 250.3 ms all-users (85,126 rows; Seq Scan 53.2 ms, hash of 140,169 runs, 3,235 temp blocks written). `cc_dec` / `cc_win` (cumulative as the harness prints them): 96.9 / 122.5 ms owner, 334.2 / 463.1 ms all-users (8,109 / 12,991 temp blocks written). Rework CTEs `rw_sel + rw_link + rw_cnt + rw_key`: 38.8 ms owner, 250.4 ms all-users (`rw_sel` includes `rw_pr` / `rw_seed`: 16.1 / 128.2 ms; the `rework` CTE scan itself adds 8.3 / 42.1 ms). The owner plan reads `runs` through `runs_user_created_idx` / `idx_runs_user` bitmap scans; the all-users plan Seq Scans `runs` (11 loops, 411.6 ms). JIT fired on three baseline custom plans: owner `window=all` 234.4 ms (337 functions), all-users 30d 449.4 ms (566) and all-users `window=all` 236.3 ms (327); it was 0 on every generic plan, owner 30d and admin user 30d.

**What the JIT-off run shows.** Without JIT, all-users 30d is 688 ms p95 in custom mode against 580 ms in generic mode, so the generic plan is about 16% faster there for a reason other than JIT, and the JIT-on custom figure (1170 ms) overstated the gap. Owner `window=all` (437 custom against 485 generic) and all-users `window=all` (1840 against 2019) are faster in custom mode without JIT, so generic is not faster across the board. Production sets JIT off, so the JIT-off figures describe the request path and the JIT-on store table describes the harness.

**Caveats.**

- The store timings ran with the PostgreSQL default JIT on and the statement timeout disabled; the HTTP table ran through the production path (JIT off, 8 s snapshot).
- The all-users `window=all` timing (about 2.2 s median, 2.18 s custom p95) is an observed statement timing on one synthetic seed with the statement timeout disabled; the `EXPLAIN ANALYZE` runs reached about 2.3 s. It is not a bound on smaller all-users windows or on HTTP latency, and that scope is refused by the API. No workload approached the 8 s statement timeout.
- The `cross_checks` immutability and write-once triggers (migrations 00312, 00325, 00360) are BEFORE INSERT OR UPDATE, not UPDATE-only. The seed inserts rows in their final state, which pass them.
- The seed has no `cross_check` child runs, so its runs table is about 40% light for its cross-check volume, and it measures a single volume point.
- Sequential requests cannot trigger the per-caller admission 503; busy is covered by unit tests. A real query timeout through the router (the snapshot's 8 s deadline) is covered by a LiveDB test (Decision Log).

**Witness lookup hardening** (measured earlier on the m3 evidence seed, not re-run in the v2 pass; EXPLAIN ANALYZE, all-users scope, 2,805-PR window, about 17k witness keys; old per-row `IN` vs the fold):

| `work_mem` | Old, custom / generic ms | New, custom / generic ms |
|---|---|---|
| 4MB | 956 / 686 | 936 / 657 |
| 256kB | 1005 / 679 | 1023 / 693 |
| 64kB | 6341 / 5983 | 1148 / 711 |

At 64kB the old plan runs the witness CTE as an unhashed SubPlan (loops=2805, about 11k rows each; 5.33 s of 6.34 s). The owner scope was unaffected (190 to 234 ms either way). `TestQualityGenericPlanShapeLiveDB` asserts from `EXPLAIN (VERBOSE, FORMAT JSON)` under `force_generic_plan` that no CTE Scan sits in an unhashed SubPlan, no Nested Loop filters on `mr_iid`, and the witness is read by a plain scan; its negative control swaps in a deliberately unhashed stand-in for the old `IN` lookup (the real old SQL cannot be forced unhashed at small test scale, because the planner hashes by estimated size), and the real old SQL at 64kB is the timing table above.

**Index decision.** Candidate 5 is `runs (repo_id, mr_iid) WHERE kind = 'mr_rework'` (589,824 bytes) and candidate 6 is `cross_checks (decided_at)` (2,088,960 bytes), both harness-only. The bar is a 20% p95 gain on a Quality workload.

With JIT on (store run, custom plan, p95, negative is faster):

| Workload | Baseline ms | Candidate 5 | Candidate 6 |
|---|---|---|---|
| Owner 30d | 185.6 | 177.5 (-4.4%) | 189.0 (+1.8%) |
| Owner `window=all` | 701.4 | 709.9 (+1.2%) | 671.3 (-4.3%) |
| Admin user 30d | 183.1 | 178.1 (-2.7%) | 186.1 (+1.6%) |
| Admin all-users 30d | 1170.0 | 894.7 (-23.5%) | 1129.7 (-3.4%) |
| Admin all-users `window=all` | 2179.5 | 1993.3 (-8.5%) | 2179.1 (0.0%) |

Candidate 6 was never used in any plan, so its deltas are noise. Candidate 5 was used in every plan, and its all-users 30d gain coincided with a different custom plan shape (temp blocks written 9,758 against 12,993; both plans carried about 449 ms of JIT). Because production's analytics snapshot runs JIT off and the baseline plan carried about 449 ms of JIT, the candidate was re-measured with JIT off in both plan modes (p95 ms, n=20, one run):

| Workload | Base custom | Cand. 5 custom | Delta | Base generic | Cand. 5 generic | Delta |
|---|---|---|---|---|---|---|
| Owner 30d | 183.6 | 174.6 | -4.9% | 191.6 | 195.7 | +2.1% |
| Owner `window=all` | 436.9 | 412.7 | -5.5% | 484.8 | 512.1 | +5.6% |
| Admin all-users 30d | 688.3 | 688.3 | 0.0% | 579.5 | 543.9 | -6.1% |
| Admin user 30d | 182.6 | 172.0 | -5.8% | 189.4 | 189.1 | -0.2% |
| Admin all-users `window=all` | 1839.9 | 1799.3 | -2.2% | 2019.0 | 2014.2 | -0.2% |

Candidate 5 was used in every plan and the `EXPLAIN` summaries show no plan-shape change (temp blocks identical to baseline in every row). With JIT off the all-users 30d custom p95 is 688 ms either way, so the cause of the earlier -23.5% is undetermined (an index-driven plan change or a different statistics sample); it was not the JIT cost, since both JIT-on plans carried the same JIT time. The JIT-off check ran on a fresh reseed (140,113 runs against 140,169) with ANALYZE after CREATE INDEX. Every JIT-off delta is within -6.1% to +5.6%, so the 20% bar is not met on any workload. No migration was added.

**Flake observed on a shared database.** `TestRunUsageGenericPlanLiveDB`'s Spend all-scope check was observed failing on an accumulated shared database at a0a259b79, where that test file and the Spend SQL were still identical to the base commit (Quality seed rows were added to it later, in this branch's m3); it passes on a fresh database. A base-commit reproduction was not recorded.

**Verified and still unverified.** Verified: the owner and admin plans, generic-plan behavior, witness and link association under the ownership boundary (any user's witness and every user's connections on admin all-users; the scoped user's only on owner and admin-selected-user), per-stage cross-check population and undated disclosure, cross-check stage fields, judge eligibility rules, the 57014 to 503 mapping (handler unit test), server-side 57014 classification (store-level test), a real query timeout through the router giving 503, `Retry-After` and slot release (the 8 s Go deadline normally fires first), request impact at the seed above, and that no index is warranted. Still unverified: legacy rework association completeness (the seed stamps every rework link, so it cannot show how many legacy rework runs lack one; retained totals are lower bounds, but deleting a rework or review can raise a class count such as `zero` or `unreviewed`), and behavior at other volumes or with cross-check child runs in the seed. The index check is one JIT-off run at n=20 on a reseed whose counts differ slightly. Passing tests do not establish retention completeness.
