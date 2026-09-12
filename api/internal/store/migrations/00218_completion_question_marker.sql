-- +goose Up

-- PRD #1226 M5: the dedicated completion-QUESTION discriminator marker. One nullable
-- column lands here: runs.completion_question_at is non-NULL for exactly as long as a run
-- is parked on a LIVE completion-interlock question (the worker authored it while awaiting
-- the owner's continue decision), and NULL for an ordinary PRD #88 ask_user clarification.
-- It replaces the imprecise "interlocked AND completion_attempts > 0" proxy that
-- completionQuestionOpen / completionPhaseRule used to key on — that proxy also matched an
-- interlocked, post-attempt run parked on an ORDINARY clarification, so an owner
-- completion-continue could deliver an `answer` that wrongly resolved the ordinary question.
-- The marker is set ONLY by SetRunAwaitingInput when the report flags a completion question,
-- and CLEARED on resolution by SetRunRunning (answer accepted, run resumes) and
-- SetRunCompletionHold (run enters the hold).
--
-- ADDITIVE (ADD COLUMN only) and PLAIN NULLABLE with NO CHECK and NO default, so an N-1
-- worker/api reading `runs` during a rolling release is byte-unaffected (NULL is the legacy
-- state for every existing row), there is NO CHECK on a live table, and
-- check-migration-additive needs neither the NOT VALID/VALIDATE two-step nor a table rewrite.
ALTER TABLE runs ADD COLUMN completion_question_at timestamptz;

-- +goose Down

-- Drop the column. Down runs only on an explicit `goose down`, never during a forward
-- rolling release (store.Migrate only goes up), so this drop is out of
-- check-migration-additive's Up-only scope.
ALTER TABLE runs DROP COLUMN completion_question_at;
