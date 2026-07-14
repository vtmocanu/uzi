-- +goose Up

-- Latest container resource sample a worker self-reports on its heartbeat (PRD #49).
-- All nullable, overwritten every heartbeat INCLUDING back to NULL when a tick
-- carries no stats (downgrade / collector error), so a stale gauge self-clears and
-- freshness is simply last_heartbeat_at. cpu_pct is `real` (a percentage needs no
-- more precision); the two byte counts are bigint; source is the free enum
-- {cgroup, process} the handler validates before write.
--
-- DISPLAY-ONLY (Decision 5): stats_* are read ONLY by the worker DTOs / web gauges,
-- NEVER by claim, run assignment, or the sweeper. A hostile worker can report
-- anything, so these columns must never become a scheduling input — an M2 regression
-- test asserts no query outside HeartbeatWorker + the worker-list SELECTs references
-- stats_. Renumbered to the next free slot above the live head at landing time (repo
-- convention: the number here is a parallel-PRD draft, not the final version).
ALTER TABLE workers
    ADD COLUMN stats_cpu_pct         real,
    ADD COLUMN stats_mem_bytes       bigint,
    ADD COLUMN stats_mem_limit_bytes bigint,
    ADD COLUMN stats_source          text;

-- +goose Down
ALTER TABLE workers
    DROP COLUMN stats_cpu_pct,
    DROP COLUMN stats_mem_bytes,
    DROP COLUMN stats_mem_limit_bytes,
    DROP COLUMN stats_source;
