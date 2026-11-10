-- +goose Up
-- First statement: the constraint swap below waits on runs locks, so the bound must cover it.
-- A lock wait past it fails the migration, which can be rerun.
SET LOCAL lock_timeout = '5s';
-- Issue #3767: new owner-held recovery_wait cause 'anthropic_spend_limit', reported by the
-- worker when the Anthropic API refuses for a spend limit. It is always parked with a NULL
-- recovery_retry_not_before (never sweeper-promoted); only the owner's Resume, set-token or
-- Cancel moves it. The widened CHECK is installed NOT VALID so no runs scan happens under
-- ACCESS EXCLUSIVE; 00425 validates it while ordinary reads and writes continue.
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted','worker_memory_pressure','retained_recovery_exhausted','codex_idle_stall','retained_recovery_adoption_failed','retained_continuation_unwitnessed','anthropic_spend_limit'
    )
) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Serialise against live run writers while the Down rewrites rows and narrows the CHECK; a held
-- lock fails the rollback after 5s rather than queueing every runs writer behind it.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- A live spend-limit hold has no pre-00424 equivalent: relabelling it as another cause would
-- fabricate a classification the run never earned. Refuse the rollback while any run is still
-- parked there (checked under the lock above) so the owner decides first.
-- +goose StatementBegin
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM runs
    WHERE status = 'recovery_wait' AND recovery_wait_cause = 'anthropic_spend_limit';
    IF n > 0 THEN
        RAISE EXCEPTION 'refusing to roll back 00424: % run(s) are parked in recovery_wait at anthropic_spend_limit; Resume, set-token or Cancel those runs first so no parked run is relabelled', n;
    END IF;
END
$$;
-- +goose StatementEnd
-- Any other row still carrying the cause (set-token leaves it stale on a queued or running row,
-- and an owner Cancel or a later terminal write leaves it set; no CHECK ties the cause to a
-- status) holds a value the prior CHECK rejects and that no longer describes a live wait: clear
-- it. Nothing is relabelled; the run, its claim and its custody holds are untouched.
UPDATE runs SET recovery_wait_cause = NULL
WHERE recovery_wait_cause = 'anthropic_spend_limit';
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full','worker_requeue_exhausted','worker_memory_pressure','retained_recovery_exhausted','codex_idle_stall','retained_recovery_adoption_failed','retained_continuation_unwitnessed'
    )
);
