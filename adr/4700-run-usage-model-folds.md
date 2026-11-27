# ADR-4700: materialized per-run, per-model usage folds; admin all-users all-time stays unavailable

**Status**: Accepted, partially (issue #4700). The materialized fold, its freshness protocol, the writers and the `AnalyticsAllSpend` read path are delivered by this change. Re-enabling admin all-users `window=all` was **not adopted**: its must-pass measurement failed (see [Decision](#decision-owner-2026-11-27)).
**Date**: 2026-11-27
**Deciders**: lead (plan), coder (implementation and measurement), reviewer, dba (lock analysis and an independent measurement on a different host); owner (decision to keep `window=all` refused).
**Related**: [ADR-1620](1620-custom-plan-cache-mode.md) (the generic-plan hazard the M4 measurement ran under, and the `run_usage_totals` query-shape rule); [ADR-1184](1184-admin-judge-scope.md) (D1: the owner and all-scope analytics statements stay separate, which is why only the all-scope statement reads the folds). The scoped analytics API and the 400 refusal come from #4684 (`prds/done/4684-scoped-usage-spend-api.md`); the user-facing limit is in [Usage analytics API](../docs/usage-analytics.md#performance-limit).
**PRD**: none for #4700; the spec lived in the issue body. The measurement context is the #4684 PRD.

## Decision summary

A materialized per-(run, model) fold of `run_usage_model_totals` now exists
(`run_usage_model_folds`, migration `00549`), with a per-run freshness row
(`run_usage_fold_state`). `AnalyticsAllSpend` reads the stored fold for a **fresh terminal**
run and folds every other run live. Writers invalidate the fold through a lock function,
and a sweeper pass plus a terminal-status kick keep it fresh.

It did not meet the goal that justified it. The issue asked for the admin all-users
`window=all` Spend request (refused with 400 by #4684) to fit the 8 s analytics budget.
With **every run stored**, the default request still misses 8 s in production plan mode,
because the cost is in building the post-fold Spend document over about 117k runs, not in
reading the fold. The owner decided to land the fold and keep the 400 refusal unchanged.
Re-enabling all-time is a separate issue, if wanted, aimed at the post-fold document.

The schema lands as migrations `00549` and `00550` (drafted as 00543 and 00544 and renumbered
above the live head at landing).

## Context

Admin all-users `window=all` measured about 10 s on the #4684 evidence seed (120,000 runs),
so #4684 shipped it as a 400 (`window all not available for all users`) and deferred it to
"a follow-up that adds per-run totals for finished runs". #4700 is that follow-up. The
all-scope Spend statement folded `run_usage` live per request through
`run_usage_model_totals` (migration `00542`); the plan was to store that fold for finished
runs and read it instead.

A stored fold is only correct if no reader can use it after a `run_usage` write it does not
include. The rest of this ADR is that freshness protocol and what it costs.

## Design

### Schema (migration 00549, 00550)

- `run_usage_fold_state(run_id PK, stale boolean default true, folded_at)`, with a partial
  index `WHERE stale`. There is **no FK to `runs`**: the state row must be creatable while
  holding only a `FOR KEY SHARE` lock on the run, without a competing FK check. A
  `runs AFTER DELETE` trigger deletes the state row instead.
- `run_usage_model_folds(run_id, model, token columns, cost_usd, cost_status)`, FK to the
  state row with `ON DELETE CASCADE`.
- `fn_run_usage_fold_lock(r)`: takes `runs` row `FOR KEY SHARE`, raises SQLSTATE `23503`
  when the run is missing, then upserts the state row to `stale = true`. The upsert is
  `ON CONFLICT DO UPDATE ... WHERE NOT stale`, so the row is locked even when it is already
  stale.
- There are **no triggers on `run_usage`**: a per-row trigger would tax every live upsert.
- `00550` adds a partial index on `runs (created_at DESC, id DESC)` for terminal, non-chat,
  `usage_refolded` runs, built with `CREATE INDEX CONCURRENTLY` (`NO TRANSACTION`), so the
  materializer's missing lane is an index range scan of at most 2000 entries.

### Lock inventory

The `runs` row is the master lock. Every path that takes more than one lock takes the `runs`
row first, except materializer step (b), which locks state rows without the `runs` row;
that is safe because it uses `SKIP LOCKED` and never waits.

| Path | Locks, in order |
|---|---|
| Live usage writers (`UpsertRunUsage`, `DeleteRunUsage` in `queries/runtime.sql`) | The claim transaction already holds the `runs` row `FOR UPDATE` (`beginClaimMutation` takes it through `GetRunOwnedByWorkerForUpdate`); the function then takes `FOR KEY SHARE` on it, then the state row. A bare store-level call in its own transaction takes `FOR KEY SHARE` only. |
| `RefoldRunUsage` (`api/internal/workersvc/usage_refold.go`) | `lock_timeout 5s`, then `LockRunForUsageRefold`: the `runs` row `FOR UPDATE`, **before** any `run_usage` write. |
| Materializer step (a0) | `runs` rows `FOR KEY SHARE SKIP LOCKED` for runs that have no state row yet (first materialization). |
| Materializer step (b) | State rows `FOR UPDATE SKIP LOCKED` for runs that already have one. |
| Run delete | Holds the `runs` row; the RI cascade, then the state-cleanup trigger. |

Below the `runs` row the order of state, `run_usage` and fold rows is irrelevant: no path
holds two of them in opposite orders while waiting.

**Correction recorded during the work.** The plan was for the refold to take the `runs` row
`FOR NO KEY UPDATE`. That does not conflict with a writer's `FOR KEY SHARE`, so the refold's
delete could block on the writer's state row, resume with a statement snapshot that predates
the writer's commit, and leave behind a row with a new key that the writer inserted. The dba
reproduced it. The refold therefore takes `FOR UPDATE`, which conflicts with `FOR KEY SHARE`
and waits out every in-flight writer before `DeleteRunUsage`'s snapshot is taken. The
comment on `LockRunForUsageRefold` in `queries/run_usage_folds.sql` records this.

### Write path

Every statement that writes `run_usage` calls the lock function through a CTE the statement
references (`UpsertRunUsage` and `DeleteRunUsage`). A missing run raises `23503`, which is
the error the `run_usage.run_id -> runs.id` FK would have raised. Enforcement tests in
`api/internal/store/run_usage_folds_test.go` pin the writers:
`TestRunUsageWritersTakeFoldLock_SQL` (SQL side), `TestRunUsageWritersTakeFoldLock_Go` (Go
callers), `TestRunUsageWriterClassifier`, and `TestRefoldRunUsageLocksRunBeforeDelete`.

### Materializer

`store.MaterializeRunUsageFolds` (`api/internal/store/run_usage_folds.go`, statements in
`queries/run_usage_folds.sql`) runs in one explicit `READ COMMITTED` transaction with
`lock_timeout 2s` and `statement_timeout 5s`, for at most 100 run ids (`MaxRunUsageFoldBatch`),
in steps:

- a0: select eligible runs with no state row, `FOR KEY SHARE SKIP LOCKED`.
- a: insert the missing state rows (stale).
- b: lock the eligible runs' state rows `FOR UPDATE SKIP LOCKED`.
- c: delete the locked runs' old fold rows.
- d: insert the fold from `run_usage_model_totals`.
- e: mark the state rows fresh.

(b) and (d) are deliberately separate statements. Under `READ COMMITTED` (d) takes its
snapshot after (b) locked the state rows, so every writer commit before the lock is visible
and every later writer blocks in `fn_run_usage_fold_lock` until the transaction ends.
Stale can therefore never be cleared over a newer write.

### Freshness semantics

| State | Reader behaviour |
|---|---|
| No state row (missing) | Read live from `run_usage_model_totals`. |
| `stale = true` | Read live. |
| `stale = false`, run terminal | Read the stored fold. |
| `stale = false`, zero fold rows | A legitimately empty fold; the run reads as a run with no usage. |

Only the `AnalyticsAllSpend` statement (`queries/analytics.sql`) reads the folds, in two
disjoint `model_fold` branches (stored for fresh terminal runs, live for the rest). The
owner statement is unchanged (ADR-1184 D1).

### Atomicity invariant

`stale = false` is visible exactly with the complete fold from the same committed
replacement. `TestRunUsageFoldAtomicityLiveDB` covers: context cancelled after (c), backend
terminated before commit, a concurrent snapshot reader during an uncommitted replacement,
and visibility of an empty fold. **Not claimed:** behaviour across a server crash, storage
failure or power loss.

### Terminal coverage

`store.UsageFolder` (`api/internal/store/usage_folder.go`) feeds the materializer:

- **Kicked.** `Notify` is wired as a run-lifecycle hook and starts a one-run materialization
  for a terminal status. It covers the `workersvc` `s.notify` terminal paths. It is
  best-effort: two kick slots, and a kick is dropped when both are busy.
- **Swept.** Pass `usage_fold_materialize`, registered **last** (`api/cmd/server/main.go`,
  `usage_folder_wiring.go`), covers every other terminal writer and any dropped kick. It has
  a 5 s parent deadline and 2.5 s lane deadlines, alternates two lanes, and bounds a lane to
  100 attempts:
  - the stale lane, ascending `run_id` by cursor;
  - the missing lane, newest first by keyset over `(created_at DESC, id DESC)` in windows of
    2000 eligible runs (the `2000` is a SQL literal, pinned by
    `TestUsageFolderMissingQueryPlanLiveDB`), with a 5-minute backoff after a full traversal
    finds nothing.
- A batch that hit a lane deadline is retried run by run on the next pass; a run that fails
  alone is skipped for 10 minutes. One semaphore serialises every materializer call, because
  two concurrent calls on the same new run fail a whole batch with `55P03`. `UsageFolder.Wait` starts after
  the HTTP drain and runs alongside the generated-output and prerequisite drain
  (`drainShutdownWork` in `api/cmd/server/main.go`), bounded to 5 s.

A dropped kick is caught by the sweeper through one of two lanes, each with its own bound.
Until then the run is read live, which is correct, only slower.

- **Stale lane.** Every usage write creates or re-stales the run's state row through
  `fn_run_usage_fold_lock`, so a finished, non-chat, refolded run with any usage write after
  the upgrade is a stale-lane candidate (unless it is in the folder's skip set). The lane takes up to 100 runs per pass and walks the stale set by ascending
  `run_id` with a wrapping cursor, so the worst case is one wrap of the stale set. That wrap
  can be long: after the rollback reset below marks about 115k rows stale it takes about
  1,158 passes, roughly 4.8 hours at one pass per 15 s.
- **Missing lane.** It covers only eligible runs that have no state row. One tick inspects a
  window of 2000 eligible runs, so a full traversal is one tick per 2000 eligible runs only
  when each window yields fewer than 100 candidates; more candidates take more passes. After
  a full traversal that finds nothing it backs off for 5 minutes.

## Operations

### Manual DML rule

Hand-written DML on `run_usage` must, in the same transaction and **before** it changes
`run_usage`, lock the runs and call the function for each affected run, in run-id order:

```sql
SELECT id FROM runs WHERE id = ANY($1) ORDER BY id FOR UPDATE;
SELECT fn_run_usage_fold_lock(s.r)
FROM (SELECT DISTINCT unnest($1::uuid[]) AS r ORDER BY 1) s;
```

A refold after the commit is not enough: a reader can use the stale-but-fresh-marked fold
in between.

Orphan state rows (see TRUNCATE below) are cleaned with:

```sql
DELETE FROM run_usage_fold_state s WHERE NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = s.run_id);
```

### Exclusions

- **`TRUNCATE`** of `runs` leaves inert orphan state rows: every reader and lane joins
  through `runs`, so an orphan is never read or folded. Clean them with the statement above.
- **Disabled triggers and the replica role** skip the `runs` delete trigger. The same orphan
  handling applies.
- **Rollback boundary.** For a rollback to a pre-#4700 build and a later re-upgrade, stop and
  drain every pre-#4700 API writer first, then run
  `UPDATE run_usage_fold_state SET stale = true`, then start the upgraded API. An old writer
  does not call the lock function, so one that commits after the reset could leave a stale
  fold marked fresh. The API Deployment's strategy is hard-coded to `Recreate`
  (`deploy/chart/templates/api-deployment.yaml`), so a forward deploy stops every old writer
  before a new one starts and never overlaps them; only a rollback needs this step.

### Rejected alternative

A lock-free state path that skips the state-row lock when the row is already stale. It was
rejected because it would require the refold to take `FOR UPDATE` on every state row it
touches and step (a0) to lock every run, not only the new ones.

## Measurements

Evidence is from a build-tagged handler harness (`analyticsevidence` build tag, files
`api/internal/handler/analytics_folds_evidence_livedb_test.go` and
`api/internal/handler/analytics_reads_evidence_livedb_test.go` and
`api/internal/store/analytics_evidence_livedb_test.go`). The seed is the #4684 evidence seed:
120,000 runs (115,757 eligible), 4-CPU PostgreSQL 17, default settings, through the real
handler with the re-enable applied in a **scratch copy only**, and the production 8 s
snapshot timeout. The handler in this change still refuses `window=all`.

**Read path, all runs stored.**

| Mode | Result for the default `?window=all` request |
|---|---|
| Production plan mode (pgx cached statements; PostgreSQL may switch to a generic plan from the 6th execution when it estimates it cheaper, which these samples observed) | median 7.7 s, p95 8.07 s, 2 of 12 over 8 s (another run: 2 of 12 were 503); generic plans up to 4 of 12 503 |
| Custom plans | median about 6.9 to 7.0 s, p95 about 7.1 to 7.2 s |
| `split=harness&breakdown=kind` | passes in every mode (p95 at most 6.65 s) |

The fold read is about 0.44 s. The remainder is the post-fold Spend document (per-run
sorting, windowing and aggregation over about 117k runs). `work_mem` 64MB helps by about 6% for custom plans and about 0 to 3% for generic plans.
That is why a full fold does not rescue the request.

**Fallback (some runs unstored).** `split=harness&breakdown=kind` passes with up to 25% of
runs unstored, is marginal at 40%, and returns 503 at 50% or more under auto and generic plans (custom plans passed at 50% for that request). With nothing stored every
sample is 503.

**Controls (39 cells).** Byte-identical bodies. Once the backfill is complete there is no
regression and admin all-users `7d`, `30d` and `90d` are 36 to 43% faster. With nothing stored
admin all-users was up to about 4 to 5% slower, within the 10% control threshold.

**Backfill.** 1,159 passes at one pass per 15 s tick is about 4.8 hours for 120k runs.

**Write path.**

- Within +0.5 ms p99 per usage write (typically +0.035 ms per upsert; worst round +0.16 ms
  in the claim shape, +0.39 ms for a bare upsert).
- Refold: +0.93 ms p99 per run.
- WAL: +11 to 66 B per write on active runs in the claim shape; a bare upsert on an existing
  key measured +108 to 109 B; a fresh-to-stale flip 276 to 416 B per transaction (one write)
  or about 330 B (five writes); +6.1 kB per refolded run.
- The one-time legacy refold's `UPDATE runs` loses HOT updates because `00550`'s predicate
  covers `usage_refolded`.
- Materializer: no p99 cost; p99.9 3.4 to 5.9 ms on runs being folded.

**DBA, independent host, 500k-run synthetic seed.** With every run folded the whole statement
costs about 54 to 62 µs per run, so the 8 s budget holds only up to roughly 130 to 145k
non-chat runs there.

## Decision (owner, 2026-11-27)

M4's must-pass criterion **failed**: even with every run folded, the default `?window=all`
request misses 8 s in production plan mode, because the cost is in the post-fold Spend
document, not in the fold read. A readiness gate (serve `all` only once the backfill is
complete) would not fix that steady-state miss, so it was not built.

1. **Do not re-enable.** The admin all-users `window=all` refusal (400) in
   `api/internal/handler/analytics.go` is unchanged. Options still omits `all` for that
   scope. Owner `all` and admin `user=<uuid>` `all` are unchanged.
2. **Land the materialized fold** with the M4 evidence above. It makes admin all-users `7d`,
   `30d` and `90d` about 36 to 43% faster on the same seed once the backfill is complete.
3. **Follow-up scope.** Re-enabling all-time is out of scope here. If it is wanted later it
   is a separate issue targeting the post-fold Spend document (its per-run sorting,
   windowing and aggregation), not the fold. The DBA's per-run cost also bounds how large
   an instance that fix could serve.

Not shipped: the handler re-enable (M5), a readiness gate, any change to the owner
statement, and any claim of behaviour across a server crash or storage failure.
