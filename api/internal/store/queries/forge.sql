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

-- name: SetForgeConnectionHumanUsername :one
-- Set or clear (NULL $3) the connecting user's own forge username on their
-- connection, scoped to the owner (PRD #19 M3). The partial unique index on
-- (base_url, human_username) rejects a value another user already mapped on the
-- same host with a 23505 the handler surfaces as a hard 409.
UPDATE forge_connections SET human_username = $3
WHERE id = $1 AND user_id = $2
RETURNING *;

-- name: DeleteForgeConnectionForUser :execrows
DELETE FROM forge_connections WHERE id = $1 AND user_id = $2;

-- name: ListAllForgeConnections :many
-- Every connection across all users, for the privilege-check sweep (single-API,
-- no leader election — same assumption as the poller/sweeper).
SELECT * FROM forge_connections ORDER BY created_at ASC;

-- name: UpdatePrivilegeReport :execrows
-- Stamp a connection's privilege report + denormalized status. :execrows so a
-- connection deleted mid-sweep is a tolerated 0-row write, not an error.
UPDATE forge_connections
SET privilege_report     = $2,
    privilege_checked_at = $3,
    privilege_status     = $4
WHERE id = $1;

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

-- name: ListEnabledReposByConnection :many
-- Enabled repos for one connection (privilege sweep + on-demand check). Caller
-- has already authorized access to the connection, so this is keyed by
-- connection id alone.
SELECT * FROM repos WHERE connection_id = $1 AND enabled = true
ORDER BY path_with_namespace ASC;

-- name: SetRepoEnabledForUser :one
UPDATE repos SET enabled = $2
WHERE repos.id = $1
  AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = $3)
RETURNING *;

-- name: SetRepoSkillsEnabledForUser :one
-- Repo-skills opt-in toggle, authorized through the repo's owning connection.
-- A non-owned or unknown id returns no rows (mapped to 404 in the handler).
UPDATE repos SET repo_skills_enabled = $2
WHERE repos.id = $1
  AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = $3)
RETURNING *;

-- name: SetRepoSkillsEnabled :one
-- Admin path for the repo-skills toggle: not scoped to the owning user. The
-- handler gates this on the caller being an admin.
UPDATE repos SET repo_skills_enabled = $2 WHERE repos.id = $1 RETURNING *;

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

-- name: ShiftBoardColumnsFrom :exec
-- Make room to insert a column at @from_position by bumping every column at or
-- after it up by one. The Human Review retrofit (GetBoard) uses this so the new
-- column lands right after In Progress with a distinct position instead of tying
-- the column it displaces (position is not unique and ORDER BY would then be
-- arbitrary).
UPDATE board_columns SET position = position + 1
WHERE repo_id = @repo_id AND position >= @from_position;

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

-- name: ListLatestRunsForRepo :many
-- The board payload's run half (PRD #12 M2): the newest run per issue for a repo,
-- one row per issue that has ever run. DISTINCT ON picks the newest by created_at
-- via the composite index runs (repo_id, issue_iid, created_at DESC). Every row
-- here genuinely has a run, so id/user_id/status are non-null (a LEFT JOIN LATERAL
-- onto issues would leave them NULL for issues with no run, which sqlc mistypes as
-- non-null and would then panic on scan — so the caller maps these onto issues in
-- Go instead, and issues with no run simply render latest_run: null). owner
-- name/email + worker name ride along for the "started by X" treatment; user_id
-- lets the caller flag viewer ownership. run_count is a window count over each
-- issue's runs (evaluated before DISTINCT ON, so it survives the newest-row pick)
-- and drives the board's "×N" retry hint without a per-issue history fan-in. Only
-- display fields — never session_id, plan_md, or any secret.
SELECT DISTINCT ON (r.issue_iid)
       r.issue_iid, r.id, r.user_id, r.status, r.mr_iid, r.failure_reason, r.stop_kind,
       r.created_at, r.updated_at,
       ru.display_name AS owner_name, ru.email AS owner_email, rw.name AS worker_name,
       COUNT(*) OVER (PARTITION BY r.issue_iid) AS run_count
FROM runs r
LEFT JOIN users ru ON ru.id = r.user_id
LEFT JOIN workers rw ON rw.id = r.worker_id
WHERE r.repo_id = @repo_id AND r.issue_iid IS NOT NULL   -- issue runs only; ci_fix runs (PRD #6) have no card
ORDER BY r.issue_iid, r.created_at DESC;

-- name: GetLatestRunForIssue :one
-- One issue's newest run with the same display fields as the board lateral join,
-- for the single-card responses (e.g. after a manual drag) so a card never loses
-- its run badge on partial updates. run_count mirrors ListLatestRunsForRepo (a
-- window count over the issue's runs, already scoped to one issue by the WHERE) so
-- the "×N" retry hint survives a drag. Returns no rows when the issue has never run.
SELECT r.id, r.user_id, r.status, r.mr_iid, r.failure_reason, r.stop_kind, r.created_at, r.updated_at,
       ru.display_name AS owner_name, ru.email AS owner_email, rw.name AS worker_name,
       COUNT(*) OVER () AS run_count
FROM runs r
LEFT JOIN users ru ON ru.id = r.user_id
LEFT JOIN workers rw ON rw.id = r.worker_id
WHERE r.repo_id = @repo_id AND r.issue_iid = @issue_iid
ORDER BY r.created_at DESC
LIMIT 1;

-- name: ListMRWatchCandidates :many
-- MR-close watcher candidates (PRD #24 M2, Decision 4). Per repo, the issue's
-- LATEST run overall (DISTINCT ON, newest by created_at, riding
-- idx_runs_issue_history) — and ONLY that run, watched only while it is
-- completed with a known MR. The DISTINCT ON runs BEFORE the status/mr_iid
-- filter on purpose: filtering status='completed' inside the DISTINCT ON would
-- pick the latest COMPLETED run, silently watching a superseded MR while a newer
-- non-completed (rework) run exists — exactly the mid-rework misfire Decision 4
-- forbids. So a non-completed latest run yields NO candidate (the watch is
-- suppressed), and a completed latest run whose own mr_iid is NULL yields none
-- either (never fall back to an older run's MR).
--
-- The issue must be open, and a COARSE column prefilter keeps the polled set
-- tiny: the card is either labelled Human Review (the close-edge watch) or the
-- run already recorded mr_state='closed' (the reopen-edge watch, Decision 10).
-- This prefilter is deliberately NOT board.ResolveColumn — highest-position-wins
-- across multiple column labels is not cheaply expressible in SQL, so the
-- authoritative source-column guard is the Go ResolveColumn check in the watcher;
-- this only bounds how many MRs get polled.
WITH latest AS (
    SELECT DISTINCT ON (r.issue_iid)
           r.id, r.issue_iid, r.status, r.mr_iid, r.mr_state
    FROM runs r
    WHERE r.repo_id = @repo_id
    ORDER BY r.issue_iid, r.created_at DESC
)
SELECT l.id, l.issue_iid, l.mr_iid, l.mr_state
FROM latest l
JOIN issues i ON i.repo_id = @repo_id AND i.forge_issue_iid = l.issue_iid
WHERE l.status = 'completed'
  AND l.mr_iid IS NOT NULL
  AND i.state = 'opened'
  AND (jsonb_exists(i.labels, 'Human Review') OR l.mr_state = 'closed');

-- name: GetIssueByIID :one
SELECT * FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2;

-- name: DeleteIssuesNotIn :execrows
-- Reconcile eviction: drop cached issues absent from the fresh forge set. An
-- empty keep-set deletes everything for the repo (all PRD issues gone forge-side).
DELETE FROM issues
WHERE repo_id = $1 AND forge_issue_iid <> ALL(@keep_iids::bigint[]);
