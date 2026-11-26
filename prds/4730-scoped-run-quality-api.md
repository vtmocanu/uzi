# PRD #4730: Scoped run diagnostics API: Quality

**Issue:** #4730.
**Status:** Ready (#4684 merged).
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Read-only API, DTO contracts, docs and specs. Additive indexes only when supported by plans. No lifecycle, tracking, interface or CLI changes. Implementation and validation leave `.github/workflows/**` untouched.
**Evidence baseline:** `5208f3e0dfc96e8bdd0218ca49d9bbb38ee53151`. Assign migration numbers at landing.

## Progress

- [ ] M1 owner/admin Quality evidence

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

**Unverified before implementation**. Legacy rework association completeness, plans and latency. Passing tests do not establish retention completeness.
