# PRD #4731: Scoped run diagnostics API: Capacity

**Issue:** #4731.
**Status:** Implemented (2026-11-26). Owner decisions on plan review: a gauge timeout or the snapshot deadline fails the whole request with 503; the admin vault observation is non-blocking.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Read-only API, DTO contracts, docs and specs. Additive indexes only when supported by plans. No lifecycle, tracking, interface or CLI changes. Implementation and validation leave `.github/workflows/**` untouched.
**Evidence baseline:** `5208f3e0dfc96e8bdd0218ca49d9bbb38ee53151`. Assign migration numbers at landing.

## Progress

- [x] M1 owner/admin Capacity diagnostics

## 1. Problem

Retained run timestamps and current provider meters offer different evidence about work pressure. Current worker counts cannot establish historical capacity.

## 2. Outcome

Owner and admin Capacity reads give approximate average concurrent ordinary-work intervals, a creation-cohort usage-wait measure and current owner/provider gauges. Historical slots, utilization, parked duration and provider headroom return explicit unavailable reasons.

1. An ordinary run that began before the window contributes only its overlap. Tied starts and ends do not change the integral. A partial local calendar bucket uses its actual duration.
2. A run with `limit_wait_count=2` contributes one run to the usage-wait numerator whether or not it is still parked.
3. A Codex login with multiple enabled aliases contributes one account reading. A request with repo or kind selectors shows their effective scope for occupancy and explicitly reports that those selectors do not partition shared provider readings.

## 3. Out of scope

New sampling or retention, historical utilization or slot denominators, exact active execution or worker occupancy, provider requests, and changes to claim, wait, recovery or provider polling.

Reading provider usage-limit history (#4692) or worker capacity samples (#4693) is out of scope, including if those capture changes land first. `not_tracked` describes what this endpoint reports.

## 4. Modules and seams

### Routes and shared read boundary

Add literal owner `/api/analytics/capacity` and admin `/api/admin/analytics/capacity` GET routes. Wire them through #4684's authentication, `RequireAdminRO`, distinct owner and admin-all query shapes, and route-owned repo visibility. Add both `noLimiter` mount rows. Use the existing options endpoint. Accept #4684's `window`, `tz`, `repo`, `harness`, `kind` and admin `user` selectors with the same validation and errors. Use section key `capacity` for admission. Unspecified parameters and grouping controls are unsupported.

For admin all-users requests, omitted `user` and explicit `user=all` both return HTTP 400 with `window all not available for all users` when `window=all`. Options omits `all` for that scope. The owner route and admin `user=<uuid>` still accept `window=all`.

Reuse #4684's analytics package and shared read boundary. Use `analytics.Admission.Acquire` for per-caller and process-wide admission, `store.RunAnalyticsSnapshot` for the snapshot, `analytics.ComputeBounds` with the captured Go clock for calendar intervals, and `apitypes.AnalyticsEnvelopeDTO` for the common envelope. Use its half-open Go-computed boundaries and 104-bucket all-time cap. SQL does no independent calendar generation or clock reads. Apply the shared authority rules before aggregating. Repo visibility comes from the mounted route. The owner route masks inaccessible repo IDs and paths. Admin routes may reveal repo IDs and paths on authorized rows, including selected-user results. Return explicit `time_basis`, applicable population, observed and unknown counts where meaningful, and reasons for unavailable measures. Live measurements disclose their effective filters. Include empty buckets where the shared calendar emits them.

### Approximate concurrency and usage-wait counts

- Use one retained interval per applicable run from `first_started_at` to its retained terminal end (`COALESCE(finished_at,status_since)`) or captured now while nonterminal. This is an elapsed envelope that can include waits and cannot recover claim or resume history. Report missing, invalid and legacy coverage limitations. Exclude chat and durable cross-check lane activity using the existing ordinary-lane predicate as the reference (`kind <> 'chat' AND NOT cross_check_lane`, `runtime.sql`; lane rules in `00313`). Retained lane assignment can be reset or absent, so disclose its historical limits and avoid inferring a past lane from current workers.
- For each valid interval intersecting the range, clip to the range, emit +1 and -1 endpoints, combine ties, compute running occupancy and integrate per actual bucket duration. Include bucket edges in the sweep. Do one sort-based sweep across endpoints and bucket boundaries, with no interval self-join or per-bucket rescan of the run population. The unit is `average_concurrent_runs`, with no slot denominator, percentage or 100% clamp. Keep zero-duration buckets explicit with a reason and no division. Label this series by interval overlap.
- Separately count creation-cohort runs with `limit_wait_count>0` over applicable runs, and return count, n and share. Use the retained per-run counter (`00091`), with no claim about episode timestamps or current parked population. Keep historical provider headroom, slot counts, utilization and parked duration null with `not_tracked`.

### Current provider gauges

- Use current accepted Anthropic readings through the enabled-token and enablement-revision joins in `anthropic_rate_limits.sql` (`ListRateLimitsForUser`, `ListRateLimits`). Use the canonical linked-account and enabled-alias signature rules in `codex_rate_limits.sql` (`GetCodexAccountRateLimitsForUser`, `ListCodexAccountRateLimits`). Missing or obsolete evidence stays visible through the existing unavailable classifications. Reuse the status logic in `handler/ratelimits.go`, `handler/codex_ratelimits.go` and `usagereserve.CodexMeterStatus` with the request's captured clock. The live vault gate is separate state. Reuse safe metadata lookup where required for classification, and disclose that it is a current observation rather than part of the historical database snapshot. An unavailable gauge leaves otherwise valid occupancy data intact.
- Gauge queries use the response's owner/provider authority, independent of historical window. `harness` chooses the provider; `repo` and `kind` are unsupported attribution dimensions for shared quotas. Return that effective scope on the gauge component. No provider call, credential refresh, cross-user caches or summing percentages. Return existing safe identity metadata only, never secret values, raw provider identifiers or poll-error text. Use private/no-store response semantics.
- Cap identity rows at 200 per provider with stable ordering and explicit completeness metadata. Apply the limit to unique enabled identities before expanding display metadata, including on instance-wide reads. Keep multiple aliases on one canonical Codex account from multiplying readings. Distinguish a proven empty set from a failed or unavailable gauge and from a truncated set.

Pin the DTO in recorded Go JSON fixtures and update existing TypeScript contract mirrors where required. Document the endpoint in `docs/` with title/order/audience frontmatter, use `task docs:sync` for the generated embed, and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`. Leave `specs/ai.md` frozen. Keep the existing endpoints and shared #4684 contract intact.

## 5. Testing decisions

- Pin interval overlap for pre-window runs, simultaneous events, gaps, partial or zero-duration buckets, coarsened buckets, DST and mixed lane evidence. Test invalid and missing starts, missing end evidence, and ongoing envelopes spanning waits. Confirm the ordinary-lane population and that no historical calculation consults current worker slots.
- Verify counter versus episode semantics, zero denominator, ownership and repo/harness/kind selectors. Test both gauge authorities, unsupported attribution metadata, no live provider calls, stale/disabled/re-enabled readings, canonical alias dedup, truncated instance scope and a gauge error that preserves valid occupancy.
- Put LiveDB cases in `store` and `handler`. Use #4684's actual-router auth, owner isolation and admin-selected scope fixtures, plus its snapshot barrier, timeout, single-flight and cancellation patterns. Test both routes with a cookie admin, an admin-read bearer and an ordinary bearer owned by an admin. Check release on every exit and SQLSTATE 57014 mapping. Pin session-TimeZone independence and local DST and all-time boundaries where used. Through the router, test identical `window=all` refusal for omitted `user` and explicit `user=all` on the admin route, omission of `all` from admin all-users Options, and acceptance of `window=all` on the owner and admin-selected-user routes.
- Name LiveDB tests with the `LiveDB` suffix. Observe targeted regression mutations fail and restored code pass.
- **In-run checks** include `task check:api`, affected `go test -race -count=1` packages, and named PostgreSQL 17 LiveDB cases with RUN and PASS evidence and zero skips (Docker required). Run `task check:migration-numbering` and `task check:migration-additive` if adding an index. Run `task docs:sync`, `task check-docs:web`, and `task check:changelog-entry` with one lowercase UUID-v4 fragment. Sweep changed contract literals and identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`; run affected `task test:e2e-lib` / `task test:forge-fake` and name unrun phases.

## 6. Milestones

### M1. Owner/admin Capacity diagnostics

Deliver the routes, overlap sweep, current gauge queries, DTO fixtures, docs and tests. Acceptance is the examples and evidence rules above, including bounded historical aggregation and current gauges with explicit effective scope.

Use #4684's 100k-run synthetic seed or an equivalent, stating owner skew, range selectivity and fresh statistics. Capture owner and admin `EXPLAIN (ANALYZE, BUFFERS)` including generic prepared plans and measured request impact. Index only demonstrated gaps, following `00283` for concurrent builds in a NO TRANSACTION migration and invalid owned index cleanup. No allow-missing. Include realistic overlap and identity cardinality. Show that work scales as a sort over endpoints and bucket edges and avoid quadratic interval/bucket or interval/interval joins.

### Landing obligations (not milestones)

Before merge, the exact PR head must pass full `test:api`, `gate:repo`, `scan:secrets`, `sast:semgrep` and the LiveDB sweep in PR CI. Report measured affected-job time and critical-path impact, separating cache, queue and retry effects where observable.

## 7. Decision Log

| Decision | Reason |
|---|---|
| Reuse #4684's scope, clock, snapshot and envelope | Current gauge scope and historical overlap are explicit parts of one read contract. |
| Sweep retained intervals without a denominator | A single interval is approximate and current slots cannot describe historical capacity. |
| Separate current readings by owner and provider | Shared quotas cannot be attributed to an individual repo or run kind. |
| Count runs with a usage-wait counter | Retained counters show entry into wait but cannot place individual episodes in time. |
| A gauge statement timeout (57014) or the snapshot deadline fails the whole Capacity request with 503, like any analytics timeout (owner decision on plan review); only non-timeout gauge errors with a live snapshot leave occupancy intact. | Retrying a 503 is the same contract as every other analytics timeout; a savepoint keeps occupancy only for ordinary gauge errors. |
| The admin vault observation is non-blocking (`TryLock`) with a 2 s budget checked between and after lookups (reads run under the budget's context, so they are cancelled when it expires; a step that does not honour cancellation can still finish after it, so this is not a hard 2 s cap) and the owner route uses `RequiresUnlock` (owner decision on plan review). | A user's lifecycle operation must not stall an instance-wide read; contention, a read error or the budget gives `vault_status_unavailable` on the admin routes only, while the owner route fails closed (`vault_locked`). |
| No index added. | No demonstrated gap within the 8 s budget (evidence below). A terminal-end index cut admin 7d to 35 ms but barely moved 90d. |

**Measured (Nov 2026).** #4684's 120k-run seed, fresh `ANALYZE`, top owner 20.1% of runs. Statement times, custom / generic plan: owner 30d 27 / 26 ms; owner all-time 206 / 225 ms; admin all-users 7d 110 / 60 ms, 30d 232 / 131 ms, 90d 270 / 177 ms; admin 30d with `repo` 3.9 / 48.8 ms; admin all-time 1127 ms with a 42 MB disk sort (refused by the API, so not reachable). Owner paths use per-user indexes only: `runs_user_terminal_end_idx`, `runs_user_nonterminal_idx` and `runs_user_created_idx` (usage-wait scan) at 30d; `runs_user_terminal_end_idx`, `runs_user_nonterminal_idx` and `idx_runs_user` all-time (custom plan; the generic plan scans `runs_user_created_idx` for usage wait instead). The admin all-users custom plans at 30d, 90d and all-time sequentially scan `runs` twice. "Scales linearly with total runs, about 0.19 s per 100k runs at 30d" is inferred from this single 120k seed, not from a size sweep. Gauge statements took 1.2-30 ms at the 201-row capped fetch (604 enabled Anthropic and 443 enabled Codex identities seeded, so the cap was hit).

**Selectivity and sweep sizes (Nov 2026).** Same seed. "Candidates" is the statement's candidate set (`pop`: terminal runs ending after `from` plus every nonterminal run created before `now`, per scope and selectors) over all runs of the scope; "events" is an upper bound on the sorted endpoint rows (two per interval plus two per bucket, from `with_interval` and the bucket count), not a measured row count, because endpoints outside the window may not be emitted. Owner (24,162 runs) 30d: 2,743 candidates (11.35%), at most 4,778 events; owner all-time: 23,672 candidates (97.97%), at most 46,734. Admin all-users (120,000 runs) 7d: 4,412 candidates (3.68%), at most 5,306; 30d: 13,368 (11.14%), at most 23,264; 90d: 19,292 (16.08%), at most 35,232; 30d with `repo`: 25 (0.02%), at most 104; all-time: 117,550 (97.96%), at most 231,726 (the case the API refuses). The log lines are `CAPACITY SELECTIVITY` in the evidence run; the harness now labels the event figure `sweep_events_upper_bound`.

**Request timings (Nov 2026).** The same seed rebuilt in a private database, `TestAnalyticsCapacityRequestEvidenceLiveDB` (build tag `analyticsevidence`, `UZI_ANALYTICS_PLAN_EVIDENCE=1`): full requests through the mounted router with a real pool and vault, the seed's clock, one warm-up then 20 requests each, every response 200, one client on a local Postgres container. Median / p90: owner Capacity 30d (top owner) 58.8 / 62.6 ms; admin all-users Capacity 7d 123 / 174 ms, 30d 197 / 200 ms, 90d 245 / 251 ms; admin all-users Spend with comparison 30d 1.54 / 1.58 s, 90d 2.56 / 2.60 s. Capacity at 90d is therefore about a tenth of the Spend request it sits beside. These include the gauge reads and the Codex vault observation.

**LiveDB suite time.** From the gate logs: the 14 handler Capacity tests that existed then (three more were added later and are not in this figure) took 20.0 s of a 67.8 s handler package run (at 5436bdc75), and the 32 store tests (Capacity, savepoint, plan guard and unit tests) 28.3 s of a 40.5 s store package run (at d738e4eea), 48.3 s together; the final repair commit added three handler LiveDB tests (about 5.3 s locally) and changed some handler and store tests, and these figures were not re-measured after it; the two plan-guard tests are 10.5 s of that. The `TryStatus` tests are plain unit tests in the vault package and are not in those logs.

**Not measured:** CI job time on the pipeline runner (a landing obligation, read from the exact-head run), concurrent load or admission contention, a size sweep, and a contended or locked vault.
