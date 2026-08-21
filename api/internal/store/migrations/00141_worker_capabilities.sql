-- +goose Up

-- Worker capability set (PRD #84 M1). The server-authoritative list of capabilities
-- this worker CAN run, the validated union of what the worker self-reports (today
-- only {docker}) and what its image template implies (jvm → {jvm}). Every value is
-- Filter-ed against the server-owned vocabulary (capability.Filter) before it lands
-- here, so an unknown/garbled worker report can never introduce a novel name. This
-- column is authoritative so a later milestone's peer subquery can read it directly.
--
-- NOT NULL DEFAULT '{}' so every existing row (and every older worker that self-reports
-- nothing) reads as the empty set rather than NULL — "no capabilities" is a concrete
-- fact, not unknown, and it keeps the matcher from having to special-case NULL.
ALTER TABLE workers ADD COLUMN capabilities text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE workers DROP COLUMN capabilities;
