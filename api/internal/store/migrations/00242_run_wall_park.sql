-- +goose Up

-- PRD #1497 M1: park a run at its wall-clock limit instead of failing it. Three schema
-- changes ride ONE migration because they are one feature and must land atomically: the
-- new 'wall' pause mode the system-authored request carries, the finalize allowance the
-- Stop action grants, and the released-incarnation pair a server-side park captures to
-- fence the old flight and bar it from the next claim (D19). 00243 validates the two
-- CHECKs added NOT VALID below.
--
-- NOTE (goose numbering): drafted as 00242, immediately after the live head 00240;
-- renumber the PAIR above the live head together at landing via `task migration:renumber`
-- if another migration lands first.

-- (a) runs_pause_mode_check: widen to a THIRD value 'wall'. The two existing values are
-- carried VERBATIM from the LIVE constraint (00204's Up: 'milestone', 'now') — a DROP+ADD
-- that re-derives the list from anything but the live constraint silently deletes whatever
-- it forgets (00092 documents exactly that failure). 'wall' is the system-authored pause
-- mode the timeout sweep stamps when a run reaches its deadline.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_pause_mode_check;
-- Added NOT VALID: skip the validating table scan (and the ACCESS EXCLUSIVE lock it would
-- otherwise hold to check every existing row) at add-time; new/updated rows are still
-- enforced. 00243 runs VALIDATE CONSTRAINT to confirm the backlog under a lock-cheap scan.
ALTER TABLE runs ADD CONSTRAINT runs_pause_mode_check
    CHECK (pause_mode IS NULL OR pause_mode IN ('milestone', 'now', 'wall')) NOT VALID;

-- (b) runs.budget_finalize_seconds: the fixed 1800s finalize allowance the Stop action
-- grants once (D9). It lives in its own column, OUTSIDE budget_extension_seconds, so the
-- owner extension cap never sees it, and the column doubles as the once-only marker (Stop
-- refuses when it is already non-zero). NOT NULL DEFAULT 0 leaves every existing row's
-- deadline unchanged. The nonnegativity CHECK is added SEPARATELY as NOT VALID and
-- validated in 00243, not inline on the ADD COLUMN: an inline CHECK is created
-- already-valid and, on a large live runs table, has PostgreSQL scan every row under the
-- ACCESS EXCLUSIVE lock the ALTER already holds. Mirrors 00219's two-step for
-- budget_extension_seconds.
ALTER TABLE runs ADD COLUMN budget_finalize_seconds int NOT NULL DEFAULT 0;
ALTER TABLE runs ADD CONSTRAINT runs_budget_finalize_seconds_check
    CHECK (budget_finalize_seconds >= 0) NOT VALID;

-- (c) runs.released_worker_id / runs.released_worker_nonce: together they name one worker
-- PROCESS INCARNATION to exclude from the next claim (D19). The server park captures them
-- from the locked worker row (released_worker_id = worker_id, released_worker_nonce =
-- workers.snapshot_register_nonce, 00236), so a restarted worker (which rotates the nonce
-- on every registration) is a different incarnation and may reclaim, while the exact
-- incarnation that failed to park is barred. Both NULLABLE with NO DEFAULT (NULL = no
-- exclusion), no FK: the pair is an informational incarnation marker cleared by the next
-- claim, not a referential edge, and released_worker_id must survive the worker row's
-- deletion (an ephemeral worker reaped while the run stays parked).
ALTER TABLE runs ADD COLUMN released_worker_id uuid;
ALTER TABLE runs ADD COLUMN released_worker_nonce text;

-- +goose Down

-- True inverse of the additive Up (goose downs are not run in this deployment; store.Migrate
-- only ever goes up). Drop the three columns (dropping budget_finalize_seconds drops
-- runs_budget_finalize_seconds_check with it), then restore the two-value pause-mode CHECK.
ALTER TABLE runs DROP COLUMN IF EXISTS released_worker_nonce;
ALTER TABLE runs DROP COLUMN IF EXISTS released_worker_id;
ALTER TABLE runs DROP COLUMN IF EXISTS budget_finalize_seconds;

-- Restore the TWO-value pause-mode CHECK (00204's Up set: 'milestone', 'now'). The
-- narrowing is best-effort and DATA-DEPENDENT, mirroring 00204/00219's honesty: the
-- re-added narrower CHECK is VALIDATED (not NOT VALID) so it FAILS if any row already holds
-- pause_mode = 'wall', and this migration then refuses to come down — the correct outcome,
-- since a down that silently stranded a 'wall' row under a constraint that claims to forbid
-- it would be worse. Drain a parked run first if you must come down.
ALTER TABLE runs DROP CONSTRAINT runs_pause_mode_check;
ALTER TABLE runs ADD CONSTRAINT runs_pause_mode_check
    CHECK (pause_mode IS NULL OR pause_mode IN ('milestone', 'now'));
