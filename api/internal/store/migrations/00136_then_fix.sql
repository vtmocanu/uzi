-- +goose Up

-- Chained fix (PRD #400 M5): `uzi handoff --review --then-fix`. This closes the
-- review→fix loop so a handoff can auto-apply the fixes its own diff-review found,
-- without a second human hand-off. The chain has THREE runs and two new columns wire
-- them together:
--
--   1. the ORIGINAL task (--then-fix stamps then_fix_requested=true on it at create);
--   2. the REVIEW run M4 auto-spawns when the original completes (review_target_run_id
--      points at the original) — it clones uzi/task/<orig>, diffs it, POSTs findings;
--   3. the FIX run THIS milestone adds: when the review run reaches its terminal
--      'completed' transition, maybeEnqueueThenFix reads the original's
--      then_fix_requested flag and the review's findings, composes them (untrusted
--      framing, the selfimprove precedent) into a NORMAL, auto-approved task run on the
--      SAME uzi/task/<orig> branch, which the existing worker implements and pushes.
--
-- then_fix_requested: set on the ORIGINAL task at create when --then-fix; read at the
-- REVIEW run's terminal transition (via the original it targets). NOT NULL DEFAULT false
-- so every existing / non-then-fix row reads false.
ALTER TABLE runs ADD COLUMN then_fix_requested boolean NOT NULL DEFAULT false;

-- then_fix_of_run_id: set ⇒ THIS run is a FIX run, and points at the ORIGINAL task it
-- fixes (provenance + dedup). NULL for every non-fix run. A fix run is otherwise a PLAIN
-- task (review_target_run_id NULL), so the worker implements-and-pushes it exactly like a
-- direct handoff — no new kind, no new worker code. ON DELETE CASCADE so deleting the
-- original removes its in-flight fix run too.
ALTER TABLE runs ADD COLUMN then_fix_of_run_id uuid REFERENCES runs(id) ON DELETE CASCADE;

-- At most ONE active fix run may exist per original task: the partial unique index makes
-- maybeEnqueueThenFix's auto-create idempotent (a duplicate raises 23505, read as "a fix
-- is already active"), the same posture the review index (00135) and the judge index
-- (00057/00058) take. Terminal fix runs (completed/failed/cancelled) are excluded so a
-- re-fix after one finishes is allowed.
CREATE UNIQUE INDEX uq_one_active_then_fix_per_target
    ON runs (then_fix_of_run_id)
    WHERE then_fix_of_run_id IS NOT NULL AND status NOT IN ('completed', 'failed', 'cancelled');

-- A fix run is a plain task: it satisfies the existing runs_kind_shape task clause
-- (repo_id set, issue_iid null, branch NOT NULL) unchanged, so no CHECK constraint is
-- touched here — then_fix_of_run_id is pure provenance metadata, not a shape discriminator.

-- +goose Down
DROP INDEX uq_one_active_then_fix_per_target;
ALTER TABLE runs DROP COLUMN then_fix_of_run_id;
ALTER TABLE runs DROP COLUMN then_fix_requested;
