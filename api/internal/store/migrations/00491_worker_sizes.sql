-- +goose Up

-- Admin-managed hosted-worker size table (PRD #4213). No seed rows here: the four
-- shipped sizes are inserted and refreshed at boot by store.ReconcileBuiltinWorkerSizes
-- (the builtin agent-template pattern), so a release that changes a shipped value
-- reaches rows an admin never edited without a re-run-unsafe SQL seed.
--
-- Integers only: CPU in millicores, memory in MiB, disks in GiB. The api never holds
-- a kube quantity string. The CHECKs are the schema-level backstop for the absolute
-- sanity caps; the handler-level validation (later milestones) adds the chart ceilings.
CREATE TABLE worker_sizes (
    name text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9-]{0,15}$'),
    cpu_request_millicores bigint NOT NULL CHECK (cpu_request_millicores > 0 AND cpu_request_millicores <= 64000),
    cpu_limit_millicores bigint NOT NULL CHECK (cpu_limit_millicores > 0 AND cpu_limit_millicores <= 64000),
    memory_request_mib bigint NOT NULL CHECK (memory_request_mib > 0 AND memory_request_mib <= 524288),
    memory_limit_mib bigint NOT NULL CHECK (memory_limit_mib > 0 AND memory_limit_mib <= 524288),
    data_gib bigint NOT NULL CHECK (data_gib > 0 AND data_gib <= 2048),
    dind_data_gib bigint NOT NULL CHECK (dind_data_gib > 0 AND dind_data_gib <= 2048),
    display_order int NOT NULL,
    builtin boolean NOT NULL DEFAULT false,
    customized boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
    CHECK (cpu_request_millicores <= cpu_limit_millicores AND memory_request_mib <= memory_limit_mib),
    CHECK (builtin OR NOT customized)
);

-- The controller's admin-size capability (DesiredWorker.Resources support), recorded
-- from fleet status reports beside the hibernation advertisement (00438), stamped with
-- the api's own receipt clock. NULL means no controller has reported one yet.
ALTER TABLE controller_report_status
    ADD COLUMN admin_sizes_version int,
    ADD COLUMN admin_sizes_advertised_at timestamptz;

-- +goose Down
-- Down DESTROYS admin edits and every custom size (the whole table is dropped) and the
-- recorded capability. Downgrading the api across this migration is unsupported: an older
-- api resolves sizes from a compiled-in list and would not know a worker whose size only
-- existed here.
ALTER TABLE controller_report_status
    DROP COLUMN admin_sizes_advertised_at,
    DROP COLUMN admin_sizes_version;
DROP TABLE worker_sizes;
