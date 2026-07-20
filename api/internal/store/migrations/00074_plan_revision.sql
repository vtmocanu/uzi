-- +goose Up
-- PRD #41: plan revision at the approval gate. Add a fifth steering-input kind,
-- 'revise_plan', a plain enqueue (like follow_up/approve_plan) carrying the user's
-- feedback text. The kind CHECK on run_user_inputs is an unnamed inline constraint
-- from 00020 that Postgres auto-named run_user_inputs_kind_check; widen it.
ALTER TABLE run_user_inputs DROP CONSTRAINT IF EXISTS run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel', 'revise_plan'));

-- Slack gate epoch carrier (PRD #41): stamps which approval-gate generation a
-- Slack message belongs to, so a stale card's button can be ignored after a revise.
ALTER TABLE slack_run_messages ADD COLUMN gate_generation int;

-- +goose Down
-- NOTE: Down fails if any revise_plan row exists (the narrowed CHECK rejects it).
ALTER TABLE run_user_inputs DROP CONSTRAINT IF EXISTS run_user_inputs_kind_check;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_kind_check
    CHECK (kind IN ('follow_up', 'approve_plan', 'reject_plan', 'cancel'));

ALTER TABLE slack_run_messages DROP COLUMN gate_generation;
