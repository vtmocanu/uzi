-- +goose Up
ALTER TABLE workers ADD COLUMN max_cross_check_slots integer
    CHECK (max_cross_check_slots BETWEEN 0 AND 16);
ALTER TABLE runs ADD COLUMN cross_check_lane boolean NOT NULL DEFAULT false
    CHECK (NOT cross_check_lane OR kind = 'cross_check');

-- A queued row has no occupancy, but recovery of the same generation must retain
-- the lane chosen at claim time, independently of later worker advertisements.
ALTER TABLE runs ADD COLUMN cross_check_lane_generation bigint;

-- +goose StatementBegin
CREATE FUNCTION runs_reset_cross_check_lane() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.cross_check_lane_generation := NULL;
    ELSIF NEW.claim_generation IS DISTINCT FROM OLD.claim_generation THEN
        NEW.cross_check_lane_generation := NULL;
    END IF;
    IF NEW.status = 'queued' THEN
        NEW.cross_check_lane := false;
    ELSIF NEW.cross_check_lane AND NEW.kind = 'cross_check' THEN
        NEW.cross_check_lane_generation := NEW.claim_generation;
    ELSIF NEW.kind = 'cross_check'
          AND NEW.cross_check_lane_generation = NEW.claim_generation THEN
        NEW.cross_check_lane := true;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER runs_reset_cross_check_lane
    BEFORE INSERT OR UPDATE OF status, claim_generation, cross_check_lane ON runs
    FOR EACH ROW EXECUTE FUNCTION runs_reset_cross_check_lane();

-- Shared child-specific rules. Other claim fences remain at each caller.
-- Strict health ignores occupancy, cordons and temporary affinity.
CREATE FUNCTION fn_cross_check_child_eligible(w workers, r runs, availability boolean, lane text,
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

-- +goose Down
DROP FUNCTION fn_cross_check_child_eligible(workers, runs, boolean, text, text, timestamptz, timestamptz);
DROP TRIGGER runs_reset_cross_check_lane ON runs;
DROP FUNCTION runs_reset_cross_check_lane();
ALTER TABLE runs DROP COLUMN cross_check_lane_generation;
ALTER TABLE runs DROP COLUMN cross_check_lane;
ALTER TABLE workers DROP COLUMN max_cross_check_slots;
