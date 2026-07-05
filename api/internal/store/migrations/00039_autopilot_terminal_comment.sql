-- +goose Up

-- PRD #19 M5 (autopilot terminal comments). A per-run marker for the single
-- run-lifecycle terminal-state hook that posts the autopilot issue comment
-- (failure: a short reason + the run link; success: the merge-request link).
--
-- The hook records this timestamp FIRST, then posts the comment, so a crash (or a
-- forge blip) between the two loses one comment rather than ever double-posting
-- (Decision 6, record-then-comment). The write is a conditional UPDATE guarded on
-- auto_approve AND autopilot_commented_at IS NULL, so it doubles as the atomic
-- claim: of the possibly-concurrent hook invocations (the inline notify racing the
-- reconcile-loop retry) exactly one wins the row and posts.
--
-- It lives on runs, not autopilot_triggers, on purpose. autopilot_triggers exists
-- because the issues cache is evicted and rewritten by FullSync, so the pre-run
-- transition/comment dedup cannot live keyed to a cached issue (Decision 5). A run
-- is not a cache — it is the durable per-outcome anchor — and a retry (remove +
-- re-add the label) is a NEW run that must post its own outcome comment, which a
-- per-run marker gives for free where a per-issue one would suppress it.
ALTER TABLE runs ADD COLUMN autopilot_commented_at timestamptz NULL;

-- +goose Down
ALTER TABLE runs DROP COLUMN autopilot_commented_at;
