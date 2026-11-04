-- +goose Up
-- First statement: the ALTER TABLE and constraint swap below wait on runs locks, so the bound
-- must cover them. A lock wait past it fails the migration, which can be rerun.
SET LOCAL lock_timeout = '5s';
-- Issue #3859: causal evidence that a worker flight departed before a claimed run ever
-- reported running. Both witnesses are bound to (claim_generation, worker register nonce,
-- snapshot epoch) so a later generation, a re-registration or a stale epoch cannot reuse them.
-- All columns are nullable and additive: NULL means no witness was recorded.
--
-- registration_evidence_*: written by the first running report (RecordRunRegistrationEvidence)
-- with the worker's own (claim_generation, register_nonce, registration epoch).
-- snapshot_departed_*: written inside an accepted active-snapshot replacement that omits the
-- run after an earlier witnessed epoch listed it or registration evidence preceded it.
ALTER TABLE runs
    ADD COLUMN registration_evidence_generation bigint,
    ADD COLUMN registration_evidence_nonce text,
    ADD COLUMN registration_evidence_epoch bigint,
    ADD COLUMN snapshot_departed_generation bigint,
    ADD COLUMN snapshot_departed_nonce text,
    ADD COLUMN snapshot_departed_epoch bigint;

-- New recovery cause: a retained continuation whose first running report was refused and whose
-- worker flight departed without a witness (parked for the owner; no automatic retry).
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted','worker_memory_pressure','retained_recovery_exhausted','codex_idle_stall','retained_recovery_adoption_failed','retained_continuation_unwitnessed'
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
       AND recovery_wait_cause IN ('worker_requeue_exhausted', 'worker_memory_pressure', 'retained_recovery_exhausted','retained_recovery_adoption_failed','retained_continuation_unwitnessed') THEN
       CASE WHEN capture_state = 'needs_action' THEN 'needs_action' ELSE 'source_only' END
  WHEN available AND NOT guarded THEN 'archive_ready'
  WHEN capture_state IN ('preparing', 'uploading') THEN 'capturing'
  WHEN capture_state = 'needs_action' THEN 'needs_action'
  WHEN COALESCE(run_status, '') <> '' AND run_status NOT IN ('completed', 'failed', 'cancelled') THEN 'active'
  ELSE 'source_only' END
$$;
-- +goose StatementEnd

-- +goose Down
-- Non-terminal reversal: a run parked at the new cause moves to retained_recovery_exhausted
-- with no retry stamp, the owner-held park the pre-00399 code already understands (Resume or
-- Cancel). The run, its claim and its custody hold are untouched, so no work is failed or lost.
UPDATE runs SET recovery_wait_cause='retained_recovery_exhausted', recovery_retry_not_before=NULL
WHERE recovery_wait_cause='retained_continuation_unwitnessed';
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted','worker_memory_pressure','retained_recovery_exhausted','codex_idle_stall','retained_recovery_adoption_failed'
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
       AND recovery_wait_cause IN ('worker_requeue_exhausted', 'worker_memory_pressure', 'retained_recovery_exhausted','retained_recovery_adoption_failed') THEN
       CASE WHEN capture_state = 'needs_action' THEN 'needs_action' ELSE 'source_only' END
  WHEN available AND NOT guarded THEN 'archive_ready'
  WHEN capture_state IN ('preparing', 'uploading') THEN 'capturing'
  WHEN capture_state = 'needs_action' THEN 'needs_action'
  WHEN COALESCE(run_status, '') <> '' AND run_status NOT IN ('completed', 'failed', 'cancelled') THEN 'active'
  ELSE 'source_only' END
$$;
-- +goose StatementEnd
ALTER TABLE runs
    DROP COLUMN snapshot_departed_epoch,
    DROP COLUMN snapshot_departed_nonce,
    DROP COLUMN snapshot_departed_generation,
    DROP COLUMN registration_evidence_epoch,
    DROP COLUMN registration_evidence_nonce,
    DROP COLUMN registration_evidence_generation;
