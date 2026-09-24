# ADR-1620: keep `plan_cache_mode = auto` on the app pool, and guard `run_usage_totals` consumers by query shape

**Status**: Accepted (issue #1620). The candidate pool default, `plan_cache_mode = force_custom_plan`, was measured and **not adopted**; the fix that shipped is the query-shape rule below.
**Date**: 2026-09-24
**Deciders**: lead (plan), coder (measurement and implementation), reviewer.
**Related**: [ADR-1079](1079-run-usage-per-leg-fold.md) and [ADR-1562](1562-run-usage-session-cumulative-basis.md) define the `run_usage_totals` fold whose cost this ADR is about; neither is changed.
**PRD**: none. The spec lived in the issue body; this ADR records the measurement behind the one decision the issue left open.

## Decision (summary)

The API's pgx pool keeps PostgreSQL's default `plan_cache_mode = auto`. Forcing a custom
plan on every execution would have made the #1620 failure class impossible for every
query. But it adds per-execution planning cost that, measured, triples the per-call time of
the worker claim (`ClaimRun`, 1.09 ms to 3.70 ms) and the message append
(`InsertRunMessage`, 0.24 ms to 0.69 ms), and costs the Runs list 53%. Those are the
hottest paths in the system, so the trade is not worth it.

The #1620 regression is instead closed where it arose, by a durable rule on how a query
may consume `run_usage_totals`:

> A multi-row consumer of `run_usage_totals` folds by a **run-id parameter**
> (`run_id = ANY($1)`, or a per-run lateral inside an **inner** join), never by a
> `LEFT JOIN` of the view into a multi-row result.

`TestRunUsageGenericPlanLiveDB` (`api/internal/store/run_usage_generic_plan_livedb_test.go`)
enforces the rule under `force_generic_plan` by bounding each plan node's row work.

## Context

`/api/runs` timed out (issue #1620). pgx caches every statement it sends as a server-side
prepared statement (its default `QueryExecModeCacheStatement`). Under `auto`, PostgreSQL
plans the first five executions of a prepared statement with the actual parameter values
(custom plans). From the sixth it may switch to one **generic** plan, planned without the
values, whenever that plan's estimated cost is not worse than the average custom plan.

`ListRunsForUser` then `LEFT JOIN`ed the `run_usage_totals` view, a three-level fold
(MAX per leg, then a window per lineage, then SUM per run) over `run_usage`. In the
generic plan the planner estimated `r.user_id = $1` at a handful of rows. It nested-looped
the view once per run row, re-folding the whole of `run_usage` each time: **13.9 s**
against **60 ms** for the custom plan. The M1 fix (commit `1e825700`) removed the view
join from the run list and moved the page's usage onto `ListRunUsageTotalsForRuns`
(`run_id = ANY(@run_ids)`), a parameter qual that pushes down to `run_usage_pkey` in
either plan. `SelfUsage` now reads the view through `CROSS JOIN LATERAL … LIMIT 1`.

The open question was whether to also remove the failure class wholesale by forcing
custom plans pool-wide. The operator stopgap is
`ALTER ROLE <app-role> SET plan_cache_mode = force_custom_plan;`. That is exactly this
setting, and it is what an incident would reach for. This ADR measures its cost.

## Measurement

**Environment.** A throwaway `postgres:17` container (PostgreSQL 17.11, stock
configuration, `shared_buffers` 128MB) on a 2-vCPU Linux dev host. The Go client ran on
the same host over loopback. The schema was migrated through the app's own `store.Migrate`
(goose head 251), then `ANALYZE`d.

**Seed.** This is the shape of the M1 regression test:

- the measured user: 300 completed issue runs, each with 4 `per_leg` `run_usage` legs (2 models × 2 lineage epochs);
- a second user with the same shape;
- 200 noise users with one usage-less run each.

On top of that, the hot-path fixtures:

- one online worker for the user (cap 4, production protocol capabilities);
- one `running` run held by that worker at claim generation 3, carrying 2000 prior `run_messages`;
- one claimable `queued` run.

Totals: 802 runs, 2400 `run_usage` legs, 2000 messages.

**Queries.** These are the sqlc texts the app sends, with production-shaped arguments:

- `ListRunsForUser`: the user's page, 200 rows.
- `ListRunUsageTotalsForRuns`: 200 run ids, 200 rows.
- `SelfUsage`: 1 row, `run_count` 300.
- `ClaimRun`: `RecoveryCapable` true, custody limit 8. It also opens the custody hold.
- `HeartbeatWorker`
- `InsertRunMessage`: a live generation at the next free seq.

**Modes.** A fresh connection per mode, with `plan_cache_mode` set through pgx
`ConnConfig.RuntimeParams` and confirmed with `SHOW` before any work. That gives clean
statement and plan caches.

**Two measurements per query × mode.**

1. `EXPLAIN (ANALYZE, SUMMARY) EXECUTE`: a server-side `PREPARE`, executed 6 times first so
   `auto`'s generic plan is eligible, then 9 samples. The table shows the median planning
   and execution time. `pg_prepared_statements` confirmed which plan ran: under `auto`,
   **every** query was on its generic plan (5 custom, then generic); under
   `force_custom_plan`, generic count 0.
2. Throughput: N = 500 executions on ONE connection through the real sqlc `Queries`
   methods (pgx's default cached-statement path), after 10 unrecorded warm-ups. Only
   the method call was timed. This ran for 3 rounds, alternating which mode went first.
   The table shows the median round's per-execution mean.

**Reset for the write queries.** Each mode's work ran inside one transaction.
`SAVEPOINT m` was taken before EACH execution and `ROLLBACK TO SAVEPOINT m` issued after
it, so every execution saw the same fixture:

- the same claimable queued run for `ClaimRun`;
- the same worker row for `HeartbeatWorker`;
- the same free `(run_id, seq)` for `InsertRunMessage`.

**Verification.** The harness failed closed: any error or unexpected row count stopped it
without recording numbers. Every counted execution in both modes (1500 per query per mode,
plus the EXPLAIN samples) passed these checks:

- `ClaimRun` returned the queued run with status `claimed`.
- `HeartbeatWorker` returned the worker.
- `InsertRunMessage` returned `inserted` and `generation_live` both true.
- The reads returned 200 / 200 / 1 rows, with `SelfUsage.run_count` = 300.

Every write EXPLAIN showed a `ModifyTable` node with exactly 1 actual row:

- `ClaimRun`: Update 1, LockRows 1, and the custody-hold Insert 1.
- `HeartbeatWorker`: Update 1.
- `InsertRunMessage`: Insert 1.

| query | mode | plan chosen | planning (ms) | execution (ms) | N=500 per-exec (µs), 3 rounds | per-exec median (µs) | change vs `auto` |
|---|---|---|---|---|---|---|---|
| `ListRunsForUser` | `auto` | generic | 0.032 | 3.685 | 6540 / 6134 / 6046 | 6134 | |
| `ListRunsForUser` | `force_custom_plan` | custom | 3.078 | 2.368 | 9244 / 9368 / 9469 | 9368 | +3234 (+53%) |
| `ListRunUsageTotalsForRuns` | `auto` | generic | 0.188 | 4.716 | 5933 / 5935 / 5862 | 5933 | |
| `ListRunUsageTotalsForRuns` | `force_custom_plan` | custom | 0.822 | 4.866 | 6502 / 6564 / 6389 | 6502 | +569 (+10%) |
| `SelfUsage` | `auto` | generic | 0.017 | 11.126 | 10766 / 10525 / 10689 | 10689 | |
| `SelfUsage` | `force_custom_plan` | custom | 1.036 | 11.081 | 11785 / 11995 / 11881 | 11881 | +1192 (+11%) |
| `ClaimRun` | `auto` | generic | 0.051 | 0.713 | 1152 / 1069 / 1092 | 1092 | |
| `ClaimRun` | `force_custom_plan` | custom | 2.428 | 0.897 | 3870 / 3697 / 3675 | 3697 | +2605 (3.4×) |
| `HeartbeatWorker` | `auto` | generic | 0.015 | 0.093 | 313 / 318 / 323 | 318 | |
| `HeartbeatWorker` | `force_custom_plan` | custom | 0.095 | 0.088 | 356 / 384 / 382 | 382 | +64 (+20%) |
| `InsertRunMessage` | `auto` | generic | 0.019 | 0.112 | 240 / 276 / 240 | 240 | |
| `InsertRunMessage` | `force_custom_plan` | custom | 0.323 | 0.210 | 671 / 686 / 745 | 686 | +446 (2.9×) |

The per-execution delta tracks the planning time: `ClaimRun`'s ~2.4 ms plan appears
almost one-for-one as +2.6 ms per call. Under a custom plan, planning is paid on every
execution, and it is a cost that scales with query complexity, not data size. The
absolute numbers are this host's. The ratios are the finding: on the two write hot paths,
planning costs several times the execution it precedes.

## Decision and rationale

The pool change was to land only if `force_custom_plan`'s added per-execution overhead
was small relative to execution time on the hot paths, with no material regression on
any of them. It fails that bar on four of the six queries:

- `ClaimRun`, which every worker calls on every claim poll, goes from 1.09 ms to 3.70 ms.
- `InsertRunMessage`, which every message batch of every running run calls, goes from 0.24 ms to 0.69 ms.
- `HeartbeatWorker` rises 20%.
- The run list itself rises 53%, trading a failure mode the M1 rewrite already removed for
  a permanent tax on the page it was meant to protect.

What the setting would buy is insurance against a *future* query regressing the same way.
That insurance is bought more cheaply by the rule and the plan-shape test. The test
forces the generic plan, so it measures exactly the plan `auto` would pick.

`plan_cache_mode` therefore stays at the server default, and `api/internal/store/pool.go`
is unchanged.

## If a pool-wide setting is ever adopted: the mechanism

If a future measurement changes the answer (a different query mix, or a planner
regression that the rule cannot cover), set the GUC through pgx
`ConnConfig.RuntimeParams`, not `QueryExecModeCacheDescribe`:

- **It keeps statement caching.** Only the server-side plan choice changes;
  `CacheDescribe` gives up server-side prepared statements for every query.
- **It is the same GUC as the operator stopgap** (`ALTER ROLE <app-role> SET
  plan_cache_mode = force_custom_plan;`), so the incident lever and the code default are
  one mechanism, not two.
- **It is visible.** `SHOW plan_cache_mode` on any app connection reports it.
- **It is DSN-overridable.** pgx keeps a DSN key it does not recognise, such as
  `?plan_cache_mode=auto`, in `RuntimeParams`. `pgxpool.ParseConfig` did so for both a
  URL DSN and a keyword DSN, checked on pgx v5 during this issue. The code should
  therefore only fill the key when it is absent. Setting that key through `RuntimeParams`
  took effect on every connection in the measurement above, confirmed with `SHOW`.

The sketch was a DB-free `poolConfig(dsn)` helper: `pgxpool.ParseConfig`, then set
`RuntimeParams["plan_cache_mode"]` unless the DSN already set it, with `OpenPool` calling
`pgxpool.NewWithConfig`.

**Caveat:** a GUC sent in the startup packet does not survive transaction-mode
PgBouncer. PgBouncer refuses a startup parameter it does not track, unless it is listed in
`ignore_startup_parameters`, which drops it silently. This was not tested here. No
PgBouncer is deployed today; if one is added, switch to `QueryExecModeCacheDescribe`, or
set the GUC per role on the server instead.

## The durable rule for `run_usage_totals`

A multi-row consumer of `run_usage_totals` folds by a run-id **parameter**:
`run_id = ANY($1)` (as `ListRunUsageTotalsForRuns` does), or a per-run lateral inside an
**inner** join (as `SelfUsage` does, `CROSS JOIN LATERAL … WHERE t.run_id = r.id LIMIT 1`).
It never `LEFT JOIN`s the view into a multi-row result.

- A parameter qual on `run_id` is the view's outermost `GROUP BY` key and the leading key
  of every inner `GROUP BY` / window `PARTITION BY`. PostgreSQL therefore pushes it down
  to an index scan of `run_usage_pkey` in the custom and the generic plan alike.
- A `LEFT JOIN` of the view gives the planner the choice that #1620 hit: fold everything
  once and hash-join, or nested-loop the whole fold per outer row. Under a generic plan's
  row estimates it picked the latter.
- `LEFT JOIN LATERAL` would keep the per-row pushdown with outer-join semantics. But it is
  unusable here, because sqlc does not propagate LATERAL nullability: it types the joined
  columns non-nullable, so a row with no usage panics at scan
  ([specs/ai.md](../specs/ai.md), lines 1774-1776). The multi-row read that needs outer
  semantics (a run with no usage) gets them from the separate `= ANY($1)` query, where
  "absent" is simply no row.

A single-row read such as `GetJudgeRunUsageForTarget` may still `LEFT JOIN` the view. It
folds the table at most once per call, and the plan-shape test bounds it at 4× the table.

`TestRunUsageGenericPlanLiveDB` enforces the rule. Under `force_generic_plan`, every plan
node over `run_usage` must stay within 4 × `count(run_usage)` rows of work, counting rows
emitted plus rows its scan discarded by Filter, both across all loops. User-scoped reads
must also stay within the user's own legs. The `ListRunsForUser` case guards against the
view join coming back into the run list.

## Consequences and follow-ups

- No runtime change. The API keeps pgx's default statement cache and PostgreSQL's
  `auto` plan choice. The operator stopgap remains available for an incident, at the
  per-call cost measured above.
- **Follow-up (not scheduled): the pool default.** Revisit `force_custom_plan` as a pool
  default only with a new measurement showing the hot-path planning overhead is small.
  That could follow a PostgreSQL planner change, a simpler `ClaimRun`, or a per-statement
  mechanism that exempts the hot paths. Until then the query-shape rule and its test are
  the guard.
- A new consumer of `run_usage_totals` must follow the rule, and should get a case in
  `TestRunUsageGenericPlanLiveDB`.
