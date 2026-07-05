-- +goose Up

-- MR-close watcher (PRD #24): the last MR state this run's card watcher observed.
--
--   mr_state  the merge-request state (opened|closed|merged|locked) the watcher
--             last saw for runs.mr_iid. NULL = never observed (pre-migration runs
--             and any run before its first watch tick). The watcher is
--             edge-triggered: it moves the card only on an opened→closed or
--             closed→opened TRANSITION, so it must remember the prior state. A
--             NULL first observation records the state WITHOUT moving (Decision 9:
--             never act on stale close history we never saw happen).
--
-- This column is WATCHER-OWNED: only forgesvc's MR-state sync writes it
-- (SetRunMRState). No run-status path touches it — SetRunCompleted rewrites
-- mr_iid but never mr_state, and the requeue/sweep paths only touch non-terminal
-- runs. There is deliberately no backfill: existing closed MRs bootstrap to their
-- current state on the first tick and do not trigger a wave of moves.
ALTER TABLE runs ADD COLUMN mr_state text;

-- +goose Down
ALTER TABLE runs DROP COLUMN mr_state;
