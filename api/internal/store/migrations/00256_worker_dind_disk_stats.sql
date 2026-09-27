-- +goose Up

-- Latest docker-in-docker volume sample a docker-tier worker self-reports on its
-- heartbeat (issue #1759), alongside the /nix and /data columns 00191 added. The
-- dind-data volume holds the inner docker daemon's images, layers and build cache,
-- so it is metered in used/total bytes AND used/total inodes (layer-heavy stores
-- run out of inodes before bytes). All nullable, overwritten every heartbeat
-- INCLUDING back to NULL when a tick carries no dind sample (a non-docker worker,
-- or statfs failed), so a stale gauge self-clears and freshness is simply
-- last_heartbeat_at, the same discipline as the other stats_disk_ columns.
--
-- DISPLAY-ONLY: stats_disk_dind_* are read ONLY by the worker DTOs / web gauges
-- (via the worker-list SELECTs' SELECT * expansion). They are NEVER a disk_pressure
-- input (that stays nix/data only), and NEVER read by claim, run assignment, or the
-- sweeper. A hostile worker can report anything, so these columns must never
-- become a scheduling input.
ALTER TABLE workers
    ADD COLUMN stats_disk_dind_bytes        bigint,
    ADD COLUMN stats_disk_dind_total_bytes  bigint,
    ADD COLUMN stats_disk_dind_inodes       bigint,
    ADD COLUMN stats_disk_dind_total_inodes bigint;

-- +goose Down
ALTER TABLE workers
    DROP COLUMN stats_disk_dind_bytes,
    DROP COLUMN stats_disk_dind_total_bytes,
    DROP COLUMN stats_disk_dind_inodes,
    DROP COLUMN stats_disk_dind_total_inodes;
