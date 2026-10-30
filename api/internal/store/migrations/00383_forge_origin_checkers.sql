-- +goose Up
-- Availability advice only; static suitability stays independent.
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
   fn_forge_origin_eligible(w, r, evaluated_at) AND
   (fn_worker_accepts_new_intake(w)
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
    AND (lead.status IN ('claimed','running') OR (lead.status = 'awaiting_followup' AND lead.kind = 'task' AND lead.interactive)) AND lead.claim_released_at IS NULL
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
        fn_forge_origin_eligible(w, r, evaluated_at) AND
        (fn_worker_accepts_new_intake(w)
         OR r.worker_id = w.id)
        AND (r.worker_id IS NULL OR r.worker_id = w.id OR r.updated_at < affinity_cutoff)
    )));
$$;
-- +goose StatementEnd

-- +goose Down
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
   (fn_worker_accepts_new_intake(w)
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
    AND (lead.status IN ('claimed','running') OR (lead.status = 'awaiting_followup' AND lead.kind = 'task' AND lead.interactive)) AND lead.claim_released_at IS NULL
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
        (fn_worker_accepts_new_intake(w)
         OR r.worker_id = w.id)
        AND (r.worker_id IS NULL OR r.worker_id = w.id OR r.updated_at < affinity_cutoff)
    )));
$$;
-- +goose StatementEnd
