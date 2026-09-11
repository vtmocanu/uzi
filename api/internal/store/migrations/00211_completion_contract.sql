-- +goose Up

-- PRD #1226 M1 (D1/D2): the structural completion interlock's additive schema. Four
-- columns land in ONE migration because they are one feature and must arrive atomically:
-- the three runs columns that carry a versioned frozen structural contract, and the
-- workers column that carries a worker's self-reported PROTOCOL capabilities. All are
-- ADDITIVE (ADD COLUMN only), so an N-1 worker reading these tables during a rolling
-- release is unaffected and check-migration-additive does NOT flag this Up section; there
-- is no CHECK on a live table here, so unlike 00204/00205 no NOT VALID/VALIDATE two-step is
-- needed. Every existing row is byte-unchanged: the three runs columns are NULLABLE with no
-- default (NULL is the legacy state), and workers.protocol_capabilities defaults to '{}'.

-- runs.completion_contract_version is the EXPLICIT legacy discriminator (D1): NULL = a
-- legacy run, NEVER enforced; a non-NULL version means the run is interlocked. CreateRun
-- stamps it to 1 when the rollout switch is on, BEFORE the first claim — so the hard claim
-- clause (D2) is not vacuous for the plan-phase worker. A run is NEVER exempted by milestone
-- cardinality; this NULL discriminator is the only exemption.
ALTER TABLE runs ADD COLUMN completion_contract_version int;

-- runs.contract_revision is set to 1 at freeze (D1). A permit later fences on it and #1227
-- bumps it. NULL until the contract is frozen.
ALTER TABLE runs ADD COLUMN contract_revision int;

-- runs.completion_contract is the FROZEN structural contract jsonb (D1), frozen together
-- with milestones_frozen at human approval / the first autopilot running report, idempotently.
-- Shape (pinned; the reserved null audit / empty finding_ids slots are filled by #1230/#1231):
--   {"profile":"structural","revision":1,
--    "criteria":[{"id":"m1.c1","milestone_id":"m1","text":"<milestone title>",
--                 "audit":null,"finding_ids":[]}]}
-- One criterion per entry in runs.milestones_frozen. The structural profile has no semantic
-- evidence. NULL until frozen.
ALTER TABLE runs ADD COLUMN completion_contract jsonb;

-- workers.protocol_capabilities is the worker-SELF-REPORTED set of PROTOCOL capabilities
-- (D2), kept DELIBERATELY SEPARATE from workers.capabilities and the scheduler vocabulary so
-- a protocol string (e.g. completion_interlock_v1) never appears in the user-facing
-- repo-capability picker. NOT NULL DEFAULT '{}' so every existing worker row reads as
-- "implements no protocol" — exactly what makes the hard claim clause block an old worker
-- from an interlocked run until a capable image self-reports the capability at register.
ALTER TABLE workers ADD COLUMN protocol_capabilities text[] NOT NULL DEFAULT '{}';

-- +goose Down

-- Drop the four columns in reverse add order. Down runs only on an explicit `goose down`,
-- never during a forward rolling release (store.Migrate only goes up), so these drops are
-- out of check-migration-additive's Up-only scope.
ALTER TABLE workers DROP COLUMN protocol_capabilities;
ALTER TABLE runs DROP COLUMN completion_contract;
ALTER TABLE runs DROP COLUMN contract_revision;
ALTER TABLE runs DROP COLUMN completion_contract_version;
