-- +goose Up
-- PRD #3708 M1: gated Plan cross-check. A run that is NOT auto-approved and whose owner has
-- Plan cross-check enabled snapshots one more mode at INSERT, plan_cross_check_gated. The
-- checker then runs before the first human plan gate, but every checker outcome (APPROVE
-- included) still presents the ordinary human gate; approval authority stays with the bound
-- human approval. The three modes (required, gated, neither) are mutually exclusive.
--
-- Additive: one NOT NULL column with a constant default (metadata-only, no table rewrite).
-- The CHECKs are added NOT VALID and validated in 00388, the two-step pattern of 00242/00243
-- and 00366/00367/00368, so no validating scan runs under this migration's ACCESS EXCLUSIVE
-- lock. The statements time out rather than queue behind a long transaction.
--
-- The migration number is a draft: it is renumbered above the live head at landing, the pair
-- (00387, 00388) together.
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs
    ADD COLUMN plan_cross_check_gated boolean NOT NULL DEFAULT false;

-- Exclusive modes. kind and egress_profile_id never change after INSERT, and auto_approve is
-- only ever cleared afterwards, so the INSERT-time snapshot stays true for the row's life.
ALTER TABLE runs ADD CONSTRAINT runs_plan_cross_check_mode_check
    CHECK (NOT plan_cross_check_gated OR (
        NOT plan_cross_check_required
        AND NOT auto_approve
        AND kind IN ('issue', 'prompt', 'ci_fix')
        AND egress_profile_id IS NULL)) NOT VALID;

-- The 00312 reason list plus 'worker_unsupported': the server (never a worker) records it
-- when a gated run's worker does not advertise the gated capability.
ALTER TABLE runs DROP CONSTRAINT runs_plan_cross_check_gate_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_plan_cross_check_gate_reason_check CHECK (
    plan_cross_check_gate_reason IN ('revise','block','malformed','model_error','model_timeout',
    'checker_unavailable','confinement_failed','timed_out','superseded','codex_lead_unsupported',
    'planning_diff_refused','interrupted','candidate_refused','checker_failed','revisions_exhausted',
    'worker_unsupported')) NOT VALID;

-- Fail-closed backstop that works under any api binary: leaving a gated awaiting_approval
-- gate for running needs an applied, bound, non-superseded approve_plan input for the
-- current gate revision and a stored plan equal to the presented one. This is a strict
-- subset of the SetRunRunning bound arm and only guards that one edge.
-- +goose StatementBegin
CREATE FUNCTION enforce_gated_plan_approval() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.plan_md IS NOT NULL
       AND OLD.gate_presented_payload->>'plan_md' = OLD.plan_md
       AND EXISTS (
           SELECT 1 FROM run_user_inputs i
           WHERE i.run_id = OLD.id AND i.kind = 'approve_plan'
             AND i.applied_at IS NOT NULL
             AND i.disposition IS DISTINCT FROM 'superseded'
             AND i.gate_binding = 'bound'
             AND i.gate_revision = OLD.gate_revision) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'uzi: gated plan cross-check run % cannot start implementing without a current bound approval', OLD.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'runs_gated_plan_approval';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER enforce_gated_plan_approval BEFORE UPDATE OF status ON runs
FOR EACH ROW
WHEN (OLD.plan_cross_check_gated AND OLD.status = 'awaiting_approval' AND NEW.status = 'running')
EXECUTE FUNCTION enforce_gated_plan_approval();

-- +goose Down
-- Fail-closed: refuse while any gated run is not terminal, so no active run is silently
-- downgraded to the any-prior-approve arm. This guard is identical to
-- scripts/gated-cross-check-drain-check.sql and to the guard in 00388's Down.
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

-- Only terminal runs can carry 'worker_unsupported'; map it to the nearest older reason.
UPDATE runs SET plan_cross_check_gate_reason = 'checker_unavailable'
WHERE plan_cross_check_gate_reason = 'worker_unsupported';

ALTER TABLE runs DROP CONSTRAINT runs_plan_cross_check_gate_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_plan_cross_check_gate_reason_check CHECK (
    plan_cross_check_gate_reason IN ('revise','block','malformed','model_error','model_timeout',
    'checker_unavailable','confinement_failed','timed_out','superseded','codex_lead_unsupported',
    'planning_diff_refused','interrupted','candidate_refused','checker_failed','revisions_exhausted')) NOT VALID;

-- The trigger's WHEN clause depends on the column, so the trigger goes first.
DROP TRIGGER enforce_gated_plan_approval ON runs;
DROP FUNCTION enforce_gated_plan_approval();
ALTER TABLE runs DROP CONSTRAINT runs_plan_cross_check_mode_check;
ALTER TABLE runs DROP COLUMN plan_cross_check_gated;
