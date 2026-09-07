-- +goose Up

-- Debounce counter backing M4's disk-pressure signal (PRD #837 M4). It counts how many
-- CONSECUTIVE fresh heartbeats have carried a volume at/above the pressure threshold, so
-- the poll can require a >=2-heartbeat streak before it declares disk_pressure — a single
-- spike or a transient statfs blip never fires the lifecycle action.
--
-- Write path:
--   * HeartbeatWorker INCREMENTS it (bounded, LEAST(..,100)) when the tick's sample is
--     over threshold, and RESETS it to 0 otherwise — so any under-threshold (or absent)
--     sample breaks the streak.
--   * RegisterWorker RESETS it to 0: a register is a fresh pod incarnation, and a prior
--     incarnation's streak must never carry forward into a newly-booted worker.
--   * The poll (ListHostedWorkersForController) DERIVES disk_pressure = streak>=2 AND the
--     worker is fresh (last_heartbeat_at within the heartbeat-stale cutoff); it never
--     writes this column.
--
-- DISPLAY/LIFECYCLE-ONLY, same contract as stats_disk_* (Decision 5): this column is a
-- read input for the poll's disk_pressure derivation ONLY, never for claim, run
-- assignment, or the sweeper. A hostile worker can drive its own value, so it must never
-- become a scheduling input; the display-only guard test allow-lists exactly the three
-- queries above.
ALTER TABLE workers
    ADD COLUMN stats_disk_pressure_streak int NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE workers
    DROP COLUMN stats_disk_pressure_streak;
