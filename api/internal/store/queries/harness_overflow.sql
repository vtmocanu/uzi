-- name: GetHarnessCreationPolicy :one
-- Capture preference and RAW checker flags in ONE READ COMMITTED statement.
SELECT harness_overflow_enabled, plan_cross_check_enabled, code_cross_check_enabled
FROM users WHERE id = @id;

-- name: SetRunHarnessDecision :execrows
-- Creation-only, write-once explanation. Chosen harness is runs.harness.
UPDATE runs SET
    harness_preferred = @preferred::text,
    harness_decision_reason = @reason::text,
    harness_preferred_score = sqlc.narg('preferred_score')::double precision,
    harness_other_score = sqlc.narg('other_score')::double precision,
    harness_preferred_measured = sqlc.narg('preferred_measured')::double precision,
    harness_other_measured = sqlc.narg('other_measured')::double precision,
    harness_preferred_held = @preferred_held::boolean,
    harness_other_held = @other_held::boolean
WHERE id = @id AND user_id = @user_id AND harness_decision_reason IS NULL;
