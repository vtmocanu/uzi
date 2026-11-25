# PRD #4684: Scoped run usage API: filter options and Spend

**Issue:** #4684.
**Status:** Ready.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** API, DTO contracts, docs and specs. Additive view/index migrations only. No user-interface or command implementation. No changes under `.github/workflows/**`, including validation.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`. Migration numbers are assigned at merge time.

## Progress

- [ ] M1 owner/admin filter options and Spend

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

- `window=7d|30d|90d|all`, default `30d`.
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

**UNVERIFIED before implementation:** latency, disk bytes, query plans and suite-runtime impact. These require the stated offline measurements; no external lookup is left to the implementation worker.
