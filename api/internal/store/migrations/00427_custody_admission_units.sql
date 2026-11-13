-- +goose Up
-- Custody admission units (issue #4162, ADR-2445 amendment 2026-11-10). Redefines the canonical
-- count (00310) and its per-hold visibility projection (00336) in place with CREATE OR REPLACE;
-- both stay LANGUAGE sql STABLE with unchanged signatures, so every caller (ClaimRun, the
-- health read, the aggregate, the owner-over-limit scans) follows automatically.
-- The D2 exclusion (one healthy live hold per run) is unchanged. After it, the remaining open
-- holds that are group-eligible count ONCE per (run, original worker): the hold's live run/worker
-- equal its own run/original worker (non-null), the run and the worker exist and belong to the
-- hold's owner, the hold's repo equals the run's repo (non-null), and 1 <= generation <=
-- claim_generation. No heartbeat term. Any NULL leaves the hold a singleton. The representative
-- is the oldest by (created_at, id); other members report the new reason same_run_group
-- (counted=false), placed right after live_claim. Per-hold parity (counted flags sum to the
-- count) is kept; custody_admission_livedb_test.go and custody_admission_units_livedb_test.go pin it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_custody_admission_count(owner_id uuid, heartbeat_cutoff timestamptz)
RETURNS bigint LANGUAGE sql STABLE AS $$
 WITH owner_open AS MATERIALIZED (
  SELECT h.id, h.user_id, h.run_id, h.state, h.inventory_guarded,
         h.generation, h.live_run_id, h.live_worker_id, h.created_at,
         h.repo_id, h.original_worker_id
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
 ), counted AS (
  SELECT h.id, h.user_id, h.run_id, h.repo_id, h.generation, h.live_run_id,
         h.live_worker_id, h.original_worker_id
  FROM owner_open h
  WHERE NOT EXISTS (SELECT 1 FROM excluded e WHERE e.id = h.id)
 ), grouped AS (
  SELECT c.run_id, c.original_worker_id
  FROM counted c
  JOIN runs r ON r.id = c.run_id AND r.user_id = c.user_id
  JOIN workers w ON w.id = c.original_worker_id AND w.user_id = c.user_id
  WHERE c.live_run_id = c.run_id
    AND c.live_worker_id = c.original_worker_id
    AND c.repo_id IS NOT NULL AND c.repo_id = r.repo_id
    AND c.generation >= 1 AND c.generation <= r.claim_generation
 )
 SELECT (SELECT count(*) FROM counted)
        - (SELECT count(*) FROM grouped)
        + (SELECT count(*) FROM (SELECT DISTINCT run_id, original_worker_id FROM grouped) g)
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_custody_admission_members(owner_id uuid, heartbeat_cutoff timestamptz)
RETURNS TABLE(hold_id uuid, counted boolean, reason text) LANGUAGE sql STABLE AS $$
 WITH owner_open AS MATERIALIZED (
  SELECT h.id, h.user_id, h.run_id, h.state, h.inventory_guarded,
         h.generation, h.live_run_id, h.live_worker_id, h.created_at,
         h.repo_id, h.original_worker_id
  FROM recovery_custody_holds h
  WHERE h.user_id = owner_id AND h.state = 'open'
 ), facts AS (
  SELECT h.id, h.run_id, h.created_at, h.generation, h.live_run_id, h.live_worker_id,
         h.original_worker_id,
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
           false) AS qualifies,
         COALESCE(
           r.id IS NOT NULL AND wo.id IS NOT NULL
           AND h.live_run_id = h.run_id
           AND h.live_worker_id = h.original_worker_id
           AND h.repo_id IS NOT NULL AND h.repo_id = r.repo_id
           AND h.generation >= 1 AND h.generation <= r.claim_generation,
           false) AS group_eligible
  FROM owner_open h
  LEFT JOIN runs r ON r.id = h.run_id AND r.user_id = h.user_id
  LEFT JOIN workers w ON w.id = h.live_worker_id AND w.user_id = h.user_id
  LEFT JOIN workers wo ON wo.id = h.original_worker_id AND wo.user_id = h.user_id
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
 ), flagged AS (
  SELECT k.*,
         (k.group_eligible AND NOT COALESCE(k.rn = 1, false)) AS grouped
  FROM ranked k
 ), grp_ranked AS (
  SELECT g.*,
         row_number() OVER (PARTITION BY g.grouped, g.run_id, g.original_worker_id ORDER BY g.created_at, g.id) AS grp_rn
  FROM flagged g
 )
 SELECT k.id::uuid AS hold_id,
        (NOT COALESCE(k.rn = 1, false) AND (NOT k.grouped OR k.grp_rn = 1))::boolean AS counted,
        (CASE
          WHEN k.rn = 1 THEN 'live_claim'
          WHEN k.grouped AND k.grp_rn > 1 THEN 'same_run_group'
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
 FROM grp_ranked k
$$;
-- +goose StatementEnd

-- +goose Down
-- Restore the 00310 and 00336 bodies verbatim.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_custody_admission_count(owner_id uuid, heartbeat_cutoff timestamptz)
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_custody_admission_members(owner_id uuid, heartbeat_cutoff timestamptz)
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
