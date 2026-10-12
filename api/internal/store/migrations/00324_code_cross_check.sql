-- +goose Up
ALTER TABLE users ADD COLUMN code_cross_check_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE runs ADD COLUMN code_cross_check_required boolean NOT NULL DEFAULT false;
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_stage_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_stage_check CHECK (stage IN ('plan', 'code'));
ALTER TABLE cross_checks
 ADD COLUMN head_commit text,
 ADD COLUMN outcome text,
 ADD COLUMN code_context jsonb,
 ADD COLUMN guidance_snapshot text,
 ADD COLUMN guidance_text text,
 ADD COLUMN guidance_digest bytea,
 ADD COLUMN repo_instructions_enabled boolean NOT NULL DEFAULT false,
 ADD COLUMN repo_instructions_text text,
 ADD COLUMN repo_instructions_digest bytea,
 ADD COLUMN dispositions jsonb,
 ADD COLUMN finalized_at timestamptz;

-- Code evidence has an independent outcome; the legacy verdict is inert.
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_code_shape CHECK (
 (stage = 'plan' AND outcome IS NULL AND head_commit IS NULL
     AND code_context IS NULL AND guidance_snapshot IS NULL
     AND guidance_text IS NULL AND guidance_digest IS NULL
     AND NOT repo_instructions_enabled AND repo_instructions_text IS NULL
     AND repo_instructions_digest IS NULL AND dispositions IS NULL AND finalized_at IS NULL)
 OR (stage = 'code' AND round = 1 AND verdict = 'failed'
     AND outcome IS NOT NULL AND outcome IN ('pending','completed','failed')
     AND code_context IS NOT NULL AND guidance_snapshot IS NOT NULL
     AND guidance_text IS NOT NULL AND guidance_digest IS NOT NULL
     AND repo_instructions_text IS NOT NULL AND repo_instructions_digest IS NOT NULL
     AND guidance_digest = sha256(convert_to(guidance_text, 'UTF8'))
     AND repo_instructions_digest = sha256(convert_to(repo_instructions_text, 'UTF8'))
     AND (repo_instructions_enabled OR repo_instructions_text = '')
     AND NOT automatic_rounds_enabled AND automatic_revision_limit = 0
     AND (
       (head_commit IS NOT NULL AND base_commit IS NOT NULL
        AND head_commit ~ '^[0-9a-f]{40}$' AND base_commit ~ '^[0-9a-f]{40}$'
        AND candidate_digest IS NOT NULL)
       OR (outcome = 'failed' AND reason_class IS NOT NULL
           AND reason_class IN ('worker_unsupported','snapshot_failed')
           AND head_commit IS NULL AND base_commit IS NULL)
     )
 ));
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_reason_class_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_reason_class_check CHECK (
 (stage = 'plan' AND (reason_class IS NULL OR
   (verdict IN ('approve','revise','block') AND reason_class = verdict)
   OR (verdict = 'failed' AND reason_class IN ('malformed','model_error','model_timeout',
     'checker_unavailable','confinement_failed','timed_out','superseded','approved_not_stored'))))
 OR (stage = 'code' AND (
   (outcome IN ('pending','completed') AND reason_class IS NULL)
   OR (outcome = 'failed' AND reason_class IS NOT NULL AND reason_class IN (
     'worker_unsupported','snapshot_failed','malformed','model_error','model_timeout',
     'checker_unavailable','confinement_failed','timed_out','superseded'))))
);
CREATE UNIQUE INDEX cross_checks_one_code_attempt ON cross_checks (lead_run_id) WHERE stage = 'code';
CREATE INDEX cross_checks_code_pending_deadline ON cross_checks (deadline_at)
 WHERE stage = 'code' AND outcome = 'pending';

-- +goose StatementBegin
CREATE FUNCTION enforce_code_cross_check_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.stage = 'code' OR NEW.stage = 'code' THEN
  IF NEW.stage IS DISTINCT FROM OLD.stage
    OR NEW.lead_run_id IS DISTINCT FROM OLD.lead_run_id
    OR NEW.lead_claim_generation IS DISTINCT FROM OLD.lead_claim_generation
    OR NEW.head_commit IS DISTINCT FROM OLD.head_commit
    OR NEW.base_commit IS DISTINCT FROM OLD.base_commit
    OR NEW.candidate_digest IS DISTINCT FROM OLD.candidate_digest
    OR NEW.code_context IS DISTINCT FROM OLD.code_context
    OR NEW.guidance_snapshot IS DISTINCT FROM OLD.guidance_snapshot
    OR NEW.guidance_text IS DISTINCT FROM OLD.guidance_text
    OR NEW.guidance_digest IS DISTINCT FROM OLD.guidance_digest
    OR NEW.repo_instructions_enabled IS DISTINCT FROM OLD.repo_instructions_enabled
    OR NEW.repo_instructions_text IS DISTINCT FROM OLD.repo_instructions_text
    OR NEW.repo_instructions_digest IS DISTINCT FROM OLD.repo_instructions_digest
    OR NEW.plan_md IS DISTINCT FROM OLD.plan_md
    OR NEW.milestones IS DISTINCT FROM OLD.milestones
    OR NEW.required_capabilities IS DISTINCT FROM OLD.required_capabilities
    OR NEW.required_tools IS DISTINCT FROM OLD.required_tools
    OR NEW.size_class IS DISTINCT FROM OLD.size_class
    OR NEW.planning_diff IS DISTINCT FROM OLD.planning_diff
    OR NEW.created_at IS DISTINCT FROM OLD.created_at
    OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at THEN
   RAISE EXCEPTION 'immutable code cross-check snapshot';
  END IF;
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER cross_checks_code_snapshot BEFORE UPDATE ON cross_checks
 FOR EACH ROW EXECUTE FUNCTION enforce_code_cross_check_snapshot();
-- +goose StatementEnd

-- Custody changes invalidate even already-decided evidence before publication.
-- Successful completion retains its record. No-snapshot causes remain intact.
-- +goose StatementBegin
CREATE FUNCTION settle_exited_code_cross_check() RETURNS trigger LANGUAGE plpgsql AS $$
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
       OR (OLD.status IN ('claimed','running') AND OLD.claim_released_at IS NULL
         AND (NEW.claim_released_at IS NOT NULL OR NEW.status NOT IN ('claimed','running')))) THEN
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
CREATE TRIGGER runs_settle_exited_code_cross_check
 AFTER UPDATE OF status, worker_id, claim_generation, claim_released_at ON runs
 FOR EACH ROW EXECUTE FUNCTION settle_exited_code_cross_check();

-- Completion flags come from the actual persisted transition, for legacy and
-- permit-backed writers alike. A retry cannot create a second attempt.
CREATE FUNCTION record_unsupported_code_cross_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.status = 'completed' AND OLD.status <> 'completed'
   AND NEW.code_cross_check_required AND NOT NEW.report_only
   AND NEW.fix_verdict IS DISTINCT FROM 'not_code'
   AND NEW.kind IN ('issue','prompt','self_improve','ci_fix','mr_rework','task')
   AND EXISTS (SELECT 1 FROM workers w WHERE w.id = NEW.worker_id
       AND NOT ('cross_check_code_v1' = ANY(w.protocol_capabilities))) THEN
  INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation,
   verdict, outcome, reason_class, code_context, guidance_snapshot, deadline_at,
   decided_at, wait_credited, guidance_text, guidance_digest,
   repo_instructions_text, repo_instructions_digest)
  VALUES (NEW.id, 'code', 1, NEW.claim_generation, 'failed', 'failed',
   'worker_unsupported', jsonb_build_object('issue_title', NEW.issue_title,
   'issue_description', NEW.issue_description), '', now(), now(), true,
   '', sha256(convert_to('', 'UTF8')), '', sha256(convert_to('', 'UTF8')))
  ON CONFLICT DO NOTHING;
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER runs_record_unsupported_code_cross_check AFTER UPDATE OF status ON runs
 FOR EACH ROW EXECUTE FUNCTION record_unsupported_code_cross_check();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_cross_check_child_eligible(w workers, r runs, availability boolean, lane text,
    check_stage text, evaluated_at timestamptz, affinity_cutoff timestamptz)
RETURNS boolean LANGUAGE sql STABLE AS $$
SELECT (
 r.kind = 'cross_check' AND check_stage = 'code'
 AND 'cross_check_code_v1' = ANY(w.protocol_capabilities)
 AND 'cross_check_lane_v1' = ANY(w.protocol_capabilities)
 AND NOT w.isolated_lane AND NOT w.maintenance_fenced
 AND lane IN ('cross_check','any') AND w.max_cross_check_slots > 0
 AND (NOT availability OR (
   (w.draining_since IS NULL AND w.maintenance_phase NOT IN ('requested','ready','stopping','recycling')
    OR r.worker_id = w.id)
   AND (SELECT count(*) FROM runs active WHERE active.worker_id = w.id AND active.cross_check_lane
     AND active.status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup')) < w.max_cross_check_slots
 ))
 AND (NOT w.ephemeral OR w.ephemeral_run_id = r.target_run_id)
 AND EXISTS (
   SELECT 1 FROM cross_checks cc JOIN runs lead ON lead.id = cc.lead_run_id
   WHERE cc.checker_run_id = r.id AND cc.lead_run_id = r.target_run_id
    AND cc.stage = 'code' AND cc.round = 1 AND cc.outcome = 'pending'
    AND cc.interrupted_at IS NULL AND cc.deadline_at > evaluated_at
    AND lead.worker_id = w.id AND r.worker_id = w.id
    AND lead.user_id = r.user_id AND lead.user_id = w.user_id
    AND lead.code_cross_check_required AND NOT lead.report_only
    AND lead.fix_verdict IS DISTINCT FROM 'not_code'
    AND lead.kind IN ('issue','prompt','self_improve','ci_fix','mr_rework','task')
    AND r.harness = cc.checker_harness AND lead.harness <> r.harness
    AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
    AND lead.claim_generation = cc.lead_claim_generation
 )
) OR (r.kind = 'cross_check'
    AND check_stage = 'plan'
    AND 'cross_check_v1' = ANY(w.protocol_capabilities)
    AND NOT w.isolated_lane AND NOT w.maintenance_fenced
    AND EXISTS (
        SELECT 1 FROM cross_checks cc JOIN runs lead ON lead.id = cc.lead_run_id
        WHERE cc.checker_run_id = r.id AND cc.lead_run_id = r.target_run_id
          AND cc.stage = check_stage
          AND cc.round = (SELECT max(latest.round) FROM cross_checks latest
                          WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage)
          AND (cc.round = 1 OR (cc.automatic_rounds_enabled
               AND cc.round <= cc.automatic_revision_limit + 1
               AND 'cross_check_rounds_v1' = ANY(w.protocol_capabilities)))
          AND cc.verdict = 'pending' AND cc.deadline_at > evaluated_at
          AND lead.user_id = r.user_id AND lead.kind <> 'cross_check'
          AND lead.status IN ('claimed', 'running') AND lead.claim_released_at IS NULL
          AND lead.claim_generation = cc.lead_claim_generation
    )
    AND (NOT w.ephemeral OR w.ephemeral_run_id IN (r.id, r.target_run_id))
    AND (
        (lane IN ('cross_check', 'any') AND w.max_cross_check_slots > 0
         AND 'cross_check_lane_v1' = ANY(w.protocol_capabilities)
         AND (NOT availability OR
              (SELECT count(*) FROM runs active
               WHERE active.worker_id = w.id AND active.cross_check_lane
                 AND active.status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup'))
              < w.max_cross_check_slots))
        OR
        (lane IN ('run', 'any') AND w.max_cross_check_slots IS NULL
         AND NOT ('cross_check_lane_v1' = ANY(w.protocol_capabilities))
         AND (NOT availability OR w.max_concurrent_runs IS NULL OR
              (SELECT count(*) FROM runs active
               WHERE active.worker_id = w.id AND active.kind <> 'chat' AND NOT active.cross_check_lane
                 AND active.status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup'))
              < w.max_concurrent_runs))
    )
    AND (NOT availability OR (
        (w.draining_since IS NULL AND w.maintenance_phase NOT IN ('requested','ready','stopping','recycling')
         OR r.worker_id = w.id)
        AND (r.worker_id IS NULL OR r.worker_id = w.id OR r.updated_at < affinity_cutoff)
    )));
$$;
-- +goose StatementEnd

-- +goose Down
-- Restore the plan-only predicate before dropping the code columns.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_cross_check_child_eligible(w workers, r runs, availability boolean, lane text,
    check_stage text, evaluated_at timestamptz, affinity_cutoff timestamptz)
RETURNS boolean LANGUAGE sql STABLE AS $$
SELECT r.kind = 'cross_check'
    AND check_stage = 'plan'
    AND 'cross_check_v1' = ANY(w.protocol_capabilities)
    AND NOT w.isolated_lane AND NOT w.maintenance_fenced
    AND EXISTS (
        SELECT 1 FROM cross_checks cc JOIN runs lead ON lead.id = cc.lead_run_id
        WHERE cc.checker_run_id = r.id AND cc.lead_run_id = r.target_run_id
          AND cc.stage = check_stage
          AND cc.round = (SELECT max(latest.round) FROM cross_checks latest
                          WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage)
          AND (cc.round = 1 OR (cc.automatic_rounds_enabled
               AND cc.round <= cc.automatic_revision_limit + 1
               AND 'cross_check_rounds_v1' = ANY(w.protocol_capabilities)))
          AND cc.verdict = 'pending' AND cc.deadline_at > evaluated_at
          AND lead.user_id = r.user_id AND lead.kind <> 'cross_check'
          AND lead.status IN ('claimed', 'running') AND lead.claim_released_at IS NULL
          AND lead.claim_generation = cc.lead_claim_generation
    )
    AND (NOT w.ephemeral OR w.ephemeral_run_id IN (r.id, r.target_run_id))
    AND (
        (lane IN ('cross_check', 'any') AND w.max_cross_check_slots > 0
         AND 'cross_check_lane_v1' = ANY(w.protocol_capabilities)
         AND (NOT availability OR
              (SELECT count(*) FROM runs active
               WHERE active.worker_id = w.id AND active.cross_check_lane
                 AND active.status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup'))
              < w.max_cross_check_slots))
        OR
        (lane IN ('run', 'any') AND w.max_cross_check_slots IS NULL
         AND NOT ('cross_check_lane_v1' = ANY(w.protocol_capabilities))
         AND (NOT availability OR w.max_concurrent_runs IS NULL OR
              (SELECT count(*) FROM runs active
               WHERE active.worker_id = w.id AND active.kind <> 'chat' AND NOT active.cross_check_lane
                 AND active.status IN ('claimed','running','awaiting_approval','awaiting_input','awaiting_followup'))
              < w.max_concurrent_runs))
    )
    AND (NOT availability OR (
        (w.draining_since IS NULL AND w.maintenance_phase NOT IN ('requested','ready','stopping','recycling')
         OR r.worker_id = w.id)
        AND (r.worker_id IS NULL OR r.worker_id = w.id OR r.updated_at < affinity_cutoff)
    ));
$$;
-- +goose StatementEnd
DROP TRIGGER runs_record_unsupported_code_cross_check ON runs;
DROP FUNCTION record_unsupported_code_cross_check();
DROP TRIGGER runs_settle_exited_code_cross_check ON runs;
DROP FUNCTION settle_exited_code_cross_check();
DROP TRIGGER cross_checks_code_snapshot ON cross_checks;
DROP FUNCTION enforce_code_cross_check_snapshot();
DELETE FROM runs WHERE kind = 'cross_check' AND id IN (
 SELECT checker_run_id FROM cross_checks WHERE stage = 'code');
DELETE FROM cross_checks WHERE stage = 'code';
DROP INDEX cross_checks_code_pending_deadline;
DROP INDEX cross_checks_one_code_attempt;
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_code_shape;
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_reason_class_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_reason_class_check CHECK (
 reason_class IS NULL OR (verdict IN ('approve','revise','block') AND reason_class = verdict)
 OR (verdict = 'failed' AND reason_class IN ('malformed','model_error','model_timeout',
 'checker_unavailable','confinement_failed','timed_out','superseded','approved_not_stored')));
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_stage_check;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_stage_check CHECK (stage = 'plan');
ALTER TABLE cross_checks DROP COLUMN head_commit, DROP COLUMN outcome, DROP COLUMN code_context,
 DROP COLUMN guidance_snapshot, DROP COLUMN guidance_text, DROP COLUMN guidance_digest,
 DROP COLUMN repo_instructions_enabled, DROP COLUMN repo_instructions_text, DROP COLUMN repo_instructions_digest, DROP COLUMN dispositions, DROP COLUMN finalized_at;
ALTER TABLE runs DROP COLUMN code_cross_check_required;
ALTER TABLE users DROP COLUMN code_cross_check_enabled;
