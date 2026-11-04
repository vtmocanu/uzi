-- +goose Up

-- Issue #3845: "Default · automatic" harness choice by soonest weekly reset. Automatic
-- decisions record harness_preferred = 'claude' (the usage-free resolver answer) and map the
-- preferred_* columns to Claude and other_* to Codex; the two new columns carry the compared
-- accounts' weekly resets (NULL when unknown). Only auto_* decisions may carry a reset, and an
-- auto_* decision must record harness_preferred = 'claude' so the column mapping holds.
--
-- The CHECK is replaced NOT VALID so the add is lock-cheap; 00403 validates it under a
-- SHARE UPDATE EXCLUSIVE lock (the 00224/00225 two-step). Every existing row satisfies the new
-- body by construction: the reason set only grows and the new columns are NULL.
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs
    ADD COLUMN harness_preferred_weekly_reset_at timestamptz,
    ADD COLUMN harness_other_weekly_reset_at timestamptz,
    DROP CONSTRAINT runs_harness_decision_shape,
    ADD CONSTRAINT runs_harness_decision_shape CHECK (
        ((harness_preferred IS NULL AND harness_decision_reason IS NULL
         AND harness_preferred_score IS NULL AND harness_other_score IS NULL
         AND harness_preferred_measured IS NULL AND harness_other_measured IS NULL
         AND harness_preferred_held IS NULL AND harness_other_held IS NULL
         AND harness_preferred_weekly_reset_at IS NULL AND harness_other_weekly_reset_at IS NULL)
        OR
        (harness_preferred IS NOT NULL AND harness_preferred IN ('claude', 'codex')
         AND harness_decision_reason IS NOT NULL
         AND harness_decision_reason IN ('off', 'preferred', 'overflow', 'unknown_usage', 'excluded',
                                         'auto_soonest_reset', 'auto_only_qualified', 'auto_fallback')
         AND harness_preferred_held IS NOT NULL AND harness_other_held IS NOT NULL))
        AND
        (harness_decision_reason IN ('auto_soonest_reset', 'auto_only_qualified', 'auto_fallback')
         OR (harness_preferred_weekly_reset_at IS NULL AND harness_other_weekly_reset_at IS NULL))
        AND
        (harness_decision_reason NOT IN ('auto_soonest_reset', 'auto_only_qualified', 'auto_fallback')
         OR harness_preferred = 'claude')
    ) NOT VALID;

-- +goose Down

-- Best-effort and DATA-DEPENDENT, as 00224's Down: the re-added 00398 CHECK is validated, so it
-- FAILS once any row holds an auto_* reason, and dropping the two columns loses their data.
-- Goose downs are not run in this deployment (store.Migrate only ever goes up); drain first if
-- you must. Goose runs 00403's Down first.
ALTER TABLE runs
    DROP CONSTRAINT runs_harness_decision_shape,
    DROP COLUMN harness_preferred_weekly_reset_at,
    DROP COLUMN harness_other_weekly_reset_at,
    ADD CONSTRAINT runs_harness_decision_shape CHECK (
        (harness_preferred IS NULL AND harness_decision_reason IS NULL
         AND harness_preferred_score IS NULL AND harness_other_score IS NULL
         AND harness_preferred_measured IS NULL AND harness_other_measured IS NULL
         AND harness_preferred_held IS NULL AND harness_other_held IS NULL)
        OR
        (harness_preferred IS NOT NULL AND harness_preferred IN ('claude', 'codex')
         AND harness_decision_reason IS NOT NULL
         AND harness_decision_reason IN ('off', 'preferred', 'overflow', 'unknown_usage', 'excluded')
         AND harness_preferred_held IS NOT NULL AND harness_other_held IS NOT NULL)
    );
