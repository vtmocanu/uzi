-- PRD #4123 M3: the weekly-limit warning ledger (weekly_limit_alerts, migration 00513).
-- Every query is keyed by the owning user AND the credential, and every observation
-- comparison happens here in SQL, never in Go.

-- name: SeedWeeklyLimitAlertAnthropic :exec
-- The first-reading seed: a row that no real observation can be older than ('-infinity').
-- DO NOTHING keeps an existing row (and takes no FK lock on the parent when it exists).
INSERT INTO weekly_limit_alerts (user_id, anthropic_secret_id, observed_at)
VALUES (@user_id::uuid, @secret_id::uuid, '-infinity')
ON CONFLICT (user_id, anthropic_secret_id) DO NOTHING;

-- name: SeedWeeklyLimitAlertCodex :exec
INSERT INTO weekly_limit_alerts (user_id, codex_account_id, observed_at)
VALUES (@user_id::uuid, @account_id::uuid, '-infinity')
ON CONFLICT (user_id, codex_account_id) DO NOTHING;

-- name: LockWeeklyLimitAlertAnthropic :one
SELECT * FROM weekly_limit_alerts
WHERE user_id = @user_id AND anthropic_secret_id = @secret_id::uuid
FOR UPDATE;

-- name: LockWeeklyLimitAlertCodex :one
SELECT * FROM weekly_limit_alerts
WHERE user_id = @user_id AND codex_account_id = @account_id::uuid
FOR UPDATE;

-- name: ApplyWeeklyLimitObservation :one
-- The strict fence: a reading at an equal or older instant matches no row (pgx.ErrNoRows =
-- dropped). On acceptance it records the instant, the anchor, the Codex multi-window flag and
-- the warned flag the caller decided, and returns the row.
UPDATE weekly_limit_alerts SET
    observed_at   = @observed_at::timestamptz,
    anchor_reset  = sqlc.narg('anchor_reset')::timestamptz,
    anchor_pct    = sqlc.narg('anchor_pct')::double precision,
    anchor_bucket = sqlc.narg('anchor_bucket')::text,
    anchor_multi  = @anchor_multi::boolean,
    warned        = @warned::boolean,
    updated_at    = now()
WHERE id = @id AND observed_at < @observed_at::timestamptz
RETURNING *;

-- name: ClaimWeeklyLimitWarning :one
-- Claims this period's warning for exactly the observation the caller made. A newer
-- observation (observed_at moved), an already-claimed period, or a retry after an ambiguous
-- commit matches no row (pgx.ErrNoRows = skip), so equal-timestamp readings cannot both claim.
UPDATE weekly_limit_alerts SET warned = true, updated_at = now()
WHERE id = @id AND observed_at = @observed_at::timestamptz AND warned = false
RETURNING id;

-- name: HasNonTerminalRunForAnthropicSecret :one
SELECT EXISTS (
    SELECT 1 FROM runs r
    WHERE r.user_id = @user_id AND r.anthropic_secret_id = @secret_id::uuid
      AND r.status NOT IN ('completed', 'failed', 'cancelled')
) AS has_run;

-- name: HasNonTerminalRunForCodexAccount :one
SELECT EXISTS (
    SELECT 1 FROM runs r
    JOIN codex_credential_state s ON s.user_secret_id = r.codex_secret_id AND s.user_id = r.user_id
    WHERE r.user_id = @user_id AND s.provider_account_id = @account_id::uuid
      AND r.status NOT IN ('completed', 'failed', 'cancelled')
) AS has_run;

-- The four queries below are the DM's run total and run lines. Their predicates are EXACTLY those of
-- the two HasNonTerminalRun queries above (the claim's), so the DM's total can never disagree with the
-- claim: no kind exclusion (a chat run counts) and no row cap on the count.

-- name: CountNonTerminalRunsForAnthropicSecret :one
SELECT count(*) FROM runs r
WHERE r.user_id = @user_id AND r.anthropic_secret_id = @secret_id::uuid
  AND r.status NOT IN ('completed', 'failed', 'cancelled');

-- name: ListNonTerminalRunsForAnthropicSecret :many
SELECT r.* FROM runs r
WHERE r.user_id = @user_id AND r.anthropic_secret_id = @secret_id::uuid
  AND r.status NOT IN ('completed', 'failed', 'cancelled')
ORDER BY r.created_at DESC, r.id DESC
LIMIT @max_rows::int;

-- name: CountNonTerminalRunsForCodexAccount :one
SELECT count(*) FROM runs r
JOIN codex_credential_state s ON s.user_secret_id = r.codex_secret_id AND s.user_id = r.user_id
WHERE r.user_id = @user_id AND s.provider_account_id = @account_id::uuid
  AND r.status NOT IN ('completed', 'failed', 'cancelled');

-- name: ListNonTerminalRunsForCodexAccount :many
SELECT r.* FROM runs r
JOIN codex_credential_state s ON s.user_secret_id = r.codex_secret_id AND s.user_id = r.user_id
WHERE r.user_id = @user_id AND s.provider_account_id = @account_id::uuid
  AND r.status NOT IN ('completed', 'failed', 'cancelled')
ORDER BY r.created_at DESC, r.id DESC
LIMIT @max_rows::int;
