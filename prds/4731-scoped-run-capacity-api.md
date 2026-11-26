# PRD #4731: Scoped run diagnostics API: Capacity

**Issue:** #4731.
**Status:** Ready (#4684 merged).
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Read-only API, DTO contracts, docs and specs. Additive indexes only when supported by plans. No lifecycle, tracking, interface or CLI changes. Implementation and validation leave `.github/workflows/**` untouched.
**Evidence baseline:** `5208f3e0dfc96e8bdd0218ca49d9bbb38ee53151`. Assign migration numbers at landing.

## Progress

- [ ] M1 owner/admin Capacity diagnostics

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

**Unverified before implementation**. Query plans, endpoint/identity cardinality impact and suite runtime.
