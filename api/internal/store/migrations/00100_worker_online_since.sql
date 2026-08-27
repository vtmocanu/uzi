-- +goose Up

-- online_since is the api-owned uptime anchor (PRD #251 M1): the timestamp at which
-- a worker most recently BECAME online, stamped by the liveness writes
-- (RegisterWorker/HeartbeatWorker) and cleared by the sweeper (MarkStaleWorkersOffline).
-- Nullable, no default, no backfill: an already-online worker with a NULL anchor
-- self-heals on its next heartbeat via the preserve-or-stamp CASE. DISPLAY-ONLY —
-- read only by the worker DTOs; no claim/scheduling/sweeper query reads it.
ALTER TABLE workers ADD COLUMN online_since timestamptz;

-- +goose Down
ALTER TABLE workers DROP COLUMN online_since;
