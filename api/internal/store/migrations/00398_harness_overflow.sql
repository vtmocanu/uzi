-- +goose Up
ALTER TABLE users ADD COLUMN harness_overflow_enabled boolean NOT NULL DEFAULT false;

-- A NULL preferred harness/reason means no creation decision (including historical
-- rows and direct chat inserts). Preferred holds compare as 0; ineligible Codex
-- destination comparisons are NULL. Measured scores retain the gauge independently.
ALTER TABLE runs
    ADD COLUMN harness_preferred text,
    ADD COLUMN harness_decision_reason text,
    ADD COLUMN harness_preferred_score double precision,
    ADD COLUMN harness_other_score double precision,
    ADD COLUMN harness_preferred_measured double precision,
    ADD COLUMN harness_other_measured double precision,
    ADD COLUMN harness_preferred_held boolean,
    ADD COLUMN harness_other_held boolean,
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
    ),
    ADD CONSTRAINT runs_harness_decision_scores CHECK (
        (harness_preferred_score IS NULL OR harness_preferred_score BETWEEN 0 AND 100)
        AND (harness_other_score IS NULL OR harness_other_score BETWEEN 0 AND 100)
        AND (harness_preferred_measured IS NULL OR harness_preferred_measured BETWEEN 0 AND 100)
        AND (harness_other_measured IS NULL OR harness_other_measured BETWEEN 0 AND 100)
    );

-- +goose Down
ALTER TABLE runs
    DROP CONSTRAINT runs_harness_decision_scores,
    DROP CONSTRAINT runs_harness_decision_shape,
    DROP COLUMN harness_preferred,
    DROP COLUMN harness_decision_reason,
    DROP COLUMN harness_preferred_score,
    DROP COLUMN harness_other_score,
    DROP COLUMN harness_preferred_measured,
    DROP COLUMN harness_other_measured,
    DROP COLUMN harness_preferred_held,
    DROP COLUMN harness_other_held;
ALTER TABLE users DROP COLUMN harness_overflow_enabled;
