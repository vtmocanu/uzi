-- name: UpsertAgentSourceStaged :one
-- Upsert the singleton staged snapshot (PRD #602 M3). singleton is hardcoded true
-- so ON CONFLICT (singleton) always targets the one row; every mutable column is
-- overwritten. roles/diff are passed as jsonb (the caller marshals the Go slices),
-- never NULL — the NOT NULL columns reject a stray SQL NULL.
INSERT INTO agent_source_staged (singleton, fetched_at, fetched_sha, source_url, source_ref, roles, diff)
VALUES (true, @fetched_at, @fetched_sha, @source_url, @source_ref, @roles, @diff)
ON CONFLICT (singleton) DO UPDATE SET
  fetched_at  = EXCLUDED.fetched_at,
  fetched_sha = EXCLUDED.fetched_sha,
  source_url  = EXCLUDED.source_url,
  source_ref  = EXCLUDED.source_ref,
  roles       = EXCLUDED.roles,
  diff        = EXCLUDED.diff
RETURNING *;

-- name: GetAgentSourceStaged :one
-- Read the singleton staged snapshot (PRD #602 M3; M4 reads it to apply). Returns
-- pgx.ErrNoRows when nothing has been staged yet.
SELECT * FROM agent_source_staged WHERE singleton = true;

-- name: DeleteAgentSourceStaged :exec
-- Clear the staged snapshot (PRD #602 M4 / de-provision). A no-op when empty.
DELETE FROM agent_source_staged WHERE singleton = true;
