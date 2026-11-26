# PRD #4685: Scoped run outcomes API: Reliability

**Issue:** #4685.
**Status:** Done (M1 delivered by the #4685 run; exact-head PR CI pending as a landing obligation).
**Priority:** Normal.
**Blocked by:** #4684 (scoped run usage API: filter options and Spend).
**Scope:** API, DTO contracts, docs and specs. Additive index migrations only if measured plans demonstrate a gap. No user-interface or command implementation. No changes under `.github/workflows/**`, including validation.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`; re-verify against #4684's merged code before planning. Migration numbers are assigned at merge time.

## Progress

- [x] M1 owner/admin Reliability and lifetime recency (routes, DTOs and recorded fixtures, creation-cohort outcomes, causes, rolling rates, breakdowns, unwindowed latest failure, recoverable-work figure, docs/specs, LiveDB and generic-plan tests, plan evidence in §8)

## 1. Problem

The existing outcome figures are lifetime and rolling-seven-day counts. They cannot show whether failures spiked, which causes dominate, or which harness, repository, kind or model concentrates them. #4684 adds a bounded, scoped read API for cost; outcomes need the same windows, filters, snapshot and privacy rules, not a second implementation.

## 2. Outcome

Owner and admin Reliability routes return creation-cohort outcomes, failure causes, rolling failure rates and dimension breakdowns, plus an unwindowed latest-failure object and a recoverable-work figure. They reuse #4684's admission, snapshot, clock, window, filter, Primary-model and envelope code unchanged.

Acceptance examples:

1. An owner requests Reliability for 30 calendar days. Outcome counts, cause totals and the rolling series reconcile for exactly that owner's creation cohort; another owner's runs are invisible.
2. An administrator requests the admin Reliability route with `user=all&breakdown=user` and gets instance totals and user groups; the owner route stays owner-only for an administrator.
3. Two usage models contribute to one run. Reliability counts it once, under its Primary model.
4. A request filtered to supporting kinds only returns not-applicable with a reason, not zero rates.

## 3. Out of scope

- Any other read section, and any new historical capture or tracking table.
- Any user-interface, navigation, chart or command implementation. API and DTOs only.
- Changing #4684's boundary, windows or Spend contract; a needed change there is its own issue.
- Changes to `/api/usage`, `/api/admin/usage`, their semantics or existing consumers.

## 4. Modules and seams

### Routes and parameters

Add literal routes `/api/analytics/reliability` (owner) and `/api/admin/analytics/reliability` (admin read-only group, `RequireAdminRO`), with #4684's authorization rules, `noLimiter` registration and admission/single-flight key (section `reliability`). Accept #4684's `window`, `tz`, `repo`, `harness`, `kind` and admin `user` parameters with the same validation and errors.

`breakdown=user|primary_model|harness|repo|kind`, default `primary_model`; `user` grouping is admin-all-users only (a 400 naming `breakdown` elsewhere). Reliability groups by Primary model, never usage model, and accepts no `split` (a 400 naming `split`).

### Reliability result

Counts select runs by creation cohort. Exclude chat, judge and cross_check. For supporting-only requests return not-applicable with a reason, not misleading zero rates.

Use #4684's shared outcome classification (`00500_outcome_kind.sql`, the `SelfRunOutcomes` family): plan rejected is failed with `fail_origin='plan_rejected'`; failed excludes that origin; `base_diverged` is its own finished outcome, excluded from failed, failure causes and latest failure; finished is completed + cancelled + plan rejected + base diverged + failed. Null failure origin groups as unknown. Failure-origin totals equal failed within the same snapshot.

Provide outcomes, top-five causes plus Other, seven-day rolling harness failure rates (using #4684's rolling-series rule: one point per displayed bucket, seven calendar days ending at its end, partial by actual coverage) and dimension breakdowns with n. Suppress rate-series/group comparisons below five finished runs; keep raw counts and n. Percentages use summed counts.

`requeued_runs` means creation-cohort runs with retained `requeue_count > 0`, not timestamped requeues or current-episode counter subtraction.

`latest_failure` is a separate unwindowed lifetime object, restricted by owner and the request's repo/harness/kind filters (there is no model filter; `breakdown` never narrows it). Return terminal timestamp `COALESCE(finished_at,status_since)`, origin, run id and `completed_since_last_failure`; preserve existing strict-after semantics. No failure means null. It is not the selected period's last failure.

Include `needs_landing`: failed runs with recoverable work, using the existing eligibility of `SelfRunOutcomes` and `workersvc.DeriveLandingState` (the Go-owned landable-origin set plus an owner-scoped available capture or preserved patch), with the note that the work may already have landed. Do not call it a guaranteed landing backlog.

### Response and documentation

Use #4684's common envelope type. Pin exact DTO names with recorded Go JSON fixtures. Extend #4684's API doc page, run `task docs:sync` (its generated `api/internal/uzidocs/embed` changes are expected), and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`; never edit frozen `specs/ai.md`.

## 5. Testing decisions

- LiveDB tests only in `store` and `handler`. Follow `TestRunOutcomesRollupsLiveDB` (`store/run_usage_integration_test.go`) and #4684's handler tests.
- Authorization through the actual router: cookie admin, admin-read bearer token, and an ordinary bearer token owned by an admin.
- Matched-population parity: existing outcome aggregates include `judge` and `cross_check`, which Reliability excludes. Pin counts against the existing aggregates over the same population, and each intentional difference with its own test. Bounded calendar windows use independent expected values, including pre-window creation/in-window finish and in-flight runs.
- Pin cause sums, `base_diverged` classification, Primary-model single counting, supporting not-applicable, n suppression, the rolling-series coverage rule, lifetime recency strict-after semantics and the recoverable-work caveat.
- Concurrent mutation during a response must not split outcome/bucket snapshots.
- For regression-like seams observe the named mutation fail and the restored code pass, per `.claude/rules/go.md`.
- **In-run checks:** `task check:api`; `go test -race -count=1` on the affected packages; the new and touched LiveDB cases run by name against PostgreSQL 17 with positive RUN/PASS counts and no SKIP (Docker required); `task check:migration-numbering` and `task check:migration-additive` if a migration is added; `task docs:sync` plus `task check-docs:web`; `check:changelog-entry` with one lowercase UUID-v4 changelog fragment. Sweep changed literals/identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`; run affected library/fake targets and name unrun phases.

## 6. Milestones

### M1: Owner/admin Reliability and lifetime recency

**Blocked by:** #4684 merged.

Deliver the literal routes, DTO/fixtures, creation-cohort outcomes and rates, unwindowed latest failure, recoverable-work figure, docs/specs/tests.

Acceptance: examples 1 to 4 hold, and every item in §5 is pinned.

Plan evidence is required: on #4684's 100k-run seed (or an equivalent stated seed), attach `EXPLAIN (ANALYZE, BUFFERS)` for the owner and admin Reliability reads and the latest-failure lookup, including generic prepared plans, and measured request/runtime impact. Index only demonstrated gaps; use `CREATE INDEX CONCURRENTLY` in a NO TRANSACTION migration, dropping an invalid owned copy first as in `00283`. No allow-missing.

### Landing obligations (not milestones)

Before merge, the exact PR head must pass full `test:api`, `gate:repo`, `scan:secrets`, `sast:semgrep` and the LiveDB sweep in PR CI. Report the measured change in CI job time.

## 7. Decision Log

| Decision | Reason; rejected alternative |
|---|---|
| Reuse #4684's boundary unchanged | One admission, snapshot, window and filter contract; reject a parallel implementation. |
| Creation cohorts, not finish-time activity | Same default as Spend; reject mixed cohort definitions across sections. |
| Exclude chat, judge and cross_check | They are supporting work, not delivery outcomes; reject counting their failures as delivery failures. |
| Reuse the existing outcome classification, incl. `base_diverged` | One definition across endpoints; reject a second failure taxonomy. |
| Unwindowed latest failure | It answers "how long since the last failure", which a window would hide. |
| Own query-plan evidence | New reads, new plans; reject assuming #4684's plans cover them. |
| Admin all-users `window=all` is refused (400) on Reliability too | One window contract with Options and Spend (#4684 owner decision); reject a section-specific window set. |
| Primary-model fold gated by `breakdown=primary_model` and the current window | Other breakdowns and previous-window rows never read the model; reject paying the run_usage fold for them (measured: about half the cohort time). |
| Latest failure is lifetime with no creation cutoff, read in the same snapshot as the cohort | Preserves the existing `SelfRunOutcomes` strict-after semantics; a run created after the cohort's `now` may appear there but not in the counts, by design. |
| Latest failure also returns `user_id` | Owner route: always the caller; admin: an id the admin already sees via `breakdown=user`. Additive, not a leak. |
| No `runs_terminal_end_idx` migration | §8: it saves about 90 ms of an 8 s budget on the all-users latest-failure lookup and regresses the repo-filtered all-users lookup (2 ms to 20 ms); reject indexing without a measured bottleneck. |

## 8. Measurement evidence

Harness: `api/internal/store/analytics_evidence_livedb_test.go` (build tag `analyticsevidence`, `UZI_ANALYTICS_PLAN_EVIDENCE=1`), PostgreSQL 17.12, #4684's seed: 120,000 runs, 846,668 run_usage rows, 1,250 repos, 200 users. A Reliability step then rewrites 35% of delivery `agent_failure` runs to human-landable origins: 2,712 landable failures (569 with an available capture only, 474 with a preserved patch only, 222 with both, 1,447 with neither), 791 available and 269 expired captures, 4,686 requeued runs. 20 measured executions per workload; Reliability measured with `jit = off` as in production; warm cache.

Cohort statement, median ms (custom / generic plan), and full request through `RunAnalyticsSnapshot` (authorize reads, history start, cohort, latest failure; 8 s deadline), median / p95 ms:

| Workload | Breakdown | SQL custom / generic | Request median / p95 |
|---|---|---|---|
| owner 30d | primary_model | 105 / 144 | 156 / 164 |
| owner 30d | harness | 45 / 85 | 98 / 102 |
| owner all-time | primary_model | 937 / 1316 | 1282 / 1319 |
| owner all-time | harness | 354 / 708 | 736 / 753 |
| admin all-users 30d | primary_model | 563 / 574 | 734 / 776 |
| admin all-users 30d | harness | 196 / 206 | 347 / 358 |
| admin all-users 30d | user | 214 / 228 | 375 / 420 |
| admin all-users 90d | primary_model | 886 / 936 | 1063 / 1088 |
| admin all-users 90d | harness | 348 / 378 | 529 / 541 |
| admin user=<uuid> all-time | primary_model | 897 / 1294 | 1294 / 1322 |
| admin user=<uuid> all-time | harness | 350 / 686 | 719 / 726 |
| owner 30d, repo filter | primary_model | 7.5 / 7.2 | 31 / 32 |
| admin all-users 30d, repo filter | primary_model | 7.8 / 18.6 | 37 / 38 |

Latest failure, median ms (custom / generic): owner full 1.3 / 4.8 (row lookup 0.6 / 4.7, strict-after count 0.7 / 0.1, via `runs_user_terminal_end_idx`); all-users full 86 / 85 (row lookup 37 / 42, count 35 / 38; sequential scans of runs).

Index candidate `runs_terminal_end_idx ON runs ((COALESCE(finished_at, status_since)), id) WHERE status IN ('completed','failed','cancelled')`: all-users latest failure 86 to 1.1 ms; admin 30d request 734 to 625 ms; no cohort plan uses it; the repo-filtered all-users latest step in the request path regresses 2.1 to 20.4 ms (generic plan choice). Not added (Decision Log).

Worst case: admin `user=<uuid>` all-time primary_model, 1338 ms max, about 6.7 s headroom; no Reliability workload timed out. Owner all-time shapes spill sorts to temp (about 3.4k blocks). The harness also showed #4684 Spend admin shapes above 8 s when measured with the server-default JIT; production disables JIT in the snapshot, so that is not a Reliability finding. Generic-plan guards: `TestAnalyticsReliabilityGenericPlanLiveDB` and `TestAnalyticsLatestFailureGenericPlanLiveDB`.

**UNVERIFIED:** production-sized latency beyond this seed (the all-users latest-failure scan grows linearly with `runs`), and CI job-time impact (landing obligation).
