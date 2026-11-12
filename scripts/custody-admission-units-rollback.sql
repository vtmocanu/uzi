-- Rollback of the custody admission units migration (issue #4162, migration 00424) for
-- operators who must run the PREVIOUS api image against an already-upgraded database.
--
-- Why: the previous api image's ClaimRun calls fn_custody_admission_count, which the 00424
-- migration redefines to count admission UNITS. Without the hard per-run bound that ships in
-- the same image, the old binary would admit a run whose own open holds already reach the
-- limit. This script restores the 00310 / 00336 function bodies (byte-for-byte the Down block
-- of 00424_custody_admission_units.sql) and removes the goose row for version 424, so a later
-- upgrade re-applies 00424 Up.
--
-- Valid only while 424 is the head: the guard below refuses unless goose version 424 is
-- applied AND is the highest applied version. Run it with the UPGRADED api stopped, before
-- starting the previous image. It is non-destructive: it only replaces two functions and
-- deletes one goose bookkeeping row; no hold, run, worker or capture state changes.
--
-- If migration 00424 is renumbered at merge, update ALL FIVE occurrences of 424 below (the
-- guard, the two refusal messages, the IS DISTINCT FROM comparison and the DELETE); the migration test custody_admission_units_migration_livedb_test.go fails
-- when the number here differs from the migration filename prefix.
--
-- Usage: psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/custody-admission-units-rollback.sql
BEGIN;

DO $$
DECLARE
  head_version bigint;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM goose_db_version WHERE version_id = 424 AND is_applied) THEN
    RAISE EXCEPTION 'custody admission units rollback refused: goose version 424 is not applied';
  END IF;
  SELECT max(version_id) INTO head_version FROM goose_db_version WHERE is_applied;
  IF head_version IS DISTINCT FROM 424 THEN
    RAISE EXCEPTION 'custody admission units rollback refused: 424 is not the head migration (head is %)', head_version;
  END IF;
END
$$;

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

DELETE FROM goose_db_version WHERE version_id = 424;

COMMIT;
