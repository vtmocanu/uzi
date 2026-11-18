-- Issue tracker connections ------------------------------------------------

-- name: UpsertIssueTrackerConnection :one
-- Connect or reconnect/rotate: one connection per (user, kind, base_url).
-- Reconnecting with a fresh token updates the ciphertext and account identity
-- in place, keeps the row id, and stamps last_verified_at.
INSERT INTO issue_tracker_connections (
    user_id, kind, base_url, account_id, account_login, token_ciphertext, last_verified_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (user_id, kind, base_url) DO UPDATE
SET account_id       = EXCLUDED.account_id,
    account_login    = EXCLUDED.account_login,
    token_ciphertext = EXCLUDED.token_ciphertext,
    last_verified_at = now()
-- Defence in depth for the reconnect rule (PRD #4173 M4 owner guidance): a conflicting row
-- whose account would change is left alone while a repo links to it, and the statement then
-- returns no row. NOT a complete guard: under READ COMMITTED the subquery reads the snapshot
-- of the statement, taken before it waits on a concurrent link's row lock, so it cannot see
-- a link that commits during that wait. The service takes the row lock and counts in a
-- separate statement first (LockIssueTrackerConnectionByIdentityForUpdate).
WHERE issue_tracker_connections.account_id = EXCLUDED.account_id
   OR NOT EXISTS (SELECT 1 FROM repos WHERE repos.issue_tracker_connection_id = issue_tracker_connections.id)
RETURNING *;

-- name: ListIssueTrackerConnectionsByUser :many
SELECT * FROM issue_tracker_connections WHERE user_id = $1 ORDER BY created_at ASC, id ASC;

-- name: GetIssueTrackerConnectionForUser :one
SELECT * FROM issue_tracker_connections WHERE id = $1 AND user_id = $2;

-- name: DeleteIssueTrackerConnectionForUser :execrows
DELETE FROM issue_tracker_connections WHERE id = $1 AND user_id = $2;

-- name: LockIssueTrackerConnectionByIdentityForUpdate :one
-- Reconnect step 1 (PRD #4173 M4 owner guidance): read the existing row of the
-- (user, kind, base_url) identity and hold FOR UPDATE, so a link (which holds the
-- row FOR SHARE) and a reconnect that changes the account cannot interleave. No row
-- means a first connect, which no repo can link to yet.
SELECT * FROM issue_tracker_connections
WHERE user_id = $1 AND kind = $2 AND base_url = $3
FOR UPDATE;

-- name: LockIssueTrackerConnectionForShareForUser :one
-- The link's re-read of the connection inside its transaction. FOR SHARE conflicts
-- with the reconnect's FOR UPDATE, which the repos foreign key's own key-share lock
-- does not when the upsert changes only non-key columns.
SELECT * FROM issue_tracker_connections WHERE id = $1 AND user_id = $2
FOR SHARE;

-- name: InsertIssueTrackerConnectionIfAbsent :one
-- The first-connect insert: never updates, so it cannot overwrite a row (and its account)
-- that another connect created. No row returned means the identity exists already, and the
-- caller re-enters the locked reconnect path.
INSERT INTO issue_tracker_connections (
    user_id, kind, base_url, account_id, account_login, token_ciphertext, last_verified_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (user_id, kind, base_url) DO NOTHING
RETURNING *;
