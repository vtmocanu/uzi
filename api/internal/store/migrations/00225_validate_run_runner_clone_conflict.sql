-- +goose Up

-- Validate the CHECK added NOT VALID in 00224 (issue #1308): the widened
-- runs_fail_origin_check (thirteenth value 'runner_clone_conflict'). VALIDATE CONSTRAINT
-- scans the table to confirm existing rows satisfy the CHECK, but takes only a SHARE UPDATE
-- EXCLUSIVE lock (write-compatible: concurrent reads and writes proceed), unlike the ACCESS
-- EXCLUSIVE lock an inline validated ADD CONSTRAINT ... CHECK would hold. Split from 00224
-- so the add is lock-cheap and the validation is non-blocking, per the standard two-step
-- pattern (00219/00220) for a CHECK on a live table.
ALTER TABLE runs VALIDATE CONSTRAINT runs_fail_origin_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00225 state — the constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK body matches 00224's Up verbatim (incl. 'runner_clone_conflict').
-- 00224's Down then narrows it as before. Mirrors 00220's Down exactly.
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
        'runner_clone_conflict'
    )) NOT VALID;
