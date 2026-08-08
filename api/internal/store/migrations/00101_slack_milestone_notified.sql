-- +goose Up
-- PRD #122 M4: the Slack side of milestone progress. The run's DM anchor records the
-- last completed-milestone COUNT the notifier posted a `✓ N/M` thread line for, so a
-- redelivered `running` state event does not re-post a line the thread already carries.
--
-- Why a NEW column and not the plan gate's gate_generation: that one is the count of
-- kind='plan' run_messages and belongs to the approval gate (handleGate reads/writes it
-- across the run's lifetime). Overloading it would make a milestone advance the plan
-- generation and silently swallow the next plan version's gate — the same reasoning
-- 00093 gives for keeping the question anchor out of gate_generation.
--
-- Nullable, as every prior column added to this table is (00074, 00093): NULL means no
-- milestone thread line has been posted yet — the notifier reads it as 0 — and is NOT
-- the same as a stored 0, which would claim a line for the empty set had been posted.
ALTER TABLE slack_run_messages ADD COLUMN milestones_notified_completed int;

-- +goose Down
ALTER TABLE slack_run_messages DROP COLUMN milestones_notified_completed;
