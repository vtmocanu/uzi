-- +goose Up

-- PRD #1809 M6 (D8): per-run disk size and data-volume inode usage, both self-reported on the
-- worker heartbeat and both DISPLAY-ONLY, the 00191 / 00256 discipline. Nothing claims, assigns,
-- sweeps or releases custody on these values: they feed the worker DTO, the run DTO's
-- home_bytes / cache_bytes, `uzi worker list`, the run page and the admin-health run-disk check.
--
--   1. workers.stats_disk_data_inodes / stats_disk_data_total_inodes: used and total inodes of the
--      data volume, beside the byte pair 00191 added. Written on EVERY heartbeat by
--      HeartbeatWorker, back to NULL when a tick carries no sample, so a stale gauge self-clears.
--
--   2. worker_run_disk: one row per (worker, run) the worker reported in its latest heartbeat's
--      run_disk list: the run's HOME bytes, its rebuildable-cache bytes, and whether the walk was
--      cut short (truncated: the sizes are then lower bounds). A heartbeat that CARRIES run_disk
--      (even an empty list) replaces the worker's whole set in one statement; a heartbeat that
--      omits it (an older worker, or a tick with no sample) leaves the rows untouched, and every
--      reader ignores rows whose sampled_at is older than 15 minutes, so a downgraded worker's
--      rows stop showing without a writer.
--
--      run_id references runs ON DELETE CASCADE: deleting a run drops its size rows, and the
--      replace statement inserts only run ids that exist AND belong to the worker's own owner, so
--      a garbled or hostile run id is dropped rather than failing the heartbeat's write.
--      worker_id cascades with the worker the same way.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
ALTER TABLE workers
    ADD COLUMN stats_disk_data_inodes       bigint,
    ADD COLUMN stats_disk_data_total_inodes bigint;

CREATE TABLE worker_run_disk (
    worker_id   uuid        NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    run_id      uuid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    home_bytes  bigint      NOT NULL CHECK (home_bytes >= 0),
    cache_bytes bigint      NOT NULL CHECK (cache_bytes >= 0),
    truncated   boolean     NOT NULL DEFAULT false,
    sampled_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (worker_id, run_id)
);

-- The run DTO reads by (worker_id, run_id) through the primary key; the run-id index serves the
-- ON DELETE CASCADE from runs.
CREATE INDEX worker_run_disk_run_id_idx ON worker_run_disk (run_id);

-- +goose Down
DROP TABLE worker_run_disk;

ALTER TABLE workers
    DROP COLUMN stats_disk_data_inodes,
    DROP COLUMN stats_disk_data_total_inodes;
