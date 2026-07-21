-- +goose Up

-- Filed→Done sync (PRD #98 Decision 6, M6). When an issue filed from a judge
-- recommendation (#68) is CLOSED on the forge, the recommendation auto-moves to Done.
-- This is the PRD's one and only migration; the rest of #98 is queries, endpoints and web.
--
-- The whole design turns on one fact about the poller: it holds a synced SNAPSHOT of
-- issue state (`issues.state`, upserted by forgesvc FullSync/IncrementalSync), NOT a
-- stream of transition events. So "write done while the linked issue is closed" is
-- LEVEL-triggered — it re-fires on every tick for as long as the issue stays closed, and
-- that breaks two things badly:
--
--   1. A human Undo (#94's disposition delete) is silently re-applied on the next tick, so
--      Undo can never stick.
--   2. Reusing #94's DO-UPDATE upsert would OVERWRITE a coordinate the user had already
--      dismissed, replacing their verdict with `done`.
--
-- Two columns fix it, and neither is optional:

-- close_synced_at is the EDGE MARKER. The pass acts on a linked issue only when the cached
-- state is closed AND this is NULL — i.e. only on the open→closed edge — and stamps it
-- immediately after. That makes the sync fire EXACTLY ONCE per close. After an Undo the
-- edge is already consumed, so the next tick does not re-apply and the Undo STICKS. A
-- reopen deliberately does NOT clear it (no auto-reopen: flapping issues must not
-- ping-pong a user's backlog), and a re-close is therefore a no-op.
ALTER TABLE recommendation_filed_issues ADD COLUMN close_synced_at timestamptz;

-- The pass's working set: settled links in one repo whose edge has not been consumed. The
-- poller runs this per repo per tick, so it is a hot path; the partial predicate keeps the
-- index to just the rows that can still fire. filed_repo_id is NOT NULL here because a
-- NULL-repo row can never be matched to a cached issue (see the query's comment on why
-- joining by iid alone would be a cross-repo bug).
CREATE INDEX idx_recommendation_filed_issues_close_pending
    ON recommendation_filed_issues (filed_repo_id)
    WHERE close_synced_at IS NULL AND filed_at IS NOT NULL AND filed_repo_id IS NOT NULL;

-- set_via records PROVENANCE so an automatic Done is visibly distinct from one a human
-- marked: NULL (the default, and every pre-existing row) means a person set it;
-- 'issue_close' means this sync did, and the UI can label it "done via #IID".
--
-- It pairs with set_by_user_id, which the sync writes as NULL — deliberately NOT
-- filed_by_user_id. Filed issues are NOT owner-scoped (#68 Decision 8 lets an admin file
-- on another user's review), so the filer may be a different person than the coordinate's
-- owner; attributing the row to them would make #94's panel name a foreign user as the
-- setter. NULL means "the system", which is the truth.
--
-- The CHECK is a closed domain, unlike the deliberately unconstrained `category` in 00071/
-- 00073: that one mirrors another table's enum and is copied from an already-validated
-- row, whereas set_via is written only from a server-side literal in one query and can
-- never arrive from a request body.
ALTER TABLE recommendation_dispositions
    ADD COLUMN set_via text CHECK (set_via IS NULL OR set_via IN ('issue_close'));

-- +goose Down
ALTER TABLE recommendation_dispositions DROP COLUMN set_via;
DROP INDEX idx_recommendation_filed_issues_close_pending;
ALTER TABLE recommendation_filed_issues DROP COLUMN close_synced_at;
