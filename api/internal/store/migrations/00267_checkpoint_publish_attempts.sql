-- +goose Up

-- PRD #1810 D2 (residual 2): a DURABLE record of every checkpoint push the api is about to send,
-- written BEFORE the forge call. A push whose outcome the api never learned (a client timeout, a
-- dropped connection) may still update the branch ref on origin later: a receive-pack request the
-- forge already accepted is not cancelled by the client giving up. Such a late update can land
-- after the run went terminal and after a newer run superseded its retention record, leaving a
-- branch ref no retention record tracks, which blocks every later publish on the branch.
--
-- A row lives from just before its push until the push's outcome is ACCOUNTED FOR: removed at once
-- when the forge definitively refused the push (nothing landed), or when the push succeeded and
-- its tip was persisted (runs.checkpoint_tip) and tracked by the run's retention record. A row
-- whose outcome is unknown, or whose landed tip no record tracks, stays: the sweeper's attempts
-- arm (ReconcileCheckpointRetentions) compares the branch ref on origin with it once the run is
-- terminal and, under the run's retention lock, either re-records the tip on the run's record
-- (compare-and-set on the record and runs.checkpoint_tip it read before listing origin) or, once
-- no custody hold of the run is open, CAS-deletes the branch ref at exactly that tip (when the
-- run's slot was handed to a newer run). While a custody hold is open that ref is kept, so the
-- newer run's checkpoints are refused (not_descendant) until custody releases: the same cost the
-- supersession's stuck exit accepts, since the ref may hold the only copy of the held work.
--
-- The publish's own tip persist and record track are compare-and-set on what the push observed
-- immediately before its forge call, so writes that arrive late never move either backwards over
-- a newer publish or the sweeper's re-record; a push whose writes move nothing keeps its row.
--
-- Not covered: an attempt whose run is gone is dropped by the sweeper, which then has no forge
-- coordinates to act with, and an attempt origin never shows is retired after the sweeper's
-- horizon (seven days). A push the forge applies after either still leaves a branch ref no record
-- tracks, until a human deletes it.
--
-- Plain columns, no ON DELETE CASCADE FK (the 00264 rationale): a run delete must not silently
-- drop the record of a push that may still land. Purely additive.
CREATE TABLE checkpoint_publish_attempts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    -- The run's checkpoint branch and ref, derived server-side exactly as Publish derives them.
    branch text NOT NULL,
    ref text NOT NULL,
    -- The tip the push would set the ref to (the tip the worker declared; the broker pushes it
    -- verbatim or refuses).
    tip text NOT NULL,
    attempted_at timestamptz NOT NULL DEFAULT now(),
    -- Sweeper bookkeeping: the earliest next comparison, how many were made, the last problem.
    next_check_at timestamptz NOT NULL DEFAULT now(),
    checks int NOT NULL DEFAULT 0,
    last_error text
);

-- The run-scoped lookups (a supersession's post-delete list, the stuck exit).
CREATE INDEX idx_checkpoint_publish_attempts_run ON checkpoint_publish_attempts (run_id, tip);
-- The sweeper's due scan.
CREATE INDEX idx_checkpoint_publish_attempts_due ON checkpoint_publish_attempts (next_check_at);

-- +goose Down
DROP TABLE checkpoint_publish_attempts;
