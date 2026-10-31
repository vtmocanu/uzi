-- +goose Up
-- Validate the two CHECKs added NOT VALID in 00387 (PRD #3708 M1): the exclusive-mode CHECK
-- runs_plan_cross_check_mode_check and the widened runs_plan_cross_check_gate_reason_check.
-- VALIDATE CONSTRAINT takes only a SHARE UPDATE EXCLUSIVE lock, so reads and writes proceed
-- during the scan (00368 pattern). Every existing row satisfies both by construction: the
-- column is new and false everywhere, and the reason list only grew.
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs VALIDATE CONSTRAINT runs_plan_cross_check_mode_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_plan_cross_check_gate_reason_check;

-- +goose Down
-- goose commits each Down in its own transaction, so the same fail-closed guard as 00387's
-- Down (and scripts/gated-cross-check-drain-check.sql) runs first: nothing commits before a
-- refusal.
SET LOCAL lock_timeout = '5s';
LOCK TABLE runs IN SHARE ROW EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM runs
    WHERE plan_cross_check_gated AND status NOT IN ('completed', 'failed', 'cancelled');
    IF n > 0 THEN
        RAISE EXCEPTION 'uzi: % non-terminal gated plan cross-check runs exist; abort the rollback', n;
    END IF;
END
$$;
-- +goose StatementEnd

-- There is no VALIDATE inverse, so restore the post-00387 state (both CHECKs present but NOT
-- VALID) by dropping and re-adding them. The bodies match 00387 verbatim. Mirrors 00368's Down.
ALTER TABLE runs DROP CONSTRAINT runs_plan_cross_check_mode_check;
ALTER TABLE runs ADD CONSTRAINT runs_plan_cross_check_mode_check
    CHECK (NOT plan_cross_check_gated OR (
        NOT plan_cross_check_required
        AND NOT auto_approve
        AND kind IN ('issue', 'prompt', 'ci_fix')
        AND egress_profile_id IS NULL)) NOT VALID;
ALTER TABLE runs DROP CONSTRAINT runs_plan_cross_check_gate_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_plan_cross_check_gate_reason_check CHECK (
    plan_cross_check_gate_reason IN ('revise','block','malformed','model_error','model_timeout',
    'checker_unavailable','confinement_failed','timed_out','superseded','codex_lead_unsupported',
    'planning_diff_refused','interrupted','candidate_refused','checker_failed','revisions_exhausted',
    'worker_unsupported')) NOT VALID;
