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
-- question id — see 00092). A count-based key would advance on that re-park and post
-- the same question a second time; equality on the id is a no-op across the requeue by
-- construction, which is the same reason 00092 keys the resume guard on identity.
--
-- NOTE (goose numbering): drafted as 00092 above what was then the branch head, and
-- renumbered to 00093 at the landing merge — PRD #35 had landed 00091_run_limit_wait
-- on main, so the #88 pair shifted up by one. The citations above point at 00092, the
-- sibling #88 migration, NOT at main's 00091. Stated explicitly because before this
-- edit the sentence was wrong twice over: it named a number this file no longer has,
-- and 00091 had since become an unrelated PRD's migration, so a reader following it
-- would have landed somewhere plausible and irrelevant.
ALTER TABLE slack_run_messages ADD COLUMN question_id text;

-- The `ts` of the message that CARRIED that question into the thread, which is what
-- lets an inbound reply be bound to the question it follows.
--
-- Slack gives an answer no id of its own — it is free text — so unlike web and CLI,
-- which echo back the id they were shown, the id has to be DERIVED. "Whichever
-- question is open when the reply arrives" is the wrong derivation: it is an
-- arrival-time key wearing identity's clothes, and it re-opens the exact race
-- identity keying exists to close. A reply written against Q1 that lands after Q2
-- opened would be stamped with Q2's id BY THE SERVER, and would then satisfy every
-- equality check downstream, because the server is the thing that supplied the id.
--
-- Ordering against the question message's own ts is what discriminates: a reply after
-- the current question's card answers that question; a reply before it answers a
-- superseded one and is refused. Note this stays correct across a requeue for free —
-- the re-park re-uses the question id, the notifier's identity dedupe therefore does
-- NOT re-post, and so this ts still points at the ORIGINAL card, which a pre-death
-- reply is still after.
ALTER TABLE slack_run_messages ADD COLUMN question_ts text;

-- +goose Down
ALTER TABLE slack_run_messages DROP COLUMN question_ts;
ALTER TABLE slack_run_messages DROP COLUMN question_id;
