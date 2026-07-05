-- +goose Up

-- PRD #19 M4 (autopilot trigger). Two additions the poller's post-sync detection
-- needs: a durable per-issue trigger ledger that outlives the issue cache, and the
-- run-level auto_approve flag the worker reads to skip the plan gate.

-- autopilot_triggers: the transition-once ledger (Decision 5). Keyed
-- (repo_id, issue_iid) so it is INDEPENDENT of the issues cache — FullSync evicts
-- and re-inserts issues rows (DeleteIssuesNotIn), so a "never re-run / never
-- re-comment" guarantee cannot live there. last_event_id is the resource-label-event
-- id of the autopilot-label application this row last handled; the poller acts only
-- when the CURRENT application's event id is greater (an absent→present transition
-- it has not seen). Event ids are globally monotonic on GitLab, so a larger id is
-- strictly a later application. This one column dedups BOTH the run AND the
-- explanatory comment: once an event id is recorded, that application never re-runs
-- and never re-comments, so a crash after recording but before commenting loses one
-- comment rather than ever double-posting (Decision 6, record-then-comment).
-- Retrying is a deliberate human gesture — remove and re-add the label mints a new,
-- larger event id.
CREATE TABLE autopilot_triggers (
    repo_id       uuid   NOT NULL REFERENCES repos ON DELETE CASCADE,
    issue_iid     bigint NOT NULL,
    last_event_id bigint NOT NULL,
    handled_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, issue_iid)
);

-- runs.auto_approve: set only on autopilot-created runs (Decision 2). The worker
-- reads it (delivered top-level in the claim payload, M5) to resolve the plan gate
-- with an approve verdict instead of parking the run at awaiting_approval. Default
-- false, so every manually-started run keeps the human plan-approval block.
ALTER TABLE runs ADD COLUMN auto_approve boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE runs DROP COLUMN auto_approve;
DROP TABLE autopilot_triggers;
