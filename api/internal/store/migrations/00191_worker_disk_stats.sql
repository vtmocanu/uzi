-- +goose Up

-- Latest per-volume disk sample a worker self-reports on its heartbeat (PRD #837 M1),
-- extending the CPU/memory columns 00064 added. Two volumes (the separate /nix and
-- /data PVCs), each a used-bytes and a total-bytes count. All nullable, overwritten
-- every heartbeat INCLUDING back to NULL when a tick carries no disk sample (dev/compose
-- has no /nix mount, or statfs failed), so a stale gauge self-clears and freshness is
-- simply last_heartbeat_at — the same discipline as the mem columns.
--
-- DISPLAY-ONLY (Decision 5): stats_disk_* are read ONLY by the worker DTOs / web gauges
-- (via the worker-list SELECTs' SELECT * expansion) and, from M4, by the poll query's
-- disk_pressure derivation for lifecycle — NEVER by claim, run assignment, or the
-- sweeper. A hostile worker can report anything, so these columns must never become a
-- scheduling input; a regression test asserts no query outside HeartbeatWorker (and, in
-- M4, the poll SELECT) references stats_disk_.
ALTER TABLE workers
    ADD COLUMN stats_disk_nix_bytes        bigint,
    ADD COLUMN stats_disk_nix_total_bytes  bigint,
    ADD COLUMN stats_disk_data_bytes       bigint,
    ADD COLUMN stats_disk_data_total_bytes bigint;

-- +goose Down
ALTER TABLE workers
    DROP COLUMN stats_disk_nix_bytes,
    DROP COLUMN stats_disk_nix_total_bytes,
    DROP COLUMN stats_disk_data_bytes,
    DROP COLUMN stats_disk_data_total_bytes;
