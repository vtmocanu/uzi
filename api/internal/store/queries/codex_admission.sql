-- name: LockCodexAdmissionRun :one
-- Run lock always precedes the throttle FK check; no alias/account row locks.
SELECT id FROM runs
WHERE id = $1::uuid AND user_id = $2::uuid AND worker_id = $3::uuid
  AND claim_generation = $4::bigint AND claim_generation > 0
  AND claim_released_at IS NULL AND status IN ('claimed', 'running', 'awaiting_approval')
  AND harness = 'codex' AND codex_auth_mode = 'subscription'
FOR SHARE;

-- name: ObserveCodexAdmission :one
-- READ COMMITTED statement snapshot: re-login committed before this snapshot is refused.
-- A replacement committed after this snapshot can race an accepted signal by design.
WITH inputs AS MATERIALIZED (
    SELECT clock_timestamp() AS t, random() * 0.2 AS jitter, gen_random_uuid() AS seed
), source AS MATERIALIZED (
    SELECT r.user_id, c.provider_account_id
    FROM runs r
    JOIN codex_credential_state c ON c.user_secret_id = r.codex_secret_id AND c.user_id = r.user_id
    JOIN user_secrets s ON s.id = c.user_secret_id AND s.user_id = r.user_id
    JOIN codex_provider_account a ON a.id = c.provider_account_id AND a.user_id = r.user_id
    WHERE r.id = $1::uuid AND r.user_id = $2::uuid AND r.worker_id = $3::uuid
      AND r.claim_generation = $4::bigint AND r.claim_generation > 0
      AND r.claim_released_at IS NULL AND r.status IN ('claimed', 'running', 'awaiting_approval')
      AND r.harness = 'codex' AND r.codex_auth_mode = 'subscription'
      AND s.disabled_at IS NULL AND s.kind = 'codex_auth' AND c.status = 'linked'
      AND c.material_revision = r.codex_material_revision
      AND a.credential_revision = r.codex_account_revision
      AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
               THEN r.codex_account_key::jsonb
                    = jsonb_build_array(a.provider_user_id, a.workspace_account_id)
               ELSE false END
), initial AS MATERIALIZED (
    SELECT codex_admission_transition(NULL::codex_account_admission_throttle,
        source.user_id, source.provider_account_id, $1::uuid, $4::bigint,
        $5::uuid, $6::text, inputs.t, inputs.jitter, inputs.seed) AS state
    FROM source CROSS JOIN inputs
)
INSERT INTO codex_account_admission_throttle AS current_state
SELECT (state).user_id, (state).provider_account_id, (state).rate_step, (state).rate_expiry, (state).rate_last_qualifying, (state).overload_step, (state).overload_expiry, (state).overload_last_qualifying, (state).observation_clock, (state).receipts, (state).overload_votes, (state).episode_id, (state).spread_width_ms, (state).effective_expiry, (state).recovery_floor FROM initial WHERE true
ON CONFLICT (user_id, provider_account_id) DO UPDATE
SET (rate_step, rate_expiry, rate_last_qualifying, overload_step, overload_expiry, overload_last_qualifying, observation_clock, receipts, overload_votes, episode_id, spread_width_ms, effective_expiry, recovery_floor) = (
    SELECT next_state.rate_step, next_state.rate_expiry, next_state.rate_last_qualifying, next_state.overload_step, next_state.overload_expiry, next_state.overload_last_qualifying, next_state.observation_clock, next_state.receipts, next_state.overload_votes, next_state.episode_id, next_state.spread_width_ms, next_state.effective_expiry, next_state.recovery_floor
    FROM inputs CROSS JOIN LATERAL codex_admission_transition(current_state,
        current_state.user_id, current_state.provider_account_id,
        $1::uuid, $4::bigint, $5::uuid, $6::text,
        inputs.t, inputs.jitter, inputs.seed) next_state
)
RETURNING *;

-- name: GetCodexAdmissionState :one
SELECT *, coalesce((CASE
    WHEN rate_expiry IS NULL AND overload_expiry IS NULL THEN NULL
    WHEN overload_expiry IS NULL OR rate_expiry >= overload_expiry THEN 'rate_limit'
    ELSE 'overload' END)::text, '')::text AS effective_class
FROM codex_account_admission_throttle
WHERE user_id = @user_id::uuid AND provider_account_id = @provider_account_id::uuid;

-- name: GetCodexAdmissionRunEligibility :one
SELECT codex_admission_run_eligible_at(@run_id::uuid)::timestamptz AS eligible_at;
