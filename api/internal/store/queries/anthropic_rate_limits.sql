-- name: ListUsersWithAnthropicToken :many
-- The user ids that hold an anthropic_token secret — the poller's per-tick work
-- set (PRD #53). It lists candidates only; whether the token can actually be
-- opened (vault unlocked, master-sealed exception) is decided per user at open
-- time, so a locked user still appears here and is skipped downstream (D3).
SELECT user_id FROM user_secrets WHERE kind = 'anthropic_token';

-- name: UpsertRateLimits :exec
-- Overwrite a user's single gauge row each poll tick (D4). A malformed reading
-- never reaches here (the poller fails closed and keeps the last good row, D5),
-- so every write carries a complete reading.
INSERT INTO anthropic_rate_limits (
    user_id, five_hour_pct, five_hour_resets_at,
    seven_day_pct, seven_day_resets_at, source, synced_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id) DO UPDATE SET
    five_hour_pct       = EXCLUDED.five_hour_pct,
    five_hour_resets_at = EXCLUDED.five_hour_resets_at,
    seven_day_pct       = EXCLUDED.seven_day_pct,
    seven_day_resets_at = EXCLUDED.seven_day_resets_at,
    source              = EXCLUDED.source,
    synced_at           = EXCLUDED.synced_at;

-- name: GetRateLimits :one
-- One user's gauge row, for GET /api/me/rate-limits (M2). Absence (pgx.ErrNoRows)
-- means no reading yet → the handler returns `unavailable`.
SELECT user_id, five_hour_pct, five_hour_resets_at,
       seven_day_pct, seven_day_resets_at, source, synced_at
FROM anthropic_rate_limits
WHERE user_id = $1;

-- name: ListRateLimits :many
-- Every user LEFT JOINed to their gauge row, for GET /api/admin/rate-limits (M2):
-- the admin view lists everyone, including users with no token / no reading yet
-- (their limit columns come back NULL). vault_locked is computed in-memory from
-- the live vault, not stored, so it is not selected here.
SELECT
    u.id           AS user_id,
    u.email        AS email,
    u.display_name AS display_name,
    rl.five_hour_pct,
    rl.five_hour_resets_at,
    rl.seven_day_pct,
    rl.seven_day_resets_at,
    rl.source,
    rl.synced_at
FROM users u
LEFT JOIN anthropic_rate_limits rl ON rl.user_id = u.id
ORDER BY u.email ASC;

-- name: DeleteRateLimits :execrows
-- Drop a user's gauge row when their token is deleted (D3b) so a token-less user
-- never shows a ghost reading. Idempotent: deleting an absent row affects 0 rows.
DELETE FROM anthropic_rate_limits WHERE user_id = $1;
