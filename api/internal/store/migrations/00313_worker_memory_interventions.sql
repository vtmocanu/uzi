-- +goose Up
ALTER TABLE runs
    ADD COLUMN memory_episode bigint NOT NULL DEFAULT 0 CHECK (memory_episode >= 0),
    ADD COLUMN memory_intervention_count integer NOT NULL DEFAULT 0 CHECK (memory_intervention_count >= 0),
    ADD COLUMN memory_policy jsonb;
ALTER TABLE runs ADD CONSTRAINT runs_memory_policy_check CHECK (
    memory_policy IS NULL OR (
        jsonb_typeof(memory_policy) = 'object'
        AND memory_policy ?& ARRAY['version','max_interventions']
        AND memory_policy - ARRAY['version','max_interventions'] = '{}'::jsonb
        AND memory_policy->>'version' = '1'
        AND (memory_policy->>'max_interventions') ~ '^[0-9]{1,4}$'
        AND (memory_policy->>'max_interventions')::integer BETWEEN 1 AND 9999
        AND memory_intervention_count <= (memory_policy->>'max_interventions')::integer
    )
);
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted','worker_memory_pressure'
    )
);
CREATE TABLE memory_interventions (
    intervention_id uuid PRIMARY KEY,
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    worker_id uuid NOT NULL,
    register_nonce text NOT NULL CHECK (length(register_nonce) BETWEEN 1 AND 128),
    claim_generation bigint NOT NULL CHECK (claim_generation >= 0),
    memory_episode bigint NOT NULL CHECK (memory_episode >= 0),
    admitted boolean NOT NULL,
    allowance_used integer NOT NULL CHECK (allowance_used >= 0),
    policy jsonb NOT NULL,
    outcome text CHECK (outcome IN ('no_signal','unknown','confirmed_drained')),
    reserved_at timestamptz NOT NULL DEFAULT now(),
    outcome_at timestamptz,
    CHECK ((outcome IS NULL) = (outcome_at IS NULL)),
    CHECK (admitted OR outcome IS NULL)
);
CREATE INDEX memory_interventions_run_episode ON memory_interventions(run_id, memory_episode);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_custody_attention(hold_state text, available boolean, guarded boolean,
                                    capture_state text, run_status text, recovery_wait_cause text)
RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE
  WHEN hold_state = 'discarded' THEN 'discarded'
  WHEN hold_state = 'released' THEN 'released'
  WHEN hold_state = 'open' AND run_status = 'recovery_wait'
       AND recovery_wait_cause IN ('worker_requeue_exhausted', 'worker_memory_pressure') THEN
       CASE WHEN capture_state = 'needs_action' THEN 'needs_action' ELSE 'source_only' END
  WHEN available AND NOT guarded THEN 'archive_ready'
  WHEN capture_state IN ('preparing', 'uploading') THEN 'capturing'
  WHEN capture_state = 'needs_action' THEN 'needs_action'
  WHEN COALESCE(run_status, '') <> '' AND run_status NOT IN ('completed', 'failed', 'cancelled') THEN 'active'
  ELSE 'source_only' END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_custody_attention(hold_state text, available boolean, guarded boolean,
                                    capture_state text, run_status text, recovery_wait_cause text)
RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE
  WHEN hold_state = 'discarded' THEN 'discarded'
  WHEN hold_state = 'released' THEN 'released'
  WHEN hold_state = 'open' AND run_status = 'recovery_wait'
       AND recovery_wait_cause = 'worker_requeue_exhausted' THEN
       CASE WHEN capture_state = 'needs_action' THEN 'needs_action' ELSE 'source_only' END
  WHEN available AND NOT guarded THEN 'archive_ready'
  WHEN capture_state IN ('preparing', 'uploading') THEN 'capturing'
  WHEN capture_state = 'needs_action' THEN 'needs_action'
  WHEN COALESCE(run_status, '') <> '' AND run_status NOT IN ('completed', 'failed', 'cancelled') THEN 'active'
  ELSE 'source_only' END
$$;
-- +goose StatementEnd

UPDATE runs SET status='failed', fail_origin='worker_lost',
    failure_reason='memory pressure hold ended by schema rollback (00313)',
    finished_at=now(), status_since=now(), updated_at=now(),
    recovery_wait_cause=NULL, recovery_retry_not_before=NULL
WHERE status='recovery_wait' AND recovery_wait_cause='worker_memory_pressure';
UPDATE runs SET recovery_wait_cause=NULL, recovery_retry_not_before=NULL
WHERE recovery_wait_cause='worker_memory_pressure';
DROP TABLE memory_interventions;
ALTER TABLE runs DROP COLUMN memory_policy, DROP COLUMN memory_intervention_count, DROP COLUMN memory_episode;
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted'
    )
);
