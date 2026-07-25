-- +goose Up

-- A THIRD stop_kind (PRD #108 M5). 'cancelled' and 'plan_rejected' (00050) are
-- both deliberate HUMAN stops; 'auto_stopped' is the SERVER stopping a run whose
-- message writes are in a confirmed permanent-failure loop — five guards, all of
-- which must hold, one of which is "other runs are succeeding on this instance".
--
-- It is stamped by the same two mechanisms 00050 documents, and for the same
-- reason (the stamp must never be losable independently of what caused it):
--   * live-poller path — inside CreateStopVerdictInput's data-modifying CTE,
--     alongside the synthetic 'cancel' input the worker's steering poll consumes;
--   * no-poller path  — alongside the status write in FailRunAutoStop.
--
-- 🔴 IT IS NOT A HUMAN STOP, AND CONSUMERS MUST NOT FOLD IT IN WITH THE OTHER TWO.
-- web's isStoppedRun styles a stop calm and neutral because a deliberate stop is
-- not breakage; an auto-stop IS breakage and its owner needs to see it. That
-- predicate reads `stop_kind IS NOT NULL`, so adding a value here silently
-- retunes it — isStoppedRun is narrowed to the two human kinds in this same MR.
-- Any FOURTH value must make that choice deliberately rather than inherit it.
--
-- stop_kind stays display-only: it does not change the run's status, the state
-- machine, or any claim/sweep gate (00050, PRD #33 Decision 3).
ALTER TABLE runs DROP CONSTRAINT runs_stop_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped'));

-- +goose Down

-- NOT symmetric-safe, deliberately: if any row already carries 'auto_stopped' the
-- re-added constraint fails and this migration refuses to come down. That is the
-- correct outcome — a down-migration that silently stranded rows violating the
-- constraint it just installed would be worse than one that stops. Goose downs
-- are not run in this deployment (store.Migrate only ever goes up).
ALTER TABLE runs DROP CONSTRAINT runs_stop_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected'));
