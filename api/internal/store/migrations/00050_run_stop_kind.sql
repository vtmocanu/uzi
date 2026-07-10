-- +goose Up

-- Deliberate-stop signal (PRD #33): a server-stamped record that a run was stopped
-- on purpose by a human, so the client never has to guess it from a free-text
-- failure_reason string (issue #15 item 3).
--
--   stop_kind  why a run stopped deliberately, or NULL for a run that stopped for
--              any other reason (a genuine failure, a timeout, or is still going).
--                'cancelled'     a cancel verdict (status ends 'cancelled', or
--                                'failed' when the agent reported "run cancelled").
--                'plan_rejected' a plan-reject verdict (status ends 'failed').
--
-- It is stamped SERVER-SIDE at the moment the server learns the intent — the
-- SubmitInput verdict branches (workersvc) — never derived from what reason string
-- the worker later reports. On the live-poller path it is stamped in the SAME
-- statement as the verdict's run_user_inputs insert (never a second, losable
-- write); the server-side no-poller paths stamp it alongside their status write.
-- The column is display-only: it does NOT change the run's status, the state
-- machine, or any claim/sweep gate (Decision 3). With it exposed, the client's
-- isStoppedRun drops its exact-string heuristic and becomes
--   status = 'cancelled' OR (status = 'failed' AND stop_kind IS NOT NULL)
-- — the 'failed' guard is load-bearing: the live path stamps while the run is
-- still non-terminal, so an unguarded read would render a still-running (or even a
-- completed, on a reject-then-approve race) run as "stopped" (Decision 4).
ALTER TABLE runs ADD COLUMN stop_kind text CHECK (stop_kind IN ('cancelled', 'plan_rejected'));

-- One-time backfill of the only two exact literals a deliberate stop ever persisted
-- (Decision 4): 'run cancelled' (agent-side, via the worker's failure path) and
-- 'plan rejected' (server-written at the no-poller reject). Historical rejects that
-- carried the user's verbatim reason are indistinguishable from real failures after
-- the fact and stay 'failed' with NULL stop_kind — an accepted, shrinking set.
-- Server-side cancels wrote status 'cancelled' with NO failure_reason and are
-- deliberately NOT matched here; isStoppedRun's 'cancelled' branch already covers them.
UPDATE runs
SET stop_kind = CASE failure_reason
    WHEN 'run cancelled' THEN 'cancelled'
    WHEN 'plan rejected' THEN 'plan_rejected'
    END
WHERE failure_reason IN ('run cancelled', 'plan rejected');

-- +goose Down
ALTER TABLE runs DROP COLUMN stop_kind;
