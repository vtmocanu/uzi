-- Issue #4684: scoped run usage (Spend) API, options and authority statements.
--
-- ADR-1184 D1: each statement is separate and an owner-shaped statement carries an
-- unconditional user predicate; never merge an owner and an all-scope statement behind a flag.
--
-- NO statement in this section may reference run_usage, run_usage_totals or
-- run_usage_model_totals: option lists, point checks and history start are authority
-- reads and must stay cheap and independent of the usage fold. A Go text assertion
-- (TestAnalyticsSQLOptionsBlocksAvoidUsageTables) enforces this for every Options, Accessible, Exists and
-- HistoryStart block.

-- name: AnalyticsTimezoneNames :many
SELECT name::text AS name FROM pg_catalog.pg_timezone_names ORDER BY name;

-- name: AnalyticsOwnerRepoOptions :many
-- Repos on the caller's connections. LIMIT 201 lets the caller serve 200 and flag truncation.
SELECT r.id, r.path_with_namespace
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE c.user_id = @user_id::uuid
ORDER BY r.path_with_namespace, r.id
LIMIT 201;

-- name: AnalyticsAllRepoOptions :many
SELECT r.id, r.path_with_namespace
FROM repos r
ORDER BY r.path_with_namespace, r.id
LIMIT 201;

-- name: AnalyticsUserOptions :many
SELECT u.id, u.email
FROM users u
ORDER BY u.email, u.id
LIMIT 201;

-- name: AnalyticsOwnerRepoAccessible :one
SELECT EXISTS (
    SELECT 1
    FROM repos r
    JOIN forge_connections c ON c.id = r.connection_id
    WHERE r.id = @repo_id::uuid AND c.user_id = @user_id::uuid
)::boolean AS accessible;

-- name: AnalyticsRepoExists :one
SELECT EXISTS (SELECT 1 FROM repos WHERE id = @repo_id::uuid)::boolean AS present;

-- name: AnalyticsUserExists :one
SELECT EXISTS (SELECT 1 FROM users WHERE id = @user_id::uuid)::boolean AS present;

-- name: AnalyticsOwnerHistoryStart :one
-- Earliest created_at of the caller's runs in scope; NULL when the scope is empty.
SELECT MIN(r.created_at)::timestamptz AS history_start
FROM runs r
WHERE r.user_id = @user_id::uuid
  AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
  AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
  AND r.kind = ANY(@kinds::text[]);

-- name: AnalyticsAllHistoryStart :one
SELECT MIN(r.created_at)::timestamptz AS history_start
FROM runs r
WHERE (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
  AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
  AND r.kind = ANY(@kinds::text[]);
