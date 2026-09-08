-- +goose Up

-- PRD #1190 M1: 'paused' — an owner-requested, NON-TERMINAL hold, in the wait family
-- beside limit_wait/pool_wait. Unlike the usage-limit park it is promoted only on demand
-- (uzi run resume / the run page), spends no budget while parked, and preserves the run's
-- session, worker affinity and message history. Five schema changes ride ONE migration
-- because they are one feature and must land atomically: the status the run holds in, the
-- three columns that carry a PENDING pause request on a still-running run, the shared
-- checkpoint_tip_at stamp, the steering-input kinds the feature writes, and the Slack
-- anchor's park_kind marker so a resume reply can be worded honestly per park kind.

-- runs.status: widen to a TWELFTH value. The eleven existing values are carried VERBATIM
-- from the LIVE constraint (the Up of 00170_run_pool_wait.sql) — a DROP+ADD that
-- re-derives the list from anything but the live constraint silently deletes whatever it
-- forgets (00092 documents exactly that failure with 'limit_wait', and 00170's OWN Down
-- omits 'pool_wait', so it is NOT the source of truth). 'paused' is a NON-TERMINAL hold,
-- like limit_wait/pool_wait/awaiting_input/awaiting_followup.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled', 'awaiting_followup',
                      'pool_wait', 'paused'));

-- A PENDING pause is a FLAG on a still-running run, not a status (Decision 3): the run
-- stays 'running' and these three columns carry the request until the worker parks at its
-- boundary. All NULLABLE with NO DEFAULT (NULL = no pause pending), so every existing row
-- is byte-unchanged (no rewrite, no NOT NULL). pause_after_count is
-- len(milestones_completed) at request time — the count the milestone mode waits to exceed.
ALTER TABLE runs ADD COLUMN pause_requested_at timestamptz;
ALTER TABLE runs ADD COLUMN pause_mode text
    CONSTRAINT runs_pause_mode_check CHECK (pause_mode IS NULL OR pause_mode IN ('milestone', 'now'));
ALTER TABLE runs ADD COLUMN pause_after_count int;

-- runs.checkpoint_tip_at: when the branch tip was last checkpoint-published for this run
-- (SetRunCheckpointTip stamps it). Shared with PRD #1189 — whichever migration lands first
-- OWNS this column, so IF NOT EXISTS is the cross-landing hedge; it is plain Postgres and
-- goose runs raw SQL, so it works. Applying this migration twice on a scratch database in
-- M1 confirms the guard is idempotent.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS checkpoint_tip_at timestamptz;

-- run_user_inputs.kind: three new steering-input kinds. The eight existing values are
-- carried VERBATIM from the LIVE constraint (last widened in 00162_run_scope_steering.sql,
-- which added 'scope'). 'pause' and 'pause_cancel' are worker-CONSUMED steering inputs (the
-- worker's route reads them); 'resume' is a SERVER-ONLY audit kind the resume endpoint
-- writes, EXCLUDED from ConsumeRunInputs so the worker never drains it. Re-deriving the
-- list from anything but the live constraint silently deletes whatever it forgets (00092
-- documents exactly that failure).
ALTER TABLE run_user_inputs DROP CONSTRAINT IF EXISTS run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope', 'pause', 'pause_cancel', 'resume'));

-- slack_run_messages.park_kind: which park the marker recorded, so the resume line can be
-- worded honestly ('paused' → "▶️ Resumed", 'limit_wait'/NULL → "▶️ Resumed · usage limit
-- cleared"); nullable, no default; slack_run_messages is NOT a worker-facing table so this
-- is not flagged by check-migration-additive.
ALTER TABLE slack_run_messages ADD COLUMN park_kind text;

-- +goose Down
-- park_kind is dropped first (last added in Up); its own block, no other slack change rides
-- this migration.
ALTER TABLE slack_run_messages DROP COLUMN IF EXISTS park_kind;

-- Each narrowing is best-effort and DATA-DEPENDENT, mirroring 00162/00170's honesty: a
-- re-added narrower CHECK FAILS if any row already holds a new value (a paused run, or a
-- pause/pause_cancel/resume input), and this migration then refuses to come down — the
-- correct outcome, since a down that silently stranded rows violating the constraint it
-- just installed would be worse. Goose downs are not run in this deployment (store.Migrate
-- only ever goes up); drain first if you must.
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop', 'scope'));

-- checkpoint_tip_at uses DROP COLUMN IF EXISTS: PRD #1189 may own the column on a
-- landing where it merged first, so a plain DROP would error on a down it never added.
ALTER TABLE runs DROP COLUMN IF EXISTS checkpoint_tip_at;
ALTER TABLE runs DROP COLUMN pause_after_count;
ALTER TABLE runs DROP COLUMN pause_mode;
ALTER TABLE runs DROP COLUMN pause_requested_at;

-- Restore the ELEVEN-value status CHECK (00170's Up set, incl. 'pool_wait') — NOT 00170's
-- own ten-value Down, which forgets pool_wait.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled', 'awaiting_followup',
                      'pool_wait'));
