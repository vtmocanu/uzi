# ADR-1620: keep `plan_cache_mode = auto` on the app pool, and guard `run_usage_totals` consumers by query shape

**Status**: Accepted (issue #1620). The candidate pool default, `plan_cache_mode = force_custom_plan`, was measured and **not adopted**; the fix that shipped is the query-shape rule below. A later measurement (#4602, [Measurement update](#measurement-update-4602-2026-11-22)) left this decision unchanged and added a per-statement idle precheck at the worker claim seam; `plan_cache_mode` stays `auto`.
**Date**: 2026-09-24
**Deciders**: lead (plan), coder (measurement and implementation), reviewer.
**Related**: [ADR-1079](1079-run-usage-per-leg-fold.md) and [ADR-1562](1562-run-usage-session-cumulative-basis.md) define the `run_usage_totals` fold whose cost this ADR is about; neither is changed. [ADR-4591](4591-api-pool-backend-memory.md) later measured statement-cache and lifetime candidates on the same pool and changed no default; `plan_cache_mode` stays as decided here. Issue #4602 re-measured `ClaimRun` on an idle queue (see [Measurement update](#measurement-update-4602-2026-11-22)); it also left `plan_cache_mode` at `auto`.
**PRD**: none. The spec lived in the issue body; this ADR records the measurement behind the one decision the issue left open.

## Decision (summary)

The API's pgx pool keeps PostgreSQL's default `plan_cache_mode = auto`. Forcing a custom
plan on every execution would have made the #1620 failure class impossible for every
query. But it adds per-execution planning cost that, measured, roughly triples the
per-call time of the worker claim (`ClaimRun`, 1.09 ms to 3.70 ms, 3.4×; historical, 2026-09-24) and nearly
triples the message append (`InsertRunMessage`, 0.24 ms to 0.69 ms, 2.9×), and costs
the Runs list 53%. Those are the hottest paths in the system, so the trade is not worth it.

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

`ListRunsForUser` then `LEFT JOIN`ed the `run_usage_totals` view. Its current
definition (migration 00244) folds `run_usage` in four levels: a `GROUP BY` per leg
`(run_id, model, lineage_epoch)`, a running-sum window per `(run_id, model,
lineage_index)`, a `GROUP BY` per `(run_id, model, lineage_index)`, and an outer
`GROUP BY` per run. In the
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
(goose head 251), then `ANALYZE`d. The measurement harness itself was a throwaway
script, not kept in this repo; the numbers below are a recorded one-off, reproducible by
following the method documented in this section, not a re-runnable fixture.

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
| `ClaimRun` | `force_custom_plan` | custom | 2.428 | 0.897 | 3870 / 3697 / 3675 | 3697 | +2605 (+239%) |
| `HeartbeatWorker` | `auto` | generic | 0.015 | 0.093 | 313 / 318 / 323 | 318 | |
| `HeartbeatWorker` | `force_custom_plan` | custom | 0.095 | 0.088 | 356 / 384 / 382 | 382 | +64 (+20%) |
| `InsertRunMessage` | `auto` | generic | 0.019 | 0.112 | 240 / 276 / 240 | 240 | |
| `InsertRunMessage` | `force_custom_plan` | custom | 0.323 | 0.210 | 671 / 686 / 745 | 686 | +446 (+186%) |

The `ClaimRun` rows above are historical (2026-09-24, a different schema, data, hardware and
plan type than the #4602 update below).

The per-execution delta tracks the planning time: `ClaimRun`'s ~2.4 ms plan appears
almost one-for-one as +2.6 ms per call. Under a custom plan, planning is paid on every
execution. Planning cost is expected to scale with query complexity rather than data
size, but only one data size was measured here, so that scaling claim is an
expectation, not something this measurement verifies. The
absolute numbers are this host's. The ratios are the finding: on the two write hot paths,
planning costs several times the execution it precedes.

## Decision and rationale

The pool change was to land only if `force_custom_plan`'s added per-execution overhead
was small relative to execution time on the hot paths, with no material regression on
any of them. It fails that bar on four of the six queries:

- `ClaimRun`, which every worker calls on every claim poll, goes from 1.09 ms to 3.70 ms (historical, 2026-09-24).
- `InsertRunMessage`, which every message batch of every running run calls, goes from 0.24 ms to 0.69 ms.
- `HeartbeatWorker` rises 20%.
- The run list itself rises 53%, trading a failure mode the M1 rewrite already removed for
  a permanent tax on the page it was meant to protect.

What the setting would buy is insurance against a *future* query regressing the same way.
That insurance is bought more cheaply by the rule and the plan-shape test. The test
forces the generic plan `auto` may switch to — `auto` uses custom plans for a prepared
statement's first five executions, and from the sixth may switch to the generic plan
whenever its estimated cost is not worse than the average custom plan — so the test
measures exactly the plan shape #1620 hit.

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
  per-call cost measured above. ADR-4591 later measured further pool candidates and also
  changed no default.
- **Follow-up (not scheduled): the pool default.** Revisit `force_custom_plan` as a pool
  default only with a new measurement showing the hot-path planning overhead is small.
  That could follow a PostgreSQL planner change, a simpler `ClaimRun`, or a per-statement
  mechanism that exempts the hot paths. Until then the query-shape rule and its test are
  the guard. Issue #4602 did not take this route: it added an idle precheck query at
  the claim seam (`HasQueuedRunForUser`) instead of a pool or plan-mode change, and
  `plan_cache_mode` stays `auto`. `ClaimRun`'s SQL is unchanged.
- A new consumer of `run_usage_totals` must follow the rule, and should get a case in
  `TestRunUsageGenericPlanLiveDB`.

## Measurement update (#4602, 2026-11-22)

Issue #4602 asked why an idle worker's `ClaimRun` poll was slow. The working hypothesis was
that `auto` stays on custom plans for it. It was **refuted on these fixtures**.

**What shipped.** `claimCheckpointRun` (`api/internal/workersvc/checkpoint_coordinates.go`)
runs a new read, `HasQueuedRunForUser` (`api/internal/store/queries/runtime.sql`), on the
caller's transaction before the savepoint loop:

```sql
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE user_id = @user_id AND status = 'queued'
      AND kind <> 'chat' AND NOT cross_check_lane
)::boolean AS has_queued;
```

When it returns false, the poll returns idle without planning or executing `ClaimRun`. The
predicates are exact top-level conjuncts of `ClaimRun`'s `target` CTE, so false means
`ClaimRun` would claim nothing at that statement snapshot. Nothing is cached between polls.
`ClaimRun`'s SQL is unchanged, `plan_cache_mode` stays `auto`, and there is no pool-wide
change. The owner selected this candidate (C2) and rejected C1, which forced the generic plan
for this one statement. Regression tests:
`api/internal/workersvc/claim_idle_precheck_livedb_test.go` (six cases: empty queue with 0
`ClaimRun` calls even when another owner has a queued run and this user has a queued chat
run; a queued run this worker cannot claim still runs `ClaimRun` once per poll; no negative
caching; concurrent claimants with a calibration step; the snapshot path's idle commit; the
leased path's idle poll). Five of them fail with the precheck removed (the concurrent-claimants
control passes either way); the snapshot test's persistence assertion also fails when the idle
commit is removed. Both checks ran in scratch exports against throwaway postgres.

**Environment.** Throwaway `postgres:17` (17.12) in docker on a shared Linux worker (GOMAXPROCS 4), go1.27.3,
jit on with `jit_above_cost` 100000 never reached, `plan_cache_mode` `auto` unless forced, pgx
`QueryExecModeCacheStatement`, a fresh template clone per cell, autovacuum disabled on every table
in the template (so no autovacuum or autoanalyze ran during timing). The harness is the opt-in
`TestClaimRunPlanMeasurement` in `api/internal/store/claimrun_plan_measure_test.go`, with
`claimrun_plan_scale_test.go` and `claimrun_plan_service_test.go`. It is gated by
`UZI_CLAIMRUN_PLAN_MEASURE=1` and a throwaway `UZI_TEST_DATABASE_URL`, sits outside the
LiveDB$ sweep, and is selected by stage.

**Units.** The "ClaimRun segment" is test code mirroring production: begin READ COMMITTED,
`GetWorkerForUpdate`, [C2 precheck], savepoint, `OptInRunFences`, [C1 set], `ClaimRun`, [C1
restore, which also runs on `ErrNoRows`], release/rollback, commit. The "isolated statement"
is `ClaimRun` alone. Each cell: 1 cold call, 10 warm-ups, N=100 timed calls. T1, T2, T4 and E
report the median of 3 reps' p50s (ranges are across reps); T3 has 2 reps; T5 has 1 rep per
profile.

**T1** (profile p1, `auto`). Segment p50, then cold + 10 warm-ups summed, both in ms.

| Scenario | base | C1 | C2 | C1+C2 |
|---|---|---|---|---|
| (a) idle-empty | 2.19 / 414-433 | 2.38 / 121-124 | 0.92 / 17.8-18.1 | 1.01 / 18-21 |
| (c) claimable | 8.44 / 474-501 | 8.77 / 200-205 | 8.67 / 492-511 | 9.09 / 212-217 |

1. The isolated statement's p50 was about 0.95 ms for (a) and 6.9-7.3 ms for (c) across candidates and reps. Indicative only: this pass ran on each cell's clone after the segment phase, so it inherits that phase's rolled-back-write residue; no decision used it.
2. Round trips per call, base vs C2: 7 vs 4 for (a), 8 vs 9 for (c).

**T2** (base, forced modes). Segment p50 / cold + warm-ups, ms.

| Mode | (a) | (c) |
|---|---|---|
| `force_generic_plan` | 2.12-2.20 / 117-127 | 8.50-9.05 / 195-210 |
| `force_custom_plan` | 62.1-63.9 / 699-731 | 64.7-66.5 / 735-773 |

**T3** (server split, `EXPLAIN (ANALYZE, SUMMARY)` `EXECUTE` 1-12).

1. Under `auto`, executions 1-5 are custom plans: 52-73 ms planning and 0.7-2.2 ms execution for (a), across both reps.
2. The generic plan is built at execution 6, with 60.7 ms planning.
3. From execution 7, planning is 0.12-0.16 ms and execution about 0.8 ms for (a). For (c), execution is about 7-8 ms, and 17.7 ms on the first.
4. Forced generic: 73.5-79.3 ms planning on execution 1, then 0.12-0.18 ms. Forced custom: 69-75 ms planning on execution 1, then 48-59 ms on every later execution.
5. Client wall time per EXPLAIN was about 7 ms against about 0.9 ms of server execution at idle, so EXPLAIN instrumentation is not free.

**Mechanism** (stage S, a plan-choice-only diagnostic at 1k, 10k, 50k, 150k, 220k and 300k
runs; 40% of the history belongs to the measured user; the template keeps pmSeed's one queued
run, and the harness removes it in both measured idle clones per scale).

1. The generic cost grew 276, 539, 1,394, 3,525, 4,968, 6,785. That reaches the production figure of about 5,000 at about 220k runs.
2. At every size, `auto` used 5 custom plans and switched to the generic plan on call 6. The custom-plan estimate was about the generic estimate + 11.5 at 1k runs and + 27 from 10k up.
3. At (c), generic execution was never slower than custom by median: 6.9-7.4 ms vs 7.9-8.4 ms.

So `auto` never stayed custom, and steady-state idle `ClaimRun` is about 1 ms (T3: 0.12-0.16 ms
planning plus about 0.8 ms execution). The cost is
per fresh connection or per re-prepare: 5 custom plans plus the generic build, about 420 ms
over the first 11 calls.

**Not explained.** The production planning/execution split is not established. The issue's
read-only samples (frequent `ClaimRun` activity, sampled in-flight ages) are length-biased and
give no per-call duration, and its fresh-session EXPLAIN planning figure is a different
measurement, so this record assigns no per-call production cost. Unverified candidate causes: connection
churn; pgx statement-cache eviction; plan invalidation, for example from autovacuum/ANALYZE
on referenced tables. The harness disables autovacuum, so it could not exhibit that last
candidate. The maintainer observed autovacuum ANALYZE on `runs` in production; its
causal effect was not established.

**T4** ({base, C2}, p1, 3 reps). Segment p50, median of the 3 reps.

| Scenario | base | C2 | Note |
|---|---|---|---|
| (b) queued run this worker cannot claim | 2.27 ms | 2.40 ms | `ClaimRun` still runs: the stated residual |
| (d) backlog of 300 queued runs | 28.55 ms | 29.43 ms (+0.9 ms, +3%) | isolated statement unchanged, about 27.1 ms (indicative, see method note 2) |
| (e) row locked by another session | 6.74 ms | 6.94 ms | |

All outcomes were correct: (d) claimed and (e) idle.

**T5** (4 profiles: lane `''`/`run`/`cross_check`, reserve on/off, custody 8/1/0, recovery
on/off, an ephemeral lease, heavy vs fresh user). (a): 2.2 to 0.93-1.19 ms with 0 `ClaimRun`
calls. (c): +0.03 to +0.14 ms. Every profile's (c) claimed.

**End-to-end `Service.Claim`** (stage E, no-snapshot path `Claim(wkr, nil)`, base 0095b4361 vs
the fix). Configured like production: `WorkerHeartbeatStale` 45s, `WorkerSpreadGrace` 9s, per
`api/internal/config/config.go`. Medians of 3 reps, base to fix.

| Scenario | p50 | cold + 10 warm-ups | statements per call |
|---|---|---|---|
| (a) idle-empty | 2.81 to 1.53 ms (-1.28 ms, -45.6%) | 442.5 to 26.8 ms (-94%) | 12 to 9 (0 `ClaimRun`) |
| (b) queued-but-ineligible | 2.85 to 3.00 ms (+0.15 ms, +5%) | | 12 to 13 |
| (c) claimable | 21.82 to 22.71 ms (+0.88 ms, +4.0%) | | 42 to 43 |

In (c) every call claimed a distinct run and opened one hold; the per-rep p50 ranges do not
overlap (base 21.54-21.95, fix 22.23-22.77 ms). In (a) and (b) every call returned idle; the
recorded per-call statement counts (1 `ClaimRun` per call on base, and in (b) on the fix) show
the idle came from the claim seam, not an earlier exit. The
owner accepted about 0.25 ms (about 3%) of extra cost on claimable polls. The end-to-end
figure is +0.88 ms (+4%), recorded as measured: larger in absolute terms than the M1 segment
estimate, similar in relative terms.

**Improvement target (post hoc, proposed for owner review).** These thresholds were set after
measurement, so meeting them is a description of the result rather than independent evidence.
On an idle-empty owner queue, end-to-end `Service.Claim` p50 falls by at least 40% and a fresh
connection's first 11 polls by at least 90%, with `ClaimRun` sent 0 times; measured -45.6% and
-94%, with 0 `ClaimRun`. Proposed bound on non-empty queues: one extra round trip, at most about
1 ms per poll end to end. Measured: +0.88 ms (+4.0%) for claimable and +0.15 ms (+5.3%) for
queued-but-ineligible polls, inside that absolute bound. The claimable figure exceeds the about 0.25 ms
segment estimate the owner accepted before M3, and is reported with this PR rather than
treated as pre-approved.

**Method notes and deviations.**

1. Stage F (a fixture feasibility check: 13 cell clones, plus 4 profile-capture clones) and stage S (the scale diagnostic: 6 templates, each with 2 measured clones and 2 EXPLAIN clones, plus 1 capture clone per invocation) ran in addition to the pre-registered 119-cell table. Together with the table they exceed the plan's 120-cell cap. That expansion was not reported to the owner beforehand; the owner was told about S's results when choosing the fix, and F produced no timing claim.
2. When this record was taken, the harness also ran an isolated-statement pass in each M1 cell. That roughly doubled the `ClaimRun` executions above the plan's stated bound of 11,861, and it ran on the cell's clone after the segment phase, inheriting that phase's residue. The pass is now off by default (`UZI_CLAIMRUN_PLAN_ISOLATED=1` enables it), so a default reproduction stays within the execution bound and drops that second phase's extra residue. Within a claimable segment phase, each rolled-back call still leaves dead tuples for the next (autovacuum is off), equally for every candidate.
3. The harness re-runs ANALYZE on `runs`, `workers` and `cross_checks` in each clone after the scenario change (`UZI_CLAIMRUN_PLAN_CLONE_ANALYZE`, on by default). Each clone samples its own statistics, so at the larger scales a plan-cost or plan-choice difference between mode clones can partly reflect `ANALYZE` sampling rather than `plan_cache_mode`.
4. Profile p1 replays lane `''`, while the real service sends `'run'`. The SQL treats them identically.
5. The backlog (d) used 300 runs.
6. The fallback reserve R was not needed.

**Residual and limits.** A queued run this worker cannot claim (b) still pays a full
`ClaimRun` per poll. Whether production pays per-call planning, and why, is not established,
so the precheck removes the idle-queue cost but is not shown to remove the production
symptom in general.
