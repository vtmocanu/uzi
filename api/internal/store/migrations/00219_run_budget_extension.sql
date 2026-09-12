-- +goose Up

-- PRD #1189 M1: budget_extension_seconds is human-granted wall-clock time added ON TOP of
-- the frozen, already-capped budget_wall_seconds. It is ADDED to the SweepRunningTimeout
-- deadline, to RunDeadline, to the health arm's effective timeout AND to the worker's own
-- armed wall, so an owner can give a run more time without mutating the immutable budget.
-- budget_wall_seconds stays capped at 8h at the freeze writers (the ceiling bounds what a
-- lead can buy itself); this extension is a DIFFERENT trust — a human grants it — so it lives
-- in its own additive column and is deliberately OUTSIDE that ceiling. started_at is left
-- untouched, so run-duration display and the health baselines are unchanged.
--
-- The nonnegativity CHECK is added SEPARATELY as NOT VALID and validated in 00220, not inline
-- on the ADD COLUMN: an inline CHECK is created already-valid and, on a large live runs table,
-- has PostgreSQL scan every row under the ACCESS EXCLUSIVE lock the ALTER already holds. The
-- two-step (add NOT VALID, then VALIDATE CONSTRAINT under a lock-cheap SHARE UPDATE EXCLUSIVE
-- scan) mirrors how run_user_inputs_kind_check is widened just below.
ALTER TABLE runs ADD COLUMN budget_extension_seconds int NOT NULL DEFAULT 0;
ALTER TABLE runs ADD CONSTRAINT runs_budget_extension_seconds_check
    CHECK (budget_extension_seconds >= 0) NOT VALID;

-- run_user_inputs.kind: a THIRTEENTH steering-input kind, 'extend' — the audit row an
-- 'extend' steering input writes. The twelve existing values are carried VERBATIM from the
-- LIVE constraint (last widened in 00216_completion_decision_kind.sql, which added
-- 'completion_decision' on top of 00204_run_paused.sql's pause/pause_cancel/resume). 'extend'
-- is a SERVER-ONLY audit kind (its control travels via runs.budget_extension_seconds on the
-- ACK/claim, never through the ConsumeRunInputs queue), EXCLUDED from ConsumeRunInputs like
-- 'scope'. Re-deriving the list from anything but the live constraint silently deletes whatever
-- it forgets (00092 documents exactly that failure) — 'completion_decision' MUST be kept.
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
-- Added NOT VALID: skip the validating table scan (and the ACCESS EXCLUSIVE lock it would
-- otherwise hold to check every existing row) at add-time; new/updated rows are still
-- enforced. 00220 runs VALIDATE CONSTRAINT to confirm the backlog under a lock-cheap scan.
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope', 'pause', 'pause_cancel', 'resume', 'completion_decision', 'extend')) NOT VALID;

-- +goose Down

-- The narrowing is best-effort and DATA-DEPENDENT, mirroring 00204's honesty: a re-added
-- narrower CHECK FAILS if any row already holds a kind='extend' value, and this migration
-- then refuses to come down — the correct outcome, since a down that silently stranded rows
-- violating the constraint it just installed would be worse. The re-added CHECK is therefore
-- VALIDATED (not NOT VALID): a NOT VALID re-add would succeed while leaving stranded
-- kind='extend' rows under a constraint that claims to forbid them. Goose downs are not run in
-- this deployment (store.Migrate only ever goes up); drain first if you must.
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope', 'pause', 'pause_cancel', 'resume', 'completion_decision'));

-- Dropping the column drops runs_budget_extension_seconds_check with it.
ALTER TABLE runs DROP COLUMN IF EXISTS budget_extension_seconds;
