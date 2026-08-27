-- +goose Up
-- PRD #88 M1: agent-initiated clarification. A run parks in a new `awaiting_input`
-- status while the lead waits on a human answer, and the answer rides back as a
-- sixth steering-input kind, 'answer'.
--
-- The runs.status CHECK is an unnamed inline constraint from 00020 that Postgres
-- auto-named runs_status_check; widen it. NOTE the eight existing values are carried
-- verbatim — a DROP+ADD that re-derives the list from anything but the LIVE constraint
-- silently deletes whatever it forgets.
--
-- That is not hypothetical here. This migration was written when 00090 was head and
-- listed seven values; PRD #35 then landed 00091_run_limit_wait, adding 'limit_wait'.
-- Renumbering this file above it made it run SECOND, so the seven-value list would
-- have dropped 'limit_wait' — failing the ALTER outright on any database holding a
-- parked run, and silently rejecting every later limit_wait write on one that did not.
-- An enumeration is a measurement with a shelf life, and a merge is when it expires.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled'));

-- run_user_inputs.kind was already re-named by 00074 (PRD #41, which added
-- 'revise_plan'). Re-add from 00074's FIVE-value list plus 'answer' = six. Writing
-- this from PRD #88's own text would drop 'revise_plan' and break #41's shipped
-- plan-revision gate.
ALTER TABLE run_user_inputs DROP CONSTRAINT IF EXISTS run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan', 'answer'));

-- The open question's identity, stamped by SetRunAwaitingInput at every park and
-- re-stamped to the SAME value when a resumed worker re-parks on the same question.
--
-- Why identity and not a timestamp: the stale-answer guard cannot key on a clock or
-- an arrival ordinal. A worker death re-queues the run (RequeueWorkerRuns sets
-- status='queued'), the resumed worker re-parks, and any when-based key is
-- re-stamped by an event the user neither caused nor can see — silently rejecting a
-- perfectly good answer they submitted against the live question before the death.
-- The question survives that requeue unchanged (the session resumes and re-asks it),
-- so an identity-keyed predicate is a no-op across the requeue by construction.
ALTER TABLE runs ADD COLUMN open_question_id text;

-- The question an `answer` row answers, extracted and validated server-side from the
-- JSON body at SubmitInput time. NULL for every other kind. This is what lets
-- SetRunRunning's resume guard ask "was THIS question answered" rather than "has this
-- run ever been answered" — the latter protects only the first question of a run and
-- leaves every later one (QUESTION_MAX defaults to 5) unguarded.
ALTER TABLE run_user_inputs ADD COLUMN question_id text;

-- +goose Down
-- NOTE, copying 00074's honesty: Down FAILS if any run is parked in 'awaiting_input'
-- or any 'answer' input row exists — the narrowed CHECKs reject them. Drain first.
ALTER TABLE run_user_inputs DROP COLUMN question_id;
ALTER TABLE runs DROP COLUMN open_question_id;

ALTER TABLE run_user_inputs DROP CONSTRAINT IF EXISTS run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan'));

ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
-- Down narrows to the EIGHT values that existed immediately before this migration —
-- i.e. 00091_run_limit_wait's set, WITH 'limit_wait' — not to 00020's original seven.
-- Rolling back to seven would delete PRD #35's status on the way down, which is the
-- same defect this migration's Up was fixed for, in the direction nobody watches.
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval',
                      'limit_wait', 'completed', 'failed', 'cancelled'));
