-- +goose Up
-- Preserve terminal evidence when worker cleanup only clears its foreign key.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION settle_exited_code_cross_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 settled cross_checks%ROWTYPE;
 interruption timestamptz := now();
BEGIN
 IF OLD.status IN ('completed','failed','cancelled')
    AND OLD.worker_id IS NOT NULL AND NEW.worker_id IS NULL
    AND (to_jsonb(NEW)-'worker_id') IS NOT DISTINCT FROM (to_jsonb(OLD)-'worker_id') THEN
  RETURN NEW;
 END IF;
 IF NEW.kind = 'cross_check' OR
    (NEW.status = 'completed' AND NEW.claim_generation = OLD.claim_generation
      AND NEW.worker_id IS NOT DISTINCT FROM OLD.worker_id
      AND NOT EXISTS (SELECT 1 FROM cross_checks cc WHERE cc.lead_run_id = NEW.id
        AND cc.stage = 'code' AND cc.outcome = 'pending'))
    OR NOT (NEW.worker_id IS DISTINCT FROM OLD.worker_id
       OR NEW.claim_generation IS DISTINCT FROM OLD.claim_generation
       OR ((OLD.status IN ('claimed','running') OR (OLD.status = 'awaiting_followup' AND OLD.kind = 'task' AND OLD.interactive)) AND OLD.claim_released_at IS NULL
         AND (NEW.claim_released_at IS NOT NULL OR NOT (NEW.status IN ('claimed','running') OR (NEW.status = 'awaiting_followup' AND NEW.kind = 'task' AND NEW.interactive))))) THEN
  RETURN NEW;
 END IF;
 IF NEW.status_since IS DISTINCT FROM OLD.status_since THEN interruption := NEW.status_since; END IF;
 UPDATE cross_checks cc SET interrupted_at = interruption,
  outcome = 'failed',
  reason_class = CASE WHEN cc.head_commit IS NULL THEN cc.reason_class
    WHEN cc.outcome = 'pending' AND cc.deadline_at <= interruption THEN 'timed_out'
    ELSE 'superseded' END,
  decided_at = CASE WHEN cc.outcome = 'pending' THEN LEAST(interruption, cc.deadline_at) ELSE cc.decided_at END
 WHERE cc.lead_run_id = NEW.id AND cc.stage = 'code' AND cc.interrupted_at IS NULL
 RETURNING cc.* INTO settled;
 IF NOT FOUND THEN RETURN NEW; END IF;
 UPDATE runs child SET status = 'cancelled', finished_at = now(), updated_at = now(),
  claim_released_at = now() WHERE child.id = settled.checker_run_id
  AND child.status NOT IN ('completed','failed','cancelled');
 UPDATE cross_checks SET wait_credited = true WHERE id = settled.id AND NOT wait_credited;
 IF FOUND THEN
  UPDATE runs lead SET budget_paused_seconds = lead.budget_paused_seconds +
   GREATEST(0, CEIL(EXTRACT(EPOCH FROM (settled.decided_at - settled.created_at)))::int)
  WHERE lead.id = NEW.id;
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- Restore the interactive custody function that preceded this migration.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION settle_exited_code_cross_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 settled cross_checks%ROWTYPE;
 interruption timestamptz := now();
BEGIN
 IF NEW.kind = 'cross_check' OR
    (NEW.status = 'completed' AND NEW.claim_generation = OLD.claim_generation
      AND NEW.worker_id IS NOT DISTINCT FROM OLD.worker_id
      AND NOT EXISTS (SELECT 1 FROM cross_checks cc WHERE cc.lead_run_id = NEW.id
        AND cc.stage = 'code' AND cc.outcome = 'pending'))
    OR NOT (NEW.worker_id IS DISTINCT FROM OLD.worker_id
       OR NEW.claim_generation IS DISTINCT FROM OLD.claim_generation
       OR ((OLD.status IN ('claimed','running') OR (OLD.status = 'awaiting_followup' AND OLD.kind = 'task' AND OLD.interactive)) AND OLD.claim_released_at IS NULL
         AND (NEW.claim_released_at IS NOT NULL OR NOT (NEW.status IN ('claimed','running') OR (NEW.status = 'awaiting_followup' AND NEW.kind = 'task' AND NEW.interactive))))) THEN
  RETURN NEW;
 END IF;
 IF NEW.status_since IS DISTINCT FROM OLD.status_since THEN interruption := NEW.status_since; END IF;
 UPDATE cross_checks cc SET interrupted_at = interruption,
  outcome = 'failed',
  reason_class = CASE WHEN cc.head_commit IS NULL THEN cc.reason_class
    WHEN cc.outcome = 'pending' AND cc.deadline_at <= interruption THEN 'timed_out'
    ELSE 'superseded' END,
  decided_at = CASE WHEN cc.outcome = 'pending' THEN LEAST(interruption, cc.deadline_at) ELSE cc.decided_at END
 WHERE cc.lead_run_id = NEW.id AND cc.stage = 'code' AND cc.interrupted_at IS NULL
 RETURNING cc.* INTO settled;
 IF NOT FOUND THEN RETURN NEW; END IF;
 UPDATE runs child SET status = 'cancelled', finished_at = now(), updated_at = now(),
  claim_released_at = now() WHERE child.id = settled.checker_run_id
  AND child.status NOT IN ('completed','failed','cancelled');
 UPDATE cross_checks SET wait_credited = true WHERE id = settled.id AND NOT wait_credited;
 IF FOUND THEN
  UPDATE runs lead SET budget_paused_seconds = lead.budget_paused_seconds +
   GREATEST(0, CEIL(EXTRACT(EPOCH FROM (settled.decided_at - settled.created_at)))::int)
  WHERE lead.id = NEW.id;
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
