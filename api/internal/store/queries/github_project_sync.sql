-- GitHub Projects v2 Status sync persistence (PRD #364). The poller (M5/M6/M7) and
-- the link/teardown handlers (M3) read and write these two side tables. Neither is
-- authoritative: the project board is the source of truth and these are reconciled
-- against it.

-- name: UpsertGithubProjectLink :one
-- Create or refresh the link row for a repo. One row per repo (repo_id UNIQUE), so
-- the conflict target is repo_id; every mutable column is overwritten and updated_at
-- bumped. Does NOT touch last_synced_at/last_error — those are written by the
-- error/clear queries below on each sync attempt, not by a (re)link.
INSERT INTO github_project_links (
    repo_id, project_node_id, project_number, status_field_id, status_options, owned_by_uzi
) VALUES (
    sqlc.arg('repo_id'), sqlc.arg('project_node_id'), sqlc.arg('project_number'),
    sqlc.arg('status_field_id'), sqlc.arg('status_options'), sqlc.arg('owned_by_uzi')
)
ON CONFLICT (repo_id) DO UPDATE SET
    project_node_id = EXCLUDED.project_node_id,
    project_number  = EXCLUDED.project_number,
    status_field_id = EXCLUDED.status_field_id,
    status_options  = EXCLUDED.status_options,
    owned_by_uzi    = EXCLUDED.owned_by_uzi,
    updated_at      = now()
RETURNING *;

-- name: GetGithubProjectLinkByRepo :one
-- The link row for a repo, or no row when the repo is not synced.
SELECT * FROM github_project_links WHERE repo_id = sqlc.arg('repo_id');

-- name: ListGithubProjectSyncedRepos :many
-- Every link row, for the poller to iterate all synced repos.
SELECT * FROM github_project_links ORDER BY created_at ASC;

-- name: DeleteGithubProjectLink :exec
-- Teardown: drop a repo's link. The item rows cascade with the repo, not with the
-- link, so item cleanup on unlink (if wanted) is a separate DeleteGithubProjectItem.
DELETE FROM github_project_links WHERE repo_id = sqlc.arg('repo_id');

-- name: SetGithubProjectLinkError :exec
-- Record a sync failure for observability: stamp last_error and last_synced_at.
UPDATE github_project_links
SET last_error = sqlc.arg('last_error'), last_synced_at = now()
WHERE repo_id = sqlc.arg('repo_id');

-- name: ClearGithubProjectLinkError :exec
-- Record a successful sync: clear last_error and stamp last_synced_at.
UPDATE github_project_links
SET last_error = NULL, last_synced_at = now()
WHERE repo_id = sqlc.arg('repo_id');

-- name: UpsertGithubProjectItem :one
-- Create or refresh the projection state for one issue. Keyed on the composite PK
-- (repo_id, forge_issue_iid); item_node_id, last_status_option_id and last_synced_at
-- are overwritten. last_status_option_id is nullable (an item may exist with no
-- Status written yet).
INSERT INTO github_project_items (
    repo_id, forge_issue_iid, item_node_id, last_status_option_id, last_synced_at
) VALUES (
    sqlc.arg('repo_id'), sqlc.arg('forge_issue_iid'), sqlc.arg('item_node_id'),
    sqlc.narg('last_status_option_id'), now()
)
ON CONFLICT (repo_id, forge_issue_iid) DO UPDATE SET
    item_node_id          = EXCLUDED.item_node_id,
    last_status_option_id = EXCLUDED.last_status_option_id,
    last_synced_at        = now()
RETURNING *;

-- name: GetGithubProjectItem :one
-- The projection state for one issue, or no row when the item is not tracked.
SELECT * FROM github_project_items
WHERE repo_id = sqlc.arg('repo_id') AND forge_issue_iid = sqlc.arg('forge_issue_iid');

-- name: ListGithubProjectItems :many
-- All tracked items for a repo, for the poller's diff pass.
SELECT * FROM github_project_items
WHERE repo_id = sqlc.arg('repo_id')
ORDER BY forge_issue_iid ASC;

-- name: SetGithubProjectItemStatusMarker :exec
-- After a successful Status write, advance just the diff marker (and stamp the
-- sync time) without touching item_node_id.
UPDATE github_project_items
SET last_status_option_id = sqlc.narg('last_status_option_id'), last_synced_at = now()
WHERE repo_id = sqlc.arg('repo_id') AND forge_issue_iid = sqlc.arg('forge_issue_iid');

-- name: DeleteGithubProjectItem :exec
-- Issue-closed removal: drop the item's projection state.
DELETE FROM github_project_items
WHERE repo_id = sqlc.arg('repo_id') AND forge_issue_iid = sqlc.arg('forge_issue_iid');
