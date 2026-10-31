-- +goose Up

-- Validate the three CHECKs added NOT VALID in 00387 (issue #3719). VALIDATE CONSTRAINT
-- scans each table but takes only a SHARE UPDATE EXCLUSIVE lock (write-compatible), unlike
-- the ACCESS EXCLUSIVE lock an inline validated ADD CONSTRAINT would hold. Split from 00387
-- per the two-step pattern (00233/00234).
--
-- The backlog satisfies every CHECK by construction: users took the 'default' column
-- default, runs.codex_select_reason is NULL on every existing row, and the auto_eligible
-- CHECK only WIDENED.
ALTER TABLE users VALIDATE CONSTRAINT users_codex_account_mode_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_codex_select_reason_check;
ALTER TABLE user_secrets VALIDATE CONSTRAINT user_secrets_auto_eligible_kind_check;

-- +goose Down

-- There is no VALIDATE inverse, so restore the pre-00388 state (each constraint present
-- but NOT VALID) by dropping and re-adding it NOT VALID. The bodies match 00387's Up
-- verbatim; 00387's Down then drops or narrows them. Mirrors 00234's Down.
ALTER TABLE users DROP CONSTRAINT users_codex_account_mode_check;
ALTER TABLE users ADD CONSTRAINT users_codex_account_mode_check
    CHECK (codex_account_mode IN ('default', 'auto')) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_codex_select_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_codex_select_reason_check
    CHECK (codex_select_reason IS NULL OR (
        harness = 'codex' AND codex_select_reason IN (
            'default', 'pool_empty', 'auto', 'best_of_pool', 'pool_stale', 'pool_exhausted'
        )
    )) NOT VALID;
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_auto_eligible_kind_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_auto_eligible_kind_check
    CHECK (NOT auto_eligible OR kind IN ('anthropic_token', 'codex_auth')) NOT VALID;
