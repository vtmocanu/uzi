-- Run-writers lookup (issue #4618): every active ci_fix / mr_rework run that writes one
-- branch of one forge project, across ALL owners, for the admin-debug read
-- GET /api/admin/run-writers. Metadata only; no run text columns.

-- name: ResolveRunWritersTarget :one
-- The forge identity of the repo the caller named: forge type, instance base URL and the
-- forge's numeric project id. No row means the repo id is unknown.
SELECT fc.forge_type, fc.base_url, r.forge_project_id
FROM repos r
JOIN forge_connections fc ON fc.id = r.connection_id
WHERE r.id = @repo_id;

-- name: ListRunWritersForTarget :many
-- Runs are matched by forge identity, never by owner: two owners' connections to the same
-- project are the same project. The instance base URL is compared canonically on both
-- sides (a trailing :443 is dropped; for github only, https://api.github.com is
-- https://github.com), mirroring settings.CanonicalInstance. The status list is spelled
-- exactly as the uq_runs_one_active_* partial indexes spell it. A ci_fix matches by its
-- pipeline_ref (the branch); an mr_rework matches only by the supplied MR iid.
SELECT r.id, r.kind, r.status, r.user_id, r.repo_id, r.pipeline_ref, r.mr_iid, r.created_at
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections fc ON fc.id = rp.connection_id
WHERE fc.forge_type = @forge_type::text
  AND (CASE WHEN @forge_type::text = 'github' AND regexp_replace(fc.base_url, ':443$', '') = 'https://api.github.com'
            THEN 'https://github.com'
            ELSE regexp_replace(fc.base_url, ':443$', '') END)
    = (CASE WHEN @forge_type::text = 'github' AND regexp_replace(@base_url::text, ':443$', '') = 'https://api.github.com'
            THEN 'https://github.com'
            ELSE regexp_replace(@base_url::text, ':443$', '') END)
  AND rp.forge_project_id = @forge_project_id::bigint
  AND r.status NOT IN ('completed', 'failed', 'cancelled')
  AND ((r.kind = 'ci_fix' AND r.pipeline_ref = @branch::text)
    OR (r.kind = 'mr_rework' AND r.mr_iid = sqlc.narg(mr_iid)::bigint))
ORDER BY r.created_at, r.id
LIMIT @max_rows::int;
