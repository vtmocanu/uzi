# PRD #4729: Scoped run diagnostics API: Flow

**Issue:** #4729.
**Status:** Ready (#4684 merged).
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Read-only API, DTO contracts, docs and specs. Additive indexes only when supported by plans. No lifecycle, tracking, interface or CLI changes. Implementation and validation leave `.github/workflows/**` untouched.
**Evidence baseline:** `5208f3e0dfc96e8bdd0218ca49d9bbb38ee53151`. Assign migration numbers at landing.

## Progress

- [ ] M1 owner/admin Flow diagnostics

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

**Unverified before implementation**. Retained legacy evidence coverage, plans, latency and suite impact.
