-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION fn_custody_attention(hold_state text, available boolean, guarded boolean,
                                    capture_state text, run_status text, recovery_wait_cause text)
RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE
  WHEN hold_state = 'discarded' THEN 'discarded'
  WHEN hold_state = 'released' THEN 'released'
  WHEN hold_state = 'open' AND run_status = 'recovery_wait'
       AND recovery_wait_cause = 'worker_requeue_exhausted' THEN
       CASE WHEN capture_state = 'needs_action' THEN 'needs_action' ELSE 'source_only' END
  WHEN available AND NOT guarded THEN 'archive_ready'
  WHEN capture_state IN ('preparing', 'uploading') THEN 'capturing'
  WHEN capture_state = 'needs_action' THEN 'needs_action'
  WHEN COALESCE(run_status, '') <> '' AND run_status NOT IN ('completed', 'failed', 'cancelled') THEN 'active'
  ELSE 'source_only' END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION fn_is_decision_attention(attention text)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT COALESCE(attention IN ('needs_action', 'source_only'), false)
$$;
-- +goose StatementEnd

-- Classification inputs, identity, and derived facts only; listing payloads come from holds.
CREATE VIEW recovery_custody_hold_facts AS
SELECT h.id, h.user_id, h.run_id, h.state, h.inventory_guarded,
       summary.has_available_capture, summary.capture_state,
       COALESCE(r.status, '')::text AS run_status,
       COALESCE(r.recovery_wait_cause, '')::text AS recovery_wait_cause,
       fn_custody_attention(h.state, summary.has_available_capture, h.inventory_guarded,
                           summary.capture_state, r.status, r.recovery_wait_cause)::text AS attention,
       fn_is_decision_attention(fn_custody_attention(h.state, summary.has_available_capture,
                           h.inventory_guarded, summary.capture_state, r.status, r.recovery_wait_cause))::boolean AS decision_needed
FROM recovery_custody_holds h
LEFT JOIN runs r ON r.id = h.run_id AND r.user_id = h.user_id
CROSS JOIN LATERAL (
 SELECT EXISTS (SELECT 1 FROM recovery_captures c
                WHERE c.hold_id = h.id AND c.user_id = h.user_id AND c.state = 'available')::boolean AS has_available_capture,
        COALESCE((SELECT c.state FROM recovery_captures c
                  WHERE c.hold_id = h.id AND c.user_id = h.user_id
                  ORDER BY c.created_at DESC, c.id DESC LIMIT 1), '')::text AS capture_state
 OFFSET 0
) summary;

-- Filter base owner/open holds before evaluating any capture summary.
-- Qualify first, then deterministically exclude one hold per run; unknown evidence counts.
-- +goose StatementBegin
CREATE FUNCTION fn_custody_admission_count(owner_id uuid, heartbeat_cutoff timestamptz)
RETURNS bigint LANGUAGE sql STABLE AS $$
 WITH owner_open AS MATERIALIZED (
  SELECT h.id, h.user_id, h.run_id, h.state, h.inventory_guarded,
         h.generation, h.live_run_id, h.live_worker_id, h.created_at
  FROM recovery_custody_holds h
  WHERE h.user_id = owner_id AND h.state = 'open'
 ), qualifying AS (
  SELECT h.id, h.run_id, h.created_at
  FROM owner_open h
  JOIN runs r ON r.id = h.run_id AND r.user_id = h.user_id
  JOIN workers w ON w.id = h.live_worker_id AND w.user_id = h.user_id
  CROSS JOIN LATERAL (
   SELECT EXISTS (SELECT 1 FROM recovery_captures c
                  WHERE c.hold_id = h.id AND c.user_id = h.user_id AND c.state = 'available') AS available,
          COALESCE((SELECT c.state FROM recovery_captures c
                    WHERE c.hold_id = h.id AND c.user_id = h.user_id
                    ORDER BY c.created_at DESC, c.id DESC LIMIT 1), '') AS capture_state
   OFFSET 0
  ) summary
  WHERE NOT fn_is_decision_attention(fn_custody_attention(
          h.state, summary.available, h.inventory_guarded, summary.capture_state, r.status, r.recovery_wait_cause))
    AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
    AND r.claim_released_at IS NULL
    AND h.generation = r.claim_generation
    AND h.live_run_id = r.id
    AND h.live_worker_id = r.worker_id
    AND w.last_heartbeat_at >= heartbeat_cutoff
 ), excluded AS (
  SELECT DISTINCT ON (run_id) id FROM qualifying ORDER BY run_id, created_at, id
 )
 SELECT (SELECT count(*) FROM owner_open) - (SELECT count(*) FROM excluded)
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION fn_custody_admission_count(uuid, timestamptz);
DROP VIEW recovery_custody_hold_facts;
DROP FUNCTION fn_is_decision_attention(text);
DROP FUNCTION fn_custody_attention(text, boolean, boolean, text, text, text);
