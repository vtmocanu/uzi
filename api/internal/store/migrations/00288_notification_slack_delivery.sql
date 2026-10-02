-- +goose Up

-- Issue #1675: a halt DM (ci_autofix_halted / mr_rework_halted) for an issue-less
-- branch could be lost because the Slack send was purely in-memory best-effort after
-- the notifications insert. These columns make delivery at-least-once for the opted-in
-- durable kinds: the row carries its stored DM render, and a redelivery sweep claims
-- rows whose delivery has not settled.
--
-- slack_render is non-NULL ONLY for opted-in kinds; every other row keeps the
-- write-only event-log behaviour. slack_attempts counts claims (the initial in-memory
-- enqueue is attempt 1). slack_delivered_at is set when delivery SETTLES: posted, or
-- the owner has no Slack link (terminal, nothing to retry).
ALTER TABLE notifications
    ADD COLUMN slack_render       jsonb,
    ADD COLUMN slack_attempts     integer NOT NULL DEFAULT 0,
    ADD COLUMN slack_attempted_at timestamptz,
    ADD COLUMN slack_delivered_at timestamptz;

-- The redelivery sweep scan: only undelivered durable rows, ordered by last attempt.
CREATE INDEX idx_notifications_slack_pending ON notifications (slack_attempted_at)
    WHERE slack_render IS NOT NULL AND slack_delivered_at IS NULL;

-- +goose Down
DROP INDEX idx_notifications_slack_pending;
ALTER TABLE notifications
    DROP COLUMN slack_delivered_at,
    DROP COLUMN slack_attempted_at,
    DROP COLUMN slack_attempts,
    DROP COLUMN slack_render;
