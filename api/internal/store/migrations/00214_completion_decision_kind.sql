-- +goose Up

-- PRD #1226 M5 (D7): widen run_user_inputs.kind with 'completion_decision' — the
-- owner/admin continue-decision the dedicated /runs/{id}/completion/decision endpoint
-- writes as an AUDIT row (it is EXCLUDED from ConsumeRunInputs, like 'resume'; the
-- decision's control travels via the transition + a separate guidance follow_up, never
-- the worker's raw steering drain). The eleven existing values are carried VERBATIM from
-- the LIVE constraint (last widened by 00204_run_paused.sql's Up + validated by
-- 00205_validate_run_paused_checks.sql), then 'completion_decision' is appended.
-- Re-deriving the list from anything but the live constraint silently deletes whatever it
-- forgets (00092 documents exactly that failure), so it is copied, not hand-typed.
ALTER TABLE run_user_inputs DROP CONSTRAINT IF EXISTS run_user_inputs_kind_check;
-- Added NOT VALID: skip the validating full-table scan (and the ACCESS EXCLUSIVE lock it
-- would hold to check every existing row) at add-time; new/updated rows are still
-- enforced. 00215 runs VALIDATE CONSTRAINT to confirm the backlog under a lock-cheap scan,
-- the same two-step 00204/00205 used.
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope', 'pause', 'pause_cancel', 'resume', 'completion_decision')) NOT VALID;

-- +goose Down

-- Narrowing is best-effort and DATA-DEPENDENT, mirroring 00204's Down: the re-added
-- narrower CHECK FAILS if any row already holds 'completion_decision', and this migration
-- then refuses to come down — the correct outcome, since a down that silently stranded
-- rows violating the constraint it just installed would be worse. Goose downs are not run
-- in this deployment (store.Migrate only ever goes up); drain first if you must. Re-added
-- NOT VALID (not validated), so the pre-'completion_decision' list is restored in the same
-- shape 00215's Down leaves the widened one — goose runs 00215's Down first, re-adding the
-- 'completion_decision' list NOT VALID, then this drops and re-adds the narrower list NOT VALID.
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope', 'pause', 'pause_cancel', 'resume')) NOT VALID;
