# PRD #4729: Scoped run diagnostics API: Flow

**Issue:** #4729.
**Status:** Done (#4729).
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Read-only API, DTO contracts, docs and specs. Additive indexes only when supported by plans. No lifecycle, tracking, interface or CLI changes. Implementation and validation leave `.github/workflows/**` untouched.
**Evidence baseline:** `5208f3e0dfc96e8bdd0218ca49d9bbb38ee53151`. Assign migration numbers at landing.

## Progress

- [x] M1 owner/admin Flow diagnostics (LiveDB store and handler tests ran with 0 skips; the 120k evidence harness ran, including a DB-side request-path phase; HTTP, auth middleware, admission, in-snapshot authorization, the DTO mapping, DTO JSON encoding and network were not measured, see section 8)

## 1. Problem

Run rows show the current state and some timestamps. They do not describe which work is waiting now, how much work finished over time, or where delay appears in a creation cohort.

## 2. Outcome

Owner and admin Flow reads report live waits, activity, timing and delivery evidence. Each measure carries its time basis and evidence limits.

1. A run created before the window and finished within it contributes to finished-by-end-day. An old run still awaiting approval contributes to Waiting now.
2. A uniquely matched applied human approval contributes to response timing. An ACK-only, superseded, or unbound input does not. Auto-approval configuration remains distinct from observed approval.
3. An issue-run creation bucket with three finished runs contributing valid durations returns `n=3` and suppressed percentiles. Any unfinished issue run in the bucket sets `incomplete`, even for an old bucket.

## 3. Out of scope

Historical lifecycle capture, exact queued or running-state time, exact execution time, actual PR merge dates, new runs-list filters, and changes to workflow execution.

## 4. Modules and seams

### Routes and shared read boundary

Add literal owner `/api/analytics/flow` and admin `/api/admin/analytics/flow` GET routes. Wire them through #4684's authentication, `RequireAdminRO`, distinct owner and admin-all query shapes, and route-owned repo visibility. Add both `noLimiter` mount rows. Use the existing options endpoint. Accept #4684's `window`, `tz`, `repo`, `harness`, `kind` and admin `user` selectors with the same validation and errors. Use section key `flow` for admission. Unspecified parameters and grouping controls are unsupported.

For admin all-users requests, omitted `user` and explicit `user=all` both return HTTP 400 with `window all not available for all users` when `window=all`. Options omits `all` for that scope. The owner route and admin `user=<uuid>` still accept `window=all`.

Reuse #4684's analytics package and shared read boundary. Use `analytics.Admission.Acquire` for per-caller and process-wide admission, `store.RunAnalyticsSnapshot` for the snapshot, `analytics.ComputeBounds` with the captured Go clock for calendar intervals, and `apitypes.AnalyticsEnvelopeDTO` for the common envelope. Use its half-open Go-computed boundaries and 104-bucket all-time cap. SQL does no independent calendar generation or clock reads. Apply the shared authority rules before aggregating. Repo visibility comes from the mounted route. The owner route masks inaccessible repo IDs and paths. Admin routes may reveal repo IDs and paths on authorized rows, including selected-user results. Return explicit `time_basis`, applicable population, observed and unknown counts where meaningful, and reasons for unavailable measures. Live measurements disclose their effective filters. Include empty buckets where the shared calendar emits them.

### Flow measures

| Measure | Contract |
|---|---|
| Waiting now | Count current queued, awaiting-approval/input/follow-up and applicable paused, limit-wait, pool-wait and recovery-wait runs. Return counts and oldest `status_since` age by current state. Use captured now, ignore the window and retain authorized repo/harness/kind scope. |
| Created and finished activity | Count created at `created_at`. Count currently terminal work at `COALESCE(finished_at,status_since)` under #4684's `run_outcome_classes` view in `00542_run_usage_model_totals.sql`. Select finished rows by end timestamp independently of creation date and label the series `time_basis=finished_at`. |
| Approximate first-start delay | `first_started_at - created_at` for nonnegative scoped creation-cohort pairs, p50/p90 and evidence counts. Holds before first execution are included. Migration `00280` backfilled from a potentially later-leg `started_at`; report that limitation. |
| Approximate human plan response | Duration from a uniquely matched, retained presentation or revision-bound plan frame to the applied human input's `created_at`. Report p50/p90, n and unknown. Measures response to the observed gate evidence. |
| Selected delivery funnel | Restrict the creation cohort to `issue` and `self_improve`. Count created, plan settled, completed, PR present and currently merged. Expose skipped, rejected, failed, cancelled, unknown and in-progress branches explicitly. When evidence cannot establish that planning was skipped, report unknown. |
| Issue-run elapsed | For terminal issue runs in the creation cohort, use `COALESCE(finished_at,status_since) - first_started_at` for valid nonnegative pairs. Return a fixed, bounded duration histogram and p50/p90 with n for the distribution, plus p50/p90 and n by creation bucket. Duration includes held time. |
| Approval classification | For the applicable plan cohort, classify proven applied human approval, auto-approval configured, or unknown. Human evidence takes precedence. Configuration alone provides no event or timestamp. |

Each elapsed bucket reports unfinished issue-run count, cohort `incomplete`, and the independent calendar `partial` marker. Suppress per-bucket percentiles with fewer than five valid finished observations and retain n and a reason. Calculate percentiles directly from each bucket's population, including coarsened all-time buckets. Report invalid or missing pairs separately from unfinished rows.

Reuse #4684's scoped merge witnesses, connection-local `(repo_id,mr_iid)` identity and earliest matching cohort attribution. Window, harness and kind filters select the PR cohort. Merge witnesses may come from outside those filters. Owner and admin-selected-user requests require a witness from the same user as the scoped cohort. Run stages count runs. Report distinct PR units at PR stages separately. If the kind selector rules out issue and self-improve, the selected delivery funnel is not applicable.

### Gate evidence

Start with `run_gate_presentations` (migration `00260`, unique `(run_id,revision)`) and `run_user_inputs`. Accept only an `approve_plan` with positive bound revision, application evidence from `applied_at`, and an eligible disposition. The normal `ApplyRunInputRows` path can leave disposition null. `AckRunInputRows` only acknowledges delivery, and `DiscardRunInputRows` can set `applied_at` on a superseded approval. Exclude superseded, declined, advisory, ACK-only and ambiguous evidence. Validate chronology and never match by revision across runs.

A presentation row can be inserted when an id-less gate is adopted after original publication. Describe what a retained timestamp measures. If the retained evidence cannot establish an eligible presentation timestamp, report unknown. If using message fallback, require an independently proven revision match and one indexed correlated lookup within set-based SQL. `idx_run_messages_plan_seq` (migration `00283`) scopes by `(run_id,seq)` for `plan` and `plan_revising`. It does not establish a message's revision. Skip fallback where there is no reliable revision evidence. Avoid global message scans and per-run application queries.

Pin the DTO in recorded Go JSON fixtures and update existing TypeScript contract mirrors where required. Document the endpoint in `docs/` with title/order/audience frontmatter, use `task docs:sync` for the generated embed, and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`. Leave `specs/ai.md` frozen. Keep the existing endpoints and shared #4684 contract intact.

## 5. Testing decisions

- Pin old live waits, pre-window creation with in-window finish, terminal and nonterminal outcomes, enqueued and parked states, missing or negative durations and proved versus unknown planning branches. Test PR deduplication, scoped witnesses and stale state on a secondary run. An out-of-filter witness from the scoped user establishes a merge. A witness from another user alone does not establish a merge in either owner or admin-selected-user scope.
- Gate fixtures cover revision replacement, same revision numbers on different runs, late adoption, invalid chronology, normal applied receipts with null disposition, superseded rows with `applied_at`, ACK-only and human evidence taking precedence over configuration. Check the presentation-table or indexed lookup plan.
- Test elapsed histogram population, per-bucket valid n, suppression, old and new incomplete cohorts and direct recomputation after all-time coarsening.
- Put LiveDB cases in `store` and `handler`. Use #4684's actual-router auth, owner isolation and admin-selected scope fixtures, plus its snapshot barrier, timeout, single-flight and cancellation patterns. Test both routes with a cookie admin, an admin-read bearer and an ordinary bearer owned by an admin. Check release on every exit and SQLSTATE 57014 mapping. Pin session-TimeZone independence and local DST and all-time boundaries where used. Through the router, test identical `window=all` refusal for omitted `user` and explicit `user=all` on the admin route, omission of `all` from admin all-users Options, and acceptance of `window=all` on the owner and admin-selected-user routes.
- Name LiveDB tests with the `LiveDB` suffix. Observe targeted regression mutations fail and restored code pass.
- **In-run checks** include `task check:api`, affected `go test -race -count=1` packages, and named PostgreSQL 17 LiveDB cases with RUN and PASS evidence and zero skips (Docker required). Run `task check:migration-numbering` and `task check:migration-additive` if adding an index. Run `task docs:sync`, `task check-docs:web`, and `task check:changelog-entry` with one lowercase UUID-v4 fragment. Sweep changed contract literals and identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`; run affected `task test:e2e-lib` / `task test:forge-fake` and name unrun phases.

## 6. Milestones

### M1. Owner/admin Flow diagnostics

Deliver the routes, queries, DTO fixtures, documentation and tests. Acceptance is the examples and evidence rules above, including independent time populations and scoped PR evidence.

Use #4684's 100k-run synthetic seed or an equivalent, stating owner skew, range selectivity and fresh statistics. Capture owner and admin `EXPLAIN (ANALYZE, BUFFERS)` including generic prepared plans and measured request impact. Index only demonstrated gaps, following `00283` for concurrent builds in a NO TRANSACTION migration and invalid owned index cleanup. No allow-missing. Include realistic presentation/message densities and show that the gate path avoids a global message scan.

### Landing obligations (not milestones)

Before merge, the exact PR head must pass full `test:api`, `gate:repo`, `scan:secrets`, `sast:semgrep` and the LiveDB sweep in PR CI. Report measured affected-job time and critical-path impact, separating cache, queue and retry effects where observable.

## 7. Decision Log

| Decision | Reason |
|---|---|
| Reuse #4684's read boundary and calendar | The sections share scope, time and snapshot rules. |
| Separate current, creation and finish-time measurements | Each measure identifies its own population. |
| Require applied revision-matched human evidence | Stale, ACK-only and unmatched records cannot prove an approval. |
| Publish elapsed cohorts with n and incomplete markers | Unfinished runs and missing durations affect interpretation. |
| Per-measure populations | Waiting, created, first-start and plan response count chat where the kind selector includes it (chat runs are probed for gate evidence in plan response when chat is among the selected kinds; the funnel and approval stay `issue` and `self_improve` only); finished reads `run_outcome_classes` and excludes chat, which has no outcome class; the funnel and approval cover `issue` and `self_improve`, elapsed covers `issue`. Blocks are independent and must not be summed against each other. |
| No `run_messages` fallback for plan response | `idx_run_messages_plan_seq` scopes by `(run_id,seq)` but a message carries no revision, so no revision match can be proven. Without a retained presentation row the pair is unknown (`no_presentation`), not guessed. No part of the statement reads `run_messages`. |
| Approval evidence rules | Eligible receipt: `approve_plan`, `gate_binding = 'bound'`, `gate_revision > 0`, `applied_at` set, disposition null or `applied`, `created_at <= applied_at`. Ambiguity counts eligible receipts only (a superseded, declined or advisory row never makes a pair ambiguous). Human proof is exactly one eligible receipt for the run's current `gate_revision`; an earlier-revision approval alone is `historical_approval_only`. Legacy (`NULL` binding) and `unbound` receipts never prove anything. Legacy disclosure follows the approved implementation plan (revision 5, measures 4 and 5): a run with a `NULL`-binding or `unbound` `approve_plan` receipt counts once in plan response `unknown.legacy_unbound` (every selected kind, the gate-evidence cohort) and, when it is an `issue` or `self_improve` run, once in the funnel `disclosure.legacy_evidence`, even when the run also has timed pairs or current human proof. An unbound receipt never approves a pair; a pair with no bound, applied receipt at all is `not_approved` (one whose bound, applied receipts all fail eligibility is `invalid_chronology` or `excluded_disposition`). Approval classification still ranks human, `auto_approve_configured`, `ambiguous`, then `legacy_unbound`, so a run with human proof is `human` there yet still disclosed as legacy. The disclosure counts are not additive. Reject treating configuration, ACK-only or superseded `applied_at` rows as proof. |
| Planning-branch precedence | rejected, settled_human, settled_auto, skipped, in_progress, failed, cancelled, unknown, so each run lands in one branch and the branches sum to `stages.created`. `settled_auto` requires `auto_approve`, `gate_revision = 0`, a plan and a non-seeded `plan_source`: the human gate clears `auto_approve`, so a run with `gate_revision > 0` and `auto_approve` set never reached the auto path. `skipped` is a seeded plan at revision 0. A completed run with neither proof is `unknown`, never assumed skipped. |
| PR units use earliest-run attribution | Distinct `(repo_id, mr_iid)` units are attributed to the earliest creation-cohort run (`created_at`, then `id`) and bucketed by that run's creation bucket, as in #4684. The merge witness is scoped to the response (same user for owner and admin `user=<uuid>`, any user for admin all-users) and ignores the window, harness, kind and repo filters. |
| Repo masking is vacuous for Flow | The document names no repo id or path, so there is nothing to mask on either route. The only repo shown is the caller's own validated `repo` selector echoed in `filters`. Reject emitting repo-keyed series. |
| Per-run `LATERAL` receipt aggregate | The first statements joined the funnel to a cohort-wide receipt aggregate; under `plan_cache_mode=force_generic_plan` that became a nested loop re-reading the aggregate per run and went quadratic (owner top all-time 12.8 s). The aggregate is now a `LATERAL` probe of `idx_run_user_inputs_run_id` per run; documents are identical (see section 8). Reject a cohort-wide `GROUP BY` CTE joined per run. |
| No index added | Candidates (`runs_terminal_end_idx`, `runs (created_at)`, both) moved admin all-users custom-plan statement medians under 8%, below the bar, so there is no migration. Generic-mode medians and buffers for the candidates were not recorded in this PRD (raw logs are not kept in the repository), and request-path timing was not run with candidate indexes installed (section 8). |

**Verified after implementation.**

- The Flow statements are set-based and bounded under both custom and generic plans: no `run_messages` access, and no Seq Scan on `run_user_inputs` or `run_gate_presentations` in any measured plan (the harness asserts no `run_messages` access and no such Seq Scan; `TestAnalyticsFlowGenericPlanShapeLiveDB` asserts only that the receipt CTEs are not re-read per outer row under a generic plan).
- Owner and admin Flow LiveDB cases in `store` and `handler` ran with 0 skips.
- Local DST boundaries: `TestAnalyticsFlowLocalDSTBoundariesLiveDB` (`api/internal/store/analytics_flow_livedb_test.go`) runs in `America/New_York` and covers the 23-hour spring-forward day 2033-03-13, the 25-hour fall-back day 2033-11-06 and a week-coarsened `window=all` spanning several transitions. A mutant that passes UTC-computed bucket arrays fails all three subtests.
- Handler timezone wiring: `TestAnalyticsFlowLocalTimezoneLiveDB` (`api/internal/handler/analytics_flow_livedb_test.go`) requests `tz=America/New_York` through the real router over the 2047-03-10 spring-forward day, asserts `timezone`, the local-midnight bucket starts (including the 23-hour bucket) and that a run created at 23:30 local, already the next UTC day, lands in the local day's created cell. A mutant passing `time.UTC` to `analytics.ComputeBounds` in the Flow handler fails it.
- Statement-level timings and DB-side request-path timings on the 120k seed are in section 8.
- Cross-check repair tests (LiveDB, `api/internal/store/analytics_flow_livedb_test.go`, `analytics_flow_scope_livedb_test.go`, `analytics_flow_evidence_livedb_test.go` (the opt-in harness, build tag `analyticsevidence` plus `UZI_ANALYTICS_PLAN_EVIDENCE=1`, where the guards live), `api/internal/handler/analytics_flow_livedb_test.go`):
  - `TestAnalyticsFlowGateEvidenceLiveDB` includes a revision-0 case with only a `NULL`-binding applied receipt (legacy, never human), and cases where an unbound receipt beside a timed pair or a presentation still discloses legacy evidence.
  - `TestAnalyticsFlowSelfImproveSettledAutoLiveDB`: a `self_improve` run with `auto_approve` at revision 0 and an agent plan is `settled_auto`.
  - `TestAnalyticsFlowChatInPlanResponseOnlyLiveDB`: a chat run with a presentation and an applied bound receipt is measured by plan response when chat is selected, and is absent from the funnel and approval.
  - `TestAnalyticsFlowSelectedUserWitnessLiveDB`: through the real router, another user's merged witness does not mark an admin-selected user's PR merged; the same user's own out-of-window merged run (control) does.
  - The evidence harness now requires a `run_id`-conditioned index scan on `idx_run_user_inputs_run_id` for every `run_user_inputs` node in every plan, and the code fails an empty `UZI_EVIDENCE_ONLY` selection, logging partial selections (the output of a negative run showing this was not recorded).

**Still unverified.**

- End-to-end production request latency: the DB-side request path was measured (section 8), but HTTP, auth middleware, admission, in-snapshot authorization, the DTO mapping, DTO JSON encoding and network were excluded, and bounds were computed in UTC.
- Production table sizes and the per-run `run_user_inputs` fan-out: the seed has 3.1 inputs per run (373,446 over 120,000) and 0.45 `approve_plan` receipts per run, which may differ from production.
- Retained legacy evidence coverage in production (how many real runs have only `unbound` or `NULL`-binding receipts), and the suite and CI job-time impact, which is a landing obligation measured on the PR.

## 8. Measurement evidence

Method: throwaway PostgreSQL 17, `jit=off`, the exact sqlc statement text prepared on one connection, `now` fixed. Harness: `analytics_flow_evidence_livedb_test.go` under the `analyticsevidence` build tag with `UZI_ANALYTICS_PLAN_EVIDENCE=1`, committed as an opt-in harness (build tag plus env var), not a gate. Figures in the tables below are statement-level; the request-path phase is described after them. Raw logs were not kept in the repository.

**Measurement currency.** All timing tables below (statement, candidate and request-path) were measured on the Flow statements as they stood before the cross-check commit `a29204d28`. That commit added chat runs to the gate-evidence probe and removed the legacy pair/proof exclusions. After it, only a partial evidence run was made: `owner-top-30d` only, with the new per-node input-scan guard passing. The full workload matrix was not re-measured.

**Seed:** 120,000 runs, 200 owners (largest 20.1% = 24,162 runs, top 10 = 59.8%), 1,250 repos; 2.41% / 10.2% / 15.2% of runs in the last 7 / 30 / 90 days. `gate_revision` for issue and `self_improve` runs: 0 for 19,769, 1 for 29,175, 2 for 12,859, 3 for 5,481. `run_user_inputs`: 373,446 rows, of which `approve_plan` 53,851 (applied 33,306; applied with NULL disposition 5,647; superseded 9,444; ACK-only 2,259; legacy 1,866; unbound 1,329). `run_gate_presentations`: 71,336. `run_messages`: 2,462,718 (20.5 per run). `ANALYZE` run.

**Finding: generic plans went quadratic.** The first Flow statements joined the funnel (`fr`) to a receipt aggregate with a `LEFT JOIN`; under `plan_cache_mode=force_generic_plan` the planner chose a nested loop that re-read the aggregate once per run. Custom plans stayed under 700 ms, but generic plans (the steady state for prepared statements) did not:

| Workload | Generic median / p95 (ms) |
|---|---|
| Owner (top), all-time | 12,776 / 13,078 (over the 8 s `statement_timeout`) |
| Admin selected-user, all-time | 12,585 / 12,655 |
| Admin all-users 90d | 7,517 / 7,610 |
| Admin all-users 30d | 3,243 (median) |

**Fix:** a per-run `LATERAL` aggregate over `run_user_inputs`, probing `idx_run_user_inputs_run_id` once per cohort run. The documents are identical: custom-mode `md5` of the document matched on 11 workloads, and a temp-table `EXCEPT ALL` comparison over 20,000 runs and 60,000 inputs found 0 differences.

**Final timings** (10 runs, median / p95 ms):

| Workload | Custom | Generic |
|---|---|---|
| Owner (top) 30d | 85.0 / 86.6 | 81.8 / 84.9 |
| Owner (top) all-time | 665.3 / 678.1 | 665.3 / 694.0 |
| Admin all-users 30d | 398.6 / 407.2 | 388.3 / 415.3 |
| Admin all-users 90d | 560.6 / 574.0 | 577.2 / 592.5 |
| Admin selected-user all-time | 641.8 / 667.2 | 654.7 / 660.8 |

Median-owner workloads ran in 1-15 ms.

**Access paths.** Owner finished-range reads use `runs_user_terminal_end_idx` and creation-range reads use `runs_user_created_idx`. Admin all-users does a parallel Seq Scan of `runs` (about 123-127 ms, not the bottleneck). Presentations are read through `run_gate_presentations_run_id_revision_key` and inputs through `idx_run_user_inputs_run_id`, per run. No plan reads `run_messages` or Seq Scans `run_user_inputs` or `run_gate_presentations` (asserted).

**Candidate indexes** (admin all-users, custom-plan statement medians in ms). These are the only recorded candidate figures. In generic mode the planner picked the terminal-end candidate with no timing change; generic medians and buffers for the candidates were not recorded in this PRD (raw logs are not kept in the repository), and request-path timing was not run with candidate indexes installed. No index was committed.

| Candidate | 30d | 90d |
|---|---|---|
| none | 388 | 555 |
| `runs_terminal_end_idx` | 392 | 563 |
| `runs (created_at)` | 395 | 563 |
| both | 371 | 554 |

Every candidate stayed under the 8% bar on these custom-plan medians, so no index migration ships.

**Regression guard.** `TestAnalyticsFlowGenericPlanShapeLiveDB` fails on the old SQL (receipt CTE re-read per outer row) and passes on the shipped statement.

**DB-side request path.** A `request-path` phase of the harness times `store.RunAnalyticsSnapshot` (8 s `statement_timeout`, `jit=off`) around the history-start query, `ComputeBounds`, the Flow statement and `DecodeAnalyticsFlowDoc`, on the pool with pgx's default statement cache. The requests run on a warm pooled connection: the baseline phase (enabled, running first through the same pool and statement cache) had already prepared the Flow statement and settled its plan choice, and workloads that share a statement share one prepared statement. The history-start statements run once per built workload (all 11 are built before the `UZI_EVIDENCE_ONLY` filter): 9 owner and 2 all-users executions over the pool, so the all-users plan could still switch inside the sample and the owner plan's state on the request connection was not checked. Any generic plan in use is whatever Postgres auto mode chose. The table came from a filtered run of these 5 workloads with the default of 20 requests each (`UZI_EVIDENCE_RUNS` unset) and the baseline phase enabled. A timeout fails the test. Seed as above. Set `UZI_EVIDENCE_REQUEST=0` to skip the phase.

| Workload | First (ms) | Median / p95 (ms) | Max (ms) | Timeouts |
|---|---|---|---|---|
| owner-top-30d | 81.6 | 81.5 / 84.3 | 84.8 | 0 |
| owner-top-all | 647.6 | 641.4 / 652.4 | 655.0 | 0 |
| admin-all-30d | 410.5 | 410.5 / 419.9 | 430.1 | 0 |
| admin-all-90d | 573.7 | 589.1 / 597.9 | 601.1 | 0 |
| admin-user-all | 648.9 | 631.2 / 651.9 | 656.2 | 0 |

Bare-statement timings from the same run match within noise. Scope: the DB-side request path only. HTTP, auth middleware, admission (`deps.admission.Acquire`), in-snapshot authorization (tz-name check, `AnalyticsUserExists` for admin `user=<uuid>`, repo checks), the DTO mapping (`mapAnalyticsFlow`), DTO JSON encoding and network are excluded, and bounds are computed in UTC rather than the caller's zone.

**E2E sweep.** `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/` for `analytics/flow`, `AnalyticsFlow`, `insufficient_observations`, `legacy_unbound` and `plan_response` found no hits (re-run for the cross-check repair, same result), so no e2e update was needed and `task test:e2e-lib` / `task test:forge-fake` were not required. No E2E phase was run in-run: all 58 scripts in `e2e/phases/` were unrun (`00-preflight.sh` through `76-compose-web-dns.sh`; list with `ls e2e/phases`).

**Not measured:** end-to-end production request latency; production table sizes and `run_user_inputs` fan-out; `mr_rework` and `cross_check` mixes; suite-runtime and CI job-time impact (a landing obligation, measured on the PR).
