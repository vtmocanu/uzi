-- Codex account auto-selection (issue #3719): the reads and writes the run-creation path
-- uses to pick among a user's pooled Codex subscription aliases by limit headroom.

-- name: ListCodexPoolAliases :many
-- Every codex_auth alias the user has pooled (auto_eligible), DISABLED rows included, so
-- "configured but fully disabled" stays distinct from "no pool configured". LEFT JOINed to
-- the alias's account link, the canonical account (identity tuple, coordination state,
-- reauth flag) and the account's admission throttle; an unlinked alias carries NULL
-- status/account columns. Owner-scoped, ordered by alias id so the lowest pooled alias of an
-- account is deterministic. Carries no secret material.
SELECT
    us.id                               AS alias_id,
    (us.disabled_at IS NULL)::boolean   AS enabled,
    cs.status                           AS link_status,
    cs.provider_account_id,
    a.provider_user_id,
    a.workspace_account_id,
    a.coord_state,
    a.reauth_required,
    t.effective_expiry                  AS throttle_effective_expiry,
    t.spread_width_ms                   AS throttle_spread_width_ms
FROM user_secrets us
LEFT JOIN codex_credential_state cs
    ON cs.user_secret_id = us.id AND cs.user_id = us.user_id
LEFT JOIN codex_provider_account a
    ON a.id = cs.provider_account_id AND a.user_id = us.user_id
LEFT JOIN codex_account_admission_throttle t
    ON t.user_id = us.user_id AND t.provider_account_id = a.id
WHERE us.user_id = @user_id AND us.kind = 'codex_auth' AND us.auto_eligible
ORDER BY us.id;

-- name: LockCodexPoolAliasForSelection :one
-- The consent and link fence of the selection path: a FOR SHARE lock on the chosen alias
-- row (the LockSecretForPromotion lock mode) that returns what the fence must re-validate
-- under it: the row's kind, its pool consent, whether it is enabled, and its account link.
-- pgx.ErrNoRows means no such alias for this owner.
SELECT
    us.kind,
    us.auto_eligible,
    (us.disabled_at IS NULL)::boolean AS enabled,
    cs.status,
    cs.provider_account_id
FROM user_secrets us
LEFT JOIN codex_credential_state cs
    ON cs.user_secret_id = us.id AND cs.user_id = us.user_id
WHERE us.id = @id AND us.user_id = @user_id
FOR SHARE OF us;

-- name: CountCodexInFlightByAccount :many
-- Per canonical Codex account, how many of the user's Codex runs are in a NONTERMINAL
-- status: the in-flight bias the selector subtracts (InflightPenalty points per run).
-- Selection happens at creation, so QUEUED runs are exactly the herd the bias must spread:
-- a sweep firing several runs back to back creates them all before any is claimed. That is
-- why this set is the whole nonterminal list, deliberately wider than Claude's claim-time
-- set. The status list is explicit (it uses idx_runs_claimable) and a guard test pins it
-- to runs_status_check minus the terminal statuses.
--
-- Counted by the run's frozen codex_account_key (the identity tuple), matched to the
-- account through the same pg_input_is_valid + jsonb_build_array idiom as 00378, so aliases
-- of one account share a count and it survives alias deletion and relinking. A run with a
-- NULL key (unlinked at create, never runnable) is not counted.
SELECT
    a.id     AS provider_account_id,
    count(*) AS in_flight
FROM runs r
JOIN codex_provider_account a
    ON a.user_id = r.user_id
   AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
            THEN r.codex_account_key::jsonb = jsonb_build_array(a.provider_user_id, a.workspace_account_id)
            ELSE false END
WHERE r.user_id = @user_id
  AND r.harness = 'codex'
  AND r.status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                   'limit_wait', 'awaiting_followup', 'pool_wait', 'paused', 'recovery_wait')
GROUP BY a.id;

-- name: SetRunCodexSelectReason :execrows
-- Record why the creation path bound the run's Codex alias. Write-once: only a Codex run
-- whose reason is still NULL is written, so a replay cannot overwrite the first choice.
-- Owner-scoped; 0 rows for a foreign, non-Codex or already-marked run.
UPDATE runs SET codex_select_reason = @reason::text
WHERE id = @id AND user_id = @user_id AND harness = 'codex' AND codex_select_reason IS NULL;

-- name: GetUserCodexAccountMode :one
-- The user's Codex account mode ('default' | 'auto').
SELECT codex_account_mode FROM users WHERE id = @id;

-- name: SetUserCodexAccountMode :execrows
-- Set the user's Codex account mode. 0 rows for an unknown user.
UPDATE users SET codex_account_mode = @mode WHERE id = @id;
