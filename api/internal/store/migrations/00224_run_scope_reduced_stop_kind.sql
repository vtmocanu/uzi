-- +goose Up

-- PRD #1227 M2 (D2): widen runs.stop_kind with 'scope_reduced' — the terminal disposition
-- for a completion whose frozen contract carries an owner-deferred (out-of-scope) set, stamped
-- SERVER-SIDE when the owner's `partial` decision revised the contract. It is DISTINCT from
-- #634's 'scope_capped' (an operator scope-ceiling truncation); the meaning of 'scope_capped' is
-- NOT widened. NOTE: scope_reduced is a 'completed'-status disposition, NOT a failed transition,
-- so — exactly like scope_capped (00162) — it is deliberately NOT added to runs_fail_origin_check.
-- The five existing values are carried VERBATIM from the LIVE constraint (last widened by
-- 00162_run_scope_steering.sql's Up, which added 'scope_capped'), then 'scope_reduced' is appended.
-- Re-deriving the list from anything but the live constraint silently deletes whatever it forgets
-- (00092 documents exactly that failure), so it is copied, not hand-typed.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_stop_kind_check;
-- Added NOT VALID: skip the validating full-table scan (and the ACCESS EXCLUSIVE lock it would
-- hold to check every existing row) at add-time; new/updated rows are still enforced. 00225 runs
-- VALIDATE CONSTRAINT to confirm the backlog under a lock-cheap scan, the same two-step
-- 00216/00217 used.
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped', 'stopped', 'scope_capped', 'scope_reduced')) NOT VALID;

-- +goose Down

-- Narrowing is best-effort and DATA-DEPENDENT, mirroring 00162's Down honesty: the re-added
-- narrower CHECK is VALIDATED (no NOT VALID), so it scans existing rows and FAILS if any row
-- already holds 'scope_reduced', and this migration then refuses to come down — the correct
-- outcome, since a down that silently stranded rows violating the constraint it just installed
-- would be worse. Goose downs are not run in this deployment (store.Migrate only ever goes up);
-- drain first if you must. The validated re-add restores the pre-'scope_reduced' state — validated,
-- exactly as 00162 left the pre-#1227 list. Goose runs 00225's Down first (re-adding the
-- 'scope_reduced' list NOT VALID), then this drops and re-adds the narrower list validated.
ALTER TABLE runs DROP CONSTRAINT runs_stop_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped', 'stopped', 'scope_capped'));
