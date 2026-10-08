-- +goose Up
ALTER TABLE cross_checks
 ADD COLUMN automatic_revision_limit integer NOT NULL DEFAULT 0 CHECK (automatic_revision_limit BETWEEN 0 AND 4),
 ADD COLUMN automatic_rounds_enabled boolean NOT NULL DEFAULT false,
 ADD COLUMN interrupted_at timestamptz,
 ADD COLUMN wait_credited boolean NOT NULL DEFAULT false;
-- Historical decided attempts have already settled their wait.
UPDATE cross_checks SET wait_credited = true WHERE verdict <> 'pending';
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_round_budget CHECK (
 (automatic_rounds_enabled AND round <= automatic_revision_limit + 1)
 OR (NOT automatic_rounds_enabled AND automatic_revision_limit = 0));
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_reason_class_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_reason_class_check CHECK (
 reason_class IS NULL OR (verdict IN ('approve','revise','block') AND reason_class = verdict)
 OR (verdict = 'failed' AND reason_class IN ('malformed','model_error','model_timeout',
 'checker_unavailable','confinement_failed','timed_out','superseded','approved_not_stored')));
ALTER TABLE runs DROP CONSTRAINT runs_plan_cross_check_gate_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_plan_cross_check_gate_reason_check CHECK (
 plan_cross_check_gate_reason IN ('revise','block','malformed','model_error','model_timeout',
 'checker_unavailable','confinement_failed','timed_out','superseded','codex_lead_unsupported',
 'planning_diff_refused','interrupted','candidate_refused','checker_failed','revisions_exhausted'));

-- The runs transition owns settlement at first custody loss. Decided evidence
-- retains its cause; only an approval with no durable plan loses authority.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION settle_exited_plan_cross_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
 settled cross_checks%ROWTYPE;
 was_pending boolean;
 interruption timestamptz := now();
BEGIN
 IF NEW.kind = 'cross_check' THEN RETURN NEW; END IF;
 IF OLD.status NOT IN ('claimed','running') OR OLD.claim_released_at IS NOT NULL
    OR NOT (NEW.status NOT IN ('claimed','running')
       OR NEW.claim_generation IS DISTINCT FROM OLD.claim_generation
       OR NEW.claim_released_at IS NOT NULL) THEN RETURN NEW; END IF;

 -- Frozen recovery writers stamp status_since with their supplied observation;
 -- direct invalidation without a new status clock uses this transaction's now().
 IF NEW.status_since IS DISTINCT FROM OLD.status_since THEN
   interruption := NEW.status_since;
 END IF;
 SELECT cc.verdict = 'pending' INTO was_pending FROM cross_checks cc
 WHERE cc.lead_run_id = NEW.id AND cc.stage = 'plan' ORDER BY cc.round DESC LIMIT 1;
 UPDATE cross_checks cc SET
   interrupted_at = COALESCE(cc.interrupted_at, interruption),
   verdict = CASE WHEN cc.verdict = 'pending' OR
       (cc.verdict = 'approve' AND cc.automatic_rounds_enabled AND NEW.plan_md IS NULL AND NEW.auto_approve AND NEW.gate_revision = 0)
       THEN 'failed' ELSE cc.verdict END,
   reason_class = CASE
       WHEN cc.verdict = 'pending' AND cc.deadline_at <= interruption THEN 'timed_out'
       WHEN cc.verdict = 'pending' THEN 'superseded'
       WHEN cc.verdict = 'approve' AND cc.automatic_rounds_enabled AND NEW.plan_md IS NULL AND NEW.auto_approve AND NEW.gate_revision = 0
         THEN 'approved_not_stored'
       ELSE cc.reason_class END,
   decided_at = CASE WHEN cc.verdict = 'pending' THEN LEAST(interruption, cc.deadline_at) ELSE cc.decided_at END
 WHERE cc.lead_run_id = NEW.id AND cc.stage = 'plan'
   AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = NEW.id AND latest.stage = 'plan')
   AND cc.interrupted_at IS NULL
 RETURNING cc.* INTO settled;
 IF NOT FOUND OR NOT was_pending OR settled.wait_credited OR settled.reason_class NOT IN ('superseded','timed_out') THEN
   RETURN NEW;
 END IF;
 -- Only the pending transition can produce an uncredited settled attempt.
 UPDATE cross_checks SET wait_credited = true WHERE id = settled.id AND NOT wait_credited;
 IF NOT FOUND THEN RETURN NEW; END IF;
 UPDATE runs child SET plan_cross_check_gate_reason = NULL, status = 'cancelled',
   finished_at = now(), updated_at = now(), claim_released_at = now()
 WHERE child.id = settled.checker_run_id AND child.status NOT IN ('completed','failed','cancelled');
 UPDATE runs lead SET budget_paused_seconds = lead.budget_paused_seconds
   + GREATEST(0, CEIL(EXTRACT(EPOCH FROM (settled.decided_at - settled.created_at)))::int)
 WHERE lead.id = NEW.id;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Budget snapshots cannot be changed after creation or widened by later workers.
-- +goose StatementBegin
CREATE FUNCTION enforce_plan_cross_check_round_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE first_check cross_checks%ROWTYPE;
BEGIN
 IF TG_OP = 'UPDATE' THEN
   IF NEW.automatic_rounds_enabled IS DISTINCT FROM OLD.automatic_rounds_enabled
      OR NEW.automatic_revision_limit IS DISTINCT FROM OLD.automatic_revision_limit
 THEN
     RAISE EXCEPTION 'immutable cross-check attempt';
   END IF;
 ELSE
   SELECT * INTO first_check FROM cross_checks
   WHERE lead_run_id = NEW.lead_run_id AND stage = NEW.stage ORDER BY round LIMIT 1;
   IF FOUND AND (NEW.automatic_revision_limit <> first_check.automatic_revision_limit
       OR NEW.automatic_rounds_enabled <> first_check.automatic_rounds_enabled) THEN
     RAISE EXCEPTION 'immutable cross-check budget';
   END IF;
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER cross_checks_round_snapshot BEFORE INSERT OR UPDATE ON cross_checks
FOR EACH ROW EXECUTE FUNCTION enforce_plan_cross_check_round_snapshot();

-- +goose Down
DROP TRIGGER cross_checks_round_snapshot ON cross_checks;
DROP FUNCTION enforce_plan_cross_check_round_snapshot();

-- Prior workers cannot reconcile later pending rounds. Retain their history as
-- failed, cancel their active children, and bank only previously uncredited waits.
-- +goose StatementBegin
WITH pending AS MATERIALIZED (
 SELECT id, wait_credited FROM cross_checks WHERE round > 1 AND verdict = 'pending' FOR UPDATE
), settled AS (
 UPDATE cross_checks cc SET verdict = 'failed', reason_class = 'superseded',
   decided_at = LEAST(now(), deadline_at), wait_credited = true
 FROM pending WHERE cc.id = pending.id
 RETURNING cc.lead_run_id, cc.checker_run_id, cc.created_at, cc.decided_at, pending.wait_credited AS was_credited
), cancelled AS (
 UPDATE runs SET status = 'cancelled', plan_cross_check_gate_reason = NULL,
   finished_at = now(), updated_at = now(), claim_released_at = now()
 WHERE id IN (SELECT checker_run_id FROM settled)
   AND status NOT IN ('completed','failed','cancelled')
)
UPDATE runs lead SET budget_paused_seconds = lead.budget_paused_seconds
 + GREATEST(0, CEIL(EXTRACT(EPOCH FROM (settled.decided_at - settled.created_at)))::int)
FROM settled WHERE lead.id = settled.lead_run_id AND NOT settled.was_credited;
-- +goose StatementEnd

UPDATE cross_checks SET reason_class = 'superseded'
 WHERE verdict = 'failed' AND reason_class = 'approved_not_stored';
UPDATE runs SET plan_cross_check_gate_reason = 'interrupted'
 WHERE plan_cross_check_gate_reason = 'revisions_exhausted';

ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_reason_class_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_reason_class_check CHECK (
 reason_class IS NULL OR (verdict IN ('approve','revise','block') AND reason_class = verdict)
 OR (verdict = 'failed' AND reason_class IN ('malformed','model_error','model_timeout',
 'checker_unavailable','confinement_failed','timed_out','superseded')));
ALTER TABLE runs DROP CONSTRAINT runs_plan_cross_check_gate_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_plan_cross_check_gate_reason_check CHECK (
 plan_cross_check_gate_reason IN ('revise','block','malformed','model_error','model_timeout',
 'checker_unavailable','confinement_failed','timed_out','superseded','codex_lead_unsupported',
 'planning_diff_refused','interrupted','candidate_refused','checker_failed'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION settle_exited_plan_cross_check() RETURNS trigger
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

ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_round_budget;
ALTER TABLE cross_checks DROP COLUMN automatic_revision_limit,
 DROP COLUMN automatic_rounds_enabled, DROP COLUMN interrupted_at, DROP COLUMN wait_credited;
