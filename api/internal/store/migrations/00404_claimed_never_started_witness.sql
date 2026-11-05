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

-- The unwitnessed park (retained_continuation_unwitnessed) records its observation next to the
-- historic exhaustion snapshot, so the evidence check admits four optional keys.
ALTER TABLE runs DROP CONSTRAINT runs_worker_recovery_evidence_check;
ALTER TABLE runs ADD CONSTRAINT runs_worker_recovery_evidence_check CHECK (
    worker_recovery_evidence IS NULL OR (
        jsonb_typeof(worker_recovery_evidence) = 'object'
        AND worker_recovery_evidence ?& ARRAY['checkpoint_tip','available_capture','publication_uncertain','capture_uncertain','custody_uncertain','unknown','recorded_at']
        AND worker_recovery_evidence - ARRAY['checkpoint_tip','available_capture','publication_uncertain','capture_uncertain','custody_uncertain','unknown','recorded_at','presence','registration_evidence_epoch','worker_snapshot_epoch','deadline'] = '{}'::jsonb
        AND (worker_recovery_evidence->'checkpoint_tip' = 'null'::jsonb OR
             (jsonb_typeof(worker_recovery_evidence->'checkpoint_tip') = 'string'
              AND worker_recovery_evidence->>'checkpoint_tip' ~ '^[0-9a-f]{40}$'))
        AND jsonb_typeof(worker_recovery_evidence->'available_capture') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'publication_uncertain') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'capture_uncertain') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'custody_uncertain') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'unknown') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'recorded_at') = 'string'
        AND length(worker_recovery_evidence->>'recorded_at') BETWEEN 20 AND 40
        AND (NOT worker_recovery_evidence ? 'presence' OR worker_recovery_evidence->>'presence' IN ('p1', 'p2'))
        AND pg_column_size(worker_recovery_evidence) <= 1024
    )
);

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
SET LOCAL lock_timeout = '5s';
-- Serialise against live run writers while the Down rewrites rows and narrows the CHECKs; a held
-- lock fails the rollback after 5s rather than queueing every runs writer behind it.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- A live unwitnessed park has no pre-00404 equivalent: relabelling it as another cause would
-- fabricate a classification the run never earned. Refuse the rollback while any run is still
-- parked there (checked under the lock above) so the owner decides first.
-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM runs
    WHERE status = 'recovery_wait' AND recovery_wait_cause = 'retained_continuation_unwitnessed';
    IF n > 0 THEN
        RAISE EXCEPTION 'refusing to roll back 00404: % run(s) are parked in recovery_wait at retained_continuation_unwitnessed; Resume or Cancel those runs first so no parked run is relabelled', n;
    END IF;
END
$$;
-- +goose StatementEnd
-- Any row still carrying the cause (an owner Cancel or a later terminal write leaves it set; the
-- guard above leaves no live recovery_wait park, and no CHECK ties the cause to a status) holds a
-- value the prior CHECK rejects and that no longer describes a live wait: clear it. Nothing is
-- relabelled; the run, its claim and its custody holds are untouched.
UPDATE runs SET recovery_wait_cause = NULL
WHERE recovery_wait_cause = 'retained_continuation_unwitnessed';
-- The extra observation keys are not representable by the prior check: drop them first, on a row
-- carrying any one of the four (not only presence).
UPDATE runs SET worker_recovery_evidence = worker_recovery_evidence - ARRAY['presence','registration_evidence_epoch','worker_snapshot_epoch','deadline']
WHERE worker_recovery_evidence ?| ARRAY['presence','registration_evidence_epoch','worker_snapshot_epoch','deadline'];
ALTER TABLE runs DROP CONSTRAINT runs_worker_recovery_evidence_check;
ALTER TABLE runs ADD CONSTRAINT runs_worker_recovery_evidence_check CHECK (
    worker_recovery_evidence IS NULL OR (
        jsonb_typeof(worker_recovery_evidence) = 'object'
        AND worker_recovery_evidence ?& ARRAY['checkpoint_tip','available_capture','publication_uncertain','capture_uncertain','custody_uncertain','unknown','recorded_at']
        AND worker_recovery_evidence - ARRAY['checkpoint_tip','available_capture','publication_uncertain','capture_uncertain','custody_uncertain','unknown','recorded_at'] = '{}'::jsonb
        AND (worker_recovery_evidence->'checkpoint_tip' = 'null'::jsonb OR
             (jsonb_typeof(worker_recovery_evidence->'checkpoint_tip') = 'string'
              AND worker_recovery_evidence->>'checkpoint_tip' ~ '^[0-9a-f]{40}$'))
        AND jsonb_typeof(worker_recovery_evidence->'available_capture') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'publication_uncertain') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'capture_uncertain') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'custody_uncertain') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'unknown') = 'boolean'
        AND jsonb_typeof(worker_recovery_evidence->'recorded_at') = 'string'
        AND length(worker_recovery_evidence->>'recorded_at') BETWEEN 20 AND 40
        AND pg_column_size(worker_recovery_evidence) <= 1024
    )
);
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
