-- +goose Up
-- PRD #88 M3: the Slack side of the clarification park. The run's DM anchor records
-- WHICH question it has already posted into the thread, so the notifier can tell a
-- genuinely new question from a re-broadcast of one already on screen.
--
-- Why a column and not the plan gate's gate_generation: that one is the count of
-- kind='plan' run_messages and belongs to the approval gate, which can be open across
-- the same run's lifetime. Overloading it would make a question advance the plan
-- generation and silently swallow the next plan version's gate.
--
-- Why identity and not a count: the run parks on the SAME question again after a
-- worker death (RequeueWorkerRuns re-queues, the resumed worker re-parks re-using the
-- question id — see 00091). A count-based key would advance on that re-park and post
-- the same question a second time; equality on the id is a no-op across the requeue by
-- construction, which is the same reason 00091 keys the resume guard on identity.
--
-- NOTE (goose numbering): drafted as the next free number above the live head 00091 on
-- this branch. Renumbered at the landing merge if a parallel PRD lands a lower one.
ALTER TABLE slack_run_messages ADD COLUMN question_id text;

-- +goose Down
ALTER TABLE slack_run_messages DROP COLUMN question_id;
