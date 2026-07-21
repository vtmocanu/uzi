-- name: ListAnthropicTokensToPoll :many
-- Every anthropic_token secret to poll each tick (PRD #104 M5): the token's id and
-- owner plus the sealed ciphertext and its sealed_with, so the poller opens them in
-- one pass instead of re-fetching each ciphertext (N+1). The ciphertext is opened
-- in-process via the vault path and is never logged nor placed in any error string.
-- Whether a given token can actually be opened (vault unlocked, master-sealed
-- exception) is decided at open time, so a locked user's tokens still appear here
-- and are skipped (PRD #53 D3).
--
-- One row per TOKEN, not per user (it was ListUsersWithAnthropicToken until M5).
-- The windows Anthropic reports are per-credential, so a user with three tokens is
-- three readings; polling only the default would leave the other two rendering
-- someone else's numbers. Poll cost therefore scales with token count, not user
-- count — see R3.
--
-- ORDER BY keeps a tick's work deterministic, which makes a slow tick's log
-- readable and a partial tick's coverage predictable rather than arbitrary.
SELECT id, user_id, ciphertext, sealed_with FROM user_secrets
WHERE kind = 'anthropic_token'
ORDER BY user_id, id;

-- name: UpsertRateLimits :exec
-- Overwrite ONE token's gauge row each poll tick (PRD #53 D4, repointed by #104
-- M5). A malformed reading never reaches here (the poller fails closed and keeps
-- the last good row, D5), so every write carries a complete reading.
--
-- user_id rides along rather than being looked up: the caller already has it from
-- the poll listing, and it is half of the composite FK that ties this row to a
-- (user, token) pair that exists.
--
-- The FK is checked on the INSERT path only: ON CONFLICT .. DO UPDATE deliberately
-- does not touch user_id, so an upsert over an EXISTING row rewrites the reading
-- without re-validating ownership. That is safe BY CONSTRUCTION, not by the
-- caller's discipline — user_secret_id is the global PRIMARY KEY of user_secrets,
-- so an id belongs to exactly one owner for its whole life and no call site can
-- construct a mismatched (user_id, user_secret_id) pair to smuggle through the
-- conflict path. Stated this way on purpose: "the poller always passes a matching
-- pair" would be the weaker true reason, and the weaker one is the one that rots
-- the moment someone adds a third caller.
INSERT INTO anthropic_rate_limits (
    user_secret_id, user_id, five_hour_pct, five_hour_resets_at,
    seven_day_pct, seven_day_resets_at, source, synced_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (user_secret_id) DO UPDATE SET
    five_hour_pct       = EXCLUDED.five_hour_pct,
    five_hour_resets_at = EXCLUDED.five_hour_resets_at,
    seven_day_pct       = EXCLUDED.seven_day_pct,
    seven_day_resets_at = EXCLUDED.seven_day_resets_at,
    source              = EXCLUDED.source,
    synced_at           = EXCLUDED.synced_at;

-- name: ListRateLimitsForUser :many
-- One user's meters, one row per TOKEN, for GET /api/me/rate-limits (PRD #104 D4 —
-- a breaking response-shape change from the single reading PRD #53 returned).
--
-- Driven from user_secrets and LEFT JOINed to the gauge, so a token with no reading
-- yet still appears (as `unavailable`) instead of vanishing from the list: a user
-- who just added a token should see it listed with no numbers, not see nothing.
-- Carries label + is_default because the UI has to say WHICH credential each meter
-- describes — the whole point of the change — and those are names, never values.
--
-- Ordered default-first then by label, so the meter a user's unbound workers spend
-- against leads the list.
SELECT s.id            AS user_secret_id,
       s.label         AS label,
       s.is_default    AS is_default,
       rl.five_hour_pct,
       rl.five_hour_resets_at,
       rl.seven_day_pct,
       rl.seven_day_resets_at,
       rl.source,
       rl.synced_at
FROM user_secrets s
LEFT JOIN anthropic_rate_limits rl ON rl.user_secret_id = s.id
WHERE s.user_id = $1 AND s.kind = 'anthropic_token'
ORDER BY s.is_default DESC, lower(s.label) ASC;

-- name: ListRateLimits :many
-- Every user's tokens LEFT JOINed to their gauge rows, for
-- GET /api/admin/rate-limits (PRD #104 D4): one row per (user, token), plus one row
-- per TOKEN-LESS user so the admin view still lists everyone.
--
-- The LEFT JOIN chain is users → user_secrets → anthropic_rate_limits, so a
-- token-less user yields a single row with a NULL user_secret_id (rendered
-- `no_token`), a token with no reading yields a row with a NULL synced_at
-- (`unavailable`), and a token with a reading yields the reading. has_token is
-- therefore derivable here (user_secret_id IS NOT NULL) rather than needing the
-- separate EXISTS the per-user shape used.
--
-- vault_locked is computed in-memory from the live vault, not stored, so it is not
-- selected here.
SELECT
    u.id           AS user_id,
    u.email        AS email,
    u.display_name AS display_name,
    s.id           AS user_secret_id,
    s.label        AS label,
    s.is_default   AS is_default,
    rl.five_hour_pct,
    rl.five_hour_resets_at,
    rl.seven_day_pct,
    rl.seven_day_resets_at,
    rl.source,
    rl.synced_at
FROM users u
LEFT JOIN user_secrets s ON s.user_id = u.id AND s.kind = 'anthropic_token'
LEFT JOIN anthropic_rate_limits rl ON rl.user_secret_id = s.id
ORDER BY u.email ASC, s.is_default DESC NULLS LAST, lower(s.label) ASC;

-- name: UserHasAnthropicToken :one
-- Whether the user holds an anthropic_token secret, for GET /api/me/rate-limits:
-- the handler derives `no_token` from this (secret-existence), not from the
-- rate_limits rows being absent. Deliberately NOT filtered on is_default — it
-- answers "does this user have any credential at all", which is the question
-- `no_token` asks. Never selects the ciphertext.
SELECT EXISTS (
    SELECT 1 FROM user_secrets WHERE user_id = $1 AND kind = 'anthropic_token'
);

-- name: DeleteRateLimits :execrows
-- Drop every gauge row a user holds. Since #104 M5 the composite FK CASCADES a
-- token's gauge row when the token itself is deleted, so this is no longer the
-- mechanism that prevents a ghost reading — the database is. It stays as the
-- belt-and-suspenders sweep the token-delete path still runs (PRD #53 D3b), and as
-- the thing that would still clear rows if a future schema change ever loosened
-- that cascade. Idempotent: 0 rows when there is nothing to drop.
DELETE FROM anthropic_rate_limits WHERE user_id = $1;
