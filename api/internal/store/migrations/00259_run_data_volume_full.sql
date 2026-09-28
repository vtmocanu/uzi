-- +goose Up

-- PRD #1809 M5 (D6): a full data volume on the worker is a recoverable, bounded condition.
-- A worker whose write to the data volume failed with disk-full (after its reclaim and one
-- retry) parks the run in 'recovery_wait' with a new typed cause, 'data_volume_full', and the
-- park counts toward a disk-only lifetime cap. This migration is the schema half:
--
--   1. runs_recovery_wait_cause_check widened with 'data_volume_full' — the five values carried
--      verbatim from 00257's Up plus the new cause. It is WORKER-REPORTABLE
--      (workersvc/forgepark.go recoveryWaitCauses); SetState routes it to its own park
--      transaction (parkDataVolumeFull), which stores the cause.
--   2. runs.disk_park_count — the DISK-ONLY lifetime park counter the cap decides on
--      (UZI_RUN_DISK_PARK_MAX), the 00232 forge_park_count precedent. Distinct from
--      recovery_wait_count, which only shapes the backoff for every cause and has no cap, and
--      from forge_park_count, which belongs to the forge park alone. A PREVENTIVE disk park (the
--      worker stopped the run before the volume filled, disk_park_preventive=true) does not bump
--      it. NOT NULL DEFAULT 0, so every existing row is 0 (never disk-parked).
--   3. runs_fail_origin_check widened with an EIGHTEENTH value, 'data_volume_full' — the
--      SERVER-DERIVED terminal origin a run gets when a counted disk park would exceed the cap.
--      It is NOT worker-reportable (workersvc/failorigin.go workerReportableFailOrigins): the
--      server stamps it inside the park transaction. The seventeen values are carried
--      verbatim from 00260_gate_revision.sql's Up (PRD #1795 added the seventeenth,
--      'gate_presentation_refused'). TestFailOriginVocabularyMatchesCheck parses THIS CHECK and
--      asserts it equals AllFailOrigins().
--   4. runs.checkpoint_contains_latest — reserved for PRD #1809 M6 (D8): whether the checkpoint
--      a park published contains the run's latest committed work. DISPLAY-ONLY: nothing
--      decides on it (custody is released by its own evidence rules, not by this flag). NULL is
--      "not reported" (every existing row, every legacy worker, and every park until M6 wires
--      the report); nothing in this migration's companion code writes it.
--
-- Both CHECKs are immediate DROP+ADD (no NOT VALID), the 00186 template: the CHECK validates
-- against the existing rows on ADD, which is cheap for these small domain columns.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check
    CHECK (recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable', 'empty_turn', 'provider_outage',
        'codex_account_unavailable', 'vault_locked', 'data_volume_full'
    ));

ALTER TABLE runs ADD COLUMN disk_park_count int NOT NULL DEFAULT 0;

ALTER TABLE runs DROP CONSTRAINT runs_fail_origin_check;
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN (
        'provisioning_failed',
        'credential_unavailable',
        'guardrail_blocked',
        'rate_limited',
        'run_timeout',
        'worker_lost',
        'agent_failure',
        'plan_rejected',
        'auto_stopped',
        'workflow_scope_missing',
        'finalize_base_align_conflict',
        'push_secret_blocked',
        'forge_unreachable',
        'history_rewritten',
        'task_undispatched',
        'plan_missing',
        'gate_presentation_refused',
        'data_volume_full'
    ));

ALTER TABLE runs ADD COLUMN checkpoint_contains_latest boolean;

-- +goose Down

-- Reverse in the opposite order. Rows carrying a now-forbidden value are cleared first (NULLed,
-- never deleted: a down-migration undoing this FEATURE must not destroy whole run records; the
-- human-readable failure_reason on a failed run is untouched), as 00250's and 00257's Downs do.
ALTER TABLE runs DROP COLUMN checkpoint_contains_latest;

UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'data_volume_full';
ALTER TABLE runs DROP CONSTRAINT runs_fail_origin_check;
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN (
        'provisioning_failed',
        'credential_unavailable',
        'guardrail_blocked',
        'rate_limited',
        'run_timeout',
        'worker_lost',
        'agent_failure',
        'plan_rejected',
        'auto_stopped',
        'workflow_scope_missing',
        'finalize_base_align_conflict',
        'push_secret_blocked',
        'forge_unreachable',
        'history_rewritten',
        'task_undispatched',
        'plan_missing',
        'gate_presentation_refused'
    ));

ALTER TABLE runs DROP COLUMN disk_park_count;

UPDATE runs SET recovery_wait_cause = NULL
WHERE recovery_wait_cause = 'data_volume_full';
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check
    CHECK (recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable', 'empty_turn', 'provider_outage',
        'codex_account_unavailable', 'vault_locked'
    ));
