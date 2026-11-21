# ADR-4591: cap the api pool's per-connection statement cache at 128

**Status**: Accepted (issue #4591). The api pool now caps pgx's per-connection statement cache at 128 unless `DATABASE_URL` sets `statement_cache_capacity`. Connection recycling (jitter, shorter lifetimes) was measured and **not adopted**.
**Date**: 2026-11-21
**Deciders**: lead (plan), coder (measurement and implementation), reviewer; the DBA-facing caveats were reviewed by the owner.
**Related**: [ADR-1620](1620-custom-plan-cache-mode.md) keeps `plan_cache_mode = auto` on the same pool and supplies the latency method reused here; this ADR does not change it.
**PRD**: none. The spec lived in the issue body; this ADR records the measurement behind the decision.

## Decision (summary)

`OpenPool` now sets pgx's `StatementCacheCapacity` to **128** (pgx's own default is 512)
whenever the DSN does not set `statement_cache_capacity` itself. Every `OpenPool` caller gets
it, the api server included. Lifetime (1h), idle time (30m) and jitter (0) stay at pgx's
defaults, and so does `plan_cache_mode` ([ADR-1620](1620-custom-plan-cache-mode.md)).

- **What it bounds.** Each cached statement keeps its plan state in the PostgreSQL backend.
  Measured: about 154 KiB of backend memory contexts, and about 62 KiB of resident anonymous
  memory, per cached statement. A cap of 128 bounds the plan-cache portion of a backend's
  memory contexts at about 128 x 154 KiB.
- **What it saved, in this harness.** Steady backend heap (Anonymous) fell 16.7% with JIT off
  (68.6 to 57.1 MiB) and 3.8% with the server's default JIT on (70.0 to 67.4 MiB). The
  production saving is **unmeasured** and expected to fall between those two.
- **What it cost.** Headline latency is flat: `ClaimRun` about 7.1-7.3 ms and
  `InsertRunMessage` about 240 us for every candidate.
- **What was rejected.** A cap of 32 thrashes (a connection running more distinct statements
  than the cap re-prepares them every pass). Recycling connections on a shorter lifetime with
  jitter (candidates C and D) adds no saving and pays about 350 ms of planning per reconnect.
  No jitter was added.
- **It is a reversible default.** `statement_cache_capacity=512` in `DATABASE_URL` restores
  pgx's default. Never set it to `0`: with pgx's default exec mode it makes every
  parameterised query fail (see [the description-cache finding](#the-description-cache-does-not-apply-here)).

The incident-scale figure that motivated the issue, 110-160Mi per backend, was **not
reproduced or attributed** here (see [Limits](#limits)).

## Context

Issue #4591 reported api-pool backends holding 110-160Mi each. pgx caches every
parameterised statement it sends as a server-side prepared statement (its default
`QueryExecModeCacheStatement`, 512 entries per connection). The backend keeps each prepared
statement's parse tree and plan in its memory contexts, and a connection that has run a wide
set of statements over its life carries all of them until it is closed. The pool recycles a
connection after 1h (pgx default), and idle ones after 30m.

The candidate levers were: cap the statement cache; recycle connections sooner, with jitter
so they do not all recycle together; and the description cache. This ADR measures them
against steady backend memory, the hot-path latency [ADR-1620](1620-custom-plan-cache-mode.md)
protects, and the cost of a reconnect.

## Measurement

The harness is `TestPoolBackendMemoryMeasurement` in
`api/internal/store/pool_memory_measure_test.go`. It is opt-in: it runs only with
`UZI_POOL_MEMORY_MEASURE=1` and `UZI_TEST_DATABASE_URL`, and it refuses to run unless the
throwaway marker `uzi_test_harness.database_marker` = `recovery-reset-v1` is present. Its
name does not end in `LiveDB`, so CI never runs it. Numbers below are a recorded one-off from
a developer host, not a re-runnable fixture; the harness and the commands below make them
reproducible.

### How the method evolved

Four runs were made. The first three were superseded; they are recorded because the
differences explain why the numbers below are trustworthy and why the earlier ones are not.

1. **Run 1.** Savepoints taken per execution were never `RELEASE`d, so subtransactions nested
   about 1520 deep and inflated absolute memory (steady Anonymous for A was 93.4 MiB). All
   candidates shared one database.
2. **Run 2.** `RELEASE` was added and autovacuum turned off, but all candidates still shared
   one database. Rolled-back writes bloated that database across the sequential candidates,
   an order confound: the capped candidates read worse than A (B128's Anonymous rose from
   66.7 to 86.8 MiB between runs 1 and 2 while contexts stayed flat). Its per-candidate
   `Private_Clean` also jumped to about 50 MiB, which was not understood yet.
3. **Run 3.** Each candidate and rep ran on its own `CREATE DATABASE ... TEMPLATE pm_template`
   clone (schema and fixture seeded once, `ANALYZE`d), two reps, Anonymous as the primary heap
   metric. Order effects disappeared (A, B128 and B32 reproduced to 0.0 MiB across reps). A
   `Private_Clean` jump of about 51 MiB still appeared on every candidate that evicts or
   recycles, and was first read as shared-buffer pages.
4. **Run 4 (the primary results).** Showed that jump is **LLVM JIT**: `llvmjit.so` and
   `libLLVM.so.19.1` are mapped into a backend from round 8, because re-planned statements on
   the bloating clone cross `jit_above_cost` (100000). It added a `jit = off` control
   (`ALTER DATABASE <clone> SET jit = off`, measurement only, read back per backend with
   `SHOW jit`), a memory-map probe for LLVM, and an `EXPLAIN` split of `ClaimRun`. Run 4's
   JIT-off runs (a and c) are primary; run 4's jit-default run (b) shows the JIT effect.
   The earlier 3.7-3.8% saving for B128 (run 3) was JIT-biased; without JIT it is 16.7%.

### Why Anonymous is the ranking metric

`Private_Clean + Private_Dirty` (smaps_rollup) counts file-backed library text and
`shared_buffers` pages that only this one backend happens to map. That mixes in memory the
decision cannot change and that is shared across processes. `Anonymous` is the backend's
heap, the part a statement cache grows and shrinks, so the decision ranks on Anonymous.
The plan's "steady-state backend private" column is therefore presented as **Anonymous
steady**, with `Private_Clean` and `Private_Dirty` shown separately and labelled. `VmHWM`
includes shared pages and is labelled so. Neither is summed with anything.

### Environment

- **Server.** A throwaway `postgres:17` container (PostgreSQL 17.12, Debian
  `17.12-1.pgdg13+1`), `shared_buffers` 128MB, `max_connections` 100, `--shm-size=1g`,
  `-c autovacuum=off`; server `jit` on, `jit_above_cost` 100000, `pg_jit_available()` true.
- **Host.** `nproc` 4 (Go's `runtime.NumCPU` reports 24), 100 GiB RAM, container cgroup
  `memory.max` = `max` (unlimited), Go 1.27.3.
- **Schema.** Migrated through `store.Migrate` to the repository's head at measurement time
  (500), much later than [ADR-1620](1620-custom-plan-cache-mode.md)'s 251. `ClaimRun` is
  correspondingly larger: about 7 ms here against ADR-1620's 1.1 ms. **Only relative numbers
  compare across the two ADRs.**
- **Fixture.** ADR-1620's shape (a user with 300 completed runs of 4 usage legs, a second user
  of the same shape, 200 noise users) plus 2000 base messages, one `running` run with 2000
  messages and one claimable `queued` run. Totals: page of 200 rows, 198 totals rows,
  `SelfUsage.run_count` 300.
- **Workload.** The api's real sqlc `Queries` methods: a breadth set of 313 methods (across 80
  of the 120 `*.sql.go` files; single-argument reads, getters, locks and deletes keyed by a
  fixture id, run in one transaction under `SAVEPOINT` / `ROLLBACK TO` / `RELEASE` so writes
  never persist), plus the hot mix (`ClaimRun`, `HeartbeatWorker`, `InsertRunMessage`, the run
  list and usage reads). 317 distinct statements are retained on a connection under pgx's
  default cache; the repository holds 1691 sqlc query texts.
- **Candidates.** A: pgx defaults (cache 512, lifetime 1h, no jitter). B128 and B32:
  `statement_cache_capacity` 128 and 32, otherwise defaults. C: cache 512 with a compressed
  lifetime of 45 s plus up to 10 s jitter (production would use minutes), health check 5 s.
  D: cache 128 with the same compressed lifetime and jitter. C and D recycled 4 times each.
- **Isolation.** Every candidate, rep, latency round and wide-read run got its own database
  clone and its own observer connection, dropped afterwards.
- **Steady state.** The median of the later half of post-round samples taken while the
  backend had at least 160 statement identities.

### Commands

The password is a placeholder. Port 47115 was free (bind probe).

```sh
docker run -d --rm --shm-size=1g --name cdr-pool4591-<n> \
  -e POSTGRES_USER=uzi -e POSTGRES_DB=uzi -e POSTGRES_PASSWORD=<pw> \
  --mount type=bind,source=$PWD/e2e/test-database-marker.sql,target=/docker-entrypoint-initdb.d/10-uzi-test-database-marker.sql,readonly \
  -p 127.0.0.1:47115:5432 postgres:17 -c autovacuum=off

cd api && export GOFLAGS=-buildvcs=false
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null UZI_POOL_MEMORY_MEASURE=1 \
  UZI_TEST_DATABASE_URL='postgres://uzi:<pw>@127.0.0.1:47115/uzi?sslmode=disable'
# (a) jit=off control, memory + explain, 2 reps
UZI_POOL_MEMORY_ROUNDS=24 UZI_POOL_MEMORY_REPS=2 UZI_POOL_MEMORY_JIT=off \
  UZI_POOL_MEMORY_PHASES=memory,explain UZI_POOL_MEMORY_OUT=out-a.jsonl \
  go test -count=1 -timeout 58m -run '^TestPoolBackendMemoryMeasurement$' -v ./internal/store
# (b) server default jit, memory, 1 rep
UZI_POOL_MEMORY_ROUNDS=24 UZI_POOL_MEMORY_REPS=1 UZI_POOL_MEMORY_JIT=default \
  UZI_POOL_MEMORY_PHASES=memory UZI_POOL_MEMORY_OUT=out-b.jsonl \
  go test -count=1 -timeout 58m -run '^TestPoolBackendMemoryMeasurement$' -v ./internal/store
# (c) jit=off, latency + cold reconnect + stress
UZI_POOL_MEMORY_JIT=off UZI_POOL_MEMORY_PHASES=latency,cold,stress UZI_POOL_MEMORY_OUT=out-c.jsonl \
  go test -count=1 -timeout 58m -run '^TestPoolBackendMemoryMeasurement$' -v ./internal/store
```

The three runs took 1626 s, 814 s and 458 s on the same container.

### Memory, JIT off (primary)

Run 4 (a), two reps; a single value means the reps were identical. Anonymous steady is the
heap metric. `Private_Dirty` shows rep 1 / rep 2. Context totals are
`pg_backend_memory_contexts` total / used. `VmHWM` includes shared pages.

| setting | steady backend heap (Anonymous), MiB | sampled peak (Anonymous), MiB | Private_Clean / Private_Dirty, MiB | VmHWM (incl. shared), MiB | context totals, MiB | retained statements |
|---|---|---|---|---|---|---|
| A: pgx defaults (cache 512) | 68.6 | 81.1 | 0.9 / 73.7, 71.9 | 125.6, 121.2 | 53.5 / 38.2 | 317 |
| B128: cache 128 | 57.1 (-16.7%) | 57.1 | 0.9 / 60.0, 60.6 | 108.2, 107.3 | 25.1 / 18.4 | 128 |
| B32: cache 32 | 51.3 (-25.2%) | 51.3 | 0.9 / 54.0, 54.7 | 101.9, 101.4 | 17.4 / 13.3 | 32 |
| C: lifetime 45 s + 10 s jitter | 70.8, 66.5 | 82.2, 86.1 | 0.9 / 73.8, 69.5 | 148.6, 152.8 | 53.4 / 38.3 | 317 |
| D: cache 128 + lifetime/jitter | 56.6, 53.9 | 60.2, 57.7 | 0.9 / 59.9, 57.0 | 118.6, 118.9 | 25.0 / 18.5 | 128 |

Reps matched to 0.0 MiB for A, B128 and B32. C varied by 4.3 MiB and D by 2.7 MiB between
reps (recycled connections, so the sampled instant differs). C is about A (+2.2 and -2.1
MiB); D is about B128. Marginal cost per cached statement, `(A - B128)` over the 189
statements it retains more: **157,506 B (about 154 KiB) of memory contexts and 63,369 B
(about 62 KiB) of Anonymous**. Contexts fall more than Anonymous because freed plan memory is
mostly not returned to the operating system by the allocator.

### Memory, server default JIT

Run 4 (b), one rep. `Private_Clean` is the mapped LLVM library text.

| setting | steady backend heap (Anonymous), MiB | sampled peak (Anonymous), MiB | Private_Clean / Private_Dirty, MiB | VmHWM (incl. shared), MiB | context totals, MiB | LLVM mapped (first round) |
|---|---|---|---|---|---|---|
| A: pgx defaults | 70.0 | 81.0 | 0.9 / 74.4 | 125.1 | 53.5 / 38.3 | no |
| B128 | 67.4 (-3.8%) | 72.7 | 51.1 / 71.7 | 175.8 | 25.1 / 18.4 | yes (8) |
| B32 | 59.8 (-14.6%) | 65.2 | 51.2 / 64.1 | 168.3 | 17.4 / 13.3 | yes (8) |
| C | 79.5 | 94.5 | 51.1 / 83.8 | 217.4 | 53.4 / 38.2 | yes (10) |
| D | 69.4 | 75.1 | 51.1 / 73.6 | 188.0 | 25.0 / 18.4 | yes (8) |

JIT added about 8.5-14 MiB of Anonymous to the candidates that evict or recycle (B128 +10.3,
B32 +8.5, C +10.9, D +14.1 against the jit-off medians) and nothing material to A (70.0
against 68.6): in the harness A never re-plans, because nothing invalidates its plans. The
LLVM text (about 51 MiB) is shared file cache, counted once per PostgreSQL instance, not per
backend. A fresh jit-default backend maps `llvmjit.so` and `libLLVM.so.19.1` after a single
`pg_jit_available()` call, and a jit-off backend maps neither.

Which re-planned statement crosses `jit_above_cost` on the bloating clone was **not
identified**. `ClaimRun` itself never JITs (its cost is about 2468 custom / 2502 generic, far
below the threshold, and no execution showed a JIT block). Only the effect is measured.

### Latency and reconnect (jit off)

Run 4 (c). Method as [ADR-1620](1620-custom-plan-cache-mode.md): N = 500 executions through
the sqlc methods on one connection after warm-up, 5 rounds with the leading candidate
rotated, the median round's per-execution mean. The columns are that mean in microseconds.

| setting | `ClaimRun` | `InsertRunMessage` | stress `ClaimRun` | stress `InsertRunMessage` | cold reconnect: first / second `ClaimRun`, ms |
|---|---|---|---|---|---|
| A | 7129 | 238 | 7344 | 238 | 99.5 / 65.0 |
| B128 | 7184 | 239 | 7545 | 245 | 98.5 / 65.3 |
| B32 | 7263 | 245 | **68449** | **977** | 102.6 / 64.2 |
| C | 7251 | 239 | 7333 | 240 | 102.7 / 66.1 |
| D | 7226 | 238 | 7535 | 244 | 103.9 / 67.4 |

- **Headline latency is flat**: the differences sit inside the round-to-round spread
  (A's five round means span 7107-7241 us). The same held in run 3 with default JIT
  (`ClaimRun` 6999-7051, `InsertRunMessage` 229-231).
- **Stress variant** (40 distinct breadth calls between hot uses, N = 200): B32's `ClaimRun`
  is about 68 ms against 7.3-7.5 ms for the others, and `InsertRunMessage` about 0.98 ms against
  about 0.24 ms. A cap smaller than the number of distinct statements between two uses evicts
  the hot statement each pass: a thrash.
- **Cold reconnect.** Connecting takes about 5 ms. The first `ClaimRun` on an empty cache
  takes about 100-106 ms and the second about 64-68 ms, for every candidate alike.

`EXPLAIN (ANALYZE, SUMMARY)` of `ClaimRun` on one connection, four runs (jit default and off,
two reps each), per execution: planning about 69-75 ms on the first, about 50-58 ms on
executions 2 to 5, about 60-65 ms on the sixth (when PostgreSQL builds the generic plan);
execution about 19 ms, then about 8 ms. Planning dominates. The six executions sum to about
350 ms of planning (348-358 ms across the four runs). **A re-prepare after an eviction or a
reconnect therefore costs about 350 ms of planning once per statement, for `ClaimRun`.**
Recycling connections (C, D) pays that on every reconnect and saved nothing over A and B128.

### Wide-read sample (a lower bound, not an envelope)

Run 3, server default JIT. Every sqlc text (1691) run once per parameter with NULL
arguments inside `BEGIN READ ONLY`, with a savepoint per statement. 705 execute; 986 are
writes the read-only transaction refuses after planning, and pgx then invalidates them, so
they are never retained; NULL arguments plan small. It is therefore a **lower bound and not
an upper bound**: it shows no worse case than what it ran, and a real wide workload could
retain more or cost more per statement.

| statement cache | plan mode | retained | Anonymous, MiB | VmHWM (incl. shared), MiB | context totals, MiB |
|---|---|---|---|---|---|
| 4096 (holds every statement) | `auto` | 705 | 97.1 | 128.6 | 80.7 / 54.6 |
| 4096 | `force_generic_plan` | 705 | 136.5 | 170.0 | 123.2 / 86.0 |
| 512 (pgx default) | `auto` | 512 | 93.2 | 124.7 | 63.4 / 42.8 |
| 512 | `force_generic_plan` | 512 | 124.6 | 156.2 | 97.4 / 67.6 |

### The description cache does not apply here

pgx v5.12.0 (the version in `api/go.mod`) reads `statement_cache_capacity` and
`description_cache_capacity` from the connection's runtime parameters and removes them, so
neither reaches PostgreSQL (`conn.go`):

```go
statementCacheCapacity := 512
if s, ok := config.RuntimeParams["statement_cache_capacity"]; ok {
	delete(config.RuntimeParams, "statement_cache_capacity")
	n, err := strconv.ParseInt(s, 10, 32)
	...
	statementCacheCapacity = int(n)
}

descriptionCacheCapacity := 512
if s, ok := config.RuntimeParams["description_cache_capacity"]; ok {
	delete(config.RuntimeParams, "description_cache_capacity")
	...
}

defaultQueryExecMode := QueryExecModeCacheStatement
```

The description cache is only read under `QueryExecModeCacheDescribe`. In `exec()`:

```go
switch mode {
case QueryExecModeCacheStatement:
	if c.statementCache == nil {
		return pgconn.CommandTag{}, errDisabledStatementCache
	}
	sd := c.statementCache.Get(sql)
	if sd == nil {
		sd, err = c.Prepare(ctx, stmtcache.StatementName(sql), sql)
		...
		c.statementCache.Put(sd)
	}
	return c.execPrepared(ctx, sd, arguments)
case QueryExecModeCacheDescribe:
	if c.descriptionCache == nil {
		return pgconn.CommandTag{}, errDisabledDescriptionCache
	}
	sd := c.descriptionCache.Get(sql)
	if sd == nil {
		sd, err = c.Prepare(ctx, "", sql)
		...
		c.descriptionCache.Put(sd)
	}
	return c.execParams(ctx, sd, arguments)
```

`getStatementDescription` (Query, QueryRow) and `sendBatchQueryExecModeCacheDescribe` follow
the same split. The only mode-independent touch is `RemoveInvalidated()` on the description
cache, a no-op when nothing was `Put` there. The api sets no `default_query_exec_mode`, so it
runs `QueryExecModeCacheStatement`: **`description_cache_capacity` has no effect on the api
pool and was not a candidate.**

The same excerpts give the `0` hazard. With `statement_cache_capacity=0` the statement cache
is nil (`if c.config.StatementCacheCapacity > 0 { c.statementCache = ... }`), and the
`QueryExecModeCacheStatement` branch returns `errDisabledStatementCache` ("cannot use
QueryExecModeCacheStatement with disabled statement cache"). A statement with no arguments
uses the simple protocol and is unaffected, so a pool at `0` fails every parameterised query.
`0` is only valid together with a different `default_query_exec_mode`.

The statement cache is an LRU that evicts on `Put` at capacity, and the evicted statement is
deallocated before the next query (`internal/stmtcache/lru_cache.go`, `conn.go`):

```go
if c.len == c.cap {
	c.invalidateOldest()
}
```

```go
func (c *Conn) deallocateInvalidatedCachedStatements(ctx context.Context) error {
	...
	for _, sd := range invalidatedStatements {
		pipeline.SendDeallocate(sd.Name)
	}
```

`deallocateInvalidatedCachedStatements` runs at the start of `Exec`, `Query` and
`SendBatch`. The pool's lifetime settings are read the same way (`pgxpool/pool.go`):
`defaultMaxConnLifetime = time.Hour`, `defaultMaxConnIdleTime = time.Minute * 30`,
`pool_max_conn_lifetime_jitter` unset means 0, and a connection past its lifetime is
destroyed on release.

## Decision and rationale

The default was to land only if a cap bounded backend memory without moving the hot paths.

- **128 clears that bar.** It cuts the plan-cache portion of a backend by about 154 KiB per
  statement it no longer holds, leaves headline latency flat, and in the harness cut steady
  heap by 16.7% with JIT off. With JIT on the measured saving is only 3.8%, because
  evicting and re-planning on the bloated clone triggered JIT, which maps about 51 MiB of
  shared library text and about 8.5-14 MiB of Anonymous. The production saving is
  unmeasured. It depends on how many distinct statements a connection runs, which the
  harness fixes at 317, and on whether any re-planned statement crosses `jit_above_cost`.
- **32 is rejected.** It saves more heap (51.3 MiB) but thrashes: its stress `ClaimRun` is
  about 68 ms against about 7 ms.
- **C and D are rejected.** Recycling on a compressed lifetime with jitter saved nothing
  over A (C) or over B128 (D), and every reconnect re-pays about 350 ms of planning per hot
  statement. No jitter was added to the pool.
- **The mechanism is the DSN key.** `poolConfig` reads `pgconn.ParseConfig(dsn)`'s
  `RuntimeParams["statement_cache_capacity"]` and sets the default only when it is absent,
  because pgx deletes the key from the parsed config and a value of 512 is indistinguishable
  from pgx's own default. This honours the URL, keyword/value and service-file forms (tests:
  `TestPoolConfigDefaults`, `TestPoolConfigDSNOverridesStatementCache`,
  `TestPoolConfigServiceFileOverridesStatementCache` in
  `api/internal/store/pool_config_test.go`; the plan named `pool_test.go`, which is a minor
  deviation). Red/green was observed: removing the apply block fails the default tests with
  "StatementCacheCapacity = 512, want 128", and ignoring the DSN fails the override tests.

## Consequences

- **Production working-set risk.** The hot path survives while fewer than about 126 other
  distinct statements run on a connection between two of its uses. Beyond that the LRU
  evicts it and each use re-prepares, and the thrash signature is `ClaimRun` p50 rising from
  about 7 ms toward 60+ ms. The remedy is to raise `statement_cache_capacity` in
  `DATABASE_URL`. To watch for it, compare `ClaimRun` latency after deploy; or, with the
  infrastructure owner's agreement, compare `pg_stat_statements` plans to calls with
  `track_planning` on.
- **JIT is the larger lever when it engages.** The operator levers, by reach:
  - `ALTER ROLE <app-role> SET jit = off;` takes effect for **new sessions only**, so
    existing pool connections keep JIT until they recycle (up to 1h) or the api restarts.
  - `jit=off` as a key in the `DATABASE_URL` query: pgx passes unknown keys through as
    startup parameters. The chart ships no PgBouncer. A transaction-mode PgBouncer would
    drop it (see [ADR-1620](1620-custom-plan-cache-mode.md)).
  - `ALTER DATABASE ... SET jit = off`, or a server-wide setting, is the infrastructure
    owner's call.

  No `jit_above_cost` value is recommended. Identify the statements that JIT first
  (`pg_stat_statements` `jit_functions > 0`, or `EXPLAIN`).
- **Sizing is an estimate, not a hard bound:**
  **api replicas × per-api DB_MAX_CONNS × per-backend memory**, where api replicas means
  the number of api pools connected to the same primary (CNPG standby instances are not part
  of the multiplier). Use about 60 MiB per backend with JIT off at the default cap of 128
  (57.1 MiB steady Anonymous, 60.0-60.6 MiB Private_Dirty) and about 75 MiB when JIT engages
  (67.4 steady, 72.7 sampled peak Anonymous, 71.7 Private_Dirty). One replica at 20
  connections is about 1.2 GiB with JIT off and about 1.5 GiB with JIT on. Add what is
  counted once (`shared_buffers`, the shared LLVM library page cache, PostgreSQL background
  processes), headroom for non-api connections (workers, migrations, CNPG's own), concurrent
  query memory (up to `work_mem` per sort or hash plan node) and other PostgreSQL and
  container overhead. It assumes every pool connection has been opened and used; pgxpool
  opens connections lazily and reaps idle ones after 30m. The chart's api Deployment uses
  `Recreate` (`deploy/chart/templates/api-deployment.yaml`), so there is no rolling-update
  overlap term. Measured on PostgreSQL 17.12 with a small synthetic fixture and 100 ms
  sampling; CNPG deployments may run a different major version.
- **Operator documentation** is
  [Database connection memory and sizing](../docs/configuration.md#database-connection-memory-and-sizing-issue-4591).
- **Not scheduled.** A production measurement of the saving, and an identified JIT-triggering
  statement, would sharpen the 3.8%-16.7% range. Neither was done.

## Limits

- **Small synthetic fixture.** 2000 messages, 300 runs per measured user, 313 breadth methods
  and 317 retained statements against about 1691 named queries in the repository. Plan sizes
  and result sizes are not production-shaped; many breadth entries return empty rows.
- **The incident scale was not reproduced.** A's `VmHWM` reached 121.2-125.6 MiB in run 4
  and 124.7-128.6 MiB in the wide-read sample, but `VmHWM` includes shared pages, so it is
  not the per-backend heap, and nothing here attributes the 110-160Mi reported.
- **One measured PostgreSQL version and host**: 17.12 with `shared_buffers` 128MB.
- **JIT figures are one rep** (run 4 b) and compare across two runs on the same container;
  the jit-off figures are two reps.
- **Sampling.** Memory is sampled from `/proc` at a 100 ms target (mean 100.0 ms, p95 100.8 ms
  in run 3; run 2 recorded a maximum interval of 102.9-105.0 ms). A peak shorter than the
  interval can fall between samples, so the sampled peak is a lower bound on the true peak;
  `VmHWM` is the kernel's high-water mark but includes shared pages. No sampling errors
  occurred in run 4.
- **cgroup `memory.current` is unavailable for the primary runs.** Runs 3 and 4 recorded no
  cgroup column in their summaries and their raw cgroup files hold only the empty-cache
  baseline capture, so the container-wide figure cannot be tied to a candidate. The only
  per-candidate cgroup values (run 1, superseded) are container-wide, including the page
  cache and every backend, so they are not per-backend numbers either. The limit is that
  total container memory under each candidate is unverified; the ranking rests on per-backend
  Anonymous.
- **The `SAVEPOINT` caveat.** [ADR-1620](1620-custom-plan-cache-mode.md)'s harness did not
  `RELEASE` its savepoints, so its absolute memory and latency carry the nesting growth run 1
  here showed.
- **`description_cache_capacity` was reasoned from the pgx source, not measured.**
