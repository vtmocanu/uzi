-- PRD #4416 M2: the claim's reads of the effective variable set and of the receipted secret
-- versions, the delivery receipts themselves, and the claim finisher's park and lock.

-- name: ListRunEnvEffectiveForRun :many
-- The effective injection set of a run: the enabled, live versions of its owner's repo scope (M2
-- reads the repo scope only; M4 adds the user scope with its precedence). The ciphertext rides
-- along so the claim can open the values; aad_version, var_id and rev rebuild the sealing context.
-- The idx run_env_vars_live_repo_name_key partial index serves the lookup.
SELECT id, var_id, repo_id, name, secret, description, ciphertext, sealed_with, aad_version, rev
FROM run_env_vars
WHERE user_id = @user_id AND repo_id = @repo_id
  AND retired_at IS NULL AND pending_delete_at IS NULL AND disabled_at IS NULL
ORDER BY name;

-- name: ListRunEnvEffectiveKeysForRun :many
-- The (var_id, rev) twin of ListRunEnvEffectiveForRun for the finish transaction's recompute: no
-- ciphertext, so the recompute under the owner fence moves no value.
SELECT var_id, rev, name
FROM run_env_vars
WHERE user_id = @user_id AND repo_id = @repo_id
  AND retired_at IS NULL AND pending_delete_at IS NULL AND disabled_at IS NULL
ORDER BY var_id, rev;

-- name: ListRunEnvRedactForRun :many
-- Every distinct secret version the run holds a receipt for, whatever generation delivered it. The
-- version row is LEFT JOINed on (var_id, rev) and the run's owner, so a version that is gone shows
-- as a NULL row_id (visible as missing, never dropped). effective says the version is in the
-- effective injection set (live, enabled, in the run's repo scope); the caller delivers it in the
-- environment instead and leaves it out of run_env_redact.
SELECT rc.var_id, rc.rev, rc.scope, rc.name AS receipt_name,
       v.id AS row_id, v.var_id AS row_var_id, v.rev AS row_rev, v.repo_id AS row_repo_id, v.name AS row_name, v.secret AS row_secret,
       v.ciphertext AS row_ciphertext, v.sealed_with AS row_sealed_with, v.aad_version AS row_aad_version,
       COALESCE(v.retired_at IS NULL AND v.pending_delete_at IS NULL AND v.disabled_at IS NULL
                AND v.repo_id = @repo_id::uuid, false)::boolean AS effective
FROM (SELECT r.var_id, r.rev, min(r.scope)::text AS scope, min(r.name)::text AS name
        FROM run_env_receipts r
       WHERE r.run_id = @run_id AND r.secret
       GROUP BY r.var_id, r.rev) rc
LEFT JOIN run_env_vars v ON v.var_id = rc.var_id AND v.rev = rc.rev AND v.user_id = @user_id
ORDER BY rc.var_id, rc.rev;

-- name: ListRunEnvRedactKeysForRun :many
-- The key twin of ListRunEnvRedactForRun for the finish recompute: no ciphertext, same rows, same
-- effective flag; present says the version row still exists.
SELECT rc.var_id, rc.rev, rc.name AS receipt_name, (v.id IS NOT NULL)::boolean AS present,
       COALESCE(v.retired_at IS NULL AND v.pending_delete_at IS NULL AND v.disabled_at IS NULL
                AND v.repo_id = @repo_id::uuid, false)::boolean AS effective
FROM (SELECT r.var_id, r.rev, min(r.name)::text AS name
        FROM run_env_receipts r
       WHERE r.run_id = @run_id AND r.secret
       GROUP BY r.var_id, r.rev) rc
LEFT JOIN run_env_vars v ON v.var_id = rc.var_id AND v.rev = rc.rev AND v.user_id = @user_id
ORDER BY rc.var_id, rc.rev;

-- name: UpsertRunEnvReceipt :exec
-- Append-only: a version delivered again by a later claim generation only advances
-- last_claim_generation. Carries no value and no hash.
INSERT INTO run_env_receipts (run_id, user_id, var_id, rev, scope, name, secret,
                              first_claim_generation, last_claim_generation)
VALUES (@run_id, @user_id, @var_id, @rev, @scope, @name, @secret, @claim_generation, @claim_generation)
ON CONFLICT (run_id, var_id, rev) DO UPDATE
   SET last_claim_generation = GREATEST(run_env_receipts.last_claim_generation, EXCLUDED.last_claim_generation);

-- name: RunEnvSecretReceiptExists :one
-- Whether any run holds a delivery receipt for one secret version (retention: such a version is
-- retired or kept pending deletion instead of dropped). Served by run_env_receipts_secret_idx.
SELECT EXISTS (SELECT 1 FROM run_env_receipts
                WHERE var_id = @var_id AND rev = @rev AND secret)::boolean AS held;

-- name: RunHasSecretRunEnvReceipts :one
SELECT EXISTS (SELECT 1 FROM run_env_receipts WHERE run_id = @run_id AND secret)::boolean AS held;

-- name: LockUserVaultStateForShareNowait :one
-- The claim finisher's share lock on the owner row, the fence every variable writer's FOR UPDATE
-- (FenceVaultWrite, LockUserVaultState) conflicts with. NOWAIT: 55P03 rolls the finish back for
-- finishRunClaim's bounded retry instead of blocking under the run lock.
SELECT vault_mode, vault_epoch FROM users WHERE id = @id FOR SHARE NOWAIT;

-- name: ParkRunEnvHistoryUnavailableRun :execrows
-- ParkRunOverrideUnavailableRun's twin with the hold reason fixed: a run holding a receipt for a
-- secret version that can no longer be delivered (missing or undecryptable) parks for its owner
-- instead of running unprotected. Same exact-claim fence, same undelivered-claim shape, same
-- one-claim Fresh restore. No automatic promoter reads this hold; the owner's resume re-queues the
-- run and the next claim parks it again while the version is missing.
WITH fresh_restored AS (
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT r.id, 'resume', '{"policy_fresh":"restored","generation":' || r.claim_generation::text || '}'
    FROM runs r
    WHERE r.id = @id AND r.worker_id = @worker_id AND r.claim_generation = @claim_generation
      AND r.claim_released_at IS NULL AND r.status = 'claimed'
      AND r.policy_fresh_consumed_generation = @claim_generation::bigint
      AND r.policy_fresh_confirmed_generation IS NULL AND r.policy_preserve_required
    RETURNING 1
)
UPDATE runs SET
    policy_pending_fresh = CASE WHEN runs.policy_fresh_consumed_generation = @claim_generation::bigint AND runs.policy_fresh_confirmed_generation IS NULL AND runs.policy_preserve_required
        THEN true ELSE runs.policy_pending_fresh END,
    policy_fresh_consumed_generation = CASE WHEN runs.policy_fresh_consumed_generation = @claim_generation::bigint AND runs.policy_fresh_confirmed_generation IS NULL AND runs.policy_preserve_required
        THEN NULL ELSE runs.policy_fresh_consumed_generation END,
    status = 'paused',
    status_since = now(),
    hold_reason = 'run_env_history_unavailable',
    claim_released_at = now(),
    codex_cap_hash = NULL,
    codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.id = @id AND runs.worker_id = @worker_id
  AND runs.claim_generation = @claim_generation
  AND runs.claim_released_at IS NULL
  AND runs.status = 'claimed';

-- name: RunEnvWorkerMissingForRun :one
-- PRD #4416 M2 health: true when the run is of a receiving kind, has an enabled live repo variable
-- or holds a secret receipt, and none of its owner's ONLINE, intake-accepting, non-ephemeral,
-- non-lane workers reports 'run_env_v1'. It drives the queued-reason resolver's run-env rung:
-- ClaimRun's standalone run_env clause can never be satisfied for such a run. Mirrors
-- IssueKeyWorkerMissingForRepo's worker filters and the claim clause's predicate.
SELECT COALESCE((EXISTS (
  SELECT 1 FROM runs r
  WHERE r.id = @run_id
    AND fn_run_env_lane(r.kind, r.review_target_run_id, r.egress_profile_id)
    AND (EXISTS (SELECT 1 FROM run_env_vars ev WHERE ev.user_id = r.user_id AND ev.repo_id = r.repo_id AND ev.retired_at IS NULL AND ev.pending_delete_at IS NULL AND ev.disabled_at IS NULL)
         OR EXISTS (SELECT 1 FROM run_env_receipts rr WHERE rr.run_id = r.id AND rr.secret))
) AND NOT EXISTS (
  SELECT 1 FROM workers w
  WHERE w.user_id = @user_id
    AND w.status = 'online'
    AND fn_worker_accepts_new_intake(w)
    AND NOT w.ephemeral
    AND NOT w.isolated_lane
    AND 'run_env_v1' = ANY(w.protocol_capabilities)
)), false)::boolean AS missing;
