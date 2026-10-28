-- name: GetCodexOperationAuthority :one
-- Callers hold the run lock. Eligibility is intentionally read only after this fence.
WITH authority AS MATERIALIZED (
SELECT r.id AS run_id, r.user_id, c.provider_account_id, r.gate_revision, r.gate_presentation_id,
       EXISTS (SELECT 1 FROM run_user_inputs i
         WHERE i.run_id = r.id AND i.kind = 'revise_plan'
           AND i.gate_binding = 'bound' AND i.gate_revision = r.gate_revision
           AND i.consumed_at IS NOT NULL AND i.applied_at IS NULL
           AND i.disposition IS DISTINCT FROM 'superseded'
           AND i.consumed_worker_id = r.worker_id
           AND i.consumed_claim_generation = r.claim_generation)::boolean AS revision_active
FROM runs r
JOIN codex_credential_state c ON c.user_secret_id = r.codex_secret_id AND c.user_id = r.user_id
JOIN user_secrets s ON s.id = c.user_secret_id AND s.user_id = r.user_id
JOIN codex_provider_account a ON a.id = c.provider_account_id AND a.user_id = r.user_id
WHERE r.id = @run_id::uuid AND r.user_id = @user_id::uuid AND r.worker_id = @worker_id::uuid
  AND r.claim_generation = @claim_generation::bigint AND r.claim_generation > 0
  AND r.claim_released_at IS NULL AND r.status IN ('claimed', 'running', 'awaiting_approval')
  AND r.harness = 'codex' AND r.codex_auth_mode = 'subscription'
  AND s.disabled_at IS NULL AND s.kind = 'codex_auth' AND c.status = 'linked'
  AND c.material_revision = r.codex_material_revision
  AND a.credential_revision = r.codex_account_revision
  AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
           THEN r.codex_account_key::jsonb = jsonb_build_array(a.provider_user_id, a.workspace_account_id)
           ELSE false END
)
SELECT authority.*, clock_timestamp()::timestamptz AS server_now,
 codex_admission_run_eligible_at(authority.run_id)::timestamptz AS eligible_at, t.episode_id,
 (CASE WHEN t.rate_expiry IS NOT NULL AND
  (t.overload_expiry IS NULL OR t.rate_expiry >= t.overload_expiry)
  THEN 'rate_limit' ELSE 'overload' END)::text AS class
FROM authority LEFT JOIN codex_account_admission_throttle t
 ON t.user_id = authority.user_id AND t.provider_account_id = authority.provider_account_id;

-- name: ValidCodexOperationSource :one
SELECT EXISTS (
 SELECT 1 FROM run_user_inputs i
 WHERE i.run_id = @run_id::uuid AND i.id = @revision_input_id::bigint AND i.kind = 'revise_plan'
   AND i.gate_binding = 'bound' AND i.gate_revision = @gate_revision::bigint
   AND i.consumed_at IS NOT NULL AND i.applied_at IS NULL
   AND i.disposition IS DISTINCT FROM 'superseded'
   AND i.consumed_worker_id = @worker_id::uuid
   AND i.consumed_claim_generation = @claim_generation::bigint
)::boolean;

-- name: ListRunAccountCooldowns :many
SELECT run_id, account_cooldown_class, account_cooldown_eligible_at
FROM codex_active_run_cooldown
WHERE run_id = ANY(@run_ids::uuid[]) AND user_id = @viewer_id::uuid;

-- name: SetWorkerActiveCooldown :exec
-- Only display metadata is updated; the frozen custody upsert has already succeeded.
UPDATE worker_active_runs SET
 cooldown_operation = sqlc.narg(operation)::jsonb,
 cooldown_class = sqlc.narg(class)::text,
 cooldown_episode_id = sqlc.narg(episode_id)::uuid,
 cooldown_eligible_at = sqlc.narg(eligible_at)::timestamptz,
 cooldown_observed_at = sqlc.narg(observed_at)::timestamptz,
 cooldown_until = CASE WHEN sqlc.narg(observed_at)::timestamptz IS NOT NULL
                       THEN sqlc.narg(observed_at)::timestamptz + interval '45 seconds' END
WHERE worker_id = @worker_id::uuid AND run_id = @run_id::uuid;
