-- +goose Up

-- Issue #1994: a worker re-lists a journaled-but-undelivered outcome as terminal_pending on every
-- heartbeat, so the lease renews forever and nothing measured how long the outcome had been stuck.
-- terminal_pending_since is the FIRST-SEEN time of a pending entry for one (worker, run, claim
-- generation): ReplaceWorkerActiveRuns carries the prior value forward across renewals (and across
-- a register) while the same generation stays listed pending, and resets it (NULL, or now() on the
-- next pending listing) when the entry clears, is omitted, or the generation changes. NULL for a
-- live (non-pending) row and for rows written before this column existed. It is display and
-- health-signal only: no claim, requeue, fail, park or orphan predicate reads it.
ALTER TABLE worker_active_runs ADD COLUMN terminal_pending_since TIMESTAMPTZ NULL;

-- +goose Down

ALTER TABLE worker_active_runs DROP COLUMN IF EXISTS terminal_pending_since;
