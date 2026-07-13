-- +goose Up

-- In-app notifications inbox (PRD #46 Decision 6). No in-app notification store
-- existed before this: the WS hub is per-run and the activity feed is a per-run
-- render of run_messages. This table is GENERIC — the judge is tenant #1, but kind
-- + payload jsonb let any feature enqueue a notification without a schema change.
--
-- Scoping (Decision 6, audit M2): a user sees their OWN notifications; an admin can
-- see all. The list is session-user-scoped, mark-read verifies row ownership.
-- payload carries the render data (title, links, verdict, MR url, …). run_id /
-- review_id are optional deep-link anchors, both ON DELETE CASCADE so a
-- notification never outlives the run/review it points at.
CREATE TABLE notifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    kind       text NOT NULL,
    payload    jsonb NOT NULL DEFAULT '{}'::jsonb,
    run_id     uuid REFERENCES runs ON DELETE CASCADE,
    review_id  uuid REFERENCES run_reviews ON DELETE CASCADE,
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Inbox listing: a user's notifications newest-first (also the per-user retention
-- prune scan, M2, keys on this).
CREATE INDEX idx_notifications_user_created ON notifications (user_id, created_at DESC);

-- The unread badge count: a partial index over just the unread rows.
CREATE INDEX idx_notifications_unread ON notifications (user_id) WHERE read_at IS NULL;

-- +goose Down
DROP TABLE notifications;
