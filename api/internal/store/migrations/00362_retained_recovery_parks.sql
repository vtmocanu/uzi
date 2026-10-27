-- +goose Up
ALTER TABLE runs ADD COLUMN retained_park_count integer NOT NULL DEFAULT 0 CHECK (retained_park_count >= 0);
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted','worker_memory_pressure','retained_recovery_exhausted'
    )
);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_custody_attention(hold_state text, available boolean, guarded boolean,
                                    capture_state text, run_status text, recovery_wait_cause text)
RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE
  WHEN hold_state = 'discarded' THEN 'discarded'
  WHEN hold_state = 'released' THEN 'released'
  WHEN hold_state = 'open' AND run_status = 'recovery_wait'
       AND recovery_wait_cause IN ('worker_requeue_exhausted', 'worker_memory_pressure', 'retained_recovery_exhausted') THEN
       CASE WHEN capture_state = 'needs_action' THEN 'needs_action' ELSE 'source_only' END
  WHEN available AND NOT guarded THEN 'archive_ready'
  WHEN capture_state IN ('preparing', 'uploading') THEN 'capturing'
  WHEN capture_state = 'needs_action' THEN 'needs_action'
  WHEN COALESCE(run_status, '') <> '' AND run_status NOT IN ('completed', 'failed', 'cancelled') THEN 'active'
  ELSE 'source_only' END
$$;
-- +goose StatementEnd

-- +goose Down
UPDATE runs SET status='failed', fail_origin='worker_lost',
 failure_reason='retained recovery hold ended by schema rollback (00362)',
 finished_at=now(), status_since=now(), updated_at=now(),
 recovery_wait_cause=NULL, recovery_retry_not_before=NULL
WHERE status='recovery_wait' AND recovery_wait_cause='retained_recovery_exhausted';
UPDATE runs SET recovery_wait_cause=NULL, recovery_retry_not_before=NULL
WHERE recovery_wait_cause='retained_recovery_exhausted';
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
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted','worker_memory_pressure'
    )
);
ALTER TABLE runs DROP COLUMN retained_park_count;
