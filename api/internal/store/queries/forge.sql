-- Forge connections -------------------------------------------------------

-- name: UpsertForgeConnection :one
-- Connect or reconnect/rotate: one connection per (user, forge_type, base_url).
-- Reconnecting with a fresh PAT updates the ciphertext and bot identity in place
-- and stamps last_verified_at.
INSERT INTO forge_connections (
    user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext, last_verified_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (user_id, forge_type, base_url) DO UPDATE
SET bot_username      = EXCLUDED.bot_username,
    bot_forge_user_id = EXCLUDED.bot_forge_user_id,
    token_ciphertext  = EXCLUDED.token_ciphertext,
    last_verified_at  = now()
RETURNING *;

-- name: ListForgeConnectionsByUser :many
SELECT * FROM forge_connections WHERE user_id = $1 ORDER BY created_at ASC;

-- name: GetForgeConnectionForUser :one
SELECT * FROM forge_connections WHERE id = $1 AND user_id = $2;

-- name: TouchForgeConnectionVerified :one
UPDATE forge_connections SET last_verified_at = now() WHERE id = $1 AND user_id = $2
RETURNING *;

-- name: DeleteForgeConnectionForUser :execrows
DELETE FROM forge_connections WHERE id = $1 AND user_id = $2;

-- Repos --------------------------------------------------------------------

-- name: UpsertRepo :one
-- Upserted whenever the membership list is fetched. Never touches `enabled`, so
-- a re-listing does not un-enable a repo the user already turned on.
INSERT INTO repos (connection_id, forge_project_id, path_with_namespace, web_url, default_branch)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (connection_id, forge_project_id) DO UPDATE
SET path_with_namespace = EXCLUDED.path_with_namespace,
    web_url             = EXCLUDED.web_url,
    default_branch      = EXCLUDED.default_branch
RETURNING *;

-- name: ListReposByConnectionForUser :many
-- Membership view for one connection, authorized through its owning user.
SELECT r.* FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.connection_id = $1 AND c.user_id = $2
ORDER BY r.path_with_namespace ASC;

-- name: ListEnabledReposForUser :many
-- Enabled repos for the current user (sidebar picker / repos list).
SELECT r.* FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE c.user_id = $1 AND r.enabled = true
ORDER BY r.path_with_namespace ASC;

-- name: GetRepoForUser :one
-- One repo plus the connection fields needed to build a forge client, scoped to
-- the owning user.
SELECT r.id, r.connection_id, r.forge_project_id, r.path_with_namespace, r.web_url,
       r.default_branch, r.enabled,
       c.forge_type, c.base_url, c.token_ciphertext, c.user_id
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = $1 AND c.user_id = $2;

-- name: SetRepoEnabledForUser :one
UPDATE repos SET enabled = $2
WHERE repos.id = $1
  AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = $3)
RETURNING *;

-- name: ListEnabledReposWithConnections :many
-- Every enabled repo across all users, with its connection, for the sync
-- engine's poller set.
SELECT r.id, r.connection_id, r.forge_project_id, r.path_with_namespace, r.web_url,
       r.default_branch, r.enabled,
       c.forge_type, c.base_url, c.token_ciphertext, c.user_id
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.enabled = true;

-- Board columns ------------------------------------------------------------

-- name: ListBoardColumns :many
SELECT * FROM board_columns WHERE repo_id = $1 ORDER BY position ASC;

-- name: CountBoardColumns :one
SELECT count(*) FROM board_columns WHERE repo_id = $1;

-- name: DeleteBoardColumnsByRepo :exec
DELETE FROM board_columns WHERE repo_id = $1;

-- name: InsertBoardColumn :exec
-- DO NOTHING makes seeding idempotent: two concurrent first-opens (e.g. React
-- StrictMode double-firing GetBoard) can both run the seed without the second
-- hitting the (repo_id, label_name) unique violation.
INSERT INTO board_columns (repo_id, label_name, position)
VALUES ($1, $2, $3)
ON CONFLICT (repo_id, label_name) DO NOTHING;

-- Issues cache -------------------------------------------------------------

-- name: UpsertIssue :one
INSERT INTO issues (
    repo_id, forge_issue_iid, title, state, labels, web_url, author,
    has_prd_link, forge_updated_at, synced_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (repo_id, forge_issue_iid) DO UPDATE
SET title            = EXCLUDED.title,
    state            = EXCLUDED.state,
    labels           = EXCLUDED.labels,
    web_url          = EXCLUDED.web_url,
    author           = EXCLUDED.author,
    has_prd_link     = EXCLUDED.has_prd_link,
    forge_updated_at = EXCLUDED.forge_updated_at,
    synced_at        = now()
RETURNING *;

-- name: ListIssuesByRepo :many
SELECT * FROM issues WHERE repo_id = $1 ORDER BY forge_issue_iid ASC;

-- name: GetIssueByIID :one
SELECT * FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2;

-- name: DeleteIssuesNotIn :execrows
-- Reconcile eviction: drop cached issues absent from the fresh forge set. An
-- empty keep-set deletes everything for the repo (all PRD issues gone forge-side).
DELETE FROM issues
WHERE repo_id = $1 AND forge_issue_iid <> ALL(@keep_iids::bigint[]);
