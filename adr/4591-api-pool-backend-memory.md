# ADR-4591: api pool backend memory — no default change; measurement inconclusive

**Status**: Accepted: no default change; measurement inconclusive (issue #4591). `OpenPool` keeps pgx's defaults.
**Date**: 2026-11-21
**Deciders**: lead (plan), coder (measurement), reviewer, dba, fact-checker; owner (review decision to keep the current defaults).
**Related**: [ADR-1620](1620-custom-plan-cache-mode.md) keeps `plan_cache_mode = auto` on the same pool and supplies the latency method reused here; this ADR does not change it. Issue #4602 later measured `ClaimRun`'s idle planning/execution split, found that `auto` switches to the generic plan after 5 custom plans, and added an idle precheck query at the claim seam; recorded in [ADR-1620](1620-custom-plan-cache-mode.md#measurement-update-4602-2026-11-22).
**PRD**: none. The spec lived in the issue body; this ADR records the measurement and why it did not change a default.

## Decision (summary)

`OpenPool` keeps pgx's defaults: `statement_cache_capacity` 512, connection lifetime 1h, idle
time 30m, lifetime jitter 0. `plan_cache_mode` stays as decided in
[ADR-1620](1620-custom-plan-cache-mode.md). No default changes.

The measurement below is **inconclusive for a global default change** (see
[Why the measurement is inconclusive](#why-the-measurement-is-inconclusive)). That is not a
finding that the candidates are ineffective. Three candidates are **not adopted from this
evidence**:

- a statement cache capacity of 128;
- a statement cache capacity of 32;
- shorter connection lifetimes with jitter (candidates C and D).

The observations the harness produced are kept below as **synthetic observations under the
confounds listed**, not as conclusions:

- With JIT off, steady backend heap (Anonymous) was 68.6 MiB at pgx's defaults and 57.1 MiB at
  a 128 cache (-16.7%). With the server's default JIT on, 70.0 vs 67.4 MiB (-3.8%), on this
  synthetic workload, with one rep; production effect unmeasured.
- A cache of 32 showed a stress `ClaimRun` of about 68 ms against about 7 ms for the others.
- `ClaimRun` planning in this harness: the first execution on a fresh connection (after a
  reconnect) spent about 69-75 ms planning, and the six prepared executions together about 348-358 ms
  (see [Latency and reconnect](#latency-and-reconnect-jit-off)).

Never set `statement_cache_capacity` to `0`: with pgx's default exec mode it makes every
query fail except an argument-less `Exec` (see
[the description-cache finding](#the-description-cache-does-not-apply-here)). `0` is only
valid together with a different `default_query_exec_mode`.

The incident-scale figure that motivated the issue, 110-160Mi per backend, was **not
reproduced or attributed** here (see [Limits](#limits)).

## History

An earlier revision of this MR implemented a 128 fallback in `OpenPool`, applied when the DSN
did not set `statement_cache_capacity`. On owner review it was reverted: the measurement is
inconclusive for a global default change. `OpenPool` is unchanged relative to the base.

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
a developer host, not a re-runnable fixture; the harness and the commands below make the
method re-runnable.

### How the method evolved

Four runs were made. The first three were superseded; they are recorded because the
differences explain why the earlier numbers were set aside. The later numbers are still synthetic observations under the confounds in [Why the measurement is inconclusive](#why-the-measurement-is-inconclusive).

1. **Run 1.** Savepoints taken per execution were never `RELEASE`d, so each `SAVEPOINT` nested inside
   the previous one, so subtransaction depth grew by one per call within each transaction (one
   level per breadth call and per latency-loop execution), and that inflated absolute memory (steady Anonymous for A was 93.4 MiB). All
   candidates shared one database.
2. **Run 2.** `RELEASE` was added and autovacuum turned off, but all candidates still shared
   one database. Rolled-back writes bloated that database across the sequential candidates,
   an order confound: the capped candidates read worse than A (B128's Anonymous rose from
   66.7 to 86.8 MiB between runs 1 and 2 while its contexts fell from 31.1 to 25.1 MiB). Its per-candidate
   `Private_Clean` also jumped to about 50 MiB, which was not understood yet.
3. **Run 3.** Each candidate and rep ran on its own `CREATE DATABASE ... TEMPLATE` clone of the template
   (then named `pm_template`; schema and fixture seeded once, `ANALYZE`d), two reps, Anonymous as the primary heap
   metric. Order effects disappeared (A, B128 and B32 reproduced to 0.0 MiB across reps). A
   `Private_Clean` jump of about 51 MiB still appeared on every candidate that evicts or
   recycles, and was first read as shared-buffer pages.
4. **Run 4 (the primary results).** Observed that the jump coincides with LLVM being mapped:
   `llvmjit.so` and `libLLVM.so.19.1` appear in a backend's memory map from round 8 (round 10 for C) in the
   candidates that evict or recycle, and `jit = off` removes both the mapping and the jump. A
   library mapping alone is not proof of a JIT compilation (see the `pg_jit_available()` probe
   below). The hypothesis, not established, is that re-planned statements on the bloating clone
   cross `jit_above_cost` (100000); the specific statement was not identified. It added a `jit = off` control
   (`ALTER DATABASE <clone> SET jit = off`, measurement only, read back per backend with
   `SHOW jit`), a memory-map probe for LLVM, and an `EXPLAIN` split of `ClaimRun`. Run 4's
   JIT-off runs (a and c) are primary; run 4's jit-default run (b) shows the JIT effect.
   The earlier 3.7-3.8% saving for B128 (run 3) was JIT-biased; in run 4's jit-off run it was 16.7% (a synthetic observation).

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
- **Isolation.** Every memory candidate and rep, and every wide-read run, got its own
  database clone and its own observer connection, dropped afterwards. The latency phase is
  isolated per round, not per candidate: each round's clone is shared by all five candidates
  in a rotated order, so later candidates in a round see that round's rolled-back-write bloat.
  The rotation balances position across rounds but does not give each candidate an identical
  starting state. Headline (k=1) latency differences between candidates were comparable to
  the round-to-round spread; the supplemental stress variant did not stay flat (B32's stress
  `ClaimRun` was about 68 ms against about 7 ms for the others).
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
# (d) wide-read sample (run 3's wide section; see the provenance caveat below)
UZI_POOL_MEMORY_JIT=default UZI_POOL_MEMORY_PHASES=wide UZI_POOL_MEMORY_OUT=out-d.jsonl \
  go test -count=1 -timeout 58m -run '^TestPoolBackendMemoryMeasurement$' -v ./internal/store
```

The three runs (a to c) took 1626 s, 814 s and 458 s on the same container.

Each invocation now names its databases `pm_<32-hex uuid>_...` and drops only the databases it
created. A crashed run can leave them behind, and nothing sweeps them automatically. To clean
up after one, take that invocation's exact `database_prefix` from its `meta` output record and
confirm the invocation has stopped, then drop only the databases named `<database_prefix>_...`.
`SELECT datname FROM pg_database WHERE datname ~ '^pm_[0-9a-f]{32}_'` is an inventory of every
invocation's databases, active runs included, never a deletion list. If the prefix or the
stopped state cannot be established, drop nothing.

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

Observations: the jit-default run's Anonymous exceeded the jit-off medians by about 8.5-14 MiB
for the candidates that evict or recycle (B128 +10.3, B32 +8.5, C +10.9, D +14.1) and by 1.4
MiB for A (70.0 against 68.6). LLVM was mapped in the first round shown above for B128, B32,
C and D and not for A. The LLVM text (about 51 MiB) is shared file cache, counted once per
PostgreSQL instance, not per backend. A fresh jit-default backend maps `llvmjit.so` and
`libLLVM.so.19.1` after a single `pg_jit_available()` call, and a jit-off backend maps neither.
So a library mapping alone is not proof of a JIT compilation: the probe maps LLVM without one.

**Hypothesis, not established:** re-planned statements on the bloating clone cross
`jit_above_cost`, which would explain why the effect appears on backends that evict or
recycle. Which statement that would be was **not identified**. `ClaimRun` itself never JITs
(its cost is about 2468 custom / 2502 generic, far below the threshold, and no execution
showed a JIT block). Only the memory differences and the mappings are observed.

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

- **Headline latency**: in this harness the differences between candidates sat inside the
  round-to-round spread (A's five round means span 7107-7241 us), under jit off and a clone
  shared by all candidates in a round, so this is an observation and not a cost conclusion. The same held in run 3 with default JIT
  (`ClaimRun` 6999-7051, `InsertRunMessage` 229-231).
- **Stress variant** (40 distinct breadth calls between hot uses, N = 200): B32's `ClaimRun`
  is about 68 ms against 7.3-7.5 ms for the others, and `InsertRunMessage` about 0.98 ms against
  about 0.24 ms. A cap smaller than the number of distinct statements between two uses evicts
  the hot statement each pass: a thrash.
- **Cold reconnect.** Connecting takes about 5 ms. The first `ClaimRun` on an empty cache
  takes about 98.5-103.9 ms and the second about 64-68 ms, for every candidate alike.

`EXPLAIN (ANALYZE, SUMMARY)` of `ClaimRun` on one connection, four runs (jit default and off,
two reps each), per execution: planning about 69-75 ms on the first, about 50-58 ms on
executions 2 to 5, about 60-65 ms on the sixth (when PostgreSQL builds the generic plan);
execution about 19 ms, then about 8 ms. Planning dominates. 348-358 ms (across the four runs)
is the **sum** of planning over the six prepared executions, paid across those executions
rather than at once. In
this harness, the first execution on a fresh connection (after a reconnect) pays about 69-75 ms of
planning (only the reconnect case was measured: `pmRunExplain` uses a fresh connection); the further custom planning (executions 2 to 5, about 50-58 ms each) and the generic
planning (the sixth, about 60-65 ms) follow on later executions, and a re-prepare on a warm connection restarts the same
custom-then-generic planning sequence, but its planning time was not measured separately. These are distinct from the cold-reconnect latencies above (98.5-103.9 ms first,
64-68 ms second `ClaimRun`). They are `ClaimRun` observations only. C showed no steady-heap saving over A; D's two jit-off
reps (56.6 and 53.9 MiB) were below B128's 57.1 MiB, within a 2.7 MiB rep-to-rep spread, and above it with JIT on
(69.4 against 67.4 MiB). Neither difference is established by this evidence.

### Wide-read sample (a lower bound, not an envelope)

Run 3, server default JIT. The table records that run's figures. The current harness prepares
every sqlc text (1691) and executes each successfully prepared text twice with NULL arguments
(one per parameter) inside `BEGIN READ ONLY`, with a savepoint per statement; its outcome tallies
count the first pass only. Whether run 3 used exactly this method is not established (see
below). The recorded tallies are: 705 execute;
986 are writes the read-only transaction refuses after planning, and pgx then invalidates
them, so they are never retained; NULL arguments plan small. It is therefore a **lower bound
and not an upper bound**: it shows no worse case than what it ran, and a real wide workload could
retain more or cost more per statement.

| statement cache | plan mode | retained | Anonymous, MiB | VmHWM (incl. shared), MiB | context totals, MiB |
|---|---|---|---|---|---|
| 4096 (holds every statement) | `auto` | 705 | 97.1 | 128.6 | 80.7 / 54.6 |
| 4096 | `force_generic_plan` | 705 | 136.5 | 170.0 | 123.2 / 86.0 |
| 512 (pgx default) | `auto` | 512 | 93.2 | 124.7 | 63.4 / 42.8 |
| 512 | `force_generic_plan` | 512 | 124.6 | 156.2 | 97.4 / 67.6 |

Every committed harness revision since commit 5f9b058a5 contains this two-pass loop, but the
revision that produced run 3 is not recorded. Exact historical reproduction is therefore not
established, and re-running command (d) is not expected to give identical numbers.

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

pgx builds the description cache from the key regardless, but uses it only under `QueryExecModeCacheDescribe`. In `exec()`:

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
pool under the default cached-statement exec mode and was not a candidate.**

The same excerpts give the `0` hazard. With `statement_cache_capacity=0` the statement cache
is nil (`if c.config.StatementCacheCapacity > 0 { c.statementCache = ... }`), and the
`QueryExecModeCacheStatement` branch returns `errDisabledStatementCache` ("cannot use
QueryExecModeCacheStatement with disabled statement cache"). In pgx v5.12.0 only an argument-less `Exec` uses the simple protocol. An argument-less
`Query` or `QueryRow` still goes through the statement description path and fails with
`errDisabledStatementCache`, so a pool at `0` fails every query except an argument-less
`Exec` under the default mode.
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
`pool_max_conn_lifetime_jitter` unset means 0. A connection past its lifetime leaves the pool
by one of three paths (pgx v5.12.0): pgxpool's background health check (`checkConnsHealth`)
closes idle connections that are past their lifetime; `Conn.Release` destroys a checked-out
connection that is past its lifetime when it is released (it is not interrupted before that);
and `Pool.Acquire` destroys an expired idle connection instead of handing it out. So the time at which a given connection is replaced has no fixed deadline.

## Why the measurement is inconclusive

- **Breadth checks pin only a result class.** `pmClassify` / `pmRunEntry` assert that each
  entry returned some rows, no rows or `ErrNoRows`. They do not check exact rows, values or
  write effects.
- **Hot-mix and fixture checks assert only specific fields.** `ClaimRun` id and status
  `claimed`; `InsertRunMessage` inserted and `generation_live`; `ListRunsForUser` and
  `ListRunUsageTotalsForRuns` row counts; `SelfUsage` `run_count`; `Ping`;
  `SchemaVersionStatus` at head; `HeartbeatWorker` id; and `verifyFixture`'s selected
  aggregates (queued-run status, message counts, custody holds, `claim_generation`). Complete
  row values and write effects are not verified.
- **Isolation differs by phase.**
  - Memory: one clone per candidate and rep.
  - Latency and stress: one clone per round, shared by all candidates in that round. Later
    candidates inherit bloat from rolled-back writes and from `verifyFixture`'s heartbeat
    `UPDATE`.
  - Cold: one clone shared by all candidates and reps.
  - Wide: one clone per cache size and plan mode.
  - Explain: one clone per jit mode and rep; a capture connection runs a real `ClaimRun` on
    it before the measured connection.
  - The template (`pm_template` in the recorded runs; `pm_<32-hex uuid>_tpl` per invocation in the
    current harness) is still read by the validation pool,
    and discover mode runs on it. All clones share one server.
- **The wide phase records other errors rather than failing**, so a wide run can complete
  with unexplained error rows.
- **JIT confounds the memory comparison.** The headline latency run (c) ran with `jit = off`,
  while the normal-JIT memory run (b) showed extra Anonymous memory. The normal-JIT 128 vs A
  difference was 3.8% (70.0 to 67.4 MiB) on this synthetic workload, with one rep;
  production effect unmeasured.
- **Candidates are confounded with each other.** C and D change lifetime together with, or
  against, the cache cap, and the JIT effect varies by candidate, so no preference among the
  candidates is justified by this evidence.

## Decision and rationale

The default would have changed only if a lower cache cap or shorter lifetime clearly bounded
backend memory without moving the hot paths. The evidence does not establish that for a
global default, so `OpenPool` keeps pgx's defaults.

- **128, 32 and lifetime/jitter (C, D) are candidates not adopted from this evidence.** They
  are not rejected or disproven; a measurement without the confounds above could change the
  picture.
- **Synthetic observations, under the confounds.**
  - 128 cut about 154 KiB of memory contexts per statement it no longer holds (this fixture's
    average) and steady heap by 16.7% with JIT off; with JIT on the difference was 3.8%
    (see above). In the jit-default run, the candidates that evict or recycle showed LLVM
    mapped (about 51 MiB of shared library text) and about 8.5-14 MiB more Anonymous than their
    jit-off runs. That these backends re-plan statements crossing `jit_above_cost` is a
    hypothesis; which statement, if any, was not identified.
  - 32 saved more heap (51.3 MiB) and its stress `ClaimRun` was about 68 ms against about
    7 ms.
  - Recycling on a compressed lifetime with jitter: C showed no steady-heap saving over A;
    D was slightly below B128 with JIT off (56.6 and 53.9 against 57.1 MiB, within its
    2.7 MiB rep-to-rep spread) and above it with JIT on. For `ClaimRun` in this harness, the
    first execution after a reconnect spent about 69-75 ms planning, and the six prepared
    executions about 348-358 ms in total (see above).
- **The statement count is a fixture parameter.** The harness retains 317 statements; the
  effect of any cap in production depends on how many distinct statements a connection runs
  and whether any re-planned statement crosses `jit_above_cost`, neither of which is
  measured in production.

## Consequences

- **No runtime change.** Operators who want to trade memory against re-prepares can set
  `statement_cache_capacity` in `DATABASE_URL`. Observation of what lowering the cache would
  trade: once a connection runs more distinct statements than the cap between two uses of a
  hot statement, the LRU can evict it and each such use re-prepares. In the harness at a cap of 32 under stress that showed as `ClaimRun`
  per-execution mean rising from about 7 ms to about 68 ms. To watch for it, compare `ClaimRun` latency, or,
  with the infrastructure owner's agreement, compare `pg_stat_statements` plans to calls with
  `track_planning` on.
- **JIT is a lever that may matter when it engages.** Whether it is the larger lever is
  open (see [Open investigation questions](#open-investigation-questions)). The operator
  levers, by reach:
  - `ALTER ROLE <app-role> SET jit = off;` takes effect for **new sessions only**, so
    existing pool connections keep JIT until they are replaced. The change takes effect as
    sessions are replaced, or after an API restart. pgxpool's background health check closes
    idle connections that are past their lifetime, but a checked-out connection is not
    interrupted and is destroyed only when it is released, so there is no fixed deadline.
  - `jit=off` as a key in the `DATABASE_URL` query: pgx passes unknown keys through as
    startup parameters. The chart ships no PgBouncer. A transaction-mode PgBouncer rejects a
    startup parameter it does not track, failing the connection, unless it is listed in
    `ignore_startup_parameters`, which drops it silently (see
    [ADR-1620](1620-custom-plan-cache-mode.md)).
  - `ALTER DATABASE ... SET jit = off`, or a server-wide setting, is the infrastructure
    owner's call.

  No `jit_above_cost` value is recommended. Identify the statements that JIT first
  (`pg_stat_statements` `jit_functions > 0`, or `EXPLAIN`).
- **Sizing is an estimate, not a hard bound:**
  **api replicas × per-api DB_MAX_CONNS × per-backend memory**, where api replicas means
  the number of api pools connected to the same primary (CNPG standby instances are not part
  of the multiplier). The per-backend figures measured here, about 60 MiB with JIT off
  (57.1 steady Anonymous, 60.0-60.6 Private_Dirty) and about 75 MiB with JIT on (67.4 steady,
  72.7 sampled peak Anonymous, 71.7 Private_Dirty), were measured at the 128 candidate on the
  synthetic fixture. They illustrate the arithmetic only (one replica at 20 connections is
  about 1.2 GiB with JIT off and about 1.5 GiB with JIT on) and are not recommendations.
  Add what is counted once (`shared_buffers`, the shared LLVM library page cache, PostgreSQL
  background processes), headroom for non-api connections (one-off `/server setup` runs,
  admin `psql` sessions, migrations (`store.Migrate` opens its own connection), CNPG's own
  connections), concurrent query memory and other PostgreSQL and container overhead. Query
  memory is up to `work_mem` per sort node and `work_mem × hash_mem_multiplier` per hash
  node (`hash_mem_multiplier` defaults to 2.0 since PostgreSQL 15; see the
  [PostgreSQL 17 resource settings](https://www.postgresql.org/docs/17/runtime-config-resource.html)).
  These multiply by the number of such nodes in a plan and by the number of concurrent
  queries, so they need headroom. Each parallel worker of a parallel query gets its own
  allowance. It assumes every pool connection has been opened
  and used; pgxpool opens connections lazily and reaps idle ones after 30m. The chart's api
  Deployment uses `Recreate` (`deploy/chart/templates/api-deployment.yaml`), so there is no
  rolling-update overlap term. Measured on PostgreSQL 17.12 with a small synthetic fixture and
  100 ms sampling; CNPG deployments may run a different major version.
- **Operator documentation** is
  [Database connection memory and sizing](../docs/configuration.md#database-connection-memory-and-sizing-issue-4591).

## Open investigation questions

- Do any statements trigger JIT on the backends that evict or recycle (the `jit_above_cost`
  hypothesis), and is JIT the larger memory lever than the statement cache?
- What produced the unreproduced 110-160Mi per-backend incident figure?

Any JIT investigation is separate work, not done here.

## Limits

- **Small synthetic fixture.** 2000 messages, 300 runs per measured user, 313 breadth methods
  and 317 retained statements against about 1691 named queries in the repository. Plan sizes
  and result sizes are not production-shaped. The breadth set is not the fully seeded,
  production-shaped working set the plan asked for: about 100 of its 313 entries return no
  rows and 47 return `ErrNoRows` against the fixture, and its expected-result check only
  pins rows / no rows / `ErrNoRows` per entry, not exact row counts or write effects. Every
  entry is still parsed, planned and cached, which is what the cache-capacity comparison
  measures, but plans and executor memory for those entries reflect near-empty results.
- **The incident scale was not reproduced.** A's `VmHWM` reached 121.2-125.6 MiB in run 4,
  and the wide-read sample's `VmHWM` was 124.7-170.0 MiB across its settings (not candidate A), but `VmHWM` includes shared pages, so it is
  not the per-backend heap, and nothing here attributes the 110-160Mi reported.
- **One measured PostgreSQL version and host**: 17.12 with `shared_buffers` 128MB.
- **JIT figures are one rep** (run 4 b) and compare across two runs on the same container;
  the jit-off figures are two reps.
- **Sampling.** Memory is sampled from `/proc` at a 100 ms target (mean 100.0 ms, p95 100.8 ms
  in run 3; run 1 recorded maximum intervals of 102.9-105.0 ms; run 2 recorded 102.8-200.8 ms, because candidate D had one roughly 200 ms gap or dropped sample). A peak shorter than the
  interval can fall between samples, so the sampled peak is a lower bound on the true peak;
  `VmHWM` is the kernel's high-water mark but includes shared pages. No sampling errors
  occurred in run 4. The summary and per-PID "sampled peak" figures come from interval
  sampler readings only, starting when the sampler starts; they exclude the empty-cache
  baseline sample, the eviction demo and the post-round samples. Per-PID `VmHWM` mixes
  sampler and held samples. The historical figures in this ADR are unchanged and are not
  recomputed maxima.
- **cgroup `memory.current` is unavailable for the primary runs.** Runs 3 and 4 recorded no
  cgroup column in their summaries and their raw cgroup files hold only the empty-cache
  baseline capture, so the container-wide figure cannot be tied to a candidate. The only
  per-candidate cgroup values (run 1, superseded) are container-wide, including the page
  cache and every backend, so they are not per-backend numbers either. The limit is that
  total container memory under each candidate is unverified; the ranking rests on per-backend
  Anonymous.
- **Harness checks are narrow.** Breadth checks pin only the result class (some rows, no
  rows, `ErrNoRows`); hot-mix and fixture checks assert only specific fields (ClaimRun id and
  status, InsertRunMessage inserted and `generation_live`, row counts, `SelfUsage`
  `run_count`, `Ping`, schema at head, heartbeat id, `verifyFixture`'s selected aggregates).
  Complete row values and write effects are not verified.
- **Clones are shared in some phases.** Memory uses one clone per candidate and rep, but
  latency and stress share one clone per round across candidates (later candidates inherit
  rolled-back-write and heartbeat-`UPDATE` bloat), cold shares one clone across all
  candidates and reps, and the template (`pm_template` in the recorded runs; `pm_<32-hex uuid>_tpl` per invocation now) is still read by the validation pool
  and discover mode. All clones share one server. The wide phase records other errors rather than failing.
- **JIT mismatch between runs.** The headline latency run ran with `jit = off`; the
  normal-JIT memory run showed extra Anonymous memory. The 3.8% normal-JIT difference is on
  this synthetic workload; production effect unmeasured.
- **The `SAVEPOINT` caveat.** [ADR-1620](1620-custom-plan-cache-mode.md)'s harness did not
  `RELEASE` its savepoints, so its absolute memory and latency carry the nesting growth run 1
  here showed.
- **`description_cache_capacity` was reasoned from the pgx source, not measured.**
