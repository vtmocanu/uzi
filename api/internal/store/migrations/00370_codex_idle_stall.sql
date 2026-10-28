-- +goose Up
ALTER TABLE runs ADD COLUMN idle_stall_park_count integer NOT NULL DEFAULT 0;
ALTER TABLE runs ADD CONSTRAINT runs_idle_stall_park_count_nonnegative CHECK (idle_stall_park_count >= 0) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted','worker_memory_pressure','retained_recovery_exhausted','codex_idle_stall'
    )
) NOT VALID;

-- +goose Down
UPDATE runs SET status='failed', fail_origin='worker_lost',
 failure_reason='codex idle stall hold ended by schema rollback (00370)',
 finished_at=now(), status_since=now(), updated_at=now(),
 recovery_wait_cause=NULL, recovery_retry_not_before=NULL
WHERE status='recovery_wait' AND recovery_wait_cause='codex_idle_stall';
UPDATE runs SET recovery_wait_cause=NULL, recovery_retry_not_before=NULL
WHERE recovery_wait_cause='codex_idle_stall';
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted','worker_memory_pressure','retained_recovery_exhausted'
    )
);
ALTER TABLE runs DROP COLUMN idle_stall_park_count;
