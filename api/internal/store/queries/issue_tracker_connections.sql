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
RETURNING *;

-- name: ListIssueTrackerConnectionsByUser :many
SELECT * FROM issue_tracker_connections WHERE user_id = $1 ORDER BY created_at ASC, id ASC;

-- name: GetIssueTrackerConnectionForUser :one
SELECT * FROM issue_tracker_connections WHERE id = $1 AND user_id = $2;

-- name: DeleteIssueTrackerConnectionForUser :execrows
DELETE FROM issue_tracker_connections WHERE id = $1 AND user_id = $2;
