-- +goose Up
-- Display evidence only. Eligibility alone never establishes ownership or source authority.
CREATE VIEW codex_active_run_cooldown AS
SELECT r.id AS run_id, r.user_id,
       (CASE WHEN t.rate_expiry IS NOT NULL AND
          (t.overload_expiry IS NULL OR t.rate_expiry >= t.overload_expiry)
         THEN 'rate_limit' ELSE 'overload' END)::text AS account_cooldown_class,
       codex_admission_run_eligible_at(r.id)::timestamptz AS account_cooldown_eligible_at
FROM runs r
JOIN worker_active_runs w ON w.run_id = r.id AND w.worker_id = r.worker_id
 AND w.claim_generation = r.claim_generation AND r.claim_generation > 0
JOIN workers owner ON owner.id = r.worker_id AND owner.user_id = r.user_id
JOIN codex_credential_state c ON c.user_secret_id = r.codex_secret_id AND c.user_id = r.user_id
JOIN user_secrets s ON s.id = c.user_secret_id AND s.user_id = r.user_id
JOIN codex_provider_account a ON a.id = c.provider_account_id AND a.user_id = r.user_id
JOIN codex_account_admission_throttle t ON t.user_id = r.user_id AND t.provider_account_id = a.id
WHERE r.status IN ('claimed', 'running', 'awaiting_approval')
 AND r.claim_released_at IS NULL AND NOT w.terminal_pending
 AND r.harness = 'codex' AND r.codex_auth_mode = 'subscription'
 AND owner.status = 'online' AND owner.last_heartbeat_at > now() - interval '45 seconds'
 AND s.disabled_at IS NULL AND s.kind = 'codex_auth' AND c.status = 'linked'
 AND c.material_revision = r.codex_material_revision
 AND a.credential_revision = r.codex_account_revision AND a.coord_state <> 'quarantined'
 AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
          THEN r.codex_account_key::jsonb = jsonb_build_array(a.provider_user_id, a.workspace_account_id)
          ELSE false END
 AND w.cooldown_observed_at <= now()
 AND w.cooldown_until > now()
 AND w.cooldown_until = w.cooldown_observed_at + interval '45 seconds'
 AND w.reported_at > now() - interval '45 seconds'
 AND w.cooldown_episode_id = t.episode_id
 AND w.cooldown_class IN ('rate_limit', 'overload')
 AND w.cooldown_eligible_at > w.cooldown_observed_at
 AND codex_admission_run_eligible_at(r.id) > now()
 AND (w.cooldown_operation->>'kind' IN ('root', 'child', 'checker')
      OR (w.cooldown_operation->>'kind' = 'advice'
          AND (r.kind = 'judge' OR r.review_target_run_id IS NOT NULL)))
 AND CASE WHEN w.cooldown_operation->'source' IS NULL OR w.cooldown_operation->'source' = 'null'::jsonb
   THEN NOT EXISTS (SELECT 1 FROM run_user_inputs i
     WHERE i.run_id = r.id AND i.kind = 'revise_plan'
       AND i.gate_binding = 'bound' AND i.gate_revision = r.gate_revision
       AND i.consumed_at IS NOT NULL AND i.applied_at IS NULL
       AND i.disposition IS DISTINCT FROM 'superseded'
       AND i.consumed_worker_id = r.worker_id
       AND i.consumed_claim_generation = r.claim_generation)
   ELSE w.cooldown_operation->'source'->>'gate_revision' = r.gate_revision::text
    AND w.cooldown_operation->'source'->>'presentation_id' = r.gate_presentation_id::text
    AND EXISTS (SELECT 1 FROM run_user_inputs i
      WHERE i.run_id = r.id AND i.id::text = w.cooldown_operation->'source'->>'revision_input_id'
        AND i.kind = 'revise_plan' AND i.gate_binding = 'bound' AND i.gate_revision = r.gate_revision
        AND i.consumed_at IS NOT NULL AND i.applied_at IS NULL
        AND i.disposition IS DISTINCT FROM 'superseded'
        AND i.consumed_worker_id = r.worker_id
        AND i.consumed_claim_generation = r.claim_generation)
   END;

-- +goose Down
DROP VIEW codex_active_run_cooldown;
