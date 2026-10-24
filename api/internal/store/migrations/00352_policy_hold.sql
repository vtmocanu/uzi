-- +goose Up
ALTER TABLE runs
 ADD COLUMN policy_refusal_count integer NOT NULL DEFAULT 0 CHECK (policy_refusal_count >= 0),
 ADD COLUMN policy_last_tag text CHECK (policy_last_tag IN ('cyberPolicy', 'misalignmentPolicyViolation')),
 ADD COLUMN policy_last_phase text CHECK (policy_last_phase IN ('planning', 'implementation')),
 ADD COLUMN policy_last_correlation_id text CHECK (policy_last_correlation_id IS NULL OR (
   policy_last_correlation_id ~ '^pr-[0-9a-f]{32}-[1-9][0-9]{0,15}$'
   AND policy_last_correlation_id !~ '[^a-z0-9-]'
   AND length(policy_last_correlation_id) BETWEEN 37 AND 52
   AND CASE WHEN policy_last_correlation_id ~ '^pr-[0-9a-f]{32}-[1-9][0-9]{0,15}$'
       THEN substring(policy_last_correlation_id FROM 37)::numeric <= 9007199254740991 ELSE false END)),
 ADD COLUMN policy_last_refused_at timestamptz,
 ADD COLUMN policy_hold_provenance text CHECK (policy_hold_provenance IN ('verified', 'unverified')),
 ADD COLUMN policy_hold_published boolean,
 ADD COLUMN policy_hold_detail text CHECK (policy_hold_detail IN ('refused', 'resume_rejected')),
 ADD COLUMN policy_last_outcome_id uuid,
 ADD COLUMN policy_last_source_generation bigint CHECK (policy_last_source_generation > 0),
 ADD COLUMN policy_fresh_claim_generation bigint CHECK (policy_fresh_claim_generation > 0);

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE runs IN SHARE ROW EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM runs
  WHERE (policy_refusal_count > 0 OR policy_fresh_claim_generation IS NOT NULL)
  AND status NOT IN ('completed', 'failed', 'cancelled')
  AND NOT (status IN ('paused', 'queued') OR
    (claim_released_at IS NOT NULL AND status IN
     ('limit_wait', 'pool_wait', 'recovery_wait', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')))) THEN
  RAISE EXCEPTION 'live or non-convertible policy-constrained rows exist; park/release before rollback';
 END IF;
END $$;
-- +goose StatementEnd
UPDATE runs AS r SET
 plan_cross_check_gate_reason = NULL,
 status = 'failed', status_since = now(),
 failure_reason = 'Codex provider safety-policy refusal (hold feature removed)',
 fail_origin = 'provider_policy_refusal', preserved_patch = NULL,
 move_pending_since = CASE WHEN r.issue_iid IS NOT NULL THEN now() END,
 finished_at = now(), milestones_in_progress = NULL, milestones_agents = NULL,
 pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
 credential_switch_requested_at = NULL, credential_switch_generation = NULL,
 hold_reason = NULL, hold_captured_head = NULL,
 health = 'ok', health_reason = NULL, health_since = NULL, updated_at = now()
WHERE r.status NOT IN ('completed', 'failed', 'cancelled')
 AND (r.policy_refusal_count > 0 OR r.policy_fresh_claim_generation IS NOT NULL);
ALTER TABLE runs
 DROP COLUMN policy_refusal_count, DROP COLUMN policy_last_tag, DROP COLUMN policy_last_phase,
 DROP COLUMN policy_last_correlation_id, DROP COLUMN policy_last_refused_at,
 DROP COLUMN policy_hold_provenance, DROP COLUMN policy_hold_published, DROP COLUMN policy_hold_detail,
 DROP COLUMN policy_last_outcome_id, DROP COLUMN policy_last_source_generation,
 DROP COLUMN policy_fresh_claim_generation;
