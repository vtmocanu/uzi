-- +goose Up
ALTER TABLE runs
    ADD COLUMN worker_recovery_episode bigint NOT NULL DEFAULT 0 CHECK (worker_recovery_episode >= 0),
    ADD COLUMN requeue_episode_baseline integer NOT NULL DEFAULT 0 CHECK (requeue_episode_baseline >= 0 AND requeue_episode_baseline <= requeue_count),
    ADD COLUMN worker_recovery_evidence jsonb;
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
        'vault_locked','data_volume_full','worker_requeue_exhausted'
    )
);
-- +goose Down
-- A deliberate schema downgrade removes owner exhaustion Resume. Fall back to
-- the prior application's terminal worker_lost behavior, preserving recovery
-- sources, custody and released-claim fences rather than leaving an untimed wait.
UPDATE runs
SET status = 'failed',
    fail_origin = 'worker_lost',
    failure_reason = 'worker recovery exhaustion hold ended by schema rollback (00309)',
    finished_at = now(), status_since = now(), updated_at = now(),
    recovery_wait_cause = NULL, recovery_retry_not_before = NULL
WHERE status = 'recovery_wait' AND recovery_wait_cause = 'worker_requeue_exhausted';
-- Outside the hold, only obsolete cause/retry metadata needs clearing.
UPDATE runs SET recovery_wait_cause = NULL, recovery_retry_not_before = NULL
WHERE recovery_wait_cause = 'worker_requeue_exhausted';
ALTER TABLE runs DROP COLUMN worker_recovery_evidence,
    DROP COLUMN requeue_episode_baseline, DROP COLUMN worker_recovery_episode;
ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check CHECK (
    recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable','empty_turn','provider_outage','codex_account_unavailable',
        'vault_locked','data_volume_full'
    )
);
