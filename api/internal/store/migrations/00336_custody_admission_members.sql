-- +goose Up
-- Per-hold, read-only visibility projection of custody admission membership (issue #2970).
-- fn_custody_admission_count (00310) stays the canonical accounting function (ADR-2445) and is
-- NOT modified here; this function only explains it. The two are kept in agreement by the parity
-- tests in custody_admission_livedb_test.go. One row per owner open hold: counted mirrors
-- fn_custody_admission_count membership (never derived from reason), reason names the first
-- matching cause in a closed vocabulary.
-- +goose StatementBegin
CREATE FUNCTION fn_custody_admission_members(owner_id uuid, heartbeat_cutoff timestamptz)
RETURNS TABLE(hold_id uuid, counted boolean, reason text) LANGUAGE sql STABLE AS $$
 WITH owner_open AS MATERIALIZED (
  SELECT h.id, h.user_id, h.run_id, h.state, h.inventory_guarded,
         h.generation, h.live_run_id, h.live_worker_id, h.created_at
  FROM recovery_custody_holds h
  WHERE h.user_id = owner_id AND h.state = 'open'
 ), facts AS (
  SELECT h.id, h.run_id, h.created_at, h.generation, h.live_run_id, h.live_worker_id,
         r.id AS r_id, r.status AS r_status, r.claim_generation AS r_gen,
         r.worker_id AS r_worker_id, r.claim_released_at AS r_released,
         w.id AS w_id, w.last_heartbeat_at AS w_hb,
         fn_custody_attention(h.state, summary.available, h.inventory_guarded,
                              summary.capture_state, r.status, r.recovery_wait_cause) AS attention,
         COALESCE(
           r.id IS NOT NULL AND w.id IS NOT NULL
           AND NOT fn_is_decision_attention(fn_custody_attention(
                 h.state, summary.available, h.inventory_guarded, summary.capture_state,
                 r.status, r.recovery_wait_cause))
           AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
           AND r.claim_released_at IS NULL
           AND h.generation = r.claim_generation
           AND h.live_run_id = r.id
           AND h.live_worker_id = r.worker_id
           AND w.last_heartbeat_at >= heartbeat_cutoff,
           false) AS qualifies
  FROM owner_open h
  LEFT JOIN runs r ON r.id = h.run_id AND r.user_id = h.user_id
  LEFT JOIN workers w ON w.id = h.live_worker_id AND w.user_id = h.user_id
  CROSS JOIN LATERAL (
   SELECT EXISTS (SELECT 1 FROM recovery_captures c
                  WHERE c.hold_id = h.id AND c.user_id = h.user_id AND c.state = 'available') AS available,
          COALESCE((SELECT c.state FROM recovery_captures c
                    WHERE c.hold_id = h.id AND c.user_id = h.user_id
                    ORDER BY c.created_at DESC, c.id DESC LIMIT 1), '') AS capture_state
   OFFSET 0
  ) summary
 ), ranked AS (
  SELECT f.*,
         CASE WHEN f.qualifies THEN
           row_number() OVER (PARTITION BY f.qualifies, f.run_id ORDER BY f.created_at, f.id)
         END AS rn
  FROM facts f
 )
 SELECT k.id::uuid AS hold_id,
        (NOT COALESCE(k.rn = 1, false))::boolean AS counted,
        (CASE
          WHEN k.rn = 1 THEN 'live_claim'
          WHEN k.qualifies THEN 'duplicate_hold'
          WHEN fn_is_decision_attention(k.attention) THEN 'decision_needed'
          WHEN k.r_id IS NOT NULL AND k.r_status IN ('completed', 'failed', 'cancelled') THEN 'terminal_run'
          WHEN k.r_id IS NOT NULL
               AND k.r_status NOT IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
               THEN 'run_not_live'
          WHEN k.r_released IS NOT NULL THEN 'claim_released'
          WHEN k.r_gen IS NOT NULL AND k.generation < k.r_gen THEN 'earlier_generation'
          WHEN k.r_gen IS NOT NULL AND k.generation > k.r_gen THEN 'other_generation'
          WHEN k.r_id IS NULL OR k.live_run_id IS NULL OR k.live_worker_id IS NULL OR k.w_id IS NULL THEN 'unknown_evidence'
          WHEN k.live_run_id IS DISTINCT FROM k.r_id OR k.live_worker_id IS DISTINCT FROM k.r_worker_id
               THEN 'identity_mismatch'
          WHEN k.w_hb IS NULL OR heartbeat_cutoff IS NULL OR k.w_hb < heartbeat_cutoff THEN 'stale_heartbeat'
          ELSE 'unknown_evidence'
         END)::text AS reason
 FROM ranked k
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION fn_custody_admission_members(uuid, timestamptz);
