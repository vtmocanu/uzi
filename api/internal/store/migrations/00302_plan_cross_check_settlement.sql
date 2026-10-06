-- +goose Up
-- The updating statement already holds this lead. Bulk/checker writers must
-- acquire affected leads before children; this trigger is scoped to NEW.id.
-- +goose StatementBegin
CREATE FUNCTION settle_exited_plan_cross_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    settled cross_checks%ROWTYPE;
BEGIN
    -- A checker update must never follow target_run_id back to its parent.
    IF NEW.kind = 'cross_check' THEN
        RETURN NEW;
    END IF;

    -- Only a change invalidating previously live custody is an exit.
    -- claimed -> running and watched-column no-ops preserve the pending check.
    IF OLD.status NOT IN ('claimed', 'running')
       OR OLD.claim_released_at IS NOT NULL
       OR NOT (
           NEW.status NOT IN ('claimed', 'running')
           OR NEW.claim_generation IS DISTINCT FROM OLD.claim_generation
           OR NEW.claim_released_at IS NOT NULL
       ) THEN
        RETURN NEW;
    END IF;

    UPDATE cross_checks cc
    SET verdict = 'failed', reason_class = 'superseded',
        decided_at = LEAST(now(), cc.deadline_at)
    WHERE cc.lead_run_id = NEW.id
      AND cc.stage = 'plan' AND cc.round = 1 AND cc.verdict = 'pending'
    RETURNING cc.* INTO settled;

    -- The pending transition owns both effects, including on generation change.
    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    UPDATE runs child
    SET plan_cross_check_gate_reason = NULL, status = 'cancelled',
        finished_at = now(), updated_at = now(), claim_released_at = now()
    WHERE child.id = settled.checker_run_id
      AND child.status NOT IN ('completed', 'failed', 'cancelled');

    -- No watched column is assigned here, so banking cannot invoke this trigger.
    UPDATE runs lead
    SET budget_paused_seconds = lead.budget_paused_seconds
        + GREATEST(0, CEIL(EXTRACT(EPOCH FROM
            (settled.decided_at - settled.created_at)))::int)
    WHERE lead.id = NEW.id;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER runs_settle_exited_plan_cross_check
AFTER UPDATE OF status, claim_generation, claim_released_at ON runs
FOR EACH ROW EXECUTE FUNCTION settle_exited_plan_cross_check();

-- +goose Down
DROP TRIGGER runs_settle_exited_plan_cross_check ON runs;
DROP FUNCTION settle_exited_plan_cross_check();
