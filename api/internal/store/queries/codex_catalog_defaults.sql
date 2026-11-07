-- Codex catalog default reasoning effort, as last reported by a worker (issue #3251). The service
-- layer is workersvc/codex_catalog_default.go (RecordCodexCatalogDefault writes,
-- CodexCatalogDefault reads). Derived data: rows are re-reported by the next Codex run.

-- name: LockCodexCatalogDefaults :exec
-- Serializes every writer of ONE user's codex_catalog_defaults rows, so the prune after an upsert
-- sees every previously committed row and the 32-row bound holds under concurrent reports. It is
-- its own statement in RecordCodexCatalogDefault's transaction, after the shared secret mutation
-- lock and before the run row is read.
--
-- Two-int advisory lock: class 1970955108 = 0x757A6364 ("uzcd"), the value of
-- store.CodexCatalogLockClass in migrate.go; TestCodexCatalogLockClassMatchesSQL pins this literal
-- to it. XACT-scoped: released on commit or rollback, so it must run on a transaction-bound
-- Queries (on a bare pool it would release immediately).
SELECT pg_advisory_xact_lock(
    1970955108,
    hashtext(sqlc.arg(user_id)::uuid::text)
);

-- name: UpsertCodexCatalogDefault :exec
-- reported_at is statement_timestamp(), NOT now(): now() is the transaction START, so a writer
-- that waited on the per-user lock while others committed would stamp an older time than theirs
-- and the prune below would delete its own freshly written row.
INSERT INTO codex_catalog_defaults (user_id, context_id, model, runtime_version, default_effort, reported_at)
VALUES (sqlc.arg(user_id), sqlc.arg(context_id), sqlc.arg(model), sqlc.arg(runtime_version), sqlc.arg(default_effort), statement_timestamp())
ON CONFLICT (user_id, context_id, model, runtime_version) DO UPDATE SET
    default_effort = EXCLUDED.default_effort,
    reported_at    = EXCLUDED.reported_at;

-- name: PruneCodexCatalogDefaults :exec
-- Keeps at most the 32 newest rows of one user and none older than 30 days. Runs under
-- LockCodexCatalogDefaults, after the writer's own upsert.
DELETE FROM codex_catalog_defaults d
WHERE d.user_id = sqlc.arg(user_id)
  AND (d.reported_at < statement_timestamp() - interval '30 days'
       OR (d.context_id, d.model, d.runtime_version) NOT IN (
           SELECT k.context_id, k.model, k.runtime_version
           FROM codex_catalog_defaults k
           WHERE k.user_id = sqlc.arg(user_id)
           ORDER BY k.reported_at DESC, k.context_id, k.model, k.runtime_version
           LIMIT 32));

-- name: ListFreshCodexCatalogDefaults :many
-- The fresh rows (reported within 30 days) for one user, context and model. At most 32 rows exist
-- per user, so the result is bounded.
SELECT runtime_version, default_effort, reported_at
FROM codex_catalog_defaults
WHERE user_id = sqlc.arg(user_id)
  AND context_id = sqlc.arg(context_id)
  AND model = sqlc.arg(model)
  AND reported_at >= now() - interval '30 days';
