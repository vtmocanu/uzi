# PRD #4685: Scoped run outcomes API: Reliability

**Issue:** #4685.
**Status:** Ready once #4684 merges.
**Priority:** Normal.
**Blocked by:** #4684 (scoped run usage API: filter options and Spend).
**Scope:** API, DTO contracts, docs and specs. Additive index migrations only if measured plans demonstrate a gap. No user-interface or command implementation. No changes under `.github/workflows/**`, including validation.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`; re-verify against #4684's merged code before planning. Migration numbers are assigned at merge time.

## Progress

- [ ] M1 owner/admin Reliability and lifetime recency

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

**UNVERIFIED before implementation:** latency and query plans of the new reads. These require the stated offline measurements; no external lookup is left to the implementation worker.
