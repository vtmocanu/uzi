-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Issue #4479: an interactive task run that parks at awaiting_followup and is then finalized by the
-- worker (idle timeout or owner stop) must re-enter 'running' on the server before it can enroll a
-- publication intent. The worker's followup_finalize report does that (AdmitRunFollowupFinalize).
-- These columns and triggers make the admission durable and race-free against both an owner's
-- follow-up submission and a delayed park report.
--
-- followup_park_seq: a per-run counter of distinct parks. The BEFORE UPDATE trigger below bumps it
-- each time the run ENTERS awaiting_followup (from any other status) or re-parks with a higher
-- open_followup_id (a lost-wake re-park); a retried report of the same park keeps its value. The
-- worker echoes the value from the park ACK in its finalize report, so the admission applies only
-- to the park the worker actually observed. DEFAULT 0 = never parked (or parked by an older api).
-- followup_finalize_generation: the claim_generation the finalize admission was granted for. It is
-- consulted only together with claim_generation and claim_released_at (a later claim makes it stale
-- without any write), so nothing needs to clear it.
ALTER TABLE runs
    ADD COLUMN followup_park_seq bigint NOT NULL DEFAULT 0,
    ADD COLUMN followup_finalize_generation bigint;

-- Park identity and the no-re-park guard. WHEN is deliberately narrow: only updates that leave or
-- enter awaiting_followup fire it, so the many other runs writes pay nothing.
--   * After the finalize admission for the CURRENT, unreleased claim, a status write INTO
--     awaiting_followup is skipped (RETURN NULL: the row is not updated at all). A delayed park
--     report, from the new api or from an old api pod running the unchanged SetRunAwaitingFollowup
--     statement, then matches 0 rows and the run stays running. Writes that leave awaiting_followup
--     are never skipped.
--   * Otherwise a transition into awaiting_followup, or a re-park with a higher open_followup_id,
--     increments the sequence.
-- Ordering: BEFORE UPDATE row triggers on runs fire in trigger-name order. This one sorts after
-- completed_publication_run_guard, enforce_gated_plan_approval, guard_checkpoint_coordinates_trg,
-- guard_publication_resume_trg, normalize_recovery_wait_phase_trg and the runs_credential_switch_*
-- pair, which only validate or normalize NEW (no writes to other rows), so skipping the row after
-- them discards nothing but their in-memory edits; it sorts before runs_source_publication_intent_guard,
-- which is then not run for the skipped row, and no AFTER trigger fires for a skipped row.
-- +goose StatementBegin
CREATE FUNCTION runs_followup_park_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'awaiting_followup' AND OLD.status <> 'awaiting_followup'
       AND OLD.followup_finalize_generation IS NOT NULL
       AND OLD.followup_finalize_generation = OLD.claim_generation
       AND OLD.claim_released_at IS NULL THEN
        RETURN NULL;
    ELSIF NEW.status = 'awaiting_followup'
          AND (OLD.status <> 'awaiting_followup'
               OR COALESCE(NEW.open_followup_id, 0) > COALESCE(OLD.open_followup_id, 0)) THEN
        NEW.followup_park_seq := OLD.followup_park_seq + 1;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER runs_followup_park_identity_trg BEFORE UPDATE ON runs
FOR EACH ROW WHEN (NEW.status = 'awaiting_followup' OR OLD.status = 'awaiting_followup')
EXECUTE FUNCTION runs_followup_park_identity();

-- An owner follow-up for an interactive task whose finalize admission is live must not be accepted:
-- the worker is committing to finalize and would never deliver it. The insert takes the run row lock
-- (the same lock the admission statement holds), so exactly one of the two commits first: a
-- follow-up committed first is seen by the admission (followup_pending), and one arriving after the
-- admission is refused here. SQLSTATE UZ415 is this guard's own (the UZ413/UZ414 convention); the
-- service maps it to ErrFollowUpFinalizing.
-- +goose StatementBegin
CREATE FUNCTION run_user_inputs_followup_finalize() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r runs%ROWTYPE;
BEGIN
    SELECT * INTO r FROM runs WHERE id = NEW.run_id FOR UPDATE;
    IF FOUND AND r.kind = 'task' AND r.interactive
       AND r.followup_finalize_generation IS NOT NULL
       AND r.followup_finalize_generation = r.claim_generation
       AND r.claim_released_at IS NULL THEN
        RAISE EXCEPTION 'run is finishing; follow-up refused' USING ERRCODE = 'UZ415';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER run_user_inputs_followup_finalize_trg BEFORE INSERT ON run_user_inputs
FOR EACH ROW WHEN (NEW.kind = 'follow_up')
EXECUTE FUNCTION run_user_inputs_followup_finalize();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER IF EXISTS run_user_inputs_followup_finalize_trg ON run_user_inputs;
DROP FUNCTION IF EXISTS run_user_inputs_followup_finalize();
DROP TRIGGER IF EXISTS runs_followup_park_identity_trg ON runs;
DROP FUNCTION IF EXISTS runs_followup_park_identity();
ALTER TABLE runs
    DROP COLUMN IF EXISTS followup_finalize_generation,
    DROP COLUMN IF EXISTS followup_park_seq;
