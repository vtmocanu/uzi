-- +goose Up

-- Validate the three CHECKs added NOT VALID in 00233 (PRD #1247 M1). VALIDATE CONSTRAINT
-- scans each table to confirm existing rows satisfy the CHECK but takes only a SHARE
-- UPDATE EXCLUSIVE lock (write-compatible), unlike the ACCESS EXCLUSIVE lock an inline
-- validated ADD CONSTRAINT ... CHECK would hold. Split from 00233 so the adds are
-- lock-cheap and the validation non-blocking, per the two-step pattern (00226/00227).
--
-- The backlog satisfies every CHECK by construction: the two override-mode columns took
-- NULL on every existing row (the inherit default), and runs.anthropic_select_reason
-- only WIDENED — every value 00089 admitted is still admitted — so no existing reason
-- can violate the re-added constraint.
ALTER TABLE runs VALIDATE CONSTRAINT runs_credential_override_mode_check;
ALTER TABLE run_schedules VALIDATE CONSTRAINT run_schedules_credential_override_mode_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_anthropic_select_reason_check;

-- +goose Down

-- There is no VALIDATE inverse (a validated CHECK simply stays validated), so restore the
-- pre-00234 state — each constraint present but NOT VALID — by dropping and re-adding it
-- NOT VALID. The CHECK bodies match 00233's Up verbatim; 00233's Down then drops them.
-- Mirrors 00227's Down exactly.
ALTER TABLE runs DROP CONSTRAINT runs_credential_override_mode_check;
ALTER TABLE runs ADD CONSTRAINT runs_credential_override_mode_check
    CHECK (credential_override_mode IS NULL OR credential_override_mode IN ('pinned', 'auto', 'default')) NOT VALID;
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_credential_override_mode_check;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_credential_override_mode_check
    CHECK (credential_override_mode IS NULL OR credential_override_mode IN ('pinned', 'auto', 'default')) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_anthropic_select_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_anthropic_select_reason_check
    CHECK (anthropic_select_reason IS NULL OR anthropic_select_reason IN (
        'default', 'pinned', 'judge',
        'auto', 'best_of_pool', 'pool_empty', 'pool_stale', 'open_failed',
        'run_pinned', 'run_default'
    )) NOT VALID;
