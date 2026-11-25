-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Take every lock this migration needs up front, in one fixed order: CREATE TABLE ... REFERENCES takes
-- SHARE ROW EXCLUSIVE on the referenced tables, so a later ALTER TABLE runs (ACCESS EXCLUSIVE) would be
-- a lock upgrade that can deadlock against a concurrent writer holding ROW EXCLUSIVE and waiting on
-- our SHARE ROW EXCLUSIVE. The strongest lock goes first; users, workers and recovery_captures are only
-- referenced, so SHARE ROW EXCLUSIVE is already the strongest this migration takes on them.
LOCK TABLE runs, checkpoint_publish_attempts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE users, workers, recovery_captures IN SHARE ROW EXCLUSIVE MODE;
-- Issue #4628 ("fresh resume"): an owner Resume of a run held at a worker-exhaustion recovery_wait
-- can start it on a SEPARATE persistent hosted worker (W2) from durable evidence (a recovery capture
-- or the broker-owned checkpoint ref) instead of waiting for the predecessor worker (W1). This
-- migration is the store half: the transfer table, the two guard triggers, the owner-admission
-- reservation and the columns the evidence selection binds to. Goose numbers are draft until
-- merge; renumber above the live head on landing.
--
-- run_fresh_resumes: one row per transfer attempt.
--   pending   start recorded; the run is untouched (still recovery_wait).
--   claimed   W2 claimed the run at claim_generation (G'); saved_tuple holds what a refusal restores.
--   seeded    W2 reported its first fenced commit (destination_attempt_id / seeded_generation, write-once).
--   superseded a later transfer seeded after this one.
--   refused / cancelled / finished   closed.
-- saved_tuple is the pre-claim runs fields a refusal puts back, as the jsonb of PrepareFreshResumeClaim;
-- saved_worker_id is the pre-claim runs.worker_id as a real FK so the restore can lock that worker row.
CREATE TABLE run_fresh_resumes (
    id                    uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id                uuid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    user_id               uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Client-generated per confirm action; a replay of a completed request is refused, never re-queued.
    request_id            uuid        NOT NULL,
    evidence_generation   bigint      NOT NULL,
    evidence_kind         text        NOT NULL CHECK (evidence_kind IN ('capture', 'checkpoint_ref')),
    -- Identity only: captures are never deleted, their bytes are protected by the capture guards.
    capture_id            uuid        REFERENCES recovery_captures(id) ON DELETE RESTRICT,
    checkpoint_ref        text,
    sha                   text        NOT NULL,
    candidate_token       text        NOT NULL,
    predecessor_worker_id uuid        REFERENCES workers(id) ON DELETE SET NULL,
    destination_worker_id uuid        REFERENCES workers(id) ON DELETE SET NULL,
    saved_worker_id       uuid        REFERENCES workers(id) ON DELETE SET NULL,
    saved_tuple           jsonb,
    claim_generation      bigint,
    destination_attempt_id text,
    seeded_generation     bigint,
    state                 text        NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'claimed', 'seeded', 'superseded', 'refused', 'cancelled', 'finished')),
    refusal_reason        text,
    admission_deadline    timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    transfer_deadline     timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    claimed_at            timestamptz,
    seeded_at             timestamptz,
    closed_at             timestamptz,
    CONSTRAINT run_fresh_resumes_run_request_key UNIQUE (run_id, request_id),
    CONSTRAINT run_fresh_resumes_evidence_shape CHECK (
        (evidence_kind = 'capture' AND capture_id IS NOT NULL AND checkpoint_ref IS NULL)
        OR (evidence_kind = 'checkpoint_ref' AND capture_id IS NULL AND checkpoint_ref IS NOT NULL)),
    -- The agent's ATTEMPT_ID_RE (agent/src/attempt-path.ts) with the generation digits bounded to fit bigint.
    CONSTRAINT run_fresh_resumes_attempt_id_shape CHECK (
        destination_attempt_id IS NULL
        OR (length(destination_attempt_id) <= 64
            AND destination_attempt_id ~ '^[0-9]{8}T[0-9]{6}Z-(g[0-9]{1,18}|gx)-[0-9a-f]{16}$')),
    CONSTRAINT run_fresh_resumes_seeded_shape CHECK (
        (seeded_at IS NULL) = (destination_attempt_id IS NULL)
        AND (seeded_at IS NULL) = (seeded_generation IS NULL))
);
CREATE INDEX run_fresh_resumes_post_seed_idx ON run_fresh_resumes (run_id)
    WHERE state IN ('seeded', 'superseded');
CREATE UNIQUE INDEX run_fresh_resumes_one_active_idx ON run_fresh_resumes (run_id)
    WHERE state IN ('pending', 'claimed');
CREATE UNIQUE INDEX run_fresh_resumes_one_seeded_idx ON run_fresh_resumes (run_id)
    WHERE state = 'seeded';
CREATE INDEX run_fresh_resumes_capture_idx ON run_fresh_resumes (capture_id)
    WHERE state IN ('pending', 'claimed');
CREATE INDEX run_fresh_resumes_predecessor_idx ON run_fresh_resumes (predecessor_worker_id);
CREATE INDEX run_fresh_resumes_saved_worker_idx ON run_fresh_resumes (saved_worker_id);
CREATE INDEX run_fresh_resumes_destination_idx ON run_fresh_resumes (destination_worker_id);
CREATE INDEX run_fresh_resumes_owner_idx ON run_fresh_resumes (user_id)
    WHERE state IN ('pending', 'claimed', 'seeded', 'superseded');

-- The destination attempt and the seeded generation are written once, with seeded_at.
-- +goose StatementBegin
CREATE FUNCTION run_fresh_resumes_write_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (OLD.destination_attempt_id IS NOT NULL AND NEW.destination_attempt_id IS DISTINCT FROM OLD.destination_attempt_id)
       OR (OLD.seeded_generation IS NOT NULL AND NEW.seeded_generation IS DISTINCT FROM OLD.seeded_generation)
       OR (OLD.seeded_at IS NOT NULL AND NEW.seeded_at IS DISTINCT FROM OLD.seeded_at) THEN
        RAISE EXCEPTION 'fresh resume seeded receipt is write-once' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER run_fresh_resumes_write_once_trg BEFORE UPDATE ON run_fresh_resumes
FOR EACH ROW EXECUTE FUNCTION run_fresh_resumes_write_once();

-- New runs / attempt columns. All metadata-only (nullable without default, or a constant default).
--   fresh_hold_version: bumped by every tuple restore and every exit from pending (the preview token
--     binds it); outside the guard trigger's WHEN, so a bump needs no opt-in.
--   checkpoint_tip_generation(+_tip): the generation the checkpoint tip was published under, valid
--     only while checkpoint_tip_generation_tip equals checkpoint_tip (an unstamped tip rewrite reads
--     as unbound).
ALTER TABLE runs
    ADD COLUMN fresh_hold_version bigint NOT NULL DEFAULT 0,
    ADD COLUMN checkpoint_tip_generation bigint,
    ADD COLUMN checkpoint_tip_generation_tip text;
ALTER TABLE checkpoint_publish_attempts ADD COLUMN claim_generation bigint;

-- Run eligibility mirror of ResumeWorkerRecoveryEpisode (runtime.sql) for the owner-held exhaustion
-- causes, narrowed for a fresh claim: no secret remediation and no source_end_declined detail, owner
-- held (no retry stamp), issue / mr_rework / self_improve kinds, never a claimed Codex run (#4476
-- provenance predicate of fn_codex_claim_provenance_admits), no active publication resume, no
-- pending fresh policy or codex policy hold (ClaimRun would insert a 'resume' input row), and no
-- pending weekly-reserve pause request. global_timeout_seconds is the server wall default
-- ResumeWorkerRecoveryEpisode takes as @global_timeout_seconds.
-- +goose StatementBegin
CREATE FUNCTION fn_fresh_resume_run_base(r runs, global_timeout_seconds integer)
RETURNS boolean LANGUAGE sql STABLE AS $$
SELECT r.status = 'recovery_wait'
   AND r.recovery_wait_cause IN ('worker_requeue_exhausted', 'retained_recovery_exhausted',
                                 'retained_recovery_adoption_failed', 'retained_continuation_unwitnessed')
   AND r.recovery_wait_detail IS DISTINCT FROM 'secret_remediation_active'
   AND r.recovery_wait_detail IS DISTINCT FROM 'source_end_declined'
   AND r.recovery_retry_not_before IS NULL
   AND r.kind IN ('issue', 'mr_rework', 'self_improve')
   AND NOT ((r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
            AND r.claim_generation >= 1)
   AND NOT EXISTS (SELECT 1 FROM run_publication_resume p WHERE p.run_id = r.id AND p.state = 'active')
   AND NOT r.policy_pending_fresh
   AND r.hold_reason IS DISTINCT FROM 'codex_policy_hold'
   AND r.pause_mode IS DISTINCT FROM 'reserve'
   AND (r.interactive OR r.started_at IS NULL
        OR (COALESCE(r.budget_wall_seconds, global_timeout_seconds) + r.budget_extension_seconds
            + r.budget_finalize_seconds)
           - (GREATEST(0, EXTRACT(EPOCH FROM (r.status_since - r.started_at))::int) - r.budget_paused_seconds) > 0)
$$;
-- +goose StatementEnd

-- BEFORE UPDATE guard on runs. The name sorts before every other BEFORE UPDATE trigger on runs, so it
-- sees NEW before any of them can skip the row (RETURN NULL) or normalize it. It validates only: it
-- never writes another table and never rewrites NEW (the 00495 invariant for BEFORE UPDATE triggers
-- on runs). WHEN keeps unrelated runs writes off it; the body is one indexed lookup by run_id.
--   Rule 1 (a pending or claimed transfer row exists): any change of status, worker_id or
--     claim_generation raises UZ416 'fresh resume transfer in progress', except
--       - the transaction opted in for this row (uzi.fresh_resume_id, set per transaction);
--       - Cancel (status -> cancelled);
--       - auto-stop (status -> failed with uzi.fresh_resume_terminal = this run id);
--       - the claimed destination starting: claimed/running -> running, worker and generation unchanged;
--       - the ON DELETE SET NULL cascade of a deleted worker: worker_id -> NULL, status and
--         generation unchanged, and the old worker row no longer visible.
--   Rule 2 (a seeded or superseded row exists, run nonterminal): a worker_id change to the
--     predecessor, or to a worker holding an open custody hold older than seeded_generation, raises
--     UZ416 'fresh resume predecessor excluded'. No opt-in bypasses it.
-- SQLSTATE UZ416 is this guard's own (the UZ413..UZ415 convention).
-- +goose StatementBegin
CREATE FUNCTION a0_guard_fresh_resume() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    t run_fresh_resumes%ROWTYPE;
BEGIN
    SELECT * INTO t FROM run_fresh_resumes
    WHERE run_id = OLD.id AND state IN ('pending', 'claimed') LIMIT 1;
    IF FOUND THEN
        IF current_setting('uzi.fresh_resume_id', true) = t.id::text THEN
            NULL;
        ELSIF NEW.status = 'cancelled' THEN
            NULL;
        ELSIF NEW.status = 'failed' AND current_setting('uzi.fresh_resume_terminal', true) = OLD.id::text THEN
            NULL;
        ELSIF t.state = 'claimed'
              AND OLD.status IN ('claimed', 'running') AND NEW.status = 'running'
              AND NEW.worker_id IS NOT DISTINCT FROM OLD.worker_id
              AND NEW.claim_generation = OLD.claim_generation
              AND OLD.worker_id IS NOT DISTINCT FROM t.destination_worker_id THEN
            NULL;
        ELSIF NEW.worker_id IS NULL AND OLD.worker_id IS NOT NULL
              AND NEW.status = OLD.status AND NEW.claim_generation = OLD.claim_generation
              AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = OLD.worker_id) THEN
            NULL;
        ELSE
            RAISE EXCEPTION 'fresh resume transfer in progress' USING ERRCODE = 'UZ416';
        END IF;
    END IF;
    IF NEW.worker_id IS NOT NULL AND NEW.worker_id IS DISTINCT FROM OLD.worker_id
       AND OLD.status NOT IN ('completed', 'failed', 'cancelled')
       AND EXISTS (
            SELECT 1 FROM run_fresh_resumes s
            WHERE s.run_id = OLD.id AND s.state IN ('seeded', 'superseded')
              AND (s.predecessor_worker_id = NEW.worker_id
                   OR EXISTS (SELECT 1 FROM recovery_custody_holds h
                              WHERE h.run_id = s.run_id AND h.state = 'open'
                                AND h.generation < s.seeded_generation
                                AND NEW.worker_id IN (h.live_worker_id, h.original_worker_id)))) THEN
        RAISE EXCEPTION 'fresh resume predecessor excluded' USING ERRCODE = 'UZ416';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER a0_guard_fresh_resume_trg BEFORE UPDATE ON runs
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status
                   OR OLD.worker_id IS DISTINCT FROM NEW.worker_id
                   OR OLD.claim_generation IS DISTINCT FROM NEW.claim_generation)
EXECUTE FUNCTION a0_guard_fresh_resume();

-- AFTER UPDATE: a run turning terminal closes its transfer rows (pending / claimed -> cancelled,
-- seeded / superseded -> finished). It writes only run_fresh_resumes, never runs.
-- +goose StatementBegin
CREATE FUNCTION fresh_resume_after() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE run_fresh_resumes
       SET state = 'cancelled', closed_at = now(), refusal_reason = COALESCE(refusal_reason, 'run_terminal')
     WHERE run_id = NEW.id AND state IN ('pending', 'claimed');
    UPDATE run_fresh_resumes
       SET state = 'finished', closed_at = now()
     WHERE run_id = NEW.id AND state IN ('seeded', 'superseded');
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER fresh_resume_after_trg AFTER UPDATE OF status ON runs
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status AND NEW.status IN ('completed', 'failed', 'cancelled'))
EXECUTE FUNCTION fresh_resume_after();

-- Owner admission reservation (R1). The 00427 count plus this owner's reserved fresh-resume units on
-- nonterminal runs: a pending row is one unit; a claimed, seeded or superseded row is one unit only
-- while W2's open hold at its claim_generation exists AND no non-excluded 00427 hold for the same
-- (run, original worker = destination worker) is already counted (anti-join on the singleton-inclusive
-- counted set, so a unit is never counted twice). Signatures are unchanged. The members projection emits
-- one synthetic row per reserved unit (hold_id = transfer id, reason 'fresh_reservation', counted).
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
 ), reserved AS (
  SELECT t.id
  FROM run_fresh_resumes t
  JOIN runs r ON r.id = t.run_id AND r.user_id = t.user_id
  WHERE t.user_id = owner_id
    AND t.state IN ('pending', 'claimed', 'seeded', 'superseded')
    AND r.status NOT IN ('completed', 'failed', 'cancelled')
    AND (t.state = 'pending'
         OR (EXISTS (SELECT 1 FROM owner_open h
                     WHERE h.run_id = t.run_id AND h.generation = t.claim_generation)
             AND NOT EXISTS (SELECT 1 FROM counted c
                             WHERE c.run_id = t.run_id AND c.original_worker_id = t.destination_worker_id)))
 )
 SELECT (SELECT count(*) FROM counted)
        - (SELECT count(*) FROM grouped)
        + (SELECT count(*) FROM (SELECT DISTINCT run_id, original_worker_id FROM grouped) g)
        + (SELECT count(*) FROM reserved)
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
 ), reserved AS (
  SELECT t.id
  FROM run_fresh_resumes t
  JOIN runs r ON r.id = t.run_id AND r.user_id = t.user_id
  WHERE t.user_id = owner_id
    AND t.state IN ('pending', 'claimed', 'seeded', 'superseded')
    AND r.status NOT IN ('completed', 'failed', 'cancelled')
    AND (t.state = 'pending'
         OR (EXISTS (SELECT 1 FROM owner_open h
                     WHERE h.run_id = t.run_id AND h.generation = t.claim_generation)
             AND NOT EXISTS (SELECT 1 FROM grp_ranked c
                             WHERE NOT COALESCE(c.rn = 1, false)
                               AND c.run_id = t.run_id AND c.original_worker_id = t.destination_worker_id)))
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
 UNION ALL
 SELECT t.id::uuid, true, 'fresh_reservation'::text FROM reserved t
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Exclude concurrent transfer writes before checking for outstanding rows (00391 shape). The next
-- statement takes a fresh READ COMMITTED snapshot after this lock is acquired.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'fresh resume schema downgrade requires READ COMMITTED';
    END IF;
    IF EXISTS (SELECT 1 FROM run_fresh_resumes t JOIN runs r ON r.id = t.run_id
               WHERE t.state IN ('pending', 'claimed', 'seeded', 'superseded')
                 AND r.status NOT IN ('completed', 'failed', 'cancelled')) THEN
        RAISE EXCEPTION 'cannot downgrade with outstanding fresh resume transfers';
    END IF;
END $$;
-- +goose StatementEnd
-- Restore the 00427 bodies FIRST. They are LANGUAGE sql, which PostgreSQL does not dependency-track,
-- so dropping run_fresh_resumes before this would leave every claim failing at runtime.
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
DROP TRIGGER fresh_resume_after_trg ON runs;
DROP TRIGGER a0_guard_fresh_resume_trg ON runs;
DROP FUNCTION fresh_resume_after();
DROP FUNCTION a0_guard_fresh_resume();
DROP FUNCTION fn_fresh_resume_run_base(runs, integer);
DROP TABLE run_fresh_resumes;
DROP FUNCTION run_fresh_resumes_write_once();
ALTER TABLE checkpoint_publish_attempts DROP COLUMN claim_generation;
ALTER TABLE runs
    DROP COLUMN checkpoint_tip_generation_tip,
    DROP COLUMN checkpoint_tip_generation,
    DROP COLUMN fresh_hold_version;
