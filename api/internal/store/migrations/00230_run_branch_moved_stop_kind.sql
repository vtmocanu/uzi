-- +goose Up

-- Issue #1117: widen runs.stop_kind with 'branch_moved' — the terminal disposition an
-- mr_rework run maps to when its finalize push is rejected non-fast-forward because a
-- concurrent same-branch writer (a human / uzi-watcher landing review fixes) advanced the
-- MR's agent/issue-* branch under it. It is a 'cancelled'-status disposition (a benign,
-- expected race, NOT an agent failure), so — exactly like scope_capped (00162) and
-- scope_reduced (00224) — it is deliberately NOT added to runs_fail_origin_check.
-- The six existing values are carried VERBATIM from the LIVE constraint (last widened by
-- 00224_run_scope_reduced_stop_kind.sql's Up, which added 'scope_reduced'), then
-- 'branch_moved' is appended. Re-deriving the list from anything but the live constraint
-- silently deletes whatever it forgets (00092 documents exactly that failure), so it is
-- copied, not hand-typed.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_stop_kind_check;
-- Added NOT VALID: skip the validating full-table scan (and the ACCESS EXCLUSIVE lock it would
-- hold to check every existing row) at add-time; new/updated rows are still enforced. 00231 runs
-- VALIDATE CONSTRAINT to confirm the backlog under a lock-cheap scan, the same two-step
-- 00224/00225 used.
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped', 'stopped', 'scope_capped', 'scope_reduced', 'branch_moved')) NOT VALID;

-- +goose Down

-- Narrowing is best-effort and DATA-DEPENDENT, mirroring 00224's Down honesty: the re-added
-- narrower CHECK is VALIDATED (no NOT VALID), so it scans existing rows and FAILS if any row
-- already holds 'branch_moved', and this migration then refuses to come down — the correct
-- outcome, since a down that silently stranded rows violating the constraint it just installed
-- would be worse. Goose downs are not run in this deployment (store.Migrate only ever goes up);
-- drain first if you must. The validated re-add restores the pre-'branch_moved' state — validated,
-- exactly as 00224 left the pre-#1117 list. Goose runs 00231's Down first (re-adding the
-- 'branch_moved' list NOT VALID), then this drops and re-adds the narrower list validated.
ALTER TABLE runs DROP CONSTRAINT runs_stop_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped', 'stopped', 'scope_capped', 'scope_reduced'));
