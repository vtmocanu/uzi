-- +goose Up

-- Issue #783: budget_paused_seconds is the accumulated wall-clock seconds a run spent
-- parked at a HUMAN GATE (awaiting_approval / awaiting_input). It is EXCLUDED from the
-- SweepRunningTimeout deadline so time spent waiting for a human to approve a plan or
-- answer a question does NOT consume the run's implementation budget — otherwise a
-- slowly-approved run false-fails with RUN_TIMEOUT. started_at is left untouched, so
-- run-duration display and the health baselines are unchanged.
ALTER TABLE runs ADD COLUMN budget_paused_seconds int NOT NULL DEFAULT 0 CHECK (budget_paused_seconds >= 0);

-- +goose Down

ALTER TABLE runs DROP COLUMN budget_paused_seconds;
