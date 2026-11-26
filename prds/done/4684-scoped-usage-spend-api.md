# PRD #4684: Scoped run usage API: filter options and Spend

**Issue:** #4684.
**Status:** Implemented; owner decision at landing (2026-11-25): admin all-users `window=all` is not offered (400), all-time deferred to a follow-up.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** API, DTO contracts, docs and specs. Additive view/index migrations only. No user-interface or command implementation. No changes under `.github/workflows/**`, including validation.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`. Migration numbers are assigned at merge time.

## Progress

- [x] M1 owner/admin filter options and Spend

## 1. Problem

The existing usage endpoints provide lifetime and rolling-seven-day totals. They cannot explain daily trends, the groups contributing to cost, or failure concentrations. Building those measurements independently in each client would produce incompatible windows, cost folds and privacy rules.

## 2. Outcome

A bounded read API provides discoverable filters and Spend. Its admission, snapshot, clock, window, filter and model-fold boundary is built to be reused by further read sections. It preserves existing usage endpoints and computes statistics on the server from one consistent database snapshot.

Acceptance examples:

1. An owner requests Spend for 30 calendar days. Daily buckets, split totals and headline cost reconcile for exactly that owner's creation cohort. Another owner's repository cannot be discovered through options or a filter error.
2. An administrator requests the admin Spend route with `user=all&split=user`. It includes authorized instance totals and user groups. The owner route remains owner-only even when its caller is an administrator.
3. Two usage models contribute to one run. Their folded dollars sum to that run's recorded cost, and with `breakdown=primary_model` the Spend breakdown counts the run once, under its Primary model. An unreported or missing-usage run remains disclosed rather than becoming a fully metered zero.

## 3. Out of scope

- Any other read section (outcome and failure analysis is a separate issue), and any new historical capture or tracking table.
- Any user-interface, navigation, chart or command implementation. API and DTOs only.
- Custom ranges, monthly grain and export formats.
- Chat accounting, subscription invoice estimates, estimated interrupted tails or allocating a cost ledger to individual PRs.
- Changes to `/api/usage`, `/api/admin/usage`, their semantics or existing consumers.

## 4. Modules and seams

### Routes, authentication and filter options

Mount literal routes, never a dynamic section catch-all:

| Scope | Routes | Authority |
|---|---|---|
| Owner | `/api/analytics/options`, `/api/analytics/spend` | Existing authenticated owner; SQL always predicates on the caller's owner UUID |
| Admin | `/api/admin/analytics/options`, `/api/admin/analytics/spend` | Existing admin read-only group and `RequireAdminRO` |

An absent section (e.g. `/api/analytics/reliability` before it exists) is chi's normal 404. Later sections add their own literal routes under the same prefixes and reuse the boundary below. Follow ADR-1184 D1: distinct owner and all-users SQL, never a nullable owner predicate or an owner-path `user=all` sentinel. The admin path accepts `user=all` (default) or a UUID. A specific admin-selected owner can use the owner-scoped query shape with that explicit UUID. The owner path rejects any `user` parameter with a field-specific 400; authorization is not inferred from a query parameter. Preserve existing bearer-token restrictions, including `RequireUser` clearing `IsAdmin` for non-`admin_ro` bearer tokens (`middleware/cli_auth.go`); do not add a credential bypass.

Register every literal route as `noLimiter` in `route_limiter_mounts_test.go`, as the existing usage routes are. Heavy-query protection is the dedicated admission mechanism below, not a new generic route limiter.

Validated parameters:

- `window=7d|30d|90d|all`, default `30d`. The admin all-users scope (`user` omitted or `user=all`) has no `all`: Options lists `7d`, `30d`, `90d` there and Spend answers 400 `window all not available for all users` before any analytics database work (authentication runs first) (owner decision at landing, 2026-11-25).
- `tz`, default `UTC` when omitted. Explicit empty and `Local` are invalid. Validate against a new cached lookup of `pg_timezone_names` (there is no existing Postgres-side precedent; `handler/schedules_catalog.go` validates with Go's `time.LoadLocation` and rejects `Local`). A name accepted by the database must also load in Go, because Go derives the bounds; test that pairing.
- `repo=<uuid>`, `harness=claude|codex`, `kind=<registry kind>|delivery|supporting`.
- Spend `split=user|usage_model|harness|repo|kind` (default `usage_model`) and `breakdown=user|primary_model|harness|repo|kind` (default `repo`).
- User grouping is admin-all-users only. Elsewhere `split=user` or `breakdown=user` is a 400 naming the offending grouping parameter (`split` or `breakdown`), distinct from the `user=<uuid>` selector error.
- Options returns the accessible repository set and, on the admin route, user choices. Cap each at 200 with deterministic ordering and explicit `truncated` metadata. Validate supplied UUID filters against authority even if omitted from a truncated options result.

An unknown and an inaccessible repo have the same field-specific 400 response. Invalid enums, unsupported section parameters and invalid timezone values are 400. Non-admin callers of admin routes get the existing forbidden response. Do not include raw SQL or sensitive values in errors.

### Query admission, snapshot and clock

Use a process-wide nonblocking semaphore for these reads with capacity `max(1, floor(pool.Config().MaxConns / 4))`, read from the live pool. The pool size comes from `DB_MAX_CONNS`, else the DSN's `pool_max_conns`, else the default of 20 (`config/config.go`). `ClampWriteSlots` (`workersvc/jobfiles.go`) is the precedent for deriving a limit from the pool; it uses half the pool for uploads, this uses a quarter. Protect options and all sections, including admin routes, with this same semaphore.

Allow one active request per authenticated caller per section (owner/admin variants share that caller/section key). Key by authenticated caller, not the admin's selected target. Concurrent duplicates fail fast, rather than coalescing different filters or retaining an unbounded queue. Release keys and slots on every exit; keep no idle per-user entries. Capacity or duplicate rejection is 503 with `Retry-After: 1`.

For an admitted request, create one approximately eight-second request deadline including pool acquisition. Execute all reads in one READ ONLY REPEATABLE READ transaction; use `SET LOCAL statement_timeout = '8s'` as the database backstop. Roll back under a bounded cleanup context after cancellation. SQLSTATE 57014 and request deadline expiry map to 503 with `Retry-After: 1`; other database failures use the existing sanitized 500 behavior.

Capture the injectable Go clock once and pass `@now`, `@from`, `@to` and previous-period bounds. This SQL never calls `now()`. Preserve session TimeZone independence: use three-argument `date_trunc` and the timezone-aware four-argument `generate_series` (PostgreSQL 17). Apply sargable half-open timestamp predicates before folding.

**Amendment D-A: Go-authoritative calendar (owner-accepted, rev 6 plan review).** This supersedes the `date_trunc` / `generate_series` sentence above and the rolling-bound wording below. Go computes every boundary (`analytics.DayStart`: the earliest instant whose local date is at least D). SQL does only half-open membership against Go-supplied arrays (`bucket_starts[]`, `bucket_ends[]`, `roll_starts[]`, `roll_ends[]`): no `date_trunc`, no `generate_series`, no `now()`, and `@now = @to`. Buckets are contiguous first-midnight intervals `[dayStart(D), dayStart(D+1))`, so a wholly skipped date yields one bucket fewer and a rollback across midnight puts the repeated instants in the later bucket. The last bucket ends at `@now` and is empty (still emitted) when `@now` is a local midnight. A rolling point covers the seven local dates ending at the last date its bucket covers, whatever the bucket width, clipped at `history_start`; it is partial when that unclipped range starts before `history_start` or belongs to today's bucket. The rolling ratio series attributes per local day over the fold range (`previous_from`, or `history_start` for `all`); the displayed bucket panel and the headline attribute over `[from, now)`.

`7d` is today plus six preceding local calendar days; 30d/90d are analogous, with the last bucket ending at captured now and marked partial. The previous comparison is the immediately preceding equal number of whole calendar days; report those actual bounds, so the partial/current-versus-full comparison is explicit. All time covers the scope's complete creation history, starting at its aligned week; use 1, 2, 4, 8... week buckets until at most 104 are needed. Report the width; do not truncate history. Empty scopes return bounded empty data without inventing an earliest run.

For `window=all` there is no previous period: the previous bounds are absent and every fold below that is written over `[previous_from, to)` uses `[history_start, to)`, where `history_start` is the aligned start of the scope's creation history. Seven-day rolling series (Spend's rolling ratios here, and any later section's) have one point per displayed bucket (so at most the bucket count, ≤ 104 points). Each point covers the seven local calendar days ending at that bucket's end (captured now for the last bucket), using real data from before the window where it exists: the fold's lower bound for these series extends six days before the window start (before `history_start` for `window=all` it has nothing to extend into). A point is marked partial when its seven days are not fully covered, that is when they reach before `history_start` or end at captured now. Pin both rules in fixtures, including week-wide all-time buckets and the point ending today.

### Canonical cost fold and dimensions

Use migration `00244` as the cost algorithm: per-leg maxima, `usage_basis`, lineage and cumulative subtraction. Add `run_usage_model_totals`, an additive view producing one row per `(run_id, model)` with tokens, recorded cost and folded cost status. Its sum and status reduction per run must equal `run_usage_totals`.

For owner reads keep ADR-1620's owner-scoped pushdown guarantee, so the database cannot fold every owner's usage, but not its literal fence: `SelfUsage` can use a correlated LATERAL with `LIMIT 1` only because `run_usage_totals` yields at most one row per run. The new per-model fold yields several rows per run, so use a cardinality-preserving fence that keeps every model row and every run without usage. Prove owner-scoped pushdown with generic prepared plans, and pin it with a multi-model, missing-usage fixture. Build a MATERIALIZED cohort/fold over `[previous_from, to)` once in the Spend aggregate statement and reuse it for every cost panel. A CTE cannot be shared across separate SQL statements: do not claim request-wide reuse by repeating it in each query. Use the scoped per-model fold to derive per-run totals once; parity tests pin equivalence with `run_usage_totals`. Admin-all-users has its own query. Never add `run_usage_messages` or estimated tails from `00292`.

Primary model means the model with most folded output tokens for that run, with a deterministic lexical model-id tie break; no folded usage means `unknown`. It is not `runs.model`, whose NULL means inherited configuration. Each run belongs to one Primary model for counts and rates. Cost splits by usage model may place one run's contributions in multiple groups; cost disclosure always comes from run-level status. Per-model coverage counts are not additive unique-run counts.

Define kind sets once from the run-kind registry, with tests pinning that they partition it:

- Delivery: issue, mr_rework, ci_fix, prompt, self_improve, task, job.
- Supporting: judge, cross_check, chat.

Recorded spend includes all tracked scoped work and explicitly excludes chat even if a stray historical usage row exists. Chat is untracked, not a recorded zero. Cost per completed run is all scoped recorded cost divided by completed Delivery runs in the same creation cohort. A zero denominator gives null plus a reason.

Return run-level metered, subscription, unreported and missing-usage counts. Coverage is metered non-chat runs divided by applicable non-chat runs; missing usage stays in the denominator. Retain partial recorded dollars and disclose incompleteness on totals, groups, buckets and ratios. Coverage counts cannot be inferred only from usage-bearing rows.

### PR identity and Spend result

Canonical PR identity is `(repo_id, mr_iid)`: `repos` is unique on `(connection_id, forge_project_id)`, and a connection has one owner. This concretely means forge connection + repository + PR number. Instance totals can count the same external PR twice when two owners connected it; disclose that boundary.

A PR is currently considered merged if any retained run with that identity has `mr_state='merged'`. Do not inspect only a helper run's stale state. Existing issue watchers update the relevant latest completed run; board-free watchers stop on terminal states. Disclose current-state coverage limitations.

**Amendment D-B: scoped merge witnesses (owner-accepted, rev 6 plan review).** "Any retained run" above means any run in the response's own scope. A PR is merged when a run in that scope with the same `(repo_id, mr_iid)` has `mr_state = 'merged'`, whatever its creation time. Owner-shaped statements add `w.user_id = @user_id` to the witness; only the admin all-users statement has no owner predicate. An owner's figure therefore never depends on another owner's runs.

Clarification (owner decision at landing, 2026-11-25): "the response's scope" means the owner scope: owner routes and admin `user=<uuid>` use that user's runs; admin all-users uses any run. Harness and kind filters choose which PRs are counted, not which runs may prove a merge, and the witness also ignores the repo and window filters. A same-owner `mr_rework` or other-harness run can therefore prove a PR merged without adding its cost to the filtered cohort. CodeRabbit's suggestion to filter the witness was declined. For an empty scope, `window=all` reports `bucket_width_days` 7 (weekly grain) with no buckets, where it previously reported 1.

Select PR identities through filtered creation-cohort Delivery runs. Assign each selected PR to exactly one bucket, the earliest matching cohort run referencing it. Cost-per-merged-PR numerator includes every filtered cohort Delivery run carrying a selected PR, including prompt, failed attempts and rework; runs without a PR are outside this ratio. The denominator is distinct selected currently merged PRs. It is a cohort ratio, not a ledger or actual merge-day activity. Rolling ratios divide summed numerators by summed denominators, never average daily ratios. This attribution differs from the existing usage endpoints' PR counts; say so in the docs.

Spend response includes: totals and disclosure; cost split buckets; ranked split totals; cost-per-completed and cost-per-merged ratios with denominators; cache-read share `read/(input+read+creation)`, null when that denominator is zero; top ten expensive runs; and a breakdown containing finished, failure rate, cost/share, completion ratio, coverage, elapsed p50/p90 and merged PRs. Call the cache metric a recorded cache-read ratio, not proven savings. Limit splits to five groups plus Other and breakdown to 50 rows plus Other. Exclude Other from any unsupported percentile aggregation rather than averaging percentiles; calculate its population percentile directly.

### Outcome classification used by the breakdown

The breakdown's finished, failure rate and completion ratio reuse the existing outcome classification (`00500_outcome_kind.sql`, the `SelfRunOutcomes` family in `queries/runtime.sql`): plan rejected is failed with `fail_origin='plan_rejected'`; failed excludes that origin; `base_diverged` is its own finished outcome, excluded from failed; finished is completed + cancelled + plan rejected + base diverged + failed. Define it once in a shared helper so later sections reuse the same classification.

### Response, documentation and resource bounds

Pin exact DTO names using recorded Go JSON fixtures; keep the common envelope a shared type so later sections reuse it. Common envelope: effective scope, captured now/generated_at, half-open bounds, timezone, bucket width/start/end/partial, previous-period bounds or omission for all time, applicability and coverage metadata. Counts/dollars/buckets reconcile for additive measures; rates and distinct-PR group counts explain nonadditivity. Return null plus a fixed reason for unsupported values.

Document the API in `docs/` with title/order/audience frontmatter, choosing an unused order within that audience, and run `task docs:sync` (its generated `api/internal/uzidocs/embed` changes are expected). Add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`; never edit frozen `specs/ai.md`.

## 5. Testing decisions

- LiveDB tests only in `store` and `handler`, already enumerated by CI. Follow `TestUsageRollupsLiveDB`, `TestRunOutcomesRollupsLiveDB` and `TestRunUsageTotalsSessionCumulativeLiveDB` (`store/run_usage_integration_test.go`), and reuse the fake-store setup of `TestAdminUsageRequiresAdmin` (`handler/usage_test.go`).
- Authorization through the actual router, not a handler wrapped directly: cookie admin, admin-read bearer token, and an ordinary bearer token owned by an admin (which must not reach admin routes).
- Parity over matched populations, not unqualified lifetime parity: the existing usage `run_count` counts a narrower kind set. Pin additive Spend totals, and the breakdown's outcome counts, against the existing aggregates over the same population, and pin each intentional difference with its own test. Bounded calendar windows use independent expected values, including pre-window creation/in-window finish and in-flight runs.
- Model-fold equality per run across sessions/models/epochs/cumulative legs; deterministic Primary model; unknown usage and complete disclosure. Pin PR dedup, stale helper state, prompt cost, cross-owner duplicate external PRs and earliest-cohort attribution.
- Pin Europe/Bucharest DST on 2026-10-25 and vary session TimeZone. Verify real empty buckets, partial cutoff and all-time coarsening.
- Concurrent mutation during a response must not split KPI/bucket snapshots. Owner/admin isolation, options truncation, inaccessible filters, literal-route absence, semaphore/single-flight release and cancellation.
- Fake `pgconn.PgError{Code:"57014"}` maps to 503; one bounded LiveDB transaction uses a 1 ms statement timeout and `pg_sleep` to prove real cancellation. This is an intentional tiny timeout probe, not a suite sleep.
- For regression-like seams observe the named mutation fail and the restored code pass, per `.claude/rules/go.md`.
- **In-run checks:** `task check:api`; `go test -race -count=1` on the affected packages; the new and touched LiveDB cases run by name against PostgreSQL 17 with positive RUN/PASS counts and no SKIP (Docker required); `task check:migration-numbering` and `task check:migration-additive` for the new migrations; `task docs:sync` plus `task check-docs:web` when docs change; `check:changelog-entry` with one lowercase UUID-v4 changelog fragment per shipping PR. Sweep changed literals/identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`; run affected library/fake targets and name unrun phases.

## 6. Milestones

### M1: Owner/admin options and Spend

**Blocked by:** none.

Deliver literal routes, scope validation, shared admission/snapshot/clock boundary, model view migration, Spend DTO/fixtures, options and API docs/specs/tests.

Acceptance: examples 1, 2 and 3 hold; current/previous/all windows and the rolling-series rules reconcile; missing usage is disclosed; literal absent sections 404; semaphore protects the pool; real timeout and snapshot tests pass.

Plan evidence is required in this milestone: seed at least 100k runs offline with stated sessions × models × epochs, owner skew, fraction in-window and fresh statistics. Attach `EXPLAIN (ANALYZE, BUFFERS)` for owner and admin reads, including generic prepared plans, and measured request/runtime impact. Index only demonstrated gaps; use `CREATE INDEX CONCURRENTLY` in a NO TRANSACTION migration, dropping an invalid owned copy first as in `00283`. No allow-missing.

### Landing obligations (not milestones)

Before merge, the exact PR head must pass full `test:api`, `gate:repo`, `scan:secrets`, `sast:semgrep` and the LiveDB sweep in PR CI. Report the measured change in CI job time.

## 7. Decision Log

| Decision | Reason; rejected alternative |
|---|---|
| Literal owner/admin routes and distinct queries | ADR-1184 D1; reject nullable-owner sentinels and dynamic section handlers. |
| Semaphore plus per-caller/section single-flight | Protect lifecycle pool access; reject unbounded waiting despite fixed query windows. |
| One read-only repeatable snapshot | KPI and buckets must agree; reject unrelated reads observing concurrent mutations. |
| Calendar creation cohorts | One default definition; reject invented daily spending deltas. |
| 00244 model fold and Primary model | Models can inherit or mix; reject raw usage sums and nullable configured-model grouping. |
| Cardinality-preserving owner fence | Keeps ADR-1620's owner pushdown without truncating multi-model folds; reject reusing `LIMIT 1`. |
| PR identity is connection-local | Fits retained schema; disclose cross-owner external duplicates rather than claiming global dedup. |
| Reuse the existing outcome classification, incl. `base_diverged` | One definition across endpoints; reject a second failure taxonomy. |
| API only | Consumers share one bounded statistical contract without presentation coupling. |
| Spend first, outcome analysis as its own issue | Each is useful alone; one PRD is one run and one PR. This issue builds the shared boundary; the next reuses it. |
| D-A: Go-authoritative calendar (owner-accepted, rev 6 plan review) | Go owns every boundary and SQL only tests membership against arrays, so DST, skipped dates and the all-time week anchor have one definition (Go's `ZoneBounds` can report an end before the queried instant in far-future years, and `ParseInLocation` can run backwards, so `DayStart` is a bounded search pinned by tests); reject SQL calendar arithmetic, which would put a second definition in the query. |
| D-B: merge witnesses scoped to the response (owner-accepted, rev 6 plan review) | An owner's cost per merged PR must not depend on other owners' runs; reject an instance-wide witness behind an owner filter. |
| Repo masking on the owner route only | Owners must not learn foreign repo ids; an admin route has no such boundary, and an admin selecting a user may see that user's foreign repo (but cannot filter by it). Which applies comes from the mounted route, never a parameter; reject masking on admin routes. |
| Snapshot helper in the `store` package (`RunAnalyticsSnapshot`) | One place owns the REPEATABLE READ READ ONLY transaction, the 8 s deadline, `SET LOCAL` settings and rollback; reject per-handler transaction code. |
| `SET LOCAL jit = off` in the snapshot | Admin all-users Spend was JIT-compiling about 446 functions: 8.2 s with JIT, 1.3 s without (30d); reject a server-wide `jit` change. |
| Admin all-users `window=all` not offered (owner decision at landing, 2026-11-25) | Measured 9.6-10.2 s against the 8 s budget, a 503. Options omits `all` for that scope and Spend answers 400 `window all not available for all users` before any analytics database work (authentication runs first); owner and admin `user=<uuid>` all-time and every bounded window are unchanged, and Options still accepts and ignores `window`. Admin all-users all-time deferred to a follow-up that adds per-run totals for finished runs; owner decision at landing. |
| Keep the lateral `OFFSET 0` fold over shape (b) `run_id = ANY(ARRAY(SELECT ...))` | Shape (b) is 5-10% faster on custom plans but 30x to 170x slower on generic plans (owner 30d 9.2 s, admin 30d 215 s); the lateral stays bounded under both. See ADR-1620's #4684 addendum. |
| No index added | Four candidates (`runs (created_at)` and three `runs (repo_id, mr_iid)` variants) gave at most 7.8% p95 gain (owner) and 4.9% (admin 30d), under the 20% bar, at 0.45-2.7 MB each; reject carrying write cost for noise. Numbers below. |

## 8. Measurement evidence

Method: throwaway PostgreSQL 17.12 with default settings (shared_buffers 128MB, work_mem 4MB, jit_above_cost 100000), the exact sqlc statement text prepared, `now` fixed, a single host (a few percent noise). Harness: `analytics_evidence_livedb_test.go` under the `analyticsevidence` build tag and `UZI_ANALYTICS_PLAN_EVIDENCE=1`, committed as an opt-in harness (build tag plus env var), not a gate. Raw logs were not kept in the repository.

**Seed:** 120,000 runs, 845,988 `run_usage` legs (8 per run), 200 owners (largest 20.1% of runs, top 10 59.8%), 1,250 repos, three years of history (10.2% of runs in 30d, 15.2% in 90d), 80.9% Delivery and 19.1% Supporting kinds (judge 17%, chat 2%), 23.4% carry an MR. `mr_rework` and `cross_check` were not seeded. `ANALYZE` run. Disk: `runs` 97.6 MB (indexes 43.4 MB), `run_usage` 201.4 MB (indexes 64.4 MB).

**Timings** (custom plan, median / p95 ms):

| Workload | JIT on | JIT off (shipped) |
|---|---|---|
| Admin all-users 30d | 8193 / 8338 | 1283 / 1302 |
| Admin all-users 30d, `split=user`, `breakdown=user` | 8294 / 8446 | 1310 / 1320 |
| Owner 30d | 710 / 722 | 250 / 254 |
| Owner 90d | 908 / 935 | 425 / 435 |
| Admin all-users `window=all` | 15678 / 15818 | 9599 / 9749 (10017 / 10090 on a second pass) |

Generic plans get no JIT: admin 30d 1295 / 1312, owner 30d 289 / 291. The admin options workload (repo options, user options and the all-users history start, timed together) takes 24 ms; the Options endpoint itself does not read history start, which is most of that time.

**Indexes** (JIT off, custom, median / p95 ms; baseline owner 30d 250 / 254, admin 30d 1283 / 1302, admin `all` 9599 / 9749, admin options workload 24.1 / 25.9):

| Candidate | Bytes | Owner 30d | Admin 30d | Admin `all` | Admin options workload |
|---|---|---|---|---|---|
| `runs (created_at)` | 2,719,744 | 246 / 251 | 1253 / 1266 | 9748 / 9833 | 1.5 / 1.7 |
| `runs (repo_id, mr_iid) WHERE mr_iid IS NOT NULL` | 1,081,344 | 235 / 241 | 1265 / 1272 | 9972 / 10037 | 24.2 / 25.9 |
| `runs (repo_id, mr_iid) WHERE mr_state = 'merged'` | 450,560 | 245 / 246 | 1254 / 1259 | 9635 / 9773 | 24.1 / 26.0 |
| `runs (user_id, repo_id, mr_iid) WHERE mr_state = 'merged'` | 630,784 | 227 / 234 | 1188 / 1238 | 9665 / 9805 | 23.7 / 26.1 |

None was kept, so there is no index migration. Side effects noted: `created_at` takes the admin history-start `MIN` from 24 ms to 1.5 ms, and the merged-witness indexes turn a 30 ms sequential scan into a 1.5 ms index-only scan.

**Finding, not fixed here:** admin all-users `window=all` takes 9.6-10.2 s with JIT off (15.7 s with it), over the 8 s `statement_timeout`, so it would return 503. In its plan the per-run `model_fold` lateral is the largest node (about 5.2 s of 117,550 per-run scans); the next is the breakdown and split ranking family (external-merge sorts and window passes over 117,550 rows, about 4 s inclusive), so removing the fold alone would still leave it near 5 s. Closing it needs a design decision: cap `all` for the admin all-users scope, precompute per-run totals, or a larger budget. No redesign was attempted. The owner decided at landing (2026-11-25) not to offer admin all-users `window=all`: it answers 400 `window all not available for all users`, and the all-time follow-up with per-run totals is deferred (see the Decision Log).

**Re-measured at landing (2026-11-25, rework head `cc6a03719`).** Same harness and setup on a throwaway PostgreSQL 17 container started with `jit=off` (every baseline EXPLAIN reports zero JIT). Seed: 120,000 runs, 847,208 `run_usage` legs, 200 owners (largest 20.1%, top 10 59.8%), 1,250 repos, 10.2% of runs in the last 30 days, ANALYZEd. These are statement-level timings: the harness lifts `statement_timeout` and times the prepared sqlc statements on one connection (20 runs after 3 warm-ups; 8 runs when the first warm-up exceeds 8 s), in custom and generic `plan_cache_mode`. End-to-end request time (HTTP, JSON encoding, pool wait, the 8 s request budget) was not measured. The existing workloads reproduce the JIT-off figures above (medians within about 2.5%, p95 within about 3.5%; custom median / p95: admin all-users 30d 1287 / 1304 ms, owner 30d 248 / 250, owner 90d 435 / 450, admin all-users `window=all` 9690 / 9767 at the store level, a statement the route no longer issues). New workloads, custom / generic median:

| Workload | custom | generic |
|---|---|---|
| Owner `window=all` | 1801 | 2190 |
| Owner 30d, `harness=claude`, `kind=issue` | 98 | 104 |
| Admin `user=<uuid>` 30d, same filters | 97 | 107 |

The two filtered workloads (EXPLAIN, custom / generic): owner 30d filtered 101.0 / 116.0 ms exec and admin `user=<uuid>` filtered 97.1 / 121.9 ms, no temp blocks, no disk sorts, zero JIT; the cohort is a bitmap scan on `runs_user_created_idx` (1,080 runs) and the per-run `model_fold` lateral is the largest node (about 44 ms over 1,080 loops). Owner all-time cost is the per-run `model_fold` lateral (about 1.0 s over 23,672 runs) plus its spilled `run_fold` sort, so it grows with the owner's run count. The candidate-index table above reproduces for indexes 1, 3 and 4; index 2 (`(repo_id, mr_iid) WHERE mr_iid IS NOT NULL`) measured about twice as slow on admin all-users `window=all` in this pass (1.6x with `split=user`, unchanged with `split=repo`, and +21% on admin 30d `split=user`), undiagnosed and possibly host noise; no index ships.

**Not measured:** `mr_rework` and `cross_check` runs (the seed has none); end-to-end request time; suite-runtime and CI job-time impact (a landing obligation, measured on the PR).
