-- +goose Up

-- PRD #1906 M5: the server-set isolated-lane marker on hosted workers (Decision 9).
--
-- A profile-bound run (runs.egress_profile_id IS NOT NULL) may be claimed ONLY by a worker
-- the api itself provisioned into the isolated lane, and a lane worker claims ONLY
-- profile-bound runs. ClaimRun keys that two-way clause on this column plus the
-- isolated_fetch_v1 protocol capability. The column is written by exactly one statement,
-- CreateEphemeralHostedWorker, from the provisioner's own decision (the run it binds is
-- profile-bound); no registration, heartbeat or worker PATCH input reaches it, so a worker
-- can never mark itself as a lane worker.
ALTER TABLE workers ADD COLUMN isolated_lane boolean NOT NULL DEFAULT false;

-- A lane worker is always an api-provisioned (hosted), run-bound (ephemeral) worker: the
-- lane's pods live in their own namespace and are thrown away with their run (PRD #529),
-- so no file from one profile-bound run is visible to the next. Same explicit-OR shape as
-- ck_workers_hosted_metadata (00066): a non-lane row passes, a lane row must be both.
ALTER TABLE workers ADD CONSTRAINT ck_workers_isolated_lane CHECK (
    NOT isolated_lane
 OR (kind = 'hosted' AND ephemeral)
);

-- +goose Down
ALTER TABLE workers DROP CONSTRAINT ck_workers_isolated_lane;
ALTER TABLE workers DROP COLUMN isolated_lane;
