-- +goose Up
-- PRD #517 M1: interactive, long-lived task runs. A run may be marked interactive at
-- create (runs.interactive), and when the worker signals done it parks in a new
-- non-terminal 'awaiting_followup' status to iterate conversationally rather than
-- terminating; the run is wound down with 'uzi run stop', which stamps a 'stopped'
-- stop_kind and rides back to the worker as a 'stop' steering input.
--
-- All four changes ride ONE migration because they are one feature and must land
-- atomically: the status the run parks in, the column that opts it in, and the two
-- kinds (stop_kind='stopped', run_user_inputs.kind='stop') the wind-down writes.

-- runs.status: widen to a TENTH value. The nine existing values are carried VERBATIM
-- from the LIVE constraint (last re-added in 00092) — a DROP+ADD that re-derives the
-- list from anything but the live constraint silently deletes whatever it forgets
-- (00092 documents exactly that failure with 'limit_wait'). 'awaiting_followup' is a
-- NON-TERMINAL park, like awaiting_approval/awaiting_input/limit_wait.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled', 'awaiting_followup'));

-- runs.interactive: the opt-in. NOT NULL DEFAULT false, mirroring open_mr (PRD #400):
-- false for every existing and non-interactive run, so a plain bool that re-delivers
-- unchanged on every claim.
ALTER TABLE runs ADD COLUMN interactive boolean NOT NULL DEFAULT false;

-- runs.stop_kind: a FOURTH value, 'stopped' — the deliberate human wind-down of an
-- interactive run. The three existing values are carried verbatim from 00082 (Postgres
-- auto-named the original inline constraint runs_stop_kind_check). NOTE for consumers:
-- 00082 records that web's isStoppedRun styles the two HUMAN stops calm/neutral and that
-- 'auto_stopped' is breakage; 'stopped' is ALSO a deliberate human stop and folds in
-- with 'cancelled'/'plan_rejected', not with 'auto_stopped'. stop_kind stays display-only
-- (it does not change status or any claim/sweep gate).
ALTER TABLE runs DROP CONSTRAINT runs_stop_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped', 'stopped'));

-- run_user_inputs.kind: a SEVENTH steering-input kind, 'stop' — the wind-down signal a
-- 'uzi run stop' writes for the worker's steering poll to consume. The six existing
-- values are carried verbatim from the LIVE constraint (last widened in 00092); writing
-- this list from PRD #517's own text would drop 'revise_plan'/'answer' and break #41/#88.
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer', 'stop'));

-- +goose Down
-- Each narrowing is best-effort and DATA-DEPENDENT, copying 00082/00074's honesty: a
-- re-added narrower CHECK FAILS if any row already holds the new value (a run parked in
-- 'awaiting_followup', a stop_kind='stopped', or a kind='stop' input), and this migration
-- then refuses to come down. That is the correct outcome — a down that silently stranded
-- rows violating the constraint it just installed would be worse. Goose downs are not run
-- in this deployment (store.Migrate only ever goes up); drain first if you must.
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer'));

ALTER TABLE runs DROP CONSTRAINT runs_stop_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_kind_check
    CHECK (stop_kind IN ('cancelled', 'plan_rejected', 'auto_stopped'));

ALTER TABLE runs DROP COLUMN interactive;

ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
-- Narrow back to the NINE values that existed immediately before this migration (00092's
-- set), NOT to any older set — rolling further back would delete a shipped status on the
-- way down, the defect 00092 was fixed for in the direction nobody watches.
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled'));
