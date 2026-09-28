-- +goose Up

-- PRD #1810 M1 (D1): a published checkpoint ref (refs/uzi-checkpoints/<branch>) is RETAINED
-- on origin while its run has an open custody hold, instead of being deleted best-effort on
-- every terminal transition. This table is the durable record of each terminal run's
-- checkpoint ref and what the api has done (or still owes) to it. Purely ADDITIVE: two
-- brand-new tables, no change to any worker-facing table.
--
-- One row per run (run_id PRIMARY KEY). The row is inserted at the run's terminal
-- transition, backfilled by the sweep reconciler for a terminal run with none, or
-- inserted by TrackTerminalCheckpointPublish for a terminal run that still has none
-- when one of its checkpoint publishes lands AFTER the terminal transition (e.g. its
-- shutdown checkpoint). A later terminal call for the same run (a duplicate report, a
-- second cancel path) finds the existing row and leaves its state alone; but the row
-- is not otherwise frozen there, a later publish of the SAME run can still advance or
-- reopen it (TrackTerminalCheckpointPublish, AdvanceCheckpointRetentionTip), and a
-- supersession (another run needing the branch slot) moves it through `superseding`
-- to `superseded` when a custody hold is still open, or straight to `settling` when
-- none is.
CREATE TABLE checkpoint_retentions (
    -- Owner/run/repo are PLAIN columns, NOT ON DELETE CASCADE FKs (the rationale
    -- recovery_custody_holds records in 00223): a run, repo or owner delete must not
    -- silently drop the record of a ref that may still exist on the forge.
    run_id uuid PRIMARY KEY,
    user_id uuid NOT NULL,
    repo_id uuid NOT NULL,
    -- The run's checkpoint branch (agent/issue-<iid> or uzi/self-improve/<run id>),
    -- derived server-side from the run row, never from a worker string.
    branch text NOT NULL,
    -- The tip the run last published (runs.checkpoint_tip at the terminal transition).
    -- Every forge write against the ref is compare-and-swap on this value.
    tip text NOT NULL,
    -- The ref that currently carries the tip: refs/uzi-checkpoints/<branch> at first,
    -- the recovery ref once supersession (M3) has moved it.
    ref text NOT NULL,
    -- refs/uzi-recovery/<run id> once supersession (M3) has begun; NULL before.
    recovery_ref text,
    -- Lifecycle:
    --   retained     the run has an open custody hold; the ref is kept on origin.
    --   superseding  a new run on the branch needs the slot; the recovery ref is being
    --                created and the branch ref deleted (M3). Intent persisted BEFORE any
    --                forge write, so a crash leaves the tip discoverable.
    --   superseded   the tip lives under recovery_ref (now also in ref); the branch slot
    --                is free. Still owed a delete once the run's last hold settles (M4).
    --   settling     no hold is open; the ref named in `ref` is owed a CAS delete at `tip`.
    --   deleted      the delete succeeded (or the ref was already gone / had moved).
    --   abandoned    the run's repo/forge context is gone, so the delete can never be
    --                brokered; kept as an audit row.
    state text NOT NULL CHECK (state IN ('retained', 'superseding', 'superseded', 'settling', 'deleted', 'abandoned')),
    -- Forge-write retry bookkeeping: attempts counts failed forge writes, next_attempt_at
    -- is the earliest time a retry may run (backoff computed by the caller), last_error
    -- the scrubbed last failure.
    attempts int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error text,
    -- Set on a delete of a recovery ref: when the reconciler should re-verify the ref is
    -- gone, and when it did (M4).
    verify_after timestamptz,
    verified_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    settled_at timestamptz
);

-- The branch-slot lookup (M3 supersession): the active record(s) holding a branch.
CREATE INDEX idx_checkpoint_retentions_branch_active
    ON checkpoint_retentions (repo_id, branch)
    WHERE state IN ('retained', 'superseding', 'settling');

-- The reconciliation sweeper's candidate scan (M4).
CREATE INDEX idx_checkpoint_retentions_work
    ON checkpoint_retentions (state, next_attempt_at)
    WHERE state IN ('superseding', 'settling', 'retained', 'superseded');

-- The post-settlement audit's candidate scan (M4): deleted records that named a recovery ref and
-- are not yet verified, by due time. Every other deleted record (the steady-state bulk) is outside
-- the index, so the audit's page never walks them.
CREATE INDEX idx_checkpoint_retentions_audit
    ON checkpoint_retentions (verify_after)
    WHERE state = 'deleted' AND verified_at IS NULL AND recovery_ref IS NOT NULL;

-- Singleton: when retention was enabled. Bounds the sweeper's later backfill (M4) of
-- terminal runs that have no row (the best-effort insert failed) to runs that ended after
-- this instant, so it never reaches back to runs the old delete-on-terminal path handled.
-- backfilled_through is the backfill's persisted watermark. What it guarantees: every candidate
-- whose backfill key (GREATEST(status_since, checkpoint_tip_at)) is below it AND that was
-- committed when the proving page was listed had a record then. A candidate whose transaction
-- committed after that list, with a key below the watermark, is not covered by the watermark
-- itself: a later pass scans from the watermark less a 10-minute overlap, which catches it only
-- if it committed within 10 minutes of its key (commit-order skew). A candidate committed later
-- than that stays unrecorded by the backfill. NULL until the first unconfined pass.
CREATE TABLE checkpoint_retention_meta (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    enabled_at timestamptz NOT NULL,
    backfilled_through timestamptz
);
INSERT INTO checkpoint_retention_meta (id, enabled_at) VALUES (true, now());

-- +goose Down
DROP TABLE checkpoint_retention_meta;
DROP TABLE checkpoint_retentions;
